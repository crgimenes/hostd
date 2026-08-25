package main

import (
	"strings"
	"testing"

	"github.com/crgimenes/hostd/supervisor"
)

func wasRunning(name string, pid int) supervisor.Status {
	return supervisor.Status{Name: name, State: supervisor.StateRunning, PID: pid}
}

// The promise: replacing hostd does not stop or restart what it supervises.
// Same names, same PIDs, nothing to say.
func TestAnUpgradeThatKeptEverythingSaysNothing(t *testing.T) {
	before := []supervisor.Status{wasRunning("caddy", 100), wasRunning("site", 200)}
	after := []supervisor.Status{wasRunning("site", 200), wasRunning("caddy", 100)}

	lost := lostServices(before, after)
	if len(lost) != 0 {
		t.Fatalf("an upgrade that changed nothing complained: %v", lost)
	}
}

// The failure this check exists for: the daemon came back and the work did not.
// Before it existed, this was reported as a good upgrade.
func TestAServiceThatStoppedIsReportedWithItsOldPID(t *testing.T) {
	before := []supervisor.Status{wasRunning("caddy", 100)}
	after := []supervisor.Status{{Name: "caddy", State: supervisor.StateStopped}}

	lost := lostServices(before, after)
	if len(lost) != 1 {
		t.Fatalf("a service that stopped produced %d complaint(s): %v", len(lost), lost)
	}
	// The old PID is what an operator greps the journal with.
	if !strings.Contains(lost[0], "caddy") || !strings.Contains(lost[0], "100") {
		t.Fatalf("the complaint does not name the service and its pid: %q", lost[0])
	}
}

// A container that came back under a new PID is the same promise broken, just
// less visibly: the process serving requests is not the one that was, and
// whatever it held went with it.
func TestAContainerThatWasReplacedIsNotSilentlyAccepted(t *testing.T) {
	before := []supervisor.Status{wasRunning("api", 100)}
	after := []supervisor.Status{wasRunning("api", 999)}

	lost := lostServices(before, after)
	if len(lost) != 1 {
		t.Fatalf("a replaced container produced %d complaint(s): %v", len(lost), lost)
	}
	if !strings.Contains(lost[0], "replaced") {
		t.Fatalf("the complaint does not say the container was replaced: %q", lost[0])
	}
}

// A machine that stopped declaring the service at all is worse than one where
// it stopped, and reads differently to whoever is looking.
func TestAServiceTheMachineNoLongerDeclaresIsSaidSo(t *testing.T) {
	before := []supervisor.Status{wasRunning("caddy", 100)}

	lost := lostServices(before, nil)
	if len(lost) != 1 {
		t.Fatalf("a vanished service produced %d complaint(s): %v", len(lost), lost)
	}
	if !strings.Contains(lost[0], "no longer declares") {
		t.Fatalf("the complaint does not distinguish vanished from stopped: %q", lost[0])
	}
}

// A job between runs has no PID and is not running, which is exactly what it
// should look like. Demanding it be found running would fail every machine that
// has a scheduled job on it.
func TestAJobBetweenRunsIsNotMistakenForLostWork(t *testing.T) {
	before := []supervisor.Status{
		{Name: "backup", State: supervisor.StateScheduled, Every: "1h"},
		wasRunning("caddy", 100),
	}
	after := []supervisor.Status{
		{Name: "backup", State: supervisor.StateScheduled, Every: "1h"},
		wasRunning("caddy", 100),
	}

	lost := lostServices(before, after)
	if len(lost) != 0 {
		t.Fatalf("a scheduled job was counted as lost work: %v", lost)
	}
}

// A machine that gained a service between the two readings did not lose one.
// Only what was running before is something there was a promise about.
func TestAServiceThatAppearedIsNotAComplaint(t *testing.T) {
	before := []supervisor.Status{wasRunning("caddy", 100)}
	after := []supervisor.Status{wasRunning("caddy", 100), wasRunning("new", 300)}

	lost := lostServices(before, after)
	if len(lost) != 0 {
		t.Fatalf("a newly started service was reported as a loss: %v", lost)
	}
}

// A first install has nothing to compare against, and inventing a complaint
// would make every new machine look broken.
func TestAMachineWithNothingRunningHasNothingToLose(t *testing.T) {
	if lost := lostServices(nil, nil); len(lost) != 0 {
		t.Fatalf("a machine with no services complained: %v", lost)
	}
	if kept(nil) != "no service was running" {
		t.Fatalf("the report does not say the promise went untested: %q", kept(nil))
	}
}
