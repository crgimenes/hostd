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
	"strconv"
	"strings"

	"github.com/crgimenes/hostd/filoconf"
)

// Operation names are public contract: a client of another release matches on
// them to tell what a daemon understands.
const (
	OpDescribe        = "describe"
	OpStatus          = "status"
	OpServiceList     = "service.list"
	OpServiceStart    = "service.start"
	OpServiceStop     = "service.stop"
	OpServiceRestrt   = "service.restart"
	OpServiceRedeploy = "service.redeploy"
	OpServiceRemove   = "service.remove"
	OpApply           = "apply"
	OpPlan            = "plan"
	OpAudit           = "audit"
	OpLogSearch       = "log.search"
	OpLogFollow       = "log.follow"
	OpLogAppend       = "log.append"
	OpMetrics         = "metrics"
	OpImagePush       = "image.push"
	OpImagePull       = "image.pull"
	OpImageList       = "image.list"
	OpImagePrune      = "image.prune"
	OpServiceVersions = "service.versions"
	OpServicePut      = "service.put"
	OpJobRun          = "job.run"
	OpServiceBackup   = "service.backup"
	OpFileList        = "file.list"
	OpFileGet         = "file.get"
	OpFilePut         = "file.put"
	OpFileDelete      = "file.delete"
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
	// The machine is doing what its declaration says and that is why the ask
	// was turned down — a job already at its ceiling, say. Asking again
	// unchanged gets the same answer.
	CodeRefused = "refused"
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
	Run     string `filo:"run"`
	Limit   int    `filo:"limit"`
	Since   uint64 `filo:"since"`
	// How many versions of one image a prune leaves behind. Not Limit: that
	// bounds how much of an answer comes back, and this decides what stops
	// existing.
	Keep int `filo:"keep"`
	// A metric query: the window in milliseconds, and what to answer it with.
	// Zero From asks for the newest value of every series instead.
	// What an image was built to run on, so a machine that cannot run it says
	// so before the bytes cross the wire.
	Arch   string  `filo:"arch"`
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

// An image does not fit the line protocol, so a request that carries one is
// followed by its bytes in chunks: a decimal length on its own line, that many
// bytes, and a zero length to end. Nothing is buffered whole at either end —
// what arrives is written straight to the runtime — and the size does not have
// to be known before the first byte, which is what lets the image be streamed
// out of the runtime that has it.
const (
	chunkSize = 1 << 20
	// A ceiling on what one upload can cost the machine. Real images are far
	// below this; something above it is a mistake or an attack, and either way
	// the answer is to stop reading.
	maxUploadBytes = 8 << 30
)

// WriteChunks copies everything from r as framed chunks. The deadline is not
// set here: a long upload that keeps moving must not die of its own length,
// and one that stops moving is the caller's to bound.
func WriteChunks(w io.Writer, r io.Reader) error {
	buffer := make([]byte, chunkSize)
	for {
		n, err := r.Read(buffer)
		if n > 0 {
			_, writeErr := fmt.Fprintf(w, "%d\n", n)
			if writeErr != nil {
				return writeErr
			}
			_, writeErr = w.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			_, err = io.WriteString(w, "0\n")
			return err
		}
		if err != nil {
			return err
		}
	}
}

// ReadChunks copies the framed chunks into w until the end marker. A frame
// that claims more than the ceiling ends the read rather than the machine.
func ReadChunks(r *bufio.Reader, w io.Writer, reset func()) (int64, error) {
	var total int64
	for {
		if reset != nil {
			// Each frame that arrives buys time for the next one: a stalled
			// upload dies, a slow one does not.
			reset()
		}
		line, err := readLimitedLine(r)
		if err != nil {
			return total, err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
		if err != nil {
			return total, fmt.Errorf("expected the length of a chunk, got %q", line)
		}
		if size == 0 {
			return total, nil
		}
		if size < 0 || size > chunkSize {
			return total, fmt.Errorf("a chunk of %d bytes is not one this protocol sends", size)
		}
		total += size
		if total > maxUploadBytes {
			return total, fmt.Errorf("the upload passed %d bytes, which is more than this machine accepts", int64(maxUploadBytes))
		}
		_, err = io.CopyN(w, r, size)
		if err != nil {
			return total, err
		}
	}
}
