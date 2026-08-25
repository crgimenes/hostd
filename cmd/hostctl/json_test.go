package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

var errUnreachable = errors.New("unreachable: connection timed out")

func emitted(t *testing.T, opt options, filoBody string, structured any) string {
	t.Helper()
	var out bytes.Buffer
	opt.out = &out
	emit(opt, filoBody, structured, func() { _, _ = out.WriteString("HUMAN") })
	return out.String()
}

// Three shapes, one answer. Which one comes out is the caller's choice and
// nothing else leaks into it: a program parsing stdout must not have to skip a
// heading, and a person must not be handed a data format.
func TestEachOutputShapeCarriesOnlyItself(t *testing.T) {
	value := api.Description{Version: "v1", Protocol: 1}
	filoBody := `(list (tuple "version" "v1"))`

	human := emitted(t, options{}, filoBody, value)
	if human != "HUMAN" {
		t.Fatalf("the default output is not the human one: %q", human)
	}

	asFilo := emitted(t, options{filoOut: true}, filoBody, value)
	if strings.TrimSpace(asFilo) != filoBody {
		t.Fatalf("-filo did not pass the daemon's own bytes through: %q", asFilo)
	}

	asJSON := emitted(t, options{jsonOut: true}, filoBody, value)
	var back api.Description
	err := json.Unmarshal([]byte(asJSON), &back)
	if err != nil {
		t.Fatalf("-json produced something no JSON reader accepts: %v\n%s", err, asJSON)
	}
	if back.Version != "v1" {
		t.Fatalf("the answer did not survive the trip: %+v", back)
	}
}

// The JSON is rendered from the decoded answer, not from the Filo text, so the
// two cannot drift. This is what proves it: a body that says one thing and a
// value that says another must come out as the VALUE, because that is what
// every other output shape is built from too.
func TestJSONComesFromTheAnswerAndNotFromTheFiloText(t *testing.T) {
	asJSON := emitted(t, options{jsonOut: true},
		`(list (tuple "version" "from-the-text"))`,
		api.Description{Version: "from-the-value"})
	if !strings.Contains(asJSON, "from-the-value") {
		t.Fatalf("-json rendered the wire text rather than the answer:\n%s", asJSON)
	}
}

// A follower has no last line, so a reader waiting for a closing bracket would
// wait forever. One object per line is what every log tool already expects.
func TestFollowedLogLinesAreOneObjectPerLine(t *testing.T) {
	var out bytes.Buffer
	opt := options{jsonOut: true, out: &out}
	for _, line := range []api.LogLine{{Seq: 1, Text: "first"}, {Seq: 2, Text: "second"}} {
		err := printLine(opt, line)
		if err != nil {
			t.Fatalf("printLine: %v", err)
		}
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("two log lines came out as %d line(s):\n%s", len(lines), out.String())
	}
	for _, line := range lines {
		var one api.LogLine
		if err := json.Unmarshal([]byte(line), &one); err != nil {
			t.Fatalf("a streamed line is not JSON on its own: %v\n%s", err, line)
		}
	}
}

// Every type that reaches an operator names its fields the same way in both
// formats. Without this, an agent reads `last-error` from one command and
// `LastError` from the next, and the inconsistency is invisible until it is
// somebody else's parser.
func TestTheJSONNamesMatchTheFiloNames(t *testing.T) {
	for _, sample := range []any{
		api.Description{}, api.ImageEntry{}, api.ImagePrune{}, api.ImageChange{},
		api.Image{}, api.LogLine{},
		api.ServiceVersions{}, api.ServiceVersion{}, api.JobRun{},
		supervisor.Status{}, supervisor.Change{},
		state.Entry{}, metrics.Series{}, metrics.Point{},
	} {
		kind := reflect.TypeOf(sample)
		for field := range kind.Fields() {
			filo, tagged := field.Tag.Lookup("filo")
			if !tagged {
				continue
			}
			asJSON, ok := field.Tag.Lookup("json")
			if !ok {
				t.Errorf("%s.%s crosses to an operator with a filo name (%q) and no json name, so JSON would call it %q",
					kind.Name(), field.Name, filo, field.Name)
				continue
			}
			if name, _, _ := strings.Cut(asJSON, ","); name != filo {
				t.Errorf("%s.%s is %q in Filo and %q in JSON", kind.Name(), field.Name, filo, name)
			}
		}
	}
}

// Several machines answer as ONE document, not one per machine with a heading
// between them: whatever parses this reads stdout once. A machine that did not
// answer carries null rather than breaking the document for the ones that did.
func TestAFleetAnswersAsOneJSONDocument(t *testing.T) {
	var out bytes.Buffer
	printFleetJSON(&out, []hostResult{
		{host: "yuki", code: exitOK, answer: "[{\"name\":\"caddy\"}]\n"},
		{host: "cronos", code: exitComms, err: errUnreachable, answer: ""},
	})

	var answers []struct {
		Host    string          `json:"host"`
		Exit    int             `json:"exit"`
		Message string          `json:"message"`
		Body    json.RawMessage `json:"body"`
	}
	err := json.Unmarshal(out.Bytes(), &answers)
	if err != nil {
		t.Fatalf("a fleet answer is not one JSON document: %v\n%s", err, out.String())
	}
	if len(answers) != 2 {
		t.Fatalf("two machines came back as %d answer(s)", len(answers))
	}

	// The body is nested as JSON, not quoted into a string: a reader that had to
	// parse twice would be a reader that can forget to.
	var services []map[string]any
	if err := json.Unmarshal(answers[0].Body, &services); err != nil {
		t.Fatalf("the answering machine's body is not usable JSON: %v", err)
	}
	if len(services) != 1 || services[0]["name"] != "caddy" {
		t.Fatalf("the body did not survive nesting: %s", answers[0].Body)
	}

	if string(answers[1].Body) != "null" {
		t.Fatalf("a machine that said nothing carries %s, not null", answers[1].Body)
	}
	// Which machine is missing, and why, is part of the fleet's state.
	if !strings.Contains(answers[1].Message, "unreachable") {
		t.Fatalf("the silent machine does not say why: %q", answers[1].Message)
	}
	if answers[1].Exit != exitComms {
		t.Fatalf("the silent machine came back as exit %d", answers[1].Exit)
	}
}
