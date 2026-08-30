package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/crgimenes/hostd/api"
)

// The file commands move a service's DATA — what lives in its named volumes.
// Configuration is not here on purpose: it comes from the operator's tree via
// deploy, and a hand edit on the machine would be a change nobody committed.
func runFile(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) == 0 {
		return exitUsage, fmt.Errorf("file needs a subcommand: ls, get or put")
	}
	switch args[0] {
	case "ls", "list":
		if len(args) < 2 {
			return exitUsage, fmt.Errorf("file ls needs a service, and optionally volume/path inside it")
		}
		wire := ""
		if len(args) > 2 {
			wire = args[2]
		}
		return runFileList(ctx, client, opt, args[1], wire)
	case "get":
		if len(args) < 3 {
			return exitUsage, fmt.Errorf("file get needs a service and volume/path/to/file")
		}
		return runFileGet(ctx, client, opt, args[1], args[2])
	case "put":
		if len(args) < 4 {
			return exitUsage, fmt.Errorf("file put needs a service, a local file, and volume/path/to/write")
		}
		return runFilePut(ctx, client, opt, args[1], args[2], args[3])
	default:
		return exitUsage, fmt.Errorf("file %s does not exist; ls, get and put do", args[0])
	}
}

func runFileList(ctx context.Context, client *api.Client, opt options, svc, wire string) (int, error) {
	resp, err := client.Do(ctx, api.Request{Op: api.OpFileList, Service: svc, Name: wire})
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var entries []api.FileEntry
	err = decode(ctx, resp.Body, &entries)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, resp.Body, entries, func() {
		if len(entries) == 0 {
			_, _ = fmt.Fprintln(opt.out, "nothing here")
			return
		}
		w := tabwriter.NewWriter(opt.out, 0, 0, 2, ' ', 0)
		for _, entry := range entries {
			if entry.Dir {
				_, _ = fmt.Fprintf(w, "%s/\t\t\n", entry.Name)
				continue
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", entry.Name, formatBytes(entry.Bytes),
				time.UnixMilli(int64(entry.MS)).Format("2006-01-02 15:04"))
		}
		_ = w.Flush()
	})
	return exitOK, nil
}

func runFileGet(ctx context.Context, client *api.Client, opt options, svc, wire string) (int, error) {
	var saved *os.File
	var into string
	resp, err := client.FetchFile(ctx, svc, wire, func(told api.FileTransfer) (io.Writer, error) {
		into = filepath.Join(opt.outDir, filepath.Base(told.Path))
		file, createErr := os.Create(into) // #nosec G304 -- the operator's own -out directory
		if createErr != nil {
			return nil, createErr
		}
		saved = file
		return file, nil
	})
	if saved != nil {
		closeErr := saved.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}
	if err != nil {
		// Half a file under the real name reads later as the whole one.
		if into != "" {
			_ = os.Remove(into)
		}
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var told api.FileTransfer
	err = decode(ctx, resp.Body, &told)
	if err != nil {
		return exitFailed, err
	}
	_, _ = fmt.Fprintf(opt.out, "%s (%s)\n", into, formatBytes(told.Bytes))
	return exitOK, nil
}

func runFilePut(ctx context.Context, client *api.Client, opt options, svc, local, wire string) (int, error) {
	file, err := os.Open(local) // #nosec G304 -- the operator's own file, named on their command line
	if err != nil {
		return exitFailed, err
	}
	defer func() { _ = file.Close() }()
	resp, err := client.SendFile(ctx, svc, wire, file)
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var told api.FileTransfer
	err = decode(ctx, resp.Body, &told)
	if err != nil {
		return exitFailed, err
	}
	_, _ = fmt.Fprintf(opt.out, "%s: %s now holds %s (%s)\n", client.Target(), svc, told.Path, formatBytes(told.Bytes))
	return exitOK, nil
}
