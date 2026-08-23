package metrics

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// USER_HZ, which sysconf(_SC_CLK_TCK) would answer. Reading it needs cgo, and
// the kernel has exported these fields in hundredths of a second on every
// architecture Linux supports since the counters existed.
const clockTicks = 100

// Parsing is separate from reading on purpose: these functions take the text
// the kernel produced, so they are exercised on a developer's machine that has
// no /proc at all.

// The first line of /proc/stat totals every core:
// cpu user nice system idle iowait irq softirq steal guest guest_nice
func parseCPU(stat string) (total float64, idle float64, err error) {
	line, _, _ := strings.Cut(stat, "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("/proc/stat does not open with a cpu line: %.40q", stat)
	}
	for i, f := range fields[1:] {
		value, convErr := strconv.ParseFloat(f, 64)
		if convErr != nil {
			return 0, 0, fmt.Errorf("cpu field %d: %w", i+1, convErr)
		}
		total += value
		// idle and iowait: time the machine had nothing to do with.
		if i == 3 || i == 4 {
			idle += value
		}
	}
	return total, idle, nil
}

// MemAvailable is the kernel's own estimate of what a new process could get,
// which is the number an operator cares about; total minus free would count
// reclaimable cache as used and cry wolf on every healthy machine.
func parseMeminfo(meminfo string) (used float64, total float64, err error) {
	var available float64
	var haveTotal, haveAvailable bool
	for line := range strings.SplitSeq(meminfo, "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		if key != "MemTotal" && key != "MemAvailable" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, convErr := strconv.ParseFloat(fields[0], 64)
		if convErr != nil {
			return 0, 0, fmt.Errorf("%s: %w", key, convErr)
		}
		// The kernel reports kB there, and only there.
		value *= 1024
		if key == "MemTotal" {
			total, haveTotal = value, true
			continue
		}
		available, haveAvailable = value, true
	}
	if !haveTotal || !haveAvailable {
		return 0, 0, fmt.Errorf("/proc/meminfo carries no MemTotal or MemAvailable")
	}
	return total - available, total, nil
}

// /proc/net/dev, two header lines then one per interface:
// iface: rx-bytes rx-packets ... tx-bytes tx-packets ...
func parseNetDev(netdev string) (rx float64, tx float64, err error) {
	found := false
	for line := range strings.SplitSeq(netdev, "\n") {
		name, rest, hasName := strings.Cut(line, ":")
		if !hasName {
			continue
		}
		name = strings.TrimSpace(name)
		// Loopback is a machine talking to itself, and counting it would make
		// a busy local socket look like traffic on the wire.
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		const txBytesIndex = 8
		if len(fields) <= txBytesIndex {
			continue
		}
		received, rxErr := strconv.ParseFloat(fields[0], 64)
		transmitted, txErr := strconv.ParseFloat(fields[txBytesIndex], 64)
		if rxErr != nil || txErr != nil {
			return 0, 0, fmt.Errorf("interface %s: unreadable byte counters", name)
		}
		rx += received
		tx += transmitted
		found = true
	}
	if !found {
		return 0, 0, fmt.Errorf("/proc/net/dev lists no interface other than lo")
	}
	return rx, tx, nil
}

func parseLoad(loadavg string) (float64, error) {
	fields := strings.Fields(loadavg)
	if len(fields) == 0 {
		return 0, fmt.Errorf("/proc/loadavg is empty")
	}
	return strconv.ParseFloat(fields[0], 64)
}

// Field 2 of /proc/<pid>/stat is the executable name and may contain spaces
// and parentheses, so positions are only reliable after the last closing one.
// Fields 14 and 15 are the ticks spent in user and system code, field 24 the
// resident set in pages.
//
// yagni: the process itself, not the tree it spawned; a service that forks
// workers under-reports. Upgrade when one does, by walking the session.
func parseProcStat(stat string) (procCounters, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return procCounters{}, fmt.Errorf("no comm field in %.40q", stat)
	}
	fields := strings.Fields(stat[end+1:])
	// fields[0] is field 3, so field N sits at index N-3.
	const (
		utimeIndex = 11
		stimeIndex = 12
		rssIndex   = 21
	)
	if len(fields) <= rssIndex {
		return procCounters{}, fmt.Errorf("only %d fields after the comm field", len(fields))
	}
	utime, utimeErr := strconv.ParseFloat(fields[utimeIndex], 64)
	stime, stimeErr := strconv.ParseFloat(fields[stimeIndex], 64)
	rss, rssErr := strconv.ParseFloat(fields[rssIndex], 64)
	if utimeErr != nil || stimeErr != nil || rssErr != nil {
		return procCounters{}, fmt.Errorf("unreadable cpu or memory fields")
	}
	return procCounters{
		CPUTicks: utime + stime,
		RSSBytes: rss * float64(os.Getpagesize()),
	}, nil
}
