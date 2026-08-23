// Package api carries the operations hostd exposes and the wire format they
// travel in.
//
// There is one implementation of every capability, on the daemon side. hostctl,
// the graphical mode and any agent are presentations of the same operations:
// no client reimplements logic, and no capability exists in only one of them.
package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crgimenes/hostd/internal/filoconf"
)

// Operations. Names are part of the public contract: a client of a different
// release must be able to tell what a daemon understands.
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
)

// Error codes are the part of a failure that programs read. Messages are for
// people and may be rewritten; these may not.
const (
	CodeOK          = "ok"
	CodeInvalid     = "invalid-request"
	CodeUnknownOp   = "unknown-operation"
	CodeNotFound    = "not-found"
	CodeFailed      = "operation-failed"
	CodeUnavailable = "unavailable"
	// CodeConflict means the host moved to another generation since the
	// caller last looked. It is a refusal, not a failure: nothing was
	// changed, and reading the current state and retrying is the answer.
	CodeConflict = "generation-conflict"
	// CodeDestructive means the operation would take a running service away
	// and was not authorised to. Destructive work is never inferred.
	CodeDestructive = "destructive-refused"
)

// maxRequestBytes bounds one request. A daemon that reads until the client
// stops talking is a daemon a single client can exhaust.
const maxRequestBytes = 1 << 20

// Request is one operation asked of hostd.
type Request struct {
	Op string `filo:"op"`
	// Name selects a service where the operation needs one.
	Name string `filo:"name"`
	// Query carries the search terms of a log operation.
	Service string `filo:"service"`
	Stream  string `filo:"stream"`
	Match   string `filo:"match"`
	Kind    string `filo:"kind"`
	Limit   int    `filo:"limit"`
	Since   uint64 `filo:"since"`
	// ExpectGeneration is the generation the caller believed it was acting
	// on. Zero means it makes no claim; any other value must match, or the
	// operation is refused with the current one rather than overwriting work
	// somebody else did in the meantime.
	ExpectGeneration uint64 `filo:"expect-generation"`
	// AllowDestructive authorises changes that take a running service away.
	// Destructive work is never inferred from an ordinary apply.
	AllowDestructive bool `filo:"allow-destructive"`
	// OnBehalfOf names the identity a caller is acting for, so an agent's
	// work is auditable as delegated rather than as its own.
	OnBehalfOf string `filo:"on-behalf-of"`
	// Body carries Filo source for operations that take a document.
	Body string `filo:"body"`
}

// Response is the answer. Code is always set, including on success, so a
// client never has to infer the outcome from the shape of the payload.
type Response struct {
	Code string `filo:"code"`
	// Message explains a failure to a person and, where possible, says how to
	// fix it.
	Message string `filo:"message"`
	// Generation is the host's generation after the operation. Every answer
	// carries it, so a caller always knows what to claim next.
	Generation uint64 `filo:"generation"`
	// Body is the requested result, already rendered as Filo.
	Body string `filo:"body"`
}

// Failed reports whether the response carries an error.
func (r Response) Failed() bool { return r.Code != CodeOK }

// Error is a failure with a stable code. The message may be rewritten between
// releases; the code is what a program is allowed to branch on.
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

// Err turns a failed response into an error carrying its code.
func (r Response) Err() error {
	if !r.Failed() {
		return nil
	}
	return Error{Code: r.Code, Message: r.Message}
}

// The wire format is one Filo document per line. Filo is the native format of
// the system, so the control channel speaks it too rather than inventing a
// second encoding. Marshalling emits a single line and escapes newlines inside
// strings, which is what makes a line a message; WriteMessage checks that
// invariant instead of trusting it.
//
// WriteMessage encodes v and writes it as one message.
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

// ReadMessage reads one message and decodes it into v.
func ReadMessage(ctx context.Context, r *bufio.Reader, v any) error {
	line, err := readLimitedLine(r)
	if err != nil {
		return err
	}
	return filoconf.Decode(ctx, "message", line, v)
}

// readLimitedLine reads one line, refusing to grow past the ceiling. A client
// that never sends a newline must cost a bounded amount of memory.
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
