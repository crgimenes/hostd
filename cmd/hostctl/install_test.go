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
		"an upgrade names what it replaced": {
			before: "hostd v0.0.1 (protocol 1, schema 1)",
			now:    "v0.0.3",
			want:   "upgraded from v0.0.1",
		},
		// A machine can answer anything at all; indexing into it would turn a
		// strange banner into a panic in the middle of a fleet-wide install.
		"an answer nothing can be read out of": {
			before: "???",
			now:    "v0.0.3",
			want:   "upgraded",
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
		"x86_64":    "amd64",
		"aarch64":   "arm64",
		"arm64":     "arm64",
		"x86_64\n":  "amd64",
		" aarch64 ": "arm64",
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
	_, err := hostdArch("armv7l")
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
