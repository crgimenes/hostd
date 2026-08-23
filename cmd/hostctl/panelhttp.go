package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/crgimenes/hostd/supervisor"
)

// What the operator is looking at. It lives here rather than in the page
// because every fragment is rendered from it: the window holds no state of its
// own beyond what it was last sent.
type viewState struct {
	kind   string
	host   string
	name   string
	window int
	from   float64
	to     float64
	closed map[string]bool
}

// A copy nobody else can move under the reader: the struct copies by value but
// the map of shut machines would not, and a tree being rendered while a click
// writes into that map is a race the reader loses silently.
func (p *panel) viewport() viewState {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()
	out := p.view
	out.closed = make(map[string]bool, len(p.view.closed))
	maps.Copy(out.closed, p.view.closed)
	return out
}

// routes serves the page and its assets, and mirrors Act over HTTP so the
// panel can be tested and looked at without a window. In the window itself an
// action travels over the glaze binding, never over a request: the page and
// its assets are the only things the scheme handler is for.
func (p *panel) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", p.page)
	mux.HandleFunc("GET /asset/{name}", p.asset)
	mux.HandleFunc("GET /act/{action...}", func(w http.ResponseWriter, r *http.Request) {
		body, err := p.Act(r.PathValue("action"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
	return mux
}

// Act is one operator action, named as a path: "select/host/yuki",
// "toggle/yuki", "window/3600", "range/live", "range/<from>/<to>", "refresh".
// It is bound into the page by glaze, so a click reaches the panel over the
// same kind of channel the panel's pushes ride — no request, no scheme,
// nothing that can lose its parameters on the way. The answer is the
// fragments the window does not have yet, and the page applies them exactly
// as it applies a push.
func (p *panel) Act(action string) (string, error) {
	if p.opt.debug {
		fmt.Fprintf(os.Stderr, "debug act action=%q\n", action)
	}
	parts := strings.Split(action, "/")
	rest := parts[1:]
	before := p.viewport()
	switch {
	case parts[0] == "select" && len(rest) > 0:
		p.pick(rest)
	case parts[0] == "toggle" && len(rest) == 1:
		p.toggle(rest[0])
	case parts[0] == "window" && len(rest) == 1:
		p.pickWindow(rest[0])
	case parts[0] == "range" && len(rest) == 1 && rest[0] == "live":
		p.pickRange(0, 0)
	case parts[0] == "range" && len(rest) == 2:
		from, _ := strconv.ParseFloat(rest[0], 64)
		to, _ := strconv.ParseFloat(rest[1], 64)
		p.pickRange(from, to)
	case parts[0] == "refresh":
		p.wake()
	default:
		return "", fmt.Errorf("no such action: %q", action)
	}
	after := p.viewport()
	// Selecting changes what the log is ABOUT, so the lines on screen are
	// answers to another question and the pane is rewritten rather than
	// added to.
	moved := before.kind != after.kind || before.host != after.host || before.name != after.name
	return p.pending(moved)
}

func (p *panel) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	content, err := ui.ReadFile("ui/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	kind := "text/plain; charset=utf-8"
	switch {
	case strings.HasSuffix(name, ".css"):
		kind = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		kind = "text/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", kind)
	// #nosec G705 -- the only bytes that can be written here are this program's
	// own embedded assets: the path is one URL segment, so it cannot contain a
	// separator, and an embedded filesystem has nothing else in it
	_, _ = w.Write(content)
}

// page is the whole window, once. Everything after it is a fragment.
func (p *panel) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	p.sentMu.Lock()
	defer p.sentMu.Unlock()
	// A window that is being drawn from scratch holds nothing yet.
	clear(p.sent)

	pieces := p.fragments()
	shell := map[string]any{}
	for _, piece := range pieces {
		shell[piece.ID] = piece.HTML
		p.sent[piece.ID] = piece.Digest
	}
	p.pushed = 0
	shell["lines"] = p.freshLines(true)
	var body bytes.Buffer
	err := p.pages.ExecuteTemplate(&body, "page", shell)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body.Bytes())
}

// pending is everything the window does not know yet, rendered. It is one
// critical section from render to bookkeeping, so two callers — the poller's
// push and a click's answer — cannot hand the window each other's past. An
// unchanged fleet comes back empty, and empty means the window is not touched.
func (p *panel) pending(replaceLog bool) (string, error) {
	p.sentMu.Lock()
	defer p.sentMu.Unlock()

	var body bytes.Buffer
	for _, piece := range p.fragments() {
		if p.sent[piece.ID] == piece.Digest {
			continue
		}
		p.sent[piece.ID] = piece.Digest
		body.WriteString(string(piece.HTML))
	}

	name := "newLines"
	if replaceLog {
		name = "allLines"
	}
	fresh := p.freshLines(replaceLog)
	if len(fresh) > 0 || replaceLog {
		err := p.pages.ExecuteTemplate(&body, name, fresh)
		if err != nil {
			return "", err
		}
	}
	return body.String(), nil
}

// push sends what changed into the page. Nothing changed, nothing sent: the
// wire is silent when the fleet is, which is what makes the window still.
func (p *panel) push() {
	if p.emit == nil {
		return
	}
	body, err := p.pending(false)
	if err != nil || body == "" {
		return
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return
	}
	p.emit("hostdApply(" + string(encoded) + ")")
}

// One swappable piece of the window: what it is called, what it says now, and
// a digest of what it says, so "did this change" is a string comparison.
type fragment struct {
	ID     string
	HTML   template.HTML
	Digest string
}

// fragments is every piece the window might need, in the order they must land:
// shells first, values second, so a value always finds its element.
//
// A shell (the tree, the detail pane) is replaced only when its STRUCTURE
// changes — a click, a machine appearing — never because a number moved: its
// digest is computed over the structure alone. The numbers that move (a state
// dot, a count, a chart) travel as their own tiny fragments, so a run starting
// somewhere repaints one dot instead of the tree somebody is about to click.
func (p *panel) fragments() []fragment {
	snap := p.latest()
	view := p.viewport()
	tree := treeOf(snap, view)
	detail := detailOf(snap, view)

	out := []fragment{
		p.renderKeyed("tree", "tree", tree, tree.StructureKey()),
		p.renderKeyed("detail", "detail", detail, detail.StructureKey()),
	}
	for _, row := range tree.Rows {
		if row.Dot != "" {
			out = append(out, p.render(row.DotID(), "treeDot", row))
		}
		if row.Meta != "" {
			out = append(out, p.render(row.MetaID(), "treeMeta", row))
		}
	}
	out = append(out, detail.volatiles(p)...)
	out = append(out, p.render("status", "status", statusOf(snap)))
	return out
}

// renderKeyed digests the given key instead of the rendered bytes: what the
// key leaves out cannot cause a swap.
func (p *panel) renderKeyed(id, name string, data any, key string) fragment {
	piece := p.render(id, name, data)
	sum := sha256.Sum256([]byte(key))
	piece.Digest = hex.EncodeToString(sum[:8])
	return piece
}

func (p *panel) render(id, name string, data any) fragment {
	var out bytes.Buffer
	err := p.pages.ExecuteTemplate(&out, name, data)
	if err != nil {
		// A fragment that cannot be rendered is said out loud rather than
		// quietly left out: a panel missing a piece looks like a fleet missing
		// a machine.
		out.Reset()
		fmt.Fprintf(&out, `<div id=%q hx-swap-oob="true" class="problem">%s</div>`, id, template.HTMLEscapeString(err.Error()))
	}
	sum := sha256.Sum256(out.Bytes())
	// #nosec G203 -- out is what html/template just produced, so everything a
	// machine sent (a service name, a line of log) is already escaped; the
	// conversion only says "do not escape this a second time"
	return fragment{ID: id, HTML: template.HTML(out.String()), Digest: hex.EncodeToString(sum[:8])}
}

// freshLines advances the window's log. The cursor is the panel's own: the
// window never reports what it holds, because the sender knows what it sent.
func (p *panel) freshLines(all bool) []lineView {
	after := p.pushed
	if all {
		after = 0
	}
	snap := p.latest()
	view := p.viewport()
	out := make([]lineView, 0, 64)
	for _, held := range snap.Lines {
		if held.N > p.pushed {
			p.pushed = held.N
		}
		if !all && held.N <= after {
			continue
		}
		if !inView(held, view) {
			continue
		}
		out = append(out, lineOf(held, view))
	}
	return out
}

/* What the page is told. */

type rowView struct {
	Key      string
	Kind     string
	Label    string
	Dot      string
	Meta     string
	Child    bool
	Selected bool
	Twist    string
	Link     string
	Toggle   string
}

// Element ids are derived from the row key, which may hold anything a machine
// or a service is called; an id must not.
func (r rowView) DotID() string  { return "tdot-" + safeID(r.Key) }
func (r rowView) MetaID() string { return "tmeta-" + safeID(r.Key) }

func safeID(key string) string {
	out := make([]byte, 0, len(key))
	for _, b := range []byte(key) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
			out = append(out, b)
			continue
		}
		out = append(out, '-')
	}
	return string(out)
}

type treeView struct {
	Rows []rowView
}

// What has to be true for the tree ELEMENT to need replacing. The dots and the
// counts are deliberately absent: they repaint through their own fragments.
func (t treeView) StructureKey() string {
	var key strings.Builder
	for _, row := range t.Rows {
		fmt.Fprintf(&key, "%s|%s|%v|%v|%s\n", row.Key, row.Label, row.Selected, row.Child, row.Twist)
	}
	return key.String()
}

func treeOf(snap snapshot, view viewState) treeView {
	out := treeView{Rows: []rowView{{
		Key:      "fleet",
		Label:    "Fleet",
		Meta:     strconv.Itoa(len(snap.Fleet)),
		Selected: view.kind == "fleet",
		Link:     "select/fleet",
	}}}
	for _, host := range snap.Fleet {
		services := host.Services
		open := !view.closed[host.Host]
		row := rowView{
			Key:      "host:" + host.Host,
			Label:    host.Host,
			Dot:      worst(host),
			Meta:     fmt.Sprintf("%d/%d", running(services), len(services)),
			Selected: view.kind == "host" && view.host == host.Host,
			Link:     "select/host/" + host.Host,
		}
		if host.Error != "" {
			row.Meta = "!"
		}
		if len(services) > 0 {
			row.Twist = "closed"
			if open {
				row.Twist = "open"
			}
			row.Toggle = "toggle/" + host.Host
		}
		out.Rows = append(out.Rows, row)
		if !open {
			continue
		}
		for _, service := range services {
			out.Rows = append(out.Rows, rowView{
				Key:   "svc:" + host.Host + "/" + service.Name,
				Label: service.Name,
				Dot:   service.State,
				Meta:  service.Every,
				Child: true,
				Selected: view.kind == "service" && view.host == host.Host &&
					view.name == service.Name,
				Link: "select/service/" + host.Host + "/" + service.Name,
			})
		}
	}
	return out
}

func running(services []supervisor.Status) int {
	count := 0
	for _, service := range services {
		if service.State == supervisor.StateRunning {
			count++
		}
	}
	return count
}

// The worst thing happening on a machine is what its one dot has to say.
func worst(host fleetHost) string {
	if host.Error != "" {
		return "unreachable"
	}
	order := []string{supervisor.StateFailed, supervisor.StateStarting, supervisor.StateStopped,
		supervisor.StateScheduled, supervisor.StateRunning}
	found := len(order) - 1
	for _, service := range host.Services {
		for at, state := range order {
			if state == service.State && at < found {
				found = at
			}
		}
	}
	if len(host.Services) == 0 {
		return supervisor.StateStopped
	}
	return order[found]
}

func itoa(value uint64) string { return strconv.FormatUint(value, 10) }

func (l line) Cursor() uint64 { return l.N }
