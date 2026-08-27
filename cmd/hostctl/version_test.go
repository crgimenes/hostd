package main

import (
	"strings"
	"testing"

	"github.com/crgimenes/hostd/daemon"
	"github.com/crgimenes/hostd/version"
)

// The ordinary case, and the one the command exists for: a machine not on the
// daemon this client carries, named together with the command that changes it.
func TestAMachineOnAnotherDaemonIsToldWhatToRun(t *testing.T) {
	answer := compareVersions("yuki.local", "v0.0.3", "v0.0.4")
	if answer.State != stateDiffers {
		t.Fatalf("state = %q, want %q", answer.State, stateDiffers)
	}
	if !strings.Contains(answer.Advice, "hostctl -host yuki.local install") {
		t.Fatalf("the advice does not name the command: %q", answer.Advice)
	}
}

// Neither end ranks versions (crg, 2026-08-27), so the newer daemon reads as a
// difference like any other — and the operator may well be putting an older one
// back on purpose, because the newest is what made the machine amber. What is
// NOT allowed is a sentence claiming a direction.
func TestAMachineOnANewerDaemonIsJustADifference(t *testing.T) {
	answer := compareVersions("selene.local", "v0.1.0", "v0.0.4")
	if answer.State != stateDiffers {
		t.Fatalf("state = %q, want %q", answer.State, stateDiffers)
	}
	sentence := sentenceFor(answer)
	for _, claim := range []string{"behind", "ahead", "upgrade", "downgrade", "older", "newer"} {
		if strings.Contains(sentence, claim) {
			t.Fatalf("the sentence claims an order with %q: %q", claim, sentence)
		}
	}
	if !strings.Contains(sentence, "v0.1.0") || !strings.Contains(sentence, "v0.0.4") {
		t.Fatalf("the sentence does not name both versions: %q", sentence)
	}
}

func TestAMachineOnTheCarriedVersionNeedsNothing(t *testing.T) {
	answer := compareVersions("m1.local", "v0.0.4", "v0.0.4")
	if answer.State != stateCurrent {
		t.Fatalf("state = %q, want %q", answer.State, stateCurrent)
	}
	if answer.Advice != "" {
		t.Fatalf("a machine with nothing to do was given advice: %q", answer.Advice)
	}
}

// A hostctl carrying no daemon has nothing to compare against, which is a
// different answer from "they differ" — there is no second version.
func TestAClientCarryingNoDaemonSaysSo(t *testing.T) {
	none := compareVersions("yuki.local", "v0.0.4", "")
	if none.State != stateUnknown || !strings.Contains(none.Advice, "carries no daemon") {
		t.Fatalf("a hostctl carrying nothing did not say so: %+v", none)
	}
}

// A development stamp is compared like any other string: two identical ones are
// the same build, and that is the only question being asked.
func TestADevelopmentStampIsComparedLikeAnyOther(t *testing.T) {
	same := compareVersions("yuki.local", "v0.0.3-1-gf5059ca-dirty", "v0.0.3-1-gf5059ca-dirty")
	if same.State != stateCurrent {
		t.Fatalf("state = %q, want %q: %+v", same.State, stateCurrent, same)
	}
	other := compareVersions("yuki.local", "dev", "v0.0.4")
	if other.State != stateDiffers {
		t.Fatalf("state = %q, want %q: %+v", other.State, stateDiffers, other)
	}
}

// The zips are written by `make` and left alone by a bare `go build`, so two
// disagreeing stamps mean the embedded daemon is from another build — which is
// how a correctly absent amber button cost a real hunt (crg, 2026-08-27).
func TestADaemonFromAnotherBuildIsReported(t *testing.T) {
	carried := daemon.Version()
	if carried == "" {
		t.Skip("this build carries no daemon, so there is nothing to disagree with")
	}
	was := version.Version
	t.Cleanup(func() { version.Version = was })

	version.Version = carried
	if staleDaemon() != "" {
		t.Fatalf("the same stamp was reported as another build: %q", staleDaemon())
	}

	version.Version = carried + "-something-else"
	note := staleDaemon()
	if note == "" {
		t.Fatal("a daemon from another build went unreported")
	}
	if !strings.Contains(note, carried) || !strings.Contains(note, version.Version) {
		t.Fatalf("the note names neither stamp: %q", note)
	}
}
