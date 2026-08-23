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
	"os"
	"os/signal"
	"syscall"

	"github.com/crgimenes/hostd/internal/api"
	"github.com/crgimenes/hostd/internal/config"
)

// Exit codes are part of the interface. Messages are for people; these are for
// programs, and they do not change.
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitComms   = 3
	exitAuth    = 4
	exitPartial = 5
)

func main() {
	os.Exit(run(os.Args[1:]))
}

type options struct {
	socket   string
	filoOut  bool
	limit    int
	stream   string
	service  string
	follow   bool
	sinceSeq uint64
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
	flags.BoolVar(&opt.follow, "follow", false, "keep watching for new lines")
	flags.Uint64Var(&opt.sinceSeq, "since", 0, "only lines after this sequence")
	flags.Usage = func() { usage(flags) }

	rest, err := parseAnywhere(flags, args)
	if err != nil {
		return exitUsage
	}
	if len(rest) == 0 {
		usage(flags)
		return exitUsage
	}
	if opt.socket == "" {
		opt.socket = config.Locate().Socket()
	}

	// A signal cancels the work in progress rather than announcing itself and
	// carrying on.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	code, err := dispatch(ctx, opt, rest)
	if err != nil {
		// Diagnostics go to stderr, so a caller reading stdout gets the result
		// and nothing else.
		fmt.Fprintf(os.Stderr, "hostctl: %v\n", err)
	}
	return code
}

// parseAnywhere accepts flags before, between and after the command words, and
// returns the words.
//
// The flag package stops parsing at the first argument that is not a flag, so
// `hostctl log --follow` would leave --follow as a positional and it would be
// read as a search pattern: the command would quietly do the wrong thing
// instead of failing. Silence is the worst outcome available here, so the
// order is not something an operator has to remember.
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

func usage(flags *flag.FlagSet) {
	fmt.Fprint(os.Stderr, `hostctl operates hostd.

usage:
  hostctl status                       what every service is doing
  hostctl describe                     versions and capabilities of the daemon
  hostctl service list                 same as status, by service
  hostctl service start <name>         ask a service to run
  hostctl service stop <name>          ask a service to stop
  hostctl service restart <name>       stop and start a service
  hostctl apply                        re-read the services directory and converge
  hostctl log [pattern]                what the services wrote
  hostctl log --follow                 keep watching

flags:
`)
	flags.PrintDefaults()
}

func dispatch(ctx context.Context, opt options, args []string) (int, error) {
	client, err := api.DialUnix(opt.socket)
	if err != nil {
		return exitComms, err
	}
	defer func() { _ = client.Close() }()

	switch args[0] {
	case "status":
		return runStatus(ctx, client, opt)
	case "describe":
		return runDescribe(ctx, client, opt)
	case "service":
		return runService(ctx, client, opt, args[1:])
	case "apply":
		return runApply(ctx, client, opt)
	case "log", "logs":
		return runLog(ctx, client, opt, args[1:])
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
	default:
		return exitFailed
	}
}
