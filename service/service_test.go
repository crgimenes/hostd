package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMinimal(t *testing.T) {
	// The simple case has to stay simple: a name and a command are a whole
	// service, everything else has a default.
	src := `(service (tuple "name" "api") (tuple "command" "/usr/local/bin/api"))`
	s, err := Parse(context.Background(), "api.filo", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Kind != KindExec {
		t.Errorf("kind = %q, want %q", s.Kind, KindExec)
	}
	if s.State != StateRunning {
		t.Errorf("state = %q, want %q", s.State, StateRunning)
	}
	if s.Restart != RestartAlways {
		t.Errorf("restart = %q, want %q", s.Restart, RestartAlways)
	}
	if s.StopGrace() != DefaultStopTimeout {
		t.Errorf("stop grace = %v, want %v", s.StopGrace(), DefaultStopTimeout)
	}
	if !s.WantRunning() {
		t.Error("a service with no declared state should want to be running")
	}
}

func TestParseFull(t *testing.T) {
	src := `(service
	  (tuple "name" "api")
	  (tuple "kind" "exec")
	  (tuple "command" "/usr/local/bin/api")
	  (tuple "args" (list "--listen" ":8080"))
	  (tuple "dir" "/var/lib/api")
	  (tuple "env" (list "ENV=production"))
	  (tuple "state" "stopped")
	  (tuple "restart" "on-failure")
	  (tuple "stop-timeout" 5))`
	s, err := Parse(context.Background(), "api.filo", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(s.Args) != 2 || s.Args[1] != ":8080" {
		t.Errorf("args = %#v", s.Args)
	}
	if s.Dir != "/var/lib/api" || len(s.Env) != 1 {
		t.Errorf("dir/env = %q %#v", s.Dir, s.Env)
	}
	if s.WantRunning() {
		t.Error("a service declared stopped must not want to run")
	}
	if s.StopGrace() != 5*time.Second {
		t.Errorf("stop grace = %v, want 5s", s.StopGrace())
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		why string
		src string
	}{
		{"no name", `(service (tuple "command" "/bin/true"))`},
		{"no command", `(service (tuple "name" "api"))`},
		{"relative command", `(service (tuple "name" "api") (tuple "command" "api"))`},
		{"relative dir", `(service (tuple "name" "api") (tuple "command" "/bin/true") (tuple "dir" "rel"))`},
		{"unknown kind", `(service (tuple "name" "api") (tuple "command" "/bin/true") (tuple "kind" "vm"))`},
		{"unknown state", `(service (tuple "name" "api") (tuple "command" "/bin/true") (tuple "state" "paused"))`},
		{"unknown restart", `(service (tuple "name" "api") (tuple "command" "/bin/true") (tuple "restart" "maybe"))`},
		{"env without =", `(service (tuple "name" "api") (tuple "command" "/bin/true") (tuple "env" (list "BROKEN")))`},
		{"negative stop timeout", `(service (tuple "name" "api") (tuple "command" "/bin/true") (tuple "stop-timeout" -1))`},
		{"name with slash", `(service (tuple "name" "a/b") (tuple "command" "/bin/true"))`},
		{"name with dots", `(service (tuple "name" "..") (tuple "command" "/bin/true"))`},
		{"uppercase name", `(service (tuple "name" "API") (tuple "command" "/bin/true"))`},
	}
	for _, c := range cases {
		_, err := Parse(context.Background(), "t.filo", c.src)
		if err == nil {
			t.Errorf("%s: accepted", c.why)
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error is not ErrInvalid: %v", c.why, err)
		}
	}
}

// A container service is a real kind that is not built yet. Saying so beats
// pretending the field does not exist.
// A container service runs the image's own command, so a command in the file
// is a misunderstanding worth naming rather than quietly ignoring.
func TestAContainerRunsTheImageNotACommand(t *testing.T) {
	src := `(service (tuple "name" "site") (tuple "kind" "container") (tuple "image" "site:1") (tuple "command" "/bin/true"))`
	_, err := Parse(context.Background(), "site.filo", src)
	if err == nil {
		t.Fatal("a container service with a command was accepted")
	}
	if !strings.Contains(err.Error(), "image's own command") {
		t.Fatalf("the error does not say where the command comes from: %v", err)
	}
}

func TestAContainerNeedsAnImage(t *testing.T) {
	src := `(service (tuple "name" "site") (tuple "kind" "container"))`
	_, err := Parse(context.Background(), "site.filo", src)
	if err == nil {
		t.Fatal("a container service with no image was accepted")
	}
	if !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
}

// A port with no address binds to loopback, where the reverse proxy on the
// same machine reaches it and the internet does not.
func TestPublishedPortsAreParsedAndDefaultToLoopback(t *testing.T) {
	src := `(service
	  (tuple "name" "site")
	  (tuple "kind" "container")
	  (tuple "image" "site@sha256:abc")
	  (tuple "ports" (list "8080:80" "0.0.0.0:9000:9000/udp")))`
	svc, err := Parse(context.Background(), "site.filo", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ports, err := svc.PublishedPorts()
	if err != nil {
		t.Fatalf("PublishedPorts: %v", err)
	}
	if len(ports) != 2 {
		t.Fatalf("got %d ports, expected 2", len(ports))
	}
	if ports[0] != (Port{HostIP: "", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}) {
		t.Fatalf("the simple form parsed as %#v", ports[0])
	}
	if ports[1] != (Port{HostIP: "0.0.0.0", HostPort: 9000, ContainerPort: 9000, Protocol: "udp"}) {
		t.Fatalf("the full form parsed as %#v", ports[1])
	}
}

func TestABrokenPortIsRefusedWhereItIsWritten(t *testing.T) {
	table := []string{"80", "not:80", "8080:0", "8080:80/sctp", "1.2.3:8080:80"}
	for _, spec := range table {
		t.Run(spec, func(t *testing.T) {
			src := `(service (tuple "name" "site") (tuple "kind" "container") (tuple "image" "site:1") (tuple "ports" (list "` + spec + `")))`
			_, err := Parse(context.Background(), "site.filo", src)
			if err == nil {
				t.Fatalf("the port %q was accepted", spec)
			}
		})
	}
}

// The two kinds do not borrow each other's fields: an exec service with an
// image is a file somebody wrote by copying the wrong example.
func TestAnExecServiceRefusesContainerFields(t *testing.T) {
	src := `(service (tuple "name" "api") (tuple "command" "/usr/bin/api") (tuple "image" "api:1"))`
	_, err := Parse(context.Background(), "api.filo", src)
	if err == nil {
		t.Fatal("an exec service with an image was accepted")
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"api", "web-1", "a", "my_service", "x2"}
	for _, n := range valid {
		if !ValidName(n) {
			t.Errorf("%q rejected", n)
		}
	}
	invalid := []string{"", "-api", "api-", "_api", "api_", "A", "a b", "a/b", "..", ".", "a.b", strings.Repeat("a", 65)}
	for _, n := range invalid {
		if ValidName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}

func writeService(t *testing.T, dir, file, body string) {
	t.Helper()
	err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600)
	if err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

func TestParseFileRequiresMatchingName(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir, "api.filo", `(service (tuple "name" "other") (tuple "command" "/bin/true"))`)
	_, err := ParseFile(context.Background(), filepath.Join(dir, "api.filo"))
	if err == nil {
		t.Fatal("mismatched file name accepted")
	}
	// The message has to tell the operator how to fix it, not just what is wrong.
	if !strings.Contains(err.Error(), "rename the file to other.filo") {
		t.Fatalf("error does not say how to fix it: %v", err)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir, "b.filo", `(service (tuple "name" "b") (tuple "command" "/bin/true"))`)
	writeService(t, dir, "a.filo", `(service (tuple "name" "a") (tuple "command" "/bin/true"))`)
	writeService(t, dir, "notes.txt", "ignored")
	services, err := LoadDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("got %d services, want 2", len(services))
	}
	// Sorted, so a plan built from them is the same plan every time.
	if services[0].Name != "a" || services[1].Name != "b" {
		t.Fatalf("not sorted: %q %q", services[0].Name, services[1].Name)
	}
}

func TestLoadDirMissingIsNotAnError(t *testing.T) {
	services, err := LoadDir(context.Background(), filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("a host with no services declared is valid: %v", err)
	}
	if len(services) != 0 {
		t.Fatalf("got %d services", len(services))
	}
}

func TestLoadDirReportsEveryProblem(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir, "good.filo", `(service (tuple "name" "good") (tuple "command" "/bin/true"))`)
	writeService(t, dir, "bad1.filo", `(service (tuple "name" "bad1"))`)
	writeService(t, dir, "bad2.filo", `(service (tuple "name" "bad2") (tuple "command" "relative"))`)
	services, err := LoadDir(context.Background(), dir)
	if err == nil {
		t.Fatal("broken files accepted")
	}
	// Every problem at once, so the operator does not fix them one restart at
	// a time.
	if !strings.Contains(err.Error(), "bad1") || !strings.Contains(err.Error(), "bad2") {
		t.Fatalf("not every problem reported: %v", err)
	}
	// The good ones still load, so one typo does not take the host down.
	if len(services) != 1 || services[0].Name != "good" {
		t.Fatalf("valid services were dropped: %#v", services)
	}
}

// Two files declaring the same service is structurally impossible, because the
// declared name must equal the file name. This proves the second file is
// rejected rather than silently shadowing the first.
func TestLoadDirCannotDuplicateAService(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir, "api.filo", `(service (tuple "name" "api") (tuple "command" "/bin/true"))`)
	writeService(t, dir, "api2.filo", `(service (tuple "name" "api") (tuple "command" "/bin/true"))`)
	services, err := LoadDir(context.Background(), dir)
	if err == nil {
		t.Fatal("a second file declaring an existing service was accepted")
	}
	if len(services) != 1 || services[0].Name != "api" {
		t.Fatalf("the valid file should still load: %#v", services)
	}
}

func TestVolumesSayWhatTheyAre(t *testing.T) {
	src := `(service
	  (tuple "name" "site")
	  (tuple "kind" "container")
	  (tuple "image" "site:1")
	  (tuple "volumes" (list "certs:/data" "/srv/www:/srv/www:ro")))`
	svc, err := Parse(context.Background(), "site.filo", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mounts, err := svc.Mounts()
	if err != nil {
		t.Fatalf("Mounts: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts", len(mounts))
	}
	if mounts[0] != (Mount{Source: "certs", Target: "/data", Named: true}) {
		t.Fatalf("named storage parsed as %#v", mounts[0])
	}
	if mounts[1] != (Mount{Source: "/srv/www", Target: "/srv/www", ReadOnly: true}) {
		t.Fatalf("a path from the machine parsed as %#v", mounts[1])
	}
}

// Handing a container the runtime's socket hands it the machine, and it looks
// like an ordinary line in an ordinary file.
func TestTheRuntimeSocketCannotBeMounted(t *testing.T) {
	src := `(service (tuple "name" "site") (tuple "kind" "container") (tuple "image" "site:1")
	  (tuple "volumes" (list "/var/run/docker.sock:/var/run/docker.sock")))`
	_, err := Parse(context.Background(), "site.filo", src)
	if err == nil {
		t.Fatal("a container was given the runtime's socket")
	}
	if !strings.Contains(err.Error(), "this machine") {
		t.Fatalf("the refusal does not say what it grants: %v", err)
	}
}

func TestABrokenVolumeIsRefusedWhereItIsWritten(t *testing.T) {
	table := []string{"/data", "certs:data", "certs:/data:rx", "srv/www:/srv/www", ":/data"}
	for _, spec := range table {
		t.Run(spec, func(t *testing.T) {
			src := `(service (tuple "name" "site") (tuple "kind" "container") (tuple "image" "site:1") (tuple "volumes" (list "` + spec + `")))`
			_, err := Parse(context.Background(), "site.filo", src)
			if err == nil {
				t.Fatalf("the volume %q was accepted", spec)
			}
		})
	}
}
