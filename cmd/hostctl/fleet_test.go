package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crgimenes/hostd/filoconf"
)

func fleetOptions(t *testing.T, inventory string) options {
	t.Helper()
	dir := t.TempDir()
	opt := options{inventory: filepath.Join(dir, "inventory.filo"), out: &bytes.Buffer{}}
	if inventory != "" {
		err := os.WriteFile(opt.inventory, []byte(inventory), 0o600)
		if err != nil {
			t.Fatalf("write inventory: %v", err)
		}
	}
	return opt
}

const fleet = `(inventory
  (host (tuple "name" "yuki.local") (tuple "tags" (list "amd64" "docker")))
  (host (tuple "name" "selene.local") (tuple "tags" (list "amd64" "docker")))
  (host (tuple "name" "cronos.local") (tuple "tags" (list "arm64" "dashboard"))))`

// The fleet is the file the operator keeps; reaching any of the machines is
// ssh's business, and ssh keeps its own list.
func TestTheFleetIsTheInventory(t *testing.T) {
	opt := fleetOptions(t, fleet)
	opt.all = true

	chosen, err := opt.selection(context.Background())
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	want := []string{"yuki.local", "selene.local", "cronos.local"}
	if !slices.Equal(chosen, want) {
		t.Fatalf("the fleet is %v, expected %v", chosen, want)
	}
}

// A fleet nobody listed is an error that says where to list it, never a
// command that quietly does nothing.
func TestAnEmptyInventorySaysWhereToWriteIt(t *testing.T) {
	opt := fleetOptions(t, "")
	opt.all = true

	_, err := opt.selection(context.Background())
	if err == nil {
		t.Fatal("-all with no inventory was accepted")
	}
	if !strings.Contains(err.Error(), "inventory.filo") {
		t.Fatalf("the error does not say where the fleet is listed: %v", err)
	}
}

func TestSelectionByTag(t *testing.T) {
	const inventory = `(inventory
  (host (tuple "name" "yuki.local") (tuple "tags" (list "debian" "docker")))
  (host (tuple "name" "cronos.local") (tuple "tags" (list "arm64" "dashboard"))))`
	opt := fleetOptions(t, inventory)
	opt.tag = "arm64"

	chosen, err := opt.selection(context.Background())
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	if !slices.Equal(chosen, []string{"cronos.local"}) {
		t.Fatalf("the tag selected %v", chosen)
	}
}

// A tag nobody uses is a typo, and answering "no machines" by quietly doing
// nothing everywhere is how a fleet command becomes a no-op nobody notices.
func TestAnUnknownTagIsAnError(t *testing.T) {
	opt := fleetOptions(t, `(inventory (host (tuple "name" "yuki.local") (tuple "tags" (list "debian"))))`)
	opt.tag = "typo"

	_, err := opt.selection(context.Background())
	if err == nil {
		t.Fatal("a tag no machine carries selected the whole fleet or nothing, silently")
	}
}

// Mixing selectors would leave the operator guessing which one won.
func TestTwoSelectorsAreRefused(t *testing.T) {
	opt := fleetOptions(t, fleet)
	opt.all = true
	opt.host = "yuki.local"

	_, err := opt.selection(context.Background())
	if err == nil {
		t.Fatal("-all and -host together were accepted")
	}
}

func TestNoSelectorMeansTheMachineUnderThisProcess(t *testing.T) {
	opt := fleetOptions(t, fleet)
	chosen, err := opt.selection(context.Background())
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	if len(chosen) != 0 {
		t.Fatalf("with no selector the command chose %v instead of the local socket", chosen)
	}
}

// Partial success is its own outcome: read as failure it retries what already
// worked, read as success it acts on a picture with a hole in it.
func TestPartialSuccessHasItsOwnCode(t *testing.T) {
	table := []struct {
		name    string
		results []hostResult
		want    int
	}{
		{"all answered", []hostResult{{code: exitOK}, {code: exitOK}}, exitOK},
		{"some answered", []hostResult{{code: exitOK}, {code: exitComms}}, exitPartial},
		{"none answered", []hostResult{{code: exitComms}, {code: exitComms}}, exitComms},
		{"none answered, different reasons", []hostResult{{code: exitRefused}, {code: exitComms}}, exitRefused},
	}
	for _, test := range table {
		t.Run(test.name, func(t *testing.T) {
			got := fleetCode(test.results)
			if got != test.want {
				t.Fatalf("exit %d, expected %d", got, test.want)
			}
		})
	}
}

// A program reads the fleet the way it reads a host: one expression, every
// machine in it, including the ones that failed.
func TestTheFleetAnswerIsOneExpression(t *testing.T) {
	var out bytes.Buffer
	printFleetFilo(&out, []hostResult{
		{host: "yuki.local", code: exitOK, answer: `(list (tuple "state" "running"))`},
		{host: "selene.local", code: exitComms, err: context.DeadlineExceeded},
	})

	var fleet []struct {
		Host    string `filo:"host"`
		Exit    int    `filo:"exit"`
		Message string `filo:"message"`
	}
	err := filoconf.Decode(context.Background(), "fleet", out.String(), &fleet)
	if err != nil {
		t.Fatalf("the fleet answer does not parse: %v\n%s", err, out.String())
	}
	if len(fleet) != 2 {
		t.Fatalf("got %d machines in the answer, expected 2", len(fleet))
	}
	if fleet[0].Host != "yuki.local" || fleet[0].Exit != exitOK {
		t.Fatalf("the first machine came back as %#v", fleet[0])
	}
	if fleet[1].Exit != exitComms || !strings.Contains(fleet[1].Message, "deadline") {
		t.Fatalf("a machine that failed did not carry its reason: %#v", fleet[1])
	}
}

// A machine is called here what ~/.ssh/config calls it. The inventory adds tags
// to that name and nothing else: where it goes, who it logs in as and with
// which key are already written down once, in the file ssh itself reads.
func TestAMachineIsKnownByItsSSHName(t *testing.T) {
	opt := fleetOptions(t, `(inventory
	  (host (tuple "name" "web1") (tuple "tags" (list "web")))
	  (host (tuple "name" "yuki.local")))`)

	entry := opt.entry(context.Background(), "web1")
	if !slices.Equal(entry.Tags, []string{"web"}) {
		t.Fatalf("the machine came back as %#v", entry)
	}
	// Naming a machine the file does not know is how somebody reaches a new
	// one: it is still a machine, it just carries no tags.
	entry = opt.entry(context.Background(), "brand.new")
	if entry.Name != "brand.new" || len(entry.Tags) != 0 {
		t.Fatalf("an unknown machine came back as %#v", entry)
	}
}
