// Package state holds the generation counter and the audit log.
//
// The generation is what stops two operators, or an operator and an agent,
// from overwriting each other. The audit log answers "who stopped the service
// at three in the morning", and is written for every mutation, accepted or not.
package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crgimenes/hostd/filoconf"
)

// One previous audit file is kept, so the audit costs at most twice the
// rotation size. Unbounded accumulation is a bug, not debt.
const (
	maxAuditBytes  = 4 << 20
	maxAuditMemory = 1000
)

// Recorded for a command over the local socket. Once operators authenticate
// with keys, the actor is the key.
const ActorLocal = "local"

type Entry struct {
	Seq    uint64  `filo:"seq"`
	TimeMS float64 `filo:"time-ms"`
	Actor  string  `filo:"actor"`
	// The identity that delegated the work. Agents are not part of the root
	// of trust, so delegation is recorded rather than flattened away.
	OnBehalfOf string `filo:"on-behalf-of"`
	Operation  string `filo:"operation"`
	Target     string `filo:"target"`
	Before     uint64 `filo:"generation-before"`
	After      uint64 `filo:"generation-after"`
	Result     string `filo:"result"`
	Detail     string `filo:"detail"`
}

const (
	ResultOK      = "ok"
	ResultRefused = "refused"
	ResultFailed  = "failed"
)

type Store struct {
	dir string

	mu         sync.Mutex
	generation uint64
	seq        uint64
	recent     []Entry
}

// Decoded into the current value, so a file written by an older hostd cannot
// zero a field it does not carry.
type snapshot struct {
	Generation uint64 `filo:"generation"`
	AuditSeq   uint64 `filo:"audit-seq"`
}

// Not zero: zero on the wire means "I make no claim", and a pristine host at
// generation zero would be indistinguishable from one.
const FirstGeneration = 1

func Open(ctx context.Context, dir string) (*Store, error) {
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, generation: FirstGeneration}
	data, err := os.ReadFile(s.snapshotPath()) // #nosec G304 -- inside hostd's own state directory
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	snap := snapshot{Generation: FirstGeneration}
	err = filoconf.Decode(ctx, "generation.filo", string(data), &snap)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	s.generation = snap.Generation
	s.seq = snap.AuditSeq
	return s, nil
}

func (s *Store) snapshotPath() string { return filepath.Join(s.dir, "generation.filo") }
func (s *Store) auditPath() string    { return filepath.Join(s.dir, "audit.filo") }

func (s *Store) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

type ErrConflict struct {
	Expected uint64
	Current  uint64
}

func (e ErrConflict) Error() string {
	return fmt.Sprintf(
		"the host is at generation %d, not %d: someone or something changed it since you looked; read the current state with hostctl status and try again",
		e.Current, e.Expected)
}

// Zero means the caller claimed nothing, which is allowed: optimistic control
// is a tool, not a toll.
func (s *Store) Check(expected uint64) error {
	if expected == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected != s.generation {
		return ErrConflict{Expected: expected, Current: s.generation}
	}
	return nil
}

// A refused or failed operation is audited too, at the generation it did not
// change: what was attempted is worth as much as what happened.
func (s *Store) Record(e Entry) uint64 {
	s.mu.Lock()
	e.Before = s.generation
	if e.Result == ResultOK {
		s.generation++
	}
	e.After = s.generation
	s.seq++
	e.Seq = s.seq
	if e.TimeMS == 0 {
		e.TimeMS = float64(time.Now().UnixMilli())
	}
	if e.Actor == "" {
		e.Actor = ActorLocal
	}
	s.recent = append(s.recent, e)
	if len(s.recent) > maxAuditMemory {
		s.recent = s.recent[len(s.recent)-maxAuditMemory:]
	}
	generation := s.generation
	s.mu.Unlock()

	// Failing to write the audit must not fail an operation that already
	// happened, but it must be visible.
	err := s.append(e)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "hostd: could not write the audit log: %v\n", err)
	}
	err = s.saveSnapshot(generation, e.Seq)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "hostd: could not save the generation: %v\n", err)
	}
	return generation
}

func (s *Store) append(e Entry) error {
	line, err := filoconf.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.rotateIfNeeded()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.auditPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- inside hostd's own state directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(strings.TrimRight(line, "\n") + "\n")
	return err
}

// At most two audit files, so the log has a ceiling without anyone having to
// remember to trim it.
func (s *Store) rotateIfNeeded() error {
	info, err := os.Stat(s.auditPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxAuditBytes {
		return nil
	}
	return os.Rename(s.auditPath(), s.auditPath()+".prev")
}

func (s *Store) saveSnapshot(generation, seq uint64) error {
	body, err := filoconf.Marshal(snapshot{Generation: generation, AuditSeq: seq})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "generation.*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	_, err = tmp.WriteString(body)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	err = tmp.Sync()
	if err != nil {
		_ = tmp.Close()
		return err
	}
	err = tmp.Close()
	if err != nil {
		return err
	}
	return os.Rename(name, s.snapshotPath())
}

func (s *Store) Recent(limit int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.recent) {
		limit = len(s.recent)
	}
	out := make([]Entry, limit)
	copy(out, s.recent[len(s.recent)-limit:])
	return out
}
