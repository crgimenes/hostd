package metrics

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
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

// Two resolutions, decided on the first day rather than when the disk fills.
// Full detail for the window an operator looks at while something is wrong,
// one minute for the history that answers what happened overnight.
const (
	RawStep    = int64(SampleInterval / time.Millisecond)
	MinuteStep = int64(time.Minute / time.Millisecond)

	rawRetention  = 6 * time.Hour
	sweepInterval = time.Minute
)

// A query that asked for everything would cost the daemon what a client
// forgot to bound.
const (
	defaultPoints = 720
	maxPoints     = 20_000
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

type Store struct {
	db   *sql.DB
	opts Options

	// Maintenance rides on the writes: no data arriving means nothing to roll
	// up or expire, and no goroutine to shut down.
	lastSweep time.Time
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA temp_store = MEMORY;
PRAGMA auto_vacuum = INCREMENTAL;

CREATE TABLE IF NOT EXISTS samples (
	step_ms INTEGER NOT NULL,
	metric  TEXT NOT NULL,
	scope   TEXT NOT NULL,
	name    TEXT NOT NULL,
	time_ms INTEGER NOT NULL,
	value   REAL NOT NULL,
	PRIMARY KEY (step_ms, metric, scope, name, time_ms)
) WITHOUT ROWID;
`

// The directory must already exist.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, schema)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare metric store %s: %w", path, err)
	}
	return &Store{db: db, opts: opts.withDefaults()}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// One transaction per tick: a few dozen rows every ten seconds is not worth a
// writer goroutine and a queue that can overflow.
func (s *Store) Append(samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Replace rather than fail: re-sampling the same instant is a repeated
	// reading, not a second truth.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO samples (
			step_ms, -- 1
			metric,  -- 2
			scope,   -- 3
			name,    -- 4
			time_ms, -- 5
			value    -- 6
		) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	latest := samples[0].Time
	for _, sample := range samples {
		_, err = stmt.ExecContext(ctx,
			RawStep,                 // 1
			sample.Metric,           // 2
			sample.Scope,            // 3
			sample.Name,             // 4
			sample.Time.UnixMilli(), // 5
			sample.Value,            // 6
		)
		if err != nil {
			return err
		}
		if sample.Time.After(latest) {
			latest = sample.Time
		}
	}
	err = tx.Commit()
	if err != nil {
		return err
	}
	return s.maintain(latest)
}

func (s *Store) maintain(now time.Time) error {
	if now.Sub(s.lastSweep) < sweepInterval {
		return nil
	}
	s.lastSweep = now
	return s.Sweep(now)
}

// Sweep folds full-resolution samples into minute averages and drops what is
// past its retention. Exported because a test proves the ceiling holds without
// waiting an hour for it.
func (s *Store) Sweep(now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// yagni: the average of the minute, so a one-tick spike is smoothed away
	// in the history; keep max alongside it when a spike is what someone came
	// looking for.
	foldBefore := now.Add(-rawRetention).UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO samples (step_ms, metric, scope, name, time_ms, value)
		SELECT ?, metric, scope, name, (time_ms / ?) * ?, AVG(value)
		FROM samples
		WHERE step_ms = ? AND time_ms < ?
		GROUP BY metric, scope, name, time_ms / ?`,
		MinuteStep, MinuteStep, MinuteStep, RawStep, foldBefore, MinuteStep)
	if err != nil {
		return fmt.Errorf("roll up metrics: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM samples WHERE step_ms = ? AND time_ms < ?`, RawStep, foldBefore)
	if err != nil {
		return fmt.Errorf("drop rolled-up metrics: %w", err)
	}

	expireBefore := now.Add(-s.opts.Retention).UnixMilli()
	_, err = s.db.ExecContext(ctx, `DELETE FROM samples WHERE time_ms < ?`, expireBefore)
	if err != nil {
		return fmt.Errorf("apply metric retention: %w", err)
	}
	// Age alone would let a fleet of services outgrow the disk before the
	// retention window ever came round.
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM samples WHERE time_ms < (
			SELECT time_ms FROM samples ORDER BY time_ms DESC LIMIT 1 OFFSET ?
		)`, s.opts.MaxRows)
	if err != nil {
		return fmt.Errorf("apply the metric row limit: %w", err)
	}
	// Deleting rows does not return the space; without this the file only grows.
	_, err = s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	if err != nil {
		return fmt.Errorf("reclaim metric space: %w", err)
	}
	return nil
}

type Query struct {
	Scope  string
	Name   string
	Metric string
	From   time.Time
	To     time.Time
	// Milliseconds per point. Zero picks the resolution that still exists for
	// the window asked about.
	StepMS int64
	// Points per answer, not per series: what bounds the response is what the
	// daemon has to render.
	Limit int
}

type Point struct {
	TimeMS float64 `filo:"time-ms" json:"time-ms"`
	Value  float64 `filo:"value" json:"value"`
}

// Times are milliseconds on the wire: every Filo number is a float64, exact
// only to 2^53, so anything finer would arrive rounded.
type Series struct {
	Scope  string  `filo:"scope" json:"scope"`
	Name   string  `filo:"name" json:"name"`
	Metric string  `filo:"metric" json:"metric"`
	StepMS float64 `filo:"step-ms" json:"step-ms"`
	Points []Point `filo:"points" json:"points"`
}

// Full detail only covers the recent past; a window that reaches further back
// is answered from the minute series rather than from a hole.
func (q Query) step() int64 {
	if q.StepMS > 0 {
		return q.StepMS
	}
	if !q.From.IsZero() && time.Since(q.From) > rawRetention {
		return MinuteStep
	}
	return RawStep
}

// What both answers filter on. Every predicate appends its placeholder and its
// argument in the same step, so the two cannot fall out of step.
func (q Query) predicates(step int64) (where []string, args []any) {
	where = []string{"step_ms = ?"}
	args = []any{step}
	if q.Scope != "" {
		where = append(where, "scope = ?")
		args = append(args, q.Scope)
	}
	if q.Name != "" {
		where = append(where, "name = ?")
		args = append(args, q.Name)
	}
	if q.Metric != "" {
		where = append(where, "metric = ?")
		args = append(args, q.Metric)
	}
	return where, args
}

func (s *Store) Query(q Query) ([]Series, error) {
	where, args := q.predicates(q.step())
	if !q.From.IsZero() {
		where = append(where, "time_ms >= ?")
		args = append(args, q.From.UnixMilli())
	}
	if !q.To.IsZero() {
		where = append(where, "time_ms <= ?")
		args = append(args, q.To.UnixMilli())
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultPoints
	}
	if limit > maxPoints {
		limit = maxPoints
	}
	args = append(args, limit)

	// Every fragment concatenated here is a literal from this function; each
	// predicate appends its placeholder and its argument in the same step, so
	// the two cannot fall out of step.
	// Newest first under the limit, then reversed: a truncated answer keeps
	// the recent end, which is the one somebody is looking at.
	// #nosec G202 -- audited: concatenates only literals, never caller input
	query := `SELECT scope, name, metric, time_ms, value FROM samples WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY time_ms DESC LIMIT ?`

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	step := float64(q.step())
	var out []Series
	index := make(map[string]int)
	for rows.Next() {
		var scope, name, metric string
		var timeMS int64
		var value float64
		err = rows.Scan(&scope, &name, &metric, &timeMS, &value)
		if err != nil {
			return nil, err
		}
		key := scope + "\x00" + name + "\x00" + metric
		at, seen := index[key]
		if !seen {
			at = len(out)
			index[key] = at
			out = append(out, Series{Scope: scope, Name: name, Metric: metric, StepMS: step})
		}
		out[at].Points = append(out[at].Points, Point{TimeMS: float64(timeMS), Value: value})
	}
	err = rows.Err()
	if err != nil {
		return nil, err
	}
	for i := range out {
		reversePoints(out[i].Points)
	}
	// The same data must come back in the same order for a person reading a
	// table and for a client diffing two answers; row order out of the query
	// follows whichever series was written last.
	slices.SortFunc(out, func(a, b Series) int {
		return cmp.Or(
			strings.Compare(a.Scope, b.Scope),
			strings.Compare(a.Name, b.Name),
			strings.Compare(a.Metric, b.Metric),
		)
	})
	return out, nil
}

func reversePoints(points []Point) {
	for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
		points[i], points[j] = points[j], points[i]
	}
}

// Latest is what a person asking "what is this machine doing" gets: the newest
// value of every series, in one row each. It narrows the same way a window
// does, so asking about one service answers about that service.
func (s *Store) Latest(q Query, within time.Duration) ([]Series, error) {
	where, args := q.predicates(RawStep)
	where = append(where, "time_ms >= ?")
	args = append(args, time.Now().Add(-within).UnixMilli())

	// #nosec G202 -- audited: concatenates only literals, never caller input
	query := `
		SELECT scope, name, metric, time_ms, value FROM (
			SELECT scope, name, metric, time_ms, value,
			       ROW_NUMBER() OVER (PARTITION BY scope, name, metric ORDER BY time_ms DESC) AS rank
			FROM samples
			WHERE ` + strings.Join(where, " AND ") + `
		) WHERE rank = 1
		ORDER BY scope, name, metric`

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the latest metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Series
	for rows.Next() {
		var series Series
		var timeMS int64
		var value float64
		err = rows.Scan(&series.Scope, &series.Name, &series.Metric, &timeMS, &value)
		if err != nil {
			return nil, err
		}
		series.StepMS = float64(RawStep)
		series.Points = []Point{{TimeMS: float64(timeMS), Value: value}}
		out = append(out, series)
	}
	return out, rows.Err()
}
