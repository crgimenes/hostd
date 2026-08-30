package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/service"
)

// The label that says a container is hostd's, and which service it is. It is
// what makes the runtime the source of truth: after a restart the daemon asks
// what it already owns instead of trusting a file to still be true.
//
// The filter is the key alone: with a trailing "=" the runtime reads it as
// "this label, empty value" and matches nothing, which looks exactly like a
// machine running no containers.
const labelService = "hostd.service"

// The network every service of this host shares. A service reaches another by
// its own name on it, which is why an application needs no port published to
// the machine at all: the only thing that has to be reachable from outside is
// whatever answers the internet.
const Network = "hostd"

// A container name derived from the service name, so an operator running
// docker ps sees which service a container belongs to without a lookup.
func containerName(service string) string { return "hostd-" + service }

// Named storage carries the service in its name: two services asking for
// "data" are asking for their own, and sharing has to be deliberate rather
// than a collision.
// VolumeName is the one place the naming lives; the file operations resolve a
// declaration's "data" to the same volume the container was given.
func VolumeName(service, volume string) string { return "hostd-" + service + "-" + volume }

// Talking to the runtime is a local socket call, but a runtime that is wedged
// must not hold a command.
const runtimeTimeout = 30 * time.Second

func runtimeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), runtimeTimeout)
}

// A stream that outlives the call that started it, but not the supervisor: a
// reader still waiting after the loop has left is a goroutine holding a
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

// What the runtime is holding for this host, one entry per service.
type held struct {
	docker.Container
	Service string
}

func (s *Supervisor) containers(ctx context.Context) ([]held, error) {
	client := s.client()
	if client == nil {
		return nil, fmt.Errorf("this machine has no container runtime: %w", docker.ErrNoRuntime)
	}
	found, err := client.List(ctx, labelService)
	if err != nil {
		return nil, err
	}
	out := make([]held, 0, len(found))
	for _, container := range found {
		out = append(out, held{Container: container, Service: container.Labels[labelService]})
	}
	return out, nil
}

// create builds the container the declaration describes, and starts it when
// the file says it should be running. The restart policy goes to the runtime:
// keeping it alive is its job, and hostd asking for the same thing at the same
// time is how a service flaps.
func (s *Supervisor) create(ctx context.Context, svc service.Service, start bool) error {
	client := s.client()
	if client == nil {
		return docker.ErrNoRuntime
	}
	ports, err := svc.PublishedPorts()
	if err != nil {
		return err
	}
	mounts, err := s.prepareMounts(ctx, svc)
	if err != nil {
		return err
	}
	mounts = append(mounts, s.configMount(svc)...)
	err = client.EnsureNetwork(ctx, Network)
	if err != nil {
		return err
	}
	// The tag in the file is a name that can be made to mean something else
	// tomorrow. What ran is recorded as the digest of this machine's copy.
	image, err := client.Image(ctx, svc.Image)
	if err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return fmt.Errorf("image %s is not on this machine; send it with hostctl image push", svc.Image)
		}
		return err
	}

	// A container from a previous definition holds the name; it is not the one
	// declared any more, and keeping it would only make the create fail.
	name := containerName(svc.Name)
	err = client.Remove(ctx, name)
	if err != nil {
		return err
	}
	id, err := client.Create(ctx, docker.Spec{
		Name:    name,
		Image:   svc.Image,
		Args:    svc.Args,
		Env:     svc.Env,
		Dir:     svc.Dir,
		Ports:   toDockerPorts(ports),
		Mounts:  mounts,
		Labels:  map[string]string{labelService: svc.Name},
		Memory:  int64(svc.Memory) * 1024 * 1024,
		NanoCPU: int64(svc.CPUs * 1e9),
		Network: Network,
		Alias:   svc.Name,
		Restart: restartPolicy(svc),
	})
	if err != nil {
		return err
	}
	if !start {
		s.event(logs.EventStarted, svc.Name, fmt.Sprintf("created container %s from %s, not started", short(id), short(image.Digest)))
		return nil
	}
	err = client.Start(ctx, id)
	if err != nil {
		// A container created and not started is a name taken for nothing.
		_ = client.Remove(ctx, id)
		return err
	}
	s.event(logs.EventStarted, svc.Name, fmt.Sprintf("started container %s from %s", short(id), short(image.Digest)))
	s.nudge()
	return nil
}

// What the file asks for, in the runtime's own words. "unless-stopped" is what
// makes an operator's stop survive a reboot of the machine and a restart of
// the runtime: the declaration says what should run, and a hand that stopped
// it is remembered.
func restartPolicy(svc service.Service) string {
	switch svc.Restart {
	case service.RestartOnFailure:
		return "on-failure"
	case service.RestartNever:
		return "no"
	default:
		return "unless-stopped"
	}
}

// Named storage is created if it is not there and never removed here: a
// service that goes away leaves its data behind, because deleting somebody's
// data is not a decision for a converge loop.
func (s *Supervisor) prepareMounts(ctx context.Context, svc service.Service) ([]docker.Mount, error) {
	declared, err := svc.Mounts()
	if err != nil {
		return nil, err
	}
	client := s.client()
	out := make([]docker.Mount, 0, len(declared))
	for _, mount := range declared {
		source := mount.Source
		if mount.Named {
			source = VolumeName(svc.Name, mount.Source)
			err = client.EnsureVolume(ctx, source, map[string]string{labelService: svc.Name})
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

// What travels with the declaration goes in read only: a container that could
// rewrite its own configuration would make the tree a lie. The target is the
// convention, never a field, and the mount exists exactly when the artifacts
// do — a bind whose source is missing is a container the runtime refuses to
// create.
func (s *Supervisor) configMount(svc service.Service) []docker.Mount {
	artifacts := filepath.Join(s.services, svc.Name+service.ArtifactSuffix)
	info, err := os.Stat(artifacts)
	if err != nil || !info.IsDir() {
		return nil
	}
	return []docker.Mount{{
		Source:   artifacts,
		Target:   service.ConfigDir(svc.Name),
		ReadOnly: true,
	}}
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

// follow keeps one reader on every running container and lets go of the ones
// that ended. The runtime keeps its own copy of the output; this is the one an
// operator reads without entering the machine.
func (s *Supervisor) follow(ctx context.Context, running []held) {
	live := make(map[string]bool, len(running))
	for _, container := range running {
		if !container.Running {
			continue
		}
		live[container.ID] = true
		s.mu.Lock()
		_, already := s.following[container.ID]
		s.mu.Unlock()
		if already {
			continue
		}
		s.startFollowing(container)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, stop := range s.following {
		if live[id] {
			continue
		}
		stop()
		delete(s.following, id)
	}
}

func (s *Supervisor) startFollowing(container held) {
	// Where this machine's own log store stopped is the only record of it that
	// matters: asking the runtime for everything since then replays nothing
	// and skips nothing.
	since, err := s.log.LastAt(container.Service)
	if err != nil {
		s.reportOnce(container.Service, fmt.Sprintf("cannot tell where the log of %s stopped: %v", container.Service, err))
		return
	}
	// Bounded by the supervisor's life rather than by whatever context the
	// caller happened to hold: a reader must outlive the call that started it,
	// and a run asked for by hand is started from a call that returns at once.
	streamCtx, cancel := s.streamContext()
	s.mu.Lock()
	s.following[container.ID] = cancel
	s.mu.Unlock()

	client := s.client()
	go func() {
		defer cancel()
		followErr := client.Logs(streamCtx, container.ID, since, func(line docker.Line) error {
			at := line.At
			if at.IsZero() {
				at = s.now()
			}
			s.log.Append(logs.Record{
				Time:    at,
				Service: container.Service,
				Stream:  line.Stream,
				Run:     runOf(container),
				Text:    line.Text,
			})
			return nil
		})
		if followErr == nil || errors.Is(followErr, context.Canceled) {
			return
		}
		s.reportOnce(container.Service, fmt.Sprintf("stopped reading the output of %s: %v", container.Service, followErr))
	}()
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

// retire takes away the plain container of a service, gracefully. It is named
// for the service alone, so the runs of a job — named for their instant — are
// not touched by it.
func (s *Supervisor) retire(ctx context.Context, svc service.Service) {
	client := s.client()
	if client == nil {
		return
	}
	container := containerName(svc.Name)
	s.sayStopping(svc)
	_ = client.Stop(ctx, container, svc.StopGrace())
	_ = client.Remove(ctx, container)
}

// sayStopping puts the wait in the timeline BEFORE it is waited out. A service
// whose process ignores SIGTERM spends its whole grace period dying, and
// thirty seconds of a screen with nothing on it is what makes an operator
// conclude that nothing happened and click again. Naming the budget also says
// where to change it: stop-timeout, in the declaration.
func (s *Supervisor) sayStopping(svc service.Service) {
	s.event(logs.EventStopped, svc.Name, fmt.Sprintf(
		"asking it to stop, waiting up to %s for it to go (stop-timeout)", svc.StopGrace()))
}
