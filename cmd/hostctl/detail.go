package main

import (
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/supervisor"
	"github.com/crgimenes/hostd/version"
)

type detailView struct {
	Title    string
	Subtitle string
	Single   bool
	Empty    string
	Cards    []cardView
	// Whether this view is about the fleet. The window buttons and the log
	// belong to the machines; a page about the panel itself has no use for
	// either, and showing them would offer a control that governs nothing on
	// screen.
	Watching bool
	// What the charts are drawn against, so a drag across one can say which
	// instants the pointer covered.
	From      int64
	To        int64
	Window    int
	Frozen    bool
	RangeText string
	// What is wrong with the machine itself, beside its name, with the button
	// that fixes it: there is no way to miss it and no way to mistake it for a
	// service's problem.
	Alert     string
	AlertAct  string
	AlertBusy bool
}

type cardView struct {
	Key     string
	Heading string
	Aside   string
	Link    string
	Problem string
	Numbers []numberView
	Head    []string
	Rows    []rowCells
	Facts   [][2]string
	Charts  []chartView
	Buttons []buttonView
	// The machines a catalog card can deploy to, in a dropdown beside its
	// button.
	Machines []string
	// A plain table, for a card whose rows are not services. Head above drives
	// the service table and its fixed columns; these two are the general one.
	GridHead []string
	Grid     []gridRow
}

type gridRow struct {
	Cells []string
	// Nothing on the machine is holding this row's subject. It is the row a
	// cleanup is about, so it is worth seeing without reading the column.
	Loose bool
	// Navigation on the first cell — a directory in a file listing.
	Link string
	// One action at the row's end — download, for a file.
	Buttons []buttonView
}

func (c cardView) AsideID() string { return "aside-" + safeID(c.Key) }
func (c cardView) StatID() string  { return "stat-" + safeID(c.Key) }
func (c cardView) FactsID() string { return "facts-" + safeID(c.Key) }

type numberView struct {
	Value string
	Of    string
}

type rowCells struct {
	Key     string
	Link    string
	Name    string
	State   string
	Dot     string
	Image   string
	Uptime  string
	Since   int64
	Job     bool
	Restart string
	Buttons []buttonView
	Problem string
}

func (r rowCells) PillID() string { return "pill-" + safeID(r.Key) }
func (r rowCells) UpID() string   { return "up-" + safeID(r.Key) }
func (r rowCells) RstID() string  { return "rst-" + safeID(r.Key) }

type buttonView struct {
	Label string
	// The picture beside the word, never instead of it: the panel is also for
	// people who have not learned the pictures yet.
	Icon string
	// The act this button performs. One click, no confirmation: what it is
	// doing shows in the log.
	Act string
	// A catalog button instead names the SERVICE, and takes the machine from
	// the dropdown beside it — the act is built at the click.
	Deploy string
	// Running right now: rendered held down, and a second click is nothing.
	Busy bool
}

type chartView struct {
	ID     string
	Legend []legendView
	Top    string
	Body   template.HTML
	Short  bool
}

type legendView struct {
	Label  string
	Colour string
	Value  string
}

type statusView struct {
	Text string
	Busy bool
}

// What has to be true for the PANE element to need replacing: which view this
// is and what it is made of — never the numbers inside, which repaint through
// their own fragments. A pane replaced under a pointer eats the click.
func (d detailView) StructureKey() string {
	var key strings.Builder
	fmt.Fprintf(&key, "%s|%s|%v|%s|%d|%v|%v|%s|%s|%s\n", d.Title, d.Subtitle, d.Single, d.Empty,
		d.Window, d.Watching, d.Frozen, d.RangeText, d.Alert, d.AlertAct+onOff(d.AlertBusy))
	for _, card := range d.Cards {
		fmt.Fprintf(&key, "%s|%s|%s|%s|%d|%d|%v\n", card.Key, card.Heading, card.Link, card.Problem, len(card.Charts), len(card.Numbers), card.Machines)
		for _, row := range card.Rows {
			fmt.Fprintf(&key, " %s|%s|%s|%s|%v\n", row.Key, row.Name, row.Image, row.Problem, row.Job)
		}
		for _, fact := range card.Facts {
			fmt.Fprintf(&key, " f:%s\n", fact[0])
		}
		// Whole rows, not just their names: an image list changes by rows
		// appearing and going, and nothing inside it repaints on its own.
		for _, row := range card.Grid {
			fmt.Fprintf(&key, " g:%s|%v\n", strings.Join(row.Cells, "\x1f"), row.Loose)
		}
		for _, button := range card.Buttons {
			fmt.Fprintf(&key, " b:%s|%s|%v\n", button.Act, button.Deploy, button.Busy)
		}
	}
	return key.String()
}

// The pieces of the pane that move on their own: dots, counts, charts, facts.
// Each is one element with one id, so a chart getting a new sample repaints
// that chart and nothing an operator is holding.
func (d detailView) volatiles(p *panel) []fragment {
	var out []fragment
	for _, card := range d.Cards {
		if card.Aside != "" {
			out = append(out, p.render(card.AsideID(), "cardAside", card))
		}
		if len(card.Numbers) > 0 {
			out = append(out, p.render(card.StatID(), "statBlock", card))
		}
		if len(card.Facts) > 0 {
			out = append(out, p.render(card.FactsID(), "factsTable", card))
		}
		for _, chart := range card.Charts {
			out = append(out, p.render(chart.ID, "chartHolder", chart))
		}
		for _, row := range card.Rows {
			out = append(out, p.render(row.PillID(), "rowPill", row))
			out = append(out, p.render(row.UpID(), "rowUptime", row))
			out = append(out, p.render(row.RstID(), "rowRestarts", row))
		}
	}
	return out
}

// No clock and no spinner: the machines appear as they answer and the log goes
// by while things happen, which is what tells somebody the program is working.
// A status that stamped the time would change every round, and a fragment that
// changes every round is a wire that is never quiet.
func onOff(on bool) string {
	if on {
		return " (running)"
	}
	return ""
}

func statusOf(snap snapshot) statusView {
	if snap.Updated.IsZero() {
		return statusView{Text: "reaching the fleet"}
	}
	return statusView{Text: fmt.Sprintf("watching %d machine(s)", len(snap.Fleet))}
}

func detailOf(snap snapshot, view viewState, info settingsInfo, catalog []service.Declaration, machines []string, files filesState) detailView {
	var out detailView
	switch view.kind {
	case "host":
		out = hostDetail(snap, view)
	case "service":
		out = serviceDetail(snap, view)
	case "images":
		out = imagesDetail(snap, view)
	case "services":
		out = catalogDetail(catalog, machines, info)
	case "files":
		out = filesDetail(view, files)
	case "settings":
		out = settingsDetail(info)
	default:
		out = fleetDetail(snap, view)
	}
	// The log and the window buttons belong to a view about what the machines
	// are DOING. A page about the panel, or about what a machine is storing,
	// governs neither, and offering a control that moves nothing is worse than
	// not offering it.
	out.Watching = view.kind != "settings"
	out.Window = view.window
	from, to := span(snap, view)
	out.From, out.To = int64(from), int64(to)
	out.Frozen = view.from > 0
	if out.Frozen {
		out.RangeText = time.UnixMilli(int64(view.from)).Format("15:04:05") + " – " +
			time.UnixMilli(int64(view.to)).Format("15:04:05")
	}
	return out
}

// The instants the charts cover, which is the newest sample and the window
// behind it — the same reckoning the charts themselves use.
func span(snap snapshot, view viewState) (from, to float64) {
	if view.from > 0 {
		return view.from, view.to
	}
	for _, host := range snap.Fleet {
		for _, series := range host.Metrics {
			for _, point := range series.Points {
				to = max(to, point.TimeMS)
			}
		}
	}
	if to == 0 {
		return 0, 0
	}
	return to - float64(view.window)*1000, to
}

func fleetDetail(snap snapshot, view viewState) detailView {
	out := detailView{Title: "Fleet", Subtitle: fmt.Sprintf("%d machine(s)", len(snap.Fleet))}
	if len(snap.Fleet) == 0 {
		out.Empty = "no machine is listed in your inventory"
		return out
	}
	for _, host := range snap.Fleet {
		card := cardView{
			Key:     "machine:" + host.Host,
			Heading: host.Host,
			Aside:   fmt.Sprintf("%d of %d running", running(host.Services), len(host.Services)),
			Link:    "select/host/" + host.Host,
		}
		if host.Error != "" {
			card.Aside = "not answering"
			card.Problem = host.Error
			out.Cards = append(out.Cards, card)
			continue
		}
		card.Numbers = numbersOf(host)
		overview := chartOf(hostLayers(host), view, percentOf, true)
		overview.ID = "plot-" + safeID(host.Host)
		card.Charts = []chartView{overview}
		out.Cards = append(out.Cards, card)
	}
	return out
}

// The image screen for one machine: what it is holding, and how much of it
// nothing is holding on to. Read-only like the rest of the panel — removing an
// image is destructive and belongs to a command with a plan in front of it.
func imagesDetail(snap snapshot, view viewState) detailView {
	host, found := machineOf(snap, view.host)
	if !found {
		return detailView{Title: view.host, Single: true, Empty: "this machine is not in the fleet any more"}
	}
	out := detailView{Title: host.Host, Subtitle: "images", Single: true}
	switch {
	case host.ImagesError != "":
		out.Empty = host.ImagesError
		return out
	case len(host.Images) > 0:
	case host.Error != "":
		out.Empty = host.Error
		return out
	default:
		// Nothing has come back yet. Saying the machine holds no images would
		// be a different, and wrong, statement.
		out.Empty = "asking " + host.Host + " what it holds…"
		return out
	}

	ours, others := splitManaged(host.Images)
	out.Subtitle = fmt.Sprintf("%d of %d image(s) put here by hostd", len(ours), len(host.Images))

	managed := cardView{
		Key:      "images",
		Heading:  "Managed by hostd",
		Aside:    fmt.Sprintf("a cleanup keeps %d of each still declared", api.DefaultImageKeep),
		GridHead: []string{"image", "size", "created", "used by", "digest"},
		Numbers:  imageNumbers(ours, true),
		Grid:     imageRows(ours),
		// One click, and the log says which images went. The policy is the
		// daemon's: only what hostd put here, never what a container or a
		// declaration is holding.
		Buttons: []buttonView{{
			Label: "Clean up",
			Icon:  "trash",
			Act:   "do/prune/" + host.Host,
		}},
	}
	if len(ours) == 0 {
		managed.Problem = "nothing here arrived through hostctl image push"
	}
	out.Cards = append(out.Cards, managed)

	if len(others) == 0 {
		return out
	}
	// Reported, never accounted for: another system building images on this
	// machine is not this one's business, and offering to remove them would be
	// hostd claiming a machine it only has a corner of.
	out.Cards = append(out.Cards, cardView{
		Key:      "images-other",
		Heading:  "Built or pulled by something else",
		GridHead: []string{"image", "size", "created", "used by", "digest"},
		Numbers:  imageNumbers(others, false),
		Grid:     imageRows(others),
	})
	return out
}

func splitManaged(all []api.ImageEntry) (ours, others []api.ImageEntry) {
	for _, image := range all {
		if image.Managed {
			ours = append(ours, image)
			continue
		}
		others = append(others, image)
	}
	return ours, others
}

// countUnused only for what hostd put here: an image nothing holds is a
// candidate for removal, and only ours are ever candidates.
func imageNumbers(images []api.ImageEntry, countUnused bool) []numberView {
	var total, free float64
	loose := 0
	for _, image := range images {
		total += image.Bytes
		if image.UsedBy == "" {
			loose++
			free += image.Bytes
		}
	}
	out := []numberView{
		{Value: strconv.Itoa(len(images)), Of: "images"},
		{Value: formatBytes(total), Of: "on disk"},
	}
	if countUnused && loose > 0 {
		out = append(out,
			numberView{Value: strconv.Itoa(loose), Of: "held by nothing"},
			numberView{Value: formatBytes(free), Of: "reclaimable"})
	}
	return out
}

func imageRows(images []api.ImageEntry) []gridRow {
	out := make([]gridRow, 0, len(images))
	for _, image := range images {
		used := image.UsedBy
		if used == "" {
			used = "-"
		}
		out = append(out, gridRow{
			Loose: image.UsedBy == "",
			Cells: []string{
				tagsText(image.Tags),
				formatBytes(image.Bytes),
				time.UnixMilli(int64(image.Created)).Format("2006-01-02 15:04"),
				used,
				shortDigest(image.Digest),
			},
		})
	}
	return out
}

// catalogDetail is the OTHER inventory: everything the tree describes, one
// small card each. Where a service runs is not written in any file — it is
// this choice, made here: pick a machine in the dropdown and deploy. The card
// deliberately does not say where the service already runs; the machine
// inventory on the left answers that, and repeating it here would repaint the
// card — dropping the choice in its dropdown — every time a service moved
// anywhere in the fleet.
func catalogDetail(catalog []service.Declaration, machines []string, info settingsInfo) detailView {
	out := detailView{
		Title:    "Services",
		Subtitle: fmt.Sprintf("%d described in %s", len(catalog), info.ConfigDir),
	}
	if len(catalog) == 0 {
		out.Empty = "this tree describes no services yet: a service is a .filo file, or a directory with an init.filo and the files that travel with it"
		return out
	}
	for _, declaration := range catalog {
		name := declaration.Service.Name
		aside := shortImage(declaration.Service.Image)
		if declaration.Service.Every != "" {
			aside += " · every " + declaration.Service.Every
		}
		out.Cards = append(out.Cards, cardView{
			Key:      "catalog:" + name,
			Heading:  name,
			Aside:    aside,
			Machines: machines,
			Buttons: []buttonView{{
				Label:  "deploy",
				Icon:   "box-arrow-up",
				Deploy: name,
			}},
		})
	}
	return out
}

func hostDetail(snap snapshot, view viewState) detailView {
	host, found := machineOf(snap, view.host)
	if !found {
		return detailView{Title: view.host, Single: true, Empty: "this machine is not in the fleet any more"}
	}
	out := detailView{
		Title:    host.Host,
		Subtitle: fmt.Sprintf("%d service(s) declared%s", len(host.Services), machineClock(host)),
		Single:   true,
	}
	if host.Error != "" {
		// The round failed; the machine did not stop existing. What was known
		// stays on screen with the failure beside it — replacing the table
		// with the error would rebuild the pane at every hiccup, and a pane
		// rebuilt under a pointer eats the click.
		out.Subtitle = host.Error
	}
	warning := alertFor(host)
	out.Alert, out.AlertAct = warning.Text, warning.Act
	if host.Error != "" && len(host.Services) == 0 {
		out.Empty = host.Error
		return out
	}

	services := cardView{
		Key:     "services",
		Heading: "Services",
		Head:    []string{"service", "state", "image", "uptime", "restarts"},
	}
	for _, service := range host.Services {
		services.Rows = append(services.Rows, cellsOf(host.Host, service))
	}
	out.Cards = append(out.Cards, services)

	load := chartOf(hostLayers(host), view, percentOf, false)
	load.ID = "plot-load"
	out.Cards = append(out.Cards, cardView{
		Key:     "load",
		Heading: "Load",
		Numbers: numbersOf(host),
		Charts:  []chartView{load},
	})

	stacked := stackedLayers(host)
	if len(stacked) > 0 {
		byService := chartOf(stacked, view, bytesOf, false)
		byService.ID = "plot-stack"
		out.Cards = append(out.Cards, cardView{
			Key:     "by-service",
			Heading: "Memory by service",
			Charts:  []chartView{byService},
		})
	}
	return out
}

// machineClock renders the machine's CURRENT time in the machine's own zone,
// from the skew the connection measured — an instant shown as it was received
// would be a clock that stopped. Minutes, so the header repaints once a minute
// instead of every round. Timezone is the operator's responsibility; this is
// the reminder, and two machines configured differently show it side by side.
func machineClock(host fleetHost) string {
	if host.Zone == "" {
		return ""
	}
	zone := time.FixedZone(host.Zone, int(host.ZoneOffsetMS/1000))
	now := time.Now().Add(time.Duration(host.ClockSkewMS) * time.Millisecond).In(zone)
	return " · " + now.Format("15:04 MST")
}

// filesDetail is one directory of one service's data: the named volumes at the
// top, directories to walk into, files to download, and an upload into the
// directory being looked at. Data only — configuration comes from the tree.
func filesDetail(view viewState, files filesState) detailView {
	out := detailView{
		Title:    view.name,
		Subtitle: "files on " + view.host + " · /" + view.path,
		Single:   true,
	}
	if files.Host != view.host || files.Service != view.name || files.Path != view.path {
		out.Empty = "asking " + view.host + "…"
		return out
	}
	if files.Busy {
		out.Empty = "asking " + view.host + "…"
		return out
	}
	if files.Problem != "" {
		out.Empty = files.Problem
		return out
	}

	base := "select/files/" + view.host + "/" + view.name
	card := cardView{
		Key:      "files",
		Heading:  "Data",
		Aside:    "what lives in the service's volumes; configuration comes from the tree",
		GridHead: []string{"name", "size", "modified", ""},
	}
	if view.path != "" {
		// Inside a volume: uploads land in the directory being looked at.
		card.Buttons = []buttonView{{
			Label: "Upload here",
			Icon:  "box-arrow-up",
			Act:   "do/fileup/" + view.host + "/" + view.name + "/" + view.path,
		}}
		up := base
		at := strings.LastIndex(view.path, "/")
		if at > 0 {
			up += "/" + view.path[:at]
		}
		card.Grid = append(card.Grid, gridRow{Cells: []string{"..", "", "", ""}, Link: up})
	}
	for _, entry := range files.Entries {
		here := view.path
		if here != "" {
			here += "/"
		}
		here += entry.Name
		if entry.Dir {
			card.Grid = append(card.Grid, gridRow{
				Cells: []string{entry.Name + "/", "", "", ""},
				Link:  base + "/" + here,
			})
			continue
		}
		card.Grid = append(card.Grid, gridRow{
			Cells: []string{entry.Name, formatBytes(entry.Bytes),
				time.UnixMilli(int64(entry.MS)).Format("2006-01-02 15:04"), ""},
			Buttons: []buttonView{{
				Icon: "box-arrow-down",
				Act:  "do/filedl/" + view.host + "/" + view.name + "/" + here,
			}},
		})
	}
	if len(card.Grid) == 0 {
		out.Empty = view.name + " declares no named volume, so it has no data here"
		return out
	}
	out.Cards = append(out.Cards, card)
	return out
}

func serviceDetail(snap snapshot, view viewState) detailView {
	host, found := machineOf(snap, view.host)
	if !found {
		return detailView{Title: view.name, Single: true, Empty: "this machine is not in the fleet any more"}
	}
	var service supervisor.Status
	known := false
	for _, candidate := range host.Services {
		if candidate.Name == view.name {
			service = candidate
			known = true
		}
	}
	if !known {
		return detailView{Title: view.name, Single: true, Empty: "no file declares this service any more"}
	}

	// The state of the moment lives in the facts, which repaint on their own;
	// a subtitle that carried it would replace the whole pane at every flap.
	said := host.Host
	if service.Every != "" {
		said = host.Host + " · every " + service.Every
	}
	out := detailView{Title: service.Name, Subtitle: said, Single: true}

	facts := [][2]string{
		{"state", service.State},
		{"image", dash(service.Image)},
		{"desired", dash(service.Desired)},
	}
	if service.Orphan {
		facts[0][1] += " (running here, declared nowhere)"
	}
	if service.Every != "" {
		facts = append(facts, [2]string{"every", service.Every},
			[2]string{"runs going", strconv.Itoa(service.Runs)})
	} else {
		facts = append(facts, [2]string{"pid", dash(strconv.Itoa(service.PID))})
	}
	facts = append(facts,
		[2]string{"restarts", strconv.Itoa(service.Restarts)},
		[2]string{"last exit", strconv.Itoa(service.LastExit)})
	if service.LastError != "" {
		facts = append(facts, [2]string{"problem", service.LastError})
	}
	out.Cards = append(out.Cards, cardView{
		Key:     "facts",
		Heading: "Declaration",
		Facts:   facts,
		// On the heading line, where a person acts on what they just read:
		// one click each, and the log says what happened.
		Buttons: serviceActions(host.Host, service.Name),
	})

	cpu := seriesOf(host, metrics.ScopeService, service.Name, metrics.MetricCPUPercent)
	memory := seriesOf(host, metrics.ScopeService, service.Name, metrics.MetricMemoryBytes)
	if len(cpu) > 0 || len(memory) > 0 {
		cpuChart := chartOf([]layer{{Label: "cpu", Points: cpu, Colour: "#007aff", Area: true}}, view, percentOf, false)
		cpuChart.ID = "plot-cpu"
		memoryChart := chartOf([]layer{{Label: "memory", Points: memory, Colour: "#ff9f0a", Area: true}}, view, bytesOf, false)
		memoryChart.ID = "plot-mem"
		out.Cards = append(out.Cards, cardView{
			Key:     "usage",
			Heading: "Usage",
			Charts:  []chartView{cpuChart, memoryChart},
		})
	}

	return out
}

// serviceActions is every operation one service offers, each one click. The
// service's own panel is where they live: one subject, all its verbs in sight.
func serviceActions(host, service string) []buttonView {
	verbs := []struct{ label, icon string }{
		{"deploy", "box-arrow-up"},
		{"restart", "arrow-clockwise"},
		{"stop", "stop-fill"},
		{"start", "play-fill"},
		{"remove", "trash"},
	}
	out := make([]buttonView, 0, len(verbs))
	for _, verb := range verbs {
		out = append(out, buttonView{
			Label: verb.label,
			Icon:  verb.icon,
			Act:   fmt.Sprintf("do/%s/%s/%s", verb.label, host, service),
		})
	}
	return out
}

func cellsOf(host string, service supervisor.Status) rowCells {
	state := service.State
	if service.Every != "" && service.Runs > 0 {
		state = fmt.Sprintf("%s (%d)", state, service.Runs)
	}
	if service.Orphan {
		state += " (orphan)"
	}
	return rowCells{
		Key:     service.Name,
		Link:    "select/service/" + host + "/" + service.Name,
		Name:    service.Name,
		State:   state,
		Dot:     service.State,
		Image:   shortImage(service.Image),
		Since:   int64(service.Since),
		Job:     service.Every != "",
		Restart: strconv.Itoa(service.Restarts),
		Problem: service.LastError,
	}
}

func machineOf(snap snapshot, name string) (fleetHost, bool) {
	for _, host := range snap.Fleet {
		if host.Host == name {
			return host, true
		}
	}
	return fleetHost{}, false
}

func numbersOf(host fleetHost) []numberView {
	cpu := lastOf(seriesOf(host, metrics.ScopeHost, "", metrics.MetricCPUPercent))
	memory := ratioOf(host, metrics.MetricMemoryBytes, metrics.MetricMemoryTotal)
	disk := ratioOf(host, metrics.MetricDiskBytes, metrics.MetricDiskTotal)
	load := lastOf(seriesOf(host, metrics.ScopeHost, "", metrics.MetricLoad1))
	return []numberView{
		{Value: percentOf(cpu), Of: "cpu"},
		{Value: percentOf(lastOf(memory)), Of: "memory"},
		{Value: percentOf(lastOf(disk)), Of: "disk"},
		{Value: loadOf(load), Of: "load"},
	}
}

func seriesOf(host fleetHost, scope, name, metric string) []metrics.Point {
	for _, series := range host.Metrics {
		if series.Scope == scope && series.Name == name && series.Metric == metric {
			return series.Points
		}
	}
	return nil
}

func ratioOf(host fleetHost, part, whole string) []metrics.Point {
	used := seriesOf(host, metrics.ScopeHost, "", part)
	total := lastOf(seriesOf(host, metrics.ScopeHost, "", whole))
	if len(used) == 0 || total == nil || *total == 0 {
		return nil
	}
	out := make([]metrics.Point, 0, len(used))
	for _, point := range used {
		out = append(out, metrics.Point{TimeMS: point.TimeMS, Value: point.Value / *total * 100})
	}
	return out
}

func lastOf(points []metrics.Point) *float64 {
	if len(points) == 0 {
		return nil
	}
	value := points[len(points)-1].Value
	return &value
}

func hostLayers(host fleetHost) []layer {
	return []layer{
		{Label: "cpu", Points: seriesOf(host, metrics.ScopeHost, "", metrics.MetricCPUPercent),
			Colour: "#007aff", Area: true, Top: 100},
		{Label: "memory", Points: ratioOf(host, metrics.MetricMemoryBytes, metrics.MetricMemoryTotal),
			Colour: "#ff9f0a", Area: true, Top: 100},
	}
}

var palette = []string{"#007aff", "#ff9f0a", "#34c759", "#af52de", "#ff375f", "#5ac8fa", "#ffd60a", "#64d2ff"}

func stackedLayers(host fleetHost) []layer {
	var names []string
	for _, series := range host.Metrics {
		if series.Scope == metrics.ScopeService && series.Metric == metrics.MetricMemoryBytes && len(series.Points) > 0 {
			names = append(names, series.Name)
		}
	}
	sort.Strings(names)
	out := make([]layer, 0, len(names))
	for at, name := range names {
		out = append(out, layer{
			Label:  name,
			Points: seriesOf(host, metrics.ScopeService, name, metrics.MetricMemoryBytes),
			Colour: palette[at%len(palette)],
			Stack:  true,
		})
	}
	return out
}

func percentOf(value *float64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatFloat(*value, 'f', 1, 64) + "%"
}

func loadOf(value *float64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatFloat(*value, 'f', 2, 64)
}

func bytesOf(value *float64) string {
	if value == nil {
		return "-"
	}
	number := *value
	units := []string{"B", "K", "M", "G", "T"}
	at := 0
	for number >= 1024 && at < len(units)-1 {
		number /= 1024
		at++
	}
	digits := 0
	if at > 0 {
		digits = 1
	}
	return strconv.FormatFloat(number, 'f', digits, 64) + units[at]
}

func shortImage(image string) string {
	name, _, _ := strings.Cut(image, "@")
	return name
}

func dash(value string) string {
	if value == "" || value == "0" {
		return "-"
	}
	return value
}

/* The log. */

type lineView struct {
	Seq   uint64
	When  string
	Where string
	Text  string
	Class string
}

func inView(held line, view viewState) bool {
	switch view.kind {
	case "host":
		return held.Host == view.host
	case "service":
		return held.Host == view.host && held.Service == view.name
	}
	return true
}

func lineOf(held line, view viewState) lineView {
	where := held.Service
	if view.kind == "fleet" {
		where = held.Host + "/" + held.Service
	}
	if held.Run != "" {
		where += "/" + held.Run
	}
	out := lineView{
		Seq:   held.Seq,
		When:  time.UnixMilli(int64(held.Time)).Format("15:04:05"),
		Where: where,
		Text:  held.Text,
	}
	if held.Stream == logs.StreamEvent {
		out.Class = "event"
	}
	// A failure wins the colour, and it is the ONLY thing that gets red here.
	// A container's stderr does not: every well-behaved program logs there, so
	// caddy's ordinary shutdown notes would paint the whole pane red. (This
	// compared the stream with "err" for a while, when the stream is called
	// "stderr"; the mismatch was hiding a wrong intent, and fixing the
	// comparison exposed it.)
	if held.Bad {
		out.Class = "err"
	}
	return out
}

/* The Settings page: where the panel's tree lives and what this binary is.
   The window is also for people who do not live in a terminal, so the one
   thing the command line configures — the tree — is visible and changeable
   here, with the system's own directory chooser. */

type settingsInfo struct {
	ConfigDir string
	Inventory string
	Machines  int
	Problem   string
}

func settingsDetail(info settingsInfo) detailView {
	out := detailView{Title: "Settings", Subtitle: "what this panel watches", Single: true}
	out.Cards = append(out.Cards, cardView{
		Key:     "tree",
		Heading: "Configuration",
		Problem: info.Problem,
		Facts: [][2]string{
			{"configuration root", info.ConfigDir},
			{"inventory", info.Inventory},
			{"machines listed", strconv.Itoa(info.Machines)},
		},
		Buttons: []buttonView{
			{Label: "Choose…", Icon: "folder2-open", Act: "config/choose"},
			{Label: "Reload", Icon: "arrow-clockwise", Act: "config/reload"},
		},
	})
	out.Cards = append(out.Cards, cardView{
		Key:     "about",
		Heading: "About hostctl",
		Facts: [][2]string{
			{"version", version.Version},
			{"protocol", strconv.Itoa(version.Protocol)},
			{"schema", strconv.Itoa(version.Schema)},
			{"services are declared in", "one .filo file, or a directory with an init.filo and the files that travel with it"},
		},
	})
	return out
}
