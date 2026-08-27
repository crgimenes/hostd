package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/service"
)

type ErrUnknownService struct{ Name string }

func (e ErrUnknownService) Error() string {
	return fmt.Sprintf("no service named %q is declared; list what exists with hostctl service list", e.Name)
}

// Status asks the runtime what it is holding and reads it against what the
// files declare. There is no third answer kept anywhere: a status that came
// from hostd's own memory would be a status that can be wrong.
func (s *Supervisor) Status() []Status {
	ctx, cancel := runtimeContext()
	defer cancel()

	declared := s.declarations()
	out := make([]Status, 0, len(declared))
	running := map[string]held{}
	live := map[string]int{}
	found, err := s.containers(ctx)
	problem := ""
	if err != nil {
		problem = err.Error()
	}
	for _, container := range found {
		running[container.Service] = container
		if container.Running {
			live[container.Service]++
		}
	}

	for name, svc := range declared {
		status := Status{
			Name:      name,
			Kind:      svc.Kind,
			Desired:   svc.State,
			Image:     svc.Image,
			State:     StateStopped,
			LastError: problem,
		}
		container, ok := running[name]
		if ok {
			s.describe(ctx, &status, svc, container)
		}
		if svc.IsJob() {
			// A job is not a thing that should be up: what says it is healthy
			// is that its runs happen, which the timeline records.
			status.Runs = live[name]
			status.Every = svc.Every
			status.State = StateScheduled
			status.LastError = problem
			if status.Runs > 0 {
				status.State = StateRunning
			}
			if status.Runs == 0 {
				// Between runs there is no process and no uptime; the numbers
				// of the run that ended would read as a run that is going.
				status.PID = 0
				status.Since = 0
			}
			out = append(out, status)
			continue
		}
		if !ok && svc.WantRunning() && problem == "" {
			status.State = StateFailed
			status.LastError = "no container exists for this service"
		}
		out = append(out, status)
	}

	// A container nobody declares is still on this machine, and a status that
	// hid it would be a status that lies by omission.
	for name, container := range running {
		_, declaredStill := declared[name]
		if declaredStill {
			continue
		}
		status := Status{Name: name, Kind: service.KindContainer, Orphan: true, State: StateStopped}
		s.describe(ctx, &status, service.Service{}, container)
		out = append(out, status)
	}

	slices.SortFunc(out, func(a, b Status) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// describe fills in what only the runtime knows: whether it is running, since
// when, how many times it came back, and what it said when it last stopped.
func (s *Supervisor) describe(ctx context.Context, status *Status, svc service.Service, container held) {
	client := s.client()
	if client == nil {
		return
	}
	observed, err := client.Inspect(ctx, container.ID)
	if err != nil {
		status.LastError = err.Error()
		return
	}
	status.PID = observed.PID
	status.Digest = observed.Digest
	status.Restarts = observed.Restarts
	status.LastExit = observed.Exit
	if !observed.Started.IsZero() {
		status.Since = float64(observed.Started.UnixMilli())
	}
	if status.Image == "" {
		status.Image = observed.Image
	}
	status.State = mapState(observed, svc)
	if status.State == StateFailed && observed.Error != "" {
		status.LastError = observed.Error
	}
}

// The runtime's words for what a container is doing, in hostd's.
//
// Nothing here remembers that an operator stopped a service, and nothing needs
// to: under a policy that keeps containers alive, one that is exited and not
// coming back is one a hand stopped, because the runtime would have restarted
// anything else. That inference is the runtime's own behaviour, so it survives
// a restart of hostd and of the machine without a file of ours.
func mapState(observed docker.Container, svc service.Service) string {
	switch observed.Status {
	case "running":
		return StateRunning
	case "restarting":
		return StateStarting
	case "paused", "dead":
		return StateFailed
	case "created":
		if svc.WantRunning() {
			return StateStarting
		}
		return StateStopped
	}
	keepsAlive := observed.Restart == "always" || observed.Restart == "unless-stopped"
	if keepsAlive {
		return StateStopped
	}
	// It was left to end, and it ended badly.
	if observed.Exit != 0 {
		return StateFailed
	}
	return StateStopped
}

func (s *Supervisor) StatusOf(name string) (Status, error) {
	for _, status := range s.Status() {
		if status.Name == name {
			return status, nil
		}
	}
	return Status{}, ErrUnknownService{Name: name}
}

// Start asks the runtime to run what is declared, creating the container if
// there is none. The bool says whether this ask moved anything: starting a
// service that is already running is accepted and changes nothing.
func (s *Supervisor) Start(name string) (bool, error) {
	svc, ok := s.declaration(name)
	if !ok {
		return false, ErrUnknownService{Name: name}
	}
	ctx, cancel := runtimeContext()
	defer cancel()

	client := s.client()
	if client == nil {
		return false, docker.ErrNoRuntime
	}
	observed, err := client.Inspect(ctx, containerName(name))
	if errors.Is(err, docker.ErrNotFound) {
		// Nothing to start: the declaration has never been built here.
		return true, s.create(ctx, svc, true)
	}
	if err != nil {
		return false, err
	}
	if observed.Running {
		return false, nil
	}
	err = client.Start(ctx, observed.ID)
	if err != nil {
		return false, err
	}
	s.event(logs.EventStarted, name, fmt.Sprintf("started container %s", short(observed.ID)))
	s.nudge()
	return true, nil
}

// Stop leaves the container in place, stopped. The runtime remembers that a
// hand stopped it, so it stays stopped through a reboot instead of coming back
// because a policy said so.
func (s *Supervisor) Stop(name string) (bool, error) {
	_, ok := s.declaration(name)
	if !ok {
		// An orphan can be stopped too: it is on this machine, and refusing to
		// touch it would leave the operator with ssh as the only way.
		_, orphan := s.heldBy(name)
		if !orphan {
			return false, ErrUnknownService{Name: name}
		}
	}
	svc, _ := s.declaration(name)
	client := s.client()
	if client == nil {
		return false, docker.ErrNoRuntime
	}
	ctx, cancel := context.WithTimeout(context.Background(), svc.StopGrace()+runtimeTimeout)
	defer cancel()

	observed, err := client.Inspect(ctx, containerName(name))
	if errors.Is(err, docker.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !observed.Running {
		return false, nil
	}
	// The runtime asks, waits and kills what did not go; the grace is the
	// service's own, so a database gets the patience it was declared with —
	// and the wait is said before it is waited out.
	s.sayStopping(svc)
	err = client.Stop(ctx, observed.ID, svc.StopGrace())
	if err != nil {
		return false, err
	}
	s.event(logs.EventStopped, name, fmt.Sprintf("stopped container %s", short(observed.ID)))
	s.nudge()
	return true, nil
}

// Deploy takes the declaration as the machine now holds it, retires whatever
// container stands, and builds a fresh one — which resolves the image tag as
// it stands NOW. Restart brings back the same container on the same image;
// this is the explicit "run the version the tag means today". The declaration
// is upserted, never looked up: what was just put on this machine deploys,
// whether or not this supervisor had met it before.
func (s *Supervisor) Deploy(svc service.Service) (bool, error) {
	client := s.client()
	if client == nil {
		return false, docker.ErrNoRuntime
	}
	ctx, cancel := context.WithTimeout(context.Background(), svc.StopGrace()+2*runtimeTimeout)
	defer cancel()
	// Between the retire and the create there is no container, and a drift
	// round reading that instant would create a second one; the mutex keeps
	// the round out, exactly as it keeps it out of an apply.
	s.converge.Lock()
	defer s.converge.Unlock()
	s.mu.Lock()
	s.declared[svc.Name] = svc
	s.mu.Unlock()
	s.retire(ctx, svc)
	if svc.IsJob() {
		// A job has no standing container: the declaration is in place and the
		// next run starts from it.
		s.nudge()
		return true, nil
	}
	err := s.create(ctx, svc, svc.WantRunning())
	if err != nil {
		return false, err
	}
	s.nudge()
	return true, nil
}

// Remove takes the service off this machine: the container goes, and the
// declaration leaves the supervisor so the drift round does not bring it
// back. The description still exists in the operator's tree — a deploy puts
// the service back — which is what makes this removal ordinary rather than
// destructive to anything that cannot be rebuilt. Volumes stay: data is never
// a removal's side effect.
func (s *Supervisor) Remove(name string) (bool, error) {
	svc, ok := s.declaration(name)
	if !ok {
		// An orphan can be removed too: it is on this machine, and refusing
		// would leave ssh as the only way.
		_, orphan := s.heldBy(name)
		if !orphan {
			return false, ErrUnknownService{Name: name}
		}
		svc = service.Service{Name: name}
	}
	client := s.client()
	if client == nil {
		return false, docker.ErrNoRuntime
	}
	ctx, cancel := context.WithTimeout(context.Background(), svc.StopGrace()+2*runtimeTimeout)
	defer cancel()
	// The declaration goes FIRST, and that is what makes the converge mutex
	// unnecessary here: a drift round finding no declaration cannot recreate
	// what is being taken away, so nothing has to be locked out while the
	// container spends its grace period dying. Holding the mutex across that
	// wait blocked every other converge on the machine — two removals asked
	// for at once waited one behind the other, which is what "I clicked both
	// and they went a long time later" was.
	//
	// The order costs one visible failure mode instead: a daemon that dies in
	// between leaves a container no file declares, which the status reports as
	// an orphan. Visible beats blocking.
	s.mu.Lock()
	delete(s.declared, name)
	s.mu.Unlock()
	s.retire(ctx, svc)
	s.event(logs.EventStopped, name, "removed from this machine; the tree still describes it, and a deploy puts it back")
	s.nudge()
	return true, nil
}

// Always a change: it interrupts what is running, or starts what is not.
func (s *Supervisor) Restart(name string) (bool, error) {
	_, err := s.Stop(name)
	if err != nil {
		return false, err
	}
	_, err = s.Start(name)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Supervisor) heldBy(name string) (held, bool) {
	ctx, cancel := runtimeContext()
	defer cancel()
	found, err := s.containers(ctx)
	if err != nil {
		return held{}, false
	}
	for _, container := range found {
		if container.Service == name {
			return container, true
		}
	}
	return held{}, false
}

// Apply computes the same plan Plan does and carries it out. A service whose
// definition did not change keeps its container: applying configuration must
// not restart the machine's work for nothing.
func (s *Supervisor) Apply(declared []service.Service) []Change {
	// Excludes the drift round for the whole apply, so nothing this takes away
	// is brought back by a round that read the declarations a moment earlier.
	// Stop below does not take this lock, which is what keeps the two from
	// meeting each other here.
	s.converge.Lock()
	defer s.converge.Unlock()

	changes := s.Plan(declared)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	wanted := make(map[string]service.Service, len(declared))
	for _, svc := range declared {
		wanted[svc.Name] = svc
	}

	for _, change := range changes {
		switch change.Action {
		case ActionRemove:
			_, err := s.Stop(change.Service)
			if err != nil {
				s.event(logs.EventProblem, change.Service, fmt.Sprintf("could not stop it to remove it: %v", err))
				continue
			}
			client := s.client()
			if client != nil {
				// The container goes; its named storage stays, because
				// deleting somebody's data is not part of applying a file.
				_ = client.Remove(ctx, containerName(change.Service))
			}
			s.mu.Lock()
			delete(s.declared, change.Service)
			s.mu.Unlock()
		case ActionUpdate, ActionAdd:
			svc := wanted[change.Service]
			s.mu.Lock()
			s.declared[change.Service] = svc
			s.mu.Unlock()
			if svc.IsJob() {
				// Declaring a job is not running it: the schedule says when.
				// The container of the same name is from when this was a
				// service, and leaving it would be leaving something running
				// that nothing declares any more.
				s.retire(ctx, svc)
				continue
			}
			err := s.create(ctx, svc, svc.WantRunning())
			if err != nil {
				s.event(logs.EventProblem, change.Service, err.Error())
			}
		}
	}
	// Whatever else the files say is now what this machine believes, including
	// services that did not change.
	s.mu.Lock()
	for _, svc := range declared {
		s.declared[svc.Name] = svc
	}
	s.mu.Unlock()

	s.event(logs.EventApplied, "hostd", fmt.Sprintf("applied configuration: %d change(s)", len(changes)))
	s.nudge()
	return changes
}

// The desired state is excluded on purpose: an operator who stopped a service
// by hand must not have it started again by an unrelated apply.
func sameDefinition(a, b service.Service) bool {
	if a.Kind != b.Kind || a.Dir != b.Dir || a.Restart != b.Restart || a.StopTimeout != b.StopTimeout {
		return false
	}
	// A container that names another image, another port, another volume or
	// another ceiling is another service: without this a deploy would leave
	// the old image running and report nothing to change.
	if a.Image != b.Image || a.Memory != b.Memory || a.CPUs != b.CPUs {
		return false
	}
	// Editing a file that travels with the declaration is editing the service:
	// without this an apply would say there is nothing to do and the container
	// would keep the configuration it was built with.
	if a.Config != b.Config || a.ConfigHash != b.ConfigHash {
		return false
	}
	// The schedule is part of what a job IS. The apply stores every
	// declaration at the end whether it changed or not, so the machine picks
	// these up regardless — what was missing was the plan SAYING so, and a
	// plan that answers "nothing to change" to an edit that changes when a job
	// runs, or whether a hung run is ever stopped, is a dry run nobody should
	// trust twice.
	if a.Every != b.Every || a.Overlap != b.Overlap ||
		a.MaxParallel != b.MaxParallel || a.RunTimeout != b.RunTimeout {
		return false
	}
	return slices.Equal(a.Args, b.Args) && slices.Equal(a.Env, b.Env) &&
		slices.Equal(a.Ports, b.Ports) && slices.Equal(a.Volumes, b.Volumes)
}
