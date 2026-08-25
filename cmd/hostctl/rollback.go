package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/crgimenes/hostd/supervisor"
)

// goBack puts the daemon that was on this machine back, after an upgrade that
// did not keep its promise.
//
// What it does and does not do is worth being exact about, because the two are
// easy to confuse. Restoring the binary STOPS THE BLEEDING: the machine runs
// the one daemon known to have worked here. It does NOT bring back a container
// that was lost — nothing brings a stopped container back except the drift
// round recreating it from the declaration, and any daemon does that. So this
// reports the two separately: what it restored, and whether the work returned.
func goBack(ctx context.Context, opt options, host string, wasRunning []supervisor.Status) rollback {
	out, err := remoteOutputErr(ctx, host, "sh -s", rollbackScript)
	if err != nil {
		return rollback{problem: err.Error()}
	}
	restored := rollback{version: strings.TrimSpace(out)}
	if restored.version == "" {
		restored.problem = "the machine did not say which daemon came back"
		return restored
	}
	// Asked again, of the daemon now running: the question was never "is the
	// binary back" but "is the machine doing its work".
	restored.stillLost = servicesSurvived(ctx, opt, host, wasRunning)
	return restored
}

// What a rollback achieved, in the two parts an operator has to tell apart.
type rollback struct {
	version string
	problem string
	// Empty when the work came back, which the drift round does on its own once
	// a working daemon is reading the declarations again.
	stillLost []string
}

func (r rollback) report(out io.Writer, host string) {
	switch {
	case r.problem != "":
		_, _ = fmt.Fprintf(out, "%s: could not go back to the previous daemon: %s\n", host, r.problem)
		return
	case len(r.stillLost) > 0:
		_, _ = fmt.Fprintf(out, "%s: went back to %s, and the work has not returned yet:\n", host, r.version)
		for _, complaint := range r.stillLost {
			_, _ = fmt.Fprintf(out, "%s:   %s\n", host, complaint)
		}
		// Said plainly because it is the difference between waiting and acting,
		// and an operator staring at a stopped service deserves to know which.
		_, _ = fmt.Fprintf(out, "%s: the drift round recreates what a declaration still names; if it does not, the declaration or the image is what to look at\n", host)
		return
	}
	_, _ = fmt.Fprintf(out, "%s: went back to %s and the work is running again\n", host, r.version)
}

// The remote half of going back. Deliberately small: it moves one file it did
// not choose the name of, restarts, and says what came up.
var rollbackScript = []byte(`set -eu
sudo=""
if [ "$(id -u)" -ne 0 ]; then
	sudo="sudo -n"
fi
if [ ! -x /usr/local/bin/hostd.previous ]; then
	echo "there is no previous hostd on this machine to go back to" >&2
	exit 1
fi
$sudo install -m 0755 /usr/local/bin/hostd.previous /usr/local/bin/hostd
$sudo systemctl restart hostd

ready=0
for _ in $(seq 1 50); do
	if $sudo test -S /run/hostd/hostd.sock; then
		ready=1
		break
	fi
	sleep 0.1
done
if [ "$ready" -eq 0 ]; then
	echo "the previous hostd did not open its socket either; systemctl status hostd" >&2
	exit 1
fi
$sudo journalctl -u hostd -n 50 -o cat --no-pager |
	grep 'listening on ' | tail -1 | awk '{print $2}'
`)
