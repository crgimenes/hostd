package main

import (
	"fmt"

	"github.com/crgimenes/hostd/daemon"
)

// What is wrong with a machine that is not wrong with a service on it: no
// daemon answering, or one older than the window carries. It shows in three
// places at once, on purpose (crg, 2026-08-26: "assim não tem como errar") —
// an amber dot beside the machine in the tree, a line in the log, and a button
// beside the machine's name that fixes it.
type alert struct {
	Text string
	Act  string
}

// alertFor reads a machine and answers what is wrong with the machine itself.
// The whole version test is an equality (crg, 2026-08-27): different means one
// of the two ends is the wrong one for the other, and which one is the
// operator's call — they may be putting an older daemon back on purpose.
func alertFor(host fleetHost) alert {
	carried := daemon.Version()
	if host.Error != "" {
		// ssh got there and nothing answered: that is what an install fixes,
		// and it is the only unreachable case where offering one is honest. A
		// machine that is switched off cannot be installed onto, and a button
		// that cannot work is worse than no button.
		if host.NoDaemon && carried != "" {
			return alert{
				Text: "no hostd answered here",
				Act:  "do/install/" + host.Host,
			}
		}
		return alert{Text: "not answering"}
	}
	if carried == "" || host.Version == "" || host.Version == carried {
		return alert{}
	}
	// Both versions named, and no claim about which is newer: the operator
	// decides what to do about a difference, and the button offers the one
	// thing this window can do about it.
	return alert{
		Text: fmt.Sprintf("hostd %s here, %s in this window", host.Version, carried),
		Act:  "do/install/" + host.Host,
	}
}
