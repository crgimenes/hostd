package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crgimenes/hostd/internal/logs"
	"github.com/crgimenes/hostd/internal/procid"
	"github.com/crgimenes/hostd/internal/service"
)

// The supervisor drives real processes, so the tests do too. They use shell
// utilities that exist on any unix; anywhere else they skip rather than fail.
func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the supervisor manages unix processes")
	}
}

type harness struct {
	t      *testing.T
	dirs   Dirs
	buffer *logs.Buffer
	sup    *Supervisor
	cancel context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	requireUnix(t)
	root := t.TempDir()
	// Every directory the supervisor touches is inside a temporary tree, by
	// construction. A test cannot reach a real host's state even if someone
	// later writes one carelessly.
	dirs := Dirs{
		State: filepath.Join(root, "supervision"),
		Spool: filepath.Join(root, "spool"),
	}
	return &harness{t: t, dirs: dirs, buffer: logs.NewBuffer(2000)}
}

// start brings up a supervisor over the given services.
func (h *harness) start(services ...service.Service) {
	h.t.Helper()
	h.sup = New(h.dirs, h.buffer)
	err := h.sup.Adopt(context.Background(), services)
	if err != nil {
		h.t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.sup.Run(ctx)
}

// stop ends the supervisor the way hostd ends: the loop leaves, the services
// it started keep running. Anything still alive is killed afterwards so a test
// does not leak processes onto the developer's machine.
func (h *harness) stop() {
	h.t.Helper()
	if h.cancel == nil {
		return
	}
	pids := make([]int, 0, 4)
	for _, st := range h.sup.Status() {
		if st.PID != 0 {
			pids = append(pids, st.PID)
		}
	}
	h.cancel()
	select {
	case <-h.sup.Done():
	case <-time.After(20 * time.Second):
		h.t.Fatal("supervisor did not stop")
	}
	h.cancel = nil
	for _, pid := range pids {
		_ = signal(pid, syscall.SIGKILL)
	}
}

// abandon simulates hostd dying: the loop stops and the supervisor object is
// dropped, while the processes it started carry on.
func (h *harness) abandon() {
	h.t.Helper()
	h.cancel()
	select {
	case <-h.sup.Done():
	case <-time.After(20 * time.Second):
		h.t.Fatal("supervisor did not stop")
	}
	h.cancel = nil
	h.sup = nil
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s\nlog:\n%s", what, h.dumpLog())
}

func (h *harness) dumpLog() string {
	var b strings.Builder
	for _, r := range h.buffer.Search(logs.Query{}) {
		b.WriteString("  " + r.Service + " " + r.Stream + ": " + r.Text + "\n")
	}
	return b.String()
}

func (h *harness) status(name string) Status {
	h.t.Helper()
	st, err := h.sup.StatusOf(name)
	if err != nil {
		h.t.Fatalf("StatusOf(%q): %v", name, err)
	}
	return st
}

func (h *harness) logText() string {
	var b strings.Builder
	for _, r := range h.buffer.Search(logs.Query{}) {
		b.WriteString(r.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func shell(name, script string) service.Service {
	return service.Service{
		Name:        name,
		Kind:        service.KindExec,
		Command:     "/bin/sh",
		Args:        []string{"-c", script},
		State:       service.StateRunning,
		Restart:     service.RestartNever,
		StopTimeout: 2,
	}
}

func TestStartsAndCapturesOutput(t *testing.T) {
	h := newHarness(t)
	svc := shell("api", `echo "listening"; echo "warning" >&2; sleep 30`)
	h.start(svc)
	defer h.stop()

	h.waitFor("the service to be running", func() bool {
		return h.status("api").State == StateRunning
	})
	h.waitFor("stdout and stderr to be captured", func() bool {
		out := h.buffer.Search(logs.Query{Service: "api", Stream: logs.StreamOut})
		errs := h.buffer.Search(logs.Query{Service: "api", Stream: logs.StreamErr})
		return len(out) == 1 && out[0].Text == "listening" &&
			len(errs) == 1 && errs[0].Text == "warning"
	})

	st := h.status("api")
	if st.PID == 0 {
		t.Fatal("a running service has no PID")
	}
	if st.Adopted {
		t.Fatal("a service this supervisor started is marked adopted")
	}
}

// The event that a service died and the last lines it wrote have to be
// readable as one timeline, or a failure has to be assembled by hand.
func TestEventsShareTheTimelineWithOutput(t *testing.T) {
	h := newHarness(t)
	h.start(shell("api", `echo "about to fail"; exit 3`))
	defer h.stop()

	h.waitFor("the exit to be recorded", func() bool {
		return strings.Contains(h.logText(), "exited with code 3")
	})
	records := h.buffer.Search(logs.Query{Service: "api"})
	var sawOutput bool
	for _, r := range records {
		if r.Stream == logs.StreamOut && r.Text == "about to fail" {
			sawOutput = true
		}
		if r.Stream == logs.StreamEvent && strings.Contains(r.Text, "exited with code 3") {
			if !sawOutput {
				t.Fatal("the exit event came before the output that explains it")
			}
			return
		}
	}
	t.Fatalf("exit event not found in:\n%s", h.dumpLog())
}

func TestRestartAlways(t *testing.T) {
	h := newHarness(t)
	svc := shell("flapper", `exit 1`)
	svc.Restart = service.RestartAlways
	h.start(svc)
	defer h.stop()

	h.waitFor("the service to be restarted", func() bool {
		return h.status("flapper").Restarts >= 2
	})
}

func TestRestartOnFailureIgnoresCleanExit(t *testing.T) {
	h := newHarness(t)
	svc := shell("once", `exit 0`)
	svc.Restart = service.RestartOnFailure
	h.start(svc)
	defer h.stop()

	h.waitFor("the clean exit to be recorded", func() bool {
		return strings.Contains(h.logText(), "exited normally")
	})
	time.Sleep(500 * time.Millisecond)
	if got := h.status("once").Restarts; got != 0 {
		t.Fatalf("a clean exit was restarted under on-failure: %d restarts", got)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	if backoff(1) != backoffMin {
		t.Fatalf("first backoff = %v, want %v", backoff(1), backoffMin)
	}
	if backoff(2) <= backoff(1) {
		t.Fatal("backoff does not grow")
	}
	// A service that crashes forever must not turn into a busy loop, and must
	// not grow into a wait nobody would sit through either.
	if backoff(50) != backoffMax {
		t.Fatalf("backoff(50) = %v, want the ceiling %v", backoff(50), backoffMax)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.start(shell("api", `sleep 30`))
	defer h.stop()

	h.waitFor("running", func() bool { return h.status("api").State == StateRunning })
	for range 3 {
		err := h.sup.Stop("api")
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
	h.waitFor("stopped", func() bool { return h.status("api").State == StateStopped })
	if pid := h.status("api").PID; pid != 0 {
		t.Fatalf("a stopped service still reports PID %d", pid)
	}
}

func TestKillsWhatWillNotStop(t *testing.T) {
	h := newHarness(t)
	// A service that ignores SIGTERM must still go away, or a stop would
	// depend on the service's good behaviour.
	svc := shell("stubborn", `trap "" TERM; while true; do sleep 0.1; done`)
	svc.StopTimeout = 1
	h.start(svc)
	defer h.stop()

	h.waitFor("running", func() bool { return h.status("stubborn").State == StateRunning })
	err := h.sup.Stop("stubborn")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	h.waitFor("the stubborn service to be killed", func() bool {
		return h.status("stubborn").State == StateStopped
	})
	if !strings.Contains(h.logText(), "killed") {
		t.Fatalf("the kill was not reported:\n%s", h.dumpLog())
	}
}

func TestStartStopAreDeclarative(t *testing.T) {
	h := newHarness(t)
	svc := shell("api", `sleep 30`)
	svc.State = service.StateStopped
	h.start(svc)
	defer h.stop()

	// Declared stopped means it never starts, however long the loop runs.
	time.Sleep(300 * time.Millisecond)
	if st := h.status("api"); st.State != StateStopped || st.PID != 0 {
		t.Fatalf("a service declared stopped was started: %+v", st)
	}

	err := h.sup.Start("api")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.waitFor("running after an explicit start", func() bool {
		return h.status("api").State == StateRunning
	})
	first := h.status("api").PID

	// Asking again changes nothing: ten requests, one process.
	for range 10 {
		_ = h.sup.Start("api")
	}
	time.Sleep(300 * time.Millisecond)
	if got := h.status("api").PID; got != first {
		t.Fatalf("repeating start replaced the process: %d then %d", first, got)
	}
}

func TestUnknownServiceSaysHowToLookItUp(t *testing.T) {
	h := newHarness(t)
	h.start()
	defer h.stop()

	err := h.sup.Start("ghost")
	if err == nil {
		t.Fatal("an undeclared service was started")
	}
	if !strings.Contains(err.Error(), "hostctl service list") {
		t.Fatalf("the error does not tell the operator what to do: %v", err)
	}
}

// This is the property that makes updating hostd a safe operation: the
// supervisor goes away, the services do not, and the next supervisor finds
// them again and proves they are the same processes.
func TestServicesSurviveSupervisorRestartAndAreAdopted(t *testing.T) {
	h := newHarness(t)
	h.start(shell("api", `echo started; sleep 30`))
	h.waitFor("running", func() bool { return h.status("api").State == StateRunning })
	original := h.status("api").PID

	h.abandon()

	// The process is still there with hostd gone.
	if !procid.Matches(original, mustToken(t, original)) {
		t.Fatal("the service died with its supervisor")
	}

	h.start(shell("api", `echo started; sleep 30`))
	defer h.stop()
	h.waitFor("the process to be adopted", func() bool {
		st := h.status("api")
		return st.State == StateRunning && st.PID == original && st.Adopted
	})
	if !strings.Contains(h.logText(), "adopted running process") {
		t.Fatalf("adoption was not reported:\n%s", h.dumpLog())
	}

	// Killing it now must still be noticed, so an adopted process is really
	// supervised and not merely counted.
	err := h.sup.Stop("api")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	h.waitFor("the adopted process to stop", func() bool {
		return h.status("api").State == StateStopped
	})
}

// Output written while no supervisor was running is not lost: it is in the
// spool, and the next supervisor reads it from the recorded offset.
func TestOutputWrittenWhileAwayIsCaptured(t *testing.T) {
	h := newHarness(t)
	h.start(shell("api", `echo first; sleep 0.2; echo while-away; sleep 30`))
	h.waitFor("the first line", func() bool {
		return strings.Contains(h.logText(), "first")
	})
	h.abandon()

	// Give the service time to write with nobody watching.
	time.Sleep(600 * time.Millisecond)

	h.buffer = logs.NewBuffer(2000)
	h.start(shell("api", `echo first; sleep 0.2; echo while-away; sleep 30`))
	defer h.stop()
	h.waitFor("the line written while hostd was away", func() bool {
		return strings.Contains(h.logText(), "while-away")
	})
}

// A service whose file disappeared while hostd was away leaves a running
// process nobody declares. Reporting it is the requirement.
func TestOrphanIsReported(t *testing.T) {
	h := newHarness(t)
	h.start(shell("api", `sleep 30`))
	h.waitFor("running", func() bool { return h.status("api").State == StateRunning })
	pid := h.status("api").PID
	h.abandon()

	// Restart with the service no longer declared.
	h.start()
	defer func() {
		_ = signal(pid, 9)
		h.stop()
	}()
	h.waitFor("the orphan to be reported", func() bool {
		return strings.Contains(h.logText(), "orphan process")
	})
}

// A service that died while hostd was away must be noticed on the way back,
// with the record saying it was observed later rather than seen happen.
func TestDeathWhileAwayIsNoticed(t *testing.T) {
	h := newHarness(t)
	h.start(shell("api", `sleep 30`))
	h.waitFor("running", func() bool { return h.status("api").State == StateRunning })
	pid := h.status("api").PID
	h.abandon()

	// The service dies with nobody watching, which is the case that a
	// supervisor coming back has to notice on its own.
	err := signal(pid, syscall.SIGKILL)
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	h.waitFor("the process to be gone", func() bool {
		_, tokenErr := procid.Token(pid)
		return tokenErr != nil
	})

	h.buffer = logs.NewBuffer(2000)
	svc := shell("api", `sleep 30`)
	svc.Restart = service.RestartNever
	h.start(svc)
	defer h.stop()

	if !strings.Contains(h.logText(), "was not running when hostd came back") {
		t.Fatalf("a death during the gap was not noticed:\n%s", h.dumpLog())
	}
	st := h.status("api")
	if !strings.Contains(st.LastError, "exited while hostd was not running") {
		t.Fatalf("status does not say what happened: %+v", st)
	}
}

// hostd going away is not the machine going away. This is the promise that
// makes updating the daemon something an operator will actually do.
func TestSupervisorExitLeavesServicesRunning(t *testing.T) {
	h := newHarness(t)
	h.start(shell("api", `sleep 30`), shell("web", `sleep 30`))
	h.waitFor("both running", func() bool {
		return h.status("api").State == StateRunning && h.status("web").State == StateRunning
	})
	apiPID := h.status("api").PID
	webPID := h.status("web").PID

	h.abandon()
	defer func() {
		_ = signal(apiPID, syscall.SIGKILL)
		_ = signal(webPID, syscall.SIGKILL)
	}()

	for _, pid := range []int{apiPID, webPID} {
		_, err := procid.Token(pid)
		if err != nil {
			t.Errorf("process %d was killed when the supervisor left: %v", pid, err)
		}
	}
}

func TestApplyKeepsUnchangedServicesRunning(t *testing.T) {
	h := newHarness(t)
	svc := shell("api", `sleep 30`)
	h.start(svc)
	defer h.stop()

	h.waitFor("running", func() bool { return h.status("api").State == StateRunning })
	before := h.status("api").PID

	changes := h.sup.Apply([]service.Service{svc})
	if len(changes) != 0 {
		t.Fatalf("applying the same definition reported changes: %v", changes)
	}
	time.Sleep(300 * time.Millisecond)
	if after := h.status("api").PID; after != before {
		t.Fatalf("an unchanged service was restarted: %d then %d", before, after)
	}
}

func TestApplyRestartsChangedAndAddsAndRemoves(t *testing.T) {
	h := newHarness(t)
	h.start(shell("api", `sleep 30`))
	defer h.stop()
	h.waitFor("running", func() bool { return h.status("api").State == StateRunning })
	before := h.status("api").PID

	changed := shell("api", `sleep 60`)
	added := shell("web", `sleep 30`)
	changes := h.sup.Apply([]service.Service{changed, added})
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "api: definition changed") || !strings.Contains(joined, "web: added") {
		t.Fatalf("apply did not report what it did: %v", changes)
	}
	h.waitFor("the changed service to be replaced", func() bool {
		st := h.status("api")
		return st.State == StateRunning && st.PID != before
	})
	h.waitFor("the added service to run", func() bool {
		return h.status("web").State == StateRunning
	})

	changes = h.sup.Apply([]service.Service{changed})
	if !strings.Contains(strings.Join(changes, "\n"), "web: no longer declared") {
		t.Fatalf("removal was not reported: %v", changes)
	}
	h.waitFor("the removed service to stop", func() bool {
		st, err := h.sup.StatusOf("web")
		return err != nil || st.State == StateStopped
	})
}

// A service that cannot start at all has to say so, and keep saying so.
func TestFailingStartIsVisible(t *testing.T) {
	h := newHarness(t)
	svc := service.Service{
		Name:    "missing",
		Kind:    service.KindExec,
		Command: "/nonexistent/binary",
		State:   service.StateRunning,
		Restart: service.RestartAlways,
	}
	h.start(svc)
	defer h.stop()

	h.waitFor("the failure to be visible", func() bool {
		st := h.status("missing")
		return st.State == StateFailed && st.LastError != ""
	})
	if !strings.Contains(h.logText(), "start failed") {
		t.Fatalf("the failure was not reported:\n%s", h.dumpLog())
	}
}

func TestStateFileIsRemovedWhenServiceStops(t *testing.T) {
	h := newHarness(t)
	h.start(shell("api", `sleep 30`))
	defer h.stop()
	h.waitFor("running", func() bool { return h.status("api").State == StateRunning })

	path := statePath(h.dirs.State, "api")
	_, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no supervision record for a running service: %v", err)
	}
	err = h.sup.Stop("api")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	h.waitFor("the supervision record to be removed", func() bool {
		_, statErr := os.Stat(path)
		return os.IsNotExist(statErr)
	})
}

// Unreadable supervision state is reported rather than skipped: it means
// processes that cannot be adopted.
func TestUnreadableStateIsReported(t *testing.T) {
	h := newHarness(t)
	err := os.MkdirAll(h.dirs.State, 0o700)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err = os.WriteFile(statePath(h.dirs.State, "api"), []byte("(this is not"), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	sup := New(h.dirs, h.buffer)
	err = sup.Adopt(context.Background(), []service.Service{shell("api", `sleep 1`)})
	if err == nil {
		t.Fatal("unreadable supervision state was accepted silently")
	}
	if !strings.Contains(err.Error(), "unreadable supervision state") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustToken(t *testing.T, pid int) string {
	t.Helper()
	token, err := procid.Token(pid)
	if err != nil {
		t.Fatalf("Token(%d): %v", pid, err)
	}
	return token
}
