// Package service defines what a service is and how a service file is read.
//
// One service is one Filo file, and the simple case stays simple: a command
// and a restart policy are a complete service.
package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crgimenes/hostd/filoconf"
)

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

	// A container service: the image to run and what of it reaches the
	// machine. Nothing is published that was not asked for, and a port with no
	// address binds to loopback, where a reverse proxy on the same host
	// reaches it and the internet does not.
	Image string   `filo:"image"`
	Ports []string `filo:"ports"`
	// Storage that outlives the container: "name:/path" is the runtime's to
	// keep, "/host/path:/path" is the machine's. Nothing is mounted that the
	// file did not name.
	Volumes []string `filo:"volumes"`
	Memory  float64  `filo:"memory-mb"`
	CPUs    float64  `filo:"cpus"`
}

// Mount is a volume as the runtime needs it. A source with no slash is named
// storage; anything else is a path on the machine.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
	Named    bool
}

func (s Service) Mounts() ([]Mount, error) {
	out := make([]Mount, 0, len(s.Volumes))
	for _, spec := range s.Volumes {
		mount, err := parseMount(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, mount)
	}
	return out, nil
}

// Handing a container the runtime's own socket hands it the machine, and it
// looks like an ordinary line in an ordinary file. Everything else is the
// operator's call, written down where it can be reviewed.
var forbiddenMounts = []string{"/var/run/docker.sock", "/run/docker.sock", "/run/podman/podman.sock"}

func parseMount(spec string) (Mount, error) {
	fields := strings.Split(spec, ":")
	readOnly := false
	if len(fields) == 3 {
		if fields[2] != "ro" && fields[2] != "rw" {
			return Mount{}, fmt.Errorf("volume %q: the third field is ro or rw", spec)
		}
		readOnly = fields[2] == "ro"
		fields = fields[:2]
	}
	if len(fields) != 2 {
		return Mount{}, fmt.Errorf("volume %q must be source:/path, with an optional ro or rw after it", spec)
	}
	source, target := fields[0], fields[1]
	if source == "" || !strings.HasPrefix(target, "/") {
		return Mount{}, fmt.Errorf("volume %q: the path inside the container must be absolute", spec)
	}
	named := !strings.Contains(source, "/")
	if !named && !strings.HasPrefix(source, "/") {
		return Mount{}, fmt.Errorf("volume %q: a path on the machine must be absolute, and a volume name must not contain a slash", spec)
	}
	if !named && slices.Contains(forbiddenMounts, filepath.Clean(source)) {
		return Mount{}, fmt.Errorf("volume %q: mounting the container runtime's socket gives the container this machine; if a service really needs it, run it outside hostd", spec)
	}
	return Mount{Source: source, Target: target, ReadOnly: readOnly, Named: named}, nil
}

// Port is a published port as the runtime needs it, parsed from the form an
// operator already knows: [address:]host:container[/protocol].
type Port struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string
}

func (s Service) PublishedPorts() ([]Port, error) {
	out := make([]Port, 0, len(s.Ports))
	for _, spec := range s.Ports {
		port, err := parsePort(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, port)
	}
	return out, nil
}

func parsePort(spec string) (Port, error) {
	rest, protocol, hasProtocol := strings.Cut(spec, "/")
	if !hasProtocol {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return Port{}, fmt.Errorf("port %q: protocol must be tcp or udp", spec)
	}
	fields := strings.Split(rest, ":")
	var address string
	switch len(fields) {
	case 2:
	case 3:
		address, fields = fields[0], fields[1:]
	default:
		return Port{}, fmt.Errorf("port %q must be host:container, with an optional address before it", spec)
	}
	host, err := strconv.Atoi(fields[0])
	if err != nil || host < 1 || host > 65535 {
		return Port{}, fmt.Errorf("port %q: %q is not a port number", spec, fields[0])
	}
	inside, err := strconv.Atoi(fields[1])
	if err != nil || inside < 1 || inside > 65535 {
		return Port{}, fmt.Errorf("port %q: %q is not a port number", spec, fields[1])
	}
	if address != "" && net.ParseIP(address) == nil {
		return Port{}, fmt.Errorf("port %q: %q is not an address", spec, address)
	}
	return Port{HostIP: address, HostPort: host, ContainerPort: inside, Protocol: protocol}, nil
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
		if s.Command == "" {
			return invalid("%s: command is required", s.Name)
		}
		if !filepath.IsAbs(s.Command) {
			return invalid("%s: command %q must be an absolute path", s.Name, s.Command)
		}
		if s.Image != "" || len(s.Ports) > 0 {
			return invalid("%s: image and ports belong to a container service, not an exec one", s.Name)
		}
		if s.Dir != "" && !filepath.IsAbs(s.Dir) {
			return invalid("%s: dir %q must be an absolute path", s.Name, s.Dir)
		}
	case KindContainer:
		err := s.normalizeContainer()
		if err != nil {
			return err
		}
	default:
		return invalid("%s: unknown kind %q", s.Name, s.Kind)
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

// The image is what a container service runs, so the command belongs to the
// image and not to the file. Everything the container gets from the machine is
// named here or it does not happen.
func (s *Service) normalizeContainer() error {
	if s.Image == "" {
		return invalid("%s: image is required for a container service", s.Name)
	}
	if s.Command != "" {
		return invalid("%s: a container service runs the image's own command; put arguments in args", s.Name)
	}
	_, err := s.PublishedPorts()
	if err != nil {
		return invalid("%s: %v", s.Name, err)
	}
	_, err = s.Mounts()
	if err != nil {
		return invalid("%s: %v", s.Name, err)
	}
	if s.Memory < 0 || s.CPUs < 0 {
		return invalid("%s: memory-mb and cpus must not be negative", s.Name)
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
