package supervisor

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/service"
)

type ErrUnknownService struct{ Name string }

func (e ErrUnknownService) Error() string {
	return fmt.Sprintf("no service named %q is declared; list what exists with hostctl service list", e.Name)
}

func (s *Supervisor) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.procs))
	for _, p := range s.procs {
		out = append(out, s.statusOf(p))
	}
	slices.SortFunc(out, func(a, b Status) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (s *Supervisor) StatusOf(name string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[name]
	if !ok {
		return Status{}, ErrUnknownService{Name: name}
	}
	return s.statusOf(p), nil
}

// Values are copied under the lock: nothing hands a caller a pointer into live
// supervisor state.
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
		// Waiting out its backoff. Calling that "stopped" would hide a service
		// that is failing to start.
		st.State = StateFailed
	default:
		st.State = StateStarting
	}
	return st
}

// Changes the desired state, so asking twice produces one process. The bool
// says whether this ask moved anything: starting a service that is already
// running is accepted and changes nothing.
func (s *Supervisor) Start(name string) (bool, error) {
	changed, err := s.setDesired(name, service.StateRunning)
	if err != nil {
		return false, err
	}
	s.nudge()
	return changed, nil
}

func (s *Supervisor) Stop(name string) (bool, error) {
	changed, err := s.setDesired(name, service.StateStopped)
	if err != nil {
		return false, err
	}
	s.nudge()
	return changed, nil
}

func (s *Supervisor) setDesired(name, state string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[name]
	if !ok {
		return false, ErrUnknownService{Name: name}
	}
	changed := p.svc.State != state
	p.svc.State = state
	// An explicit start means now, not after the backoff earned by crashes.
	// Cutting a wait short is a change even when the desired state already
	// said running.
	if state == service.StateRunning {
		changed = changed || !p.nextStart.IsZero()
		p.failures = 0
		p.nextStart = time.Time{}
	}
	return changed, nil
}

// Always a change: it interrupts what is running, or starts what is not.
func (s *Supervisor) Restart(name string) (bool, error) {
	s.mu.Lock()
	p, ok := s.procs[name]
	if !ok {
		s.mu.Unlock()
		return false, ErrUnknownService{Name: name}
	}
	p.svc.State = service.StateRunning
	p.failures = 0
	p.nextStart = time.Time{}
	if p.running && !p.stopping {
		s.beginStop(p, s.now())
	}
	s.mu.Unlock()
	s.nudge()
	return true, nil
}

// Computes the same plan Plan does and carries it out. A service whose
// definition did not change keeps its process: applying configuration must not
// restart the machine's work for nothing.
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
			// Forgotten only once it is actually gone: a service is never
			// dropped from view while its process lives.
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

// The desired state is excluded on purpose: an operator who stopped a service
// by hand must not have it started again by an unrelated apply.
func sameDefinition(a, b service.Service) bool {
	if a.Kind != b.Kind || a.Command != b.Command || a.Dir != b.Dir ||
		a.Restart != b.Restart || a.StopTimeout != b.StopTimeout {
		return false
	}
	return slices.Equal(a.Args, b.Args) && slices.Equal(a.Env, b.Env)
}
