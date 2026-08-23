package supervisor

import (
	"context"
	"errors"
	"os"
	"strings"
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
	h.sup = New(h.dirs, h.buffer)
	h.sup.Runtime(client)

	svc := container(testService, image, `echo "listening on 80"; echo "warming up" >&2; sleep 30`)
	err := h.sup.Adopt(context.Background(), []service.Service{svc})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.sup.Run(ctx)
	defer h.stop()

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
	h.sup = New(h.dirs, h.buffer)
	h.sup.Runtime(client)

	svc := container(testService, image, `while true; do echo tick; sleep 1; done`)
	err := h.sup.Adopt(context.Background(), []service.Service{svc})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.sup.Run(ctx)
	h.waitFor("the container to be running", func() bool { return h.status(testService).State == StateRunning })
	first := h.status(testService)

	// The daemon leaves the way it does on an upgrade: the loop stops, the
	// container keeps running.
	cancel()
	select {
	case <-h.sup.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("the supervisor did not stop")
	}

	next := New(h.dirs, h.buffer)
	next.Runtime(client)
	err = next.Adopt(context.Background(), []service.Service{svc})
	if err != nil {
		t.Fatalf("Adopt after restart: %v", err)
	}
	h.sup = next
	ctx, cancel = context.WithCancel(context.Background())
	h.cancel = cancel
	go next.Run(ctx)
	defer h.stop()

	h.waitFor("the container to be adopted", func() bool { return h.status(testService).Adopted })
	after := h.status(testService)
	if after.PID != first.PID {
		t.Fatalf("the container was replaced: process %d became %d", first.PID, after.PID)
	}
	if after.State != StateRunning {
		t.Fatalf("the adopted container reads as %s", after.State)
	}
}

// Stopping is the runtime's business, but the outcome is hostd's promise: the
// service ends and the name is free for the next start.
func TestStopsAContainerAndFreesTheName(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	cleanup(t, client, testService)
	h.sup = New(h.dirs, h.buffer)
	h.sup.Runtime(client)

	svc := container(testService, image, `while true; do sleep 1; done`)
	err := h.sup.Adopt(context.Background(), []service.Service{svc})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.sup.Run(ctx)
	defer h.stop()

	h.waitFor("the container to be running", func() bool { return h.status(testService).State == StateRunning })
	_, err = h.sup.Stop(testService)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	h.waitFor("the container to stop", func() bool { return h.status(testService).State == StateStopped })

	// Gone from the runtime, not merely stopped: a name held by a dead
	// container is a start that fails tomorrow.
	//
	// Asked by name, never by label: the label belongs to every hostd service
	// on this machine, and a suite that reads the machine's state fails
	// because of somebody else's running service — or, worse, passes because
	// of it.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, err = client.Inspect(context.Background(), containerName(testService))
		if errors.Is(err, docker.ErrNotFound) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the container was left behind after the service stopped")
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
	h.sup = New(h.dirs, h.buffer)
	h.sup.Runtime(client)

	err := h.sup.Adopt(context.Background(), []service.Service{container(testService, image, "sleep 30")})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.sup.Run(ctx)
	defer h.stop()

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
	h.sup = New(h.dirs, h.buffer)
	h.sup.Runtime(client)

	svc := container(testService, image, `echo reached-by-name > /tmp/i.html; httpd -f -p 80 -h /tmp`)
	err := h.sup.Adopt(context.Background(), []service.Service{svc})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.sup.Run(ctx)
	defer h.stop()
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
