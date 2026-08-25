package main

import (
	"context"
	"fmt"
	"time"

	"github.com/crgimenes/hostd/supervisor"
)

// How long the machine is given to be itself again after the daemon comes back.
// The socket is already open by then — what this waits for is the drift round
// that re-adopts the containers, which is the thing being checked.
const adoptionSettle = 20 * time.Second

const adoptionPoll = time.Second

// The promise this whole project is built on: replacing hostd does not stop or
// restart the containers it supervises. What keeps it is that hostd never owned
// them — the runtime does, and the label plus the log timestamps are how the
// next daemon finds its way back to them.
//
// Nothing checked it. An install confirmed the version that came up and
// reported success, so a daemon that came back having lost the fleet's work
// would have been reported as a good upgrade. This is that check, and it is
// worth more than any setting in the unit file: it measures the outcome
// instead of trusting a mechanism to still be the mechanism.
func servicesSurvived(ctx context.Context, opt options, host string, before []supervisor.Status) []string {
	if len(runningIn(before)) == 0 {
		return nil
	}
	deadline := time.Now().Add(adoptionSettle)
	var complaints []string
	for {
		client, err := connectTo(ctx, opt, host)
		if err == nil {
			after, statusErr := fetchStatuses(ctx, client)
			_ = client.Close()
			if statusErr == nil {
				complaints = lostServices(before, after)
				// The fast path is the normal one: a machine that kept its
				// work says so on the first ask.
				if len(complaints) == 0 {
					return nil
				}
			} else {
				complaints = []string{fmt.Sprintf("could not read what is running: %v", statusErr)}
			}
		} else {
			complaints = []string{fmt.Sprintf("could not be reached to check: %v", err)}
		}
		if time.Now().After(deadline) {
			return complaints
		}
		select {
		case <-ctx.Done():
			return complaints
		case <-time.After(adoptionPoll):
		}
	}
}

// lostServices compares what was running before the daemon was replaced with
// what is running after, and says what the upgrade cost.
//
// Both halves matter and they are different news. A service that is gone means
// the machine is doing less work than it was. A service that came back under a
// new PID means the container was replaced — which is the same promise broken,
// just less visibly: the process that was serving requests is not the one
// serving them now, and anything it held in memory went with it.
//
// Only services that were RUNNING with a PID are compared. A job between runs
// has neither, and demanding it be found running would fail every machine with
// a scheduled job on it.
func lostServices(before, after []supervisor.Status) []string {
	now := make(map[string]supervisor.Status, len(after))
	for _, status := range after {
		now[status.Name] = status
	}
	var complaints []string
	for _, was := range runningIn(before) {
		is, found := now[was.Name]
		switch {
		case !found:
			complaints = append(complaints,
				fmt.Sprintf("%s was running as pid %d and the machine no longer declares it", was.Name, was.PID))
		case is.State != supervisor.StateRunning:
			complaints = append(complaints,
				fmt.Sprintf("%s was running as pid %d and is now %s", was.Name, was.PID, is.State))
		case is.PID != was.PID:
			complaints = append(complaints,
				fmt.Sprintf("%s was running as pid %d and is now pid %d, so its container was replaced", was.Name, was.PID, is.PID))
		}
	}
	return complaints
}

// What was actually running, which is the only thing there is a promise about.
func runningIn(statuses []supervisor.Status) []supervisor.Status {
	var out []supervisor.Status
	for _, status := range statuses {
		if status.State == supervisor.StateRunning && status.PID != 0 {
			out = append(out, status)
		}
	}
	return out
}
