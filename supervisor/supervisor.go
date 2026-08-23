// Package supervisor keeps the containers a machine declares in the state the
// files ask for.
//
// It does not keep them alive: the runtime does that, with the policy the
// declaration asks for. Two supervisors with different opinions about one
// process is how a service ends up flapping, so there is one, and it is the
// one that already survives its own restart and the machine's reboot.
//
// What is running is the runtime's answer, never a file of ours; where the log
// reader stopped is hostd's own log store. Nothing about a service is written
// down twice.
package supervisor

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/service"
)

const (
	// A container the runtime is holding needs nobody watching it. This is
	// for what the runtime cannot notice: a container somebody removed by
	// hand, or a service declared while the machine could not start it.
	driftInterval = 15 * time.Second
	// Convergence durations kept for the percentiles -debug reports: an
	// average hides the one round that ran long.
	tickSamples = 256
)

// The json names are the filo names on purpose: a field is called one thing,
// whether it is read by hostctl over the wire or by the panel in the window.
type Status struct {
	Name    string `filo:"name" json:"name"`
	Kind    string `filo:"kind" json:"kind"`
	Desired string `filo:"desired" json:"desired"`
	State   string `filo:"state" json:"state"`
	PID     int    `filo:"pid" json:"pid"`
	// Running on this machine without any file declaring it.
	Orphan bool    `filo:"orphan" json:"orphan"`
	Since  float64 `filo:"since-ms" json:"since-ms"`
	// How many runs of a job are going. A job between runs has none, which is
	// what it should look like.
	Runs      int    `filo:"runs" json:"runs"`
	Restarts  int    `filo:"restarts" json:"restarts"`
	LastExit  int    `filo:"last-exit" json:"last-exit"`
	LastError string `filo:"last-error" json:"last-error"`
	Image     string `filo:"image" json:"image"`
	// How often a job runs, as the file says it.
	Every string `filo:"every" json:"every"`
}

const (
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateFailed   = "failed"
	// A job between runs. Calling that stopped would read as something that
	// should be up and is not, when it is a job doing exactly what it should.
	StateScheduled = "scheduled"
)

type Supervisor struct {
	log *logs.Store
	// Where this machine keeps declarations, and beside each one the files
	// that travel with it.
	services string
	runtime  *docker.Client
	now      func() time.Time

	mu sync.Mutex
	// What the files say, as of the last adopt or apply. What is actually
	// running is asked of the runtime every time it is needed.
	declared map[string]service.Service
	// One log reader per container being followed, by container id.
	following map[string]context.CancelFunc
	// Reported once each, so a machine with a service the runtime cannot
	// start does not write the same line every fifteen seconds.
	reported map[string]string
	// The last instant each job was fired for, so a schedule is not run twice
	// for the same slot and a daemon that restarts does not fire for the past.
	fired map[string]time.Time
	// Runs someone is waiting on, by container id. A run whose waiter left
	// with the daemon is picked up again rather than left unfinished.
	awaiting map[string]bool

	ticks    uint64
	sampled  int
	tickRing [tickSamples]time.Duration

	wake   chan struct{}
	done   chan struct{}
	closed sync.Once
}

// New does not touch the machine until Adopt or Run.
func New(store *logs.Store, services string) *Supervisor {
	return &Supervisor{
		log:       store,
		services:  services,
		now:       time.Now,
		declared:  make(map[string]service.Service),
		following: make(map[string]context.CancelFunc),
		reported:  make(map[string]string),
		fired:     make(map[string]time.Time),
		awaiting:  make(map[string]bool),
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// Runtime hands the supervisor the container daemon to use. Without it every
// service fails with a message saying this machine runs no containers, which
// is the honest answer rather than a silence.
func (s *Supervisor) Runtime(client *docker.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = client
}

// Adopt records what the files declare. There is nothing to take over: the
// runtime kept the containers running while hostd was away, and it is asked
// what it holds whenever anybody wants to know.
func (s *Supervisor) Adopt(ctx context.Context, declared []service.Service) error {
	s.mu.Lock()
	for _, svc := range declared {
		s.declared[svc.Name] = svc
	}
	s.mu.Unlock()

	if s.client() == nil {
		// A machine that runs no containers is a valid machine: the daemon
		// still answers, and every service reads as failed with the reason.
		return nil
	}
	running, err := s.containers(ctx)
	if err != nil {
		return err
	}
	for _, container := range running {
		_, known := s.declaration(container.Service)
		if known || !container.Running {
			continue
		}
		// Pretending an orphan does not exist is how a machine ends up with a
		// container nobody owns.
		s.event(logs.EventOrphan, container.Service, fmt.Sprintf(
			"container %s is running but no file declares it; stop it with hostctl service stop %s",
			short(container.ID), container.Service))
	}
	return nil
}

// Run watches for what the runtime cannot report by itself and keeps a reader
// on every container's output. Leaving stops neither: the containers are the
// runtime's, and the next hostd picks the reading up where the log store says
// it stopped.
func (s *Supervisor) Run(ctx context.Context) {
	defer s.closed.Do(func() { close(s.done) })
	// The clock and the machine are different questions asked at different
	// rates: a job every second cannot wait for the drift round.
	go s.schedule(ctx)
	ticker := time.NewTicker(driftInterval)
	defer ticker.Stop()
	for {
		start := s.now()
		s.observe(ctx)
		s.sample(s.now().Sub(start))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Supervisor) Done() <-chan struct{} { return s.done }

// Converge now rather than at the next tick, so a command takes effect at once.
func (s *Supervisor) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// observe closes the difference the runtime cannot: a container somebody
// removed by hand, a service declared while the machine could not start it,
// and the readers that carry output into the timeline.
func (s *Supervisor) observe(ctx context.Context) {
	if s.client() == nil {
		return
	}
	running, err := s.containers(ctx)
	if err != nil {
		s.reportOnce("runtime", fmt.Sprintf("cannot ask the container runtime what it is running: %v", err))
		return
	}
	s.follow(ctx, running)
	s.adoptRuns(ctx, running)

	// The runs of a job are excluded: a service has one container, and a run
	// standing in for it would be answered about as though it were the service.
	present := make(map[string]held, len(running))
	for _, container := range running {
		if container.Labels[labelRun] != "" {
			continue
		}
		present[container.Service] = container
	}
	for name, svc := range s.declarations() {
		if svc.IsJob() {
			// A job between runs is a job with nothing running, which is what
			// it should look like. The schedule decides when that changes.
			// A container of its own means it used to be a service, and
			// leaving it would leave something no file declares any more.
			_, stray := present[name]
			if stray {
				s.retire(ctx, svc)
			}
			continue
		}
		container, exists := present[name]
		if exists {
			s.convergePolicy(ctx, svc, container)
			continue
		}
		if !svc.WantRunning() {
			continue
		}
		// Declared, wanted, and not there at all: the runtime has nothing to
		// restart because nothing was ever created, or somebody removed it.
		err = s.create(ctx, svc, true)
		if err != nil {
			s.reportOnce(name, fmt.Sprintf("cannot start %s: %v", name, err))
			continue
		}
		s.clearReport(name)
	}
}

// The declaration says what the runtime should do when a container ends. A
// container whose policy drifted — because the file changed, or because it was
// created by an older hostd — is corrected in place: recreating it to change
// one field would interrupt a service for nothing.
func (s *Supervisor) convergePolicy(ctx context.Context, svc service.Service, container held) {
	wanted := restartPolicy(svc)
	client := s.client()
	if client == nil {
		return
	}
	observed, err := client.Inspect(ctx, container.ID)
	if err != nil || observed.Restart == wanted {
		return
	}
	err = client.UpdateRestart(ctx, container.ID, wanted)
	if err != nil {
		s.reportOnce(svc.Name, fmt.Sprintf("cannot set the restart policy of %s: %v", svc.Name, err))
		return
	}
	s.event(logs.EventApplied, svc.Name, fmt.Sprintf(
		"restart policy was %q and the file asks for %q; changed it without interrupting the service",
		observed.Restart, wanted))
}

// A problem that persists is reported once, not every fifteen seconds: a
// timeline full of the same line is a timeline nobody reads.
func (s *Supervisor) reportOnce(name, text string) {
	s.mu.Lock()
	last := s.reported[name]
	s.reported[name] = text
	s.mu.Unlock()
	if last == text {
		return
	}
	s.event(logs.EventProblem, name, text)
}

func (s *Supervisor) clearReport(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reported, name)
}

func (s *Supervisor) client() *docker.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtime
}

func (s *Supervisor) declaration(name string) (service.Service, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.declared[name]
	return svc, ok
}

func (s *Supervisor) declarations() map[string]service.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]service.Service, len(s.declared))
	maps.Copy(out, s.declared)
	return out
}

// event records a fact about a service in the same timeline as its output, so
// that a death and the last lines before it are read together.
func (s *Supervisor) event(kind, name, text string) {
	s.log.Append(logs.Record{
		Time:    s.now(),
		Service: name,
		Stream:  logs.StreamEvent,
		Kind:    kind,
		Text:    text,
	})
}

type Stats struct {
	Services int
	Running  int
	Ticks    uint64
	TickP50  time.Duration
	TickP95  time.Duration
	TickMax  time.Duration
}

func (s *Supervisor) Stats() Stats {
	s.mu.Lock()
	st := Stats{Services: len(s.declared), Ticks: s.ticks}
	samples := make([]time.Duration, s.sampled)
	copy(samples, s.tickRing[:s.sampled])
	s.mu.Unlock()

	for _, status := range s.Status() {
		if status.State == StateRunning {
			st.Running++
		}
	}
	slices.Sort(samples)
	st.TickP50 = percentile(samples, 0.50)
	st.TickP95 = percentile(samples, 0.95)
	st.TickMax = percentile(samples, 1)
	return st
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted)-1)*p)]
}

func (s *Supervisor) sample(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickRing[s.ticks%tickSamples] = d
	s.ticks++
	if s.sampled < tickSamples {
		s.sampled++
	}
}
