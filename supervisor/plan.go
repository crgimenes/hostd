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
	Service     string `filo:"service" json:"service"`
	Action      string `filo:"action" json:"action"`
	Detail      string `filo:"detail" json:"detail"`
	Destructive bool   `filo:"destructive" json:"destructive"`
	Disruptive  bool   `filo:"disruptive" json:"disruptive"`
}

// Apply runs this same computation, so a dry run and the real thing cannot
// drift apart.
func (s *Supervisor) Plan(declared []service.Service) []Change {
	wanted := make(map[string]service.Service, len(declared))
	for _, svc := range declared {
		wanted[svc.Name] = svc
	}
	// What is running comes from the runtime, so a plan is made against the
	// machine as it is rather than against what hostd remembers.
	live := map[string]bool{}
	ctx, cancel := runtimeContext()
	found, err := s.containers(ctx)
	cancel()
	if err == nil {
		for _, container := range found {
			live[container.Service] = container.Running
		}
	}

	var changes []Change
	for name, known := range s.declarations() {
		svc, still := wanted[name]
		if !still {
			changes = append(changes, Change{
				Service:     name,
				Action:      ActionRemove,
				Detail:      removedDetail(known),
				Destructive: live[name],
			})
			continue
		}
		if sameDefinition(known, svc) {
			continue
		}
		changes = append(changes, Change{
			Service:    name,
			Action:     ActionUpdate,
			Detail:     definitionDiff(known, svc),
			Disruptive: live[name],
		})
	}
	for name, svc := range wanted {
		_, exists := s.declaration(name)
		if exists {
			continue
		}
		changes = append(changes, Change{
			Service: name,
			Action:  ActionAdd,
			Detail:  addedDetail(svc),
		})
	}

	// The same declarations against the same observed state must produce the
	// same plan: what a person reviews and what an agent compares are one.
	slices.SortFunc(changes, func(a, b Change) int {
		return cmp.Or(strings.Compare(a.Service, b.Service), strings.Compare(a.Action, b.Action))
	})
	return changes
}

// A job between runs has nothing running to stop, so the removal reads as
// safe. Saying that its schedule stops is what keeps that from being read as
// "this changes nothing".
func removedDetail(svc service.Service) string {
	if svc.IsJob() {
		return "no file declares this job any more; it stops running every " + svc.Every
	}
	return "no file declares this service any more"
}

func addedDetail(svc service.Service) string {
	if svc.IsJob() {
		return fmt.Sprintf("declared as a job, every %s, state %s", svc.Every, svc.State)
	}
	return fmt.Sprintf("declared as %s, state %s", svc.Kind, svc.State)
}

// So a plan is reviewable without diffing two files by eye.
func definitionDiff(old, updated service.Service) string {
	var parts []string
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
	if old.ConfigHash != updated.ConfigHash {
		parts = append(parts, "configuration files changed")
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
