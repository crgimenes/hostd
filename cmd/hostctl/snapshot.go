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

// poll keeps the snapshot current until the window closes. It is the ONLY
// goroutine that ever talks to the fleet: the scheme handler that serves the
// page runs on the UI thread, so a handler that waited on ssh would freeze the
// window — and a handler that talked concurrently with this loop would
// interleave two requests on one pipe. A handler that needs fresh numbers
// nudges and answers from what is already held.
func (p *panel) poll(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	p.round(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.nudge:
		}
		p.round(ctx)
	}
}

// asked, not waited for: the handler goes back to the window immediately and
// the next answer carries what the round brought.
func (p *panel) wake() {
	select {
	case p.nudge <- struct{}{}:
	default:
	}
}

func (p *panel) round(ctx context.Context) {
	finished := make(chan struct{})
	go p.sayIfSlow(finished)

	view := p.viewport()
	// Each machine is folded in and pushed as it answers, so the window fills
	// up rather than appearing all at once when the last one is done.
	p.Fleet(view.window, view.from, view.to, p.sequences(), func(answer fleetHost) {
		p.absorb([]fleetHost{answer})
		p.push()
	})

	close(finished)
	p.working(false)
	p.push()
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
