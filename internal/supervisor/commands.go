package supervisor

import (
	"fmt"
	"sort"
	"time"

	"github.com/crgimenes/hostd/internal/logs"
	"github.com/crgimenes/hostd/internal/service"
)

// ErrUnknownService is returned for a service no file declares.
type ErrUnknownService struct{ Name string }

func (e ErrUnknownService) Error() string {
	return fmt.Sprintf("no service named %q is declared; list what exists with hostctl service list", e.Name)
}

// Status reports the observed state of every service, sorted by name.
func (s *Supervisor) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.procs))
	for _, p := range s.procs {
		out = append(out, s.statusOf(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// StatusOf reports one service.
func (s *Supervisor) StatusOf(name string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[name]
	if !ok {
		return Status{}, ErrUnknownService{Name: name}
	}
	return s.statusOf(p), nil
}

// statusOf builds a status from a process. Values are copied under the lock so
// that nothing hands a caller a pointer into live supervisor state.
func (s *Supervisor) statusOf(p *proc) Status {
	st := Status{
		Name:      p.svc.Name,
		Kind:      p.svc.Kind,
		Desired:   p.svc.State,
		PID:       p.pid,
		Adopted:   p.adopted,
		Restarts:  p.restarts,
		LastExit:  p.lastExit,
		LastError: p.lastError,
	}
	if !p.started.IsZero() {
		st.Since = float64(p.started.UnixMilli())
	}
	switch {
	case p.running:
		st.State = StateRunning
	case !p.svc.WantRunning():
		st.State = StateStopped
	case !p.nextStart.IsZero() && s.now().Before(p.nextStart):
		// It is meant to run and is waiting out its backoff. Calling that
		// "stopped" would hide a service that is failing to start.
		st.State = StateFailed
	default:
		st.State = StateStarting
	}
	return st
}

// Start asks a service to run. It changes the desired state, so asking twice
// produces one process, and the answer survives until something changes it.
func (s *Supervisor) Start(name string) error {
	err := s.setDesired(name, service.StateRunning)
	if err != nil {
		return err
	}
	s.nudge()
	return nil
}

// Stop asks a service to stop.
func (s *Supervisor) Stop(name string) error {
	err := s.setDesired(name, service.StateStopped)
	if err != nil {
		return err
	}
	s.nudge()
	return nil
}

func (s *Supervisor) setDesired(name, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[name]
	if !ok {
		return ErrUnknownService{Name: name}
	}
	p.svc.State = state
	if state == service.StateRunning {
		// An explicit start is a statement that the operator wants it now,
		// not after the backoff earned by earlier crashes.
		p.failures = 0
		p.nextStart = time.Time{}
	}
	return nil
}

// Restart stops a service and lets the loop start it again.
func (s *Supervisor) Restart(name string) error {
	s.mu.Lock()
	p, ok := s.procs[name]
	if !ok {
		s.mu.Unlock()
		return ErrUnknownService{Name: name}
	}
	p.svc.State = service.StateRunning
	p.failures = 0
	p.nextStart = time.Time{}
	if p.running && !p.stopping {
		s.beginStop(p, s.now())
	}
	s.mu.Unlock()
	s.nudge()
	return nil
}

// Apply converges on the declared set of services and returns what it did.
//
// It computes the same plan Plan does and then carries it out, so a dry run
// and the real thing describe one transition rather than two that happen to
// agree most of the time.
//
// A service whose definition did not change keeps its process: applying
// configuration must not restart the machine's work for nothing.
func (s *Supervisor) Apply(declared []service.Service) []Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	changes := s.plan(declared)

	wanted := make(map[string]service.Service, len(declared))
	for _, svc := range declared {
		wanted[svc.Name] = svc
	}

	for _, c := range changes {
		switch c.Action {
		case ActionRemove:
			p := s.procs[c.Service]
			if p == nil {
				continue
			}
			if !p.running {
				delete(s.procs, c.Service)
				continue
			}
			// It stops first and is forgotten once it is actually gone, so a
			// service is never dropped from view while its process lives.
			p.svc.State = service.StateStopped
			p.removeWhenStopped = true
			s.beginStop(p, now)
		case ActionUpdate:
			p := s.procs[c.Service]
			if p == nil {
				continue
			}
			p.svc = wanted[c.Service]
			p.failures = 0
			p.nextStart = time.Time{}
			if p.running && !p.stopping {
				s.beginStop(p, now)
			}
		case ActionAdd:
			s.procs[c.Service] = &proc{svc: wanted[c.Service]}
		}
	}
	s.event(logs.EventApplied, "hostd", fmt.Sprintf("applied configuration: %d change(s)", len(changes)))
	return changes
}

// sameDefinition reports whether two definitions describe the same process.
// The desired state is deliberately excluded: an operator who stopped a
// service by hand should not have it started again by an unrelated apply.
func sameDefinition(a, b service.Service) bool {
	if a.Kind != b.Kind || a.Command != b.Command || a.Dir != b.Dir ||
		a.Restart != b.Restart || a.StopTimeout != b.StopTimeout {
		return false
	}
	return equalStrings(a.Args, b.Args) && equalStrings(a.Env, b.Env)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
