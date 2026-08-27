package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/logs"
)

// One click is one action (crg, 2026-08-26). There is no confirmation and no
// dialog: what an action is doing arrives in the log, line by line, the same
// way everything else on this screen does — and the button that started it
// stays held down until it is over. A progress indicator with nothing behind
// it is anxiety with nowhere to go; a log going by is somebody watching their
// machine work, and able to act when it goes wrong.

// Generous on purpose: stopping a container spends its own stop-timeout.
const actionTimeout = 2 * time.Minute

// A deploy may carry a gigabyte; bytes take what bytes take.
const deployTimeout = 30 * time.Minute

// action reads one "do/<verb>/<machine>[/<service>]" and answers with what it
// does — checked before anything is started, so a mistyped act is refused
// rather than reported halfway through. The work itself runs off this
// goroutine: nothing waits for it, because the log is what tells the story.
func (p *panel) action(parts []string) (func(), error) {
	refuse := fmt.Errorf("no such action: %q", strings.Join(parts, "/"))
	if len(parts) < 2 {
		return nil, refuse
	}
	verb, host := parts[0], parts[1]
	name := ""
	if len(parts) > 2 {
		name = parts[2]
	}
	ops := map[string]string{
		"remove":  api.OpServiceRemove,
		"restart": api.OpServiceRestrt,
		"stop":    api.OpServiceStop,
		"start":   api.OpServiceStart,
	}
	op, isCommand := ops[verb]
	switch {
	case verb == "deploy" && name != "":
		return func() { p.deploy(host, name) }, nil
	case isCommand && name != "":
		return func() { p.command(host, name, op) }, nil
	case verb == "prune":
		return func() { p.prune(host) }, nil
	case verb == "install":
		return func() { p.install(host) }, nil
	}
	return nil, refuse
}

// hold marks an action as running, so its button renders held down and a
// second click on it does nothing. Answers false when it was already running.
func (p *panel) hold(action string) bool {
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	if p.busy[action] {
		return false
	}
	p.busy[action] = true
	return true
}

func (p *panel) release(action string) {
	p.busyMu.Lock()
	delete(p.busy, action)
	p.busyMu.Unlock()
}

func (p *panel) running(action string) bool {
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	return p.busy[action]
}

// What each one-request action is about to do, in the words of somebody
// watching: the log says it before the machine is asked.
var asking = map[string]string{
	api.OpServiceRemove: "stopping it and taking it off this machine…",
	api.OpServiceRestrt: "restarting it…",
	api.OpServiceStop:   "stopping it…",
	api.OpServiceStart:  "starting it…",
}

// deploy puts a service from the catalog on a machine, saying what it is doing
// as it does it. It overwrites: the declaration, the image, the container.
func (p *panel) deploy(host, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	client, err := p.reach(host)
	if err != nil {
		p.problem(host, name, err)
		return
	}
	defer func() { _ = client.Close() }()
	outcome, err := deployService(ctx, client, p.opt, p.configDir(), name, func(step string) {
		p.sayLine(host, name, step, false)
	})
	if err != nil {
		p.problem(host, name, err)
		return
	}
	p.sayLine(host, name, outcome, false)
	p.wake()
}

// command is every action that is one request: the daemon does it, the machine
// reports it in its own timeline, and this line says the ask was answered.
func (p *panel) command(host, name, op string) {
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	client, err := p.reach(host)
	if err != nil {
		p.problem(host, name, err)
		return
	}
	defer func() { _ = client.Close() }()
	// Said before the wait, not after it: stopping a container spends its
	// whole grace period, and a log that only speaks once the work is done is
	// a log that was silent exactly while somebody wondered.
	p.sayLine(host, name, asking[op], false)
	resp, err := client.Do(ctx, api.Request{Op: op, Name: name})
	if err != nil {
		p.problem(host, name, err)
		return
	}
	if resp.Failed() {
		p.problem(host, name, resp.Err())
		return
	}
	p.wake()
}

func (p *panel) prune(host string) {
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	client, err := p.reach(host)
	if err != nil {
		p.problem(host, "hostd", err)
		return
	}
	defer func() { _ = client.Close() }()
	resp, err := client.Do(ctx, api.Request{
		Op:               api.OpImagePrune,
		Keep:             api.DefaultImageKeep,
		AllowDestructive: true,
	})
	if err != nil {
		p.problem(host, "hostd", err)
		return
	}
	if resp.Failed() {
		p.problem(host, "hostd", resp.Err())
		return
	}
	var plan api.ImagePrune
	err = decode(ctx, resp.Body, &plan)
	if err != nil {
		p.problem(host, "hostd", err)
		return
	}
	var freed float64
	for _, image := range plan.Remove {
		if image.Removed {
			freed += image.Bytes
		}
		if image.Problem != "" {
			p.sayLine(host, "hostd", fmt.Sprintf("%s stayed: %s", tagsText(image.Tags), image.Problem), true)
		}
	}
	p.sayLine(host, "hostd", fmt.Sprintf("cleaned up %d image(s), %s freed; %d kept",
		len(plan.Remove), formatBytes(freed), plan.Kept), false)
	p.wake()
}

// install puts this window's own hostd on a machine — the operation a machine
// with no daemon, or one too old for an operation, needs. Its report goes to
// the log like everything else.
func (p *panel) install(host string) {
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	opt := p.opt
	opt.host = host
	opt.out = lineWriter{say: func(text string) { p.sayLine(host, "hostd", text, false) }}
	// The CLI writes this to stderr; a window has no stderr its operator reads,
	// and the log is where they are looking.
	stale := staleDaemon()
	if stale != "" {
		p.sayLine(host, "hostd", stale, false)
	}
	_, err := runInstall(ctx, opt, nil)
	if err != nil {
		p.problem(host, "hostd", err)
		return
	}
	// The daemon this window was talking to has been replaced, so the rounds'
	// pipe is dead: let it go, and the next round dials the one that came up
	// and asks it what it is — a version left in the cache would keep the
	// amber dot on a machine that is now current.
	p.drop(host)
	p.forgetVersion(host)
	p.wake()
}

// lineWriter turns what a command prints into log lines, so an operation
// written for a terminal reports into the window without being rewritten.
type lineWriter struct {
	say func(string)
}

func (w lineWriter) Write(p []byte) (int, error) {
	for line := range strings.Lines(string(p)) {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			w.say(trimmed)
		}
	}
	return len(p), nil
}

func (p *panel) problem(host, service string, err error) {
	p.sayLine(host, service, err.Error(), true)
}

// sayLine puts a line of hostctl's own into the window's timeline, beside what
// the machines are saying. It lives only here: what this operator's session
// did is worth watching while it happens, and the machine's audit log is what
// remembers it afterwards.
func (p *panel) sayLine(host, service, text string, bad bool) {
	p.snapMu.Lock()
	p.cursor++
	p.snap.Lines = append(p.snap.Lines, line{
		Time:    float64(time.Now().UnixMilli()),
		Service: service,
		Stream:  logs.StreamEvent,
		Text:    text,
		Host:    host,
		N:       p.cursor,
		Bad:     bad,
	})
	if len(p.snap.Lines) > maxLines {
		p.snap.Lines = p.snap.Lines[len(p.snap.Lines)-maxLines:]
	}
	p.snapMu.Unlock()
	p.push()
}
