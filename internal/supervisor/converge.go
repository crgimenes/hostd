package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"

	"github.com/crgimenes/hostd/internal/logs"
	"github.com/crgimenes/hostd/internal/procid"
	"github.com/crgimenes/hostd/internal/service"
)

// reconcile closes the difference between what is declared and what is
// observed, once.
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

// observe notices a process that is no longer there. A process hostd started
// is reaped by its wait goroutine; an adopted one can only be checked by
// asking the kernel whether it is still the same process.
func (s *Supervisor) observe(p *proc, now time.Time) {
	if !p.running || p.cmd != nil {
		return
	}
	if procid.Matches(p.pid, p.token) {
		return
	}
	p.running = false
	p.pid = 0
	p.lastError = "exited (adopted process, exit code unknown)"
	s.event(p.svc.Name, "adopted process is gone; exit code is unknown because hostd was not its parent")
	s.afterExit(p, now, -1)
}

// maybeStart starts a service unless its backoff says to wait.
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
	s.event(p.svc.Name, fmt.Sprintf("start failed: %v; next attempt in %s", err, backoff(p.failures)))
}

// start launches the process and records enough to find it again.
func (s *Supervisor) start(p *proc, now time.Time) error {
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
	// Setsid puts the service in its own session, so a signal aimed at hostd's
	// process group does not reach it. Together with the spool files, this is
	// what lets hostd die and be replaced without the service noticing.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	err = cmd.Start()
	if err != nil {
		return err
	}
	token, err := procid.Token(cmd.Process.Pid)
	if err != nil {
		// Without an identity the process could not be adopted later, and a
		// process hostd cannot prove is its own is worse than no process.
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
		s.event(p.svc.Name, fmt.Sprintf("started process %d but could not record its identity: %v; a hostd restart will not be able to adopt it", p.pid, err))
	}
	s.event(p.svc.Name, fmt.Sprintf("started process %d", p.pid))
	go s.wait(p.svc.Name, cmd)
	return nil
}

// wait reaps a process hostd started and hands the exit back to the loop.
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
	// The process is gone and its spool holds everything it ever wrote.
	// Draining before recording the exit is what puts the death and the last
	// lines that explain it in the right order.
	s.drain(name)

	s.mu.Lock()
	p, ok := s.procs[name]
	// A process that is no longer the current one for this service has
	// already been superseded; its exit says nothing about the live one.
	// A supervisor whose loop has left says nothing at all.
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
	// A process that was asked to stop exits by signal, and Wait reports that
	// as an error. Showing "signal: terminated" next to a service the operator
	// just stopped would report their own command back to them as a fault.
	if err != nil && !p.stopping {
		p.lastError = err.Error()
	}
	switch {
	case p.stopping:
		s.event(name, fmt.Sprintf("stopped (exit %d)", code))
	case code == 0:
		s.event(name, "exited normally (exit 0)")
	default:
		s.event(name, fmt.Sprintf("exited with code %d", code))
	}
	s.afterExit(p, now, code)
	s.mu.Unlock()
	s.nudge()
}

// afterExit applies the restart policy to a process that is no longer running.
func (s *Supervisor) afterExit(p *proc, now time.Time, code int) {
	_ = removeState(s.dirs.State, p.svc.Name)
	wasStopping := p.stopping
	p.stopping = false
	p.killed = false
	if wasStopping {
		p.failures = 0
		p.nextStart = time.Time{}
		return
	}
	if !p.started.IsZero() && now.Sub(p.started) >= stableRun {
		// It ran long enough to count as healthy, so it does not inherit the
		// impatience earned by earlier crashes.
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

// beginStop asks a process to end, politely.
func (s *Supervisor) beginStop(p *proc, now time.Time) {
	if !p.running || p.stopping {
		return
	}
	p.stopping = true
	p.deadline = now.Add(p.svc.StopGrace())
	err := signal(p.pid, syscall.SIGTERM)
	if err != nil {
		s.event(p.svc.Name, fmt.Sprintf("could not signal process %d: %v", p.pid, err))
		return
	}
	s.event(p.svc.Name, fmt.Sprintf("asked process %d to stop", p.pid))
}

// enforceStop escalates a stop that is taking too long.
func (s *Supervisor) enforceStop(p *proc, now time.Time) {
	if !p.running {
		p.stopping = false
		return
	}
	if now.Before(p.deadline) {
		return
	}
	if !p.killed {
		p.killed = true
		p.deadline = now.Add(killGrace)
		_ = signal(p.pid, syscall.SIGKILL)
		s.event(p.svc.Name, fmt.Sprintf("process %d did not stop within %s; killed", p.pid, p.svc.StopGrace()))
		return
	}
	// Killed and still there: only an adopted process can reach here, since a
	// child would have been reaped. Stop claiming to supervise it.
	if p.cmd == nil && !procid.Matches(p.pid, p.token) {
		p.running = false
		p.pid = 0
		p.stopping = false
		return
	}
	s.event(p.svc.Name, fmt.Sprintf("process %d survived SIGKILL; it is no longer being supervised", p.pid))
	p.lastError = fmt.Sprintf("process %d survived SIGKILL", p.pid)
	p.running = false
	p.stopping = false
}

// signal sends sig to the process group of pid. Services are started with
// their own session, so the whole tree a service spawned is asked to stop, not
// only its first process.
func signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-pid, sig)
	if err == nil {
		return nil
	}
	// An adopted process may not lead its own group after all; fall back to
	// the process itself rather than give up on stopping it.
	return syscall.Kill(pid, sig)
}

// hasFinished reports whether the loop has already left.
func (s *Supervisor) hasFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// backoff grows with consecutive failures and stops at a ceiling.
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

// tails collects the spool readers, sorted so that captured output arrives in
// a stable order rather than in map order.
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
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// pumpLogs turns what services wrote into records.
func (s *Supervisor) pumpLogs() {
	s.read(s.tails(""), true)
	s.persistOffsets()
}

// drain reads one service's spool to the end. It is what the exit path calls
// so that nothing a service wrote arrives after the event saying it died.
func (s *Supervisor) drain(name string) {
	s.read(s.tails(name), false)
}

// read consumes the given spool readers.
//
// The reads happen under their own lock rather than the supervisor's: a slow
// disk must not hold up a command from hostctl, but two goroutines reading the
// same spool would double-count what they find.
func (s *Supervisor) read(tails []namedTail, recycle bool) {
	s.pumpMu.Lock()
	defer s.pumpMu.Unlock()
	now := s.now()
	for _, t := range tails {
		for {
			records, err := t.tail.Read(now)
			if err != nil {
				s.event(t.name, fmt.Sprintf("could not read captured output: %v", err))
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
			s.event(t.name, fmt.Sprintf("could not recycle the output spool: %v", err))
			continue
		}
		if lost > 0 {
			// Losing output is acceptable when a service outruns the ceiling.
			// Losing it silently is not.
			s.event(t.name, fmt.Sprintf("output spool exceeded its ceiling; %d bytes were discarded", lost))
		}
	}
}

// persistOffsets keeps the recorded read position close to reality, so a
// restarted hostd resumes near where this one stopped.
//
// It writes only when an offset actually moved, and at most once per second
// per service: a supervisor that reserialises its state on every tick spends
// the machine's disk on nothing. The cost of that thrift is bounded and
// deliberate — after an unclean death, up to a second of already captured
// output is read again, which is a duplicate line rather than a lost one.
func (s *Supervisor) persistOffsets() { s.savePositions(persistInterval) }

// persistAllOffsets writes every moved offset regardless of the interval. It
// runs when hostd is on its way out, so the next supervisor resumes from where
// this one really stopped.
func (s *Supervisor) persistAllOffsets() { s.savePositions(0) }

func (s *Supervisor) savePositions(minInterval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, p := range s.procs {
		if !p.running || p.pid == 0 || p.outTail == nil {
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

// event records a fact about a service in the same timeline as its output, so
// that a death and the last lines before it are read together.
func (s *Supervisor) event(name, text string) {
	s.log.Append(logs.Record{
		Time:    s.now(),
		Service: name,
		Stream:  logs.StreamEvent,
		Text:    text,
	})
}
