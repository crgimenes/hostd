package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/supervisor"
)

// How long a machine is given to bring the service up before the move is called
// a failure and the source is put back. Long enough for an image to be unpacked
// and a process to bind its port; short enough that a migration that is not
// going to work does not hold the operator all morning.
const migrateSettle = 45 * time.Second

// How often the destination is asked whether it is up yet.
const migratePoll = time.Second

// runMigrate makes the fleet agree with the tree about where one service lives.
//
// The tree is the intent and the operator writes it: they move the service by
// changing `hosts` in its declaration, the way they change anything else, and
// this reads that change and carries it out in an order that never leaves two
// copies running and never leaves none for longer than the start takes.
//
// Nothing here is a new capability. It is stop, put, apply and start — the
// operations any client has — placed in the one order that is safe, with the
// machine put back the way it was if the far end does not come up.
func runMigrate(ctx context.Context, opt options, args []string) (int, error) {
	if len(args) != 1 {
		return exitUsage, fmt.Errorf("service migrate needs the name of one service; the tree says where it goes")
	}
	name := args[0]

	declared, err := declarationOf(ctx, opt, name)
	if err != nil {
		return exitUsage, err
	}
	fleet, err := opt.fleet(ctx)
	if err != nil {
		return exitUsage, err
	}

	out := opt.out
	_, _ = fmt.Fprintf(out, "asking %d machine(s) where %s is\n", len(fleet), name)
	found := survey(ctx, opt, fleet, name, declared)

	move, code, err := planMove(name, found)
	if err != nil {
		return code, err
	}
	err = destinationCanHostIt(move.to, declared.Service)
	if err != nil {
		return exitFailed, err
	}
	// Asked before anything is stopped: a service taken down for a move that
	// then cannot find its image is downtime spent on nothing.
	carrying, err := imagePlan(ctx, opt, move, declared.Service)
	if err != nil {
		return exitFailed, err
	}
	describeMove(out, name, move, declared.Service)
	_, _ = fmt.Fprintf(out, "  %s\n", carrying.explain(move.to.host))

	if !opt.allowDestr {
		_, _ = fmt.Fprintln(out, "nothing was moved")
		_, _ = fmt.Fprintln(out, "run it again with -allow-destructive to carry it out")
		return exitRefused, nil
	}
	return carryOut(ctx, opt, name, declared, move, carrying)
}

// Whether the image the declaration names has to travel, and whether it can.
type imageMove struct {
	image string
	// Already on the destination, so the move costs nothing but the start.
	there bool
	// On the operator's own runtime, which is the only place it can be sent
	// from: a host fetches nothing by itself, by design.
	here bool
}

func (m imageMove) explain(destination string) string {
	if m.there {
		return fmt.Sprintf("%s already has %s", destination, m.image)
	}
	return fmt.Sprintf("%s will be sent from here to %s", m.image, destination)
}

// imagePlan refuses the one case that cannot work: an image the destination
// does not have and this machine cannot send. A registry would be the other
// answer, and the project deliberately does not have one.
func imagePlan(ctx context.Context, opt options, plan move, declared service.Service) (imageMove, error) {
	carrying := imageMove{image: declared.Image}
	if declared.Image == "" {
		return carrying, nil
	}
	to, err := connectTo(ctx, opt, plan.to.host)
	if err != nil {
		return carrying, err
	}
	defer func() { _ = to.Close() }()

	carrying.there, err = hasImage(ctx, to, declared.Image)
	if err != nil {
		return carrying, err
	}
	if carrying.there {
		return carrying, nil
	}
	carrying.here = imageIsLocal(ctx, declared.Image)
	if carrying.here {
		return carrying, nil
	}
	return carrying, fmt.Errorf(
		"%s runs %s, which %s does not have and this machine cannot send; build or load it here first, or push it with hostctl -host %s image push %s",
		declared.Name, declared.Image, plan.to.host, plan.to.host, declared.Image)
}

// hasImage asks the machine what it holds. A tag and a digest are both ways a
// declaration names an image, so both are matched.
func hasImage(ctx context.Context, client *api.Client, image string) (bool, error) {
	resp, err := client.Do(ctx, api.Request{Op: api.OpImageList})
	if err != nil {
		return false, err
	}
	if resp.Failed() {
		// A daemon too old to list images cannot be asked. Saying so beats
		// guessing, because the guess decides whether a service is stopped.
		return false, fmt.Errorf("cannot tell whether the image is there: %w", resp.Err())
	}
	var held []api.ImageEntry
	err = decode(ctx, resp.Body, &held)
	if err != nil {
		return false, err
	}
	for _, entry := range held {
		if entry.Digest == image || slices.Contains(entry.Tags, image) {
			return true, nil
		}
	}
	return false, nil
}

func imageIsLocal(ctx context.Context, image string) bool {
	local, err := docker.Open()
	if err != nil {
		return false
	}
	_, err = local.Image(ctx, image)
	return err == nil
}

// Where a service is, as one machine answered.
type placement struct {
	host string
	// Answered at all. A machine that did not is not a machine that is empty,
	// and a migration that assumed so could start a second copy of a service
	// that is still running somewhere.
	answered bool
	problem  string
	wanted   bool
	status   supervisor.Status
	holds    bool
	describe api.Description
}

func declarationOf(ctx context.Context, opt options, name string) (service.Declaration, error) {
	declarations, loadErr := service.LoadTree(ctx, opt.config)
	if loadErr != nil {
		// A tree that cannot be read in full cannot say where anything lives:
		// the file that failed to parse may be the one being moved.
		return service.Declaration{}, loadErr
	}
	for _, declaration := range declarations {
		if declaration.Service.Name == name {
			return declaration, nil
		}
	}
	return service.Declaration{}, fmt.Errorf("%s is not declared in %s; the tree is what says where a service lives", name, opt.config)
}

// survey asks every machine at once. One that is switched off must not hold up
// the others, and must be reported rather than read as an absence.
func survey(ctx context.Context, opt options, fleet []string, name string, declared service.Declaration) []placement {
	found := make([]placement, len(fleet))
	slots := make(chan struct{}, fleetConcurrency)
	var wg sync.WaitGroup
	for i, host := range fleet {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			entry := opt.entry(ctx, host)
			one := placement{
				host:   host,
				wanted: declared.Service.BelongsTo(entry.Name, entry.Tags),
			}
			client, err := connectTo(ctx, opt, host)
			if err != nil {
				one.problem = err.Error()
				found[i] = one
				return
			}
			defer func() { _ = client.Close() }()
			one.answered = true

			resp, err := client.Do(ctx, api.Request{Op: api.OpStatus})
			if err != nil || resp.Failed() {
				one.answered = false
				one.problem = firstProblem(err, resp)
				found[i] = one
				return
			}
			var statuses []supervisor.Status
			err = decode(ctx, resp.Body, &statuses)
			if err == nil {
				for _, status := range statuses {
					if status.Name != name {
						continue
					}
					one.holds = true
					one.status = status
				}
			}
			answer, err := client.Do(ctx, api.Request{Op: api.OpDescribe})
			if err == nil && !answer.Failed() {
				_ = decode(ctx, answer.Body, &one.describe)
			}
			found[i] = one
		})
	}
	wg.Wait()
	return found
}

func firstProblem(err error, resp api.Response) string {
	if err != nil {
		return err.Error()
	}
	return resp.Err().Error()
}

func connectTo(ctx context.Context, opt options, host string) (*api.Client, error) {
	one := opt
	one.host = host
	return connect(ctx, one)
}

// What the move is: one machine losing the service and one gaining it.
type move struct {
	from placement
	to   placement
}

// planMove reads the survey and refuses everything that is not a migration.
// Each refusal names the operation that IS the right one, because "this is not
// a migration" without that is a dead end.
func planMove(name string, found []placement) (move, int, error) {
	var wants, holds, silent []placement
	for _, one := range found {
		if !one.answered {
			silent = append(silent, one)
			continue
		}
		if one.wanted {
			wants = append(wants, one)
		}
		if one.holds && !one.wanted {
			holds = append(holds, one)
		}
	}
	// A machine that did not answer may be the one running it. Moving on that
	// picture risks a second copy of a service that never stopped.
	if len(silent) > 0 {
		// Not a bad request: the fleet could not be read. An agent that reads
		// this as bad arguments never retries, when retrying once the machine
		// is back is exactly the right move.
		return move{}, exitComms, fmt.Errorf("%s did not answer, so where %s runs is not known; a migration decided on a partial picture can start a second copy",
			strings.Join(hostsOf(silent), ", "), name)
	}
	switch {
	case len(wants) == 0:
		return move{}, exitUsage, fmt.Errorf("the tree places %s on no machine; removing a service is push and apply -allow-destructive, not a migration", name)
	case len(wants) > 1:
		return move{}, exitUsage, fmt.Errorf("the tree places %s on %s; a service on several machines is not something to migrate, it is already where it belongs",
			name, strings.Join(hostsOf(wants), ", "))
	case len(holds) == 0:
		return move{}, exitUsage, fmt.Errorf("no machine outside %s is running %s; putting it there for the first time is push and apply",
			wants[0].host, name)
	case len(holds) > 1:
		return move{}, exitUsage, fmt.Errorf("%s is on %s at once; sort that out before moving it, because this would have to choose which copy is the real one",
			name, strings.Join(hostsOf(holds), ", "))
	}
	return move{from: holds[0], to: wants[0]}, exitOK, nil
}

func hostsOf(found []placement) []string {
	out := make([]string, 0, len(found))
	for _, one := range found {
		out = append(out, one.host)
	}
	slices.Sort(out)
	return out
}

// destinationCanHostIt refuses before anything is stopped. A migration that
// finds out at the far end is a migration that took a service down to learn
// something it could have asked.
func destinationCanHostIt(to placement, declared service.Service) error {
	if to.describe.Arch == "" {
		return fmt.Errorf("%s has no container runtime, so it cannot run %s", to.host, declared.Name)
	}
	if declared.Memory <= 0 {
		return nil
	}
	want := declared.Memory * (1 << 20)
	// Zero means the machine did not say, not that it has none: refusing on a
	// number nobody gave would block a move for a missing sample.
	if to.describe.MemoryBytes > 0 && to.describe.MemoryBytes < want {
		return fmt.Errorf("%s declares %d MB and %s has %s in total",
			declared.Name, int64(declared.Memory), to.host, formatBytes(to.describe.MemoryBytes))
	}
	return nil
}

func describeMove(out io.Writer, name string, plan move, declared service.Service) {
	_, _ = fmt.Fprintf(out, "%s moves from %s to %s\n", name, plan.from.host, plan.to.host)
	_, _ = fmt.Fprintf(out, "  %s runs %s, %d cpu(s), %s\n",
		plan.to.host, runtimeText(plan.to.describe), plan.to.describe.CPUs, formatBytes(plan.to.describe.MemoryBytes))
	// Data does not travel. Saying which paths stay behind is the difference
	// between a warned operator and a lost database.
	staying := declared.Volumes
	if len(staying) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "  %d volume(s) do NOT travel and stay on %s:\n", len(staying), plan.from.host)
	for _, volume := range staying {
		_, _ = fmt.Fprintf(out, "    %s\n", volume)
	}
	_, _ = fmt.Fprintf(out, "  %s will start with empty storage; copy the data yourself if it matters\n", plan.to.host)
}

// carryOut is the order the whole command exists for.
//
// The image and the declaration go first, while the service is still up, so the
// time it is down is the start and not the transfer. Only then is the source
// stopped. If the destination does not come up, the source is started again and
// the tree is left to be reconciled by whoever reads the failure — never a
// fleet with the service nowhere.
func carryOut(ctx context.Context, opt options, name string, declared service.Declaration, plan move, carrying imageMove) (int, error) {
	out := opt.out
	to, err := connectTo(ctx, opt, plan.to.host)
	if err != nil {
		return exitComms, err
	}
	defer func() { _ = to.Close() }()
	from, err := connectTo(ctx, opt, plan.from.host)
	if err != nil {
		return exitComms, err
	}
	defer func() { _ = from.Close() }()

	// The image goes while the service is still up: hundreds of megabytes of
	// downtime is downtime spent on a transfer, not on a start.
	if !carrying.there && carrying.image != "" {
		_, _ = fmt.Fprintf(out, "sending %s to %s\n", carrying.image, plan.to.host)
		code, pushErr := runImagePush(ctx, to, opt, []string{carrying.image})
		if pushErr != nil {
			return code, pushErr
		}
	}

	_, _ = fmt.Fprintf(out, "sending the declaration to %s\n", plan.to.host)
	err = putDeclaration(ctx, to, opt, declared)
	if err != nil {
		return exitFailed, err
	}

	_, _ = fmt.Fprintf(out, "stopping %s on %s\n", name, plan.from.host)
	stopped, err := ensureStopped(ctx, opt, plan.from.host, name, from)
	if err != nil {
		return stopFailure(name, plan.from.host, stopped, err)
	}

	_, _ = fmt.Fprintf(out, "converging %s\n", plan.to.host)
	err = callOK(ctx, to, api.Request{
		Op:               api.OpApply,
		AllowDestructive: true,
		OnBehalfOf:       opt.onBehalfOf,
	})
	if err == nil {
		err = waitForService(ctx, to, name)
	}
	if err != nil {
		return putBack(ctx, opt, name, plan, from, err)
	}

	// Only now does the source lose it: until this point the declaration is
	// still there, which is what makes putting it back a start and not a deploy.
	_, _ = fmt.Fprintf(out, "removing %s from %s\n", name, plan.from.host)
	err = dropFromSource(ctx, opt, from, plan.from.host, name)
	if err != nil {
		// The service IS running at the destination. Saying this is a partial
		// success rather than a failure is what stops somebody moving it twice.
		_, _ = fmt.Fprintf(out, "%s is running on %s, but %s still holds it: %v\n", name, plan.to.host, plan.from.host, err)
		return exitPartial, nil
	}
	_, _ = fmt.Fprintf(out, "%s now runs on %s\n", name, plan.to.host)
	return exitOK, nil
}

// stopFailure reports the difference between a service that is down and one
// that is not. Both are failures of this command; only one leaves the fleet
// with the service stopped, and the operator has to be told which.
func stopFailure(name, host string, stopped bool, cause error) (int, error) {
	if stopped {
		return exitPartial, fmt.Errorf("%s is stopped on %s but the move did not finish (%w); run this again to carry on from here",
			name, host, cause)
	}
	return exitFailed, fmt.Errorf("%s was not stopped on %s, so nothing was moved: %w", name, host, cause)
}

// ensureStopped answers what the machine IS, not whether the request arrived.
// The ssh underneath can drop after the daemon has already acted, and a step
// read as "did not happen" when it did is how a move gets reported as never
// started while the service is down. The second return says whether it is down;
// the error says whether this command can carry on.
func ensureStopped(ctx context.Context, opt options, host, name string, from *api.Client) (bool, error) {
	err := callOK(ctx, from, api.Request{
		Op:         api.OpServiceStop,
		Name:       name,
		OnBehalfOf: opt.onBehalfOf,
	})
	if err == nil {
		return true, nil
	}
	// Its own connection: the one that just failed cannot be trusted to answer,
	// and what is being asked is the state, not the answer.
	fresh, dialErr := connectTo(ctx, opt, host)
	if dialErr != nil {
		return false, fmt.Errorf("%w (and %s could not be asked whether it took effect: %v)", err, host, dialErr)
	}
	defer func() { _ = fresh.Close() }()
	status, statusErr := serviceStatus(ctx, fresh, name)
	if statusErr != nil {
		return false, fmt.Errorf("%w (and its state could not be read: %v)", err, statusErr)
	}
	return status.State != supervisor.StateRunning, err
}

// putBack starts the service where it was. The failure that caused it is what
// the operator needs to read, so it is what comes back.
func putBack(ctx context.Context, opt options, name string, plan move, from *api.Client, cause error) (int, error) {
	_, _ = fmt.Fprintf(opt.out, "%s did not come up on %s; starting it again on %s\n", name, plan.to.host, plan.from.host)
	err := callOK(ctx, from, api.Request{
		Op:         api.OpServiceStart,
		Name:       name,
		OnBehalfOf: opt.onBehalfOf,
	})
	if err != nil {
		return exitFailed, fmt.Errorf("%s did not come up on %s (%v) AND could not be started again on %s (%w): it is running nowhere",
			name, plan.to.host, cause, plan.from.host, err)
	}
	return exitFailed, fmt.Errorf("%s did not come up on %s and is running on %s again: %w", name, plan.to.host, plan.from.host, cause)
}

// dropFromSource sends the source the tree as the tree now is — which no longer
// declares this service for it — and converges. The prune is what removes the
// declaration; the apply is what stops holding the container.
func dropFromSource(ctx context.Context, opt options, from *api.Client, host, name string) error {
	declarations, loadErr := service.LoadTree(ctx, opt.config)
	if loadErr != nil {
		return loadErr
	}
	machine := opt.entry(ctx, host)
	belongs := belongingTo(declarations, machine)
	for _, declaration := range belongs {
		if declaration.Service.Name == name {
			return fmt.Errorf("the tree still declares %s for %s", name, host)
		}
	}
	err := prune(ctx, from, opt, belongs, len(declarations))
	if err != nil {
		return err
	}
	return callOK(ctx, from, api.Request{
		Op:               api.OpApply,
		AllowDestructive: true,
		OnBehalfOf:       opt.onBehalfOf,
	})
}

// waitForService gives the destination time to bring it up. A job is up when it
// is scheduled: waiting for it to be running would wait for its next due time,
// which may be an hour away.
func waitForService(ctx context.Context, client *api.Client, name string) error {
	deadline := time.Now().Add(migrateSettle)
	var last string
	for {
		status, err := serviceStatus(ctx, client, name)
		switch {
		case err != nil:
			last = err.Error()
		case status.State == supervisor.StateRunning, status.State == supervisor.StateScheduled:
			return nil
		default:
			last = status.State
			if status.LastError != "" {
				last = status.State + ": " + status.LastError
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s it is %s", migrateSettle, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(migratePoll):
		}
	}
}

func serviceStatus(ctx context.Context, client *api.Client, name string) (supervisor.Status, error) {
	resp, err := client.Do(ctx, api.Request{Op: api.OpStatus})
	if err != nil {
		return supervisor.Status{}, err
	}
	if resp.Failed() {
		return supervisor.Status{}, resp.Err()
	}
	var statuses []supervisor.Status
	err = decode(ctx, resp.Body, &statuses)
	if err != nil {
		return supervisor.Status{}, err
	}
	for _, status := range statuses {
		if status.Name == name {
			return status, nil
		}
	}
	return supervisor.Status{}, fmt.Errorf("it is not declared there")
}

func callOK(ctx context.Context, client *api.Client, req api.Request) error {
	resp, err := client.Do(ctx, req)
	if err != nil {
		return err
	}
	if resp.Failed() {
		return resp.Err()
	}
	return nil
}
