package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/daemon"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

// A supervisor the test writes the script for: what the plan says is what the
// test is about, and a real one would need a container runtime to say it.
type scriptedSup struct {
	// Closed to let a held action finish: proving a button holds itself down
	// needs an action that is still running while the test looks.
	hold       chan struct{}
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
	if s.hold != nil {
		<-s.hold
	}
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

func (s *scriptedSup) Backup(ctx context.Context, name string) (supervisor.BackupRun, error) {
	return supervisor.BackupRun{}, supervisor.ErrNoBackup{Name: name}
}

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

// waitFor gives an action started off this goroutine time to land. Nothing in
// the window waits for one either: the log says what is happening while it
// happens, and the screen catches up when it is done.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A click is the action: no confirmation, no dialog. What it did shows up in
// the log, which is the one place this window tells its story.
func TestAClickIsTheAction(t *testing.T) {
	view, sup, _ := actingPanel(t)
	get(t, view, "/")

	get(t, view, "/act/do/restart/yuki/caddy")
	waitFor(t, "the restart to reach the daemon", func() bool { return sup.count(&sup.restarted) == 1 })
}

// A deploy overwrites, and says what it is doing as it goes: the declaration
// lands on the machine, the container is recreated from it, and every step is
// a line in the log — including the image it could not find anywhere.
func TestADeployOverwritesAndNarratesItself(t *testing.T) {
	view, sup, services := actingPanel(t)
	get(t, view, "/")

	get(t, view, "/act/do/deploy/yuki/caddy")
	// Waited on the last thing the deploy says, not on the supervisor's
	// counter: the counter moves inside the daemon and the outcome line is
	// written after the answer comes back.
	waitFor(t, "the deploy to say it is done", func() bool {
		return strings.Contains(view.logText(), "caddy is")
	})
	if sup.count(&sup.redeployed) != 1 {
		t.Fatalf("the deploy did not recreate the container: %d", sup.count(&sup.redeployed))
	}
	_, err := os.Stat(filepath.Join(services, "caddy.filo"))
	if err != nil {
		t.Fatalf("the declaration did not land on the machine: %v", err)
	}

	said := view.logText()
	for _, expected := range []string{"pulling it from its registry", "sending the declaration", "caddy is"} {
		if !strings.Contains(said, expected) {
			t.Fatalf("the log does not carry %q:\n%s", expected, said)
		}
	}
}

// A remove takes the service off the machine — container, declaration, image —
// with one click, because the tree still describes it and a deploy puts it
// back. Volumes are never touched, which is what makes it ordinary.
func TestARemoveTakesTheServiceOffTheMachine(t *testing.T) {
	view, sup, services := actingPanel(t)
	get(t, view, "/")
	get(t, view, "/act/do/deploy/yuki/caddy")
	waitFor(t, "the deploy", func() bool { return sup.count(&sup.redeployed) == 1 })

	get(t, view, "/act/do/remove/yuki/caddy")
	// Waited on the FILE, not on the supervisor's counter: the counter moves
	// inside the supervisor and the declaration goes after it, so waiting on
	// the counter would read the directory a moment too early.
	waitFor(t, "the declaration to leave the machine", func() bool {
		_, err := os.Stat(filepath.Join(services, "caddy.filo"))
		return os.IsNotExist(err)
	})
	if sup.count(&sup.removed) != 1 {
		t.Fatalf("the removal did not reach the daemon: %d", sup.count(&sup.removed))
	}
}

// Nothing in the window asks before acting, and nothing opens a dialog: the
// act on a button is the operation itself.
func TestNothingAsksAndNothingIsModal(t *testing.T) {
	view, _, _ := actingPanel(t)
	pages := []string{"/", "/act/select/host/yuki", "/act/select/service/yuki/caddy",
		"/act/select/services", "/act/select/images/yuki", "/act/select/settings"}
	for _, path := range pages {
		body := get(t, view, path).Body.String()
		for _, forbidden := range []string{"confirm/", "showModal", "<dialog"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s still carries %q", path, forbidden)
			}
		}
	}
	script, err := ui.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	for _, forbidden := range []string{"showModal", "actionWorking", "spinner"} {
		if strings.Contains(string(script), forbidden) {
			t.Fatalf("app.js still carries %q", forbidden)
		}
	}
}

// The button an action is running holds itself down, and a second click on it
// does nothing: the panel keeps that state, because a class in the page would
// be lost with the fragment the rounds replace.
func TestAHeldButtonIgnoresTheSecondClick(t *testing.T) {
	view, sup, _ := actingPanel(t)
	get(t, view, "/")
	release := make(chan struct{})
	sup.hold = release
	defer close(release)

	get(t, view, "/act/do/restart/yuki/caddy")
	waitFor(t, "the action to be held", func() bool { return view.running("do/restart/yuki/caddy") })

	pane := get(t, view, "/act/select/service/yuki/caddy").Body.String()
	if !strings.Contains(pane, "disabled") {
		t.Fatalf("the button of a running action is not held down: %s", pane)
	}
	get(t, view, "/act/do/restart/yuki/caddy")
	if sup.count(&sup.restarted) != 0 {
		t.Fatal("a held action was started a second time")
	}
}

// The other inventory: every service the tree describes, one card each, with
// the machines in a dropdown beside its deploy. It deliberately does not say
// where each service already runs — that is what the machine inventory on the
// left is for, and repeating it here would repaint the card, dropping the
// choice in its dropdown, every time anything moved in the fleet.
func TestTheCatalogListsWhatTheTreeDescribes(t *testing.T) {
	view, _, _ := actingPanel(t)
	get(t, view, "/")

	catalog := get(t, view, "/act/select/services").Body.String()
	for _, expected := range []string{"caddy", "other", `data-deploy="caddy"`, `class="where"`, `value="yuki"`} {
		if !strings.Contains(catalog, expected) {
			t.Fatalf("the catalog is missing %q: %s", expected, catalog)
		}
	}
	tree := get(t, view, "/").Body.String()
	if !strings.Contains(tree, `data-act="select/services"`) {
		t.Fatalf("the tree has no way to reach the catalog: %s", tree)
	}
}

// An action gets a connection of its own, and a FRESH one. Its own, because
// one request owns a pipe from first byte to last: sharing the rounds' pipe
// would freeze the screen exactly while somebody waits on their click. Fresh,
// because a kept pipe is `hostd -stdio` on the far side, and that dies with
// every daemon restart and idle timeout — a pipe held from an hour ago is dead
// exactly when it is finally needed.
func TestEveryActionGetsAFreshConnectionOfItsOwn(t *testing.T) {
	view, _, _ := actingPanel(t)
	rounds, err := view.client("yuki")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	first, err := view.reach("yuki")
	if err != nil {
		t.Fatalf("reach: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := view.reach("yuki")
	if err != nil {
		t.Fatalf("reach again: %v", err)
	}
	defer func() { _ = second.Close() }()

	if rounds == first {
		t.Fatal("an action shares the telemetry pipe, so a slow action freezes the screen")
	}
	if first == second {
		t.Fatal("actions reuse a kept pipe, which is dead after any daemon restart")
	}
}

// The connection an action opened is closed when it is done: one ssh per click
// is fine, one ssh per click left behind is a leak that ends in a daemon that
// stops accepting.
func TestAnActionClosesWhatItOpened(t *testing.T) {
	view, sup, _ := actingPanel(t)
	dial := view.dial
	var mu sync.Mutex
	var opened []*api.Client
	view.dial = func(host string) (*api.Client, error) {
		client, err := dial(host)
		if err == nil {
			mu.Lock()
			opened = append(opened, client)
			mu.Unlock()
		}
		return client, err
	}
	get(t, view, "/")

	get(t, view, "/act/do/restart/yuki/caddy")
	waitFor(t, "the restart to land", func() bool { return sup.count(&sup.restarted) == 1 })

	mu.Lock()
	last := opened[len(opened)-1]
	mu.Unlock()
	// A closed connection cannot carry a request, which is the only way to ask
	// a client whether it is closed.
	waitFor(t, "the connection to be closed", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := last.Do(ctx, api.Request{Op: api.OpStatus})
		return err != nil
	})
}

// A failure is a line in the log like everything else, in the colour of a
// failure: there is nowhere else for it to go, and nothing to dismiss.
func TestAFailureIsALineInTheLog(t *testing.T) {
	view, _, _ := actingPanel(t)
	get(t, view, "/")

	get(t, view, "/act/do/deploy/yuki/nosuchservice")
	waitFor(t, "the failure to reach the log", func() bool {
		return strings.Contains(view.logText(), "does not describe")
	})
	page := get(t, view, "/").Body.String()
	if !strings.Contains(page, `class="err"`) {
		t.Fatalf("a failure is not marked as one in the log: %s", page)
	}
}

// logText is what the window's log pane holds, for a test to read.
func (p *panel) logText() string {
	p.snapMu.RLock()
	defer p.snapMu.RUnlock()
	var out strings.Builder
	for _, held := range p.snap.Lines {
		out.WriteString(held.Host + " " + held.Service + " " + held.Text + "\n")
	}
	return out.String()
}

// A machine behind this window says so in three places at once: an amber dot
// beside it in the tree, a line in the log, and a button beside its name.
func TestAMachineBehindThisWindowSaysSoThreeWays(t *testing.T) {
	view, _, _ := actingPanel(t)
	// The window carries whatever this build carries; the machine is made to
	// look older than any of it.
	view.snapMu.Lock()
	view.snap.Fleet[0].Version = "v0.0.1"
	view.snapMu.Unlock()
	if daemon.Version() == "" {
		t.Skip("this build carries no daemon, so nothing can differ from it")
	}

	page := get(t, view, "/").Body.String()
	if !strings.Contains(page, `class="dot alert"`) {
		t.Fatalf("the machine's dot is not amber: %s", page)
	}
	pane := get(t, view, "/act/select/host/yuki").Body.String()
	if !strings.Contains(pane, `id="alert"`) || !strings.Contains(pane, `data-act="do/install/yuki"`) {
		t.Fatalf("no alert beside the machine's name: %s", pane)
	}
	if !strings.Contains(pane, "hostd v0.0.1 here") {
		t.Fatalf("the alert does not name both versions: %s", pane)
	}

	view.sayAlert(view.snap.Fleet[0])
	if !strings.Contains(view.logText(), "v0.0.1 here") {
		t.Fatalf("the alert is not in the log: %s", view.logText())
	}
	// Said once, not every round: a round is two seconds apart.
	before := strings.Count(view.logText(), "v0.0.1 here")
	for range 3 {
		view.sayAlert(view.snap.Fleet[0])
	}
	if strings.Count(view.logText(), "v0.0.1 here") != before {
		t.Fatal("the alert repeats itself every round")
	}
}

// A machine level-with this window is quiet: no dot, no line, no button.
func TestAMachineThatIsCurrentSaysNothing(t *testing.T) {
	view, _, _ := actingPanel(t)
	view.snapMu.Lock()
	view.snap.Fleet[0].Version = daemon.Version()
	view.snapMu.Unlock()

	pane := get(t, view, "/act/select/host/yuki").Body.String()
	if strings.Contains(pane, `id="alert"`) {
		t.Fatalf("a current machine is warned about anyway: %s", pane)
	}
}

// The warning clears itself: an install replaces the daemon, so what this
// window remembers of its version is dropped and the next round asks again.
// Left cached, the amber dot would stay on a machine that is now current.
func TestUpdatingAMachineForgetsItsOldVersion(t *testing.T) {
	view, _, _ := actingPanel(t)
	view.snapMu.Lock()
	view.snap.Fleet[0].Version = "v0.0.1"
	view.snapMu.Unlock()
	view.sayAlert(view.snap.Fleet[0])

	view.forgetVersion("yuki")
	version, _, _, _ := view.knownClock("yuki")
	if version != "" {
		t.Fatal("the old version survived the update, so the dot would stay amber")
	}
	// And the same warning can be said again if it turns out to still be true.
	said := strings.Count(view.logText(), "v0.0.1 here")
	view.sayAlert(fleetHost{Host: "yuki", Version: "v0.0.1"})
	if strings.Count(view.logText(), "v0.0.1 here") == said {
		t.Fatal("a warning silenced before the update stays silenced after it")
	}
}

// The services screen is read on the way in and not again: it carries a
// dropdown per service, and a pane the rounds replace is a dropdown that jumps
// back to its first machine — under the pointer of somebody deploying several
// services one after another.
func TestTheCatalogDoesNotRepaintWhileDeploying(t *testing.T) {
	view, sup, _ := actingPanel(t)
	get(t, view, "/")
	first := get(t, view, "/act/select/services").Body.String()
	if !strings.Contains(first, `class="where"`) {
		t.Fatalf("the catalog has no machine chooser: %s", first)
	}

	// Rounds while the screen is open touch nothing on it.
	for range 3 {
		if again := get(t, view, "/act/refresh").Body.String(); strings.Contains(again, `id="detail"`) {
			t.Fatalf("a round replaced the catalog: %q", again)
		}
	}
	// And neither does a deploy, which is the case that mattered: the chooser
	// must survive being clicked through six times in a row.
	get(t, view, "/act/do/deploy/yuki/caddy")
	waitFor(t, "the deploy to land", func() bool { return sup.count(&sup.redeployed) == 1 })
	after := get(t, view, "/act/refresh").Body.String()
	if strings.Contains(after, `id="detail"`) {
		t.Fatalf("a deploy replaced the catalog, resetting its dropdowns: %q", after)
	}
}

// A machine that is switched off is not a machine missing a daemon: offering to
// install onto it would offer something that cannot work. ssh reaching a
// machine where nothing answers IS that case, and gets the button.
func TestOnlyAMachineMissingItsDaemonIsOfferedOne(t *testing.T) {
	if daemon.Version() == "" {
		t.Skip("this build carries no daemon to offer")
	}
	off := alertFor(fleetHost{Host: "cronos", Error: "ssh: connect to host cronos port 22: Operation timed out"})
	if off.Act != "" {
		t.Fatalf("a machine that is off was offered an install: %+v", off)
	}
	if off.Text == "" {
		t.Fatal("a machine that is off says nothing at all")
	}
	bare := alertFor(fleetHost{Host: "fresh", Error: "hostd on fresh closed the connection", NoDaemon: true})
	if bare.Act != "do/install/fresh" {
		t.Fatalf("a machine with no daemon was not offered one: %+v", bare)
	}
}

// The file actions carry the whole wire path through the click, service first,
// and refuse the shapes that carry too little to act on.
func TestFileActionsParse(t *testing.T) {
	view, _, _ := actingPanel(t)
	work, err := view.action([]string{"filedl", "yuki", "site", "data", "x.txt"})
	if err != nil || work == nil {
		t.Fatalf("a download click did not become work: %v", err)
	}
	work, err = view.action([]string{"fileup", "yuki", "site", "data"})
	if err != nil || work == nil {
		t.Fatalf("an upload click did not become work: %v", err)
	}
	_, err = view.action([]string{"filedl", "yuki", "site"})
	if err == nil {
		t.Fatal("a download with no file to download was accepted")
	}
}
