// Package filoconf evaluates hostd's Filo configuration under fixed limits and
// decodes the result into Go values.
//
// Evaluating a configuration never changes the machine. The engine here has no
// builtin that touches files, network or processes: a configuration file is a
// calculation that produces a declaration, and everything that acts on the
// declaration lives outside this package.
package filoconf

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crgimenes/filo"
	"github.com/crgimenes/filo/filomath"
	"github.com/crgimenes/filo/filostrings"
)

// Limits bound a single evaluation. A configuration file is written by an
// operator and read by a daemon that must keep running, so an accidental
// infinite loop has to end as an error instead of a hung host.
const (
	stepLimit      = 200_000
	recursionLimit = 64
	timeout        = 2 * time.Second
)

// maxSourceSize caps the file before it is even parsed. Without a ceiling, a
// stray multi-gigabyte file in the services directory would be read into
// memory before any limit applied.
const maxSourceSize = 1 << 20

// ErrTooLarge reports a source larger than maxSourceSize.
var ErrTooLarge = errors.New("filoconf: source too large")

// constructors are the record-shaped builtins a configuration may use. Each
// one returns its arguments as a list, so `(service (tuple "name" "api"))`
// evaluates to the key/value list that Decode expects. They exist to make the
// files read like declarations instead of nested list literals.
var constructors = []string{"service", "host", "inventory"}

func newEngine() *filo.Engine {
	eng := filo.NewEngine()
	filostrings.RegisterBuiltins(eng)
	filomath.RegisterBuiltins(eng)
	for _, name := range constructors {
		eng.MustRegisterBuiltin(name, record)
	}
	return eng
}

// record returns its arguments unchanged as a list.
func record(_ context.Context, args []filo.Value) (filo.Value, error) {
	return filo.VList(args), nil
}

// Eval evaluates src under the package limits. The name is only used to make
// errors point at the file the operator has to fix.
func Eval(ctx context.Context, name, src string) (filo.Value, error) {
	if len(src) > maxSourceSize {
		return filo.Value{}, fmt.Errorf("%s: %w (%d bytes, limit %d)", name, ErrTooLarge, len(src), maxSourceSize)
	}
	cfg := filo.EvalConfig{
		StepLimit:      stepLimit,
		RecursionLimit: recursionLimit,
		Timeout:        timeout,
	}
	value, _, err := newEngine().RunScript(ctx, src, nil, cfg)
	if err != nil {
		return filo.Value{}, fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

// Decode evaluates src and unmarshals the result into target.
func Decode(ctx context.Context, name, src string, target any) error {
	value, err := Eval(ctx, name, src)
	if err != nil {
		return err
	}
	err = filo.UnmarshalFromValue(value, target)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Marshal renders v as Filo text. Structured output is a contract, so this is
// the only place that decides how hostd writes Filo back out.
func Marshal(v any) (string, error) {
	out, err := filo.Marshal(v)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(repairEmptyLists(out), "\n") + "\n", nil
}

// repairEmptyLists rewrites `()` as `(list)`.
//
// filo.Marshal renders an empty slice as `()`, which its own evaluator rejects
// as an empty list expression, so a value with an empty field marshals to text
// that cannot be read back. A response that a client cannot decode is a broken
// contract, and an empty list is the most ordinary result there is: no
// services declared, no log lines matched.
//
// The rewrite skips string literals: a captured log line may legitimately
// contain the two characters, and rewriting inside one would corrupt the very
// output hostd exists to preserve.
func repairEmptyLists(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inString := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inString && c == '\\' && i+1 < len(src):
			// An escape carries its next byte with it, so a `\"` does not end
			// the string and a `\\` does not swallow the quote after it.
			b.WriteByte(c)
			i++
			b.WriteByte(src[i])
			continue
		case c == '"':
			inString = !inString
		case !inString && c == '(' && i+1 < len(src) && src[i+1] == ')':
			b.WriteString("(list)")
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
