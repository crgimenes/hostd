package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/procid"
	"github.com/crgimenes/hostd/service"
)

func (s *Supervisor) reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, p := range s.procs {
		s.observe(p, now)
		switch {
		case p.stopping:
			s.enforceStop(p, now)
		case p.svc.WantRunning() && !p.running:
			s.maybeStart(p, now)
		case !p.svc.WantRunning() && p.running:
			s.beginStop(p, now)
		}
	}
}

// A process hostd started is reaped by its wait goroutine; an adopted one can
// only be checked by asking the kernel whether it is still the same process.
func (s *Supervisor) observe(p *proc, now time.Time) {
	// A container's end is reported by the goroutine waiting on the runtime,
	// the same way a child's is reported by the one waiting on it.
	if !p.running || p.cmd != nil || p.container != "" {
		return
	}
	if procid.Matches(p.pid, p.token) {
		return
	}
	p.running = false
	p.pid = 0
	p.lastError = "exited (adopted process, exit code unknown)"
	s.event(logs.EventGone, p.svc.Name, "adopted process is gone; exit code is unknown because hostd was not its parent")
	s.afterExit(p, now, -1)
}

func (s *Supervisor) maybeStart(p *proc, now time.Time) {
	if now.Before(p.nextStart) {
		return
	}
	err := s.start(p, now)
	if err == nil {
		return
	}
	p.lastError = err.Error()
	p.failures++
	p.nextStart = now.Add(backoff(p.failures))
	s.event(logs.EventStartFailed, p.svc.Name, fmt.Sprintf("start failed: %v; next attempt in %s", err, backoff(p.failures)))
}

func (s *Supervisor) start(p *proc, now time.Time) error {
	if p.svc.Kind == service.KindContainer {
		return s.startContainer(p, now)
	}
	s.ensureTails(p)
	out, err := logs.OpenSpool(s.dirs.Spool, p.svc.Name, logs.StreamOut)
	if err != nil {
		return fmt.Errorf("open stdout spool: %w", err)
	}
	defer func() { _ = out.Close() }()
	errFile, err := logs.OpenSpool(s.dirs.Spool, p.svc.Name, logs.StreamErr)
	if err != nil {
		return fmt.Errorf("open stderr spool: %w", err)
	}
	defer func() { _ = errFile.Close() }()

	cmd := exec.Command(p.svc.Command, p.svc.Args...) // #nosec G204 -- running the declared command is the entire job
	cmd.Dir = p.svc.Dir
	cmd.Env = p.svc.Env
	cmd.Stdout = out
	cmd.Stderr = errFile
	// Its own session, so a signal aimed at hostd's process group does not
	// reach it. With the spool files, this is what lets hostd be replaced
	// without the service noticing. The systemd unit needs KillMode=process
	// for the same reason.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	err = cmd.Start()
	if err != nil {
		return err
	}
	token, err := procid.Token(cmd.Process.Pid)
	if err != nil {
		// A process hostd could not later prove is its own is worse than no
		// process at all.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("read process identity: %w", err)
	}

	p.cmd = cmd
	p.pid = cmd.Process.Pid
	p.token = token
	p.started = now
	p.adopted = false
	p.running = true
	p.stopping = false
	p.killed = false
	p.lastError = ""

	err = writeState(s.dirs.State, procState{
		Name:      p.svc.Name,
		PID:       p.pid,
		Token:     token,
		StartedAt: float64(now.UnixMilli()),
		OutOffset: p.outTail.Offset(),
		ErrOffset: p.errTail.Offset(),
	})
	if err != nil {
		s.event(logs.EventProblem, p.svc.Name, fmt.Sprintf("started process %d but could not record its identity: %v; a hostd restart will not be able to adopt it", p.pid, err))
	}
	s.event(logs.EventStarted, p.svc.Name, fmt.Sprintf("started process %d", p.pid))
	go s.wait(p.svc.Name, cmd)
	return nil
}

func (s *Supervisor) wait(name string, cmd *exec.Cmd) {
	err := cmd.Wait()
	code := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		code = -1
	}

	if s.hasFinished() {
		return
	}
	// Draining before recording the exit is what puts the death and the last
	// lines that explain it in the right order.
	s.drain(name)

	s.mu.Lock()
	p, ok := s.procs[name]
	// A superseded process says nothing about the live one, and a supervisor
	// whose loop has left says nothing at all.
	if s.finished || !ok || p.cmd != cmd {
		s.mu.Unlock()
		return
	}
	now := s.now()
	p.running = false
	p.cmd = nil
	p.pid = 0
	p.lastExit = code
	p.lastError = ""
	// Wait reports a signalled exit as an error; showing "signal: terminated"
	// after a stop would report the operator's own command back as a fault.
	if err != nil && !p.stopping {
		p.lastError = err.Error()
	}
	switch {
	case p.stopping:
		s.event(logs.EventStopped, name, fmt.Sprintf("stopped (exit %d)", code))
	case code == 0:
		s.event(logs.EventExited, name, "exited normally (exit 0)")
	default:
		s.event(logs.EventExited, name, fmt.Sprintf("exited with code %d", code))
	}
	s.afterExit(p, now, code)
	s.mu.Unlock()
	s.nudge()
}

func (s *Supervisor) afterExit(p *proc, now time.Time, code int) {
	_ = removeState(s.dirs.State, p.svc.Name)
	wasStopping := p.stopping
	p.stopping = false
	p.killed = false
	if p.removeWhenStopped {
		delete(s.procs, p.svc.Name)
		return
	}
	if wasStopping {
		p.failures = 0
		p.nextStart = time.Time{}
		return
	}
	// Ran long enough to count as healthy, so it does not inherit the
	// impatience earned by earlier crashes.
	if !p.started.IsZero() && now.Sub(p.started) >= stableRun {
		p.failures = 0
	}
	restart := false
	switch p.svc.Restart {
	case service.RestartAlways:
		restart = true
	case service.RestartOnFailure:
		restart = code != 0
	}
	if !restart || !p.svc.WantRunning() {
		return
	}
	p.restarts++
	p.failures++
	p.nextStart = now.Add(backoff(p.failures))
}

func (s *Supervisor) beginStop(p *proc, now time.Time) {
	if !p.running || p.stopping {
		return
	}
	p.stopping = true
	p.deadline = now.Add(p.svc.StopGrace())
	if p.container != "" {
		// The runtime asks, waits and kills; the call blocks for the whole
		// grace, so it cannot happen under the supervisor's lock.
		go s.stopContainer(p.svc.Name, p.container, p.svc.StopGrace())
		s.event(logs.EventStopped, p.svc.Name, fmt.Sprintf("asked container %s to stop", short(p.container)))
		return
	}
	err := signal(p.pid, syscall.SIGTERM)
	if err != nil {
		s.event(logs.EventProblem, p.svc.Name, fmt.Sprintf("could not signal process %d: %v", p.pid, err))
		return
	}
	s.event(logs.EventStopped, p.svc.Name, fmt.Sprintf("asked process %d to stop", p.pid))
}

func (s *Supervisor) enforceStop(p *proc, now time.Time) {
	if !p.running {
		p.stopping = false
		return
	}
	if now.Before(p.deadline) {
		return
	}
	// The runtime kills what did not stop within the grace, so there is
	// nothing for hostd to send; what is left is to wait for the exit.
	if p.container != "" {
		return
	}
	if !p.killed {
		p.killed = true
		p.deadline = now.Add(killGrace)
		_ = signal(p.pid, syscall.SIGKILL)
		s.event(logs.EventKilled, p.svc.Name, fmt.Sprintf("process %d did not stop within %s; killed", p.pid, p.svc.StopGrace()))
		return
	}
	// Killed and still there: only an adopted process reaches here, since a
	// child would have been reaped.
	if p.cmd == nil && !procid.Matches(p.pid, p.token) {
		p.running = false
		p.pid = 0
		p.stopping = false
		return
	}
	s.event(logs.EventProblem, p.svc.Name, fmt.Sprintf("process %d survived SIGKILL; it is no longer being supervised", p.pid))
	p.lastError = fmt.Sprintf("process %d survived SIGKILL", p.pid)
	p.running = false
	p.stopping = false
}

// Signals the process group: a service leads its own session, so the whole
// tree it spawned is asked to stop, not only its first process.
func signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-pid, sig)
	if err == nil {
		return nil
	}
	// An adopted process may not lead its own group after all.
	return syscall.Kill(pid, sig)
}

func (s *Supervisor) hasFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

func backoff(failures int) time.Duration {
	if failures < 1 {
		return backoffMin
	}
	d := backoffMin
	for range failures - 1 {
		d *= 2
		if d >= backoffMax {
			return backoffMax
		}
	}
	return d
}

type namedTail struct {
	name string
	tail *logs.Tailer
}

// Sorted, so captured output arrives in a stable order and not in map order.
func (s *Supervisor) tails(only string) []namedTail {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []namedTail
	for name, p := range s.procs {
		if p.outTail == nil || (only != "" && name != only) {
			continue
		}
		out = append(out, namedTail{name, p.outTail}, namedTail{name, p.errTail})
	}
	slices.SortStableFunc(out, func(a, b namedTail) int { return strings.Compare(a.name, b.name) })
	return out
}

func (s *Supervisor) pumpLogs() {
	s.read(s.tails(""), true)
	s.persistOffsets()
}

// Called on the exit path so nothing a service wrote arrives after the event
// saying it died.
func (s *Supervisor) drain(name string) {
	s.read(s.tails(name), false)
}

// Under its own lock rather than the supervisor's: a slow disk must not hold
// up a command, but two goroutines on one spool would double-count.
func (s *Supervisor) read(tails []namedTail, recycle bool) {
	s.pumpMu.Lock()
	defer s.pumpMu.Unlock()
	now := s.now()
	for _, t := range tails {
		for {
			records, err := t.tail.Read(now)
			if err != nil {
				s.event(logs.EventProblem, t.name, fmt.Sprintf("could not read captured output: %v", err))
				break
			}
			for _, r := range records {
				s.log.Append(r)
			}
			if len(records) == 0 {
				break
			}
		}
		if !recycle {
			continue
		}
		lost, err := t.tail.Recycle()
		if err != nil {
			s.event(logs.EventProblem, t.name, fmt.Sprintf("could not recycle the output spool: %v", err))
			continue
		}
		if lost > 0 {
			s.event(logs.EventSpoolLost, t.name, fmt.Sprintf("output spool exceeded its ceiling; %d bytes were discarded", lost))
		}
	}
}

// Writes only when an offset moved, and at most once per second per service:
// reserialising on every tick spends the disk on nothing. The bounded cost is
// that an unclean death re-reads up to a second of captured output, which is a
// duplicate line rather than a lost one.
func (s *Supervisor) persistOffsets() { s.savePositions(persistInterval) }

// Ignores the interval: hostd is on its way out and the next supervisor must
// resume from where this one really stopped.
func (s *Supervisor) persistAllOffsets() { s.savePositions(0) }

func (s *Supervisor) savePositions(minInterval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, p := range s.procs {
		if !p.running || p.pid == 0 {
			continue
		}
		if p.container != "" {
			s.saveContainerPosition(p, now, minInterval)
			continue
		}
		if p.outTail == nil {
			continue
		}
		out, errOff := p.outTail.Offset(), p.errTail.Offset()
		if out == p.persistedOut && errOff == p.persistedErr {
			continue
		}
		if now.Sub(p.lastPersist) < minInterval {
			continue
		}
		err := writeState(s.dirs.State, procState{
			Name:      p.svc.Name,
			PID:       p.pid,
			Token:     p.token,
			StartedAt: float64(p.started.UnixMilli()),
			OutOffset: out,
			ErrOffset: errOff,
		})
		if err != nil {
			continue
		}
		p.persistedOut, p.persistedErr, p.lastPersist = out, errOff, now
	}
}

// The container's own position: the timestamp its log reader reached, which is
// where the next hostd asks the runtime to resume from.
func (s *Supervisor) saveContainerPosition(p *proc, now time.Time, minInterval time.Duration) {
	if p.logSince.IsZero() || now.Sub(p.lastPersist) < minInterval {
		return
	}
	err := writeState(s.dirs.State, procState{
		Name:       p.svc.Name,
		PID:        p.pid,
		Container:  p.container,
		StartedAt:  float64(p.started.UnixMilli()),
		LogSinceMS: float64(p.logSince.UnixMilli()),
	})
	if err != nil {
		return
	}
	p.lastPersist = now
}

// event records a fact about a service in the same timeline as its output, so
// that a death and the last lines before it are read together.
// The kind is a stable code a program matches on; the text is for a person and
// may be rewritten.
func (s *Supervisor) event(kind, name, text string) {
	s.log.Append(logs.Record{
		Time:    s.now(),
		Service: name,
		Stream:  logs.StreamEvent,
		Kind:    kind,
		Text:    text,
	})
}
