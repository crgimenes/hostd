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

		s.startRun(ctx, svc, slot)
	}
}

// startRun begins one run of a job, unless what is already running says not to.
// Nothing here coordinates the runs with each other: they share the work
// through whatever they read from, and a scheduler that tried to be clever
// about it would be a second opinion about somebody else's queue.
func (s *Supervisor) startRun(ctx context.Context, svc service.Service, slot time.Time) {
	live, err := s.runsOf(ctx, svc.Name)
	if err != nil {
		s.reportOnce(svc.Name, fmt.Sprintf("cannot tell what %s is already running: %v", svc.Name, err))
		return
	}
	if svc.Overlap == service.OverlapSkip && len(live) > 0 {
		s.event(logs.EventJobSkipped, svc.Name, fmt.Sprintf(
			"a run was due and the previous one is still going; this service is declared %q", service.OverlapSkip))
		return
	}
	// Losing a turn is acceptable; losing it in silence is not. The ceiling
	// exists because a job whose runs stop finishing would start one every
	// turn until the machine died.
	if len(live) >= svc.Parallel() {
		s.event(logs.EventJobSkipped, svc.Name, fmt.Sprintf(
			"a run was due and %d are already going, which is the ceiling; raise max-parallel or find out why they are not finishing",
			len(live)))
		return
	}

	id, err := s.createRun(ctx, svc, slot)
	if err != nil {
		s.reportOnce(svc.Name, fmt.Sprintf("cannot start a run of %s: %v", svc.Name, err))
		return
	}
	s.clearReport(svc.Name)
	s.logRun(svc.Name, runID(slot), logs.EventJobStarted, fmt.Sprintf("run %s started as container %s", runID(slot), short(id)))
	// The reader starts with the run, not at the next drift round: a run that
	// lasts a second would otherwise be gone before anybody read what it said,
	// and what a job printed is most of why anybody looks at a job.
	s.startFollowing(ctx, held{
		ID: id, Name: runName(svc.Name, slot), Running: true,
		Service: svc.Name,
	})
	s.mu.Lock()
	s.awaiting[id] = true
	s.mu.Unlock()
	go s.awaitRun(svc.Name, runID(slot), id, s.now())
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
