// Package service defines what a service is and how a service file is read.
//
// One service is one Filo file, and the simple case stays simple: a command
// and a restart policy are a complete service.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crgimenes/hostd/filoconf"
)

// Only exec exists today; container is accepted by the parser so it can be
// rejected with an error that says it is not built yet.
const (
	KindExec      = "exec"
	KindContainer = "container"
)

const (
	StateRunning = "running"
	StateStopped = "stopped"
)

const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

// Deliberately generous: a database flushing to disk deserves more patience
// than a web server.
const DefaultStopTimeout = 30 * time.Second

const Extension = ".filo"

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

func (s Service) StopGrace() time.Duration {
	if s.StopTimeout <= 0 {
		return DefaultStopTimeout
	}
	return time.Duration(s.StopTimeout * float64(time.Second))
}

func (s Service) WantRunning() bool {
	return s.State == StateRunning
}

var ErrInvalid = errors.New("invalid service")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// The name reaches the filesystem in spool and state files, so this is a trust
// boundary, not a style preference.
func ValidName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Defaults are applied here and never in the supervisor, which must see a
// complete definition.
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

// The file name must match the declared name, so the file an operator edits is
// the service they think they are editing.
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

// Sorted by name, so a plan built from the result is deterministic. A missing
// directory is not an error: a host with no services declared is valid.
func LoadDir(ctx context.Context, dir string) ([]Service, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Two files cannot declare one service: ParseFile requires name to match
	// file name, and the filesystem keeps file names unique.
	var services []Service
	var problems []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), Extension) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, parseErr := ParseFile(ctx, path)
		if parseErr != nil {
			// Every problem at once: the operator does not fix them one
			// restart at a time.
			problems = append(problems, parseErr.Error())
			continue
		}
		services = append(services, s)
	}
	slices.SortFunc(services, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	if len(problems) > 0 {
		return services, fmt.Errorf("%w:\n  %s", ErrInvalid, strings.Join(problems, "\n  "))
	}
	return services, nil
}
