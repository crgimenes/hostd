package metrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A fake that answers like the kernel does, including failing on demand: a
// source that always succeeds makes the reported-failure path untestable.
type fakeSource struct {
	counters hostCounters
	procs    map[int]procCounters
	hostErr  error
	procErr  error
}

func (f *fakeSource) host() (hostCounters, error) {
	return f.counters, f.hostErr
}

func (f *fakeSource) process(pid int) (procCounters, error) {
	if f.procErr != nil {
		return procCounters{}, f.procErr
	}
	counters, ok := f.procs[pid]
	if !ok {
		return procCounters{}, errors.New("no such process")
	}
	return counters, nil
}

func newSampler(t *testing.T, src *fakeSource, procs []Process) (*Sampler, *Store) {
	t.Helper()
	store := openStore(t, Options{})
	s := NewSampler(store, func() []Process { return procs }, func(error) {})
	s.src = src
	return s, store
}

func valueOf(t *testing.T, samples []Sample, scope, name, metric string) float64 {
	t.Helper()
	for _, s := range samples {
		if s.Scope == scope && s.Name == name && s.Metric == metric {
			return s.Value
		}
	}
	t.Fatalf("no sample for %s/%s/%s in %d samples", scope, name, metric, len(samples))
	return 0
}

func has(samples []Sample, scope, name, metric string) bool {
	for _, s := range samples {
		if s.Scope == scope && s.Name == name && s.Metric == metric {
			return true
		}
	}
	return false
}

// A rate needs two readings. Inventing one from the first tick would draw a
// number that was never measured.
func TestFirstTickHasNoRates(t *testing.T) {
	src := &fakeSource{counters: hostCounters{CPUTotal: 1000, CPUIdle: 900, MemoryUsed: 4, MemoryTotal: 8}}
	s, _ := newSampler(t, src, nil)

	samples, err := s.collect(time.Unix(100, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if has(samples, ScopeHost, "", MetricCPUPercent) {
		t.Fatal("the first tick reported a cpu rate it could not have measured")
	}
	if valueOf(t, samples, ScopeHost, "", MetricMemoryBytes) != 4 {
		t.Fatal("a gauge was not reported on the first tick")
	}
}

func TestHostRatesComeFromTheDeltas(t *testing.T) {
	src := &fakeSource{counters: hostCounters{CPUTotal: 1000, CPUIdle: 900, NetRX: 1000, NetTX: 500}}
	s, _ := newSampler(t, src, nil)

	_, err := s.collect(time.Unix(100, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// A quarter of the added time was idle, so three quarters were work.
	src.counters = hostCounters{CPUTotal: 1400, CPUIdle: 1000, NetRX: 11000, NetTX: 5500}
	samples, err := s.collect(time.Unix(110, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := valueOf(t, samples, ScopeHost, "", MetricCPUPercent)
	if got != 75 {
		t.Fatalf("cpu is %v percent, expected 75", got)
	}
	got = valueOf(t, samples, ScopeHost, "", MetricNetRX)
	if got != 1000 {
		t.Fatalf("rx is %v bytes per second, expected 1000", got)
	}
	got = valueOf(t, samples, ScopeHost, "", MetricNetTX)
	if got != 500 {
		t.Fatalf("tx is %v bytes per second, expected 500", got)
	}
}

// An interface reset or a reboot makes the counter fall. Subtracting anyway
// would draw a spike that never happened.
func TestCountersThatWentBackwardsAreNotTraffic(t *testing.T) {
	src := &fakeSource{counters: hostCounters{CPUTotal: 1000, CPUIdle: 900, NetRX: 5_000_000, NetTX: 5_000_000}}
	s, _ := newSampler(t, src, nil)
	_, err := s.collect(time.Unix(100, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	src.counters = hostCounters{CPUTotal: 1400, CPUIdle: 1300, NetRX: 1000, NetTX: 1000}
	samples, err := s.collect(time.Unix(110, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if has(samples, ScopeHost, "", MetricNetRX) {
		t.Fatal("a counter reset was reported as traffic")
	}
}

func TestServiceCPUIsPercentOfOneCore(t *testing.T) {
	src := &fakeSource{
		counters: hostCounters{CPUTotal: 1000, CPUIdle: 900},
		procs:    map[int]procCounters{42: {CPUTicks: 100, RSSBytes: 2048}},
	}
	s, _ := newSampler(t, src, []Process{{Name: "api", PID: 42}})

	_, err := s.collect(time.Unix(100, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// A full second of cpu over ten seconds of wall clock is ten percent.
	src.procs[42] = procCounters{CPUTicks: 200, RSSBytes: 4096}
	samples, err := s.collect(time.Unix(110, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := valueOf(t, samples, ScopeService, "api", MetricCPUPercent)
	if got != 10 {
		t.Fatalf("the service reads %v percent, expected 10", got)
	}
	got = valueOf(t, samples, ScopeService, "api", MetricMemoryBytes)
	if got != 4096 {
		t.Fatalf("the service reads %v bytes resident, expected 4096", got)
	}
}

// A service that stopped and came back is a new process. Subtracting the old
// counters from the new ones would report a spike nobody caused.
func TestCountersOfAStoppedServiceAreForgotten(t *testing.T) {
	src := &fakeSource{
		counters: hostCounters{CPUTotal: 1000, CPUIdle: 900},
		procs:    map[int]procCounters{42: {CPUTicks: 5000, RSSBytes: 2048}},
	}
	procs := []Process{{Name: "api", PID: 42}}
	store := openStore(t, Options{})
	s := NewSampler(store, func() []Process { return procs }, func(error) {})
	s.src = src

	_, err := s.collect(time.Unix(100, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	procs = nil
	_, err = s.collect(time.Unix(110, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	procs = []Process{{Name: "api", PID: 43}}
	src.procs[43] = procCounters{CPUTicks: 10, RSSBytes: 2048}
	samples, err := s.collect(time.Unix(120, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if has(samples, ScopeService, "api", MetricCPUPercent) {
		t.Fatal("the restarted service inherited the counters of the old process")
	}
}

// One unreadable process must not cost the host sample, and the error must
// still reach whoever asked for it.
func TestAnUnreadableServiceDoesNotStopTheHostSample(t *testing.T) {
	src := &fakeSource{
		counters: hostCounters{CPUTotal: 1000, CPUIdle: 900, MemoryUsed: 7},
		procErr:  errors.New("permission denied"),
	}
	s, _ := newSampler(t, src, []Process{{Name: "api", PID: 42}})

	samples, err := s.collect(time.Unix(100, 0))
	if err == nil {
		t.Fatal("a service that could not be read was not reported")
	}
	if valueOf(t, samples, ScopeHost, "", MetricMemoryBytes) != 7 {
		t.Fatal("the host sample was lost with the service that failed")
	}
}

// On a platform without the counters, the sampler says so once and leaves,
// rather than writing an empty series forever.
func TestRunStopsWhereThereAreNoCounters(t *testing.T) {
	store := openStore(t, Options{})
	s := NewSampler(store, func() []Process { return nil }, func(error) {})
	s.src = &fakeSource{hostErr: ErrUnsupported}

	var reported error
	s.report = func(err error) { reported = err }
	done := make(chan struct{})
	go func() {
		s.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler kept running with nothing to read")
	}
	if !errors.Is(reported, ErrUnsupported) {
		t.Fatalf("the absence was not reported: %v", reported)
	}
}

func TestTickStoresWhatItCollected(t *testing.T) {
	src := &fakeSource{counters: hostCounters{CPUTotal: 1000, CPUIdle: 900, MemoryUsed: 3, MemoryTotal: 8}}
	store := openStore(t, Options{})
	s := NewSampler(store, func() []Process { return nil }, func(error) {})
	s.src = src

	err := s.Tick(time.Now())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	series, err := store.Latest(Query{}, time.Minute)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(series) == 0 {
		t.Fatal("a tick stored nothing")
	}
}

func openStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "metrics.db"), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// The real counters, where they exist. The parsers are exercised from
// fixtures everywhere; this is the only test that needs a kernel that has
// /proc, so it skips rather than fails where hostd is developed.
func TestSystemSourceReadsTheRealCounters(t *testing.T) {
	counters, err := systemSource{}.host()
	if errors.Is(err, ErrUnsupported) {
		t.Skip("this platform exposes no /proc counters")
	}
	if err != nil {
		t.Fatalf("host counters: %v", err)
	}
	if counters.CPUTotal <= 0 || counters.MemoryTotal <= 0 || counters.DiskTotal <= 0 {
		t.Fatalf("a counter came back empty: %#v", counters)
	}
	if counters.MemoryUsed > counters.MemoryTotal || counters.DiskUsed > counters.DiskTotal {
		t.Fatalf("more is used than exists: %#v", counters)
	}

	own, err := systemSource{}.process(os.Getpid())
	if err != nil {
		t.Fatalf("own counters: %v", err)
	}
	if own.RSSBytes <= 0 {
		t.Fatalf("this test process reports %v bytes resident", own.RSSBytes)
	}
}

// A counter that cannot be read fails again in ten seconds, and the timeline
// it is reported into is the same one an operator reads.
func TestARepeatedProblemIsNotRepeatedEveryTick(t *testing.T) {
	store := openStore(t, Options{})
	var reports []string
	s := NewSampler(store, func() []Process { return nil }, func(err error) {
		reports = append(reports, err.Error())
	})

	at := time.Unix(1000, 0)
	s.notify(errors.New("permission denied"), at)
	s.notify(errors.New("permission denied"), at.Add(time.Minute))
	if len(reports) != 1 {
		t.Fatalf("the same problem was reported %d times in a minute", len(reports))
	}

	// Still true ten minutes later, and still worth saying.
	s.notify(errors.New("permission denied"), at.Add(repeatProblem+time.Second))
	if len(reports) != 2 {
		t.Fatal("a problem that outlived the quiet window went unreported")
	}
	// A different problem is not the same problem.
	s.notify(errors.New("no such file"), at.Add(repeatProblem+2*time.Second))
	if len(reports) != 3 {
		t.Fatal("a new problem was swallowed by the previous one")
	}
}
