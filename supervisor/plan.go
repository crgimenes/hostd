package supervisor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/crgimenes/hostd/service"
)

const (
	ActionAdd     = "add"
	ActionRemove  = "remove"
	ActionUpdate  = "update"
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionRestart = "restart"
)

// Destructive (stops and does not come back) and Disruptive (interrupted but
// returns) are separate on purpose: marking both the same way would either wave
// removals through or make every ordinary change ask for permission, and a
// confirmation everybody types without reading protects nobody.
type Change struct {
	Service     string `filo:"service"`
	Action      string `filo:"action"`
	Detail      string `filo:"detail"`
	Destructive bool   `filo:"destructive"`
	Disruptive  bool   `filo:"disruptive"`
}

// Apply runs this same computation, so a dry run and the real thing cannot
// drift apart.
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

	// The same declarations against the same observed state must produce the
	// same plan: what a person reviews and what an agent compares are one.
	slices.SortFunc(changes, func(a, b Change) int {
		return cmp.Or(strings.Compare(a.Service, b.Service), strings.Compare(a.Action, b.Action))
	})
	return changes
}

// So a plan is reviewable without diffing two files by eye.
func definitionDiff(old, updated service.Service) string {
	var parts []string
	if old.Command != updated.Command {
		parts = append(parts, fmt.Sprintf("command %s -> %s", old.Command, updated.Command))
	}
	if !slices.Equal(old.Args, updated.Args) {
		parts = append(parts, "arguments changed")
	}
	if !slices.Equal(old.Env, updated.Env) {
		parts = append(parts, "environment changed")
	}
	if old.Dir != updated.Dir {
		parts = append(parts, fmt.Sprintf("dir %q -> %q", old.Dir, updated.Dir))
	}
	if old.Image != updated.Image {
		parts = append(parts, fmt.Sprintf("image %s -> %s", old.Image, updated.Image))
	}
	if !slices.Equal(old.Ports, updated.Ports) {
		parts = append(parts, "published ports changed")
	}
	if old.Memory != updated.Memory || old.CPUs != updated.CPUs {
		parts = append(parts, "resource limits changed")
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
	return strings.Join(parts, ", ")
}

func HasDestructive(changes []Change) bool {
	for _, c := range changes {
		if c.Destructive {
			return true
		}
	}
	return false
}

func Destructive(changes []Change) []Change {
	var out []Change
	for _, c := range changes {
		if c.Destructive {
			out = append(out, c)
		}
	}
	return out
}
