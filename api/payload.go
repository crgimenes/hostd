package api

import (
	"time"

	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/logs"
)

// Time is milliseconds since the epoch: every Filo number is a float64, which
// holds integers exactly only to 2^53, so nanoseconds would arrive rounded.
// Milliseconds stay exact past 2255, and Seq orders lines within one.
type LogLine struct {
	Seq     uint64  `filo:"seq"`
	Time    float64 `filo:"time-ms"`
	Service string  `filo:"service"`
	Stream  string  `filo:"stream"`
	Kind    string  `filo:"kind"`
	Text    string  `filo:"text"`
}

func (l LogLine) At() time.Time {
	return time.UnixMilli(int64(l.Time))
}

func toLine(r logs.Record) LogLine {
	return LogLine{
		Seq:     r.Seq,
		Time:    float64(r.Time.UnixMilli()),
		Service: r.Service,
		Stream:  r.Stream,
		Kind:    r.Kind,
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

func body(v any) Response {
	out, err := filoconf.Marshal(v)
	if err != nil {
		// A result hostd cannot render is a failure, never an empty success.
		return Response{Code: CodeFailed, Message: "could not render the result: " + err.Error()}
	}
	return Response{Code: CodeOK, Body: out}
}

// For a value already known to render, inside a response reporting a failure.
func mustMarshal(v any) string {
	out, err := filoconf.Marshal(v)
	if err != nil {
		return ""
	}
	return out
}
