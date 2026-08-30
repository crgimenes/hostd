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
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/config"
	"github.com/crgimenes/hostd/daemon"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/version"
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

// Stamped with -X main.Version by whatever builds a release.
var Version string

func main() {
	// The window has to be created on the thread the process started on: the
	// macOS UI toolkit refuses anything else, and by the time the panel is
	// asked for, this goroutine may have been moved. Locking here costs a
	// pinned thread for every command and buys the one that opens a window.
	runtime.LockOSThread()
	version.Set(Version)
	os.Exit(run(os.Args[1:]))
}

type options struct {
	socket     string
	filoOut    bool
	jsonOut    bool
	limit      int
	keep       int
	outDir     string
	stream     string
	kind       string
	run        string
	service    string
	follow     bool
	sinceSeq   uint64
	expectGen  uint64
	allowDestr bool
	onBehalfOf string
	dryRun     bool
	debug      bool
	host       string
	hosts      []string
	all        bool
	tag        string
	config     string
	inventory  string
	remote     string
	scope      string
	metric     string
	// Where the answer goes. Stdout normally; a buffer per host when several
	// machines are asked at once, so two answers never interleave.
	out    io.Writer
	window time.Duration
	step   time.Duration
}

func run(args []string) int {
	opt := options{out: os.Stdout}
	flags := flag.NewFlagSet("hostctl", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&opt.host, "host", "", "machine to operate; without it, the hostd on this machine")
	hosts := flags.String("hosts", "", "machines to operate, separated by commas")
	flags.BoolVar(&opt.all, "all", false, "every machine listed in the inventory")
	flags.StringVar(&opt.tag, "tag", "", "every machine carrying this tag in the inventory")
	flags.StringVar(&opt.config, "config", "", "directory of declarations to send (default the hostd directory of your config)")
	flags.StringVar(&opt.inventory, "inventory", "", "file listing the fleet (default inventory.filo inside -config)")
	flags.StringVar(&opt.remote, "remote-command", "hostd -stdio", "what ssh runs on the machine to reach its daemon")
	flags.StringVar(&opt.socket, "socket", "", "path to the hostd control socket")
	flags.BoolVar(&opt.filoOut, "filo", false, "write the result as Filo and nothing else")
	flags.BoolVar(&opt.jsonOut, "json", false, "write the result as JSON and nothing else")
	flags.IntVar(&opt.limit, "limit", 200, "maximum number of log lines")
	flags.IntVar(&opt.keep, "keep", api.DefaultImageKeep, "versions of each still-declared image a prune leaves behind")
	flags.StringVar(&opt.outDir, "out", ".", "directory a backup is saved into")
	flags.StringVar(&opt.stream, "stream", "", "only stdout, stderr or event")
	flags.StringVar(&opt.service, "service", "", "only this service")
	flags.StringVar(&opt.kind, "kind", "", "only events of this kind, e.g. service.exited")
	flags.StringVar(&opt.run, "run", "", "only this run of a job")
	flags.BoolVar(&opt.follow, "follow", false, "keep watching for new lines")
	flags.Uint64Var(&opt.sinceSeq, "since", 0, "only lines after this sequence")
	flags.Uint64Var(&opt.expectGen, "expect-generation", 0, "refuse if the host has moved past this generation")
	flags.BoolVar(&opt.allowDestr, "allow-destructive", false, "authorise changes that take a running service away")
	flags.StringVar(&opt.onBehalfOf, "on-behalf-of", "", "identity this command is being run for, recorded in the audit log")
	flags.BoolVar(&opt.dryRun, "dry-run", false, "show what apply would do, and do nothing")
	flags.BoolVar(&opt.debug, "debug", false, "write one key=value diagnostic line per request to stderr")
	showVersion := flags.Bool("version", false, "print the version and exit")
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
	if *showVersion {
		fmt.Printf("hostctl %s (protocol %d, schema %d)\n", version.Version, version.Protocol, version.Schema)
		// Whether this build can put a daemon on a machine by itself. A
		// development build cannot, and finding that out here beats finding
		// it out mid-install.
		carried := daemon.Carried()
		if len(carried) == 0 {
			fmt.Println("carries no hostd (development build; make dist embeds it)")
			return exitOK
		}
		fmt.Printf("carries hostd %s for linux/%s\n", daemon.Version(), strings.Join(carried, ", linux/"))
		stale := staleDaemon()
		if stale != "" {
			fmt.Println(stale)
		}
		return exitOK
	}
	// No command is the command an operator gives most: watching. Asking for
	// help still prints help, and a machine without a window says so with the
	// reason rather than a usage screen.
	if len(rest) == 0 {
		rest = []string{"gui"}
	}
	if opt.socket == "" {
		opt.socket = config.Locate().Socket()
	}
	err = opt.resolveClientPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostctl: %v\n", err)
		return exitUsage
	}
	if *hosts != "" {
		opt.hosts = strings.Split(*hosts, ",")
	}
	// Two answers to "what shape is the output" is a question with no answer.
	// Guessing which one wins is the kind of thing a script inherits and nobody
	// notices until the parser on the other end changes.
	if opt.filoOut && opt.jsonOut {
		fmt.Fprintln(os.Stderr, "hostctl: choose -filo or -json, not both")
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The panel watches several machines at once and keeps its own connection
	// to each, so it is not something to fan out: one window, many hosts.
	if rest[0] == "gui" {
		code, guiErr := runGUI(ctx, opt, rest[1:])
		if guiErr != nil {
			fmt.Fprintf(os.Stderr, "hostctl: %v\n", guiErr)
		}
		return code
	}

	// Generating a Caddyfile reads the tree and nothing else: no machine has to
	// be reachable, and -host only says whose proxy this is, so it must not go
	// through the fan-out that would ask every machine the same question.
	if rest[0] == "caddyfile" {
		code, caddyErr := runCaddyfile(ctx, opt, rest[1:])
		if caddyErr != nil {
			fmt.Fprintf(os.Stderr, "hostctl: %v\n", caddyErr)
		}
		return code
	}

	chosen, err := opt.selection(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostctl: %v\n", err)
		return exitUsage
	}
	if len(chosen) > 1 {
		if opt.follow {
			fmt.Fprintln(os.Stderr, "hostctl: -follow watches one machine at a time; name it with -host")
			return exitUsage
		}
		// Optimistic control is per machine: a generation from one host means
		// nothing on another, and claiming it on all of them would refuse
		// every machine but one.
		if opt.expectGen != 0 {
			fmt.Fprintln(os.Stderr, "hostctl: -expect-generation applies to one machine; name it with -host")
			return exitUsage
		}
		return fanOut(ctx, opt, chosen, rest)
	}
	if len(chosen) == 1 {
		opt.host = chosen[0]
	}

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
  hostctl                              watch the fleet in a window
  hostctl -host <machine> status       operate a machine over the network
  hostctl -all status                  ask every machine at once
  hostctl status                       what every service is doing
  hostctl describe                     versions and capabilities of the daemon
  hostctl -all version                 which machines are not on the daemon this
                                       hostctl carries
  hostctl service deploy <name>        put a service from the tree on that
                                       machine — declaration, image and a fresh
                                       container; deploying again overwrites,
                                       which is how a new version goes live
  hostctl service remove <name>        take it off that machine — container,
                                       declaration and image; the tree still
                                       describes it, and a deploy puts it back
  hostctl service backup <name>        run the service's own backup_data.sh on
                                       the machine and save the file it makes
                                       here (-out says where); everything the
                                       script says lands in the machine's log
  hostctl service list                 same as status, by service
  hostctl service versions <name>      which versions it could be put back on
  hostctl job run <name>               run a job now, without waiting for it
  hostctl file ls <name> [vol/path]    the files in a service's data volumes
  hostctl file get <name> <vol/path>   download one of them (-out says where)
  hostctl file put <name> <local> <vol/path>
                                       place a file in a service's data
  hostctl file rm <name> <vol/path>    delete one file of it, by name
  hostctl service start <name>         ask a service to run
  hostctl service stop <name>          ask a service to stop
  hostctl service restart <name>       stop and start a service
  hostctl service redeploy <name>      a fresh container from the declaration,
                                       resolving its image tag again
  hostctl plan                         what an apply would do, without doing it
  hostctl apply                        re-read the services directory and converge
  hostctl audit                        who changed what, and when
  hostctl log [pattern]                what the services wrote
  hostctl log -follow                  keep watching
  hostctl caddyfile                    write a Caddyfile from what the tree
                                       declares, to stdout
  hostctl push                         refresh the declarations of the services
                                       that machine already runs, from the tree
  hostctl install                      put this hostctl's own hostd on it
  hostctl image ls                     the images that machine holds
  hostctl image prune                  what an image cleanup would remove
  hostctl image push <image>           send an image built here to that machine
                                       (sending is not deploying: a service runs
                                       what its declaration names)
  hostctl metrics                      what the host and its services are using
  hostctl metrics -window 1h           the same, over a window

flags:
`)
	flags.PrintDefaults()
	_, _ = fmt.Fprint(w, `
output:
  the requested result goes to stdout, diagnostics to stderr.
  -filo makes stdout a Filo expression and nothing else, and -json the same
  answer as JSON. Filo is what the daemon speaks; JSON is rendered here, from
  the same values, for whatever reads it more easily.

exit status:
  0 success   1 failed   2 bad arguments   3 no connection
  4 not authorised   5 partial success   6 refused, nothing changed

the network:
  -host names one machine, -hosts a few, -all every machine in the inventory,
  and -tag those carrying a tag there. Several machines are asked at once and
  each answer is printed whole under its host; exit 5 means some answered and
  some did not.

  Reaching a machine is ssh running "hostd -stdio" on it, so authentication,
  host identity and the record of the attempt are sshd's, and no port of ours
  is on the network. Membership of the hostd group on that machine is the
  permission to operate it, and the audit records the account that did.

example:
  hostctl -host yuki.local status
  hostctl -all metrics
  hostctl log -service api -follow
  hostctl metrics -service api -metric cpu-percent -window 30m
`)
}

// The operator's own files, which live on the operator's machine: hostctl runs
// where the person is, never on the host it operates.
func (o *options) resolveClientPaths() error {
	if o.config == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		o.config = filepath.Join(dir, "hostd")
	}
	if o.inventory == "" {
		o.inventory = filepath.Join(o.config, service.InventoryFile)
	}
	return nil
}

// Reaching another machine is running the daemon's stdio mode there over ssh.
// Authentication, host identity and the record of the attempt are sshd's, and
// none of it is written again here.
func connect(ctx context.Context, opt options) (*api.Client, error) {
	if opt.host == "" {
		return api.DialUnix(opt.socket)
	}
	// A machine that does not answer is a machine the operator will look at:
	// there is no retry and no reconnection here, because a client that keeps
	// trying hides which machine went away.
	return api.DialSSH(ctx, opt.host, strings.Fields(opt.remote))
}

func dispatch(ctx context.Context, opt options, args []string) (int, error) {
	// Installing is what makes a machine answerable, so it cannot begin by
	// reaching the daemon. It sits inside dispatch rather than beside it so
	// -all and -tag fan it out like any other command.
	if args[0] == "install" {
		return runInstall(ctx, opt, args[1:])
	}

	client, err := connect(ctx, opt)
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
	case "version":
		return runFleetVersion(ctx, client, opt, args[1:])
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
	case "image":
		return runImage(ctx, client, opt, args[1:])
	case "job":
		return runJob(ctx, client, opt, args[1:])
	case "file":
		return runFile(ctx, client, opt, args[1:])
	case "push":
		return runPush(ctx, client, opt, args[1:])
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
	case api.CodeConflict, api.CodeDestructive, api.CodeRefused:
		// Nothing was changed. The caller is meant to look and decide, which
		// is a different outcome from an operation that tried and failed.
		return exitRefused
	default:
		return exitFailed
	}
}
