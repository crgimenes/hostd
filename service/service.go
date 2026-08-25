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

// A service is a container. Supervising a process directly was hostd's job
// once and is the runtime's now: it already keeps a container alive through
// its own restart and the machine's reboot, and two supervisors with different
// opinions about one process is how a service flaps.
const KindContainer = "container"

const (
	StateRunning = "running"
	StateStopped = "stopped"
)

const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

// What a job does when its last run is still going.
const (
	// Start anyway. The runs share the work, which is what a worker pool is
	// for, and they agree with each other in the queue they read from.
	OverlapAllow = "overlap"
	// Let this turn pass. For work that must not run twice at once and has no
	// other way of saying so.
	OverlapSkip = "skip"
)

// Below this a scheduler is a fork bomb with extra steps.
const MinimumEvery = time.Second

// A ceiling nobody declared is still a ceiling: a job whose runs stop
// finishing would otherwise start one every turn until the machine dies.
const DefaultMaxParallel = 10

// Deliberately generous: a database flushing to disk deserves more patience
// than a web server.
const DefaultStopTimeout = 30 * time.Second

const Extension = ".filo"

type Service struct {
	Name string `filo:"name"`
	Kind string `filo:"kind"`
	// What the image runs, when the file wants something other than what the
	// image itself says.
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
	// Which machines this belongs on. A declaration that names neither
	// belongs on every machine it is pushed to, which is what a fleet of one
	// wants and what a heterogeneous fleet must be able to say otherwise: the
	// tree is shared, and the database does not belong on the web machines.
	Hosts []string `filo:"hosts"`
	Tags  []string `filo:"tags"`
	// Where the files that travel with this declaration are mounted, read
	// only. Without it a service directory with artifacts has nowhere to put
	// them, which is a mistake worth naming rather than ignoring.
	Config string  `filo:"config"`
	Memory float64 `filo:"memory-mb"`
	CPUs   float64 `filo:"cpus"`

	// A job: run this every so often instead of keeping it up. The cron this
	// replaces stops at the minute; a duration says what it means and goes
	// below it.
	Every string `filo:"every"`
	// What to do when the last run has not finished. Overlapping is the
	// default because it is what cron does and what a worker pool wants: the
	// instances share the work, and the queue they read from is where they
	// agree with each other.
	Overlap string `filo:"overlap"`
	// How many may run at once. Scaling without a ceiling is not elasticity,
	// it is a machine dying slowly while a new instance starts every two
	// minutes for ever.
	MaxParallel float64 `filo:"max-parallel"`
	// How long one run may take before it is stopped. Without it a run that
	// hangs holds its place for ever: the ceiling above fills with runs that
	// will never finish, every turn after that is skipped, and the job quietly
	// stops happening while the service still reads as scheduled.
	RunTimeout string `filo:"run-timeout"`

	// The hash of the files that travel with this declaration, computed where
	// they are read and never declared. It is what makes editing a Caddyfile a
	// change the plan can see: without it, an apply would say there is nothing
	// to do and the container would keep the old configuration.
	ConfigHash string
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

// A job is a service with a schedule: it runs and ends, over and over, instead
// of staying up.
func (s Service) IsJob() bool { return s.Every != "" }

func (s Service) Interval() time.Duration {
	every, err := time.ParseDuration(s.Every)
	if err != nil {
		return 0
	}
	return every
}

// RunLimit is how long one run of this job may take, zero when the declaration
// sets no bound. Not to be confused with StopGrace above, which is how long a
// container gets to end AFTER something decided to stop it — this is what
// decides.
//
// Validation has already refused anything that does not parse, so a failure
// here is a service that never got that far.
func (s Service) RunLimit() time.Duration {
	limit, err := time.ParseDuration(s.RunTimeout)
	if err != nil {
		return 0
	}
	return limit
}

func (s Service) Parallel() int {
	if s.MaxParallel <= 0 {
		return DefaultMaxParallel
	}
	return int(s.MaxParallel)
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
		s.Kind = KindContainer
	}
	if s.Kind != KindContainer {
		return invalid("%s: kind %q does not exist; a service is a container, and the runtime keeps it alive", s.Name, s.Kind)
	}
	err := s.normalizeContainer()
	if err != nil {
		return err
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
		return invalid("%s: image is required", s.Name)
	}
	if s.Dir != "" && !filepath.IsAbs(s.Dir) {
		return invalid("%s: dir %q must be an absolute path inside the container", s.Name, s.Dir)
	}
	_, err := s.PublishedPorts()
	if err != nil {
		return invalid("%s: %v", s.Name, err)
	}
	_, err = s.Mounts()
	if err != nil {
		return invalid("%s: %v", s.Name, err)
	}
	if s.Config != "" && !filepath.IsAbs(s.Config) {
		return invalid("%s: config %q must be an absolute path inside the container", s.Name, s.Config)
	}
	err = s.normalizeSchedule()
	if err != nil {
		return err
	}
	if s.Memory < 0 || s.CPUs < 0 {
		return invalid("%s: memory-mb and cpus must not be negative", s.Name)
	}
	return nil
}

// A job runs and ends. Everything here is about not letting it pretend to be a
// service that stays up, or a service pretend to be a job.
func (s *Service) normalizeSchedule() error {
	if !s.IsJob() {
		if s.Overlap != "" || s.MaxParallel != 0 || s.RunTimeout != "" {
			return invalid("%s: overlap, max-parallel and run-timeout belong to a job, which is a service with every", s.Name)
		}
		return nil
	}
	every, err := time.ParseDuration(s.Every)
	if err != nil {
		return invalid("%s: every %q is not a duration, like 30s or 2m: %v", s.Name, s.Every, err)
	}
	if every < MinimumEvery {
		return invalid("%s: every %s is below %s, and a scheduler firing faster than that is a fork bomb with extra steps",
			s.Name, every, MinimumEvery)
	}
	if s.Overlap == "" {
		s.Overlap = OverlapAllow
	}
	switch s.Overlap {
	case OverlapAllow, OverlapSkip:
	default:
		return invalid("%s: overlap %q is not %q or %q; waiting and killing are not built, and inventing one silently would be worse",
			s.Name, s.Overlap, OverlapAllow, OverlapSkip)
	}
	if s.MaxParallel < 0 {
		return invalid("%s: max-parallel must not be negative", s.Name)
	}
	if s.RunTimeout != "" {
		limit, timeoutErr := time.ParseDuration(s.RunTimeout)
		if timeoutErr != nil {
			return invalid("%s: run-timeout %q is not a duration, like 30s or 2m: %v", s.Name, s.RunTimeout, timeoutErr)
		}
		if limit <= 0 {
			return invalid("%s: run-timeout %s would stop every run before it began", s.Name, limit)
		}
	}
	// The runtime must not bring a job back: it ended because it was done.
	if s.Restart != "" && s.Restart != RestartNever {
		return invalid("%s: a job ends when it is done, so restart must be %q; the schedule is what runs it again",
			s.Name, RestartNever)
	}
	s.Restart = RestartNever
	if len(s.Ports) > 0 {
		return invalid("%s: a job publishes no port; several runs of it would ask for the same one", s.Name)
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
		s.ConfigHash, parseErr = hashArtifacts(filepath.Join(dir, s.Name+ArtifactSuffix))
		if parseErr != nil {
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

// BelongsTo answers whether this declaration is meant for a machine. The name
// is the one ssh is given, which is the one in ~/.ssh/config: a machine has one
// name here for the same reason it has one there.
//
// Naming neither hosts nor tags means everywhere: a tree that says nothing
// about placement is a tree whose services all belong wherever it is pushed.
func (s Service) BelongsTo(name string, tags []string) bool {
	if len(s.Hosts) == 0 && len(s.Tags) == 0 {
		return true
	}
	if slices.Contains(s.Hosts, name) {
		return true
	}
	for _, tag := range tags {
		if slices.Contains(s.Tags, tag) {
			return true
		}
	}
	return false
}
