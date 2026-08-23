// Package service defines what a service is and how a service file is read.
//
// One service is one Filo file. The simple case has to stay simple: a command
// to run and a restart policy is a complete service.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/crgimenes/hostd/internal/filoconf"
)

// Kinds of service. Only exec exists today; containers arrive with the Docker
// backend and are rejected until then with an error that says so.
const (
	KindExec      = "exec"
	KindContainer = "container"
)

// Desired states.
const (
	StateRunning = "running"
	StateStopped = "stopped"
)

// Restart policies.
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

// DefaultStopTimeout is how long a service has to exit after SIGTERM before it
// is killed. It is deliberately generous: a database flushing to disk deserves
// more patience than a web server.
const DefaultStopTimeout = 30 * time.Second

// Extension of a service file inside the services directory.
const Extension = ".filo"

// Service is the declared state of one service.
type Service struct {
	Name        string   `filo:"name"`
	Kind        string   `filo:"kind"`
	Command     string   `filo:"command"`
	Args        []string `filo:"args"`
	Dir         string   `filo:"dir"`
	Env         []string `filo:"env"`
	State       string   `filo:"state"`
	Restart     string   `filo:"restart"`
	StopTimeout float64  `filo:"stop-timeout"`
}

// StopGrace returns how long the service may take to exit after SIGTERM.
func (s Service) StopGrace() time.Duration {
	if s.StopTimeout <= 0 {
		return DefaultStopTimeout
	}
	return time.Duration(s.StopTimeout * float64(time.Second))
}

// WantRunning reports whether the declared state is running.
func (s Service) WantRunning() bool {
	return s.State == StateRunning
}

// ErrInvalid is the class of every rejected service definition.
var ErrInvalid = errors.New("invalid service")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ValidName reports whether a name is safe to use as an identifier and as a
// path element. The name reaches the filesystem in spool and state files, so
// this is a trust boundary, not a style preference.
func ValidName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_':
			// A leading or trailing separator makes names that differ only by
			// punctuation, which reads badly in logs and in file names.
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// normalize fills defaults and validates. The zero value of a field means "not
// declared", so defaults are applied here and never in the supervisor, which
// must see a complete definition.
func (s *Service) normalize() error {
	s.Name = strings.TrimSpace(s.Name)
	if !ValidName(s.Name) {
		return invalid("name %q must be 1-64 characters of a-z, 0-9, - and _", s.Name)
	}
	if s.Kind == "" {
		s.Kind = KindExec
	}
	switch s.Kind {
	case KindExec:
	case KindContainer:
		return invalid("%s: kind %q is not implemented yet; only %q works today", s.Name, KindContainer, KindExec)
	default:
		return invalid("%s: unknown kind %q", s.Name, s.Kind)
	}
	if s.Command == "" {
		return invalid("%s: command is required", s.Name)
	}
	if !filepath.IsAbs(s.Command) {
		return invalid("%s: command %q must be an absolute path", s.Name, s.Command)
	}
	if s.Dir != "" && !filepath.IsAbs(s.Dir) {
		return invalid("%s: dir %q must be an absolute path", s.Name, s.Dir)
	}
	for _, e := range s.Env {
		if !strings.Contains(e, "=") {
			return invalid("%s: env entry %q must be in the form NAME=value", s.Name, e)
		}
	}
	if s.State == "" {
		s.State = StateRunning
	}
	if s.State != StateRunning && s.State != StateStopped {
		return invalid("%s: unknown state %q, expected %q or %q", s.Name, s.State, StateRunning, StateStopped)
	}
	if s.Restart == "" {
		s.Restart = RestartAlways
	}
	switch s.Restart {
	case RestartAlways, RestartOnFailure, RestartNever:
	default:
		return invalid("%s: unknown restart policy %q, expected %q, %q or %q",
			s.Name, s.Restart, RestartAlways, RestartOnFailure, RestartNever)
	}
	if s.StopTimeout < 0 {
		return invalid("%s: stop-timeout must not be negative", s.Name)
	}
	return nil
}

// Parse reads one service definition from Filo source.
func Parse(ctx context.Context, name, src string) (Service, error) {
	var s Service
	err := filoconf.Decode(ctx, name, src, &s)
	if err != nil {
		return Service{}, err
	}
	err = s.normalize()
	if err != nil {
		return Service{}, fmt.Errorf("%s: %w", name, err)
	}
	return s, nil
}

// ParseFile reads one service definition from disk. The file name must match
// the declared service name, so that the file an operator edits is the service
// they think they are editing.
func ParseFile(ctx context.Context, path string) (Service, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path comes from hostd's own services directory
	if err != nil {
		return Service{}, err
	}
	base := filepath.Base(path)
	s, err := Parse(ctx, base, string(data))
	if err != nil {
		return Service{}, err
	}
	want := strings.TrimSuffix(base, Extension)
	if s.Name != want {
		return Service{}, fmt.Errorf("%s: %w: declares name %q but the file is named %q; rename the file to %s%s",
			base, ErrInvalid, s.Name, want, s.Name, Extension)
	}
	return s, nil
}

// LoadDir reads every service file in dir, sorted by name so that a plan built
// from them is deterministic. A directory that does not exist is not an error:
// a host with no services declared yet is a valid host.
func LoadDir(ctx context.Context, dir string) ([]Service, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Two files cannot declare the same service: ParseFile requires the
	// declared name to match the file name, and the filesystem already keeps
	// file names unique.
	var services []Service
	var problems []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), Extension) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, parseErr := ParseFile(ctx, path)
		if parseErr != nil {
			// One broken file must not hide the others: the operator gets
			// every problem at once instead of fixing them one restart at a
			// time.
			problems = append(problems, parseErr.Error())
			continue
		}
		services = append(services, s)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	if len(problems) > 0 {
		return services, fmt.Errorf("%w:\n  %s", ErrInvalid, strings.Join(problems, "\n  "))
	}
	return services, nil
}
