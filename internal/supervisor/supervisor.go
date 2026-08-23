// Package supervisor keeps declared services in the state they were declared
// in, and finds them again when hostd itself restarts.
//
// The loop here is the project's own model applied to itself: nothing issues a
// sequence of commands at a process. A tick compares what is declared with
// what is observed and closes the difference, which is why an operator can ask
// ten times for a service to be running and get one process.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/crgimenes/hostd/internal/logs"
	"github.com/crgimenes/hostd/internal/procid"
	"github.com/crgimenes/hostd/internal/service"
)

// Tunables of the convergence loop.
const (
	// tickInterval is how often the supervisor compares declared with
	// observed. It also bounds how long a log line waits before it is
	// captured, so it is short enough to feel live.
	tickInterval = 100 * time.Millisecond
	// backoffMin and backoffMax bound the wait between restart attempts. A
	// service that crashes on start must not spin the machine.
	backoffMin = 200 * time.Millisecond
	backoffMax = 30 * time.Second
	// stableRun is how long a process must live before its restart backoff is
	// forgiven. Without it, a service that crashes every hour would eventually
	// be treated like one that crashes every second.
	stableRun = 30 * time.Second
	// killGrace is how long a process gets between SIGKILL and giving up on
	// it, after its own stop timeout has already passed.
	killGrace = 2 * time.Second
	// persistInterval bounds how often the read position of a spool file is
	// written back to disk.
	persistInterval = time.Second
)

// Dirs are the directories the supervisor owns.
type Dirs struct {
	// State holds one record per running process, for adoption.
	State string
	// Spool holds what services write to stdout and stderr.
	Spool string
}

// Status is the observed state of one service.
type Status struct {
	Name    string `filo:"name"`
	Kind    string `filo:"kind"`
	Desired string `filo:"desired"`
	// State is one of running, stopped, starting or failed.
	State string `filo:"state"`
	PID   int    `filo:"pid"`
	// Adopted reports that this process was found on startup rather than
	// started by this hostd.
	Adopted   bool    `filo:"adopted"`
	Since     float64 `filo:"since-ms"`
	Restarts  int     `filo:"restarts"`
	LastExit  int     `filo:"last-exit"`
	LastError string  `filo:"last-error"`
}

// Observed states.
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
	// cmd is nil for an adopted process: hostd is its supervisor but not its
	// parent, so it cannot wait on it or read its exit code.
	cmd *exec.Cmd

	running  bool
	stopping bool
	killed   bool
	// removeWhenStopped drops the service from view once its process is
	// really gone, so an undeclared service is never forgotten while it is
	// still running.
	removeWhenStopped bool
	deadline          time.Time
	restarts          int
	failures          int
	nextStart         time.Time
	lastExit          int
	lastError         string

	outTail *logs.Tailer
	errTail *logs.Tailer
	// persisted* is the last read position written to disk, so an unchanged
	// offset does not cost a write.
	persistedOut int64
	persistedErr int64
	lastPersist  time.Time
}

// Supervisor converges declared services toward their declared state.
type Supervisor struct {
	dirs Dirs
	log  *logs.Buffer
	now  func() time.Time

	mu       sync.Mutex
	procs    map[string]*proc
	finished bool
	// pumpMu serialises reads of the spool files, which happen outside mu so
	// that a slow disk cannot hold up a command.
	pumpMu sync.Mutex

	wake   chan struct{}
	done   chan struct{}
	closed sync.Once
}

// New creates a supervisor. It does not touch the machine until Adopt or Run.
func New(dirs Dirs, buffer *logs.Buffer) *Supervisor {
	return &Supervisor{
		dirs:  dirs,
		log:   buffer,
		now:   time.Now,
		procs: make(map[string]*proc),
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
}

// Adopt reconnects to the processes a previous hostd left running and reports
// what it found. It must run before Run, and it is what makes restarting hostd
// safe for the services it supervises.
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
			// A process whose service file is gone is an orphan. Reporting it
			// is the point: pretending it does not exist is how a machine ends
			// up with a process nobody owns.
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
			// The service died while hostd was away. It is noticed now, and
			// the record says so rather than pretending it was seen happen.
			p := s.procs[name]
			s.attachTails(p, st)
			p.lastError = "exited while hostd was not running"
			s.event(logs.EventMissed, name, "service was not running when hostd came back; restarting it if its policy asks for that")
			err = errors.Join(err, removeState(s.dirs.State, name))
		}
	}
	return err
}

// attachTails resumes reading each spool file from the recorded offset.
func (s *Supervisor) attachTails(p *proc, st procState) {
	p.outTail = logs.NewTailer(logs.SpoolPath(s.dirs.Spool, p.svc.Name, logs.StreamOut), p.svc.Name, logs.StreamOut, st.OutOffset)
	p.errTail = logs.NewTailer(logs.SpoolPath(s.dirs.Spool, p.svc.Name, logs.StreamErr), p.svc.Name, logs.StreamErr, st.ErrOffset)
}

func (s *Supervisor) ensureTails(p *proc) {
	if p.outTail == nil {
		s.attachTails(p, procState{})
	}
}

// Run converges until the context is cancelled, and then leaves.
//
// It deliberately does not stop the services on the way out. hostd going away
// is not the machine going away: a restart for an update, a crash, or an
// operator restarting the daemon must leave the work of the host running. The
// last thing Run does is write down where it stopped reading each spool file,
// so the next supervisor adopts the processes and resumes their output instead
// of replaying or losing it.
func (s *Supervisor) Run(ctx context.Context) {
	defer s.closed.Do(func() {
		// Nothing may change supervisor state after the loop has left. In
		// production the process is exiting anyway; the flag makes that
		// explicit so a wait goroutine still in flight cannot erase the
		// supervision record that the next hostd needs to adopt with.
		s.mu.Lock()
		s.finished = true
		s.mu.Unlock()
		close(s.done)
	})
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		s.pumpLogs()
		s.reconcile()
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

// Done is closed once Run has returned.
func (s *Supervisor) Done() <-chan struct{} { return s.done }

// nudge asks the loop to converge now instead of at the next tick, so that a
// command from hostctl takes effect immediately.
func (s *Supervisor) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
