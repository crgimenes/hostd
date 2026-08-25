package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/service"
)

// The label carrying which run of a job a container is. Several runs of one job
// exist at the same time on purpose, so the service label alone does not
// identify a container.
const labelRun = "hostd.run"

// How often the scheduler looks at the clock. It bounds how late a run can be,
// so it has to be well under the shortest interval a job may declare.
const scheduleTick = 250 * time.Millisecond

// A run is named for the instant it was scheduled for, not for when it
// started: two machines reading the same declaration would name the same run
// the same way, and a late start does not rename it.
func runName(service string, slot time.Time) string {
	return containerName(service) + "-" + strconv.FormatInt(slot.UnixMilli(), 10)
}

func runID(slot time.Time) string { return strconv.FormatInt(slot.UnixMilli(), 10) }

// Aligned to the wall clock, the way cron is: a job that runs every two
// minutes runs at even minutes, and restarting the daemon does not shift its
// schedule.
func slotOf(now time.Time, every time.Duration) time.Time {
	return now.Truncate(every)
}

// schedule fires jobs. It is separate from the drift round because it is about
// the clock rather than about the machine: a job every second cannot wait
// fifteen seconds to be noticed.
func (s *Supervisor) schedule(ctx context.Context) {
	ticker := time.NewTicker(scheduleTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.fireDue(ctx, s.now())
	}
}

func (s *Supervisor) fireDue(ctx context.Context, now time.Time) {
	if s.client() == nil {
		return
	}
	for name, svc := range s.declarations() {
		if !svc.IsJob() || !svc.WantRunning() {
			continue
		}
		every := svc.Interval()
		if every <= 0 {
			continue
		}
		slot := slotOf(now, every)

		s.mu.Lock()
		last, seen := s.fired[name]
		if !seen {
			// First sight of a job is not a reason to run it: the schedule
			// says when, and "when hostd started" is not one of the times.
			s.fired[name] = slot
			s.mu.Unlock()
			continue
		}
		if !slot.After(last) {
			s.mu.Unlock()
			continue
		}
		s.fired[name] = slot
		s.mu.Unlock()

		_, _ = s.startRun(ctx, svc, slot, "a run was due")
	}
}

// RunNow starts one run of a job because somebody asked, not because the clock
// did. It answers with the run's id: a job may take an hour, and holding the
// connection until it ends would be a command that times out on exactly the
// jobs worth watching. What it printed is then in the same timeline as every
// scheduled run, under the same -run filter.
//
// The run is named for the instant it was asked for rather than for a schedule
// slot. Two runs must never share a name, and truncating to the slot would
// collide with the scheduled run of that same slot.
//
// What the declaration says about overlap and the ceiling still holds. A hand
// asking is not a reason to run more copies than the file allows: the file is
// where that number is decided, and a command that overruled it would be the
// tool overruling the tree.
func (s *Supervisor) RunNow(name string) (string, error) {
	svc, declared := s.declaration(name)
	if !declared {
		return "", ErrUnknownService{Name: name}
	}
	if !svc.IsJob() {
		return "", ErrNotAJob{Name: name}
	}
	if s.client() == nil {
		return "", docker.ErrNoRuntime
	}
	ctx, cancel := runtimeContext()
	defer cancel()
	return s.startRun(ctx, svc, s.now(), "a run was asked for")
}

// Asking a container for a run is asking the wrong thing of the right service,
// which is the caller's mistake and not the machine's failure. The refusal
// names the operation that IS the right one: "this is not a job" on its own is
// a dead end.
type ErrNotAJob struct{ Name string }

func (e ErrNotAJob) Error() string {
	return fmt.Sprintf("%s is a container and has no runs; start it with hostctl service start %s", e.Name, e.Name)
}

// Why a run did not begin. Nothing was changed and the caller has to look
// before asking again, which is a different outcome from a failure: a job at
// its ceiling is a job doing what its declaration says.
type ErrRunRefused struct{ Reason string }

func (e ErrRunRefused) Error() string { return e.Reason }

// startRun begins one run of a job, unless what is already running says not to.
// Nothing here coordinates the runs with each other: they share the work
// through whatever they read from, and a scheduler that tried to be clever
// about it would be a second opinion about somebody else's queue.
func (s *Supervisor) startRun(ctx context.Context, svc service.Service, slot time.Time, because string) (string, error) {
	live, err := s.runsOf(ctx, svc.Name)
	if err != nil {
		s.reportOnce(svc.Name, fmt.Sprintf("cannot tell what %s is already running: %v", svc.Name, err))
		return "", err
	}
	if svc.Overlap == service.OverlapSkip && len(live) > 0 {
		return "", s.refuseRun(svc.Name, fmt.Sprintf(
			"%s and the previous one is still going; this service is declared %q", because, service.OverlapSkip))
	}
	// Losing a turn is acceptable; losing it in silence is not. The ceiling
	// exists because a job whose runs stop finishing would start one every
	// turn until the machine died.
	if len(live) >= svc.Parallel() {
		return "", s.refuseRun(svc.Name, fmt.Sprintf(
			"%s and %d are already going, which is the ceiling; raise max-parallel or find out why they are not finishing",
			because, len(live)))
	}

	id, err := s.createRun(ctx, svc, slot)
	if err != nil {
		s.reportOnce(svc.Name, fmt.Sprintf("cannot start a run of %s: %v", svc.Name, err))
		return "", err
	}
	s.clearReport(svc.Name)
	s.logRun(svc.Name, runID(slot), logs.EventJobStarted, fmt.Sprintf("run %s started as container %s", runID(slot), short(id)))
	// The reader starts with the run, not at the next drift round: a run that
	// lasts a second would otherwise be gone before anybody read what it said,
	// and what a job printed is most of why anybody looks at a job.
	s.startFollowing(held{
		ID: id, Name: runName(svc.Name, slot), Running: true,
		Service: svc.Name,
	})
	s.mu.Lock()
	s.awaiting[id] = true
	s.mu.Unlock()
	go s.awaitRun(svc.Name, runID(slot), id, s.now())
	return runID(slot), nil
}

// A turn not taken belongs in the timeline whoever passed it up: the operator
// who asked reads the answer, and the one reading the log a week later reads
// the same sentence.
func (s *Supervisor) refuseRun(name, reason string) error {
	s.event(logs.EventJobSkipped, name, reason)
	return ErrRunRefused{Reason: reason}
}

// killAfter is how long a run has left before its limit.
//
// Measured from when the container STARTED, never from when this daemon began
// watching it. A run adopted from a previous daemon has already spent part of
// its allowance, and restarting the clock on every upgrade would hand a hung
// run a fresh hour each time hostd is replaced — which is the one case a
// timeout exists for. A run already past its limit gets zero and is stopped at
// once.
func killAfter(started, now time.Time, limit time.Duration) time.Duration {
	left := limit - now.Sub(started)
	if left < 0 {
		return 0
	}
	return left
}

// The limit as the declaration reads when the run is picked up — at its start,
// or when a later daemon adopts it. An edit made mid-run reaches the run after
// it, not this one, which is the same way `every` behaves. A run whose service
// is no longer declared keeps no bound, which is the same answer the ceiling
// gives it.
func (s *Supervisor) runLimitOf(name string) time.Duration {
	svc, found := s.declarations()[name]
	if !found {
		return 0
	}
	return svc.RunLimit()
}

// stopOverrun ends a run that passed its limit. It says so before stopping:
// the exit code that follows is whatever the kill produced, and on its own it
// would read as the job failing rather than as this deciding it had had long
// enough.
func (s *Supervisor) stopOverrun(name, run, id string, limit time.Duration) {
	client := s.client()
	if client == nil {
		return
	}
	s.logRun(name, run, logs.EventJobOverran, fmt.Sprintf(
		"run %s passed its run-timeout of %s and is being stopped", run, limit))
	ctx, cancel := runtimeContext()
	defer cancel()
	// The service's own grace, the same one an operator gets when they stop it
	// by hand: a job that cleans up after itself is given the chance to.
	grace := service.DefaultStopTimeout
	svc, found := s.declarations()[name]
	if found {
		grace = svc.StopGrace()
	}
	err := client.Stop(ctx, id, grace)
	if err != nil {
		s.reportOnce(name, fmt.Sprintf("run %s passed its run-timeout and could not be stopped: %v", run, err))
	}
}

// awaitRun records what a run did. The container is kept until its output has
// been read: removing it first would take the log with it, and a job whose
// output disappears when it fails is a job nobody can debug.
func (s *Supervisor) awaitRun(name, run, id string, started time.Time) {
	defer func() {
		s.mu.Lock()
		delete(s.awaiting, id)
		s.mu.Unlock()
	}()
	client := s.client()
	if client == nil {
		return
	}
	ctx, cancel := s.streamContext()
	defer cancel()

	// A run that hangs would otherwise hold its place for ever: the ceiling
	// fills with runs that will never finish, every turn after that is skipped,
	// and the job quietly stops happening while the service still reads as
	// scheduled. Nothing else in the system notices that.
	limit := s.runLimitOf(name)
	if limit > 0 {
		timer := time.AfterFunc(killAfter(started, s.now(), limit), func() {
			s.stopOverrun(name, run, id, limit)
		})
		defer timer.Stop()
	}

	code, err := client.Wait(ctx, id)
	if errors.Is(err, context.Canceled) || s.leaving() {
		// The daemon is going, not the run: it keeps going under the runtime,
		// and the next daemon picks it up. Saying it ended here would be
		// writing down something that did not happen.
		return
	}
	if err != nil {
		s.logRun(name, run, logs.EventJobFinished, fmt.Sprintf("run %s ended and could not be read: %v", run, err))
		return
	}
	// The reader is still draining what the container wrote as it ended.
	time.Sleep(500 * time.Millisecond)
	took := s.now().Sub(started).Truncate(time.Millisecond)
	s.logRun(name, run, logs.EventJobFinished, fmt.Sprintf("run %s finished with exit %d after %s", run, code, took))

	removeCtx, removeCancel := runtimeContext()
	defer removeCancel()
	_ = client.Remove(removeCtx, id)
}

func (s *Supervisor) leaving() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// adoptRuns takes over the runs nobody is waiting on: the ones a previous
// daemon started, and the ones whose waiter left with it. Without this a run
// that outlived an upgrade would never be recorded and its container would
// stay for ever.
func (s *Supervisor) adoptRuns(ctx context.Context, running []held) {
	client := s.client()
	if client == nil {
		return
	}
	for _, container := range running {
		run := container.Labels[labelRun]
		if run == "" {
			continue
		}
		s.mu.Lock()
		awaited := s.awaiting[container.ID]
		if container.Running && !awaited {
			s.awaiting[container.ID] = true
		}
		s.mu.Unlock()
		if awaited {
			continue
		}
		if container.Running {
			observed, err := client.Inspect(ctx, container.ID)
			started := s.now()
			if err == nil && !observed.Started.IsZero() {
				started = observed.Started
			}
			go s.awaitRun(container.Service, run, container.ID, started)
			continue
		}
		// It ended while nobody was watching, which is what an upgrade in the
		// middle of a run looks like. The runtime still knows how it went.
		observed, err := client.Inspect(ctx, container.ID)
		if err == nil {
			s.logRun(container.Service, run, logs.EventJobFinished,
				fmt.Sprintf("run %s finished with exit %d while no daemon was watching", run, observed.Exit))
		}
		_ = client.Remove(ctx, container.ID)
	}
}

func (s *Supervisor) logRun(service, run, kind, text string) {
	s.log.Append(logs.Record{
		Time:    s.now(),
		Service: service,
		Stream:  logs.StreamEvent,
		Kind:    kind,
		Run:     run,
		Text:    text,
	})
}

// runsOf answers with the runs of a job that have not ended.
func (s *Supervisor) runsOf(ctx context.Context, name string) ([]held, error) {
	found, err := s.containers(ctx)
	if err != nil {
		return nil, err
	}
	var out []held
	for _, container := range found {
		if container.Service == name && container.Running {
			out = append(out, container)
		}
	}
	return out, nil
}

// createRun builds the container for one run. It carries the same declaration
// a service does, minus what a job cannot have: the runtime never restarts it,
// because it ended by finishing.
func (s *Supervisor) createRun(ctx context.Context, svc service.Service, slot time.Time) (string, error) {
	client := s.client()
	mounts, err := s.prepareMounts(ctx, svc)
	if err != nil {
		return "", err
	}
	mounts = append(mounts, s.configMount(svc)...)
	err = client.EnsureNetwork(ctx, Network)
	if err != nil {
		return "", err
	}
	name := runName(svc.Name, slot)
	// A run of the same instant that somehow survived is not this one.
	err = client.Remove(ctx, name)
	if err != nil {
		return "", err
	}
	id, err := client.Create(ctx, docker.Spec{
		Name:   name,
		Image:  svc.Image,
		Args:   svc.Args,
		Env:    svc.Env,
		Dir:    svc.Dir,
		Mounts: mounts,
		Labels: map[string]string{
			labelService: svc.Name,
			labelRun:     runID(slot),
		},
		Memory:  int64(svc.Memory) * 1024 * 1024,
		NanoCPU: int64(svc.CPUs * 1e9),
		Network: Network,
		Alias:   svc.Name + "-" + runID(slot),
		Restart: "no",
	})
	if err != nil {
		return "", err
	}
	err = client.Start(ctx, id)
	if err != nil {
		_ = client.Remove(ctx, id)
		return "", err
	}
	return id, nil
}

// The run a container belongs to, from the name hostd gave it.
func runOf(container held) string {
	_, run, found := strings.Cut(strings.TrimPrefix(container.Name, containerName(container.Service)), "-")
	if !found {
		return ""
	}
	return run
}
