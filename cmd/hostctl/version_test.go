package main

import (
	"strings"
	"testing"

	"github.com/crgimenes/hostd/daemon"
	"github.com/crgimenes/hostd/version"
)

// The ordinary case, and the one the command exists for: a machine on an older
// daemon than the client carries, named together with the command that fixes it.
func TestAMachineOnAnOlderDaemonIsToldWhatToRun(t *testing.T) {
	answer := compareVersions("yuki.local", "v0.0.3", "v0.0.4")
	if answer.State != stateBehind {
		t.Fatalf("state = %q, want %q", answer.State, stateBehind)
	}
	if !strings.Contains(answer.Advice, "hostctl -host yuki.local install") {
		t.Fatalf("the advice does not name the command: %q", answer.Advice)
	}
}

// The inverted case, which is the whole reason this is a comparison and not a
// list: the machine is fine and the CLIENT is the old one. A report that called
// this "up to date" would be the last thing said before an -all install walked
// the fleet backwards.
func TestAMachineAheadMeansTheClientIsTheOldOne(t *testing.T) {
	answer := compareVersions("selene.local", "v0.1.0", "v0.0.4")
	if answer.State != stateAhead {
		t.Fatalf("state = %q, want %q", answer.State, stateAhead)
	}
	if !strings.Contains(answer.Advice, "this hostctl is the old one") {
		t.Fatalf("the advice blames the wrong side: %q", answer.Advice)
	}
	if strings.Contains(sentenceFor(answer), "upgrade it") {
		t.Fatalf("the machine is told to upgrade when it is the client that must: %q", sentenceFor(answer))
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

// A development build of either side is not ranked. Saying so beats saying
// "behind", which would send somebody to install a daemon this binary does not
// even carry.
func TestWhatCannotBeComparedSaysSo(t *testing.T) {
	dev := compareVersions("yuki.local", "dev", "v0.0.4")
	if dev.State != stateUnknown || !strings.Contains(dev.Advice, "not both release versions") {
		t.Fatalf("a dev daemon was ranked: %+v", dev)
	}
	none := compareVersions("yuki.local", "v0.0.4", "")
	if none.State != stateUnknown || !strings.Contains(none.Advice, "carries no daemon") {
		t.Fatalf("a hostctl carrying nothing did not say so: %+v", none)
	}
}

// An old client installing over a newer daemon is a downgrade wearing the word
// upgrade. Caught before anything is sent, because after the restart it has
// already happened.
func TestInstallingBackwardsIsRefusedBeforeAnythingIsSent(t *testing.T) {
	backwards, refusal := goingBackwards("yuki.local", "hostd v0.1.0 (protocol 1, schema 1)", "v0.0.4")
	if !backwards {
		t.Fatal("installing v0.0.4 over v0.1.0 was not seen as going backwards")
	}
	for _, want := range []string{"yuki.local", "v0.1.0", "v0.0.4", "update hostctl"} {
		if !strings.Contains(refusal.Error(), want) {
			t.Fatalf("the refusal does not carry %q: %v", want, refusal)
		}
	}
}

// Refusing on a guess would block the install that repairs a machine answering
// nonsense, and a first install has nothing to go back from.
func TestOnlyAKnownDowngradeIsRefused(t *testing.T) {
	for _, c := range []struct {
		why     string
		before  string
		carried string
	}{
		{"nothing installed yet", "", "v0.0.4"},
		{"a development hostctl", "hostd v0.1.0 (protocol 1, schema 1)", ""},
		{"a machine answering nonsense", "something went wrong", "v0.0.4"},
		{"a daemon on a development build", "hostd dev (protocol 1, schema 1)", "v0.0.4"},
		{"an ordinary upgrade", "hostd v0.0.3 (protocol 1, schema 1)", "v0.0.4"},
		{"the same version again", "hostd v0.0.4 (protocol 1, schema 1)", "v0.0.4"},
	} {
		backwards, _ := goingBackwards("yuki.local", c.before, c.carried)
		if backwards {
			t.Errorf("%s was refused as a downgrade", c.why)
		}
	}
}

// Calling a downgrade an upgrade is the kind of small lie that gets believed
// later, when somebody reads the line and stops looking.
func TestTheTransitionNamesADowngradeAsOne(t *testing.T) {
	if got := transition("hostd v0.1.0 (protocol 1, schema 1)", "v0.0.4"); got != "downgraded from v0.1.0" {
		t.Fatalf("transition = %q", got)
	}
	if got := transition("hostd v0.0.3 (protocol 1, schema 1)", "v0.0.4"); got != "upgraded from v0.0.3" {
		t.Fatalf("transition = %q", got)
	}
	if got := transition("", "v0.0.4"); got != "installed" {
		t.Fatalf("transition = %q", got)
	}
}

// A development stamp cannot be ranked, but two identical ones are plainly the
// same build. Answering "cannot tell" to "is this the same daemon?" would be
// refusing to read what is in front of it — and a fleet installed from a dev
// build is the case where every machine would say it.
func TestTheSameStampOnBothSidesIsTheSameDaemon(t *testing.T) {
	answer := compareVersions("yuki.local", "v0.0.3-1-gf5059ca-dirty", "v0.0.3-1-gf5059ca-dirty")
	if answer.State != stateCurrent {
		t.Fatalf("state = %q, want %q: %+v", answer.State, stateCurrent, answer)
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
