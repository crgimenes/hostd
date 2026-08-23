// Package supervisor keeps declared services in the state they were declared
// in, and finds them again when hostd itself restarts.
//
// Nothing issues a sequence of commands at a process: a tick compares declared
// with observed and closes the difference, which is why asking ten times for a
// service to run produces one process.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/procid"
	"github.com/crgimenes/hostd/service"
)

const (
	// Also bounds how long a log line waits before capture, so it has to feel
	// live, not only converge.
	tickInterval = 100 * time.Millisecond
	// A service that crashes on start must not spin the machine, and must not
	// grow into a wait nobody would sit through.
	backoffMin = 200 * time.Millisecond
	backoffMax = 30 * time.Second
	// How long a process must live before its backoff is forgiven; without
	// it, crashing hourly eventually looks like crashing every second.
	stableRun       = 30 * time.Second
	killGrace       = 2 * time.Second
	persistInterval = time.Second
	// Convergence durations kept for the percentiles -debug reports: an
	// average hides the one tick that ran long.
	tickSamples = 256
)

type Dirs struct {
	// One record per running process, for adoption.
	State string
	Spool string
}

type Status struct {
	Name    string `filo:"name"`
	Kind    string `filo:"kind"`
	Desired string `filo:"desired"`
	State   string `filo:"state"`
	PID     int    `filo:"pid"`
	// Found on startup rather than started by this hostd.
	Adopted   bool    `filo:"adopted"`
	Since     float64 `filo:"since-ms"`
	Restarts  int     `filo:"restarts"`
	LastExit  int     `filo:"last-exit"`
	LastError string  `filo:"last-error"`
}

const (
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateFailed   = "failed"
)

type proc struct {
	svc     service.Service
	pid     int
	token   string
	started time.Time
	adopted bool
	// nil for an adopted process: hostd supervises it without being its
	// parent, so it can neither wait on it nor read its exit code.
	cmd *exec.Cmd

	running  bool
	stopping bool
	killed   bool
	// Drop it from view once the process is really gone: an undeclared
	// service is never forgotten while it is still running.
	removeWhenStopped bool
	deadline          time.Time
	restarts          int
	failures          int
	nextStart         time.Time
	lastExit          int
	lastError         string

	outTail *logs.Tailer
	errTail *logs.Tailer
	// Last read position written to disk: an unchanged offset costs no write.
	persistedOut int64
	persistedErr int64
	lastPersist  time.Time
}

type Supervisor struct {
	dirs Dirs
	log  *logs.Store
	now  func() time.Time

	mu       sync.Mutex
	procs    map[string]*proc
	finished bool
	// Serialises spool reads, which happen outside mu so a slow disk cannot
	// hold up a command.
	pumpMu sync.Mutex

	ticks    uint64
	sampled  int
	tickRing [tickSamples]time.Duration

	wake   chan struct{}
	done   chan struct{}
	closed sync.Once
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
	defer s.mu.Unlock()
	st := Stats{Services: len(s.procs), Ticks: s.ticks}
	for _, p := range s.procs {
		if p.running {
			st.Running++
		}
	}
	samples := make([]time.Duration, s.sampled)
	copy(samples, s.tickRing[:s.sampled])
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

// New does not touch the machine until Adopt or Run.
func New(dirs Dirs, store *logs.Store) *Supervisor {
	return &Supervisor{
		dirs:  dirs,
		log:   store,
		now:   time.Now,
		procs: make(map[string]*proc),
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
}

// Must run before Run. This is what makes restarting hostd safe for the
// services it supervises.
func (s *Supervisor) Adopt(ctx context.Context, declared []service.Service) error {
	states, err := readStates(ctx, s.dirs.State)

	s.mu.Lock()
	defer s.mu.Unlock()

	byName := make(map[string]service.Service, len(declared))
	for _, svc := range declared {
		byName[svc.Name] = svc
		s.procs[svc.Name] = &proc{svc: svc}
	}

	for name, st := range states {
		_, declaredStill := byName[name]
		alive := procid.Matches(st.PID, st.Token)
		switch {
		case !declaredStill && alive:
			// Pretending an orphan does not exist is how a machine ends up
			// with a process nobody owns.
			s.event(logs.EventOrphan, name, fmt.Sprintf("orphan process %d is running but no service declares it; stop it with hostctl service stop %s", st.PID, name))
			p := &proc{
				svc:     service.Service{Name: name, Kind: service.KindExec, State: service.StateStopped},
				pid:     st.PID,
				token:   st.Token,
				started: st.startTime(),
				adopted: true,
				running: true,
			}
			s.attachTails(p, st)
			s.procs[name] = p
		case !declaredStill:
			err = errors.Join(err, removeState(s.dirs.State, name))
		case alive:
			p := s.procs[name]
			p.pid = st.PID
			p.token = st.Token
			p.started = st.startTime()
			p.adopted = true
			p.running = true
			s.attachTails(p, st)
			s.event(logs.EventAdopted, name, fmt.Sprintf("adopted running process %d after hostd restart", st.PID))
		default:
			// Died while hostd was away: noticed now, and recorded as observed
			// late rather than as seen happen.
			p := s.procs[name]
			s.attachTails(p, st)
			p.lastError = "exited while hostd was not running"
			s.event(logs.EventMissed, name, "service was not running when hostd came back; restarting it if its policy asks for that")
			err = errors.Join(err, removeState(s.dirs.State, name))
		}
	}
	return err
}

func (s *Supervisor) attachTails(p *proc, st procState) {
	p.outTail = logs.NewTailer(logs.SpoolPath(s.dirs.Spool, p.svc.Name, logs.StreamOut), p.svc.Name, logs.StreamOut, st.OutOffset)
	p.errTail = logs.NewTailer(logs.SpoolPath(s.dirs.Spool, p.svc.Name, logs.StreamErr), p.svc.Name, logs.StreamErr, st.ErrOffset)
}

func (s *Supervisor) ensureTails(p *proc) {
	if p.outTail == nil {
		s.attachTails(p, procState{})
	}
}

// Deliberately does not stop the services on the way out: hostd going away is
// not the machine going away. Before leaving it records where it stopped
// reading each spool, so the next supervisor resumes instead of replaying.
func (s *Supervisor) Run(ctx context.Context) {
	defer s.closed.Do(func() {
		// Without this, a wait goroutine still in flight could erase the
		// supervision record the next hostd needs to adopt with.
		s.mu.Lock()
		s.finished = true
		s.mu.Unlock()
		close(s.done)
	})
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		start := s.now()
		s.pumpLogs()
		s.reconcile()
		s.sample(s.now().Sub(start))
		select {
		case <-ctx.Done():
			s.pumpLogs()
			s.persistAllOffsets()
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
