package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, dir
}

// A pristine host is not generation zero, because zero on the wire means "I
// make no claim". Otherwise a caller could not tell the two apart.
func TestPristineHostIsNotGenerationZero(t *testing.T) {
	s, _ := open(t)
	if s.Generation() != FirstGeneration {
		t.Fatalf("generation = %d, want %d", s.Generation(), FirstGeneration)
	}
	if FirstGeneration == 0 {
		t.Fatal("the first generation must not collide with the no-claim value")
	}
}

func TestOnlyAcceptedOperationsMoveTheGeneration(t *testing.T) {
	s, _ := open(t)
	start := s.Generation()

	got := s.Record(Entry{Operation: "service.stop", Target: "api", Result: ResultOK}, true)
	if got != start+1 || s.Generation() != start+1 {
		t.Fatalf("an accepted operation left the generation at %d", s.Generation())
	}
	for _, result := range []string{ResultRefused, ResultFailed} {
		before := s.Generation()
		s.Record(Entry{Operation: "service.start", Target: "api", Result: result}, true)
		if s.Generation() != before {
			t.Errorf("a %s operation moved the generation to %d", result, s.Generation())
		}
	}
}

func TestCheck(t *testing.T) {
	s, _ := open(t)
	current := s.Generation()

	err := s.Check(0)
	if err != nil {
		t.Fatalf("claiming nothing was refused: %v", err)
	}
	err = s.Check(current)
	if err != nil {
		t.Fatalf("the current generation was refused: %v", err)
	}
	err = s.Check(current + 1)
	if err == nil {
		t.Fatal("a wrong generation was accepted")
	}
	conflict, ok := err.(ErrConflict)
	if !ok {
		t.Fatalf("error is not ErrConflict: %T", err)
	}
	if conflict.Current != current {
		t.Fatalf("conflict reports current %d, want %d", conflict.Current, current)
	}
	// The message has to say what to do, not only that something is wrong.
	if !strings.Contains(err.Error(), "hostctl status") {
		t.Fatalf("the message does not say how to recover: %v", err)
	}
}

func TestAuditRecordsRefusalsToo(t *testing.T) {
	s, _ := open(t)
	s.Record(Entry{Operation: "apply", Result: ResultOK, Detail: "2 changes"}, true)
	s.Record(Entry{Operation: "service.stop", Target: "api", Result: ResultRefused, Detail: "stale generation"}, true)

	entries := s.Recent(0)
	if len(entries) != 2 {
		t.Fatalf("audit has %d entries, want 2", len(entries))
	}
	// A record of what was attempted is worth as much as a record of what
	// happened.
	if entries[1].Result != ResultRefused {
		t.Fatalf("the refusal was not audited: %+v", entries[1])
	}
	if entries[0].Seq >= entries[1].Seq {
		t.Fatalf("audit sequence is not monotonic: %d then %d", entries[0].Seq, entries[1].Seq)
	}
	if entries[0].Actor != ActorLocal {
		t.Fatalf("actor was not filled in: %+v", entries[0])
	}
	if entries[0].TimeMS == 0 {
		t.Fatal("audit entry has no time")
	}
}

func TestRecentIsBounded(t *testing.T) {
	s, _ := open(t)
	for range maxAuditMemory + 50 {
		s.Record(Entry{Operation: "service.start", Target: "api", Result: ResultOK}, true)
	}
	// Accumulated data with no ceiling is a bug, not technical debt.
	got := len(s.Recent(0))
	if got != maxAuditMemory {
		t.Fatalf("audit holds %d entries, want its ceiling of %d", got, maxAuditMemory)
	}
	got = len(s.Recent(10))
	if got != 10 {
		t.Fatalf("limit returned %d entries", got)
	}
}

// The generation has to survive a restart of the daemon, or an operator's
// claim would be meaningless across one.
func TestGenerationSurvivesReopen(t *testing.T) {
	s, dir := open(t)
	for range 3 {
		s.Record(Entry{Operation: "apply", Result: ResultOK}, true)
	}
	want := s.Generation()

	reopened, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Generation() != want {
		t.Fatalf("generation after reopen = %d, want %d", reopened.Generation(), want)
	}
	// The audit sequence continues rather than restarting, so two entries
	// never share a number.
	reopened.Record(Entry{Operation: "apply", Result: ResultOK}, true)
	entries := reopened.Recent(1)
	if len(entries) != 1 || entries[0].Seq <= 3 {
		t.Fatalf("audit sequence restarted: %+v", entries)
	}
}

// A file from an older hostd must not zero the fields it does not carry.
func TestOlderSnapshotKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "generation.filo"),
		[]byte(`(list (tuple "generation" 7))`), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Generation() != 7 {
		t.Fatalf("generation = %d, want 7", s.Generation())
	}
}

func TestAuditFileRotatesAtItsCeiling(t *testing.T) {
	s, dir := open(t)
	detail := strings.Repeat("x", 4096)
	for range (maxAuditBytes / 4096) + 10 {
		s.Record(Entry{Operation: "apply", Result: ResultOK, Detail: detail}, true)
	}
	info, err := os.Stat(filepath.Join(dir, "audit.filo"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() >= maxAuditBytes {
		t.Fatalf("the audit file grew past its ceiling: %d bytes", info.Size())
	}
	// One previous file is kept, so the audit costs at most twice the ceiling
	// and history is not thrown away the moment it rotates.
	_, err = os.Stat(filepath.Join(dir, "audit.filo.prev"))
	if err != nil {
		t.Fatalf("the rotated file was not kept: %v", err)
	}
}

func TestConcurrentRecordsKeepDistinctGenerations(t *testing.T) {
	s, _ := open(t)
	const writers = 8
	const each = 20
	var wg sync.WaitGroup
	seen := make([]uint64, 0, writers*each)
	var mu sync.Mutex
	for range writers {
		wg.Go(func() {
			for range each {
				g := s.Record(Entry{Operation: "service.start", Target: "api", Result: ResultOK}, true)
				mu.Lock()
				seen = append(seen, g)
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	unique := make(map[uint64]bool, len(seen))
	for _, g := range seen {
		if unique[g] {
			t.Fatalf("two operations were given the same generation %d", g)
		}
		unique[g] = true
	}
	if s.Generation() != FirstGeneration+writers*each {
		t.Fatalf("generation = %d, want %d", s.Generation(), FirstGeneration+writers*each)
	}
}

// The counter follows the state, not the attempt. An accepted operation that
// found nothing to do leaves the host where it was, and moving anyway would
// refuse somebody's next expect-generation for a change nobody made.
func TestAnAcceptedOperationThatChangedNothingHoldsTheGeneration(t *testing.T) {
	s, _ := open(t)
	start := s.Generation()

	got := s.Record(Entry{Operation: "service.start", Target: "api", Result: ResultOK}, false)
	if got != start || s.Generation() != start {
		t.Fatalf("an operation that changed nothing moved the generation to %d", s.Generation())
	}
	// Still audited: what was attempted is worth as much as what happened.
	recent := s.Recent(1)
	if len(recent) != 1 || recent[0].Before != start || recent[0].After != start {
		t.Fatalf("the attempt was not audited at the generation it did not change: %#v", recent)
	}
}
