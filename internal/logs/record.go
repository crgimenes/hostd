// Package logs captures what a service writes and makes it readable without
// entering the machine.
//
// A service writes to a spool file, not to a pipe held by hostd. A pipe would
// tie the service's life to the supervisor's: when hostd died, the read end
// would close and the next write would kill the service with EPIPE. Writing to
// a file keeps the service running and its output intact while hostd is away,
// which is what makes restarting the supervisor a safe operation.
package logs

import "time"

// Stream names, as they appear to anyone reading the log.
const (
	StreamOut = "stdout"
	StreamErr = "stderr"
	// StreamEvent carries hostd's own facts about the service, so that a
	// death and the last lines before it read in one timeline.
	StreamEvent = "event"
)

// Record is one captured line.
type Record struct {
	// Seq is a per-host monotonic sequence, so two lines written in the same
	// millisecond still have an order.
	Seq     uint64    `filo:"seq"`
	Time    time.Time `filo:"time"`
	Service string    `filo:"service"`
	Stream  string    `filo:"stream"`
	Text    string    `filo:"text"`
}
