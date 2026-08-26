package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

// A supervisor the test writes the script for: what the plan says is what the
// test is about, and a real one would need a container runtime to say it.
type scriptedSup struct {
	mu         sync.Mutex
	statuses   []supervisor.Status
	plan       []supervisor.Change
	restarted  []string
	stopped    []string
	started    []string
	redeployed []string
	removed    []string
	applies    int
}

func (s *scriptedSup) Status() []supervisor.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses
}

func (s *scriptedSup) StatusOf(name string) (supervisor.Status, error) {
	return supervisor.Status{Name: name, State: supervisor.StateRunning}, nil
}

func (s *scriptedSup) Start(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, name)
	return true, nil
}

func (s *scriptedSup) Stop(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = append(s.stopped, name)
	return true, nil
}

func (s *scriptedSup) Restart(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restarted = append(s.restarted, name)
	return true, nil
}

func (s *scriptedSup) Deploy(svc service.Service) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redeployed = append(s.redeployed, svc.Name)
	return true, nil
}

func (s *scriptedSup) Remove(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, name)
	return true, nil
}

func (s *scriptedSup) RunNow(name string) (string, error) { return "", nil }

func (s *scriptedSup) Plan(declared []service.Service) []supervisor.Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plan
}

func (s *scriptedSup) Apply(declared []service.Service) []supervisor.Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applies++
	return s.plan
}

func (s *scriptedSup) count(of *[]string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(*of)
}

// A real daemon on a real socket, short of the container runtime: the window's
// actions must speak the same protocol the command line speaks, and a fake
// answering machine would prove they speak to a fake.
func startDaemon(t *testing.T) (*scriptedSup, string, string) {
	t.Helper()
	// The shortest possible directory: the socket path has a hard kernel limit,
	// and a long test name inside TempDir once made these tests SKIP silently.
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, err := state.Open(ctx, filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	logStore, err := logs.Open(ctx, filepath.Join(dir, "logs.db"), logs.Options{})
	if err != nil {
		t.Fatalf("logs.Open: %v", err)
	}
	t.Cleanup(func() { _ = logStore.Close() })
	metricStore, err := metrics.Open(ctx, filepath.Join(dir, "metrics.db"), metrics.Options{})
	if err != nil {
		t.Fatalf("metrics.Open: %v", err)
	}
	t.Cleanup(func() { _ = metricStore.Close() })

	services := filepath.Join(dir, "services")
	err = os.MkdirAll(services, 0o700)
	if err != nil {
		t.Fatalf("mkdir services: %v", err)
	}
	sup := &scriptedSup{}
	server := api.NewServer(sup, store, logStore, metricStore, services)
	listener, err := api.ListenUnix(socket)
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(ctx, listener) }()
	return sup, socket, services
}

// A panel whose clicks land on the daemon above, and whose tree declares one
// service for the machine.
func actingPanel(t *testing.T) (*panel, *scriptedSup, string) {
	t.Helper()
	sup, socket, services := startDaemon(t)
	view := probePanel(t)
	view.dial = func(host string) (*api.Client, error) { return api.DialUnix(socket) }
	tree := t.TempDir()
	files := map[string]string{
		"caddy.filo": `(service (tuple "name" "caddy") (tuple "image" "hostd-nowhere:0"))`,
		"other.filo": `(service (tuple "name" "other") (tuple "image" "busybox"))`,
		"inventory.filo": `(inventory
  (host (tuple "name" "yuki"))
  (host (tuple "name" "selene")))`,
	}
	for name, content := range files {
		err := os.WriteFile(filepath.Join(tree, name), []byte(content), 0o600)
		if err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	view.cfgDir = tree
	return view, sup, services
}

// A click asks first and acts second: the confirmation performs nothing, the
// run performs exactly the operation the command line would.
func TestARestartClickIsConfirmedThenPerformed(t *testing.T) {
	view, sup, _ := actingPanel(t)
	get(t, view, "/")

	asked := get(t, view, "/act/confirm/restart/yuki/caddy").Body.String()
	if !strings.Contains(asked, `id="action"`) || !strings.Contains(asked, "data-open") {
		t.Fatalf("the confirmation dialog did not come back: %s", asked)
	}
	if !strings.Contains(asked, `data-act="run/restart/yuki/caddy"`) {
		t.Fatalf("the dialog has no button that performs the restart: %s", asked)
	}
	if !strings.Contains(asked, "hostctl -host yuki service restart caddy") {
		t.Fatalf("the dialog does not show the command line equivalent: %s", asked)
	}
	if sup.count(&sup.restarted) != 0 {
		t.Fatal("asking about a restart restarted something")
	}

	done := get(t, view, "/act/run/restart/yuki/caddy").Body.String()
	if sup.count(&sup.restarted) != 1 {
		t.Fatalf("the run did not reach the daemon: restarted %d time(s)", sup.count(&sup.restarted))
	}
	if !strings.Contains(done, "done") {
		t.Fatalf("the outcome did not come back to the dialog: %s", done)
	}
}

// A daemon too old to know an operation is not a dead end: the window carries
// the daemon that knows it, and the dialog offers to put it there.
func TestAnOldDaemonOffersItsOwnUpdate(t *testing.T) {
	view := probePanel(t)
	out := view.failedResp(dialogView{Title: "redeploy caddy on m1.local"}, "m1.local",
		api.Response{Code: api.CodeUnknownOp, Message: `this hostd does not implement "service.redeploy"`})
	if out.Confirm.Act != "confirm/update/m1.local" {
		t.Fatalf("an unknown operation did not offer the update: %+v", out)
	}
	if !out.Bad || out.Outcome == "" {
		t.Fatalf("the refusal itself must still be shown: %+v", out)
	}

	// And the confirmation renders, whichever daemon this build carries.
	acting, _, _ := actingPanel(t)
	asked := get(t, acting, "/act/confirm/update/yuki").Body.String()
	if !strings.Contains(asked, `id="action"`) || !strings.Contains(asked, "update hostd on yuki") {
		t.Fatalf("the update confirmation did not come back: %s", asked)
	}
}

// A deploy is confirmed and then overwrites: the declaration lands on the
// machine, the container is recreated from it, and what the machine was
// already complaining about is said in the same dialog.
func TestDeployIsConfirmedThenOverwrites(t *testing.T) {
	view, sup, services := actingPanel(t)
	get(t, view, "/")

	asked := get(t, view, "/act/confirm/deploy/yuki/caddy").Body.String()
	if !strings.Contains(asked, `data-act="run/deploy/yuki/caddy"`) {
		t.Fatalf("the dialog has no button that deploys: %s", asked)
	}
	if !strings.Contains(asked, "hostctl -host yuki service deploy caddy") {
		t.Fatalf("the dialog does not show the command line equivalent: %s", asked)
	}
	if sup.count(&sup.redeployed) != 0 {
		t.Fatal("confirming a deploy deployed something")
	}

	sup.mu.Lock()
	sup.statuses = []supervisor.Status{{
		Name: "caddy", State: supervisor.StateFailed,
		LastError: "image hostd-nowhere:0 is not on this machine; send it with hostctl image push",
	}}
	sup.mu.Unlock()
	done := get(t, view, "/act/run/deploy/yuki/caddy").Body.String()
	_, err := os.Stat(filepath.Join(services, "caddy.filo"))
	if err != nil {
		t.Fatalf("the declaration did not land on the machine: %v", err)
	}
	// The image is nowhere: the machine is asked to pull, and this daemon has
	// no runtime to pull into — said, not hidden, and not fatal: the start is
	// the judge.
	if !strings.Contains(done, "pulling it from its registry") || !strings.Contains(done, "could not pull") {
		t.Fatalf("the image path is not told step by step: %s", done)
	}
	if sup.count(&sup.redeployed) != 1 {
		t.Fatalf("the deploy did not recreate the container: %d", sup.count(&sup.redeployed))
	}
	if !strings.Contains(done, "deployed") {
		t.Fatalf("the outcome did not come back: %s", done)
	}
	if !strings.Contains(done, "the machine reports: caddy") {
		t.Fatalf("the dialog hides what the machine is reporting: %s", done)
	}
}

// A remove is confirmed in red, and then takes the service off the machine:
// container, declaration and image — while the tree keeps describing it, so a
// deploy puts it back.
func TestRemoveIsConfirmedThenTakesItOffTheMachine(t *testing.T) {
	view, sup, services := actingPanel(t)
	get(t, view, "/")
	get(t, view, "/act/run/deploy/yuki/caddy")
	_, err := os.Stat(filepath.Join(services, "caddy.filo"))
	if err != nil {
		t.Fatalf("the deploy did not land the declaration: %v", err)
	}

	asked := get(t, view, "/act/confirm/remove/yuki/caddy").Body.String()
	if !strings.Contains(asked, `data-act="run/remove/yuki/caddy"`) {
		t.Fatalf("the dialog has no button that removes: %s", asked)
	}
	if !strings.Contains(asked, `class="danger"`) {
		t.Fatalf("a removal is not offered in red: %s", asked)
	}
	if sup.count(&sup.removed) != 0 {
		t.Fatal("confirming a removal removed something")
	}

	done := get(t, view, "/act/run/remove/yuki/caddy").Body.String()
	if sup.count(&sup.removed) != 1 {
		t.Fatalf("the removal did not reach the daemon: %d", sup.count(&sup.removed))
	}
	_, err = os.Stat(filepath.Join(services, "caddy.filo"))
	if !os.IsNotExist(err) {
		t.Fatalf("the declaration is still on the machine: %v", err)
	}
	if !strings.Contains(done, `data-act="confirm/deploy/yuki/caddy"`) {
		t.Fatalf("the outcome does not offer the way back as a button: %s", done)
	}
}

// The machine's deploy button is the catalog: everything the tree describes,
// each one a click away, with what already runs said beside the name.
func TestTheCatalogListsEveryDescribedService(t *testing.T) {
	view, _, _ := actingPanel(t)
	get(t, view, "/")

	catalog := get(t, view, "/act/confirm/add/yuki").Body.String()
	if !strings.Contains(catalog, `data-act="confirm/deploy/yuki/caddy"`) ||
		!strings.Contains(catalog, `data-act="confirm/deploy/yuki/other"`) {
		t.Fatalf("the catalog does not offer every described service: %s", catalog)
	}
	// The probe snapshot already runs caddy on yuki; deploying again overwrites,
	// and the catalog says so instead of hiding the entry.
	if !strings.Contains(catalog, "already here; deploying replaces it") {
		t.Fatalf("the catalog does not say what is already running: %s", catalog)
	}
}

// An action waits on its own pipe: one request owns a connection from first
// byte to last, so an action that takes seconds on the rounds' pipe would
// freeze the screen exactly while somebody waits to see what their click did.
func TestActionsRideTheirOwnConnection(t *testing.T) {
	view, _, _ := actingPanel(t)
	rounds, err := view.client("yuki")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	acting, err := view.actionClient("yuki")
	if err != nil {
		t.Fatalf("actionClient: %v", err)
	}
	if rounds == acting {
		t.Fatal("actions share the telemetry pipe, so a slow action freezes the screen")
	}
}

// A run reports from the corner, over a live screen; only a question that
// needs an answer takes the centre as a modal.
func TestARunReportsFromTheCorner(t *testing.T) {
	view, _, _ := actingPanel(t)
	get(t, view, "/")

	asked := get(t, view, "/act/confirm/restart/yuki/caddy").Body.String()
	if strings.Contains(asked, "data-corner") {
		t.Fatalf("a question is not a corner card: %s", asked)
	}
	done := get(t, view, "/act/run/restart/yuki/caddy").Body.String()
	if !strings.Contains(done, "data-corner") {
		t.Fatalf("a run's report is not a corner card: %s", done)
	}

	script, err := ui.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	if !strings.Contains(string(script), `hasAttribute("data-corner")`) || !strings.Contains(string(script), "piece.show()") {
		t.Fatal("app.js does not open corner cards without a modal")
	}
}

// A machine ssh reaches where no hostd answers is the state an install fixes,
// so that is what the dialog offers — instead of an error that reads like a
// dead end.
func TestNoHostdAnsweringOffersTheInstall(t *testing.T) {
	view := probePanel(t)
	out := view.failedErr(dialogView{Title: "deploy site on selene"}, "selene",
		fmt.Errorf("hostd on selene closed the connection without answering: %w", api.ErrNoAnswer))
	if out.Confirm.Act != "confirm/update/selene" {
		t.Fatalf("no install was offered: %+v", out)
	}
	if !out.Bad || out.Outcome == "" {
		t.Fatalf("the failure itself must still be shown: %+v", out)
	}
}
