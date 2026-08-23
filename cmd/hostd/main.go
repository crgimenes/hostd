// Command hostd supervises the services declared on this machine.
//
// It is headless: it has no interface of its own. Everything a person or an
// agent does with it arrives over the control API, which hostctl speaks.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/crgimenes/hostd/internal/api"
	"github.com/crgimenes/hostd/internal/config"
	"github.com/crgimenes/hostd/internal/logs"
	"github.com/crgimenes/hostd/internal/service"
	"github.com/crgimenes/hostd/internal/state"
	"github.com/crgimenes/hostd/internal/supervisor"
	"github.com/crgimenes/hostd/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("hostd %s (protocol %d, schema %d)\n", version.Version, version.Protocol, version.Schema)
		return
	}

	err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// A signal cancels the context, and cancelling is what actually stops the
	// work. A handler that only prints a message is a handler that cancels
	// nothing.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	paths := config.Locate()
	err := paths.EnsureDirs()
	if err != nil {
		return fmt.Errorf("prepare directories: %w", err)
	}
	cfg, err := config.Load(ctx, paths.ConfigFile())
	if err != nil {
		return err
	}

	buffer := logs.NewBuffer(cfg.LogBuffer)
	declared, loadErr := service.LoadDir(ctx, paths.ServicesDir())
	if loadErr != nil {
		// Broken files must not stop the machine: the services that are
		// readable still run, and the problem is reported where an operator
		// will see it.
		fmt.Fprintf(os.Stderr, "hostd: %v\n", loadErr)
		buffer.Append(logs.Record{Service: "hostd", Stream: logs.StreamEvent, Text: loadErr.Error()})
	}

	store, err := state.Open(ctx, paths.StateDir())
	if err != nil {
		return err
	}

	sup := supervisor.New(supervisor.Dirs{
		State: paths.SupervisionDir(),
		Spool: paths.SpoolDir(),
	}, buffer)

	// Adoption comes before anything else: the processes a previous hostd left
	// running are found and taken back over, rather than duplicated.
	adoptErr := sup.Adopt(ctx, declared)
	if adoptErr != nil {
		fmt.Fprintf(os.Stderr, "hostd: %v\n", adoptErr)
		buffer.Append(logs.Record{Service: "hostd", Stream: logs.StreamEvent, Text: adoptErr.Error()})
	}

	listener, err := api.ListenUnix(paths.Socket())
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	server := api.NewServer(sup, store, buffer, paths.ServicesDir())
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(ctx, listener) }()

	buffer.Append(logs.Record{
		Service: "hostd",
		Stream:  logs.StreamEvent,
		Text:    fmt.Sprintf("hostd %s listening on %s", version.Version, paths.Socket()),
	})
	fmt.Fprintf(os.Stderr, "hostd %s listening on %s\n", version.Version, paths.Socket())

	sup.Run(ctx)

	// Leaving does not stop the services: they keep running and the next hostd
	// adopts them. That is what makes updating the daemon safe.
	return <-serverErr
}
