package main

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/crgimenes/hostd/api"
)

// How often the fleet is asked. The window never waits for it: the page reads
// whatever the last round brought, so a machine on the far side of a slow link
// delays its own numbers and nobody else's.
const pollInterval = 2 * time.Second

// The most log lines the window keeps. A window that grows without bound is a
// window that dies slowly.
const maxLines = 4000

// How long a round may take before the window is told it is working. Under
// this, saying so would be a flicker on every tick and the wire would never be
// quiet; over it, a still window reads as a frozen one — which is what the
// first round looks like, when four machines are being reached for the first
// time and the slowest takes seconds.
const slowRound = 400 * time.Millisecond

// What the panel knows, as of the last round.
type snapshot struct {
	Fleet   []fleetHost
	Lines   []line
	Updated time.Time
	// A round is going and taking long enough to say so, and which machines it
	// is still waiting on: "working" with no name is indistinguishable from
	// stuck, and the machine that is switched off is exactly what somebody
	// wants named.
	Busy     bool
	Reaching []string
}

// A log line with the machine that wrote it, which the answer does not carry
// because the machine already knows who it is, and a number of the panel's own.
// A sequence only means anything beside the machine that issued it, so the
// window cannot ask "what is new" with somebody else's numbering.
type line struct {
	api.LogLine
	Host string
	N    uint64
}

// Each machine is watched by its own loop, and a loop is the only goroutine
// that ever talks to its machine: the scheme handler that serves the page runs
// on the UI thread, so a handler that waited on ssh would freeze the window —
// and two goroutines on one pipe would interleave their requests. A handler
// that needs fresh numbers nudges and answers from what is already held.
//
// One loop per machine rather than one round over the fleet, because a round
// ends when its slowest machine does: a machine that is switched off spends
// its ssh timeout on every answer the operator is waiting for, and a click
// looks like nothing happened. Here it spends that timeout on its own line.
type hostLoop struct {
	nudge chan struct{}
	stop  context.CancelFunc
}

// start begins watching the fleet; syncLoops keeps the loops matching the
// inventory from then on.
func (p *panel) start(ctx context.Context) {
	p.life = ctx
	p.syncLoops()
}

func (p *panel) syncLoops() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Not started (the command line, the tests): nothing to keep in step.
	if p.life == nil {
		return
	}
	kept := make(map[string]bool, len(p.hosts))
	for _, host := range p.hosts {
		kept[host] = true
	}
	for host, loop := range p.loops {
		if kept[host] {
			continue
		}
		loop.stop()
		delete(p.loops, host)
	}
	for _, host := range p.hosts {
		if p.loops[host] != nil {
			continue
		}
		watching, stop := context.WithCancel(p.life)
		loop := &hostLoop{nudge: make(chan struct{}, 1), stop: stop}
		p.loops[host] = loop
		go p.pollHost(watching, host, loop.nudge)
	}
}

func (p *panel) pollHost(ctx context.Context, host string, nudge chan struct{}) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		p.roundOne(ctx, host)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-nudge:
		}
	}
}

// asked, not waited for: the handler goes back to the window immediately and
// the next answer carries what the machines bring.
func (p *panel) wake() {
	p.mu.Lock()
	loops := make([]*hostLoop, 0, len(p.loops))
	for _, loop := range p.loops {
		loops = append(loops, loop)
	}
	p.mu.Unlock()
	for _, loop := range loops {
		select {
		case loop.nudge <- struct{}{}:
		default:
		}
	}
}

// roundOne asks one machine and folds the answer in, so the window fills up
// machine by machine rather than all at once when the last one is done.
func (p *panel) roundOne(ctx context.Context, host string) {
	asking, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	finished := make(chan struct{})
	go p.sayIfSlow(finished)

	view := p.viewport()
	// How far back the graphs reach, unless the operator dragged a range on a
	// chart, which is used instead.
	fromMS, toMS := view.from, view.to
	if fromMS <= 0 {
		fromMS = float64(time.Now().Add(-time.Duration(view.window) * time.Second).UnixMilli())
		toMS = 0
	}
	answer := p.ask(asking, host, fromMS, toMS, p.sequences()[host], host == imagesHost(view))

	close(finished)
	p.absorb([]fleetHost{answer})
	p.settle()
	p.push()
}

// settle lowers the indicator only when no machine is still being reached:
// with a loop per machine, the first one home must not silence the ones still
// out.
func (p *panel) settle() {
	p.snapMu.RLock()
	idle := len(p.snap.Reaching) == 0
	p.snapMu.RUnlock()
	if idle {
		p.working(false)
	}
}

// Which machine, if any, is being asked for its images this round: the one
// whose image screen is open, and no other.
func imagesHost(view viewState) string {
	if view.kind != "images" {
		return ""
	}
	return view.host
}

// sayIfSlow tells the window the panel is working, but only once the round has
// taken long enough for a person to wonder.
func (p *panel) sayIfSlow(finished chan struct{}) {
	select {
	case <-finished:
		return
	case <-time.After(slowRound):
	}
	p.working(true)
}

// working pushes only on the edge: a state that is already on screen is not
// sent again.
func (p *panel) working(busy bool) {
	p.snapMu.Lock()
	changed := p.snap.Busy != busy
	p.snap.Busy = busy
	p.snapMu.Unlock()
	if changed {
		p.push()
	}
}

// reaching records that a machine is being asked, and answers whether the
// window should be told again. The set is what the status names.
func (p *panel) reaching(host string, going bool) {
	p.snapMu.Lock()
	at := slices.Index(p.snap.Reaching, host)
	switch {
	case going && at < 0:
		p.snap.Reaching = append(p.snap.Reaching, host)
	case !going && at >= 0:
		p.snap.Reaching = slices.Delete(p.snap.Reaching, at, at+1)
	}
	busy := p.snap.Busy
	p.snapMu.Unlock()
	// Only worth a push while the window is already showing the indicator:
	// otherwise this is a name nobody is reading.
	if busy {
		p.push()
	}
}

// absorb folds answers into what the panel knows, one machine or many.
// Separate from the asking so what it promises — a bad answer never unmakes
// the picture, and a machine keeps its place — can be tested without a fleet.
func (p *panel) absorb(fleet []fleetHost) {
	p.snapMu.Lock()
	defer p.snapMu.Unlock()
	p.snap.Updated = time.Now()
	for _, host := range fleet {
		at := slices.IndexFunc(p.snap.Fleet, func(known fleetHost) bool { return known.Host == host.Host })
		if at < 0 {
			// New to the panel: it takes the place the inventory gives it, so
			// the order on screen is the order in the file rather than the
			// order the machines happened to answer in.
			p.snap.Fleet = append(p.snap.Fleet, host)
			p.orderFleet()
			at = slices.IndexFunc(p.snap.Fleet, func(known fleetHost) bool { return known.Host == host.Host })
		}
		if host.Error != "" {
			// One bad round must not unmake the picture: with the answer
			// empty, the machine's services would vanish from the tree and
			// come back next round — which reads as the tree closing and
			// opening by itself. What was known stays, marked unanswered.
			known := p.snap.Fleet[at]
			if len(known.Services) > 0 {
				host.Services = known.Services
				host.Metrics = known.Metrics
			}
			// A failed round asked for no images either, so what is on the
			// image screen would empty itself every time ssh hiccups.
			if len(known.Images) > 0 {
				host.Images = known.Images
				host.ImagesError = known.ImagesError
			}
		}
		p.snap.Fleet[at] = host
		for _, arrived := range host.Lines {
			// A machine numbers its own lines, so a sequence only means
			// anything beside the machine that wrote it.
			key := host.Host + ":" + itoa(arrived.Seq)
			if p.held[key] {
				continue
			}
			p.held[key] = true
			p.cursor++
			p.snap.Lines = append(p.snap.Lines, line{LogLine: arrived, Host: host.Host, N: p.cursor})
		}
		if host.Since > p.since[host.Host] {
			p.since[host.Host] = host.Since
		}
	}
	if len(p.snap.Lines) > maxLines {
		dropped := p.snap.Lines[:len(p.snap.Lines)-maxLines]
		for _, gone := range dropped {
			delete(p.held, gone.Host+":"+itoa(gone.Seq))
		}
		p.snap.Lines = p.snap.Lines[len(p.snap.Lines)-maxLines:]
	}
}

// orderFleet puts the machines in the order the inventory lists them, so a
// slow machine does not sink to the bottom of the tree for having answered
// last. The caller holds snapMu.
func (p *panel) orderFleet() {
	order := make(map[string]int, len(p.snap.Fleet))
	for at, host := range p.hostsNow() {
		order[host] = at
	}
	slices.SortStableFunc(p.snap.Fleet, func(a, b fleetHost) int {
		return order[a.Host] - order[b.Host]
	})
}

func (p *panel) sequences() map[string]uint64 {
	p.snapMu.RLock()
	defer p.snapMu.RUnlock()
	out := make(map[string]uint64, len(p.since))
	maps.Copy(out, p.since)
	return out
}

func (p *panel) latest() snapshot {
	p.snapMu.RLock()
	defer p.snapMu.RUnlock()
	return p.snap
}
