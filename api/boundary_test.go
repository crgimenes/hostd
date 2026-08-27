package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The machine's name goes after a "--", so ssh cannot read it as an option.
// Without that, a name beginning with a dash IS an option — "-oProxyCommand=..."
// runs whatever it says, on the operator's own machine — and the names do not
// all come from a keyboard: -all and -tag read them out of the inventory file.
func TestAMachineNameCannotBecomeAnSSHOption(t *testing.T) {
	arguments := SSHArguments("-oProxyCommand=touch /tmp/pwned", []string{"hostd", "-stdio"})

	end := slices.Index(arguments, "--")
	if end < 0 {
		t.Fatalf("no -- ends the options: %q", arguments)
	}
	if arguments[end+1] != "-oProxyCommand=touch /tmp/pwned" {
		t.Fatalf("the name is not the first thing after --: %q", arguments)
	}
	// Everything before the end marker is this program's own; nothing the
	// caller supplied may appear there.
	for _, argument := range arguments[:end] {
		if argument == "-oProxyCommand=touch /tmp/pwned" {
			t.Fatalf("the name reached the option half of the line: %q", arguments)
		}
	}
	// And what the operator asked to run stays after the host, in order.
	if !slices.Equal(arguments[end+2:], []string{"hostd", "-stdio"}) {
		t.Fatalf("the remote command was rearranged: %q", arguments)
	}
}

// Every reach ssh makes has to give up on a machine that is not there. Without
// this, `hostctl -all install` with one switched-off machine printed nothing for
// tens of kernel seconds and got interrupted (crg, 2026-08-27) — the install had
// its own option list, and the option it was missing was this one.
func TestEverySSHReachGivesUpOnAMachineThatIsNotThere(t *testing.T) {
	arguments := SSHArguments("yuki.local", nil)

	at := slices.Index(arguments, fmt.Sprintf("ConnectTimeout=%d", connectTimeout))
	if at < 0 {
		t.Fatalf("no ConnectTimeout in the ssh options: %q", arguments)
	}
	if arguments[at-1] != "-o" {
		t.Fatalf("ConnectTimeout is not passed as an option: %q", arguments)
	}
	if connectTimeout > 15 {
		t.Fatalf("a %ds wait reads as a program that hung", connectTimeout)
	}
}

// ".." reads as a name and is not one: filepath.Base leaves it alone, and
// joining it lands in the services directory itself. It failed before this
// because a directory is not a file, which is luck rather than a guard.
func TestAnArtifactNamedDotDotIsRefused(t *testing.T) {
	f := newFixture(t)
	services := filepath.Join(f.dir, "services")
	err := os.MkdirAll(services, 0o700)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for _, name := range []string{"..", ".", "", "../escape", "sub/dir"} {
		err = f.server.writeDeclaration(Declaration{
			Name:      "probe",
			Source:    `(service (tuple "name" "probe") (tuple "image" "x:1"))`,
			Artifacts: []Artifact{{Name: name, Content: ""}},
		})
		if err == nil {
			t.Errorf("an artifact named %q was accepted", name)
		}
	}

	// The directory it was aiming at is untouched, and nothing was left behind.
	entries, err := os.ReadDir(services)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Fatalf("a failed write left %s behind", entry.Name())
		}
	}
}

// A service name becomes a file name, a container name and a volume name. The
// allowlist is what keeps all three from being something else.
func TestAServiceNameIsAnAllowlistAndNotAFilter(t *testing.T) {
	f := newFixture(t)
	client := f.client()
	defer func() { _ = client.Close() }()

	for _, name := range []string{"../escape", "a/b", ".", "..", "UPPER", "with space", "-lead", "trail-"} {
		source := `(service (tuple "name" "` + name + `") (tuple "image" "x:1"))`
		resp, err := client.Do(context.Background(), Request{
			Op:   OpServicePut,
			Body: `(list (tuple "name" "` + name + `") (tuple "source" ` + quoted(source) + `))`,
		})
		if err != nil {
			t.Fatalf("service.put: %v", err)
		}
		if !resp.Failed() {
			t.Errorf("a service named %q was accepted", name)
		}
	}
}

func quoted(s string) string {
	out := `"`
	for _, r := range s {
		switch r {
		case '"':
			out += `\"`
		case '\\':
			out += `\\`
		default:
			out += string(r)
		}
	}
	return out + `"`
}
