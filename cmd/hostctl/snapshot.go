package main

import (
	"context"
	"maps"
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
	Problem string
	// A round is going and taking long enough to say so.
	Busy bool
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
	p.absorb(p.Fleet(view.window, view.from, view.to, p.sequences()))

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

// absorb folds one round's answer into what the panel knows. Separate from the
// asking so what it promises — a bad answer never unmakes the picture — can be
// tested without a fleet.
func (p *panel) absorb(fleet []fleetHost) {
	p.snapMu.Lock()
	defer p.snapMu.Unlock()
	previous := p.snap.Fleet
	p.snap.Fleet = fleet
	p.snap.Updated = time.Now()
	p.snap.Problem = ""
	for at, host := range fleet {
		if host.Error != "" {
			p.snap.Problem = host.Error
			// One bad round must not unmake the picture: with the answer
			// empty, the machine's services would vanish from the tree and
			// come back next round — which reads as the tree closing and
			// opening by itself. What was known stays, marked unanswered.
			for _, known := range previous {
				if known.Host == host.Host && len(known.Services) > 0 {
					fleet[at].Services = known.Services
					fleet[at].Metrics = known.Metrics
				}
			}
		}
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
