package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/service"
)

// runServiceVersions shows what a service could be put back on, and the exact
// line to write to do it. It changes nothing: the tree is the source of desired
// state, so going back is an edit the operator makes and an apply converges —
// the same shape migrate settled on, for the same reason.
func runServiceVersions(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) == 0 {
		return exitUsage, fmt.Errorf("service versions needs a service name")
	}
	resp, err := client.Do(ctx, api.Request{Op: api.OpServiceVersions, Name: args[0]})
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var found api.ServiceVersions
	err = decode(ctx, resp.Body, &found)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, resp.Body, found, func() { printVersions(opt.out, client.Target(), found) })
	return exitOK, nil
}

func printVersions(out io.Writer, target string, found api.ServiceVersions) {
	_, _ = fmt.Fprintf(out, "host %s, service %s\n", target, found.Service)
	_, _ = fmt.Fprintf(out, "declared as %s\n", found.Image)
	if len(found.Versions) == 0 {
		_, _ = fmt.Fprintf(out, "this machine holds no image for %s; push one before it can run\n", found.Service)
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "VERSION\tSIZE\tCREATED\tIN USE\tOTHER TAGS")
	for _, version := range found.Versions {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			version.Ref,
			formatBytes(version.Bytes),
			time.UnixMilli(int64(version.Created)).Format("2006-01-02 15:04"),
			inUseText(version),
			otherTags(version))
	}
	_ = w.Flush()

	if !slices.ContainsFunc(found.Versions, func(v api.ServiceVersion) bool { return v.Declared }) {
		// Not a rollback problem but the same operator's problem, found here
		// first: the file names something this machine does not have.
		_, _ = fmt.Fprintf(out, "nothing here matches %s, so a start would fail until it is pushed\n", found.Image)
	}
	printGoBack(out, found)
}

// printGoBack names one version and prints the edit that pins it. A concrete
// line is the point: what makes going back hard is not choosing, it is that the
// tag in the file already moved and re-declaring it does nothing.
func printGoBack(out io.Writer, found api.ServiceVersions) {
	previous, exists := previousTo(found.Versions)
	if !exists {
		_, _ = fmt.Fprintln(out, "there is no older version here to go back to")
		return
	}
	_, _ = fmt.Fprintf(out, "\nto go back, put this in %s%s, then push and apply:\n", found.Service, service.Extension)
	_, _ = fmt.Fprintf(out, "  (tuple \"image\" %q)\n", previous.Ref)
	if previous.Ref == previous.Digest {
		// Honest about what is being pinned: a digest is this machine's own id
		// for the bytes, so the same declaration on another machine names
		// nothing at all.
		_, _ = fmt.Fprintln(out, "that version carries no hostd stamp, so it is pinned by digest, which means nothing on another machine")
	}
}

// The version one step older than the declared one. Stepping from the
// declaration rather than from the top matters once a rollback has happened:
// the newest is then something the operator deliberately moved away from, and
// offering it as "back" would offer to undo the rollback.
func previousTo(versions []api.ServiceVersion) (api.ServiceVersion, bool) {
	at := slices.IndexFunc(versions, func(v api.ServiceVersion) bool { return v.Declared })
	if at < 0 {
		return versions[0], true
	}
	if at+1 >= len(versions) {
		return api.ServiceVersion{}, false
	}
	return versions[at+1], true
}

func inUseText(version api.ServiceVersion) string {
	var uses []string
	if version.Running {
		uses = append(uses, "running")
	}
	if version.Declared {
		uses = append(uses, "declared")
	}
	if len(uses) == 0 {
		return "-"
	}
	return strings.Join(uses, ", ")
}

// The moving tags, which are what an operator recognises the version by even
// though they are not what to declare. The stamp is already the first column.
func otherTags(version api.ServiceVersion) string {
	var rest []string
	for _, tag := range version.Tags {
		if tag == version.Ref {
			continue
		}
		rest = append(rest, tag)
	}
	if len(rest) == 0 {
		return "-"
	}
	return strings.Join(rest, ", ")
}
