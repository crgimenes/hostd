package main

import (
	"context"
	"fmt"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/service"
)

// runPush sends the declarations the operator keeps under version control to a
// machine. It does not apply them: what a machine holds and what it runs are
// different questions, and the second one is answered by apply, with a plan and
// a generation.
func runPush(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) > 0 {
		return exitUsage, fmt.Errorf("push takes no arguments; it sends the tree named by -config")
	}
	declarations, loadErr := service.LoadTree(ctx, opt.config)
	if len(declarations) == 0 {
		if loadErr != nil {
			return exitUsage, loadErr
		}
		return exitUsage, fmt.Errorf("no service is declared in %s", opt.config)
	}

	// Which machine this is, so a tree shared by a heterogeneous fleet sends
	// each machine only what is declared for it.
	machine := opt.entry(ctx, client.Target())

	out := opt.out
	sent := 0
	belongs := make([]service.Declaration, 0, len(declarations))
	for _, declaration := range declarations {
		if !declaration.Service.BelongsTo(machine.Name, machine.Tags) {
			continue
		}
		belongs = append(belongs, declaration)
	}
	for _, declaration := range belongs {
		payload, err := filoconf.Marshal(api.Send(declaration))
		if err != nil {
			return exitFailed, err
		}
		resp, err := client.Do(ctx, api.Request{
			Op:         api.OpServicePut,
			Body:       payload,
			OnBehalfOf: opt.onBehalfOf,
		})
		if err != nil {
			return exitComms, err
		}
		if resp.Failed() {
			return codeFor(resp.Err()), fmt.Errorf("%s: %w", declaration.Service.Name, resp.Err())
		}
		sent++
		_, _ = fmt.Fprintf(out, "%s;%s;%d\n", client.Target(), declaration.Service.Name, len(declaration.Artifacts))
	}
	// A tree that could not be read in full is not a tree that can say what a
	// machine should stop holding: the service missing from it may be the one
	// whose file failed to parse.
	if loadErr == nil {
		err := prune(ctx, client, opt, belongs, len(declarations))
		if err != nil {
			return exitFailed, err
		}
	}

	// A file on the machine has not changed what runs there. Saying so is what
	// keeps somebody from believing a push was a deploy.
	_, _ = fmt.Fprintf(out, "sent %d of %d declaration(s); run apply to converge\n", sent, len(declarations))
	// Broken files do not stop the good ones, and the status says the picture
	// is incomplete.
	if loadErr != nil {
		return exitPartial, loadErr
	}
	return exitOK, nil
}

// prune tells the machine which services the tree carries, so the ones deleted
// from it stop being held there. Deleting a file changes nothing that runs:
// the next plan proposes the removal, and an operator reviews it.
func prune(ctx context.Context, client *api.Client, opt options, belongs []service.Declaration, declared int) error {
	keep := api.ServiceSet{Names: make([]string, 0, len(belongs)), Declared: declared}
	for _, declaration := range belongs {
		keep.Names = append(keep.Names, declaration.Service.Name)
	}
	payload, err := filoconf.Marshal(keep)
	if err != nil {
		return err
	}
	resp, err := client.Do(ctx, api.Request{
		Op:         api.OpServicePrune,
		Body:       payload,
		OnBehalfOf: opt.onBehalfOf,
	})
	if err != nil {
		return err
	}
	if resp.Failed() {
		// An older daemon that does not know the operation is not a failed
		// push: everything the tree carries did arrive.
		if resp.Code == api.CodeUnknownOp {
			return nil
		}
		return resp.Err()
	}
	var removed api.ServiceSet
	err = decode(ctx, resp.Body, &removed)
	if err != nil {
		return err
	}
	for _, name := range removed.Names {
		_, _ = fmt.Fprintf(opt.out, "%s;%s;no longer declared\n", client.Target(), name)
	}
	return nil
}
