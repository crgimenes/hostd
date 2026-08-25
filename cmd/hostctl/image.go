package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/docker"
)

func runImage(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) == 0 {
		return exitUsage, fmt.Errorf("image needs a subcommand: ls, or push <image>")
	}
	switch args[0] {
	case "ls", "list":
		return runImageList(ctx, client, opt)
	case "push":
		return runImagePush(ctx, client, opt, args[1:])
	default:
		return exitUsage, fmt.Errorf("unknown image subcommand %q; expected ls or push", args[0])
	}
}

// The images belong to the host, so the host is asked: hostctl runs on the
// operator's machine and never on the one it operates. The only runtime it
// opens itself is the local one in push below, and only to read the image it
// is about to send.
func runImageList(ctx context.Context, client *api.Client, opt options) (int, error) {
	resp, err := client.Do(ctx, api.Request{Op: api.OpImageList})
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var held []api.ImageEntry
	err = decode(ctx, resp.Body, &held)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, resp.Body, func() { printImages(opt.out, client.Target(), held) })
	return exitOK, nil
}

func printImages(out io.Writer, target string, held []api.ImageEntry) {
	_, _ = fmt.Fprintf(out, "host %s\n", target)
	if len(held) == 0 {
		_, _ = fmt.Fprintln(out, "this machine holds no images")
		return
	}
	var total, ourTotal, free float64
	ours, loose := 0, 0
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "IMAGE\tSIZE\tCREATED\tUSED BY\tOWNER\tDIGEST")
	for _, image := range held {
		total += image.Bytes
		used := image.UsedBy
		if used == "" {
			used = "-"
		}
		if image.Managed {
			ours++
			ourTotal += image.Bytes
			if image.UsedBy == "" {
				loose++
				free += image.Bytes
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			tagsText(image.Tags),
			formatBytes(image.Bytes),
			time.UnixMilli(int64(image.Created)).Format("2006-01-02 15:04"),
			used,
			ownerText(image.Managed),
			shortDigest(image.Digest))
	}
	_ = w.Flush()
	_, _ = fmt.Fprintf(out, "%d images, %s\n", len(held), formatBytes(total))
	_, _ = fmt.Fprintf(out, "%d put here by hostd, %s\n", ours, formatBytes(ourTotal))
	if loose == 0 {
		return
	}
	// Only ours are ever candidates: what another system built on this machine
	// is reported and never counted as something to reclaim. Nothing here
	// removes anything — that is a separate command, and a destructive one.
	_, _ = fmt.Fprintf(out, "%d of ours held by nothing, %s\n", loose, formatBytes(free))
}

// An image hostd did not put here belongs to whatever else runs on the machine.
// Naming that plainly is the point: it is reported, not accounted for.
func ownerText(managed bool) string {
	if managed {
		return "hostd"
	}
	return "other"
}

// An untagged image is not nameless: it is startable by digest, which is what
// a declaration pinned to one does. Saying "untagged" rather than printing
// nothing says the row is a version, not a defect.
func tagsText(tags []string) string {
	if len(tags) == 0 {
		return "<untagged>"
	}
	return strings.Join(tags, ", ")
}

func shortDigest(digest string) string {
	const shown = 12
	trimmed := strings.TrimPrefix(digest, "sha256:")
	if len(trimmed) <= shown {
		return trimmed
	}
	return trimmed[:shown]
}

// runImagePush sends an image built on this machine to another one, over the
// same ssh the commands travel on. There is no registry in the middle: the host
// fetches nothing by itself, which is what keeps the install to a binary and
// the machine free of credentials for a service it never asked for.
func runImagePush(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) == 0 {
		return exitUsage, fmt.Errorf("image push needs the image to send, as it is named here")
	}
	image := args[0]

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
