// Package logs captures what a service writes and makes it readable without
// entering the machine.
//
// A service writes to a spool file, never to a pipe held by hostd: a pipe whose
// reader died kills the service with EPIPE on its next write. That is what
// makes restarting the supervisor safe.
package logs

import (
	"strings"
	"time"
)

const (
	StreamOut = "stdout"
	StreamErr = "stderr"
	// hostd's own facts, in the same timeline as the output, so a death and
	// the last lines before it read together.
	StreamEvent = "event"
)

// Event kinds are codes, not prose: the text may be rewritten between
// releases, the code may not.
const (
	EventStarted     = "service.started"
	EventExited      = "service.exited"
	EventStopped     = "service.stopped"
	EventKilled      = "service.killed"
	EventStartFailed = "service.start-failed"
	EventAdopted     = "service.adopted"
	EventOrphan      = "service.orphan"
	EventGone        = "service.gone"
	EventMissed      = "service.missed"
	EventApplied     = "config.applied"
	EventSpoolLost   = "spool.overflowed"
	EventLogDropped  = "logs.dropped"
	EventJobStarted  = "job.started"
	EventJobFinished = "job.finished"
	EventJobSkipped  = "job.skipped"
	EventJobOverran  = "job.overran"
	EventImage       = "image.received"
	EventBackup      = "backup"
	EventDaemon      = "hostd.started"
	EventProblem     = "hostd.problem"
)

type Record struct {
	// Per-host monotonic, so two lines from the same millisecond still have
	// an order.
	Seq     uint64    `filo:"seq"`
	Time    time.Time `filo:"time"`
	Service string    `filo:"service"`
	Stream  string    `filo:"stream"`
	// Empty for captured output.
	Kind string `filo:"kind"`
	// Which run of a job wrote this. Several runs of one job write at the same
	// time on purpose, and a timeline that could not tell them apart would be
	// a timeline nobody can follow.
	Run  string `filo:"run"`
	Text string `filo:"text"`
}

type Query struct {
	Service string
	Stream  string
	Kind    string
	Run     string
	Match   string
	// Most recent kept, oldest first in the result.
	Limit int
	// Only records above this sequence: how a follower resumes without
	// repeating what it printed.
	Since uint64
}

// A follower receives everything appended, so it filters with the same rule a
// search uses.
func (q Query) Matches(r Record) bool {
	if q.Service != "" && r.Service != q.Service {
		return false
	}
	if q.Stream != "" && r.Stream != q.Stream {
		return false
	}
	if q.Kind != "" && r.Kind != q.Kind {
		return false
	}
	if q.Run != "" && r.Run != q.Run {
		return false
	}
	if q.Since > 0 && r.Seq <= q.Since {
		return false
	}
	if q.Match != "" && !strings.Contains(strings.ToLower(r.Text), strings.ToLower(q.Match)) {
		return false
	}
	return true
}
