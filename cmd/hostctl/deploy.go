package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/supervisor"
)

// Deploy and remove are the two verbs the whole system exists for: the tree
// describes services, the inventory lists machines, and the operator puts any
// description on any machine that can run it — and takes it off again. WHERE a
// service runs is not written in any file: it is this decision, made here,
// each time.
//
// A deploy always overwrites: the current declaration, the current image when
// this machine holds it, and a fresh container. Deploying what already runs
// is how a new version goes live — that is the point, not an error.

// deployService is the whole deploy, told step by step through say as it
// happens: an operation that takes seconds in silence is an operation somebody
// clicks again. The command line and the window both run exactly this; the
// return is the final outcome line.
func deployService(ctx context.Context, client *api.Client, opt options, dir, name string, say func(string)) (string, error) {
	declaration, err := catalogEntry(ctx, dir, name)
	if err != nil {
		return "", err
	}
	err = ensureImage(ctx, client, declaration.Service.Image, say)
	if err != nil {
		return "", err
	}

	say(fmt.Sprintf("sending the declaration and %d file(s) beside it…", len(declaration.Artifacts)))
	err = putDeclaration(ctx, client, opt, declaration)
	if err != nil {
		return "", err
	}

	say("starting a fresh container…")
	resp, err := client.Do(ctx, api.Request{Op: api.OpServiceRedeploy, Name: name, OnBehalfOf: opt.onBehalfOf})
	if err != nil {
		return "", err
	}
	if resp.Failed() {
		return "", resp.Err()
	}
	var statuses []supervisor.Status
	state := name + " is deployed"
	err = decode(ctx, resp.Body, &statuses)
	if err == nil {
		for _, status := range statuses {
			if status.Name == name {
				state = fmt.Sprintf("%s is %s", name, status.State)
			}
		}
	}
	return state, nil
}

// ensureImage gets the declared image onto the machine, by whichever path is
// true: the version built HERE is pushed; one the machine already holds is
// left alone; anything else the machine pulls from its registry. Every path is
// said out loud, and only the push is fatal — for the rest, the start below is
// the judge of whether the image really arrived.
func ensureImage(ctx context.Context, client *api.Client, image string, say func(string)) error {
	local, err := docker.Open()
	if err == nil {
		_, err = local.Image(ctx, image)
		if err == nil {
			say(fmt.Sprintf("sending image %s from this machine…", image))
			received, pushErr := pushImage(ctx, client, local, image)
			if pushErr != nil {
				return pushErr
			}
			say(fmt.Sprintf("image %s sent (%s)", image, formatBytes(received.Bytes)))
			return nil
		}
	}
	if machineHolds(ctx, client, image) {
		say(fmt.Sprintf("image %s is already on the machine", image))
		return nil
	}
	say(fmt.Sprintf("image %s is not here nor there; the machine is pulling it from its registry…", image))
	resp, err := client.Do(ctx, api.Request{Op: api.OpImagePull, Name: image})
	if err != nil {
		return err
	}
	if resp.Failed() {
		// Not fatal: an older daemon, a machine with no runtime, a registry
		// that said no — the start below gives the final verdict either way.
		say("could not pull: " + resp.Err().Error())
		return nil
	}
	say(fmt.Sprintf("image %s pulled by the machine", image))
	return nil
}

// Whether the machine already holds the image under this exact name. An
// answer it cannot give (an older daemon, no runtime) reads as "no", and the
// pull below says the rest.
func machineHolds(ctx context.Context, client *api.Client, image string) bool {
	var held []api.ImageEntry
	err := ask(ctx, client, api.Request{Op: api.OpImageList}, &held)
	if err != nil {
		return false
	}
	for _, entry := range held {
		if slices.Contains(entry.Tags, image) {
			return true
		}
	}
	return false
}

// catalogEntry finds one service in the tree, and a miss answers with what the
// tree does describe: the next thing typed is one of these names.
func catalogEntry(ctx context.Context, dir, name string) (service.Declaration, error) {
	declarations, loadErr := service.LoadTree(ctx, dir)
	names := make([]string, 0, len(declarations))
	for _, declaration := range declarations {
		if declaration.Service.Name == name {
			return declaration, nil
		}
		names = append(names, declaration.Service.Name)
	}
	said := fmt.Sprintf("the tree at %s does not describe %q", dir, name)
	if len(names) > 0 {
		said += "; it describes " + strings.Join(names, ", ")
	}
	if loadErr != nil {
		said += fmt.Sprintf(" (and part of it could not be read: %v)", loadErr)
	}
	return service.Declaration{}, errors.New(said)
}

func runServiceDeploy(ctx context.Context, client *api.Client, opt options, name string) (int, error) {
	outcome, err := deployService(ctx, client, opt, opt.config, name, func(step string) {
		_, _ = fmt.Fprintf(opt.out, "%s: %s\n", client.Target(), step)
	})
	if err != nil {
		return codeFor(err), err
	}
	_, _ = fmt.Fprintf(opt.out, "%s: %s\n", client.Target(), outcome)
	_, _ = fmt.Fprintf(opt.out, "watch it with hostctl -host %s log -follow -service %s\n", client.Target(), name)
	return exitOK, nil
}

func runServiceRemove(ctx context.Context, client *api.Client, opt options, name string) (int, error) {
	resp, err := client.Do(ctx, api.Request{Op: api.OpServiceRemove, Name: name, OnBehalfOf: opt.onBehalfOf})
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var report api.Removal
	err = decode(ctx, resp.Body, &report)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, resp.Body, report, func() {
		_, _ = fmt.Fprintf(opt.out, "%s: container %s\n", client.Target(), report.Container)
		_, _ = fmt.Fprintf(opt.out, "%s: image %s\n", client.Target(), report.Image)
		_, _ = fmt.Fprintf(opt.out, "%s: %s removed; the tree still describes it, and a deploy puts it back\n",
			client.Target(), report.Service)
	})
	return exitOK, nil
}
