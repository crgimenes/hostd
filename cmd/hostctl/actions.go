package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/logs"

	"github.com/crgimenes/glaze"
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
	case verb == "filerm" && len(parts) > 3:
		wire := strings.Join(parts[2:], "/")
		act := "do/" + strings.Join(parts, "/")
		if !p.disarm(act) {
			// First click arms; the button says so and nothing is deleted.
			return func() { p.arm(act) }, nil
		}
		return func() { p.deleteFile(host, wire) }, nil
	case verb == "filedl" && len(parts) > 3:
		wire := strings.Join(parts[2:], "/")
		return func() { p.downloadFile(host, wire) }, nil
	case verb == "fileup" && len(parts) > 3:
		// parts[2] is the service, the rest the directory being looked at.
		wire := strings.Join(parts[3:], "/")
		return func() { p.uploadFile(host, parts[2], wire) }, nil
	}
	return nil, refuse
}

// How long an armed delete waits for its second click before standing down.
const armedFor = 5 * time.Second

func (p *panel) arm(act string) {
	p.armedMu.Lock()
	if p.armed == nil {
		p.armed = map[string]time.Time{}
	}
	p.armed[act] = time.Now().Add(armedFor)
	p.armedMu.Unlock()
	p.push()
	// Stand down by itself: an armed button somebody walked away from must not
	// still be armed when they come back.
	time.AfterFunc(armedFor, func() {
		p.armedMu.Lock()
		expired := !p.armed[act].After(time.Now())
		if expired {
			delete(p.armed, act)
		}
		p.armedMu.Unlock()
		if expired {
			p.push()
		}
	})
}

// disarm answers whether the act was armed, and spends the arming either way.
func (p *panel) disarm(act string) bool {
	p.armedMu.Lock()
	defer p.armedMu.Unlock()
	until, held := p.armed[act]
	if !held {
		return false
	}
	delete(p.armed, act)
	return until.After(time.Now())
}

func (p *panel) isArmed(act string) bool {
	p.armedMu.Lock()
	defer p.armedMu.Unlock()
	return p.armed[act].After(time.Now())
}

// deleteFile is the second click: the file goes, the log says so, the listing
// refreshes. The wire is service/volume/path, the same shape the click built.
func (p *panel) deleteFile(host, wire string) {
	svc, rest, _ := strings.Cut(wire, "/")
	client, err := p.reach(host)
	if err != nil {
		p.problem(host, svc, err)
		return
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	resp, err := client.Do(ctx, api.Request{Op: api.OpFileDelete, Service: svc, Name: rest})
	if err != nil {
		p.problem(host, svc, err)
		return
	}
	if resp.Failed() {
		p.problem(host, svc, resp.Err())
		return
	}
	p.sayLine(host, svc, "deleted "+rest, false)
	at := strings.LastIndex(rest, "/")
	dir := ""
	if at > 0 {
		dir = rest[:at]
	}
	p.loadFiles(host, svc, dir)
}

// Where a download lands: the user's own Downloads when there is one, the
// working directory when there is not. No dialog — the log says where it went,
// which is the same channel everything else answers on.
func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	downloads := filepath.Join(home, "Downloads")
	info, err := os.Stat(downloads)
	if err != nil || !info.IsDir() {
		return home
	}
	return downloads
}

// downloadFile brings one file of a service's data here. The wire path is
// service/volume/inside — the service first, the same shape the click built.
func (p *panel) downloadFile(host, wire string) {
	svc, rest, _ := strings.Cut(wire, "/")
	client, err := p.reach(host)
	if err != nil {
		p.problem(host, svc, err)
		return
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()

	var saved *os.File
	var into string
	resp, err := client.FetchFile(ctx, svc, rest, func(told api.FileTransfer) (io.Writer, error) {
		into = filepath.Join(downloadsDir(), filepath.Base(told.Path))
		file, createErr := os.Create(into) // #nosec G304 -- the user's own downloads directory
		if createErr != nil {
			return nil, createErr
		}
		saved = file
		return file, nil
	})
	if saved != nil {
		closeErr := saved.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}
	if err != nil {
		if into != "" {
			// Half a file under the real name reads later as the whole one.
			_ = os.Remove(into)
		}
		p.problem(host, svc, err)
		return
	}
	if resp.Failed() {
		p.problem(host, svc, resp.Err())
		return
	}
	p.sayLine(host, svc, fmt.Sprintf("saved %s here as %s", rest, into), false)
}

// uploadFile asks with the system's own file chooser and places the choice in
// the directory the files screen is looking at.
func (p *panel) uploadFile(host, svc, dir string) {
	if p.dialogs == nil {
		p.problem(host, svc, fmt.Errorf("uploading needs the window; use hostctl file put on the command line here"))
		return
	}
	local, err := p.dialogs.OpenFile(glaze.FileDialogOptions{Title: "Upload into " + svc + ":/" + dir})
	if err != nil {
		p.problem(host, svc, err)
		return
	}
	if local == "" {
		return
	}
	file, err := os.Open(local) // #nosec G304 -- the file the person just chose
	if err != nil {
		p.problem(host, svc, err)
		return
	}
	defer func() { _ = file.Close() }()

	client, err := p.reach(host)
	if err != nil {
		p.problem(host, svc, err)
		return
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	wire := dir + "/" + filepath.Base(local)
	resp, err := client.SendFile(ctx, svc, wire, file)
	if err != nil {
		p.problem(host, svc, err)
		return
	}
	if resp.Failed() {
		p.problem(host, svc, resp.Err())
		return
	}
	var told api.FileTransfer
	_ = decode(ctx, resp.Body, &told)
	p.sayLine(host, svc, fmt.Sprintf("placed %s as %s (%s)", filepath.Base(local), wire, formatBytes(told.Bytes)), false)
	// What the screen shows includes the new file now.
	p.loadFiles(host, svc, dir)
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
