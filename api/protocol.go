// Package api carries the operations hostd exposes and the wire format they
// travel in.
//
// Every capability is implemented once, on the daemon side; hostctl, the
// graphical mode and any agent are presentations of the same operations.
package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crgimenes/hostd/filoconf"
)

// Operation names are public contract: a client of another release matches on
// them to tell what a daemon understands.
const (
	OpDescribe      = "describe"
	OpStatus        = "status"
	OpServiceList   = "service.list"
	OpServiceStart  = "service.start"
	OpServiceStop   = "service.stop"
	OpServiceRestrt = "service.restart"
	OpApply         = "apply"
	OpPlan          = "plan"
	OpAudit         = "audit"
	OpLogSearch     = "log.search"
	OpLogFollow     = "log.follow"
	OpMetrics       = "metrics"
)

// Codes are what programs read; messages are for people and may be rewritten.
const (
	CodeOK          = "ok"
	CodeInvalid     = "invalid-request"
	CodeUnknownOp   = "unknown-operation"
	CodeNotFound    = "not-found"
	CodeFailed      = "operation-failed"
	CodeUnavailable = "unavailable"
	// Both of these mean nothing was changed and the caller must look before
	// retrying, which is a different outcome from a failure.
	CodeConflict    = "generation-conflict"
	CodeDestructive = "destructive-refused"
)

// A daemon that reads until the client stops talking is one a single client
// can exhaust.
const maxRequestBytes = 1 << 20

type Request struct {
	Op      string `filo:"op"`
	Name    string `filo:"name"`
	Service string `filo:"service"`
	Stream  string `filo:"stream"`
	Match   string `filo:"match"`
	Kind    string `filo:"kind"`
	Limit   int    `filo:"limit"`
	Since   uint64 `filo:"since"`
	// A metric query: the window in milliseconds, and what to answer it with.
	// Zero From asks for the newest value of every series instead.
	Scope  string  `filo:"scope"`
	Metric string  `filo:"metric"`
	FromMS float64 `filo:"from-ms"`
	ToMS   float64 `filo:"to-ms"`
	StepMS float64 `filo:"step-ms"`
	// Zero makes no claim; any other value must match the host's generation
	// or the operation is refused rather than overwriting somebody's work.
	ExpectGeneration uint64 `filo:"expect-generation"`
	AllowDestructive bool   `filo:"allow-destructive"`
	// The identity a caller is acting for: an agent's work is auditable as
	// delegated rather than as its own.
	OnBehalfOf string `filo:"on-behalf-of"`
	Body       string `filo:"body"`
}

// Code is set on success too, so a client never infers the outcome from the
// shape of the payload.
type Response struct {
	Code    string `filo:"code"`
	Message string `filo:"message"`
	// Carried by every answer, so a caller always knows what to claim next.
	Generation uint64 `filo:"generation"`
	Body       string `filo:"body"`
}

func (r Response) Failed() bool { return r.Code != CodeOK }

// The code is what a program branches on; the message may be rewritten.
type Error struct {
	Code    string
	Message string
}

func (e Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func (r Response) Err() error {
	if !r.Failed() {
		return nil
	}
	return Error{Code: r.Code, Message: r.Message}
}

// One Filo document per line: marshalling emits a single line and escapes
// newlines inside strings. WriteMessage checks that invariant rather than
// trusting it.
func WriteMessage(w io.Writer, v any) error {
	body, err := filoconf.Marshal(v)
	if err != nil {
		return err
	}
	body = strings.TrimRight(body, "\n")
	if strings.ContainsAny(body, "\n\r") {
		return errors.New("api: encoded message contains a line break, which would split it in two")
	}
	_, err = io.WriteString(w, body+"\n")
	return err
}

func ReadMessage(ctx context.Context, r *bufio.Reader, v any) error {
	line, err := readLimitedLine(r)
	if err != nil {
		return err
	}
	return filoconf.Decode(ctx, "message", line, v)
}

// A client that never sends a newline must cost a bounded amount of memory.
func readLimitedLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		if b.Len()+len(chunk) > maxRequestBytes {
			return "", fmt.Errorf("message exceeds %d bytes", maxRequestBytes)
		}
		b.Write(chunk)
		if !isPrefix {
			return b.String(), nil
		}
	}
}
