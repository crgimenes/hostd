// Command hostctl operates hostd.
//
// It is the single place a person or a program reaches the fleet from: the CLI
// and, later, the graphical mode are two presentations of the same operations.
// Everything works without a TTY.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/config"
)

// Part of the interface: messages are for people, these are for programs.
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitComms   = 3
	exitAuth    = 4
	exitPartial = 5
	// Nothing changed and the caller must look before retrying. An agent that
	// cannot tell this from a failure either retries what it should not, or
	// gives up on what it only had to re-read.
	exitRefused = 6
)

func main() {
	os.Exit(run(os.Args[1:]))
}

type options struct {
	socket     string
	filoOut    bool
	limit      int
	stream     string
	kind       string
	service    string
	follow     bool
	sinceSeq   uint64
	expectGen  uint64
	allowDestr bool
	onBehalfOf string
	dryRun     bool
	debug      bool
	scope      string
	metric     string
	window     time.Duration
	step       time.Duration
}

func run(args []string) int {
	var opt options
	flags := flag.NewFlagSet("hostctl", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&opt.socket, "socket", "", "path to the hostd control socket")
	flags.BoolVar(&opt.filoOut, "filo", false, "write the result as Filo and nothing else")
	flags.IntVar(&opt.limit, "limit", 200, "maximum number of log lines")
	flags.StringVar(&opt.stream, "stream", "", "only stdout, stderr or event")
	flags.StringVar(&opt.service, "service", "", "only this service")
	flags.StringVar(&opt.kind, "kind", "", "only events of this kind, e.g. service.exited")
	flags.BoolVar(&opt.follow, "follow", false, "keep watching for new lines")
	flags.Uint64Var(&opt.sinceSeq, "since", 0, "only lines after this sequence")
	flags.Uint64Var(&opt.expectGen, "expect-generation", 0, "refuse if the host has moved past this generation")
	flags.BoolVar(&opt.allowDestr, "allow-destructive", false, "authorise changes that take a running service away")
	flags.StringVar(&opt.onBehalfOf, "on-behalf-of", "", "identity this command is being run for, recorded in the audit log")
	flags.BoolVar(&opt.dryRun, "dry-run", false, "show what apply would do, and do nothing")
	flags.BoolVar(&opt.debug, "debug", false, "write one key=value diagnostic line per request to stderr")
	flags.StringVar(&opt.scope, "scope", "", "metrics of the host or of the services")
	flags.StringVar(&opt.metric, "metric", "", "only this metric, e.g. cpu-percent")
	flags.DurationVar(&opt.window, "window", 0, "how far back metrics reach; without it, the latest values")
	flags.DurationVar(&opt.step, "step", 0, "seconds per metric point; without it, the finest the window still has")
	// Asking for help is legitimate use, so it goes to stdout and succeeds.
	flags.SetOutput(os.Stdout)
	flags.Usage = func() { usage(flags, os.Stdout) }

	rest, err := parseAnywhere(flags, args)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		usage(flags, os.Stderr)
		return exitUsage
	}
	if len(rest) == 0 {
		usage(flags, os.Stderr)
		return exitUsage
	}
	if opt.socket == "" {
		opt.socket = config.Locate().Socket()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	code, err := dispatch(ctx, opt, rest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostctl: %v\n", err)
	}
	return code
}

// The flag package stops at the first positional, which would leave
// `hostctl log -follow` reading -follow as a search pattern: the command
// would quietly do the wrong thing instead of failing.
func parseAnywhere(flags *flag.FlagSet, args []string) ([]string, error) {
	var words []string
	for {
		err := flags.Parse(args)
		if err != nil {
			return nil, err
		}
		args = flags.Args()
		if len(args) == 0 {
			return words, nil
		}
		words = append(words, args[0])
		args = args[1:]
	}
}

func usage(flags *flag.FlagSet, w io.Writer) {
	flags.SetOutput(w)
	_, _ = fmt.Fprint(w, `hostctl operates hostd.

usage:
  hostctl status                       what every service is doing
  hostctl describe                     versions and capabilities of the daemon
  hostctl service list                 same as status, by service
  hostctl service start <name>         ask a service to run
  hostctl service stop <name>          ask a service to stop
  hostctl service restart <name>       stop and start a service
  hostctl plan                         what an apply would do, without doing it
  hostctl apply                        re-read the services directory and converge
  hostctl audit                        who changed what, and when
  hostctl log [pattern]                what the services wrote
  hostctl log -follow                  keep watching
  hostctl metrics                      what the host and its services are using
  hostctl metrics -window 1h           the same, over a window

flags:
`)
	flags.PrintDefaults()
	_, _ = fmt.Fprint(w, `
output:
  the requested result goes to stdout, diagnostics to stderr.
  -filo makes stdout a Filo expression and nothing else.

exit status:
  0 success   1 failed   2 bad arguments   3 no connection
  4 not authorised   5 partial success   6 refused, nothing changed

example:
  hostctl log -service api -follow
  hostctl metrics -service api -metric cpu-percent -window 30m
`)
}

func dispatch(ctx context.Context, opt options, args []string) (int, error) {
	client, err := api.DialUnix(opt.socket)
	if err != nil {
		return exitComms, err
	}
	defer func() { _ = client.Close() }()
	if opt.debug {
		client.Debug = os.Stderr
	}

	switch args[0] {
	case "status":
		return runStatus(ctx, client, opt)
	case "describe":
		return runDescribe(ctx, client, opt)
	case "service":
		return runService(ctx, client, opt, args[1:])
	case "plan":
		return runPlan(ctx, client, opt)
	case "apply":
		return runApply(ctx, client, opt)
	case "audit":
		return runAudit(ctx, client, opt)
	case "log", "logs":
		return runLog(ctx, client, opt, args[1:])
	case "metrics":
		return runMetrics(ctx, client, opt)
	default:
		return exitUsage, fmt.Errorf("unknown command %q; run hostctl with no arguments to see what exists", args[0])
	}
}

// codeFor maps a daemon error onto the exit status a program reads. The
// mapping goes through the stable code, never through the message.
func codeFor(err error) int {
	var apiErr api.Error
	if !errors.As(err, &apiErr) {
		return exitFailed
	}
	switch apiErr.Code {
	case api.CodeInvalid, api.CodeUnknownOp:
		return exitUsage
	case api.CodeUnavailable:
		return exitComms
	case api.CodeConflict, api.CodeDestructive:
		// Nothing was changed. The caller is meant to look and decide, which
		// is a different outcome from an operation that tried and failed.
		return exitRefused
	default:
		return exitFailed
	}
}
