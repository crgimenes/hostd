package supervisor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/crgimenes/hostd/internal/service"
)

// Actions a plan can contain.
const (
	ActionAdd     = "add"
	ActionRemove  = "remove"
	ActionUpdate  = "update"
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionRestart = "restart"
)

// Change is one step of a plan.
//
// Destructive and Disruptive are separate on purpose. A restart interrupts a
// service and it comes back; a removal takes it away and it does not. Marking
// both the same way would either wave through removals or make every ordinary
// configuration change ask for permission, and a confirmation everybody types
// without reading protects nobody.
type Change struct {
	Service string `filo:"service"`
	Action  string `filo:"action"`
	Detail  string `filo:"detail"`
	// Destructive means a service stops and does not come back on its own.
	Destructive bool `filo:"destructive"`
	// Disruptive means the service is interrupted but returns.
	Disruptive bool `filo:"disruptive"`
}

// Plan reports what applying the given declarations would do, without doing
// any of it.
//
// Apply runs the same computation, so a dry run and the real thing cannot
// drift apart: there is one description of the transition and two things done
// with it.
func (s *Supervisor) Plan(declared []service.Service) []Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plan(declared)
}

func (s *Supervisor) plan(declared []service.Service) []Change {
	wanted := make(map[string]service.Service, len(declared))
	for _, svc := range declared {
		wanted[svc.Name] = svc
	}

	var changes []Change
	for name, p := range s.procs {
		svc, still := wanted[name]
		if !still {
			changes = append(changes, Change{
				Service:     name,
				Action:      ActionRemove,
				Detail:      "no file declares this service any more",
				Destructive: p.running,
			})
			continue
		}
		if sameDefinition(p.svc, svc) {
			continue
		}
		changes = append(changes, Change{
			Service:    name,
			Action:     ActionUpdate,
			Detail:     definitionDiff(p.svc, svc),
			Disruptive: p.running,
		})
	}
	for name, svc := range wanted {
		_, exists := s.procs[name]
		if exists {
			continue
		}
		changes = append(changes, Change{
			Service: name,
			Action:  ActionAdd,
			Detail:  fmt.Sprintf("declared as %s, state %s", svc.Kind, svc.State),
		})
	}

	// Sorted so the same declarations always produce the same plan: a plan a
	// person reviews and a plan an agent compares have to be one thing.
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Service != changes[j].Service {
			return changes[i].Service < changes[j].Service
		}
		return changes[i].Action < changes[j].Action
	})
	return changes
}

// definitionDiff says what actually changed, so the plan is reviewable without
// diffing two files by eye.
func definitionDiff(old, updated service.Service) string {
	var parts []string
	if old.Command != updated.Command {
		parts = append(parts, fmt.Sprintf("command %s -> %s", old.Command, updated.Command))
	}
	if !equalStrings(old.Args, updated.Args) {
		parts = append(parts, "arguments changed")
	}
	if !equalStrings(old.Env, updated.Env) {
		parts = append(parts, "environment changed")
	}
	if old.Dir != updated.Dir {
		parts = append(parts, fmt.Sprintf("dir %q -> %q", old.Dir, updated.Dir))
	}
	if old.Kind != updated.Kind {
		parts = append(parts, fmt.Sprintf("kind %s -> %s", old.Kind, updated.Kind))
	}
	if old.Restart != updated.Restart {
		parts = append(parts, fmt.Sprintf("restart %s -> %s", old.Restart, updated.Restart))
	}
	if old.StopTimeout != updated.StopTimeout {
		parts = append(parts, fmt.Sprintf("stop timeout %gs -> %gs", old.StopTimeout, updated.StopTimeout))
	}
	if len(parts) == 0 {
		return "definition changed"
	}
	var out strings.Builder
	out.WriteString(parts[0])
	for _, p := range parts[1:] {
		out.WriteString(", " + p)
	}
	return out.String()
}

// HasDestructive reports whether a plan takes a running service away.
func HasDestructive(changes []Change) bool {
	for _, c := range changes {
		if c.Destructive {
			return true
		}
	}
	return false
}

// Destructive returns only the changes that take a service away.
func Destructive(changes []Change) []Change {
	var out []Change
	for _, c := range changes {
		if c.Destructive {
			out = append(out, c)
		}
	}
	return out
}
