package logs

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "logs.db"), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func search(t *testing.T, s *Store, q Query) []Record {
	t.Helper()
	records, err := s.Search(q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return records
}

// A reader must never fail to find a line it has just been told about, even
// though writes are batched.
func TestReadYourOwnWrites(t *testing.T) {
	s := openStore(t, Options{})
	s.Append(Record{Service: "api", Stream: StreamOut, Text: "just written"})
	got := search(t, s, Query{})
	if len(got) != 1 || got[0].Text != "just written" {
		t.Fatalf("a line appended a moment ago was not found: %#v", got)
	}
}

func TestSequenceIsMonotonic(t *testing.T) {
	s := openStore(t, Options{})
	first := s.Append(Record{Text: "a"})
	second := s.Append(Record{Text: "b"})
	if first.Seq == 0 || second.Seq != first.Seq+1 {
		t.Fatalf("sequence is not monotonic: %d then %d", first.Seq, second.Seq)
	}
}

// Sequence numbers continue across a restart, so two lines never share one and
// a follower's "everything after N" keeps meaning what it meant.
func TestSequenceContinuesAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.db")

	first, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	last := first.Append(Record{Text: "before"})
	err = first.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()

	next := second.Append(Record{Text: "after"})
	if next.Seq <= last.Seq {
		t.Fatalf("sequence restarted: %d then %d", last.Seq, next.Seq)
	}
	// And what was written before is still there: the point of a store.
	got := search(t, second, Query{})
	if len(got) != 2 || got[0].Text != "before" {
		t.Fatalf("history did not survive the restart: %#v", got)
	}
}

func TestSearchFilters(t *testing.T) {
	s := openStore(t, Options{})
	s.Append(Record{Service: "api", Stream: StreamOut, Text: "listening on 8080"})
	s.Append(Record{Service: "api", Stream: StreamErr, Text: "connection timeout"})
	s.Append(Record{Service: "web", Stream: StreamOut, Text: "timeout talking to api"})
	s.Append(Record{Service: "api", Stream: StreamEvent, Kind: EventExited, Text: "exited with code 3"})

	cases := []struct {
		why   string
		query Query
		want  int
	}{
		{"by service", Query{Service: "api"}, 3},
		{"by stream", Query{Stream: StreamErr}, 1},
		{"by event kind", Query{Kind: EventExited}, 1},
		{"by text", Query{Match: "timeout"}, 2},
		{"by service and text", Query{Service: "api", Match: "timeout"}, 1},
	}
	for _, c := range cases {
		got := search(t, s, c.query)
		if len(got) != c.want {
			t.Errorf("%s: got %d records, want %d: %#v", c.why, len(got), c.want, got)
		}
	}
}

// A limit keeps the most recent lines, which is what somebody looking at a
// problem wants, and the result still reads oldest first.
func TestSearchLimitKeepsTheMostRecent(t *testing.T) {
	s := openStore(t, Options{})
	for i := range 10 {
		s.Append(Record{Service: "api", Stream: StreamOut, Text: fmt.Sprint(i)})
	}
	got := search(t, s, Query{Limit: 3})
	if len(got) != 3 {
		t.Fatalf("got %d records", len(got))
	}
	if got[0].Text != "7" || got[2].Text != "9" {
		t.Fatalf("limit did not keep the most recent, in order: %#v", got)
	}
}

func TestSearchSinceResumesWithoutRepeating(t *testing.T) {
	s := openStore(t, Options{})
	first := s.Append(Record{Service: "api", Text: "one"})
	s.Append(Record{Service: "api", Text: "two"})
	got := search(t, s, Query{Since: first.Seq})
	if len(got) != 1 || got[0].Text != "two" {
		t.Fatalf("Since returned %#v", got)
	}
}

// An operator types a fragment of a log line, not a query language. Punctuation
// that means something to the index must not turn a search into a syntax error.
func TestSearchAcceptsWhatAPersonTypes(t *testing.T) {
	s := openStore(t, Options{})
	s.Append(Record{Service: "api", Text: `panic: runtime error: index out of range [3]`})
	s.Append(Record{Service: "api", Text: `GET /users?id=1 200`})
	s.Append(Record{Service: "api", Text: `dial tcp 10.0.0.1:5432: connect: refused`})

	for _, term := range []string{
		"panic:",
		"index out of range",
		`"quoted"`,
		"10.0.0.1:5432",
		"users?id=1",
		"connect: refused",
		"AND",
		"*",
		"(unbalanced",
	} {
		_, err := s.Search(Query{Match: term})
		if err != nil {
			t.Errorf("searching for %q failed instead of returning results: %v", term, err)
		}
	}

	got := search(t, s, Query{Match: "index out of range"})
	if len(got) != 1 {
		t.Fatalf("a plain phrase found %d records", len(got))
	}
}

func TestTimeSurvivesTheStore(t *testing.T) {
	s := openStore(t, Options{})
	when := time.Date(2026, 8, 22, 15, 4, 5, 123_000_000, time.UTC)
	s.Append(Record{Service: "api", Time: when, Text: "x"})
	got := search(t, s, Query{})
	if len(got) != 1 {
		t.Fatalf("got %d records", len(got))
	}
	if !got[0].Time.Equal(when) {
		t.Fatalf("time changed in the store: stored %s, read %s", when, got[0].Time.UTC())
	}
}

func TestAppendFillsTheTime(t *testing.T) {
	s := openStore(t, Options{})
	r := s.Append(Record{Service: "api", Text: "x"})
	if r.Time.IsZero() {
		t.Fatal("a record with no time was stored")
	}
}

func TestWatch(t *testing.T) {
	s := openStore(t, Options{})
	ch, stop := s.Watch(4)
	defer stop()
	s.Append(Record{Service: "api", Text: "hello"})
	select {
	case r := <-ch:
		if r.Text != "hello" {
			t.Fatalf("watcher got %q", r.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher received nothing")
	}
}

// A watcher that stops while writers are busy must not take the process down.
func TestWatchStopIsSafeUnderLoad(t *testing.T) {
	s := openStore(t, Options{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				s.Append(Record{Service: "api", Text: "x"})
			}
		})
	}
	for range 20 {
		_, stop := s.Watch(1)
		stop()
		stop() // stopping twice must not close the channel twice
	}
	wg.Wait()
}

// Retention by age is a ceiling that exists from the first day, not something
// switched on later when the disk fills.
func TestRetentionDropsOldLines(t *testing.T) {
	s := openStore(t, Options{Retention: time.Hour})
	old := time.Now().Add(-2 * time.Hour)
	s.Append(Record{Service: "api", Time: old, Text: "ancient"})
	s.Append(Record{Service: "api", Text: "recent"})

	s.Flush()
	s.sweep()

	got := search(t, s, Query{})
	if len(got) != 1 || got[0].Text != "recent" {
		t.Fatalf("retention kept the wrong lines: %#v", got)
	}
}

func countRows(s *Store) (int64, error) {
	s.Flush()
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&n)
	return n, err
}

// The age limit alone would let one very loud day fill the disk.
func TestRetentionCapsTheRowCount(t *testing.T) {
	s := openStore(t, Options{MaxRows: 50})
	for i := range 200 {
		s.Append(Record{Service: "api", Text: fmt.Sprint(i)})
	}
	s.Flush()
	s.sweep()

	count, err := countRows(s)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count > 50 {
		t.Fatalf("the store holds %d records, past its ceiling of 50", count)
	}
	// It keeps the most recent, not an arbitrary slice.
	got := search(t, s, Query{Limit: 1})
	if len(got) != 1 || got[0].Text != "199" {
		t.Fatalf("retention dropped the newest lines: %#v", got)
	}
}

// The full-text index has to forget what the table forgot, or a search would
// return rows that are no longer there.
func TestRetentionAlsoClearsTheSearchIndex(t *testing.T) {
	s := openStore(t, Options{Retention: time.Hour})
	s.Append(Record{Service: "api", Time: time.Now().Add(-2 * time.Hour), Text: "forgettable-token"})
	s.Append(Record{Service: "api", Text: "kept"})
	s.Flush()
	s.sweep()

	got := search(t, s, Query{Match: "forgettable-token"})
	if len(got) != 0 {
		t.Fatalf("a swept line is still findable: %#v", got)
	}
}

// Losing lines because the writer fell behind is a last resort. Losing them
// silently is not allowed.
func TestOverflowIsCounted(t *testing.T) {
	s := openStore(t, Options{})
	// Fill the queue far past its depth without giving the writer a chance to
	// drain it.
	for range queueDepth * 2 {
		s.Append(Record{Service: "api", Text: "flood"})
	}
	// Whether the writer kept up depends on the machine, so this asserts the
	// accounting rather than the loss: whatever was dropped was counted.
	s.Flush()
	count, err := countRows(s)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if uint64(count)+s.Stats().Dropped != uint64(queueDepth*2) {
		t.Fatalf("%d stored plus %d dropped does not account for %d appended",
			count, s.Stats().Dropped, queueDepth*2)
	}
}

// A store that cannot be written must say so where somebody will see it.
func TestWriteFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.db")
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var reported bytes.Buffer
	old := stderr
	stderr = &reported
	defer func() { stderr = old }()

	// Closing the database underneath the writer is the bluntest way to make
	// the next write fail.
	err = s.db.Close()
	if err != nil {
		t.Fatalf("close db: %v", err)
	}
	s.Append(Record{Service: "api", Text: "into the void"})
	s.Flush()

	if !strings.Contains(reported.String(), "could not write") {
		t.Fatalf("a failed write was not reported: %q", reported.String())
	}
	if s.Stats().Dropped == 0 {
		t.Fatal("lines lost to a failed write were not counted")
	}
}

func TestOpenFailsLoudlyOnAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere, so this cannot be exercised")
	}
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "absent", "logs.db"), Options{})
	if err == nil {
		t.Fatal("opening a store in a missing directory succeeded")
	}
}

func BenchmarkAppend(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(context.Background(), filepath.Join(dir, "logs.db"), Options{})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	line := "2026-08-22 15:04:05 GET /users/42 200 1.2ms"
	b.ResetTimer()
	for range b.N {
		s.Append(Record{Service: "api", Stream: StreamOut, Text: line})
	}
	s.Flush()
}

func BenchmarkSearch(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(context.Background(), filepath.Join(dir, "logs.db"), Options{})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	for i := range 200_000 {
		s.Append(Record{
			Service: "api",
			Stream:  StreamOut,
			Text:    fmt.Sprintf("GET /users/%d 200 %dms", i, i%50),
		})
	}
	s.Flush()

	b.ResetTimer()
	for range b.N {
		_, err = s.Search(Query{Match: "/users/199999", Limit: 50})
		if err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}

// Counting a loss is half the rule; the other half is saying so where an
// operator and an agent both look.
func TestDroppedLinesAreReported(t *testing.T) {
	s := openStore(t, Options{})

	var reported bytes.Buffer
	old := stderr
	stderr = &reported
	defer func() { stderr = old }()

	s.dropped.Add(7)
	s.reportDropped()

	if !strings.Contains(reported.String(), "lost 7 log lines") {
		t.Fatalf("a loss was not reported on stderr: %q", reported.String())
	}
	got := search(t, s, Query{Kind: EventLogDropped})
	if len(got) != 1 {
		t.Fatalf("a loss left no event in the timeline: %#v", got)
	}

	// Nothing new lost, nothing new said: a repeated report would drown the
	// one that matters.
	reported.Reset()
	s.reportDropped()
	if reported.Len() != 0 {
		t.Fatalf("a second report with nothing new to say: %q", reported.String())
	}
}

// A machine that has been running carries a table an older hostd built. Losing
// its history to a new column would be a strange way to keep the promise that
// the history is the point.
func TestAnOlderStoreGainsWhatIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = old.Exec(`CREATE TABLE entries (
		seq INTEGER PRIMARY KEY, time_ms INTEGER NOT NULL, service TEXT NOT NULL,
		stream TEXT NOT NULL, kind TEXT NOT NULL DEFAULT '', text TEXT NOT NULL)`)
	if err != nil {
		t.Fatalf("build the old table: %v", err)
	}
	_, err = old.Exec(`INSERT INTO entries (seq, time_ms, service, stream, kind, text)
		VALUES (1, ?, 'api', 'stdout', '', 'from before')`, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("write history: %v", err)
	}
	err = old.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("a store from an older hostd could not be opened: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The history is still there, and the store writes again.
	got := search(t, store, Query{})
	if len(got) != 1 || got[0].Text != "from before" {
		t.Fatalf("the history did not survive: %#v", got)
	}
	store.Append(Record{Service: "api", Stream: StreamOut, Run: "42", Text: "from now"})
	got = search(t, store, Query{Run: "42"})
	if len(got) != 1 || got[0].Text != "from now" {
		t.Fatalf("the store did not write into the table it just changed: %#v", got)
	}
}
