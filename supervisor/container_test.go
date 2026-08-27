package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/service"
)

// A container service is supervised against the real runtime or not at all: a
// fake runtime would prove that hostd talks to a fake. Where there is no
// runtime, or no small image to run, the test skips.
func requireRuntime(t *testing.T) (*docker.Client, string) {
	t.Helper()
	client, err := docker.Open()
	if errors.Is(err, docker.ErrNoRuntime) {
		t.Skip("this machine has no container runtime")
	}
	if err != nil {
		t.Fatalf("docker.Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = client.Ping(ctx)
	if err != nil {
		t.Skipf("the runtime socket is there but does not answer: %v", err)
	}
	// The suite never pulls: a test that reaches the network is a test that
	// fails on a train.
	for _, image := range []string{os.Getenv("HOSTD_TEST_IMAGE"), "busybox:latest", "alpine:latest"} {
		if image == "" {
			continue
		}
		_, err = client.ImageDigest(ctx, image)
		if err == nil {
			return client, image
		}
	}
	t.Skip("no small image on this machine; docker pull busybox to run this test")
	return nil, ""
}

// Temporary directories make a test's files its own, but the runtime's names
// are the machine's: a container called after a service somebody really runs
// would be deleted by this suite's cleanup. The name says whose it is.
const testService = "hostd-suite-probe"

// The suite drives a real runtime and a real log store; everything it creates
// carries a name nothing else on the machine could have, because the runtime's
// names are the machine's and not the test's.
type harness struct {
	t        *testing.T
	buffer   *logs.Store
	services string
	sup      *Supervisor
	cancel   context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := logs.Open(context.Background(), filepath.Join(t.TempDir(), "logs.db"), logs.Options{})
	if err != nil {
		t.Fatalf("logs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &harness{t: t, buffer: store, services: t.TempDir()}
}

// start2 starts a supervisor the test already built, which the job tests do
// because they hand it a runtime first.
//
// Stopping it is the harness's job, never the test's. A supervisor a test
// forgot to stop keeps its drift round going for the rest of the package, and
// its declarations are not the next test's: it removes the container that test
// just created, or brings back one it retired. Two tests here really did leak
// one, and that was the whole of the flake in this suite.
func (h *harness) start2(services ...service.Service) {
	h.t.Helper()
	err := h.sup.Adopt(context.Background(), services)
	if err != nil {
		h.t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.t.Cleanup(h.stop)
	go h.sup.Run(ctx)
}

func (h *harness) start(services ...service.Service) {
	h.t.Helper()
	h.sup = New(h.buffer, h.services)
	h.start2(services...)
}

// stop ends the loop the way hostd ends: the containers keep running, because
// they are the runtime's. Idempotent, so a test that stops early and the
// cleanup that stops anyway do not fight over it.
func (h *harness) stop() {
	h.t.Helper()
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case <-h.sup.Done():
	case <-time.After(20 * time.Second):
		h.t.Fatal("the supervisor did not stop")
	}
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s:\n%s", what, h.logText())
}

func (h *harness) status(name string) Status {
	h.t.Helper()
	status, err := h.sup.StatusOf(name)
	if err != nil {
		return Status{Name: name}
	}
	return status
}

func (h *harness) search(q logs.Query) []logs.Record {
	h.t.Helper()
	records, err := h.buffer.Search(q)
	if err != nil {
		h.t.Fatalf("Search: %v", err)
	}
	return records
}

func (h *harness) logText() string {
	var b strings.Builder
	for _, r := range h.search(logs.Query{}) {
		b.WriteString(r.Service)
		b.WriteString(" ")
		b.WriteString(r.Stream)
		b.WriteString(": ")
		b.WriteString(r.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func container(name, image, script string) service.Service {
	return service.Service{
		Name:        name,
		Kind:        service.KindContainer,
		Image:       image,
		Args:        []string{"sh", "-c", script},
		State:       service.StateRunning,
		Restart:     service.RestartNever,
		StopTimeout: 2,
	}
}

// Everything the runtime is holding for hostd goes away with the test, even
// when it fails: a suite that leaves containers behind is a suite nobody runs
// twice.
func cleanup(t *testing.T, client *docker.Client, names ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, name := range names {
			_ = client.Remove(ctx, containerName(name))
		}
	})
}

func TestRunsAndCapturesAContainer(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	svc := container(testService, image, `echo "listening on 80"; echo "warming up" >&2; sleep 30`)
	h.start2(svc)

	h.waitFor("the container to be running", func() bool { return h.status(testService).State == StateRunning })

	// The container's main process, so a container service is measured the
	// same way as any other.
	if h.status(testService).PID == 0 {
		t.Fatal("a running container reported no process")
	}

	h.waitFor("its output in the timeline", func() bool {
		return strings.Contains(h.logText(), "listening on 80")
	})
	// Which stream a line came from is what tells an operator the service is
	// complaining rather than reporting.
	var stderr bool
	for _, record := range h.search(logs.Query{Service: testService}) {
		if record.Stream == logs.StreamErr && strings.Contains(record.Text, "warming up") {
			stderr = true
		}
	}
	if !stderr {
		t.Fatalf("what the container wrote to stderr was not kept apart:\n%s", h.logText())
	}
}

// The runtime holds the process, so restarting hostd must not restart the
// container: the daemon asks the runtime what it already owns.
func TestAdoptsAContainerAcrossARestart(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	svc := container(testService, image, `while true; do echo tick; sleep 1; done`)
	h.start2(svc)
	h.waitFor("the container to be running", func() bool { return h.status(testService).State == StateRunning })
	first := h.status(testService)

	// The daemon leaves the way it does on an upgrade: the loop stops, the
	// container keeps running.
	h.stop()

	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	h.start2(svc)

	h.waitFor("the container to be seen again", func() bool { return h.status(testService).State == StateRunning })
	after := h.status(testService)
	if after.PID != first.PID {
		t.Fatalf("the container was replaced: process %d became %d", first.PID, after.PID)
	}
	// Nothing was adopted, because nothing was ever taken over: the runtime
	// held it the whole time and the new daemon simply asked.
	if after.Restarts != first.Restarts {
		t.Fatalf("the container came back %d times across a daemon restart", after.Restarts-first.Restarts)
	}
}

// Stopping is the runtime's business, but the outcome is hostd's promise: the
// service ends and the name is free for the next start.
func TestStoppingIsRememberedByTheRuntime(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	// The policy that keeps a service alive is what makes a stop legible: the
	// runtime would have brought anything else back, so a container that is
	// exited under it was stopped by a hand. A service declared "never" has no
	// such record, and a stop that ends in a kill reads as the kill it was.
	svc := container(testService, image, `while true; do sleep 1; done`)
	svc.Restart = service.RestartAlways
	h.start2(svc)

	h.waitFor("the container to be running", func() bool { return h.status(testService).State == StateRunning })
	_, err := h.sup.Stop(testService)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	h.waitFor("the container to stop", func() bool { return h.status(testService).State == StateStopped })

	// The container stays, stopped. Nothing here remembers that a hand
	// stopped it: the runtime would have restarted anything else under this
	// policy, and that is the record — one that survives a restart of hostd
	// and of the machine.
	observed, err := client.Inspect(context.Background(), containerName(testService))
	if err != nil {
		t.Fatalf("the stopped container is gone: %v", err)
	}
	if observed.Running {
		t.Fatal("the container is still running after the service was stopped")
	}
	if observed.Restart != "unless-stopped" {
		t.Fatalf("the container carries the policy %q; a stop must be remembered by the runtime", observed.Restart)
	}
}

// A machine with no runtime is a valid machine: declaring a container on it
// has to fail with something an operator can act on.
func TestAContainerOnAMachineWithoutARuntimeSaysSo(t *testing.T) {
	h := newHarness(t)
	h.start(container(testService, "whatever:1", "true"))
	defer h.stop()

	h.waitFor("the failure to be reported", func() bool {
		return strings.Contains(h.status(testService).LastError, "container runtime")
	})
}

// A tag can be made to mean something else tomorrow, so what ran is recorded
// as the digest.
func TestTheStartedEventNamesTheImageThatRan(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	h.start2(container(testService, image, "sleep 30"))

	h.waitFor("the started event", func() bool {
		for _, record := range h.search(logs.Query{Kind: logs.EventStarted}) {
			if strings.Contains(record.Text, "started container") && strings.Contains(record.Text, " from ") {
				return true
			}
		}
		return false
	})
}

// The deploy flow depends on this: a file naming another image is another
// service, and an apply that said "nothing to change" would leave the old
// image running after a push.
func TestANewImageIsAChange(t *testing.T) {
	before := container(testService, "site@sha256:aaa", "sleep 1")
	after := before
	after.Image = "site@sha256:bbb"
	if sameDefinition(before, after) {
		t.Fatal("a container pointing at another image was read as unchanged")
	}

	ported := before
	ported.Ports = []string{"8080:80"}
	if sameDefinition(before, ported) {
		t.Fatal("a container publishing another port was read as unchanged")
	}

	limited := before
	limited.Memory = 512
	if sameDefinition(before, limited) {
		t.Fatal("a container with another memory ceiling was read as unchanged")
	}
}

// The proxy case, which is why the shared network exists: one service reaches
// another by its own name, so an application publishes nothing to the machine
// and only whatever answers the internet does.
func TestServicesReachEachOtherByName(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	svc := container(testService, image, `echo reached-by-name > /tmp/i.html; httpd -f -p 80 -h /tmp`)
	h.start2(svc)
	h.waitFor("the service to be running", func() bool { return h.status(testService).State == StateRunning })

	// A second container on the same network, asking for the service by the
	// name the file gave it. Nothing is published to the machine.
	probe := "hostd-suite-caller"
	callCtx, callCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer callCancel()
	t.Cleanup(func() {
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer removeCancel()
		_ = client.Remove(removeCtx, probe)
	})
	_ = client.Remove(callCtx, probe)
	id, err := client.Create(callCtx, docker.Spec{
		Name:    probe,
		Image:   image,
		Args:    []string{"wget", "-q", "-T", "10", "-O-", "http://" + testService + "/i.html"},
		Network: Network,
		Alias:   probe,
	})
	if err != nil {
		t.Fatalf("create the caller: %v", err)
	}
	err = client.Start(callCtx, id)
	if err != nil {
		t.Fatalf("start the caller: %v", err)
	}
	code, err := client.Wait(callCtx, id)
	if err != nil {
		t.Fatalf("wait for the caller: %v", err)
	}

	var answer string
	err = client.Logs(callCtx, id, time.Time{}, func(line docker.Line) error {
		answer += line.Text
		return nil
	})
	if err != nil {
		t.Fatalf("read the caller's output: %v", err)
	}
	if code != 0 || !strings.Contains(answer, "reached-by-name") {
		t.Fatalf("one service could not reach another by name: exit %d, output %q", code, answer)
	}
}

// A container created before the file said what to do when it ends — or by an
// older hostd — is corrected in place. Recreating it to change one field would
// interrupt a service for nothing.
func TestARestartPolicyThatDriftedIsCorrectedInPlace(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	svc := container(testService, image, `while true; do sleep 1; done`)
	svc.Restart = service.RestartAlways
	h.start2(svc)
	h.waitFor("the container to be running", func() bool { return h.status(testService).State == StateRunning })

	before := h.status(testService)
	err := client.UpdateRestart(context.Background(), containerName(testService), "no")
	if err != nil {
		t.Fatalf("drift the policy: %v", err)
	}

	h.waitFor("the policy to be corrected", func() bool {
		observed, inspectErr := client.Inspect(context.Background(), containerName(testService))
		return inspectErr == nil && observed.Restart == "unless-stopped"
	})
	after := h.status(testService)
	if after.PID != before.PID {
		t.Fatalf("the service was interrupted to change a policy: process %d became %d", before.PID, after.PID)
	}
}

func job(name, image, script, every string) service.Service {
	svc := container(name, image, script)
	svc.Every = every
	svc.Restart = service.RestartNever
	svc.Overlap = service.OverlapAllow
	return svc
}

// The whole point of a job: it runs, it ends, and it runs again, and the
// history says what each run did.
func TestAJobRunsOnItsScheduleAndIsRecorded(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	t.Cleanup(func() { removeRuns(t, client, testService) })

	h.start2(job(testService, image, `echo working; exit 0`, "1s"))
	defer h.stop()

	h.waitFor("two runs to finish", func() bool {
		return len(h.search(logs.Query{Service: testService, Kind: logs.EventJobFinished})) >= 2
	})
	finished := h.search(logs.Query{Service: testService, Kind: logs.EventJobFinished})
	if !strings.Contains(finished[0].Text, "exit 0") || !strings.Contains(finished[0].Text, "after") {
		t.Fatalf("a finished run does not say what it did: %q", finished[0].Text)
	}
	// Each run is named for the instant it was due, so two of them are two
	// entries and not one line twice.
	if finished[0].Run == "" || finished[0].Run == finished[1].Run {
		t.Fatalf("runs are not told apart: %q and %q", finished[0].Run, finished[1].Run)
	}
	// What a run wrote is in the timeline under the run that wrote it.
	output := h.search(logs.Query{Service: testService, Stream: logs.StreamOut})
	if len(output) == 0 || output[0].Run == "" {
		t.Fatalf("the output of a run does not say which run wrote it: %#v", output)
	}
}

// Overlapping is the point for a worker pool: the runs share the work, and
// they agree with each other in the queue they read from, not here.
func TestRunsOverlapWhenTheJobSaysSo(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	t.Cleanup(func() { removeRuns(t, client, testService) })

	// Each run outlives its interval, so they pile up on purpose.
	h.start2(job(testService, image, `sleep 6`, "1s"))
	defer h.stop()

	h.waitFor("several runs to be going at once", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		live, err := h.sup.runsOf(ctx, testService)
		return err == nil && len(live) >= 3
	})
}

// Scaling without a ceiling is not elasticity: it is a machine dying slowly
// while a new run starts every turn.
func TestTheCeilingHoldsAndSaysSo(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	t.Cleanup(func() { removeRuns(t, client, testService) })

	svc := job(testService, image, `sleep 20`, "1s")
	svc.MaxParallel = 2
	h.start2(svc)
	defer h.stop()

	h.waitFor("the ceiling to be reported", func() bool {
		return len(h.search(logs.Query{Service: testService, Kind: logs.EventJobSkipped})) > 0
	})
	skipped := h.search(logs.Query{Service: testService, Kind: logs.EventJobSkipped})
	if !strings.Contains(skipped[0].Text, "ceiling") {
		t.Fatalf("the ceiling was hit and the reason is not in the timeline: %q", skipped[0].Text)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	live, err := h.sup.runsOf(ctx, testService)
	if err != nil {
		t.Fatalf("runsOf: %v", err)
	}
	if len(live) > 2 {
		t.Fatalf("%d runs are going with a ceiling of 2", len(live))
	}
}

// Work that must not run twice at once says so, and losing a turn is reported
// rather than silent.
func TestASkippingJobLetsTheTurnPass(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	t.Cleanup(func() { removeRuns(t, client, testService) })

	svc := job(testService, image, `sleep 8`, "1s")
	svc.Overlap = service.OverlapSkip
	h.start2(svc)
	defer h.stop()

	h.waitFor("a turn to be skipped", func() bool {
		return len(h.search(logs.Query{Service: testService, Kind: logs.EventJobSkipped})) > 0
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	live, err := h.sup.runsOf(ctx, testService)
	if err != nil {
		t.Fatalf("runsOf: %v", err)
	}
	if len(live) > 1 {
		t.Fatalf("%d runs are going for a job declared to skip", len(live))
	}
}

// Starting the daemon is not one of the times a job was asked to run.
func TestStartingTheDaemonDoesNotFireAJob(t *testing.T) {
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	svc := job(testService, "whatever:1", "true", "1h")
	h.sup.declared[testService] = svc

	now := time.Now()
	h.sup.fireDue(context.Background(), now)
	h.sup.fireDue(context.Background(), now.Add(time.Minute))
	if len(h.search(logs.Query{Kind: logs.EventJobStarted})) != 0 {
		t.Fatal("a job fired because the daemon started, not because it was due")
	}
}

// The schedule is the wall clock, so restarting the daemon does not shift it.
func TestSlotsAreAlignedToTheClock(t *testing.T) {
	every := 2 * time.Minute
	at := time.Date(2026, 8, 23, 17, 5, 30, 0, time.UTC)
	slot := slotOf(at, every)
	if slot.Minute()%2 != 0 || slot.Second() != 0 {
		t.Fatalf("a job every two minutes would run at %s", slot)
	}
	if slotOf(at.Add(20*time.Second), every) != slot {
		t.Fatal("two instants in the same slot produced different slots")
	}
}

func removeRuns(t *testing.T, client *docker.Client, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	found, err := client.List(ctx, labelService)
	if err != nil {
		return
	}
	for _, container := range found {
		if container.Labels[labelService] == name {
			_ = client.Remove(ctx, container.ID)
		}
	}
}

// A run that outlives an upgrade is taken over by the next daemon: without
// that it would never be recorded and its container would stay for ever.
func TestARunInFlightIsPickedUpByTheNextDaemon(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	t.Cleanup(func() { removeRuns(t, client, testService) })

	svc := job(testService, image, `sleep 5`, "1s")
	h.start2(svc)
	h.waitFor("a run to start", func() bool {
		return len(h.search(logs.Query{Service: testService, Kind: logs.EventJobStarted})) > 0
	})

	// The daemon leaves the way it does on an upgrade, mid-run.
	h.stop()
	// It did not write down an ending that did not happen.
	for _, record := range h.search(logs.Query{Service: testService, Kind: logs.EventJobFinished}) {
		if strings.Contains(record.Text, "could not be read") {
			t.Fatalf("leaving was recorded as the run failing: %q", record.Text)
		}
	}

	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	h.start2(svc)

	h.waitFor("the run to be finished by the daemon that found it", func() bool {
		return len(h.search(logs.Query{Service: testService, Kind: logs.EventJobFinished})) > 0
	})
	// And the container it left behind is gone.
	h.waitFor("the container to be cleared", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		live, err := h.sup.containers(ctx)
		if err != nil {
			return false
		}
		for _, container := range live {
			if container.Service == testService && !container.Running {
				return false
			}
		}
		return true
	})
}

// Declaring a job is not running it: an apply that started one would run it
// off schedule, and the container it left behind would sit in the parallel
// ceiling for ever.
func TestApplyingAJobDoesNotStartIt(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	t.Cleanup(func() { removeRuns(t, client, testService) })

	// It starts life as a service, so the apply has something to take away.
	svc := service.Service{
		Name: testService, Kind: service.KindContainer, Image: image,
		Args: []string{"sh", "-c", "sleep 300"}, State: service.StateRunning,
	}
	h.start2(svc)
	h.waitFor("the service to be up", func() bool {
		observed, err := client.Inspect(context.Background(), containerName(testService))
		return err == nil && observed.Running
	})

	h.sup.Apply([]service.Service{job(testService, image, `echo ran`, "1m")})

	_, err := client.Inspect(context.Background(), containerName(testService))
	if !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("declaring a job left a container of its own: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	live, err := h.sup.runsOf(ctx, testService)
	if err != nil {
		t.Fatalf("runsOf: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("declaring a job started %d run(s)", len(live))
	}
}

// The drift round is what catches the container a job was left with by a
// daemon that did not know better; without it nothing ever removes it.
func TestTheDriftRoundTakesAwayAJobsOwnContainer(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	t.Cleanup(func() { removeRuns(t, client, testService) })

	svc := job(testService, image, `sleep 300`, "1h")
	// What an older daemon left: the job with a container of its own.
	err := h.sup.create(context.Background(), svc, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h.start2(svc)

	h.waitFor("the stray container to be taken away", func() bool {
		_, err := client.Inspect(context.Background(), containerName(testService))
		return errors.Is(err, docker.ErrNotFound)
	})
}

// A death hostd did not cause reaches the timeline: the runtime announces it,
// and the announcement is the only trace when a restart policy quietly brings
// the service back.
func TestADeathTheRuntimeAnnouncesLandsInTheTimeline(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	h.start2(container(testService, image, "exit 7"))

	h.waitFor("the runtime to announce the death", func() bool {
		records := h.search(logs.Query{Service: testService, Kind: logs.EventExited})
		return len(records) > 0 && strings.Contains(records[0].Text, "exited with code 7")
	})
}

// Deploy recreates the container from the declaration handed to it — the PID
// that answered before is not the one that answers after, and that is the
// point: the image tag is resolved again on the way, which restart never does.
// The declaration is upserted, so a service this supervisor never met deploys
// exactly the same way.
func TestDeployRecreatesTheContainer(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	svc := container(testService, image, "sleep 300")
	h.start2(svc)
	h.waitFor("the service to run", func() bool { return h.status(testService).State == StateRunning })
	before := h.status(testService).PID

	changed, err := h.sup.Deploy(svc)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !changed {
		t.Fatal("a deploy claims nothing changed")
	}
	h.waitFor("the fresh container", func() bool {
		status := h.status(testService)
		return status.State == StateRunning && status.PID != 0 && status.PID != before
	})
}

// Deploying a job puts its declaration in place and leaves the running to the
// schedule: a job has no standing container to recreate.
func TestDeployOfAJobPlacesTheDeclaration(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	h.start2()
	svc := container("tick", image, "true")
	svc.Every = "1h"
	changed, err := h.sup.Deploy(svc)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !changed {
		t.Fatal("deploying a job claims nothing changed")
	}
	status, err := h.sup.StatusOf("tick")
	if err != nil || status.Every != "1h" {
		t.Fatalf("the job's declaration is not in place: %+v (%v)", status, err)
	}
}

// Remove takes the service off this machine — container and declaration — and
// the drift round it nudges does not bring anything back: the declaration left
// the supervisor with the container.
func TestRemoveTakesTheServiceOffTheMachine(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)
	h.start2(container(testService, image, "sleep 300"))
	h.waitFor("the service to run", func() bool { return h.status(testService).State == StateRunning })

	changed, err := h.sup.Remove(testService)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !changed {
		t.Fatal("a removal claims nothing changed")
	}
	h.waitFor("the service to be gone", func() bool {
		_, missing := h.sup.StatusOf(testService)
		return missing != nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = client.Inspect(ctx, containerName(testService))
	if !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("the container is still on the machine: %v", err)
	}
}

// Two removals asked for at once do not wait one behind the other: a container
// spending its grace period must not hold the machine's other work, which is
// what holding the converge mutex across the wait did.
func TestTwoRemovalsDoNotQueueBehindEachOther(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	first, second := testService, testService+"-2"
	cleanup(t, client, first, second)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	// Long enough that queueing would show as double, short enough for a test.
	one := container(first, image, "trap '' TERM; sleep 300")
	one.StopTimeout = 4
	two := container(second, image, "trap '' TERM; sleep 300")
	two.StopTimeout = 4
	h.start2(one, two)
	h.waitFor("both to run", func() bool {
		return h.status(first).State == StateRunning && h.status(second).State == StateRunning
	})

	start := time.Now()
	var wg sync.WaitGroup
	for _, name := range []string{first, second} {
		wg.Go(func() {
			_, err := h.sup.Remove(name)
			if err != nil {
				t.Errorf("Remove %s: %v", name, err)
			}
		})
	}
	wg.Wait()
	// Two four-second graces in parallel finish in about four; queued they
	// would take eight.
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("two removals took %s, so they queued behind each other", elapsed)
	}
}
