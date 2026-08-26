package main

import (
	"context"
	"fmt"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/daemon"
	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/version"
)

// What a machine runs against what this client carries. States are codes, not
// prose: a program branches on them, and the sentence beside them may be
// rewritten.
const (
	stateCurrent = "current"
	stateBehind  = "behind"
	// The machine is on a version this client does not have. It is not the
	// machine that needs anything — this hostctl is the old one, and using it
	// to "upgrade" would take the fleet backwards.
	stateAhead   = "ahead"
	stateUnknown = "unknown"
)

// The answer is computed here rather than on the machine, so unlike every other
// command both machine-readable shapes are rendered from this value. -filo
// cannot pass the daemon's own bytes through, because the daemon was never
// asked the question: it only said what it runs.
type fleetVersion struct {
	Host string `filo:"host" json:"host"`
	Runs string `filo:"runs" json:"runs"`
	// Empty for a development build of hostctl, which carries no daemon and so
	// has nothing to compare against.
	Carries string `filo:"carries" json:"carries"`
	State   string `filo:"state" json:"state"`
	Advice  string `filo:"advice" json:"advice"`
}

// runFleetVersion answers "who is behind", which is the question `install`
// cannot answer because installing is what it does. It changes nothing.
func runFleetVersion(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) > 0 {
		return exitUsage, fmt.Errorf("version takes no arguments; name the machines with -host, -hosts, -tag or -all")
	}
	resp, err := client.Do(ctx, api.Request{Op: api.OpDescribe})
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var described api.Description
	err = decode(ctx, resp.Body, &described)
	if err != nil {
		return exitFailed, err
	}
	answer := compareVersions(client.Target(), described.Version, daemon.Version())

	body, err := filoconf.Marshal(answer)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, body, answer, func() {
		_, _ = fmt.Fprintln(opt.out, sentenceFor(answer))
	})
	// A report that says a machine is behind is a report that worked. Exiting
	// non-zero on it would make `-all version` read as a fleet half of which
	// failed to answer, which is a different and much more alarming thing. The
	// refusal that matters lives in install, where going backwards would
	// actually happen.
	return exitOK, nil
}

// compareVersions is pure so that the case nobody can stage on a bench — a
// machine ahead of the client — is a case a test covers.
func compareVersions(host, runs, carries string) fleetVersion {
	answer := fleetVersion{Host: host, Runs: runs, Carries: carries, State: stateUnknown}
	if carries == "" {
		answer.Advice = "this hostctl carries no daemon, so there is nothing to compare; a release build does"
		return answer
	}
	// Two identical strings are the same daemon, whether or not either is a
	// release version. Ranking is impossible for a development stamp; equality
	// is not, and answering "cannot tell" to "is this the same build?" would be
	// refusing to read what is in front of it.
	if runs == carries {
		answer.State = stateCurrent
		return answer
	}
	order, comparable := version.Compare(runs, carries)
	if !comparable {
		answer.Advice = fmt.Sprintf("%q and %q are not both release versions, so which is newer cannot be told", runs, carries)
		return answer
	}
	switch {
	case order == 0:
		answer.State = stateCurrent
	case order < 0:
		answer.State = stateBehind
		answer.Advice = fmt.Sprintf("hostctl -host %s install", host)
	default:
		answer.State = stateAhead
		answer.Advice = "this hostctl is the old one; update it before installing anything from it"
	}
	return answer
}

func sentenceFor(answer fleetVersion) string {
	switch answer.State {
	case stateCurrent:
		return fmt.Sprintf("runs %s, the version this hostctl carries", answer.Runs)
	case stateBehind:
		return fmt.Sprintf("runs %s, behind the %s this hostctl carries; upgrade it with %s",
			answer.Runs, answer.Carries, answer.Advice)
	case stateAhead:
		return fmt.Sprintf("runs %s, ahead of the %s this hostctl carries; %s",
			answer.Runs, answer.Carries, answer.Advice)
	}
	return fmt.Sprintf("runs %s; %s", answer.Runs, answer.Advice)
}
