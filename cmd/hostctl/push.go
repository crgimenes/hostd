package main

import (
	"context"
	"fmt"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/supervisor"
)

// runPush refreshes the descriptions of the services this machine already
// runs. The tree describes services; WHERE they run is decided by deploy and
// remove — so a push adds nothing and takes nothing away, and what runs does
// not change until an apply (or a deploy, which overwrites) says so.
func runPush(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) > 0 {
		return exitUsage, fmt.Errorf("push takes no arguments; it refreshes this machine's services from the tree named by -config")
	}
	resp, err := client.Do(ctx, api.Request{Op: api.OpServiceList})
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var held []supervisor.Status
	err = decode(ctx, resp.Body, &held)
	if err != nil {
		return exitFailed, err
	}

	declarations, loadErr := service.LoadTree(ctx, opt.config)
	described := make(map[string]service.Declaration, len(declarations))
	for _, declaration := range declarations {
		described[declaration.Service.Name] = declaration
	}

	out := opt.out
	sent := 0
	for _, status := range held {
		if status.Orphan {
			continue
		}
		declaration, ok := described[status.Name]
		if !ok {
			_, _ = fmt.Fprintf(out, "%s;%s;runs here but the tree does not describe it\n", client.Target(), status.Name)
			continue
		}
		err = putDeclaration(ctx, client, opt, declaration)
		if err != nil {
			return codeFor(err), err
		}
		sent++
		_, _ = fmt.Fprintf(out, "%s;%s;%d\n", client.Target(), declaration.Service.Name, len(declaration.Artifacts))
	}
	// A file on the machine has not changed what runs there. Saying so is what
	// keeps somebody from believing a push was a deploy.
	_, _ = fmt.Fprintf(out, "refreshed %d declaration(s); run apply to converge, or service deploy to overwrite one\n", sent)
	// Broken files do not stop the good ones, and the status says the picture
	// is incomplete.
	if loadErr != nil {
		return exitPartial, loadErr
	}
	return exitOK, nil
}

// What a tree shared by a heterogeneous fleet declares for one machine.
func belongingTo(declarations []service.Declaration, machine inventoryEntry) []service.Declaration {
	out := make([]service.Declaration, 0, len(declarations))
	for _, declaration := range declarations {
		if !declaration.Service.BelongsTo(machine.Name, machine.Tags) {
			continue
		}
		out = append(out, declaration)
	}
	return out
}

// putDeclaration sends one declaration and whatever travels with it. Sending is
// not applying: the machine holds the file and runs what it ran before.
func putDeclaration(ctx context.Context, client *api.Client, opt options, declaration service.Declaration) error {
	payload, err := filoconf.Marshal(api.Send(declaration))
	if err != nil {
		return err
	}
	resp, err := client.Do(ctx, api.Request{
		Op:         api.OpServicePut,
		Body:       payload,
		OnBehalfOf: opt.onBehalfOf,
	})
	if err != nil {
		return err
	}
	if resp.Failed() {
		return fmt.Errorf("%s: %w", declaration.Service.Name, resp.Err())
	}
	return nil
}

// pruneTree tells the machine which services the tree carries, so the ones
// deleted from it stop being held there, and answers with what was let go.
// Deleting a file changes nothing that runs: the next plan proposes the
// removal, and an operator reviews it.
func pruneTree(ctx context.Context, client *api.Client, opt options, belongs []service.Declaration, declared int) ([]string, error) {
	keep := api.ServiceSet{Names: make([]string, 0, len(belongs)), Declared: declared}
	for _, declaration := range belongs {
		keep.Names = append(keep.Names, declaration.Service.Name)
	}
	payload, err := filoconf.Marshal(keep)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(ctx, api.Request{
		Op:         api.OpServicePrune,
		Body:       payload,
		OnBehalfOf: opt.onBehalfOf,
	})
	if err != nil {
		return nil, err
	}
	if resp.Failed() {
		// An older daemon that does not know the operation is not a failed
		// push: everything the tree carries did arrive.
		if resp.Code == api.CodeUnknownOp {
			return nil, nil
		}
		return nil, resp.Err()
	}
	var removed api.ServiceSet
	err = decode(ctx, resp.Body, &removed)
	if err != nil {
		return nil, err
	}
	return removed.Names, nil
}
