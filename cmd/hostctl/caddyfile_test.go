package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crgimenes/hostd/service"
)

func proxied(name string, port float64, domains ...string) service.Service {
	return service.Service{Name: name, Image: name + ":1", Domain: domains, UpstreamPort: port}
}

// What a proxy needs is the container's alias and the port it listens on, not
// anything published to the machine: a service behind a proxy usually publishes
// nothing at all, which is the point of putting it behind one.
func TestTheProxyIsSentToTheContainerAndNotToTheMachine(t *testing.T) {
	out, err := caddyfile([]service.Service{
		proxied("site", 0, "crg.eti.br", "www.crg.eti.br"),
		proxied("api", 8080, "api.crg.eti.br"),
	}, "/tree", "")
	if err != nil {
		t.Fatalf("caddyfile: %v", err)
	}
	for _, want := range []string{
		"crg.eti.br, www.crg.eti.br {",
		"\treverse_proxy site:80\n",
		"api.crg.eti.br {",
		"\treverse_proxy api:8080\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the file does not carry %q:\n%s", want, out)
		}
	}
}

// The file is committed, so regenerating it must not produce a change nobody
// made: a diff is how anyone reviews what a generator did, and reordered blocks
// would bury the one line that mattered.
func TestTheSameDeclarationsAlwaysWriteTheSameFile(t *testing.T) {
	one := []service.Service{
		proxied("site", 0, "www.crg.eti.br", "crg.eti.br"),
		proxied("api", 8080, "api.crg.eti.br"),
		proxied("blog", 0, "blog.crg.eti.br"),
	}
	other := []service.Service{
		proxied("blog", 0, "blog.crg.eti.br"),
		proxied("site", 0, "crg.eti.br", "www.crg.eti.br"),
		proxied("api", 8080, "api.crg.eti.br"),
	}
	first, err := caddyfile(one, "/tree", "")
	if err != nil {
		t.Fatalf("caddyfile: %v", err)
	}
	second, err := caddyfile(other, "/tree", "")
	if err != nil {
		t.Fatalf("caddyfile: %v", err)
	}
	if first != second {
		t.Fatalf("the same declarations in another order wrote a different file:\n%s\n---\n%s", first, second)
	}
}

// Two services answering for one name is a file whose behaviour depends on
// which block Caddy read last. Which one is wrong is the operator's call, so
// both are named and nothing is written.
func TestOneNameCannotReachTwoServices(t *testing.T) {
	_, err := caddyfile([]service.Service{
		proxied("site", 0, "crg.eti.br"),
		proxied("old", 0, "crg.eti.br"),
	}, "/tree", "")
	if err == nil {
		t.Fatal("two services declared one domain and the file was written anyway")
	}
	for _, want := range []string{"site", "old", "crg.eti.br"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %v", want, err)
		}
	}
}

// This command is written to be redirected over a file that is serving traffic.
// An empty result has to reach the caller as an empty result, so nothing takes
// a working proxy down without a word.
func TestATreeWithNoDomainWritesNothing(t *testing.T) {
	out, err := caddyfile([]service.Service{
		{Name: "worker", Image: "worker:1", Every: "1h"},
		{Name: "db", Image: "db:1"},
	}, "/tree", "")
	if err != nil {
		t.Fatalf("caddyfile: %v", err)
	}
	if out != "" {
		t.Fatalf("a tree with no domain wrote a Caddyfile:\n%s", out)
	}
}

// The generated file says what it is and, more importantly, that nothing will
// come back and rewrite it. hostd renders nothing at runtime — that decision is
// the reason this command exists, and whoever opens the file months later reads
// it there rather than in a wiki.
func TestTheFileSaysItIsOrdinaryFromHereOn(t *testing.T) {
	out, err := caddyfile([]service.Service{proxied("site", 0, "crg.eti.br")}, "/tree", "yuki.local")
	if err != nil {
		t.Fatalf("caddyfile: %v", err)
	}
	for _, want := range []string{"/tree", "yuki.local", "Edit it by hand", "nothing regenerates it"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the header does not carry %q:\n%s", want, out)
		}
	}
}

// The command reads the operator's TREE, which has an inventory.filo and
// services that are directories. It read it with LoadDir once — the loader for
// a machine's services directory, where every .filo is a service — so the
// inventory came back as a broken service and a directory service was
// invisible. Found the day the first directory service declared a domain.
func TestTheCommandReadsATreeWithAnInventoryAndDirectoryServices(t *testing.T) {
	tree := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		err := os.MkdirAll(filepath.Dir(filepath.Join(tree, path)), 0o700)
		if err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		err = os.WriteFile(filepath.Join(tree, path), []byte(content), 0o600)
		if err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write("inventory.filo", `(inventory (host (tuple "name" "yuki.local")))`)
	write("watcher/init.filo", `(service
  (tuple "name" "watcher")
  (tuple "image" "watcher:1")
  (tuple "domain" (list ":8317"))
  (tuple "upstream-port" 8317))`)
	write("watcher/config.filo", `(set Listen "0.0.0.0:8317")`)

	var out strings.Builder
	code, err := runCaddyfile(context.Background(), options{config: tree, out: &out}, nil)
	if err != nil {
		t.Fatalf("runCaddyfile: %v", err)
	}
	if code != exitOK {
		t.Fatalf("exit %d, want %d", code, exitOK)
	}
	if !strings.Contains(out.String(), "reverse_proxy watcher:8317") {
		t.Fatalf("the directory service is missing from the Caddyfile:\n%s", out.String())
	}
}
