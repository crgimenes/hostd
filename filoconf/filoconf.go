// Package filoconf evaluates hostd's Filo configuration and decodes it.
//
// No builtin registered here touches files, network or processes: a
// configuration is a calculation that produces a declaration.
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

// A runaway configuration must end as an error, never as a hung daemon.
const (
	stepLimit      = 200_000
	recursionLimit = 64
	timeout        = 2 * time.Second
)

// Checked before parsing: the evaluation limits do not apply to reading a
// file into memory.
const maxSourceSize = 1 << 20

var ErrTooLarge = errors.New("filoconf: source too large")

// Each returns its arguments as a list, so `(service (tuple "name" "api"))`
// evaluates to the key/value list Decode expects.
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

func record(_ context.Context, args []filo.Value) (filo.Value, error) {
	return filo.VList(args), nil
}

// Eval evaluates src under the package limits. The name appears in errors, so
// it must be the file the operator has to fix.
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

func Marshal(v any) (string, error) {
	out, err := filo.Marshal(v)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(repairEmptyLists(out), "\n") + "\n", nil
}

// filo.Marshal renders an empty slice as `()`, which its own evaluator rejects,
// so an empty result would not survive a round trip. String literals are
// skipped: a captured log line may contain those two characters.
func repairEmptyLists(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inString := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inString && c == '\\' && i+1 < len(src):
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
