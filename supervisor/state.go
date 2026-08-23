package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/service"
)

// What hostd must remember to find a process again after its own restart. It
// lives on persistent storage, not in tmpfs: losing it would leave a running
// service with no supervisor able to prove which process it was.
type procState struct {
	Name string `filo:"name"`
	PID  int    `filo:"pid"`
	// The kernel's process identity: a PID alone would let a reused number
	// pass for the original.
	Token string `filo:"token"`
	// Milliseconds: a Filo number is a float64, exact well below nanoseconds.
	StartedAt float64 `filo:"started-at-ms"`
	// How much of each spool has become log records, so a new hostd resumes
	// instead of replaying or skipping.
	OutOffset int64 `filo:"out-offset"`
	ErrOffset int64 `filo:"err-offset"`
	// A container service: what the runtime is holding, and the timestamp its
	// log reader had reached.
	Container  string  `filo:"container"`
	LogSinceMS float64 `filo:"log-since-ms"`
}

func (p procState) logSince() time.Time {
	if p.LogSinceMS == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(p.LogSinceMS))
}

func (p procState) startTime() time.Time {
	return time.UnixMilli(int64(p.StartedAt))
}

const stateExtension = ".filo"

func statePath(dir, name string) string {
	return filepath.Join(dir, name+stateExtension)
}

// Atomic: a half-written record would describe a process that cannot be
// verified, which is worse than no record at all.
func writeState(dir string, p procState) error {
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return err
	}
	body, err := filoconf.Marshal(p)
	if err != nil {
		return err
	}
	final := statePath(dir, p.Name)
	tmp, err := os.CreateTemp(dir, p.Name+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	_, err = tmp.WriteString(body)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	// The rename is only atomic for data that reached the disk.
	err = tmp.Sync()
	if err != nil {
		_ = tmp.Close()
		return err
	}
	err = tmp.Close()
	if err != nil {
		return err
	}
	return os.Rename(tmpName, final)
}

func removeState(dir, name string) error {
	err := os.Remove(statePath(dir, name))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readStates(ctx context.Context, dir string) (map[string]procState, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]procState{}, nil
		}
		return nil, err
	}
	states := make(map[string]procState, len(entries))
	var problems []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), stateExtension) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, readErr := os.ReadFile(path) // #nosec G304 -- path is inside hostd's own state directory
		if readErr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", e.Name(), readErr))
			continue
		}
		var p procState
		decodeErr := filoconf.Decode(ctx, e.Name(), string(data), &p)
		if decodeErr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", e.Name(), decodeErr))
			continue
		}
		if !service.ValidName(p.Name) || p.Name != strings.TrimSuffix(e.Name(), stateExtension) {
			problems = append(problems, fmt.Sprintf("%s: declares name %q", e.Name(), p.Name))
			continue
		}
		states[p.Name] = p
	}
	// Unreadable supervision state means processes that cannot be adopted.
	if len(problems) > 0 {
		return states, fmt.Errorf("unreadable supervision state:\n  %s", strings.Join(problems, "\n  "))
	}
	return states, nil
}
