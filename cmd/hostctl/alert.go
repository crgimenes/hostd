package main

import (
	"fmt"

	"github.com/crgimenes/hostd/daemon"
	"github.com/crgimenes/hostd/version"
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
// A daemon that cannot be ranked against this window's — a development build,
// a release candidate — is NOT called behind: guessing an order is how a fleet
// walks backwards in silence, and the version command already refuses to.
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
	// Order when the two can be ranked, and no claim of order when they
	// cannot: a development build carries a stamp nothing can sort, and
	// guessing is how a fleet walks backwards in silence. What is left is
	// still worth saying — the two differ — so the alert NAMES BOTH and lets
	// the operator decide, instead of asserting which is newer.
	order, comparable := version.Compare(host.Version, carried)
	if comparable && order >= 0 {
		return alert{}
	}
	return alert{
		Text: fmt.Sprintf("hostd %s here, %s in this window", host.Version, carried),
		Act:  "do/install/" + host.Host,
	}
}
