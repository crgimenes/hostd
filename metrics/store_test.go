package metrics

import (
	"slices"
	"testing"
	"time"
)

func appendAt(t *testing.T, s *Store, at time.Time, scope, name, metric string, value float64) {
	t.Helper()
	err := s.Append([]Sample{{Time: at, Scope: scope, Name: name, Metric: metric, Value: value}})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func rowCount(t *testing.T, s *Store, step int64) int {
	t.Helper()
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE step_ms = ?`, step).Scan(&n)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestQueryReturnsOneSeriesPerThingMeasured(t *testing.T) {
	s := openStore(t, Options{})
	now := time.Now()
	appendAt(t, s, now.Add(-2*time.Minute), ScopeHost, "", MetricCPUPercent, 10)
	appendAt(t, s, now.Add(-time.Minute), ScopeHost, "", MetricCPUPercent, 20)
	appendAt(t, s, now.Add(-time.Minute), ScopeService, "api", MetricCPUPercent, 5)

	series, err := s.Query(Query{From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, expected one for the host and one for the service", len(series))
	}
	for _, got := range series {
		if got.Scope == ScopeHost && len(got.Points) != 2 {
			t.Fatalf("the host series holds %d points, expected 2", len(got.Points))
		}
	}
}

// Oldest first: a graph reads left to right, and a caller must not have to
// reverse what it was given.
func TestQueryReturnsPointsInTimeOrder(t *testing.T) {
	s := openStore(t, Options{})
	now := time.Now()
	for i := range 5 {
		appendAt(t, s, now.Add(-time.Duration(i)*time.Minute), ScopeHost, "", MetricLoad1, float64(i))
	}
	series, err := s.Query(Query{From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	points := series[0].Points
	for i := 1; i < len(points); i++ {
		if points[i].TimeMS <= points[i-1].TimeMS {
			t.Fatalf("point %d goes back in time: %#v", i, points)
		}
	}
}

// A truncated answer keeps the recent end, which is the one somebody is
// looking at.
func TestQueryKeepsTheRecentEndWhenItTruncates(t *testing.T) {
	s := openStore(t, Options{})
	now := time.Now()
	for i := range 20 {
		appendAt(t, s, now.Add(-time.Duration(i)*time.Minute), ScopeHost, "", MetricLoad1, float64(i))
	}
	series, err := s.Query(Query{From: now.Add(-time.Hour), To: now, Limit: 3})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	points := series[0].Points
	if len(points) != 3 {
		t.Fatalf("got %d points, expected the limit of 3", len(points))
	}
	// The values were seeded as minutes ago, so the newest three are 2, 1, 0.
	if points[2].Value != 0 {
		t.Fatalf("the newest point is %v, expected the most recent sample", points[2].Value)
	}
}

func TestQueryFiltersByServiceAndMetric(t *testing.T) {
	s := openStore(t, Options{})
	now := time.Now()
	appendAt(t, s, now, ScopeService, "api", MetricCPUPercent, 1)
	appendAt(t, s, now, ScopeService, "api", MetricMemoryBytes, 2)
	appendAt(t, s, now, ScopeService, "db", MetricCPUPercent, 3)

	series, err := s.Query(Query{Scope: ScopeService, Name: "api", Metric: MetricCPUPercent})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(series) != 1 || series[0].Name != "api" || series[0].Metric != MetricCPUPercent {
		t.Fatalf("the filter let something else through: %#v", series)
	}
}

// Full detail is kept for hours, not weeks: what survives is the minute
// average, and asking about yesterday must find it.
func TestSweepFoldsDetailIntoMinutes(t *testing.T) {
	s := openStore(t, Options{})
	now := time.Now()
	old := now.Add(-8 * time.Hour).Truncate(time.Minute)
	appendAt(t, s, old, ScopeHost, "", MetricCPUPercent, 10)
	appendAt(t, s, old.Add(10*time.Second), ScopeHost, "", MetricCPUPercent, 20)
	appendAt(t, s, old.Add(20*time.Second), ScopeHost, "", MetricCPUPercent, 30)
	appendAt(t, s, now, ScopeHost, "", MetricCPUPercent, 99)

	err := s.Sweep(now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if rowCount(t, s, RawStep) != 1 {
		t.Fatalf("full detail past its window survived: %d rows", rowCount(t, s, RawStep))
	}
	series, err := s.Query(Query{StepMS: MinuteStep, From: old.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(series) != 1 || len(series[0].Points) != 1 {
		t.Fatalf("the folded minute is not there: %#v", series)
	}
	if series[0].Points[0].Value != 20 {
		t.Fatalf("the minute holds %v, expected the average of 10, 20 and 30", series[0].Points[0].Value)
	}
}

// A window that reaches past the detail window is answered from the minutes
// rather than from a hole.
// Superseded by the stitched answer: a window is no longer answered from one
// resolution, it takes full detail where that still exists and minutes where
// only minutes remain. What is left to pin here is the explicit override.
func TestAnExplicitStepIsObeyed(t *testing.T) {
	clause, args := (Query{StepMS: MinuteStep}).stepPredicate(time.Now())
	if clause != "step_ms = ?" || len(args) != 1 || args[0] != MinuteStep {
		t.Fatalf("an explicit step was rewritten: %s %v", clause, args)
	}
}

func TestRetentionDropsWhatIsPastItsWindow(t *testing.T) {
	s := openStore(t, Options{Retention: time.Hour})
	now := time.Now()
	appendAt(t, s, now.Add(-2*time.Hour), ScopeHost, "", MetricLoad1, 1)
	appendAt(t, s, now, ScopeHost, "", MetricLoad1, 2)

	err := s.Sweep(now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	series, err := s.Query(Query{StepMS: RawStep, From: now.Add(-24 * time.Hour), To: now})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(series) != 1 || len(series[0].Points) != 1 {
		t.Fatalf("retention kept the wrong thing: %#v", series)
	}
}

// Age alone would let a loud fleet fill the disk before the window came round.
func TestTheRowCeilingHoldsRegardlessOfAge(t *testing.T) {
	s := openStore(t, Options{MaxRows: 10})
	now := time.Now()
	for i := range 50 {
		appendAt(t, s, now.Add(-time.Duration(i)*time.Second), ScopeHost, "", MetricLoad1, float64(i))
	}
	err := s.Sweep(now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	got := rowCount(t, s, RawStep)
	if got > 11 {
		t.Fatalf("the store holds %d rows, past its ceiling of 10", got)
	}
}

// Re-sampling the same instant is a repeated reading, not a second truth.
func TestRepeatedInstantReplacesRatherThanFails(t *testing.T) {
	s := openStore(t, Options{})
	at := time.Now()
	appendAt(t, s, at, ScopeHost, "", MetricLoad1, 1)
	appendAt(t, s, at, ScopeHost, "", MetricLoad1, 2)

	series, err := s.Query(Query{StepMS: RawStep})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(series[0].Points) != 1 || series[0].Points[0].Value != 2 {
		t.Fatalf("the repeated instant was not replaced: %#v", series[0].Points)
	}
}

func TestLatestReturnsTheNewestOfEachSeries(t *testing.T) {
	s := openStore(t, Options{})
	now := time.Now()
	appendAt(t, s, now.Add(-time.Minute), ScopeHost, "", MetricLoad1, 1)
	appendAt(t, s, now, ScopeHost, "", MetricLoad1, 2)
	appendAt(t, s, now, ScopeService, "api", MetricMemoryBytes, 4096)

	series, err := s.Latest(Query{}, time.Hour)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, expected 2: %#v", len(series), series)
	}
	for _, got := range series {
		if len(got.Points) != 1 {
			t.Fatalf("%s carries %d points, expected only the newest", got.Metric, len(got.Points))
		}
		if got.Scope == ScopeHost && got.Points[0].Value != 2 {
			t.Fatalf("the host reads %v, expected the newest value", got.Points[0].Value)
		}
	}
}

// Nothing sampled recently is not the same answer as a number: a stale value
// presented as current is how somebody acts on a machine that went away.
func TestLatestIgnoresWhatIsTooOldToMeanAnything(t *testing.T) {
	s := openStore(t, Options{})
	appendAt(t, s, time.Now().Add(-2*time.Hour), ScopeHost, "", MetricLoad1, 1)

	series, err := s.Latest(Query{}, time.Minute)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(series) != 0 {
		t.Fatalf("a two-hour-old sample was presented as current: %#v", series)
	}
}

// The same data has to come back in the same order every time: a table a
// person reads and an answer a client diffs both depend on it.
func TestQueryOrdersSeriesTheSameWayEveryTime(t *testing.T) {
	s := openStore(t, Options{})
	now := time.Now()
	appendAt(t, s, now, ScopeService, "db", MetricCPUPercent, 1)
	appendAt(t, s, now.Add(time.Second), ScopeHost, "", MetricLoad1, 2)
	appendAt(t, s, now.Add(2*time.Second), ScopeService, "api", MetricCPUPercent, 3)

	series, err := s.Query(Query{StepMS: RawStep})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var order []string
	for _, got := range series {
		order = append(order, got.Scope+"/"+got.Name)
	}
	want := []string{"host/", "service/api", "service/db"}
	if !slices.Equal(order, want) {
		t.Fatalf("series came back as %v, expected %v", order, want)
	}
}

// Asking about one service has to answer about that service: the no-window
// answer narrows the same way a window does.
func TestLatestNarrowsLikeAWindowDoes(t *testing.T) {
	s := openStore(t, Options{})
	now := time.Now()
	appendAt(t, s, now, ScopeHost, "", MetricLoad1, 1)
	appendAt(t, s, now, ScopeService, "api", MetricCPUPercent, 2)
	appendAt(t, s, now, ScopeService, "db", MetricCPUPercent, 3)

	series, err := s.Latest(Query{Scope: ScopeService, Name: "api"}, time.Hour)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(series) != 1 || series[0].Name != "api" {
		t.Fatalf("the filter let something else through: %#v", series)
	}
}

// The defect an operator sees as "the chart vanished on 6h": the sweep folds
// full detail older than six hours into minutes and deletes it, so a window
// answered from ONE step has a hole — six hours of minutes that do not exist
// yet, or a day missing its newest six hours of detail. The answer stitches
// both steps.
func TestAWindowIsStitchedAcrossBothSteps(t *testing.T) {
	store := openStore(t, Options{})
	now := time.Now()

	// What the machine holds after a sweep: minutes for the old stretch, full
	// detail for the recent one.
	old := Sample{Time: now.Add(-20 * time.Hour), Scope: ScopeHost, Metric: MetricCPUPercent, Value: 10}
	recent := Sample{Time: now.Add(-10 * time.Minute), Scope: ScopeHost, Metric: MetricCPUPercent, Value: 20}
	err := store.Append([]Sample{recent})
	if err != nil {
		t.Fatalf("add recent: %v", err)
	}
	err = store.Append([]Sample{old})
	if err != nil {
		t.Fatalf("add old: %v", err)
	}
	err = store.Sweep(now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	for _, window := range []time.Duration{6 * time.Hour, 24 * time.Hour} {
		out, err := store.Query(Query{From: now.Add(-window), Metric: MetricCPUPercent})
		if err != nil {
			t.Fatalf("query %s: %v", window, err)
		}
		if len(out) != 1 {
			t.Fatalf("the %s window answered %d series", window, len(out))
		}
		found := false
		for _, point := range out[0].Points {
			if point.Value == 20 {
				found = true
			}
		}
		if !found {
			t.Fatalf("the %s window is missing its newest samples: %+v", window, out[0].Points)
		}
	}

	// The day window also reaches the folded past.
	out, err := store.Query(Query{From: now.Add(-24 * time.Hour), Metric: MetricCPUPercent})
	if err != nil {
		t.Fatalf("query day: %v", err)
	}
	sawOld := false
	for _, point := range out[0].Points {
		if point.Value == 10 {
			sawOld = true
		}
	}
	if !sawOld {
		t.Fatalf("the day window lost the folded past: %+v", out[0].Points)
	}
}
