package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/service"
)

func runImage(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) == 0 {
		return exitUsage, fmt.Errorf("image needs a subcommand: ls, or push <image>")
	}
	switch args[0] {
	case "ls", "list":
		return runImageList(ctx, client, opt)
	case "prune":
		return runImagePrune(ctx, client, opt)
	case "push":
		return runImagePush(ctx, client, opt, args[1:])
	default:
		return exitUsage, fmt.Errorf("unknown image subcommand %q; expected ls, prune or push", args[0])
	}
}

// runImagePrune shows what would go, and removes it only when authorised. The
// plan and the removal are one computation on the daemon, so what is printed
// here without the flag is exactly what goes with it.
func runImagePrune(ctx context.Context, client *api.Client, opt options) (int, error) {
	// A request that carries no keep means "your default"; a person who typed
	// zero means something else entirely, and quietly giving them three would
	// hide that the machine did not do what they asked.
	if opt.keep < 1 {
		return exitUsage, fmt.Errorf("-keep is how many versions survive, so it is at least 1; got %d", opt.keep)
	}
	resp, err := client.Do(ctx, api.Request{
		Op:               api.OpImagePrune,
		Keep:             opt.keep,
		AllowDestructive: opt.allowDestr,
		OnBehalfOf:       opt.onBehalfOf,
	})
	if err != nil {
		return exitComms, err
	}
	var plan api.ImagePrune
	// A partial failure still carries the plan: which images went and which
	// would not is the whole answer, and dropping it would leave the operator
	// with a count and no names.
	decodeErr := decode(ctx, resp.Body, &plan)
	if resp.Failed() && decodeErr != nil {
		return codeFor(resp.Err()), resp.Err()
	}
	if decodeErr != nil {
		return exitFailed, decodeErr
	}
	emit(opt, resp.Body, plan, func() { printPrune(opt.out, client.Target(), plan) })
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	return exitOK, nil
}

func printPrune(out io.Writer, target string, plan api.ImagePrune) {
	_, _ = fmt.Fprintf(out, "host %s, keeping %d version(s) of each image\n", target, plan.Keep)
	if len(plan.Remove) == 0 {
		_, _ = fmt.Fprintf(out, "nothing to remove; %d image(s) of ours are held or within the %d kept\n", plan.Kept, plan.Keep)
		return
	}
	var freed float64
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "IMAGE\tSIZE\tDIGEST\tRESULT")
	for _, image := range plan.Remove {
		result := "would remove"
		switch {
		case image.Problem != "":
			result = image.Problem
		case image.Removed:
			result = "removed"
			freed += image.Bytes
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			tagsText(image.Tags), formatBytes(image.Bytes), shortDigest(image.Digest), result)
	}
	_ = w.Flush()
	if !plan.Applied {
		var wouldFree float64
		for _, image := range plan.Remove {
			wouldFree += image.Bytes
		}
		_, _ = fmt.Fprintf(out, "%d image(s), %s; nothing was removed\n", len(plan.Remove), formatBytes(wouldFree))
		_, _ = fmt.Fprintln(out, "run it again with -allow-destructive to remove them")
		return
	}
	_, _ = fmt.Fprintf(out, "%d image(s) removed, %s freed; %d kept\n", len(plan.Remove), formatBytes(freed), plan.Kept)
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
	emit(opt, resp.Body, held, func() { printImages(opt.out, client.Target(), held) })
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
	received, err := pushImage(ctx, client, local, image, nil)
	if errors.Is(err, errComms) {
		return exitComms, err
	}
	if err != nil {
		return codeFor(err), err
	}
	// The id to declare is the one that machine now has: it is the machine
	// that will run it.
	emit(opt, received.Body, received.Image, func() {
		_, _ = fmt.Fprintf(opt.out, "%s;%s;%.0f;sha256:%s\n",
			client.Target(), received.Digest, received.Bytes, received.Content)
		printNotADeploy(opt.out, received.Image)
	})
	return exitOK, nil
}

// A transport failure and an operation failure are different exit codes, and
// the wrapping is how the shared push says which one it was.
var errComms = errors.New("could not reach the machine")

// pushImage streams an image out of the local runtime into the target's. The
// tar goes from one runtime to the other through the pipe: nothing touches a
// disk on either side, and what proves the transfer is the hash of the bytes —
// two daemons reading the same archive arrive at different image ids, because
// an id is of the config each one writes.
func pushImage(ctx context.Context, client *api.Client, local *docker.Client, image string, progress func(int64)) (pushedImage, error) {
	built, err := local.Image(ctx, image)
	if err != nil {
		return pushedImage{}, fmt.Errorf("%s is not on this machine; build it first: %w", image, err)
	}
	content, writer := io.Pipe()
	go func() {
		saveErr := local.Save(ctx, image, writer)
		_ = writer.CloseWithError(saveErr)
	}()
	sent := sha256.New()

	body := io.Reader(io.TeeReader(content, sent))
	if progress != nil {
		// A gigabyte crossing an ssh pipe in silence is the wait that makes
		// somebody click again.
		body = docker.Progress(body, progress)
	}
	resp, err := client.Push(ctx, image, built.Arch, body)
	if err != nil {
		return pushedImage{}, fmt.Errorf("%w: %v", errComms, err)
	}
	if resp.Failed() {
		return pushedImage{}, resp.Err()
	}
	var received api.Image
	err = decode(ctx, resp.Body, &received)
	if err != nil {
		return pushedImage{}, err
	}
	sum := hex.EncodeToString(sent.Sum(nil))
	if received.Content != sum {
		return pushedImage{}, fmt.Errorf(
			"%s arrived as sha256:%s and left here as sha256:%s; the bytes did not survive the trip",
			image, received.Content, sum)
	}
	return pushedImage{Image: received, Body: resp.Body}, nil
}

// What a push answers with, plus the wire text an emit passes through.
type pushedImage struct {
	api.Image
	Body string
}

// A push moves bytes and moves a tag. It does not change what any service is
// running, and nothing else says so: the next apply reports "nothing to change"
// because the declaration still reads exactly as it did, and a restart brings
// the container back on the image it was created from. Whoever just pushed is
// the person who needs to know that, at the moment they need it.
func printNotADeploy(out io.Writer, received api.Image) {
	if received.Ref == "" {
		// An older daemon did not answer with the stamp. Guessing one would be
		// worse than saying less.
		return
	}
	_, _ = fmt.Fprintf(out, "marked as %s\n", received.Ref)
	_, _ = fmt.Fprintln(out, "a push is not a deploy: what a service runs does not change until its")
	_, _ = fmt.Fprintf(out, "declaration names this version. Put this in the service's %s, then push\n", service.Extension)
	_, _ = fmt.Fprintln(out, "and apply:")
	_, _ = fmt.Fprintf(out, "  (tuple \"image\" %q)\n", received.Ref)
}
