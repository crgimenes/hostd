package logs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	// The driver is pure Go, so hostd keeps building with CGO_ENABLED=0 and
	// shipping as one static binary.
	_ "modernc.org/sqlite"
)

// Both ceilings are mandatory: unbounded accumulation is a bug, not debt.
const (
	DefaultRetention = 14 * 24 * time.Hour
	DefaultMaxRows   = 2_000_000
)

// Lines arrive from the convergence loop, which must never wait on a disk, so
// one writer goroutine batches them.
const (
	batchSize     = 256
	flushInterval = 5 * time.Millisecond
	// How far the writer may fall behind before appending drops lines rather
	// than holding up the supervisor.
	queueDepth    = 8192
	sweepInterval = time.Minute
)

// Zero in either field means the default.
type Options struct {
	Retention time.Duration
	MaxRows   int64
}

func (o Options) withDefaults() Options {
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.MaxRows <= 0 {
		o.MaxRows = DefaultMaxRows
	}
	return o
}

// Store is the only place log lines live: a copy alongside it would be a
// second implementation of the same capability, and the two would disagree the
// day it mattered.
type Store struct {
	db   *sql.DB
	opts Options

	seq atomic.Uint64

	queue   chan Record
	flushed chan chan struct{}
	closed  chan struct{}
	done    chan struct{}
	once    sync.Once

	// Lines lost because the writer could not keep up. Losing them is a last
	// resort; losing them silently is not allowed.
	dropped atomic.Uint64
	// Touched only by the writer goroutine.
	reported uint64

	mu       sync.Mutex
	watchers map[int]chan Record
	nextID   int
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA cache_size = -16384;
PRAGMA temp_store = MEMORY;
PRAGMA wal_autocheckpoint = 1000;
PRAGMA auto_vacuum = INCREMENTAL;

CREATE TABLE IF NOT EXISTS entries (
	seq     INTEGER PRIMARY KEY,
	time_ms INTEGER NOT NULL,
	service TEXT NOT NULL,
	stream  TEXT NOT NULL,
	kind    TEXT NOT NULL DEFAULT '',
	text    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS entries_time    ON entries(time_ms);
CREATE INDEX IF NOT EXISTS entries_service ON entries(service, seq);
CREATE INDEX IF NOT EXISTS entries_kind    ON entries(kind, seq) WHERE kind <> '';

-- External content: the index reads from the table instead of keeping a
-- second copy of every line.
CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
	text,
	content = 'entries',
	content_rowid = 'seq',
	tokenize = 'unicode61'
);

CREATE TRIGGER IF NOT EXISTS entries_ai AFTER INSERT ON entries BEGIN
	INSERT INTO entries_fts(rowid, text) VALUES (new.seq, new.text);
END;
CREATE TRIGGER IF NOT EXISTS entries_ad AFTER DELETE ON entries BEGIN
	INSERT INTO entries_fts(entries_fts, rowid, text) VALUES ('delete', old.seq, old.text);
END;
`

// The directory must already exist.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// Writers serialise on SQLite's lock anyway; one connection makes the
	// ordering explicit instead of leaving it to the pool.
	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx, schema)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare log store %s: %w", path, err)
	}

	s := &Store{
		db:       db,
		opts:     opts.withDefaults(),
		queue:    make(chan Record, queueDepth),
		flushed:  make(chan chan struct{}),
		closed:   make(chan struct{}),
		done:     make(chan struct{}),
		watchers: make(map[int]chan Record),
	}

	// Continuing the sequence across restarts is what keeps a follower's
	// "everything after N" meaning the same thing.
	var highest int64
	err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM entries`).Scan(&highest)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	// #nosec G115 -- audited: a sequence reaching 2^63 needs 9.2e18 lines
	s.seq.Store(uint64(highest))

	go s.writeLoop()
	return s, nil
}

func (s *Store) Close() error {
	s.once.Do(func() {
		close(s.closed)
		<-s.done
	})
	return s.db.Close()
}

// Append does not wait for the disk: the caller is the supervision loop. The
// line reaches followers at once and is written in the next batch.
func (s *Store) Append(r Record) Record {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	r.Seq = s.seq.Add(1)

	// Under the same lock that unregisters a watcher, which is what makes
	// closing its channel safe; the send never blocks, so it cannot stall.
	s.mu.Lock()
	for _, ch := range s.watchers {
		select {
		case ch <- r:
		default:
		}
	}
	s.mu.Unlock()

	select {
	case s.queue <- r:
	default:
		s.dropped.Add(1)
	}
	return r
}

// What -debug reports: a queue that stays full is the warning that comes
// before the loss.
type Stats struct {
	Queued  int
	Dropped uint64
}

func (s *Store) Stats() Stats {
	return Stats{Queued: len(s.queue), Dropped: s.dropped.Load()}
}

// writeLoop is the only writer: one transaction per batch, not per line.
func (s *Store) writeLoop() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()

	batch := make([]Record, 0, batchSize)
	for {
		select {
		case r := <-s.queue:
			batch = append(batch, r)
			if len(batch) >= batchSize {
				batch = s.flush(batch)
			}
		case <-ticker.C:
			batch = s.flush(batch)
		case <-sweep.C:
			batch = s.flush(batch)
			s.sweep()
			s.reportDropped()
		case waiter := <-s.flushed:
			// Drain first, so a reader sees its own writes.
			for {
				select {
				case r := <-s.queue:
					batch = append(batch, r)
					continue
				default:
				}
				break
			}
			batch = s.flush(batch)
			close(waiter)
		case <-s.closed:
			for {
				select {
				case r := <-s.queue:
					batch = append(batch, r)
					continue
				default:
				}
				break
			}
			s.flush(batch)
			return
		}
	}
}

func (s *Store) flush(batch []Record) []Record {
	if len(batch) == 0 {
		return batch
	}
	err := s.writeBatch(batch)
	if err != nil {
		// The log store is what hostd reports through, so a failure to write
		// it has only the journal of whatever started hostd left.
		_, _ = fmt.Fprintf(stderr, "hostd: could not write %d log lines: %v\n", len(batch), err)
		s.dropped.Add(uint64(len(batch)))
	}
	return batch[:0]
}

func (s *Store) writeBatch(batch []Record) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO entries (
			seq,     -- 1
			time_ms, -- 2
			service, -- 3
			stream,  -- 4
			kind,    -- 5
			text     -- 6
		) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range batch {
		// #nosec G115 -- audited: a sequence reaching 2^63 needs 9.2e18 lines
		_, err = stmt.ExecContext(ctx,
			int64(r.Seq),       // 1
			r.Time.UnixMilli(), // 2
			r.Service,          // 3
			r.Stream,           // 4
			r.Kind,             // 5
			r.Text,             // 6
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Flush waits until everything appended is on disk. Search calls it so a
// reader never fails to find a line it was just told about.
func (s *Store) Flush() {
	waiter := make(chan struct{})
	select {
	case s.flushed <- waiter:
		<-waiter
	case <-s.closed:
	}
}

func (s *Store) Search(q Query) ([]Record, error) {
	s.Flush()

	where := []string{"1 = 1"}
	var args []any
	if q.Service != "" {
		where = append(where, "e.service = ?")
		args = append(args, q.Service)
	}
	if q.Stream != "" {
		where = append(where, "e.stream = ?")
		args = append(args, q.Stream)
	}
	if q.Kind != "" {
		where = append(where, "e.kind = ?")
		args = append(args, q.Kind)
	}
	if q.Since > 0 {
		where = append(where, "e.seq > ?")
		// #nosec G115 -- audited: a sequence reaching 2^63 needs 9.2e18 lines
		args = append(args, int64(q.Since))
	}
	from := "entries e"
	if q.Match != "" {
		from = "entries e JOIN entries_fts f ON f.rowid = e.seq"
		where = append(where, "entries_fts MATCH ?")
		args = append(args, ftsQuery(q.Match))
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	args = append(args, limit)

	// Every fragment concatenated here is a literal from this function; every
	// value the caller supplied went into args as a placeholder. The numbered
	// column convention does not apply to a built statement, and is not needed:
	// each predicate appends its placeholder and its argument together, so the
	// two cannot fall out of step.
	// Newest first with a limit, then reversed: a limit keeps the most recent
	// lines, which is what somebody looking at a problem wants.
	// #nosec G202 -- audited: concatenates only literals, never caller input
	query := "SELECT e.seq, e.time_ms, e.service, e.stream, e.kind, e.text FROM " + from +
		" WHERE " + strings.Join(where, " AND ") + " ORDER BY e.seq DESC LIMIT ?"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search logs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Record, 0, limit)
	for rows.Next() {
		var r Record
		var ms int64
		var seq int64
		err = rows.Scan(&seq, &ms, &r.Service, &r.Stream, &r.Kind, &r.Text)
		if err != nil {
			return nil, err
		}
		// #nosec G115 -- audited: seq is a positive counter written by this store
		r.Seq = uint64(seq)
		r.Time = time.UnixMilli(ms)
		out = append(out, r)
	}
	err = rows.Err()
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// FTS5 has its own query language, where a stray quote or operator is a syntax
// error. An operator types a fragment of a log line and must get results, not
// a parser complaint, so the input becomes a quoted phrase.
func ftsQuery(match string) string {
	return `"` + strings.ReplaceAll(match, `"`, `""`) + `"*`
}

// The returned function stops the delivery and must be called.
func (s *Store) Watch(bufferSize int) (<-chan Record, func()) {
	if bufferSize < 1 {
		bufferSize = 1
	}
	ch := make(chan Record, bufferSize)
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.watchers[id] = ch
	s.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.watchers, id)
			s.mu.Unlock()
			close(ch)
		})
	}
}

// Both destinations are needed: the event puts the loss in the timeline an
// operator reads, and stderr survives a store that is itself failing.
func (s *Store) reportDropped() {
	total := s.dropped.Load()
	if total == s.reported {
		return
	}
	lost := total - s.reported
	s.reported = total
	text := fmt.Sprintf("lost %d log lines since the last report: the writer could not keep up or could not write", lost)
	_, _ = fmt.Fprintf(stderr, "hostd: %s\n", text)
	s.Append(Record{Service: "hostd", Stream: StreamEvent, Kind: EventLogDropped, Text: text})
}

// Runs on the writer goroutine, so it cannot race with a batch.
func (s *Store) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cutoff := time.Now().Add(-s.opts.Retention).UnixMilli()
	_, err := s.db.ExecContext(ctx, `DELETE FROM entries WHERE time_ms < ?`, cutoff)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hostd: could not apply log retention: %v\n", err)
		return
	}
	// Age alone would let one very loud day fill the disk.
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM entries WHERE seq <= (
			SELECT seq FROM entries ORDER BY seq DESC LIMIT 1 OFFSET ?
		)`, s.opts.MaxRows)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_, _ = fmt.Fprintf(stderr, "hostd: could not apply the log row limit: %v\n", err)
		return
	}
	// Deleting rows does not return the space; without this the file only grows.
	_, err = s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hostd: could not reclaim log space: %v\n", err)
	}
}
