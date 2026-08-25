package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/crgimenes/hostd/api"
)

func versionRow(ref string, declared bool) api.ServiceVersion {
	return api.ServiceVersion{Digest: "sha256:" + strings.TrimPrefix(ref, "site:hostd-"), Ref: ref, Declared: declared}
}

// Stepping back from the declaration, not from the top of the list. Once a
// rollback has happened the newest version is the one somebody deliberately
// moved away from, and offering it as "back" would offer to undo the rollback.
func TestGoingBackStepsFromTheDeclaredVersion(t *testing.T) {
	versions := []api.ServiceVersion{
		versionRow("site:hostd-cccccccccccc", false),
		versionRow("site:hostd-bbbbbbbbbbbb", true),
		versionRow("site:hostd-aaaaaaaaaaaa", false),
	}
	previous, exists := previousTo(versions)
	if !exists || previous.Ref != "site:hostd-aaaaaaaaaaaa" {
		t.Fatalf("previousTo = %q, %v; want the version below the declared one", previous.Ref, exists)
	}
}

func TestThereIsNothingBelowTheOldestVersion(t *testing.T) {
	versions := []api.ServiceVersion{
		versionRow("site:hostd-bbbbbbbbbbbb", false),
		versionRow("site:hostd-aaaaaaaaaaaa", true),
	}
	_, exists := previousTo(versions)
	if exists {
		t.Fatal("the declared version is the oldest here, so there is nowhere to go back to")
	}
}

// A declaration naming an image this machine does not hold has no place in the
// list to step from, and the newest is then the useful answer.
func TestWithNothingDeclaredHereTheNewestIsOffered(t *testing.T) {
	versions := []api.ServiceVersion{
		versionRow("site:hostd-bbbbbbbbbbbb", false),
		versionRow("site:hostd-aaaaaaaaaaaa", false),
	}
	previous, exists := previousTo(versions)
	if !exists || previous.Ref != "site:hostd-bbbbbbbbbbbb" {
		t.Fatalf("previousTo = %q, %v; want the newest", previous.Ref, exists)
	}
}

// The command's output is an instruction, so the instruction has to be exact:
// the field name, the quoted reference, and the file to put it in.
func TestTheOutputCarriesTheEditToMake(t *testing.T) {
	var out bytes.Buffer
	printVersions(&out, "yuki.local", api.ServiceVersions{
		Service:    "site",
		Image:      "site:laptop",
		Repository: "site",
		Versions: []api.ServiceVersion{
			{Ref: "site:hostd-bbbbbbbbbbbb", Digest: "sha256:bbbbbbbbbbbb", Tags: []string{"site:laptop", "site:hostd-bbbbbbbbbbbb"}, Running: true, Declared: true},
			{Ref: "site:hostd-aaaaaaaaaaaa", Digest: "sha256:aaaaaaaaaaaa", Tags: []string{"site:hostd-aaaaaaaaaaaa"}},
		},
	})
	text := out.String()
	for _, want := range []string{
		`site.filo`,
		`(tuple "image" "site:hostd-aaaaaaaaaaaa")`,
		"running, declared",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the output does not carry %q:\n%s", want, text)
		}
	}
}

// Pinning by digest works and does not travel: the same declaration on another
// machine names nothing, because a digest is each runtime's own id for the
// bytes. Saying so is the difference between a rollback and a broken tree.
func TestPinningByDigestSaysItDoesNotTravel(t *testing.T) {
	var out bytes.Buffer
	printVersions(&out, "yuki.local", api.ServiceVersions{
		Service:    "site",
		Image:      "site:laptop",
		Repository: "site",
		Versions: []api.ServiceVersion{
			{Ref: "site:hostd-bbbbbbbbbbbb", Digest: "sha256:bbbbbbbbbbbb", Declared: true},
			{Ref: "sha256:aaaaaaaaaaaa", Digest: "sha256:aaaaaaaaaaaa"},
		},
	})
	if !strings.Contains(out.String(), "means nothing on another machine") {
		t.Fatalf("a digest pin has to say what it costs:\n%s", out.String())
	}
}

func TestAnImageTheMachineDoesNotHoldIsCalledOut(t *testing.T) {
	var out bytes.Buffer
	printVersions(&out, "yuki.local", api.ServiceVersions{
		Service:    "site",
		Image:      "site:laptop",
		Repository: "site",
		Versions:   []api.ServiceVersion{{Ref: "site:hostd-aaaaaaaaaaaa", Digest: "sha256:aaaaaaaaaaaa"}},
	})
	if !strings.Contains(out.String(), "a start would fail") {
		t.Fatalf("the declared image is not here and the output must say so:\n%s", out.String())
	}
}

// The exit status is the interface for whatever is not a person. Refused has to
// stay distinct from failed: an agent that reads "the ceiling is full" as a
// failure retries until the ceiling clears, and one that reads a conflict as
// success acts on a machine that did not do what it asked.
func TestTheExitStatusSeparatesRefusedFromFailed(t *testing.T) {
	for _, this := range []struct {
		code string
		want int
	}{
		{api.CodeOK, exitFailed},
		{api.CodeInvalid, exitUsage},
		{api.CodeUnknownOp, exitUsage},
		{api.CodeUnavailable, exitComms},
		{api.CodeConflict, exitRefused},
		{api.CodeDestructive, exitRefused},
		{api.CodeRefused, exitRefused},
		{api.CodeFailed, exitFailed},
		{api.CodeNotFound, exitFailed},
	} {
		got := codeFor(api.Error{Code: this.code})
		if got != this.want {
			t.Errorf("%s exits %d, want %d", this.code, got, this.want)
		}
	}
}
