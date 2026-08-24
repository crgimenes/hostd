package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
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

// What the panel is: read only. A mutating action is shown as the hostctl
// command that would do it, ready to copy — the panel does not act, it says
// what to run.
type panel struct {
	opt   options
	hosts []string
	// The lifetime of the panel's ssh connections. DialSSH hands the context
	// to exec.CommandContext, which KILLS the process when it ends — so a
	// connection dialed with a round's context died with the round, and every
	// other refresh found four broken pipes where its fleet used to be.
	life context.Context

	// The machines and their connections share one lock: letting go of a
	// machine and closing what was open for it is one decision.
	mu      sync.Mutex
	clients map[string]*api.Client

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
	}

	snapMu sync.RWMutex
	snap   snapshot
	since  map[string]uint64
	held   map[string]bool
	cursor uint64

	viewMu sync.Mutex
	view   viewState

	// Rings the poller: the one goroutine allowed to talk to the fleet.
	nudge chan struct{}

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
		Funcs(template.FuncMap{"icon": iconSVG}).
		ParseFS(ui, "ui/panel.tmpl")
	if err != nil {
		return nil, err
	}
	return &panel{
		opt:     opt,
		hosts:   hosts,
		cfgDir:  opt.config,
		clients: map[string]*api.Client{},
		since:   map[string]uint64{},
		held:    map[string]bool{},
		sent:    map[string]string{},
		nudge:   make(chan struct{}, 1),
		view:    viewState{kind: "fleet", window: 3600, closed: map[string]bool{}},
		pages:   pages,
	}, nil
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
	Since    uint64              `json:"since"`
}

// Fleet asks every machine at once. The window in seconds is how far back the
// graphs reach, unless the operator dragged a range on a chart, which arrives
// as fromMS and toMS and is used instead. since is the last log sequence the
// panel already has, so a round carries only what is new.
// Fleet asks every machine at once and hands each answer over the moment it
// arrives. It does NOT wait for the slowest: a machine that is switched off
// takes as long as ssh takes to give up, and waiting for it would hide every
// machine that answered — which is the whole fleet gone from the window
// because one of them is unplugged.
func (p *panel) Fleet(window int, fromMS, toMS float64, since map[string]uint64, arrived func(fleetHost)) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if fromMS <= 0 {
		fromMS = float64(time.Now().Add(-time.Duration(window) * time.Second).UnixMilli())
		toMS = 0
	}

	var wg sync.WaitGroup
	for _, host := range p.hostsNow() {
		wg.Go(func() {
			arrived(p.one(ctx, host, fromMS, toMS, since[host]))
		})
	}
	wg.Wait()
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
	p.wake()
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

func (p *panel) one(ctx context.Context, host string, fromMS, toMS float64, since uint64) fleetHost {
	answer := fleetHost{Host: host, Since: since}
	p.reaching(host, true)
	defer p.reaching(host, false)
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
		p.drop(host)
		return answer
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
	opened, err := api.DialSSH(p.life, host, strings.Fields(p.opt.remote))
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
	defer p.viewMu.Unlock()
	p.view.kind = rest[0]
	p.view.host = ""
	p.view.name = ""
	if len(rest) > 1 {
		p.view.host = rest[1]
	}
	if len(rest) > 2 {
		p.view.name = rest[2]
	}
	if p.view.host != "" {
		// Asking about a machine is asking what is on it. Leaving it shut
		// would answer with the name of the machine the operator just named.
		p.view.closed[p.view.host] = false
	}
}

func (p *panel) toggle(host string) {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()
	p.view.closed[host] = !p.view.closed[host]
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
	view.life = polling
	go view.poll(polling)

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
