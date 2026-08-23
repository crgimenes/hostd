package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/docker"
)

// runImage pushes an image built on this machine to another one, over the same
// ssh the commands travel on. There is no registry in the middle: the host
// fetches nothing by itself, which is what keeps the install to a binary and
// the machine free of credentials for a service it never asked for.
func runImage(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) == 0 || args[0] != "push" {
		return exitUsage, fmt.Errorf("image needs a subcommand: push <image>")
	}
	if len(args) < 2 {
		return exitUsage, fmt.Errorf("image push needs the image to send, as it is named here")
	}
	image := args[1]

	local, err := docker.Open()
	if err != nil {
		return exitFailed, fmt.Errorf("no container runtime on this machine to read %s from: %w", image, err)
	}
	built, err := local.Image(ctx, image)
	if err != nil {
		return exitFailed, fmt.Errorf("%s is not on this machine; build it first: %w", image, err)
	}

	// The tar goes from one runtime to the other through the pipe: nothing is
	// written to disk on either side, and a machine never holds an image it is
	// only passing on.
	content, writer := io.Pipe()
	go func() {
		saveErr := local.Save(ctx, image, writer)
		_ = writer.CloseWithError(saveErr)
	}()
	sent := sha256.New()

	resp, err := client.Push(ctx, image, built.Arch, io.TeeReader(content, sent))
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var received api.Image
	err = decode(ctx, resp.Body, &received)
	if err != nil {
		return exitFailed, err
	}
	// What proves the transfer is the hash of the bytes, not the image id: two
	// daemons reading the same archive arrive at different ids, because an id
	// is of the config each one writes.
	sum := hex.EncodeToString(sent.Sum(nil))
	if received.Content != sum {
		return exitFailed, fmt.Errorf(
			"%s arrived as sha256:%s and left here as sha256:%s; the bytes did not survive the trip",
			image, received.Content, sum)
	}
	// The id to declare is the one that machine now has: it is the machine
	// that will run it.
	emit(opt, resp.Body, func() {
		_, _ = fmt.Fprintf(opt.out, "%s;%s;%.0f;sha256:%s\n",
			client.Target(), received.Digest, received.Bytes, received.Content)
	})
	return exitOK, nil
}
