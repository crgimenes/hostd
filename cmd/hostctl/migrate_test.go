package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/service"
)

func reachable(host string, wanted, holds bool) placement {
	return placement{
		host:     host,
		answered: true,
		wanted:   wanted,
		holds:    holds,
		describe: api.Description{Arch: "amd64", Runtime: "26.1.5", CPUs: 4, MemoryBytes: 8 << 30},
	}
}

// The move the whole command exists for: the tree now says one machine, another
// one is still running it.
func TestAMoveIsTheTreeDisagreeingWithTheFleet(t *testing.T) {
	plan, _, err := planMove("api", []placement{
		reachable("yuki", false, true),
		reachable("m1", true, false),
		reachable("selene", false, false),
	})
	if err != nil {
		t.Fatalf("planMove: %v", err)
	}
	if plan.from.host != "yuki" || plan.to.host != "m1" {
		t.Fatalf("moving from %s to %s", plan.from.host, plan.to.host)
	}
}

// A machine that did not answer may be the one running it. Deciding on that
// picture is how a service ends up running in two places at once, which for
// anything holding data is the worst outcome of the three.
func TestASilentMachineStopsTheMoveRatherThanBeingReadAsEmpty(t *testing.T) {
	_, code, err := planMove("api", []placement{
		{host: "yuki", answered: false, problem: "connection timed out"},
		reachable("m1", true, false),
	})
	if err == nil {
		t.Fatal("a migration went ahead without knowing where the service runs")
	}
	// The fleet could not be read; nothing was wrong with the request. An agent
	// that reads this as bad arguments never retries, when retrying once the
	// machine is back is exactly the right move.
	if code != exitComms {
		t.Fatalf("an unreachable machine came back as exit %d, want %d", code, exitComms)
	}
	if !strings.Contains(err.Error(), "yuki") {
		t.Fatalf("the refusal does not name the machine that went quiet: %v", err)
	}
	if !strings.Contains(err.Error(), "second copy") {
		t.Fatalf("the refusal does not say what the risk is: %v", err)
	}
}

// Each refusal has to name the operation that IS right, or the operator is left
// knowing only that this was not it.
func TestEveryRefusalNamesTheRightOperation(t *testing.T) {
	cases := map[string]struct {
		found []placement
		says  string
	}{
		"the tree places it nowhere": {
			found: []placement{reachable("yuki", false, true)},
			says:  "apply -allow-destructive",
		},
		"it is not running anywhere yet": {
			found: []placement{reachable("m1", true, false)},
			says:  "push and apply",
		},
	}
	for name, one := range cases {
		t.Run(name, func(t *testing.T) {
			_, code, err := planMove("api", one.found)
			if err == nil {
				t.Fatal("this is not a migration and was accepted as one")
			}
			// The fleet answered; what was asked for is not a migration.
			if code != exitUsage {
				t.Fatalf("came back as exit %d, want %d", code, exitUsage)
			}
			if !strings.Contains(err.Error(), one.says) {
				t.Fatalf("the refusal does not say what to do instead: %v", err)
			}
		})
	}
}

// Two copies running is a state to sort out by hand: choosing which one is real
// is not a decision a tool should make silently.
func TestTwoCopiesRunningIsRefused(t *testing.T) {
	_, _, err := planMove("api", []placement{
		reachable("yuki", false, true),
		reachable("selene", false, true),
		reachable("m1", true, false),
	})
	if err == nil {
		t.Fatal("a service running in two places was migrated anyway")
	}
	if !strings.Contains(err.Error(), "yuki") || !strings.Contains(err.Error(), "selene") {
		t.Fatalf("the refusal does not name both machines: %v", err)
	}
}

// Refused before anything stops. A migration that learns at the far end is one
// that took a service down to find out something it could have asked first.
func TestAMachineWithNoRuntimeIsRefusedBeforeAnythingStops(t *testing.T) {
	to := placement{host: "cronos", answered: true, wanted: true}
	err := destinationCanHostIt(to, service.Service{Name: "api"})
	if err == nil {
		t.Fatal("a machine that cannot run containers was accepted as a destination")
	}
	if !strings.Contains(err.Error(), "cronos") {
		t.Fatalf("the refusal does not name the machine: %v", err)
	}
}

func TestADestinationWithTooLittleMemoryIsRefused(t *testing.T) {
	to := reachable("small", true, false)
	to.describe.MemoryBytes = 512 << 20
	err := destinationCanHostIt(to, service.Service{Name: "api", Memory: 1024})
	if err == nil {
		t.Fatal("a machine with less memory than the service declares was accepted")
	}
}

// A machine whose sampler has not run reports zero, which is not a claim that
// it has no memory. Refusing on a number nobody gave would block a move for a
// missing sample.
func TestAMachineThatDidNotSayItsMemoryIsNotRefusedForIt(t *testing.T) {
	to := reachable("quiet", true, false)
	to.describe.MemoryBytes = 0
	err := destinationCanHostIt(to, service.Service{Name: "api", Memory: 1024})
	if err != nil {
		t.Fatalf("a machine was refused over a number it never gave: %v", err)
	}
}

// Data stays behind, and the operator has to be told before the service is
// stopped rather than after it starts empty somewhere else.
func TestTheVolumesThatStayBehindAreNamed(t *testing.T) {
	var out bytes.Buffer
	describeMove(&out, "db", move{from: reachable("yuki", false, true), to: reachable("m1", true, false)},
		service.Service{Name: "db", Volumes: []string{"data:/var/lib/postgresql/data"}})
	text := out.String()
	if !strings.Contains(text, "do NOT travel") {
		t.Fatalf("the warning about data is missing:\n%s", text)
	}
	if !strings.Contains(text, "data:/var/lib/postgresql/data") {
		t.Fatalf("the volume that stays behind is not named:\n%s", text)
	}
	if !strings.Contains(text, "empty storage") {
		t.Fatalf("it does not say what the destination will start with:\n%s", text)
	}
}

// A service with no storage should not be given a warning about storage.
func TestAServiceWithNoVolumesGetsNoDataWarning(t *testing.T) {
	var out bytes.Buffer
	describeMove(&out, "api", move{from: reachable("yuki", false, true), to: reachable("m1", true, false)},
		service.Service{Name: "api"})
	if strings.Contains(out.String(), "do NOT travel") {
		t.Fatalf("a stateless service was warned about data:\n%s", out.String())
	}
}

// The two ways a stop can fail are not the same news, and this is the one that
// bit on the bench: the request took effect and lost its answer on the way
// back. Reporting that as "nothing was moved" sends an operator looking for a
// running service that is not running.
func TestAStopThatLostItsAnswerIsNotReportedAsNeverHappening(t *testing.T) {
	cause := errors.New("closed the connection without answering")

	code, err := stopFailure("api", "m1", true, cause)
	if code != exitPartial {
		t.Fatalf("a service that IS stopped came back as exit %d, want %d", code, exitPartial)
	}
	if !strings.Contains(err.Error(), "is stopped") {
		t.Fatalf("the report does not say the service is down: %v", err)
	}
	if !strings.Contains(err.Error(), "run this again") {
		t.Fatalf("the report does not say how to finish the move: %v", err)
	}

	code, err = stopFailure("api", "m1", false, cause)
	if code != exitFailed {
		t.Fatalf("a service still running came back as exit %d, want %d", code, exitFailed)
	}
	if !strings.Contains(err.Error(), "nothing was moved") {
		t.Fatalf("the report does not say the fleet is untouched: %v", err)
	}
}
