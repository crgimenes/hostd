package api

import (
	"time"

	"github.com/crgimenes/hostd/internal/filoconf"
	"github.com/crgimenes/hostd/internal/logs"
)

// LogLine is a captured line as it travels on the wire.
//
// The time is a number rather than formatted text: a timestamp a program has
// to parse back out of a string is a timestamp that will be parsed wrong
// somewhere. It is milliseconds since the epoch because every Filo number is a
// float64, which holds integers exactly only up to 2^53 — nanoseconds since
// the epoch passed that in 1970 and would arrive rounded. Milliseconds stay
// exact past the year 2255, and Seq is what orders two lines from the same
// millisecond.
type LogLine struct {
	Seq     uint64  `filo:"seq"`
	Time    float64 `filo:"time-ms"`
	Service string  `filo:"service"`
	Stream  string  `filo:"stream"`
	Text    string  `filo:"text"`
}

// At returns the wall time of the line.
func (l LogLine) At() time.Time {
	return time.UnixMilli(int64(l.Time))
}

func toLine(r logs.Record) LogLine {
	return LogLine{
		Seq:     r.Seq,
		Time:    float64(r.Time.UnixMilli()),
		Service: r.Service,
		Stream:  r.Stream,
		Text:    r.Text,
	}
}

func toLines(records []logs.Record) []LogLine {
	out := make([]LogLine, 0, len(records))
	for _, r := range records {
		out = append(out, toLine(r))
	}
	return out
}

// body renders a successful result.
func body(v any) Response {
	out, err := filoconf.Marshal(v)
	if err != nil {
		// Structured output is a contract. A result hostd cannot render is a
		// failure of the daemon, not an empty success.
		return Response{Code: CodeFailed, Message: "could not render the result: " + err.Error()}
	}
	return Response{Code: CodeOK, Body: out}
}

// mustMarshal renders a value that is known to be renderable, for use inside a
// response that is already reporting a failure.
func mustMarshal(v any) string {
	out, err := filoconf.Marshal(v)
	if err != nil {
		return ""
	}
	return out
}
