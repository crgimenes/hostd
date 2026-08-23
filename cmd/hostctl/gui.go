package main

import (
	"context"
	"embed"
	"fmt"
	"mime"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/crgimenes/glaze"
	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/metrics"
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

	mu      sync.Mutex
	clients map[string]*api.Client
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

// Fleet is called by the page on a timer. The window in seconds is how far back
// the graphs reach, unless the operator dragged a range on a chart, which
// arrives as fromMS and toMS and is used instead. since is the last log
// sequence the page already has, so a refresh carries only what is new.
func (p *panel) Fleet(window int, fromMS, toMS float64, since map[string]uint64) []fleetHost {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if fromMS <= 0 {
		fromMS = float64(time.Now().Add(-time.Duration(window) * time.Second).UnixMilli())
		toMS = 0
	}

	out := make([]fleetHost, len(p.hosts))
	var wg sync.WaitGroup
	for i, host := range p.hosts {
		wg.Go(func() {
			out[i] = p.one(ctx, host, fromMS, toMS, since[host])
		})
	}
	wg.Wait()
	return out
}

func (p *panel) one(ctx context.Context, host string, fromMS, toMS float64, since uint64) fleetHost {
	answer := fleetHost{Host: host, Since: since}
	client, err := p.client(ctx, host)
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

// One connection per machine, kept: opening an ssh for every refresh would
// spend a handshake a second on a window that is only watching.
func (p *panel) client(ctx context.Context, host string) (*api.Client, error) {
	p.mu.Lock()
	existing, ok := p.clients[host]
	p.mu.Unlock()
	if ok {
		return existing, nil
	}
	opened, err := api.DialSSH(ctx, host, strings.Fields(p.opt.remote))
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.clients[host] = opened
	p.mu.Unlock()
	return opened, nil
}

// A machine that stopped answering is let go of, not retried in a loop: the
// next refresh opens a new connection, and if it fails again the panel says so
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

	view := &panel{opt: opt, hosts: hosts, clients: map[string]*api.Client{}}
	defer view.close()

	window, err := glaze.NewWithOptions(glaze.Options{
		SchemeHandlers: map[string]glaze.SchemeHandler{"app": serveUI},
		// A dashboard is clicked in passing, so a click on an inactive window
		// must reach the page instead of being spent activating it.
		AcceptsFirstMouse: true,
	})
	if err != nil {
		return exitFailed, err
	}
	defer window.Destroy()

	window.SetTitle("hostd")
	window.SetSize(1100, 760, glaze.HintNone)
	_, err = glaze.BindMethods(window, "hostd", view)
	if err != nil {
		return exitFailed, err
	}
	window.Navigate(uiOrigin + "index.html")
	window.Run()
	return exitOK, nil
}

// serveUI answers from the embedded files and nothing else: there is no path
// on the operator's machine this can reach, because there is no filesystem
// behind it.
func serveUI(req *glaze.SchemeRequest) *glaze.SchemeResponse {
	parsed, err := url.Parse(req.URL)
	if err != nil {
		return nil
	}
	name := path.Clean("/" + parsed.Path)
	if name == "/" {
		name = "/index.html"
	}
	content, err := ui.ReadFile("ui" + name)
	if err != nil {
		return nil
	}
	kind := mime.TypeByExtension(path.Ext(name))
	if kind == "" {
		kind = "application/octet-stream"
	}
	return &glaze.SchemeResponse{Body: content, MIMEType: kind}
}
