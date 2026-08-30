package main

import (
	"strings"
	"testing"
)

// What an operator is asking when they run this: did anything change, and from
// what. A bare "ok" leaves them re-running it to find out.
func TestTheInstallReportsWhatChanged(t *testing.T) {
	cases := map[string]struct {
		before string
		now    string
		want   string
	}{
		"a machine with no daemon yet": {
			before: "",
			now:    "v0.0.3",
			want:   "installed",
		},
		"a machine already at this version": {
			before: "hostd v0.0.3 (protocol 1, schema 1)",
			now:    "v0.0.3",
			want:   "unchanged",
		},
		"a change names what it replaced": {
			before: "hostd v0.0.1 (protocol 1, schema 1)",
			now:    "v0.0.3",
			want:   "replaced v0.0.1",
		},
		// No direction is claimed (crg, 2026-08-27): naming one would need an
		// order this program does not compute, and an older daemon going back on
		// purpose is a legitimate install.
		"a newer daemon replaced by an older one, with no word about direction": {
			before: "hostd v0.1.0 (protocol 1, schema 1)",
			now:    "v0.0.3",
			want:   "replaced v0.1.0",
		},
		// A machine can answer anything at all; indexing into it would turn a
		// strange banner into a panic in the middle of a fleet-wide install.
		"an answer nothing can be read out of": {
			before: "???",
			now:    "v0.0.3",
			want:   "replaced",
		},
		// It used to be read as the version "went", because only the field count
		// was checked.
		"an answer with enough words to look like one": {
			before: "something went wrong",
			now:    "v0.0.3",
			want:   "replaced",
		},
	}
	for name, one := range cases {
		t.Run(name, func(t *testing.T) {
			got := transition(one.before, one.now)
			if got != one.want {
				t.Fatalf("transition(%q, %q) = %q, want %q", one.before, one.now, got, one.want)
			}
		})
	}
}

// hostd targets 64-bit x86 and ARM, and uname spells them differently from Go.
// A machine that is neither is refused with its own name in the message.
func TestTheArchitectureMapCoversWhatAReleaseCarries(t *testing.T) {
	for machine, want := range map[string]string{
		"Linux x86_64":    "amd64",
		"Linux aarch64":   "arm64",
		"Linux arm64":     "arm64",
		"Linux x86_64\n":  "amd64",
		" Linux aarch64 ": "arm64",
	} {
		got, err := hostdArch(machine)
		if err != nil {
			t.Errorf("hostdArch(%q): %v", machine, err)
			continue
		}
		if got != want {
			t.Errorf("hostdArch(%q) = %q, want %q", machine, got, want)
		}
	}

	// The 32-bit ARM of an older board is the realistic wrong answer here, and
	// it has to come back naming itself.
	_, err := hostdArch("Linux armv7l")
	if err == nil {
		t.Fatal("a machine hostd does not target was accepted")
	}
	if !strings.Contains(err.Error(), "armv7l") {
		t.Fatalf("the refusal does not name what the machine said: %v", err)
	}
}

// The daemon has to come out of the archive the release publishes, not out of
// a path or a download. A development build carries none, and the message has
// to say that is why rather than reading as a broken machine.
func TestTheEmbeddedDaemonIsAWholeBinaryOrAClearRefusal(t *testing.T) {
	binary, err := daemonBinary("amd64")
	if err != nil {
		if !strings.Contains(err.Error(), "make dist") {
			t.Fatalf("the refusal does not say how a build gets the daemon: %v", err)
		}
		t.Skip("development build; the carried-daemon path needs make dist")
	}
	// ELF, because what is sent has to be a Linux binary whatever this machine
	// is: an operator on macOS installs onto Linux, always.
	if len(binary) < 4 || string(binary[:4]) != "\x7fELF" {
		t.Fatalf("the carried daemon is not an ELF binary (%d bytes)", len(binary))
	}
}

// The scratch directory is the one string this command takes from the far
// machine and then interpolates into a shell command running there. Everything
// refused here is refused rather than repaired: a value that has to be cleaned
// up is a value nobody can reason about.
func TestTheScratchPathIsRefusedRatherThanRepaired(t *testing.T) {
	accepted := map[string]string{
		"/tmp/tmp.AbC123\n":      "/tmp/tmp.AbC123",
		"  /var/tmp/tmp.xyz  \n": "/var/tmp/tmp.xyz",
		"/tmp/tmp.a-b_c":         "/tmp/tmp.a-b_c",
	}
	for answer, want := range accepted {
		got, err := scratchPath(answer)
		if err != nil {
			t.Errorf("scratchPath(%q): %v", answer, err)
			continue
		}
		if got != want {
			t.Errorf("scratchPath(%q) = %q, want %q", answer, got, want)
		}
	}

	refused := map[string]string{
		"":                         "nothing",
		"   ":                      "nothing",
		"tmp/relative":             "absolute",
		"/tmp/../etc":              "back up",
		"Welcome!\n/tmp/tmp.a":     "more than one line",
		"/tmp/tmp.a'; rm -rf /; #": "characters",
		"/tmp/tmp.$(id)":           "characters",
		"/tmp/tmp.a b":             "characters",
		"/tmp/tmp.`whoami`":        "characters",
	}
	for answer, because := range refused {
		_, err := scratchPath(answer)
		if err == nil {
			t.Errorf("scratchPath(%q) was accepted; it goes into a shell command on that machine", answer)
			continue
		}
		if !strings.Contains(err.Error(), because) {
			t.Errorf("scratchPath(%q) refused with %q, which does not say %q", answer, err, because)
		}
	}
}

// uname -m alone cannot tell a Linux host from a Mac — both answer arm64, only
// one can run what this binary embeds. A Mac is a developer's machine, not a
// host (crg, 2026-08-30): refused with where loopback lives instead.
func TestAMacIsRefusedAsAnInstallTarget(t *testing.T) {
	_, err := hostdArch("Darwin arm64")
	if err == nil {
		t.Fatal("a Mac was accepted as a target for a Linux daemon")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("the refusal does not say what to do instead: %v", err)
	}
	_, err = hostdArch("FreeBSD amd64")
	if err == nil || !strings.Contains(err.Error(), "FreeBSD") {
		t.Fatalf("another system was not refused by name: %v", err)
	}
}
