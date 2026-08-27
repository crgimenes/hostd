package main

import (
	"context"
	"fmt"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/daemon"
	"github.com/crgimenes/hostd/filoconf"
)

// What a machine runs against what this client carries. States are codes, not
// prose: a program branches on them, and the sentence beside them may be
// rewritten.
//
// There is no "behind" and no "ahead" (crg, 2026-08-27). Neither end needs to
// know which of two versions is newer — different means one of the two is the
// wrong one for the other, and which one is the operator's call. That buys
// freedom with the protocol while it is being worked on, and it lets somebody
// deliberately put an older daemon back: a machine that just went amber may be
// amber BECAUSE of the newest version.
const (
	stateCurrent = "current"
	stateDiffers = "differs"
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

// runFleetVersion answers "which machines are not on the daemon I carry", which
// is the question `install` cannot answer because installing is what it does.
// It changes nothing.
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
	// A report that says a machine differs is a report that worked. Exiting
	// non-zero on it would make `-all version` read as a fleet half of which
	// failed to answer, which is a different and much more alarming thing.
	return exitOK, nil
}

// compareVersions is pure, and the whole comparison is one equality: two
// identical strings are the same daemon, whether or not either is a release
// version, and anything else is a difference somebody has to look at.
func compareVersions(host, runs, carries string) fleetVersion {
	answer := fleetVersion{Host: host, Runs: runs, Carries: carries, State: stateUnknown}
	if carries == "" {
		answer.Advice = "this hostctl carries no daemon, so there is nothing to compare; a release build does"
		return answer
	}
	if runs == carries {
		answer.State = stateCurrent
		return answer
	}
	answer.State = stateDiffers
	answer.Advice = fmt.Sprintf("hostctl -host %s install", host)
	return answer
}

func sentenceFor(answer fleetVersion) string {
	switch answer.State {
	case stateCurrent:
		return fmt.Sprintf("runs %s, the version this hostctl carries", answer.Runs)
	case stateDiffers:
		return fmt.Sprintf("runs %s, not the %s this hostctl carries; put this one on it with %s",
			answer.Runs, answer.Carries, answer.Advice)
	}
	return fmt.Sprintf("runs %s; %s", answer.Runs, answer.Advice)
}
