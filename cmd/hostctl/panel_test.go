package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/crgimenes/glaze"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/supervisor"
)

func probePanel(t *testing.T) *panel {
	t.Helper()
	view, err := newPanel(options{remote: "hostd -stdio"}, []string{"yuki", "selene"})
	if err != nil {
		t.Fatalf("newPanel: %v", err)
	}
	now := float64(time.Now().UnixMilli())
	view.snap = snapshot{
		Updated: time.Now(),
		Fleet: []fleetHost{{
			Host: "yuki",
			Services: []supervisor.Status{
				{Name: "caddy", State: supervisor.StateRunning, Image: "caddy:2", Since: now - 4_500_000, Restarts: 1},
				{Name: "ticker", State: supervisor.StateScheduled, Image: "busybox", Every: "10s"},
				{Name: "queue", State: supervisor.StateFailed, Image: "queue:7", LastError: "image queue:7 is not on this machine"},
			},
			Metrics: []metrics.Series{
				{Scope: metrics.ScopeHost, Metric: metrics.MetricCPUPercent, Points: []metrics.Point{
					{TimeMS: now - 20000, Value: 12}, {TimeMS: now - 10000, Value: 30}, {TimeMS: now, Value: 21}}},
				{Scope: metrics.ScopeHost, Metric: metrics.MetricMemoryBytes, Points: []metrics.Point{
					{TimeMS: now - 10000, Value: 5 << 30}, {TimeMS: now, Value: 6 << 30}}},
				{Scope: metrics.ScopeHost, Metric: metrics.MetricMemoryTotal, Points: []metrics.Point{{TimeMS: now, Value: 16 << 30}}},
				{Scope: metrics.ScopeService, Name: "caddy", Metric: metrics.MetricMemoryBytes, Points: []metrics.Point{
					{TimeMS: now - 10000, Value: 90 << 20}, {TimeMS: now, Value: 96 << 20}}},
				{Scope: metrics.ScopeService, Name: "ticker", Metric: metrics.MetricMemoryBytes, Points: []metrics.Point{
					{TimeMS: now - 10000, Value: 12 << 20}, {TimeMS: now, Value: 13 << 20}}},
			},
		}, {
			Host:  "selene",
			Error: "ssh: connect to host selene port 22: Operation timed out",
		}},
		Lines: []line{
			{Seq: 1, Time: now - 4000, Service: "caddy", Stream: "out", Text: "GET /health 200", Host: "yuki", N: 1},
			{Seq: 2, Time: now - 2000, Service: "ticker", Stream: "event", Run: "1787506200000", Text: "run finished with exit 0", Host: "yuki", N: 2},
		},
	}
	return view
}

func get(t *testing.T, view *panel, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	view.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s answered %d: %s", path, recorder.Code, recorder.Body)
	}
	return recorder
}

// The whole reason the window is served in pieces: when the fleet did not
// change, the answer is empty and nothing on screen is replaced. A panel that
// re-sends what it already sent is a panel whose rows move out from under the
// pointer and whose selected text disappears every two seconds.
func TestAFleetThatDidNotChangeSendsNothing(t *testing.T) {
	view := probePanel(t)
	first := get(t, view, "/").Body.String()
	if !strings.Contains(first, `id="tree"`) || !strings.Contains(first, "caddy") {
		t.Fatalf("the page is missing the fleet: %s", first)
	}

	for round := range 3 {
		again := get(t, view, "/act/refresh").Body.String()
		if again != "" {
			t.Fatalf("round %d re-sent what the window already holds: %q", round, again)
		}
	}
}

// A state that flips repaints its dot and nothing anybody is holding: the tree
// and the pane are replaced only when their structure changes, because a shell
// swapped under a pointer eats the click — which is what "I click a machine
// and it closes by itself" was.
func TestAStateChangeRepaintsTheDotAndNotTheTree(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/")

	view.snapMu.Lock()
	view.snap.Fleet[0].Services[0].State = supervisor.StateFailed
	view.snapMu.Unlock()

	answer := get(t, view, "/act/refresh").Body.String()
	if strings.Contains(answer, `id="tree"`) {
		t.Fatalf("a state flip replaced the whole tree: %q", answer)
	}
	if strings.Contains(answer, `id="detail"`) {
		t.Fatalf("a state flip replaced the whole pane: %q", answer)
	}
	if !strings.Contains(answer, `id="tdot-svc-yuki-caddy"`) {
		t.Fatalf("the dot that changed was not sent: %q", answer)
	}
	if !strings.Contains(answer, `id="tmeta-host-yuki"`) {
		t.Fatalf("the running count changed and was not sent: %q", answer)
	}
	if strings.Contains(answer, `id="status"`) {
		t.Fatalf("the status did not change and was sent anyway: %q", answer)
	}
}

// A service appearing is a structure change, and structure changes replace the
// shell: that is the one moment a full swap is the truth.
func TestANewServiceReplacesTheTree(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/")

	view.snapMu.Lock()
	view.snap.Fleet[0].Services = append(view.snap.Fleet[0].Services,
		supervisor.Status{Name: "fresh", State: supervisor.StateRunning, Image: "fresh:1"})
	view.snapMu.Unlock()

	answer := get(t, view, "/act/refresh").Body.String()
	if !strings.Contains(answer, `id="tree"`) {
		t.Fatalf("a new service did not replace the tree: %q", answer)
	}
	if !strings.Contains(answer, "fresh") {
		t.Fatalf("the new service is not in what was sent: %q", answer)
	}
}

// New lines are appended, never re-sent: the window keeps what it is showing.
func TestOnlyNewLinesAreSent(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/")

	answer := get(t, view, "/act/refresh").Body.String()
	if strings.Contains(answer, "GET /health") {
		t.Fatalf("a line the window already holds was sent again: %q", answer)
	}

	view.snapMu.Lock()
	view.snap.Lines = append(view.snap.Lines, line{
		Seq: 3, Service: "caddy", Text: "something new", Host: "yuki", N: 3,
	})
	view.snapMu.Unlock()

	answer = get(t, view, "/act/refresh").Body.String()
	if !strings.Contains(answer, "something new") {
		t.Fatalf("the new line was not sent: %q", answer)
	}
	if !strings.Contains(answer, `data-swap="append:#lines"`) {
		t.Fatalf("the new line does not append: %q", answer)
	}
	if strings.Contains(answer, "GET /health") {
		t.Fatalf("the old lines came with it: %q", answer)
	}
}

// Selecting a machine changes what the log is about, so the pane is replaced
// rather than appended to.
func TestSelectingAMachineAsksTheLogAgain(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/")

	answer := get(t, view, "/act/select/service/yuki/ticker").Body.String()
	if !strings.Contains(answer, `<ol id="lines">`) {
		t.Fatalf("the log was not replaced: %q", answer)
	}
	if strings.Contains(answer, "GET /health") {
		t.Fatalf("a line of another service survived the selection: %q", answer)
	}
	if !strings.Contains(answer, "run finished with exit 0") {
		t.Fatalf("the service's own lines are missing: %q", answer)
	}
}

// A machine that did not answer belongs in the picture, where the operator can
// read why — and copy it.
func TestAMachineThatDidNotAnswerSaysWhy(t *testing.T) {
	view := probePanel(t)
	page := get(t, view, "/").Body.String()
	if !strings.Contains(page, "Operation timed out") {
		t.Fatalf("the unreachable machine is silent: %s", page)
	}
}

// The panel is one binary: a page that fetches anything from the network is a
// page that breaks on the laptop that is offline when a machine goes down.
func TestNothingInTheWindowReachesTheNetwork(t *testing.T) {
	for _, name := range []string{"ui/panel.tmpl", "ui/app.css", "ui/app.js"} {
		content, err := ui.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, forbidden := range []string{"http://", "https://", "//cdn"} {
			if strings.Contains(string(content), forbidden) {
				t.Fatalf("%s reaches outside the binary: %s", name, forbidden)
			}
		}
	}
}

// Not a test: how the panel is looked at without opening a window on somebody's
// screen. It serves the same handler the window serves, over a port, only when
// asked for by name.
func TestServeThePanelForLooking(t *testing.T) {
	addr := os.Getenv("HOSTD_PANEL_ADDR")
	if addr == "" {
		t.Skip("set HOSTD_PANEL_ADDR to serve the panel for looking at")
	}
	view := probePanel(t)
	server := &http.Server{Addr: addr, Handler: view.routes(), ReadHeaderTimeout: 5 * time.Second}
	t.Log("serving the panel on", addr)
	_ = server.ListenAndServe()
}

// The menu drives the page by calling functions in it. A menu item that names
// a function the page does not have is a menu item that does nothing and says
// nothing, which is worse than a menu item that is not there.
func TestEveryMenuItemCallsSomethingThePageHas(t *testing.T) {
	source, err := os.ReadFile("menu.go")
	if err != nil {
		t.Fatalf("read menu.go: %v", err)
	}
	script, err := ui.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	called := regexp.MustCompile(`call\("([a-zA-Z]+)\(`).FindAllStringSubmatch(string(source), -1)
	if len(called) == 0 {
		t.Fatal("no menu item reaches the page; the pattern this test matches must have changed")
	}
	for _, match := range called {
		if !strings.Contains(string(script), "function "+match[1]+"(") {
			t.Fatalf("the menu calls %s and the page has no such function", match[1])
		}
	}
}

// The defect an operator sees as "I click a machine and it shuts by itself":
// the triangle sits inside the row, and if a click reached both, two actions
// would race and whichever answered last would paint the older view. The page
// dispatches a click to the NEAREST act and that one only, so nesting cannot
// double an action.
func TestOneClickIsOneAct(t *testing.T) {
	script, err := ui.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	if !strings.Contains(string(script), `closest("[data-act]")`) {
		t.Fatal("clicks are not dispatched to the nearest act")
	}
	tree, err := ui.ReadFile("ui/panel.tmpl")
	if err != nil {
		t.Fatalf("read panel.tmpl: %v", err)
	}
	if !strings.Contains(string(tree), `data-act="{{.Toggle}}"`) {
		t.Fatal("the triangle carries no act of its own")
	}
}

// Asking about a machine is asking what is on it.
func TestSelectingAMachineOpensIt(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/")
	get(t, view, "/act/toggle/yuki")
	if !view.viewport().closed["yuki"] {
		t.Fatal("the toggle did not shut the machine")
	}

	get(t, view, "/act/select/host/yuki")
	if view.viewport().closed["yuki"] {
		t.Fatal("a machine that was asked about stayed shut")
	}
	answer := get(t, view, "/act/refresh").Body.String()
	if strings.Contains(answer, `id="tree"`) {
		t.Fatalf("the round after the click undid it: %q", answer)
	}
}

// Two answers in flight must not paint each other's past. Rendering happens
// under the same lock as the bookkeeping, so whichever goes second sees the
// view the first one left.
func TestConcurrentAnswersNeverPaintAnOlderView(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/")

	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			recorder := httptest.NewRecorder()
			view.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/act/refresh", nil))
		})
		wg.Go(func() {
			recorder := httptest.NewRecorder()
			view.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/act/select/host/yuki", nil))
		})
	}
	wg.Wait()

	if view.viewport().closed["yuki"] {
		t.Fatal("the machine ended shut after being asked about")
	}
	// Whatever the window holds now is what the view says, so one more round
	// changes nothing.
	settled := get(t, view, "/act/refresh").Body.String()
	again := get(t, view, "/act/refresh").Body.String()
	if again != "" {
		t.Fatalf("the panel never settles; it sent %q after sending %q", again, settled)
	}
}

// The collapse itself: a round that fails answers with no services, and a tree
// drawn from that answer shows the machine childless — which on the screen is
// "I clicked yuki and moments later it closed". What was known survives the
// round that could not confirm it.
func TestABadRoundDoesNotUnmakeTheTree(t *testing.T) {
	view := probePanel(t)
	first := get(t, view, "/").Body.String()
	if !strings.Contains(first, "caddy") {
		t.Fatalf("the page is missing the fleet: %s", first)
	}

	view.absorb([]fleetHost{
		{Host: "yuki", Error: "ssh: broken pipe"},
		{Host: "selene", Error: "ssh: connect to host selene port 22: Operation timed out"},
	})

	answer := get(t, view, "/act/refresh").Body.String()
	if strings.Contains(answer, `id="tree"`) {
		t.Fatalf("a failed round replaced the tree: %q", answer)
	}
	page := get(t, view, "/").Body.String()
	if !strings.Contains(page, "caddy") || !strings.Contains(page, "ticker") {
		t.Fatalf("the services vanished with the round that failed: %s", page)
	}
}

// The window is written to, never read from: the poller pushes what changed,
// and a fleet where nothing happened pushes NOTHING — no message, no touched
// HTML, no lost selection. This is the panel's central promise.
func TestAQuietFleetPushesNothing(t *testing.T) {
	view := probePanel(t)
	var pushes []string
	view.emit = func(js string) { pushes = append(pushes, js) }
	get(t, view, "/")

	for range 3 {
		view.push()
	}
	if len(pushes) != 0 {
		t.Fatalf("a quiet fleet pushed %d time(s): %q", len(pushes), pushes[0])
	}

	view.snapMu.Lock()
	view.snap.Fleet[0].Services[0].State = supervisor.StateFailed
	view.snapMu.Unlock()
	view.push()
	if len(pushes) != 1 {
		t.Fatalf("a state change pushed %d time(s)", len(pushes))
	}
	if !strings.Contains(pushes[0], "hostdApply(") || !strings.Contains(pushes[0], "tdot-svc-yuki-caddy") {
		t.Fatalf("the push does not carry the dot that changed: %q", pushes[0])
	}
	view.push()
	if len(pushes) != 1 {
		t.Fatalf("the same change was pushed twice")
	}
}

// A line the first page already carries is never pushed again: the sender
// knows what it sent, and the window is not asked what it holds.
func TestALineThePageCarriesIsNotPushedAgain(t *testing.T) {
	view := probePanel(t)
	var pushes []string
	view.emit = func(js string) { pushes = append(pushes, js) }

	page := get(t, view, "/").Body.String()
	if !strings.Contains(page, "GET /health 200") {
		t.Fatalf("the first page does not carry the log it already has: %s", page)
	}
	view.push()
	if len(pushes) != 0 {
		t.Fatalf("a line the page holds was pushed again: %q", pushes)
	}

	view.snapMu.Lock()
	view.snap.Lines = append(view.snap.Lines, line{
		Seq: 9, Service: "caddy", Text: "fresh line", Host: "yuki", N: 3,
	})
	view.snapMu.Unlock()
	view.push()
	if len(pushes) != 1 || !strings.Contains(pushes[0], "fresh line") {
		t.Fatalf("the new line did not arrive: %q", pushes)
	}
	if strings.Contains(pushes[0], "GET /health 200") {
		t.Fatalf("old lines came with it: %q", pushes[0])
	}
}

// glaze binds a method as one flat global named prefix_method: Act becomes
// window.hostd_act. The name is not written by hand on either side of this
// test — glaze produces it, and the page must call what it produced. The
// first panel shipped calling a name nothing had bound, and rendered blank.
func TestThePageCallsTheNameGlazeBinds(t *testing.T) {
	recorder := &boundNames{}
	names, err := glaze.BindMethods(recorder, "hostd", &panel{})
	if err != nil {
		t.Fatalf("BindMethods: %v", err)
	}
	script, err := ui.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	found := false
	for _, name := range names {
		if name == "hostd_act" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Act is not bound; glaze bound %v", names)
	}
	if !strings.Contains(string(script), "window.hostd_act(") {
		t.Fatal("the page never calls hostd_act, which is the only way a click reaches the panel")
	}
}

// Only Bind is of any interest; the rest is what the interface demands.
type boundNames struct{ names []string }

func (b *boundNames) Bind(name string, _ any) error {
	b.names = append(b.names, name)
	return nil
}

func (b *boundNames) Run()                                                  {}
func (b *boundNames) Terminate()                                            {}
func (b *boundNames) Dispatch(func())                                       {}
func (b *boundNames) Destroy()                                              {}
func (b *boundNames) Window() unsafe.Pointer                                { return nil }
func (b *boundNames) SetTitle(string)                                       {}
func (b *boundNames) SetSize(int, int, glaze.Hint)                          {}
func (b *boundNames) Navigate(string)                                       {}
func (b *boundNames) SetHtml(string)                                        {}
func (b *boundNames) Init(string)                                           {}
func (b *boundNames) Eval(string)                                           {}
func (b *boundNames) Focus()                                                {}
func (b *boundNames) Raise()                                                {}
func (b *boundNames) Unbind(string) error                                   { return nil }
func (b *boundNames) OpenFile(glaze.FileDialogOptions) (string, error)      { return "", nil }
func (b *boundNames) OpenFiles(glaze.FileDialogOptions) ([]string, error)   { return nil, nil }
func (b *boundNames) SaveFile(glaze.FileDialogOptions) (string, error)      { return "", nil }
func (b *boundNames) OpenDirectory(glaze.FileDialogOptions) (string, error) { return "", nil }

// The Settings page: the person who never touches a terminal can still see
// where the tree lives, point the panel somewhere else, and read what this
// binary is.
func TestTheSettingsPageSaysWhereTheTreeLives(t *testing.T) {
	view := probePanel(t)
	view.cfgDir = "/somewhere/fleet"
	page := get(t, view, "/act/select/settings").Body.String()
	for _, expected := range []string{"Settings", "/somewhere/fleet", "inventory.filo", "config/choose", "config/reload"} {
		if !strings.Contains(page, expected) {
			t.Fatalf("the settings page is missing %q: %s", expected, page)
		}
	}
	tree := get(t, view, "/").Body.String()
	if !strings.Contains(tree, `data-act="select/settings"`) {
		t.Fatalf("the tree has no way to reach the settings: %s", tree)
	}
}

// Reload re-reads the inventory under the root, and what the tree stopped
// listing is let go of, connection included.
func TestReloadRereadsTheInventory(t *testing.T) {
	view := probePanel(t)
	dir := t.TempDir()
	view.cfgDir = dir
	inventory := filepath.Join(dir, "inventory.filo")
	err := os.WriteFile(inventory, []byte(`(inventory
	  (host (tuple "name" "yuki"))
	  (host (tuple "name" "fresh.local")))`), 0o600)
	if err != nil {
		t.Fatalf("write inventory: %v", err)
	}

	get(t, view, "/act/config/reload")
	hosts := view.hostsNow()
	if !slices.Equal(hosts, []string{"yuki", "fresh.local"}) {
		t.Fatalf("the panel watches %v after the reload", hosts)
	}
	if view.settings().Problem != "" {
		t.Fatalf("a good reload left a problem: %q", view.settings().Problem)
	}

	// A tree that lists nothing is refused, and what was watched stays.
	err = os.WriteFile(inventory, []byte(`(inventory)`), 0o600)
	if err != nil {
		t.Fatalf("rewrite inventory: %v", err)
	}
	get(t, view, "/act/config/reload")
	if !slices.Equal(view.hostsNow(), []string{"yuki", "fresh.local"}) {
		t.Fatal("an empty inventory was obeyed")
	}
	if view.settings().Problem == "" {
		t.Fatal("an empty inventory was refused in silence")
	}
}

// Outside the window there is no native chooser; the page says so instead of
// doing nothing, because a button that silently does nothing is a defect
// nobody can report.
func TestChoosingWithoutAWindowSaysSo(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/act/config/choose")
	if view.settings().Problem == "" {
		t.Fatal("choose without a window said nothing")
	}
}

// Icons come with the binary, never from a network, and every one the page
// asks for has to exist: a missing icon would render as nothing at all.
func TestEveryIconThePageAsksForIsCarried(t *testing.T) {
	page, err := ui.ReadFile("ui/panel.tmpl")
	if err != nil {
		t.Fatalf("read panel.tmpl: %v", err)
	}
	asked := regexp.MustCompile(`icon "([a-z0-9-]+)"`).FindAllStringSubmatch(string(page), -1)
	if len(asked) == 0 {
		t.Fatal("the page asks for no icon; the pattern this test matches must have changed")
	}
	for _, match := range asked {
		_, err := iconSVG(match[1])
		if err != nil {
			t.Fatalf("the page asks for the %q icon and the binary does not carry it", match[1])
		}
	}
	// The ones chosen in Go rather than in the template.
	view := probePanel(t)
	for _, path := range []string{"/", "/act/select/settings", "/act/select/host/yuki"} {
		body := get(t, view, path).Body.String()
		if strings.Contains(body, "<svg") && strings.Contains(body, "http") {
			t.Fatalf("%s draws an icon from the network", path)
		}
	}
}

// The window has a floor, and the layout below it has a way to give the screen
// back: a sidebar that hides and a hamburger that returns it.
func TestTheWindowHasAFloorAndTheSidebarCanHide(t *testing.T) {
	source, err := os.ReadFile("gui.go")
	if err != nil {
		t.Fatalf("read gui.go: %v", err)
	}
	if !strings.Contains(string(source), "glaze.HintMin") {
		t.Fatal("the window can be dragged to nothing; it has no minimum size")
	}
	script, err := ui.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	for _, needed := range []string{"matchMedia", "hideNav", `el("reveal")`} {
		if !strings.Contains(string(script), needed) {
			t.Fatalf("the narrow layout has no %s", needed)
		}
	}
	page, err := ui.ReadFile("ui/panel.tmpl")
	if err != nil {
		t.Fatalf("read panel.tmpl: %v", err)
	}
	if !strings.Contains(string(page), `id="reveal"`) {
		t.Fatal("there is no way to bring the sidebar back")
	}
}

// A window that sits perfectly still while four machines are being reached for
// the first time reads as a frozen one. It says it is working — but only when
// the round is slow enough for a person to wonder, or the indicator would
// blink on every tick and the wire would never be quiet.
func TestTheWindowIsToldWhenTheFleetIsSlow(t *testing.T) {
	view := probePanel(t)
	var pushes []string
	view.emit = func(js string) { pushes = append(pushes, js) }
	get(t, view, "/")

	// A round that answers at once says nothing at all.
	finished := make(chan struct{})
	close(finished)
	view.sayIfSlow(finished)
	if len(pushes) != 0 {
		t.Fatalf("a quick round announced itself: %q", pushes)
	}

	// One that drags says so, once.
	view.working(true)
	if len(pushes) != 1 || !strings.Contains(pushes[0], "spinner") {
		t.Fatalf("a slow round did not raise the indicator: %q", pushes)
	}
	view.working(true)
	if len(pushes) != 1 {
		t.Fatal("the indicator was raised twice for one state")
	}
	view.working(false)
	if len(pushes) != 2 || strings.Contains(pushes[1], "spinner") {
		t.Fatalf("the indicator was not lowered: %q", pushes)
	}
}

// Before the first answer the window already says it is working: that is the
// round the operator is most likely to mistake for a freeze.
func TestTheFirstPaintAlreadySaysItIsWorking(t *testing.T) {
	view, err := newPanel(options{}, []string{"yuki"})
	if err != nil {
		t.Fatalf("newPanel: %v", err)
	}
	page := get(t, view, "/").Body.String()
	if !strings.Contains(page, "spinner") || !strings.Contains(page, "asking the fleet") {
		t.Fatalf("the first paint looks idle: %s", page)
	}
}

// The window buttons and the log are about the machines. A page about the
// panel itself shows neither: a control that governs nothing on screen is a
// control that lies about what it does.
func TestTheSettingsPageHasNoFleetControls(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/")

	settings := get(t, view, "/act/select/settings").Body.String()
	if strings.Contains(settings, `id="windows"`) {
		t.Fatalf("the settings page offers a time window: %s", settings)
	}
	if !strings.Contains(settings, `data-log="off"`) {
		t.Fatalf("the settings page does not put the log away: %s", settings)
	}

	// And a page that IS about the machines keeps both.
	watching := get(t, view, "/act/select/host/yuki").Body.String()
	if !strings.Contains(watching, `id="windows"`) || !strings.Contains(watching, `data-log="on"`) {
		t.Fatalf("a machine's page lost its controls: %s", watching)
	}
}
