package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMinimal(t *testing.T) {
	// The simple case has to stay simple: a name and an image are a whole
	// service, everything else has a default.
	src := `(service (tuple "name" "api") (tuple "image" "api:1"))`
	s, err := Parse(context.Background(), "api.filo", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Kind != KindContainer {
		t.Errorf("kind = %q, want %q", s.Kind, KindContainer)
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
	  (tuple "kind" "container")
	  (tuple "image" "api:1")
	  (tuple "args" (list "-listen" ":8080"))
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
		{"no name", `(service (tuple "image" "api:1"))`},
		{"no image", `(service (tuple "name" "api"))`},
		{"relative dir", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "dir" "rel"))`},
		{"a kind that does not exist", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "kind" "exec"))`},
		{"unknown state", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "state" "paused"))`},
		{"unknown restart", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "restart" "maybe"))`},
		{"env without =", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "env" (list "BROKEN")))`},
		{"negative stop timeout", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "stop-timeout" -1))`},
		{"name with slash", `(service (tuple "name" "a/b") (tuple "image" "api:1"))`},
		{"name with dots", `(service (tuple "name" "..") (tuple "image" "api:1"))`},
		{"uppercase name", `(service (tuple "name" "API") (tuple "image" "api:1"))`},
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
	writeService(t, dir, "api.filo", `(service (tuple "name" "other") (tuple "image" "probe:1"))`)
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
	writeService(t, dir, "b.filo", `(service (tuple "name" "b") (tuple "image" "probe:1"))`)
	writeService(t, dir, "a.filo", `(service (tuple "name" "a") (tuple "image" "probe:1"))`)
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
	writeService(t, dir, "good.filo", `(service (tuple "name" "good") (tuple "image" "probe:1"))`)
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
	writeService(t, dir, "api.filo", `(service (tuple "name" "api") (tuple "image" "probe:1"))`)
	writeService(t, dir, "api2.filo", `(service (tuple "name" "api") (tuple "image" "probe:1"))`)
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

// A job is a service with a schedule, and the cron this replaces stops at the
// minute.
func TestAJobDeclaresHowOftenItRuns(t *testing.T) {
	src := `(service
	  (tuple "name" "worker")
	  (tuple "image" "worker:1")
	  (tuple "every" "30s"))`
	svc, err := Parse(context.Background(), "worker.filo", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !svc.IsJob() || svc.Interval() != 30*time.Second {
		t.Fatalf("the schedule parsed as %v", svc.Interval())
	}
	// Overlapping is what cron does and what a worker pool wants.
	if svc.Overlap != OverlapAllow {
		t.Fatalf("a job defaults to %q", svc.Overlap)
	}
	// A ceiling nobody declared is still a ceiling.
	if svc.Parallel() != DefaultMaxParallel {
		t.Fatalf("a job with no ceiling declared allows %d", svc.Parallel())
	}
	// The runtime must not bring a job back: it ended because it was done.
	if svc.Restart != RestartNever {
		t.Fatalf("a job carries the restart policy %q", svc.Restart)
	}
}

func TestABrokenScheduleIsRefusedWhereItIsWritten(t *testing.T) {
	table := []struct {
		why string
		src string
	}{
		{"not a duration", `(tuple "every" "soon")`},
		{"below the floor", `(tuple "every" "10ms")`},
		{"a policy that does not exist", `(tuple "every" "1m") (tuple "overlap" "wait")`},
		{"restarted by the runtime", `(tuple "every" "1m") (tuple "restart" "always")`},
		{"a job publishing a port", `(tuple "every" "1m") (tuple "ports" (list "8080:80"))`},
		{"overlap without a schedule", `(tuple "overlap" "skip")`},
		{"a ceiling without a schedule", `(tuple "max-parallel" 4)`},
	}
	for _, test := range table {
		t.Run(test.why, func(t *testing.T) {
			src := `(service (tuple "name" "worker") (tuple "image" "worker:1") ` + test.src + `)`
			_, err := Parse(context.Background(), "worker.filo", src)
			if err == nil {
				t.Fatalf("accepted: %s", test.src)
			}
		})
	}
}

// A shared tree in a heterogeneous fleet: the database does not belong on the
// web machines, and saying nothing about placement still means everywhere.

// A domain is written straight into a Caddyfile, so what is wrong with it has
// to be caught where the declaration is read. Pasting a URL out of a browser is
// the mistake to expect, and it would render a file that fails to load with the
// proxy already down.
func TestADomainIsANameAndNotAURL(t *testing.T) {
	cases := []struct {
		why string
		src string
	}{
		{"a pasted URL", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "domain" (list "https://api.example.com/v1")))`},
		{"an empty name", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "domain" (list "")))`},
		{"a Caddy block pasted in", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "domain" (list "api.example.com {")))`},
		{"a port with nowhere to send", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "upstream-port" 8080))`},
		{"not a port", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "domain" (list "a.example.com")) (tuple "upstream-port" 99999))`},
		{"a fractional port", `(service (tuple "name" "api") (tuple "image" "api:1") (tuple "domain" (list "a.example.com")) (tuple "upstream-port" 80.5))`},
	}
	for _, c := range cases {
		_, err := Parse(context.Background(), "api.filo", c.src)
		if err == nil {
			t.Errorf("%s was accepted", c.why)
		}
	}
}

// Eighty is the default because it is what an image serving a site listens on
// unless it says otherwise, and the alias is the service's own name on the
// managed network.
func TestWhereAProxyIsSentDefaultsToPortEighty(t *testing.T) {
	plain := Service{Name: "site"}
	if plain.Upstream() != "site:80" {
		t.Fatalf("Upstream() = %q, want site:80", plain.Upstream())
	}
	other := Service{Name: "api", UpstreamPort: 8080}
	if other.Upstream() != "api:8080" {
		t.Fatalf("Upstream() = %q, want api:8080", other.Upstream())
	}
}

// Not every fleet is on the public internet. A machine serving one thing
// matches no name at all — a bare port, which is what the bench has run since
// before any of this existed — and a network with no public DNS serves names
// over http:// so the proxy does not go looking for certificates it can never
// obtain. Refusing either would leave the internal case unable to say itself.
func TestAnAddressDoesNotHaveToBeAPublicDomain(t *testing.T) {
	for _, address := range []string{":80", ":8081", "http://site.internal", "https://api.example.com"} {
		src := fmt.Sprintf(`(service (tuple "name" "site") (tuple "image" "site:1") (tuple "domain" (list %q)))`, address)
		svc, err := Parse(context.Background(), "site.filo", src)
		if err != nil {
			t.Errorf("%s was refused: %v", address, err)
			continue
		}
		if len(svc.Domain) != 1 || svc.Domain[0] != address {
			t.Errorf("%s came back as %v", address, svc.Domain)
		}
	}
}
