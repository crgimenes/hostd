package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/crgimenes/hostd/filoconf"
)

// Enough to answer a small fleet in one round trip, few enough that a laptop
// on hotel wifi is not opening a hundred connections at once.
const fleetConcurrency = 8

// The fleet is a file the operator keeps: which machines exist and what to call
// groups of them. A machine is named by whatever ssh is given, so the name here
// is the name in ~/.ssh/config — the address, the user, the port, the key and
// the jump host are all that file's business, which the operator already keeps
// and which every other tool on their machine already honours. A second place
// to write them would be a second place to disagree with ssh.
type inventoryEntry struct {
	Name string   `filo:"name"`
	Tags []string `filo:"tags"`
}

func (o options) inventoryPath() string { return o.inventory }

// A missing inventory is not an error: a fleet with no tags is a fleet.
func readInventory(ctx context.Context, path string) ([]inventoryEntry, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator's own inventory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []inventoryEntry
	err = filoconf.Decode(ctx, filepath.Base(path), string(data), &entries)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// entry is what the inventory knows about a machine. One the file does not
// list is still a machine — naming a machine that is not in the file is how
// somebody reaches a new one — it just carries no tags.
func (o options) entry(ctx context.Context, name string) inventoryEntry {
	entries, err := readInventory(ctx, o.inventoryPath())
	if err != nil {
		return inventoryEntry{Name: name}
	}
	for _, candidate := range entries {
		if candidate.Name == name {
			return candidate
		}
	}
	return inventoryEntry{Name: name}
}

// selection turns the flags into the machines to ask. Exactly one selector is
// allowed: mixing them would leave the operator guessing which one won.
func (o options) selection(ctx context.Context) ([]string, error) {
	chosen := 0
	for _, set := range []bool{o.host != "", len(o.hosts) > 0, o.all, o.tag != ""} {
		if set {
			chosen++
		}
	}
	if chosen > 1 {
		return nil, fmt.Errorf("choose one of -host, -hosts, -all or -tag, not several")
	}
	switch {
	case o.host != "":
		return []string{o.host}, nil
	case len(o.hosts) > 0:
		return o.hosts, nil
	case o.all:
		return o.fleet(ctx)
	case o.tag != "":
		return o.tagged(ctx)
	}
	return nil, nil
}

func (o options) fleet(ctx context.Context) ([]string, error) {
	entries, err := readInventory(ctx, o.inventoryPath())
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no machine is listed in %s; add one, or name it with -host", o.inventoryPath())
	}
	return out, nil
}

func (o options) tagged(ctx context.Context) ([]string, error) {
	entries, err := readInventory(ctx, o.inventoryPath())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if slices.Contains(entry.Tags, o.tag) {
			out = append(out, entry.Name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no machine carries the tag %q in %s", o.tag, o.inventoryPath())
	}
	return out, nil
}

type hostResult struct {
	host   string
	code   int
	err    error
	answer string
}

// fanOut asks every machine the same thing at once and reports what each one
// said. A host that fails does not stop the others: the answer that arrives is
// worth having, and the exit status says the picture is incomplete.
func fanOut(ctx context.Context, opt options, hosts []string, args []string) int {
	results := make([]hostResult, len(hosts))
	done := make([]chan struct{}, len(hosts))
	slots := make(chan struct{}, fleetConcurrency)
	var wg sync.WaitGroup
	for i, host := range hosts {
		done[i] = make(chan struct{})
		wg.Go(func() {
			defer close(done[i])
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i] = askOne(ctx, opt, host, args)
		})
	}

	// A machine-readable answer for several machines is ONE document, never a
	// document per machine with a heading between them: whatever parses it
	// reads stdout once. That one costs waiting for every machine.
	if opt.filoOut || opt.jsonOut {
		wg.Wait()
		if opt.filoOut {
			printFleetFilo(opt.out, results)
			return fleetCode(results)
		}
		printFleetJSON(opt.out, results)
		return fleetCode(results)
	}
	// Inventory order, and each machine's block leaves as soon as the machines
	// before it have had theirs: this used to print nothing at all until the
	// last machine answered, so `-all install` with one switched-off machine
	// was a blank screen — and a program that looks frozen is one the operator
	// interrupts, which is exactly what happened.
	for i := range hosts {
		<-done[i]
		printHostResult(opt.out, results[i])
	}
	wg.Wait()
	return fleetCode(results)
}

// A machine that did not answer belongs in the picture, in the picture's own
// stream: which host is missing is part of the fleet's state, not a diagnostic
// about hostctl.
func printHostResult(out io.Writer, result hostResult) {
	_, _ = fmt.Fprintf(out, "== %s\n", result.host)
	if result.answer != "" {
		_, _ = fmt.Fprint(out, result.answer)
		return
	}
	if result.err != nil {
		_, _ = fmt.Fprintf(out, "did not answer: %v\n", result.err)
		return
	}
	_, _ = fmt.Fprintln(out, "did not answer")
}

// Each machine writes into its own buffer, so two answers arriving at once
// cannot interleave into one unreadable page.
func askOne(ctx context.Context, opt options, host string, args []string) hostResult {
	var answer bytes.Buffer
	one := opt
	one.host = host
	one.out = &answer
	code, err := dispatch(ctx, one, args)
	return hostResult{host: host, code: code, err: err, answer: answer.String()}
}

// Partial success is its own outcome: an agent that reads it as failure
// retries what already worked, and one that reads it as success acts on a
// picture with a hole in it.
func fleetCode(results []hostResult) int {
	worked, failed := 0, 0
	first := exitOK
	for _, result := range results {
		if result.code == exitOK {
			worked++
			continue
		}
		failed++
		if first == exitOK {
			first = result.code
		}
	}
	switch {
	case failed == 0:
		return exitOK
	case worked == 0:
		return first
	default:
		return exitPartial
	}
}

// What one machine answered, inside the fleet's single document. Body is raw
// on purpose: the per-host answer is already JSON, and quoting it into a string
// would make every reader parse twice.
type fleetAnswer struct {
	Host    string          `json:"host"`
	Exit    int             `json:"exit"`
	Message string          `json:"message"`
	Body    json.RawMessage `json:"body"`
}

func printFleetJSON(out io.Writer, results []hostResult) {
	answers := make([]fleetAnswer, 0, len(results))
	for _, result := range results {
		one := fleetAnswer{Host: result.host, Exit: result.code, Body: json.RawMessage("null")}
		if result.err != nil {
			one.Message = result.err.Error()
		}
		body := strings.TrimSpace(result.answer)
		// A machine that did not answer, or answered something no reader
		// accepts, carries null rather than breaking the whole document for
		// the machines that did answer.
		if json.Valid([]byte(body)) {
			one.Body = json.RawMessage(body)
		}
		answers = append(answers, one)
	}
	writeJSON(out, answers)
}

// One expression carrying every machine's answer, so a program reads the fleet
// the way it reads a host.
func printFleetFilo(out io.Writer, results []hostResult) {
	var b strings.Builder
	b.WriteString("(list")
	for _, result := range results {
		body := strings.TrimSpace(result.answer)
		if body == "" {
			body = "(list)"
		}
		message := ""
		if result.err != nil {
			message = result.err.Error()
		}
		_, _ = fmt.Fprintf(&b, " (list (tuple \"host\" %q) (tuple \"exit\" %d) (tuple \"message\" %q) (tuple \"body\" %s))",
			result.host, result.code, message, body)
	}
	b.WriteString(")")
	_, _ = fmt.Fprintln(out, b.String())
}
