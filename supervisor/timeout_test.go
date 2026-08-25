package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/service"
)

// The rule the whole timeout rests on: a run's allowance is measured from when
// its CONTAINER started, not from when this daemon began watching it.
func TestARunAdoptedFromAnotherDaemonKeepsTheClockItStartedOn(t *testing.T) {
	now := time.Now()
	const limit = time.Hour

	// Started under this daemon, a moment ago: nearly the whole hour left.
	fresh := killAfter(now.Add(-time.Minute), now, limit)
	if fresh < 58*time.Minute || fresh > limit {
		t.Fatalf("a run one minute old has %s left of an hour", fresh)
	}

	// Adopted after an upgrade, fifty minutes in: ten left, NOT a fresh hour.
	// Getting this wrong is what would let a hung run survive for ever by
	// being handed a new allowance on every daemon restart.
	adopted := killAfter(now.Add(-50*time.Minute), now, limit)
	if adopted < 9*time.Minute || adopted > 11*time.Minute {
		t.Fatalf("a run fifty minutes into an hour has %s left", adopted)
	}

	// Already past it when this daemon found it: stopped at once, never a
	// negative delay that a timer would read as "never".
	late := killAfter(now.Add(-2*time.Hour), now, limit)
	if late != 0 {
		t.Fatalf("a run two hours into an hour has %s left, not 0", late)
	}
}

// A job that hangs is the failure this exists for: without a bound it holds a
// place for ever, the ceiling fills with runs that will never finish, every
// turn after that is skipped, and the job quietly stops happening while the
// service still reads as scheduled.
func TestAJobRunThatHangsIsStoppedAtItsLimit(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	// Its own name, and its own cleanup: a job leaves a container per RUN, and
	// the shared helper only knows about the one named after the service. A
	// suite that leaves those behind poisons whatever test runs next under the
	// same name — which is how this one first failed.
	const job = "hostd-suite-timeout"
	cleanupRuns(t, client, job)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	// Sleeps far past its limit and would otherwise never end.
	svc := container(job, image, `echo working; sleep 600`)
	svc.Every = "1s"
	svc.RunTimeout = "3s"

	err := h.sup.Adopt(context.Background(), []service.Service{svc})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.sup.Run(ctx)
	defer h.stop()

	h.waitFor("a run to start", func() bool {
		return strings.Contains(h.logText(), "started as container")
	})
	// Said before the exit code arrives: on its own that code reads as the job
	// failing rather than as this deciding it had had long enough.
	h.waitFor("the run to be stopped for overrunning", func() bool {
		return strings.Contains(h.logText(), "passed its run-timeout")
	})
	h.waitFor("the run to actually end", func() bool {
		return strings.Contains(h.logText(), "finished with exit")
	})
}

// Removes every container the runtime is holding for one job, runs included.
func cleanupRuns(t *testing.T, client *docker.Client, name string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		held, err := client.List(ctx, labelService+"="+name)
		if err != nil {
			return
		}
		for _, one := range held {
			_ = client.Remove(ctx, one.ID)
		}
		_ = client.Remove(ctx, containerName(name))
	})
}
