// Package metrics samples what the host and its services are consuming and
// keeps the series long enough to answer for the hours nobody was watching.
//
// A counter the kernel exposes is cumulative; what an operator reads is a rate.
// The conversion happens here, once, so a stored series never depends on who
// is asking.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Scope and metric names are public contract: the dashboard and any agent
// match on them, so the text may not be rewritten between releases.
const (
	ScopeHost    = "host"
	ScopeService = "service"
)

const (
	MetricCPUPercent  = "cpu-percent"
	MetricMemoryBytes = "memory-bytes"
	MetricMemoryTotal = "memory-total-bytes"
	MetricDiskBytes   = "disk-bytes"
	MetricDiskTotal   = "disk-total-bytes"
	MetricNetRX       = "net-rx-bytes-per-second"
	MetricNetTX       = "net-tx-bytes-per-second"
	MetricLoad1       = "load-1"
)

// Short enough that a spike is visible, long enough that sampling costs
// nothing next to what it measures.
const SampleInterval = 10 * time.Second

// A platform hostd does not target reports this instead of inventing a number:
// a made-up series is worse than an absent one.
var ErrUnsupported = errors.New("metrics: the kernel counters are only read on linux")

type Sample struct {
	Time  time.Time
	Scope string
	// The service name; empty for the host itself.
	Name   string
	Metric string
	Value  float64
}

// What the supervisor knows and this package does not: sampling has no opinion
// about what a service is.
type Process struct {
	Name string
	PID  int
}

// Cumulative kernel counters, as read. Jiffies and bytes since boot mean
// nothing on their own; the sampler turns consecutive readings into rates.
type hostCounters struct {
	CPUTotal    float64
	CPUIdle     float64
	MemoryUsed  float64
	MemoryTotal float64
	DiskUsed    float64
	DiskTotal   float64
	NetRX       float64
	NetTX       float64
	Load1       float64
}

type procCounters struct {
	CPUTicks float64
	RSSBytes float64
}

// Implemented by the platform files, and by a fake in the tests, which is what
// lets the arithmetic be exercised where /proc does not exist.
type source interface {
	host() (hostCounters, error)
	process(pid int) (procCounters, error)
}

type Sampler struct {
	store  *Store
	src    source
	procs  func() []Process
	report func(error)

	lastAt   time.Time
	lastHost hostCounters
	haveHost bool
	lastProc map[string]procCounters

	lastProblem   string
	lastProblemAt time.Time
}

// A counter that cannot be read now fails again in ten seconds. Saying so at
// every tick would bury the timeline the message is written to; saying it once
// and again ten minutes later keeps it visible without drowning it.
const repeatProblem = 10 * time.Minute

func (s *Sampler) notify(err error, now time.Time) {
	text := err.Error()
	if text == s.lastProblem && now.Sub(s.lastProblemAt) < repeatProblem {
		return
	}
	s.lastProblem = text
	s.lastProblemAt = now
	s.report(err)
}

// procs is asked for the live processes at every tick, so a service that
// started a second ago is sampled without anyone re-registering it. report
// receives what could not be read: a sampler that fails in silence is a graph
// that lies by omission.
func NewSampler(store *Store, procs func() []Process, report func(error)) *Sampler {
	return &Sampler{
		store:    store,
		src:      systemSource{},
		procs:    procs,
		report:   report,
		lastProc: make(map[string]procCounters),
	}
}

// Run samples until the context ends. It returns rather than spinning when the
// platform has no counters to read: on a machine hostd does not target, the
// absence is reported once.
func (s *Sampler) Run(ctx context.Context) {
	ticker := time.NewTicker(SampleInterval)
	defer ticker.Stop()
	for {
		now := time.Now()
		err := s.Tick(now)
		if errors.Is(err, ErrUnsupported) {
			s.notify(err, now)
			return
		}
		if err != nil {
			s.notify(err, now)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick reads every counter once and stores what it could derive. A service
// that could not be read does not stop the host sample, and the reverse.
func (s *Sampler) Tick(now time.Time) error {
	samples, err := s.collect(now)
	if len(samples) == 0 {
		return err
	}
	return errors.Join(err, s.store.Append(samples))
}

func (s *Sampler) collect(now time.Time) ([]Sample, error) {
	elapsed := now.Sub(s.lastAt).Seconds()
	current, err := s.src.host()
	if errors.Is(err, ErrUnsupported) {
		return nil, err
	}

	var samples []Sample
	var problems error
	if err != nil {
		problems = errors.Join(problems, fmt.Errorf("read host counters: %w", err))
	}
	if err == nil {
		samples = append(samples,
			s.sample(now, ScopeHost, "", MetricMemoryBytes, current.MemoryUsed),
			s.sample(now, ScopeHost, "", MetricMemoryTotal, current.MemoryTotal),
			s.sample(now, ScopeHost, "", MetricDiskBytes, current.DiskUsed),
			s.sample(now, ScopeHost, "", MetricDiskTotal, current.DiskTotal),
			s.sample(now, ScopeHost, "", MetricLoad1, current.Load1),
		)
		// The first tick has nothing to subtract from: a rate needs two
		// readings, and inventing one would put a wrong number on the graph.
		if s.haveHost && elapsed > 0 {
			samples = append(samples, s.hostRates(now, current, elapsed)...)
		}
		s.lastHost = current
		s.haveHost = true
	}

	live := make(map[string]bool)
	for _, p := range s.procs() {
		live[p.Name] = true
		counters, procErr := s.src.process(p.PID)
		if procErr != nil {
			problems = errors.Join(problems, fmt.Errorf("read counters of %s (pid %d): %w", p.Name, p.PID, procErr))
			continue
		}
		samples = append(samples, s.sample(now, ScopeService, p.Name, MetricMemoryBytes, counters.RSSBytes))
		previous, seen := s.lastProc[p.Name]
		if seen && elapsed > 0 {
			samples = append(samples, s.sample(now, ScopeService, p.Name, MetricCPUPercent,
				cpuPercent(counters.CPUTicks-previous.CPUTicks, elapsed)))
		}
		s.lastProc[p.Name] = counters
	}
	// A service that stopped must not have its old counters subtracted from a
	// new process's, which would show a spike that never happened.
	for name := range s.lastProc {
		if !live[name] {
			delete(s.lastProc, name)
		}
	}

	s.lastAt = now
	return samples, problems
}

func (s *Sampler) hostRates(now time.Time, current hostCounters, elapsed float64) []Sample {
	out := make([]Sample, 0, 3)
	total := current.CPUTotal - s.lastHost.CPUTotal
	idle := current.CPUIdle - s.lastHost.CPUIdle
	if total > 0 {
		out = append(out, s.sample(now, ScopeHost, "", MetricCPUPercent, (1-idle/total)*100))
	}
	// A counter that went backwards means the interface was reset or the
	// machine rebooted; reporting the reset as traffic would invent a spike.
	rx := current.NetRX - s.lastHost.NetRX
	tx := current.NetTX - s.lastHost.NetTX
	if rx >= 0 && tx >= 0 {
		out = append(out,
			s.sample(now, ScopeHost, "", MetricNetRX, rx/elapsed),
			s.sample(now, ScopeHost, "", MetricNetTX, tx/elapsed),
		)
	}
	return out
}

func (s *Sampler) sample(now time.Time, scope, name, metric string, value float64) Sample {
	return Sample{Time: now, Scope: scope, Name: name, Metric: metric, Value: value}
}

// Percent of one core, the way top reports it: a service using two cores fully
// reads as 200, which is the number an operator can act on.
func cpuPercent(ticks, elapsed float64) float64 {
	if ticks < 0 || elapsed <= 0 {
		return 0
	}
	return ticks / clockTicks / elapsed * 100
}
