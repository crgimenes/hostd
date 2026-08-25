package main

import (
	"context"
	"fmt"

	"github.com/crgimenes/hostd/api"
)

func runJob(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) == 0 {
		return exitUsage, fmt.Errorf("job needs a subcommand: run <name>")
	}
	if args[0] != "run" {
		return exitUsage, fmt.Errorf("unknown job subcommand %q; expected run", args[0])
	}
	if len(args) < 2 {
		return exitUsage, fmt.Errorf("job run needs a job name")
	}
	return runJobNow(ctx, client, opt, args[1])
}

// runJobNow asks for one turn of a job and returns as soon as it exists. It
// does not wait for the run to end: a job may take an hour, and a command that
// waited would time out on exactly the jobs worth watching. What it prints is
// how to watch it instead.
func runJobNow(ctx context.Context, client *api.Client, opt options, name string) (int, error) {
	resp, err := client.Do(ctx, api.Request{
		Op:               api.OpJobRun,
		Name:             name,
		ExpectGeneration: opt.expectGen,
		OnBehalfOf:       opt.onBehalfOf,
	})
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var started api.JobRun
	err = decode(ctx, resp.Body, &started)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, resp.Body, started, func() {
		_, _ = fmt.Fprintf(opt.out, "host %s, %s run %s started\n", client.Target(), started.Service, started.Run)
		_, _ = fmt.Fprintf(opt.out, "watch it with: hostctl -host %s log -service %s -run %s -follow\n",
			client.Target(), started.Service, started.Run)
	})
	return exitOK, nil
}
