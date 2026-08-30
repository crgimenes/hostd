package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crgimenes/glaze"
	"github.com/crgimenes/glaze/menu"
	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/supervisor"
)

//go:embed ui
var ui embed.FS

// The panel is hostctl in another presentation, never a second implementation:
// every number on the screen came from the same operation the command line
// calls, over the same ssh.
//
// It is served from a custom scheme, so nothing on the operator's machine
// listens on a port. A loopback server would be one more door on the machine of
// the person who is only looking at graphs.
const uiOrigin = "app://hostd/"

// What the panel is: the operator's window on the fleet. It watches by being
// pushed to, and it acts — always the same API operation the command line
// calls, behind a confirmation that still shows the terminal equivalent.
type panel struct {
	opt   options
	hosts []string
	// The lifetime of the panel's ssh connections. DialSSH hands the context
	// to exec.CommandContext, which KILLS the process when it ends — so a
	// connection dialed with a round's context died with the round, and every
	// other refresh found four broken pipes where its fleet used to be.
	life context.Context

	// The machines, their connections and their loops share one lock: letting
	// go of a machine, closing what was open for it and stopping its loop is
	// one decision.
	mu      sync.Mutex
	clients map[string]*api.Client
	loops   map[string]*hostLoop

	// one behind a seam: proving a slow machine cannot delay a fast one needs
	// a machine that is slow on purpose.
	ask func(ctx context.Context, host string, fromMS, toMS float64, since uint64, wantImages bool) fleetHost
	// How a machine is reached — ssh normally; the tests hand over a dial to an
	// in-process daemon, because proving the window acts must not need a fleet.
	dial func(host string) (*api.Client, error)

	// Where the panel's tree lives, changeable from the Settings page: the
	// window is also for people who do not live in a terminal, and a path
	// only a flag can change is a path only a terminal can change.
	cfgMu      sync.Mutex
	cfgDir     string
	cfgProblem string

	// The native directory chooser, absent outside the window (tests, the
	// harness): choosing needs a desktop, reading does not.
	dialogs interface {
		OpenDirectory(glaze.FileDialogOptions) (string, error)
		OpenFile(glaze.FileDialogOptions) (string, error)
	}

	snapMu sync.RWMutex
	snap   snapshot
	since  map[string]uint64
	held   map[string]bool
	cursor uint64

	// The actions running right now, by act: their buttons render held down,
	// and a second click on one does nothing. It lives here rather than in the
	// page because a fragment the rounds replace would take a class with it.
	busyMu sync.Mutex
	busy   map[string]bool
	// The machine-level warnings already said in the log, so a round does not
	// repeat one every two seconds.
	said map[string]bool

	// What the tree describes, as of the last time the services screen was
	// opened: the rounds never re-read it, so that pane never repaints under
	// somebody choosing a machine in it.
	catalogMu   sync.Mutex
	catalogList []service.Declaration

	// One directory of one service's data, for the files screen. Same rule as
	// the catalog: read on the way in and on navigation, never on the poll.
	filesMu sync.Mutex
	files   filesState

	// Destructive buttons arm on the first click and act on the second, so a
	// slip beside the download button deletes nothing. Armed is a moment, not
	// a mode: it expires by itself.
	armedMu sync.Mutex
	armed   map[string]time.Time

	viewMu sync.Mutex
	view   viewState

	// How the fleet reaches the window: the poller pushes what changed into
	// the page, and a round where nothing changed pushes nothing. The page
	// never asks — a screen polling a server is a screen that touches its own
	// HTML on a clock, and anything the operator held is lost to the tick.
	emit func(js string)

	// The newest log line the window holds, in the panel's own numbering.
	pushed uint64

	// What the window already holds, by fragment. A piece whose digest has not
	// moved is never sent again, and a piece that is never sent is a piece that
	// cannot flicker under somebody's pointer.
	sentMu sync.Mutex
	sent   map[string]string

	pages *template.Template
}

func newPanel(opt options, hosts []string) (*panel, error) {
	pages, err := template.New("panel.tmpl").
		Funcs(template.FuncMap{
			"icon": iconSVG,
			// The last cell of a grid row is where a row action renders.
			"lastCell": func(cells []string) int { return len(cells) - 1 },
		}).
		ParseFS(ui, "ui/panel.tmpl")
	if err != nil {
		return nil, err
	}
	view := &panel{
		opt:     opt,
		hosts:   hosts,
		cfgDir:  opt.config,
		clients: map[string]*api.Client{},
		loops:   map[string]*hostLoop{},
		since:   map[string]uint64{},
		held:    map[string]bool{},
		busy:    map[string]bool{},
		said:    map[string]bool{},
		sent:    map[string]string{},
		view:    viewState{kind: "fleet", window: 3600, closed: map[string]bool{}, opened: map[string]bool{}},
		pages:   pages,
	}
	view.ask = view.one
	view.dial = func(host string) (*api.Client, error) {
		life := view.life
		// Dialing before start (an action in a panel a test never started)
		// must not hand exec a nil context.
		if life == nil {
			life = context.Background()
		}
		// Quiet unless -debug: the rounds retry a machine with no daemon every
		// two seconds, and its complaint on the terminal every time reads as
		// the program looping. The card already says the machine is not
		// answering.
		diagnostics := io.Discard
		if view.opt.debug {
			diagnostics = os.Stderr
		}
		return api.DialSSHDiag(life, host, strings.Fields(view.opt.remote), diagnostics)
	}
	return view, nil
}

// Fleet answers with everything the overview needs, one entry per machine. A
// machine that did not answer says so in its own entry: which host is missing
// is part of the picture, not an error that hides the rest.
type fleetHost struct {
	Host     string              `json:"host"`
	Error    string              `json:"error,omitempty"`
	Services []supervisor.Status `json:"services"`
	Metrics  []metrics.Series    `json:"metrics"`
	Lines    []api.LogLine       `json:"lines"`
	// Only the machine whose image screen is open carries these. Asking every
	// machine every two seconds for a list nobody is reading would spend the
	// fleet's round on it.
	Images      []api.ImageEntry `json:"images"`
	ImagesError string           `json:"images-error,omitempty"`
	Since       uint64           `json:"since"`
	// What hostd the machine runs, asked once per connection: it only changes
	// when the daemon is replaced, and that is exactly when this window drops
	// the connection anyway.
	Version string `json:"version"`
	// The machine's clock against this window's, and its timezone, from the
	// same describe. The skew is what lets the header show the machine's
	// CURRENT time instead of a stale instant; timezone is the operator's
	// responsibility, and showing it is the reminder. Zone empty means a
	// daemon from before the field.
	ClockSkewMS  float64 `json:"clock-skew-ms"`
	Zone         string  `json:"zone"`
	ZoneOffsetMS float64 `json:"zone-offset-ms"`
	// ssh reached the machine and no hostd answered — which is the one failure
	// an install fixes. A machine that is simply switched off is not this, and
	// offering to install onto it would be offering something that cannot work.
	NoDaemon bool `json:"no-daemon,omitempty"`
}

// forgetVersion drops what this window remembers of a machine's hostd, so the
// next round asks again. Called where the daemon was replaced: a version that
// stayed cached across an install would leave the amber dot on a machine that
// had just been brought up to date.
func (p *panel) forgetVersion(host string) {
	p.snapMu.Lock()
	for at := range p.snap.Fleet {
		if p.snap.Fleet[at].Host == host {
			p.snap.Fleet[at].Version = ""
		}
	}
	p.snapMu.Unlock()
	// And the warning it caused can be said again, if it is still true.
	p.busyMu.Lock()
	for key := range p.said {
		if strings.HasPrefix(key, host+"\x00") {
			delete(p.said, key)
		}
	}
	p.busyMu.Unlock()
}

// What this window last heard the machine's hostd call itself, and the clock
// that came with it.
func (p *panel) knownClock(host string) (version string, skewMS float64, zone string, offsetMS float64) {
	p.snapMu.RLock()
	defer p.snapMu.RUnlock()
	for _, known := range p.snap.Fleet {
		if known.Host == host {
			return known.Version, known.ClockSkewMS, known.Zone, known.ZoneOffsetMS
		}
	}
	return "", 0, "", 0
}

func (p *panel) hostsNow() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.hosts)
}

// chooseConfig asks the person, with the system's own directory chooser, and
// is why this runs inside an Act: the dialog blocks its goroutine, and a Bind
// callback is off the UI thread by contract.
func (p *panel) chooseConfig() {
	if p.dialogs == nil {
		p.setConfigProblem("choosing a directory needs the window; use -config on the command line here")
		return
	}
	dir, err := p.dialogs.OpenDirectory(glaze.FileDialogOptions{Title: "Choose the configuration directory"})
	if err != nil {
		p.setConfigProblem(err.Error())
		return
	}
	if dir == "" {
		return
	}
	p.cfgMu.Lock()
	p.cfgDir = dir
	p.cfgMu.Unlock()
	p.reloadFleet()
}

// reloadFleet re-reads the inventory under the configuration root and watches
// what it now says. A tree that lists nothing is refused, not obeyed: an empty
// window teaches nobody what went wrong.
func (p *panel) reloadFleet() {
	p.cfgMu.Lock()
	dir := p.cfgDir
	p.cfgMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entries, err := readInventory(ctx, filepath.Join(dir, service.InventoryFile))
	if err != nil {
		p.setConfigProblem(err.Error())
		return
	}
	if len(entries) == 0 {
		p.setConfigProblem(fmt.Sprintf("no machine is listed in %s", filepath.Join(dir, service.InventoryFile)))
		return
	}
	p.setConfigProblem("")

	hosts := make([]string, 0, len(entries))
	for _, entry := range entries {
		hosts = append(hosts, entry.Name)
	}
	p.mu.Lock()
	kept := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		kept[host] = true
	}
	for host, client := range p.clients {
		if kept[host] {
			continue
		}
		_ = client.Close()
		delete(p.clients, host)
	}
	p.hosts = hosts
	p.mu.Unlock()
	// A machine taken out of the inventory leaves the picture too: the tree
	// would otherwise carry it until the window closed.
	p.snapMu.Lock()
	p.snap.Fleet = slices.DeleteFunc(p.snap.Fleet, func(known fleetHost) bool { return !kept[known.Host] })
	p.snapMu.Unlock()
	p.syncLoops()
	p.loadCatalog()
	p.wake()
}

// catalog is what the tree describes, as it was read when its screen was
// opened. Deliberately NOT re-read by the rounds: this pane carries a dropdown
// per service, and a pane that is replaced is a dropdown that jumps back to
// its first machine — under the pointer of somebody deploying six services in
// a row (crg, 2026-08-27). It is refreshed on entering the screen and when the
// tree's root changes, which is when it can have changed.
func (p *panel) catalog(view viewState) []service.Declaration {
	if view.kind != "services" {
		return nil
	}
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	return p.catalogList
}

func (p *panel) loadCatalog() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A tree half of which cannot be read still describes the other half, and
	// the services screen is where somebody would go to notice.
	declarations, _ := service.LoadTree(ctx, p.configDir())
	p.catalogMu.Lock()
	p.catalogList = declarations
	p.catalogMu.Unlock()
}

func (p *panel) configDir() string {
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	return p.cfgDir
}

func (p *panel) setConfigProblem(text string) {
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	p.cfgProblem = text
}

// What the Settings page says. The version comes from the binary; the rest is
// what this window is pointed at right now.
func (p *panel) settings() settingsInfo {
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	return settingsInfo{
		ConfigDir: p.cfgDir,
		Inventory: filepath.Join(p.cfgDir, service.InventoryFile),
		Machines:  len(p.hostsNow()),
		Problem:   p.cfgProblem,
	}
}

func (p *panel) one(ctx context.Context, host string, fromMS, toMS float64, since uint64, wantImages bool) fleetHost {
	answer := fleetHost{Host: host, Since: since}
	if p.opt.debug {
		// Said on the way OUT as well as on the way in: a machine that never
		// answers would otherwise leave no trace at all until ssh gives up,
		// which is exactly the case somebody is trying to diagnose.
		fmt.Fprintf(os.Stderr, "debug reaching host=%s\n", host)
		start := time.Now()
		defer func() {
			fmt.Fprintf(os.Stderr, "debug round host=%s elapsed-ms=%.0f error=%q\n",
				host, float64(time.Since(start).Microseconds())/1000, answer.Error)
		}()
	}
	client, err := p.client(host)
	if err != nil {
		answer.Error = err.Error()
		return answer
	}

	err = ask(ctx, client, api.Request{Op: api.OpStatus}, &answer.Services)
	if err != nil {
		answer.Error = err.Error()
		// Both a machine that is switched off and a machine with no daemon end
		// as an empty pipe, so the answer to "which is it" comes from ssh
		// itself: when it complained, IT is the reason, and installing onto a
		// machine ssh cannot reach is not a thing to offer.
		trouble := client.Trouble()
		if trouble != "" {
			answer.Error = trouble
		}
		// Silence means ssh reached it and no hostd answered — and so does the
		// remote shell saying it has no hostd to run: that is a live machine
		// with nothing installed, which is exactly the case the install button
		// exists for. A fresh VPS was read as "not answering" before this,
		// because its shell's complaint counted as trouble (crg, 2026-08-30).
		noBinary := strings.Contains(trouble, "command not found") ||
			strings.Contains(trouble, "not found: hostd")
		answer.NoDaemon = (trouble == "" || noBinary) && errors.Is(err, api.ErrNoAnswer)
		p.drop(host)
		return answer
	}
	// Asked only when this window does not know it yet: a version changes when
	// the daemon is replaced, and replacing it drops the connection.
	answer.Version, answer.ClockSkewMS, answer.Zone, answer.ZoneOffsetMS = p.knownClock(host)
	if answer.Version == "" {
		var described api.Description
		describeErr := ask(ctx, client, api.Request{Op: api.OpDescribe}, &described)
		if describeErr == nil {
			answer.Version = described.Version
			if described.TimeMS > 0 {
				answer.ClockSkewMS = described.TimeMS - float64(time.Now().UnixMilli())
				answer.Zone = described.Zone
				answer.ZoneOffsetMS = described.ZoneOffsetMS
			}
		}
	}

	err = ask(ctx, client, api.Request{
		Op:     api.OpMetrics,
		FromMS: fromMS,
		ToMS:   toMS,
		Limit:  2000,
	}, &answer.Metrics)
	if err != nil {
		answer.Error = err.Error()
		return answer
	}

	err = ask(ctx, client, api.Request{Op: api.OpLogSearch, Since: since, Limit: 300}, &answer.Lines)
	if err != nil {
		answer.Error = err.Error()
		return answer
	}
	for _, line := range answer.Lines {
		if line.Seq > answer.Since {
			answer.Since = line.Seq
		}
	}
	if wantImages {
		err = ask(ctx, client, api.Request{Op: api.OpImageList}, &answer.Images)
		if err != nil {
			// Kept apart from Error: a daemon too old to know the operation, or
			// a machine with no runtime, must not blank the services and charts
			// it answered perfectly well.
			answer.ImagesError = err.Error()
		}
	}
	return answer
}

// One connection per machine, kept: opening an ssh for every round would spend
// a handshake a second on a window that is only watching.
func (p *panel) client(host string) (*api.Client, error) {
	p.mu.Lock()
	existing, ok := p.clients[host]
	p.mu.Unlock()
	if ok {
		return existing, nil
	}
	// Dialed on the panel's own lifetime, never a round's: the round asks and
	// leaves, the connection stays.
	opened, err := p.dial(host)
	if err != nil {
		return nil, err
	}
	if p.opt.debug {
		// The terminal the panel was started from becomes its flight recorder:
		// one line per request, with the machine and how long it took. When
		// the window misbehaves, this is what says which machine, which
		// operation, and whether the answer ever came.
		opened.Debug = os.Stderr
	}
	p.mu.Lock()
	p.clients[host] = opened
	p.mu.Unlock()
	return opened, nil
}

// reach opens a connection of an action's own, and the caller closes it. Two
// reasons, and the second one cost a deploy on the bench:
//
// Its own, because one request owns a pipe from first byte to last: an action
// that takes seconds on the rounds' pipe would starve the telemetry — a frozen
// screen exactly while somebody waits to see what their click did.
//
// And FRESH, never kept: a pipe is `hostd -stdio` on the far side, and that
// process dies with every daemon restart, ssh hiccup and idle timeout. The
// rounds keep theirs warm and redial two seconds later, so nothing shows. An
// action's would sit unused for an hour and then be dead exactly when it was
// needed — which is what "the image arrived and the client got EOF" was.
func (p *panel) reach(host string) (*api.Client, error) {
	opened, err := p.dial(host)
	if err != nil {
		return nil, err
	}
	if p.opt.debug {
		opened.Debug = os.Stderr
	}
	return opened, nil
}

// A machine that stopped answering is let go of, not retried in a loop: the
// next round opens a new connection, and if it fails again the panel says so
// where the operator can see which machine went away.
func (p *panel) drop(host string) {
	p.mu.Lock()
	client, ok := p.clients[host]
	delete(p.clients, host)
	p.mu.Unlock()
	if ok {
		_ = client.Close()
	}
}

func (p *panel) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for host, client := range p.clients {
		_ = client.Close()
		delete(p.clients, host)
	}
}

/* What the window asks of the view. */

// pick is ["fleet"] or ["host", name] or ["service", host, name].
func (p *panel) pick(rest []string) {
	p.viewMu.Lock()
	p.view.kind = rest[0]
	p.view.host = ""
	p.view.name = ""
	p.view.path = ""
	if len(rest) > 1 {
		p.view.host = rest[1]
	}
	if len(rest) > 2 {
		p.view.name = rest[2]
	}
	if len(rest) > 3 {
		// The rest is a path inside the service's data, slashes and all.
		p.view.path = strings.Join(rest[3:], "/")
	}
	if p.view.host != "" {
		// Asking about a machine is asking what is on it. Leaving it shut
		// would answer with the name of the machine the operator just named.
		p.view.closed[p.view.host] = false
	}
	kind, host, name, path := p.view.kind, p.view.host, p.view.name, p.view.path
	p.viewMu.Unlock()
	if kind == "images" {
		// A machine's images are only fetched while their screen is open, so
		// opening it has to ask rather than sit empty until the next tick.
		p.wake()
	}
	if kind == "services" {
		// Read on the way in, and not again: see panel.catalog.
		p.loadCatalog()
	}
	if kind == "files" {
		// Same rule as the catalog: read on the way in and on navigation,
		// never on the poll — a listing repainting under the pointer is how a
		// click lands on the wrong file.
		p.loadFiles(host, name, path)
	}
}

func (p *panel) toggle(host string) {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()
	p.view.closed[host] = !p.view.closed[host]
}

// A service's own fold, closed by default: its one child is the Files row.
func (p *panel) toggleService(host, name string) {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()
	key := host + "/" + name
	p.view.opened[key] = !p.view.opened[key]
}

func (p *panel) pickWindow(value string) {
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return
	}
	p.viewMu.Lock()
	p.view.window = seconds
	p.view.from = 0
	p.view.to = 0
	p.viewMu.Unlock()
	p.wake()
}

// A zero range is the way back to live: what the operator dragged stops being
// what the charts are about.
func (p *panel) pickRange(from, to float64) {
	p.viewMu.Lock()
	p.view.from = from
	p.view.to = to
	p.viewMu.Unlock()
	p.wake()
}

// ask is one request and its answer, decoded: the panel calls the same
// operations the command line does, so a number on the screen and a number in
// the terminal cannot disagree.
func ask(ctx context.Context, client *api.Client, req api.Request, out any) error {
	resp, err := client.Do(ctx, req)
	if err != nil {
		return err
	}
	if resp.Failed() {
		return resp.Err()
	}
	if out == nil || resp.Body == "" {
		return nil
	}
	return decode(ctx, resp.Body, out)
}

func runGUI(ctx context.Context, opt options, args []string) (int, error) {
	if len(args) > 0 {
		return exitUsage, fmt.Errorf("gui takes no arguments; choose the machines with -host, -hosts, -all or -tag")
	}
	hosts, err := opt.selection(ctx)
	if err != nil {
		return exitUsage, err
	}
	if len(hosts) == 0 {
		// A panel with no machine named is a panel about the fleet: watching
		// is what it is for, and the inventory is what the fleet is.
		hosts, err = opt.fleet(ctx)
		if err != nil {
			// This is what somebody sees on their first run, so it says how to
			// get somewhere rather than only what is missing.
			return exitUsage, fmt.Errorf("%w\nlist your machines there, or run hostctl -h to see what else it does", err)
		}
	}

	view, err := newPanel(opt, hosts)
	if err != nil {
		return exitFailed, err
	}
	defer view.close()

	// The fleet is asked off the UI thread, for good: the scheme handler that
	// serves the window runs ON that thread, so a round trip over ssh inside it
	// would freeze the window for as long as the slowest machine takes.
	polling, stop := context.WithCancel(ctx)
	defer stop()
	view.start(polling)

	window, err := glaze.NewWithOptions(glaze.Options{
		SchemeHandlers: map[string]glaze.SchemeHandler{"app": serve(view.routes(), opt.debug)},
		// A dashboard is clicked in passing, so a click on an inactive window
		// must reach the page instead of being spent activating it.
		AcceptsFirstMouse: true,
	})
	if err != nil {
		return exitFailed, err
	}
	defer window.Destroy()

	view.emit = window.Eval
	view.dialogs = window
	// The bridge the clicks ride: window.hostd_act in the page is Act here.
	_, err = glaze.BindMethods(window, "hostd", view)
	if err != nil {
		return exitFailed, err
	}
	window.SetTitle("hostd")
	window.SetSize(1100, 760, glaze.HintNone)
	// The floor is a phone screen: below it the layout has nothing left to
	// give up, and a window dragged to a sliver reads as the app vanishing.
	window.SetSize(390, 700, glaze.HintMin)
	// A machine with no menu bar is not a reason to refuse to watch the fleet;
	// it only means the keyboard shortcuts belong to the platform.
	bar, err := install(window)
	if err != nil && !errors.Is(err, menu.ErrUnsupported) {
		return exitFailed, err
	}
	if bar != nil {
		defer bar.Release()
	}
	window.Navigate(uiOrigin)
	window.Run()
	return exitOK, nil
}

// serve turns an ordinary handler into a scheme handler. Nothing listens
// anywhere: the request never leaves the process, which is what keeps the
// machine of the person who is only looking at graphs free of open doors.
func serve(handler http.Handler, debug bool) glaze.SchemeHandler {
	return func(req *glaze.SchemeRequest) *glaze.SchemeResponse {
		if debug {
			fmt.Fprintf(os.Stderr, "debug scheme url=%q\n", req.URL)
		}
		asked, err := http.NewRequest(http.MethodGet, strings.TrimPrefix(req.URL, "app://hostd"), nil)
		if err != nil {
			return nil
		}
		var wrote answered
		handler.ServeHTTP(&wrote, asked)
		if wrote.code == http.StatusNotFound {
			return nil
		}
		return &glaze.SchemeResponse{Body: wrote.body.Bytes(), MIMEType: wrote.Header().Get("Content-Type")}
	}
}

// The smallest thing an http.Handler can write into. The alternative is
// net/http/httptest, which belongs to tests and would ship in the binary.
type answered struct {
	head http.Header
	body bytes.Buffer
	code int
}

func (a *answered) Header() http.Header {
	if a.head == nil {
		a.head = http.Header{}
	}
	return a.head
}

func (a *answered) Write(p []byte) (int, error) {
	if a.code == 0 {
		a.code = http.StatusOK
	}
	return a.body.Write(p)
}

func (a *answered) WriteHeader(code int) {
	if a.code == 0 {
		a.code = code
	}
}

// What the files screen is showing: one directory of one service's data,
// fetched on the way in and on navigation, never on the poll.
type filesState struct {
	Host    string
	Service string
	Path    string
	Entries []api.FileEntry
	Problem string
	Busy    bool
}

func (p *panel) filesNow() filesState {
	p.filesMu.Lock()
	defer p.filesMu.Unlock()
	return p.files
}

// loadFiles asks the machine for one directory listing, on a connection of the
// action's own. The screen shows "asking…" until the answer lands.
func (p *panel) loadFiles(host, svc, path string) {
	p.filesMu.Lock()
	p.files = filesState{Host: host, Service: svc, Path: path, Busy: true}
	p.filesMu.Unlock()
	go func() {
		entries, problem := p.fetchFiles(host, svc, path)
		p.filesMu.Lock()
		// A stale answer must not overwrite a newer navigation.
		if p.files.Host == host && p.files.Service == svc && p.files.Path == path {
			p.files.Entries = entries
			p.files.Problem = problem
			p.files.Busy = false
		}
		p.filesMu.Unlock()
		p.push()
	}()
}

func (p *panel) fetchFiles(host, svc, path string) ([]api.FileEntry, string) {
	client, err := p.reach(host)
	if err != nil {
		return nil, err.Error()
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, api.Request{Op: api.OpFileList, Service: svc, Name: path})
	if err != nil {
		return nil, err.Error()
	}
	if resp.Failed() {
		return nil, resp.Message
	}
	var entries []api.FileEntry
	err = decode(ctx, resp.Body, &entries)
	if err != nil {
		return nil, err.Error()
	}
	return entries, ""
}
