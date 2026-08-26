package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/daemon"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/supervisor"
)

// The window acts, and the verbs are the operator's own (crg, 2026-08-26):
// the tree describes services, the inventory lists machines, and any
// description goes onto — and comes off — any machine that can run it. A
// click performs the SAME operation the command line performs, over the same
// connection, behind a confirmation that says what will happen and still
// shows the terminal equivalent for whoever prefers it.

// Generous on purpose: stopping a container spends its own stop-timeout.
const actionTimeout = 2 * time.Minute

// A deploy may carry an image; bytes take what bytes take.
const deployTimeout = 10 * time.Minute

// One confirmation or outcome, rendered into the #action dialog.
type dialogView struct {
	Title string
	Notes []string
	// A table, when the action is about a list — the images a cleanup would
	// remove.
	GridHead []string
	Grid     [][]string
	// The command line equivalent, still shown: the window acting does not
	// take the terminal away from anybody.
	Command string
	Confirm buttonView
	// One button per choice, when the dialog is a catalog — the services the
	// tree describes, each a deploy away.
	Choices []buttonView
	Danger  bool
	Outcome string
	Bad     bool
	// A run reports from the corner, over a live screen; only a question that
	// needs an answer takes the centre.
	Corner bool
}

func isServiceVerb(verb string) bool {
	return verb == "restart" || verb == "stop" || verb == "start" || verb == "redeploy"
}

// action answers one "confirm/..." or "run/..." act with the dialog to show.
func (p *panel) action(parts []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	var view dialogView
	switch {
	case parts[0] == "confirm" && len(parts) == 4 && isServiceVerb(parts[1]):
		view = confirmService(parts[1], parts[2], parts[3])
	case parts[0] == "confirm" && parts[1] == "deploy" && len(parts) == 4:
		view = p.confirmDeploy(parts[2], parts[3])
	case parts[0] == "run" && parts[1] == "deploy" && len(parts) == 4:
		view = p.runDeploy(parts[2], parts[3])
	case parts[0] == "confirm" && parts[1] == "remove" && len(parts) == 4:
		view = confirmRemove(parts[2], parts[3])
	case parts[0] == "run" && parts[1] == "remove" && len(parts) == 4:
		view = p.runRemove(ctx, parts[2], parts[3])
	case parts[0] == "confirm" && parts[1] == "add" && len(parts) == 3:
		view = p.confirmAdd(ctx, parts[2])
	case parts[0] == "confirm" && parts[1] == "prune" && len(parts) == 3:
		view = p.prune(ctx, parts[2], false)
	case parts[0] == "run" && parts[1] == "prune" && len(parts) == 3:
		view = p.prune(ctx, parts[2], true)
	case parts[0] == "confirm" && parts[1] == "update" && len(parts) == 3:
		view = p.confirmUpdate(ctx, parts[2])
	case parts[0] == "run" && parts[1] == "update" && len(parts) == 3:
		view = p.runUpdate(ctx, parts[2])
	case parts[0] == "run" && len(parts) == 4 && isServiceVerb(parts[1]):
		view = p.runService(ctx, parts[1], parts[2], parts[3])
	default:
		return "", fmt.Errorf("no such action: %q", strings.Join(parts, "/"))
	}
	view.Corner = parts[0] == "run"
	var out bytes.Buffer
	err := p.pages.ExecuteTemplate(&out, "actionDialog", view)
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

func failed(view dialogView, err error) dialogView {
	view.Outcome = err.Error()
	view.Bad = true
	return view
}

// failedResp is failed with the answer's code read: a daemon too old to know
// the operation is not a dead end — the window carries the daemon that knows
// it, and the person clicking always has the permission to put it there.
func (p *panel) failedResp(view dialogView, host string, resp api.Response) dialogView {
	view = failed(view, resp.Err())
	if resp.Code == api.CodeUnknownOp {
		view.Notes = append(view.Notes,
			"this machine's hostd is older than this window; updating it is one click, and the services keep running through the restart")
		view.Confirm = buttonView{Label: "update hostd on " + host, Act: "confirm/update/" + host}
	}
	return view
}

func confirmService(verb, host, name string) dialogView {
	return dialogView{
		Title:   fmt.Sprintf("%s %s on %s", verb, name, host),
		Notes:   []string{"The window runs the same operation the command line runs:"},
		Command: fmt.Sprintf("hostctl -host %s service %s %s", host, verb, name),
		Confirm: buttonView{Label: verb, Act: strings.Join([]string{"run", verb, host, name}, "/")},
		Danger:  verb == "stop",
	}
}

func (p *panel) runService(ctx context.Context, verb, host, name string) dialogView {
	ops := map[string]string{
		"restart":  api.OpServiceRestrt,
		"stop":     api.OpServiceStop,
		"start":    api.OpServiceStart,
		"redeploy": api.OpServiceRedeploy,
	}
	view := dialogView{Title: fmt.Sprintf("%s %s on %s", verb, name, host)}
	client, err := p.actionClient(host)
	if err != nil {
		return failed(view, err)
	}
	resp, err := client.Do(ctx, api.Request{Op: ops[verb], Name: name})
	if err != nil {
		p.dropAction(host)
		return p.failedErr(view, host, err)
	}
	if resp.Failed() {
		return p.failedResp(view, host, resp)
	}
	p.wake()
	view.Outcome = "done — " + stateOf(ctx, resp, name)
	return view
}

// What the daemon says the service is now, which is worth more than "ok".
func stateOf(ctx context.Context, resp api.Response, name string) string {
	var statuses []supervisor.Status
	err := decode(ctx, resp.Body, &statuses)
	if err == nil {
		for _, status := range statuses {
			if status.Name == name {
				return fmt.Sprintf("%s is %s", name, status.State)
			}
		}
	}
	return name + " answered"
}

// A deploy always overwrites: the tree's declaration, the image this machine
// holds locally, and a fresh container. Deploying what already runs is how a
// new version goes live — that is the point, not an error.
func (p *panel) confirmDeploy(host, name string) dialogView {
	return dialogView{
		Title: fmt.Sprintf("deploy %s on %s", name, host),
		Notes: []string{
			fmt.Sprintf("sends %s's declaration and files from the tree and starts a fresh container", name),
			"the image goes too, by whichever path is true: built here it is sent from here; already on the machine it stays; otherwise the machine pulls it from its registry",
			"deploying over a running service replaces it — that is how a new version goes live",
		},
		Command: fmt.Sprintf("hostctl -host %s service deploy %s", host, name),
		Confirm: buttonView{Label: "deploy " + name, Act: strings.Join([]string{"run", "deploy", host, name}, "/")},
	}
}

func (p *panel) runDeploy(host, name string) dialogView {
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	view := dialogView{
		Title:   fmt.Sprintf("deploy %s on %s", name, host),
		Command: fmt.Sprintf("hostctl -host %s service deploy %s", host, name),
		Corner:  true,
	}
	client, err := p.actionClient(host)
	if err != nil {
		return failed(view, err)
	}
	// Each step lands in the dialog as it happens: an operation that takes
	// seconds in silence is an operation somebody clicks again.
	outcome, err := deployService(ctx, client, p.opt, p.configDir(), name, func(step string) {
		view.Notes = append(view.Notes, step)
		p.sayDialog(view)
	})
	if err != nil {
		return p.failedErr(view, host, err)
	}
	p.wake()
	view.Notes = append(view.Notes, p.troubles(ctx, client)...)
	view.Outcome = "deployed — " + outcome
	return view
}

// failedErr is failedResp for the paths that only kept the error.
func (p *panel) failedErr(view dialogView, host string, err error) dialogView {
	if apiErr, ok := errors.AsType[api.Error](err); ok {
		return p.failedResp(view, host, api.Response{Code: apiErr.Code, Message: apiErr.Message})
	}
	view = failed(view, err)
	// A machine ssh reached where no hostd answered is the state an install
	// fixes, so that is what is offered — not a dead end.
	if errors.Is(err, api.ErrNoAnswer) {
		view.Notes = append(view.Notes,
			"no hostd answered on this machine; this window carries one, and installing it is one click")
		view.Confirm = buttonView{Label: "install hostd on " + host, Act: "confirm/update/" + host}
	}
	return view
}

// sayDialog pushes an interim state of the action dialog into the window,
// mid-run. Outside the window (the HTTP mirror) there is nobody to push to,
// and the finished dialog carries the whole story anyway.
func (p *panel) sayDialog(view dialogView) {
	if p.emit == nil {
		return
	}
	var out bytes.Buffer
	err := p.pages.ExecuteTemplate(&out, "actionDialog", view)
	if err != nil {
		return
	}
	encoded, err := json.Marshal(out.String())
	if err != nil {
		return
	}
	p.emit("hostdApply(" + string(encoded) + ")")
}

func confirmRemove(host, name string) dialogView {
	return dialogView{
		Title: fmt.Sprintf("remove %s from %s", name, host),
		Notes: []string{
			fmt.Sprintf("stops %s and removes its container, its declaration and its image from %s; volumes and their data stay", name, host),
			"the tree still describes it — a deploy puts it back",
		},
		Command: fmt.Sprintf("hostctl -host %s service remove %s", host, name),
		Confirm: buttonView{Label: fmt.Sprintf("remove %s from %s", name, host), Act: strings.Join([]string{"run", "remove", host, name}, "/")},
		Danger:  true,
	}
}

func (p *panel) runRemove(ctx context.Context, host, name string) dialogView {
	view := dialogView{
		Title:   fmt.Sprintf("remove %s from %s", name, host),
		Command: fmt.Sprintf("hostctl -host %s service remove %s", host, name),
	}
	client, err := p.actionClient(host)
	if err != nil {
		return failed(view, err)
	}
	resp, err := client.Do(ctx, api.Request{Op: api.OpServiceRemove, Name: name})
	if err != nil {
		p.dropAction(host)
		return p.failedErr(view, host, err)
	}
	if resp.Failed() {
		return p.failedResp(view, host, resp)
	}
	var report api.Removal
	err = decode(ctx, resp.Body, &report)
	if err != nil {
		return failed(view, err)
	}
	p.wake()
	view.Notes = append(view.Notes,
		"container "+report.Container,
		"image "+report.Image)
	view.Outcome = fmt.Sprintf("%s removed from %s — the tree still describes it", name, host)
	view.Confirm = buttonView{
		Label: fmt.Sprintf("deploy %s back", name),
		Act:   strings.Join([]string{"confirm", "deploy", host, name}, "/"),
	}
	return view
}

// confirmAdd is the catalog: everything the tree describes, each one a deploy
// away. What the machine already runs is said beside the name — deploying it
// again overwrites, which is what deploying it again is for.
func (p *panel) confirmAdd(ctx context.Context, host string) dialogView {
	view := dialogView{Title: "deploy on " + host}
	dir := p.configDir()
	declarations, loadErr := service.LoadTree(ctx, dir)
	if loadErr != nil {
		view.Notes = append(view.Notes, "part of the tree could not be read: "+loadErr.Error())
	}
	if len(declarations) == 0 {
		view.Notes = append(view.Notes, fmt.Sprintf(
			"the tree at %s describes no services yet; a service is a .filo file, or a directory with an init.filo and the files that travel with it", dir))
		return view
	}

	running := map[string]bool{}
	machine, found := machineOf(p.latest(), host)
	if found {
		for _, status := range machine.Services {
			running[status.Name] = true
		}
	}
	view.Notes = append(view.Notes, fmt.Sprintf("the tree at %s describes:", dir))
	for _, declaration := range declarations {
		name := declaration.Service.Name
		label := fmt.Sprintf("deploy %s (%s)", name, declaration.Service.Image)
		if running[name] {
			label += " — already here; deploying replaces it"
		}
		view.Choices = append(view.Choices, buttonView{
			Label: label,
			Icon:  "box-arrow-up",
			Act:   strings.Join([]string{"confirm", "deploy", host, name}, "/"),
		})
	}
	return view
}

// What the machine is already reporting about its services, one line each —
// the "image is not on this machine" that explains a deploy with nothing
// running.
func (p *panel) troubles(ctx context.Context, client *api.Client) []string {
	var statuses []supervisor.Status
	err := ask(ctx, client, api.Request{Op: api.OpStatus}, &statuses)
	if err != nil {
		return nil
	}
	var out []string
	for _, status := range statuses {
		if status.LastError != "" {
			out = append(out, fmt.Sprintf("the machine reports: %s — %s", status.Name, status.LastError))
		}
	}
	return out
}

// prune is the image cleanup. The plan and the removal are one computation on
// the daemon — what the dialog lists without authorisation is exactly what the
// red button removes with it.
func (p *panel) prune(ctx context.Context, host string, destructive bool) dialogView {
	view := dialogView{
		Title:   "clean up images on " + host,
		Command: fmt.Sprintf("hostctl -host %s image prune", host),
	}
	client, err := p.actionClient(host)
	if err != nil {
		return failed(view, err)
	}
	resp, err := client.Do(ctx, api.Request{
		Op:               api.OpImagePrune,
		Keep:             api.DefaultImageKeep,
		AllowDestructive: destructive,
	})
	if err != nil {
		p.dropAction(host)
		return p.failedErr(view, host, err)
	}
	var plan api.ImagePrune
	decodeErr := decode(ctx, resp.Body, &plan)
	if resp.Failed() && decodeErr != nil {
		return p.failedResp(view, host, resp)
	}
	if decodeErr != nil {
		return failed(view, decodeErr)
	}

	view.Notes = append(view.Notes, fmt.Sprintf(
		"the newest %d version(s) of each image stay, and so does anything a container or a declaration is holding; what another system built here is never touched",
		plan.Keep))
	if len(plan.Remove) == 0 {
		view.Outcome = fmt.Sprintf("nothing to remove — %d image(s) of ours are held or within the %d kept", plan.Kept, plan.Keep)
		return view
	}

	view.GridHead = []string{"image", "size", "result"}
	var total float64
	for _, image := range plan.Remove {
		result := "would remove"
		switch {
		case image.Problem != "":
			result = image.Problem
		case image.Removed:
			result = "removed"
		}
		total += image.Bytes
		view.Grid = append(view.Grid, []string{tagsText(image.Tags), formatBytes(image.Bytes), result})
	}
	if plan.Applied {
		p.wake()
		view.Outcome = fmt.Sprintf("%d image(s) removed, %s freed; %d kept", len(plan.Remove), formatBytes(total), plan.Kept)
		if resp.Failed() {
			view = failed(view, resp.Err())
		}
		return view
	}
	view.Danger = true
	view.Confirm = buttonView{
		Label: fmt.Sprintf("remove %d image(s), freeing %s", len(plan.Remove), formatBytes(total)),
		Act:   "run/prune/" + host,
	}
	return view
}

// confirmUpdate says which hostd the machine runs and which one this window
// carries, and what the install rite does, before the button that does it.
func (p *panel) confirmUpdate(ctx context.Context, host string) dialogView {
	view := dialogView{
		Title:   "update hostd on " + host,
		Command: fmt.Sprintf("hostctl -host %s install", host),
	}
	carried := daemon.Version()
	if carried == "" {
		return failed(view, fmt.Errorf(
			"this build of hostctl carries no daemon to install; a released hostctl always does — from a source tree, run make dist first"))
	}
	running := "a version it could not say"
	client, err := p.actionClient(host)
	if err == nil {
		var described api.Description
		askErr := ask(ctx, client, api.Request{Op: api.OpDescribe}, &described)
		if askErr == nil && described.Version != "" {
			running = described.Version
		}
	}
	view.Notes = append(view.Notes,
		fmt.Sprintf("%s runs hostd %s; this window carries hostd %s", host, running, carried),
		"the daemon is replaced and restarted; the containers it supervises keep running through it — the install checks that they did, and puts the previous daemon back by itself if they did not")
	view.Confirm = buttonView{Label: "update hostd", Act: "run/update/" + host}
	return view
}

// runUpdate is the same install the command line runs, report included. The
// person clicking may never have opened a terminal, but they are the operator:
// having the permission to run everything is the premise of this panel.
func (p *panel) runUpdate(ctx context.Context, host string) dialogView {
	view := dialogView{
		Title:   "update hostd on " + host,
		Command: fmt.Sprintf("hostctl -host %s install", host),
	}
	opt := p.opt
	opt.host = host
	var report bytes.Buffer
	opt.out = &report
	_, err := runInstall(ctx, opt, nil)
	if err != nil {
		for line := range strings.Lines(report.String()) {
			if strings.TrimSpace(line) != "" {
				view.Notes = append(view.Notes, strings.TrimSpace(line))
			}
		}
		return failed(view, err)
	}
	// The daemon this window was talking to is gone; both pipes follow it.
	p.drop(host)
	p.dropAction(host)
	p.wake()
	view.Outcome = updateOutcome(report.String(), host)
	return view
}

// The install reports "host;arch;version;transition;kept" for machines; said
// back in words for the person who clicked.
func updateOutcome(report, host string) string {
	lines := strings.Split(strings.TrimSpace(report), "\n")
	fields := strings.Split(lines[len(lines)-1], ";")
	if len(fields) == 5 {
		return fmt.Sprintf("%s now runs hostd %s (%s); %s", host, fields[2], fields[3], fields[4])
	}
	return host + " updated"
}
