package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/service"
)

// The label that says a container is hostd's, and which service it is. It is
// what makes adoption possible: after a restart the daemon asks the runtime
// what it already owns instead of trusting a file to still be true.
// The filter is the key alone: with a trailing "=" the runtime reads it as
// "this label, empty value" and matches nothing, which looks exactly like a
// machine running no containers.
const labelService = "hostd.service"

// The network every service of this host shares. A service reaches another by
// its own name on it, which is why an application needs no port published to
// the machine at all: the only thing that has to be reachable from outside is
// whatever answers the internet.
const Network = "hostd"

// Named storage carries the service in its name: two services asking for
// "data" are asking for their own, and sharing has to be deliberate rather
// than a collision.
func volumeName(service, volume string) string { return "hostd-" + service + "-" + volume }

// A container name derived from the service name, so an operator running
// docker ps sees which service a container belongs to without a lookup.
func containerName(service string) string { return "hostd-" + service }

// Talking to the runtime is a local socket call, but a runtime that is wedged
// must not hold the supervision loop.
const runtimeTimeout = 30 * time.Second

func runtimeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), runtimeTimeout)
}

// A stream that outlives the call that started it, but not the supervisor: a
// log follower still reading after the loop has left is a goroutine holding a
// connection nobody will read.
func (s *Supervisor) streamContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// Runtime hands the supervisor the container daemon to use. Without it a
// container service fails to start with a message saying so, which is the
// honest answer on a machine that runs no containers.
func (s *Supervisor) Runtime(client *docker.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = client
}

// startContainer creates and starts the container for p, under the same lock
// and the same rules as starting a process: it either ends with something
// running and recorded, or with an error and nothing left behind.
func (s *Supervisor) startContainer(p *proc, now time.Time) error {
	if s.runtime == nil {
		return fmt.Errorf("this machine has no container runtime; %w", docker.ErrNoRuntime)
	}
	ctx, cancel := runtimeContext()
	defer cancel()

	ports, err := p.svc.PublishedPorts()
	if err != nil {
		return err
	}
	mounts, err := s.prepareMounts(ctx, p.svc)
	if err != nil {
		return err
	}
	err = s.runtime.EnsureNetwork(ctx, Network)
	if err != nil {
		return err
	}
	// The tag in the file is a name that can be made to mean something else
	// tomorrow. What ran is recorded as the digest, so "the same image" stays
	// a checkable claim.
	digest, err := s.runtime.ImageDigest(ctx, p.svc.Image)
	if err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return fmt.Errorf("image %s is not on this machine; push it with hostctl before declaring it", p.svc.Image)
		}
		return err
	}

	// A container left over from a previous life holds the name; it is gone
	// for good, and keeping it would only make the create fail.
	name := containerName(p.svc.Name)
	_ = s.runtime.Remove(ctx, name)

	id, err := s.runtime.Create(ctx, docker.Spec{
		Name:    name,
		Image:   p.svc.Image,
		Args:    p.svc.Args,
		Env:     p.svc.Env,
		Dir:     p.svc.Dir,
		Ports:   toDockerPorts(ports),
		Mounts:  mounts,
		Labels:  map[string]string{labelService: p.svc.Name},
		Memory:  int64(p.svc.Memory) * 1024 * 1024,
		NanoCPU: int64(p.svc.CPUs * 1e9),
		Network: Network,
		Alias:   p.svc.Name,
	})
	if err != nil {
		return err
	}
	err = s.runtime.Start(ctx, id)
	if err != nil {
		// A container created and not started is a name taken for nothing.
		_ = s.runtime.Remove(ctx, id)
		return err
	}
	observed, err := s.runtime.Inspect(ctx, id)
	if err != nil {
		return err
	}

	p.container = id
	// The container's main process, so the metrics of a container service are
	// read the same way as any other service's.
	p.pid = observed.PID
	p.token = ""
	p.started = now
	p.adopted = false
	p.running = true
	p.stopping = false
	p.killed = false
	p.lastError = ""

	err = writeState(s.dirs.State, procState{
		Name:      p.svc.Name,
		PID:       p.pid,
		Container: id,
		StartedAt: float64(now.UnixMilli()),
	})
	if err != nil {
		s.event(logs.EventProblem, p.svc.Name, fmt.Sprintf("started container %s but could not record it: %v; a hostd restart will adopt it by label instead", short(id), err))
	}
	s.event(logs.EventStarted, p.svc.Name, fmt.Sprintf("started container %s from %s", short(id), short(digest)))
	go s.waitContainer(p.svc.Name, id)
	go s.followContainer(p.svc.Name, id, time.Time{})
	return nil
}

// Named storage is created if it is not there and never removed here: a
// service that goes away leaves its data behind, because deleting somebody's
// data is not a decision for a converge loop.
func (s *Supervisor) prepareMounts(ctx context.Context, svc service.Service) ([]docker.Mount, error) {
	declared, err := svc.Mounts()
	if err != nil {
		return nil, err
	}
	out := make([]docker.Mount, 0, len(declared))
	for _, mount := range declared {
		source := mount.Source
		if mount.Named {
			source = volumeName(svc.Name, mount.Source)
			err = s.runtime.EnsureVolume(ctx, source, map[string]string{labelService: svc.Name})
			if err != nil {
				return nil, err
			}
		}
		out = append(out, docker.Mount{
			Source:   source,
			Target:   mount.Target,
			ReadOnly: mount.ReadOnly,
			Named:    mount.Named,
		})
	}
	return out, nil
}

func toDockerPorts(ports []service.Port) []docker.Port {
	out := make([]docker.Port, 0, len(ports))
	for _, port := range ports {
		out = append(out, docker.Port{
			HostIP:        port.HostIP,
			HostPort:      port.HostPort,
			ContainerPort: port.ContainerPort,
			Protocol:      port.Protocol,
		})
	}
	return out
}

// waitContainer is the container's version of waiting on a child: it blocks
// until the runtime says the container ended, and only then records the death,
// so an exit is never noticed a tick late.
func (s *Supervisor) waitContainer(name, id string) {
	// No deadline: waiting is the whole operation. It ends when the container
	// does, when the runtime goes away, or when this daemon leaves.
	ctx, cancel := s.streamContext()
	defer cancel()
	code, err := s.runtime.Wait(ctx, id)
	if s.hasFinished() {
		return
	}
	// The last lines a container wrote explain the exit, and the follower is
	// still draining them; giving it a moment puts them before the event.
	time.Sleep(200 * time.Millisecond)

	s.mu.Lock()
	p, ok := s.procs[name]
	if s.finished || !ok || p.container != id {
		s.mu.Unlock()
		return
	}
	now := s.now()
	p.running = false
	p.pid = 0
	p.lastExit = code
	p.lastError = ""
	if err != nil && !p.stopping {
		p.lastError = err.Error()
	}
	switch {
	case p.stopping:
		s.event(logs.EventStopped, name, fmt.Sprintf("container stopped (exit %d)", code))
	case code == 0:
		s.event(logs.EventExited, name, "container exited normally (exit 0)")
	default:
		s.event(logs.EventExited, name, fmt.Sprintf("container exited with code %d", code))
	}
	container := p.container
	p.container = ""
	s.afterExit(p, now, code)
	s.mu.Unlock()

	// Outside the lock: the name has to be free before the next start, and a
	// removal is a call to the runtime.
	removeCtx, removeCancel := runtimeContext()
	defer removeCancel()
	_ = s.runtime.Remove(removeCtx, container)
	s.nudge()
}

// followContainer copies what the container writes into the same timeline as
// the events about it. The runtime keeps its own copy; this is the one an
// operator reads without entering the machine.
func (s *Supervisor) followContainer(name, id string, since time.Time) {
	ctx, cancel := s.streamContext()
	defer cancel()
	err := s.runtime.Logs(ctx, id, since, func(line docker.Line) error {
		if s.hasFinished() {
			return errStopFollowing
		}
		at := line.At
		if at.IsZero() {
			at = s.now()
		}
		s.log.Append(logs.Record{
			Time:    at,
			Service: name,
			Stream:  line.Stream,
			Text:    line.Text,
		})
		s.rememberLogPosition(name, id, at)
		return nil
	})
	if err == nil || errors.Is(err, errStopFollowing) || errors.Is(err, context.Canceled) || s.hasFinished() {
		return
	}
	s.event(logs.EventProblem, name, fmt.Sprintf("stopped reading the container's output: %v", err))
}

var errStopFollowing = errors.New("supervisor: the daemon is leaving")

// Where the follower got to, so the next hostd resumes instead of replaying
// the whole log or skipping what arrived while it was away.
func (s *Supervisor) rememberLogPosition(name, id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[name]
	if !ok || p.container != id {
		return
	}
	p.logSince = at
}

// adoptContainers finds what the runtime is already running for this host and
// takes it over, so restarting hostd does not restart the services.
func (s *Supervisor) adoptContainers(ctx context.Context, states map[string]procState) error {
	if s.runtime == nil {
		return nil
	}
	found, err := s.runtime.List(ctx, labelService)
	if err != nil {
		return fmt.Errorf("ask the container runtime what it is running: %w", err)
	}
	for _, container := range found {
		name := container.Name
		if len(name) > len("hostd-") {
			name = name[len("hostd-"):]
		}
		p, declared := s.procs[name]
		if !declared {
			if container.Running {
				s.event(logs.EventOrphan, name, fmt.Sprintf("container %s is running but no file declares it; stop it with hostctl service stop %s", short(container.ID), name))
			}
			continue
		}
		if !container.Running {
			// A dead container holds the name the next start needs.
			_ = s.runtime.Remove(ctx, container.ID)
			continue
		}
		observed, inspectErr := s.runtime.Inspect(ctx, container.ID)
		if inspectErr != nil {
			return inspectErr
		}
		p.container = container.ID
		p.pid = observed.PID
		p.started = observed.Started
		p.adopted = true
		p.running = true
		p.logSince = states[name].logSince()
		s.event(logs.EventAdopted, name, fmt.Sprintf("adopted running container %s after hostd restart", short(container.ID)))
		go s.waitContainer(name, container.ID)
		go s.followContainer(name, container.ID, p.logSince)
	}
	return nil
}

// stopContainer asks the runtime to stop the container and lets it kill what
// does not go. The call blocks for as long as the service's grace, so it does
// not happen under the supervisor's lock.
func (s *Supervisor) stopContainer(name, id string, grace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), grace+runtimeTimeout)
	defer cancel()
	err := s.runtime.Stop(ctx, id, grace)
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.event(logs.EventProblem, name, fmt.Sprintf("could not stop container %s: %v", short(id), err))
}

// Identifiers are 64 hex characters, sometimes behind the algorithm that
// produced them; the first twelve are what everything prints and what an
// operator recognises.
func short(id string) string {
	_, hex, found := strings.Cut(id, ":")
	if found {
		id = hex
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
