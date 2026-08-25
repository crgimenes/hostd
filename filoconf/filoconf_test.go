package filoconf

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type svc struct {
	Name    string   `filo:"name"`
	Kind    string   `filo:"kind"`
	Args    []string `filo:"args"`
	Enabled bool     `filo:"enabled"`
}

func TestDecodeRecord(t *testing.T) {
	src := `(service
	  (tuple "name" "api")
	  (tuple "kind" "exec")
	  (tuple "args" (list "--listen" ":8080"))
	  (tuple "enabled" #t))`
	var got svc
	err := Decode(context.Background(), "api.filo", src, &got)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Name != "api" || got.Kind != "exec" || !got.Enabled {
		t.Fatalf("unexpected decode: %+v", got)
	}
	if len(got.Args) != 2 || got.Args[0] != "--listen" || got.Args[1] != ":8080" {
		t.Fatalf("unexpected args: %#v", got.Args)
	}
}

// A configuration file is a program, so a value may be computed. What it may
// not do is act on the machine.
func TestDecodeComputedValue(t *testing.T) {
	src := `(let ((port 8080))
	  (service
	    (tuple "name" "api")
	    (tuple "args" (list (str-concat ":" (string port))))))`
	var got svc
	err := Decode(context.Background(), "api.filo", src, &got)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Args) != 1 || got.Args[0] != ":8080" {
		t.Fatalf("unexpected args: %#v", got.Args)
	}
}

func TestNoSideEffectBuiltins(t *testing.T) {
	// None of these may exist in a configuration engine. The test is the
	// guard: adding a convenience builtin that touches the host would make it
	// fail.
	forbidden := []string{
		`(print "x")`,
		`(println "x")`,
		`(exec "/bin/sh")`,
		`(read-file "/etc/passwd")`,
		`(http-get "http://example.com")`,
		`(rand-int 10)`,
		`(uuid-v4)`,
	}
	for _, src := range forbidden {
		_, err := Eval(context.Background(), "t.filo", src)
		if err == nil {
			t.Errorf("%s evaluated without error; configuration must not reach the host", src)
		}
	}
}

func TestStepLimitStopsRunawayLoop(t *testing.T) {
	// A configuration that never finishes must fail as an error instead of
	// hanging the daemon that reads it.
	src := `(def loop (fn (n) (loop (+ n 1)))) (loop 0)`
	_, err := Eval(context.Background(), "t.filo", src)
	if err == nil {
		t.Fatal("runaway recursion evaluated without error")
	}
}

func TestSourceSizeLimit(t *testing.T) {
	src := strings.Repeat("; comment\n", (maxSourceSize/10)+1)
	_, err := Eval(context.Background(), "big.filo", src)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

func TestErrorNamesTheFile(t *testing.T) {
	_, err := Eval(context.Background(), "broken.filo", `(service (tuple "name"`)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if !strings.Contains(err.Error(), "broken.filo") {
		t.Fatalf("error does not name the file the operator must fix: %v", err)
	}
}

func TestContextCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Eval(ctx, "t.filo", `(def loop (fn (n) (loop (+ n 1)))) (loop 0)`)
	if err == nil {
		t.Fatal("evaluation ignored a cancelled context")
	}
}

// An empty result is the most ordinary result there is, so it has to survive
// the round trip. filo once rendered an empty slice as "()", which its own
// evaluator rejected, and hostd carried a repair for it until filo v0.0.18
// fixed the source; this is what says the repair is not needed again.
func TestEmptyValuesRoundTrip(t *testing.T) {
	type row struct {
		Name string   `filo:"name"`
		Args []string `filo:"args"`
	}
	cases := []struct {
		why string
		in  any
	}{
		{"empty slice at the top level", []row{}},
		{"empty slice inside a struct", row{Name: "api"}},
		{"empty slice of strings", []string{}},
	}
	for _, c := range cases {
		out, err := Marshal(c.in)
		if err != nil {
			t.Errorf("%s: Marshal: %v", c.why, err)
			continue
		}
		_, err = Eval(context.Background(), "t.filo", out)
		if err != nil {
			t.Errorf("%s: rendered %s, which cannot be read back: %v", c.why, strings.TrimSpace(out), err)
		}
	}
}

// A captured log line is whatever a program decided to write: parentheses,
// backslashes and quotes included. It has to come back exactly as it went in,
// because preserving it is what hostd is for — and text that gets rewritten on
// the way through is the failure mode any future shortcut here would have.
func TestACapturedLineSurvivesMarshallingUnchanged(t *testing.T) {
	type line struct {
		Text  string   `filo:"text"`
		Extra []string `filo:"extra"`
	}
	original := `panic: runtime error () at foo\bar "quoted"`
	out, err := Marshal(line{Text: original})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back line
	err = Decode(context.Background(), "t.filo", out, &back)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if back.Text != original {
		t.Fatalf("log text was rewritten:\n got %q\nwant %q", back.Text, original)
	}
	if len(back.Extra) != 0 {
		t.Fatalf("empty field decoded as %#v", back.Extra)
	}
}

// hostd's persistent state lives for years across upgrades, so a document
// written by an older version must not zero the fields it never heard of.
// Decoding starts from the current value and overlays what the document
// carries; a field that is absent keeps its default.
func TestAbsentFieldsKeepTheirDefaults(t *testing.T) {
	type snapshot struct {
		Name    string `filo:"name"`
		Buffer  int    `filo:"buffer"`
		Enabled bool   `filo:"enabled"`
	}
	got := snapshot{Name: "old", Buffer: 42, Enabled: true}
	err := Decode(context.Background(), "old.filo", `(host (tuple "name" "new"))`, &got)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Name != "new" {
		t.Errorf("declared field was not applied: %+v", got)
	}
	if got.Buffer != 42 || !got.Enabled {
		t.Errorf("an older document zeroed fields it does not carry: %+v", got)
	}
}
