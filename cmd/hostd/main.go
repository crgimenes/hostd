// Command hostd supervises the services declared on this machine.
//
// It is headless: everything a person or an agent does with it arrives over
// the control API, which hostctl speaks.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/config"
	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
	"github.com/crgimenes/hostd/version"
)

// Stamped with -X main.Version by whatever builds a release.
var Version string

func main() {
	version.Set(Version)
	showVersion := flag.Bool("version", false, "print the version and exit")
	debug := flag.Bool("debug", false, "write one key=value diagnostic line to stderr every 30s")
	stdio := flag.Bool("stdio", false, "serve one client over stdin and stdout, which is how ssh reaches this daemon")
	flag.Usage = func() {
		_, _ = fmt.Print(`hostd supervises the services declared on this machine.

usage:
  hostd [flags]

It has no interface of its own: operate it with hostctl.
Paths default to /etc/hostd, /var/lib/hostd and /run/hostd, and HOSTD_ROOT
redirects all of them.

flags:
`)
		flag.CommandLine.SetOutput(os.Stdout)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("hostd %s (protocol %d, schema %d)\n", version.Version, version.Protocol, version.Schema)
		return
	}

	if *stdio {
		// The daemon holding the state is the one already running; this
		// process only carries the conversation to it.
		err := api.Stdio(config.Locate().Socket(), os.Stdin, os.Stdout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hostd: %v\n", err)
			os.Exit(1)
		}
		return
	}

	err := run(*debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostd: %v\n", err)
		os.Exit(1)
	}
}

func run(debug bool) error {
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

	logStore, err := logs.Open(ctx, paths.LogDatabase(), logs.Options{
		Retention: cfg.LogRetention(),
		MaxRows:   cfg.LogMaxRows,
	})
	if err != nil {
		return err
	}
	defer func() { _ = logStore.Close() }()

	metricStore, err := metrics.Open(ctx, paths.MetricsDatabase(), metrics.Options{
		Retention: cfg.MetricsRetention(),
		MaxRows:   cfg.MetricsMaxRows,
	})
	if err != nil {
		return err
	}
	defer func() { _ = metricStore.Close() }()

	declared, loadErr := service.LoadDir(ctx, paths.ServicesDir())
	if loadErr != nil {
		// Broken files must not stop the machine: the readable services still
		// run and the problem is reported where an operator sees it.
		fmt.Fprintf(os.Stderr, "hostd: %v\n", loadErr)
		logStore.Append(logs.Record{
			Service: "hostd",
			Stream:  logs.StreamEvent,
			Kind:    logs.EventProblem,
			Text:    loadErr.Error(),
		})
	}

	store, err := state.Open(ctx, paths.StateDir())
	if err != nil {
		return err
	}

	sup := supervisor.New(supervisor.Dirs{
		State: paths.SupervisionDir(),
		Spool: paths.SpoolDir(),
	}, logStore)

	runtime, runtimeErr := docker.Open()
	if runtimeErr == nil {
		pingErr := runtime.Ping(ctx)
		if pingErr != nil {
			runtimeErr = pingErr
		}
	}
	if runtimeErr == nil {
		sup.Runtime(runtime)
	}

	adoptErr := sup.Adopt(ctx, declared)
	if adoptErr != nil {
		fmt.Fprintf(os.Stderr, "hostd: %v\n", adoptErr)
		logStore.Append(logs.Record{
			Service: "hostd",
			Stream:  logs.StreamEvent,
			Kind:    logs.EventProblem,
			Text:    adoptErr.Error(),
		})
	}

	go metrics.NewSampler(metricStore, func() []metrics.Process {
		// Asked at every tick, so a service that started a second ago is
		// sampled without anyone registering it anywhere.
		var live []metrics.Process
		for _, st := range sup.Status() {
			if st.PID != 0 {
				live = append(live, metrics.Process{Name: st.Name, PID: st.PID})
			}
		}
		return live
	}, func(err error) {
		logStore.Append(logs.Record{
			Service: "hostd",
			Stream:  logs.StreamEvent,
			Kind:    logs.EventProblem,
			Text:    err.Error(),
		})
	}).Run(ctx)

	listener, err := api.ListenUnix(paths.Socket())
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	if debug {
		go reportDiagnostics(ctx, sup, logStore)
	}

	server := api.NewServer(sup, store, logStore, metricStore, paths.ServicesDir())
	if runtimeErr == nil {
		server.Runtime(runtime)
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(ctx, listener) }()

	logStore.Append(logs.Record{
		Service: "hostd",
		Stream:  logs.StreamEvent,
		Kind:    logs.EventDaemon,
		Text:    fmt.Sprintf("hostd %s listening on %s", version.Version, paths.Socket()),
	})
	// Which capability this machine has is worth saying once, where the
	// operator reads it: a container service declared on a machine with no
	// runtime fails later, and this is the line that explains why.
	logStore.Append(logs.Record{
		Service: "hostd",
		Stream:  logs.StreamEvent,
		Kind:    logs.EventDaemon,
		Text:    runtimeLine(runtime, runtimeErr),
	})
	fmt.Fprintf(os.Stderr, "hostd %s listening on %s\n", version.Version, paths.Socket())
	// Reaching this machine from another is ssh running `hostd -stdio` here.
	// There is no port of ours on the network to defend.

	sup.Run(ctx)

	// Leaving does not stop the services: the next hostd adopts them.
	return <-serverErr
}

func runtimeLine(client *docker.Client, err error) string {
	if err != nil {
		return "no container runtime on this machine: " + err.Error()
	}
	return "container runtime at " + client.Socket()
}

const debugInterval = 30 * time.Second

// One key=value line an agent greps: percentiles rather than an average,
// because the tick that ran long is the one worth seeing.
func reportDiagnostics(ctx context.Context, sup *supervisor.Supervisor, logStore *logs.Store) {
	ticker := time.NewTicker(debugInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		tick := sup.Stats()
		stored := logStore.Stats()
		fmt.Fprintf(os.Stderr,
			"hostd: debug services=%d running=%d ticks=%d tick-p50-ms=%.2f tick-p95-ms=%.2f tick-max-ms=%.2f log-queued=%d log-dropped=%d\n",
			tick.Services, tick.Running, tick.Ticks,
			milliseconds(tick.TickP50), milliseconds(tick.TickP95), milliseconds(tick.TickMax),
			stored.Queued, stored.Dropped)
	}
}

func milliseconds(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
