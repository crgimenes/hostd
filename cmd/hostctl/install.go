package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/daemon"
	"github.com/crgimenes/hostd/supervisor"
	"github.com/crgimenes/hostd/version"
)

// Putting hostd on a machine is copying a binary and activating a service.
// Anything more would be a playbook, and needing a playbook means the install
// already failed its purpose.
//
// The daemon comes out of this binary, not off the network: a released hostctl
// carries the hostd of its own version, so an install needs no registry, no
// download and no Go toolchain anywhere — not on the machine being installed,
// and not on the operator's either. Re-running is the upgrade path, and the
// services the machine supervises are not interrupted by it.
func runInstall(ctx context.Context, opt options, args []string) (int, error) {
	if len(args) > 0 {
		return exitUsage, fmt.Errorf("install takes no arguments; name the machine with -host, -hosts, -tag or -all")
	}
	if opt.host == "" {
		return exitUsage, fmt.Errorf("install needs a machine; name it with -host")
	}
	host := opt.host
	out := opt.out

	machine, err := remoteArch(ctx, host)
	if err != nil {
		return exitComms, err
	}
	binary, err := daemonBinary(machine)
	if err != nil {
		return exitFailed, err
	}

	// What is there now, so the operator reads a transition rather than a
	// bare success. A machine with no hostd yet answers nothing, which is the
	// honest before-state of a first install.
	before := strings.TrimSpace(remoteOutput(ctx, host, "/usr/local/bin/hostd -version 2>/dev/null || true"))

	// Nothing is refused on version grounds (crg, 2026-08-27): neither end needs
	// to know which of two versions is newer, and putting an older daemon back
	// is a legitimate thing to want — a machine that just went amber may be
	// amber BECAUSE of the newest one. What the operator gets instead is the
	// transition named in the report, and the amber dot until the two agree.
	//
	// A diagnostic, not a refusal: a development build installing its own zips
	// is ordinary, and this only says the two stamps disagree. The window says
	// the same thing into its log, where its operator is looking.
	stale := staleDaemon()
	if stale != "" {
		_, _ = fmt.Fprintf(os.Stderr, "hostctl: %s\n", stale)
	}

	// And what it is DOING now, which is what the upgrade must not change. A
	// machine with no daemon yet has nothing to lose, and says so by answering
	// nothing here.
	wasRunning := runningServices(ctx, opt, host)

	answer, err := remoteOutputErr(ctx, host, "mktemp -d", nil)
	if err != nil {
		return exitComms, fmt.Errorf("%s: %w", host, err)
	}
	remoteDir, err := scratchPath(answer)
	if err != nil {
		return exitFailed, fmt.Errorf("%s: %w", host, err)
	}

	err = sendFile(ctx, host, remoteDir+"/hostd", binary)
	if err != nil {
		return exitComms, err
	}
	err = sendFile(ctx, host, remoteDir+"/hostd.service", daemon.Unit())
	if err != nil {
		return exitComms, err
	}

	reported, err := remoteOutputErr(ctx, host,
		"REMOTE_DIR='"+remoteDir+"' sh -s", installScript)
	if err != nil {
		return exitFailed, fmt.Errorf("%s: %w", host, err)
	}
	reported = strings.TrimSpace(reported)

	// Against the DAEMON's version, never this binary's. They are the same in a
	// release, but a development build carrying zips a previous `make dist`
	// left is exactly the case where they differ — and comparing against the
	// wrong one would fail an install that in fact worked.
	sent := daemon.Version()
	if reported != sent {
		return exitFailed, fmt.Errorf(
			"%s: the daemon that came up reports %s, not the %s just installed; the restart did not take",
			host, reported, sent)
	}
	// Group membership is decided at login, and a multiplexed connection keeps
	// the groups of the session that opened it — with ControlPersist that can be
	// ten minutes of "permission denied" nobody can explain. Ending the master
	// makes the next command a fresh login. It happens BEFORE the check below,
	// which is the first thing to need the group on a first install.
	_ = exec.CommandContext(ctx, "ssh", "-O", "exit", "--", host).Run() // #nosec G204 -- "--" keeps the name a name

	// The version that came up is not the whole question. A daemon that came
	// back having lost the work it was supervising is not a good upgrade, and
	// until this check existed it was reported as one.
	lost := servicesSurvived(ctx, opt, host, wasRunning)
	if len(lost) > 0 {
		for _, complaint := range lost {
			_, _ = fmt.Fprintf(out, "%s: %s\n", host, complaint)
		}
		// Automatic, and the argument for it is the unattended case: `-all
		// install` at three in the morning must not leave a machine on a daemon
		// that just lost its work while the operator is asleep. The binary that
		// was there is the one known to have worked HERE, which is worth more
		// than its version number. An upgrade that was in fact fine and got
		// reverted by something unrelated costs one re-run; the other mistake
		// costs a night.
		_, _ = fmt.Fprintf(out, "%s: putting the previous daemon back\n", host)
		restored := goBack(ctx, opt, host, wasRunning)
		restored.report(out, host)
		return exitFailed, fmt.Errorf(
			"%s did not keep its services through the upgrade to %s, so it was put back; hostd is meant to be replaceable without touching them",
			host, reported)
	}

	_, _ = fmt.Fprintf(out, "%s;%s;%s;%s;%s\n",
		host, machine, reported, transition(before, reported), kept(wasRunning))
	return exitOK, nil
}

// What the upgrade carried through untouched. Saying "nothing was running" out
// loud matters: it is the difference between a promise kept and a promise never
// tested on this machine.
func kept(wasRunning []supervisor.Status) string {
	if len(wasRunning) == 0 {
		return "no service was running"
	}
	return fmt.Sprintf("%d service(s) kept running", len(wasRunning))
}

// Reaching one named machine, when the caller already knows which. The install
// and its self-test are the two that do: everything else arrives here through
// the fan-out, which has connected already.
func connectTo(ctx context.Context, opt options, host string) (*api.Client, error) {
	one := opt
	one.host = host
	return connect(ctx, one)
}

func fetchStatuses(ctx context.Context, client *api.Client) ([]supervisor.Status, error) {
	resp, err := client.Do(ctx, api.Request{Op: api.OpStatus})
	if err != nil {
		return nil, err
	}
	if resp.Failed() {
		return nil, resp.Err()
	}
	var statuses []supervisor.Status
	err = decode(ctx, resp.Body, &statuses)
	if err != nil {
		return nil, err
	}
	return statuses, nil
}

// runningServices asks the daemon that is there now what it is doing. A machine
// being installed for the first time has none, and that is not an error: it is
// a machine with nothing to lose.
func runningServices(ctx context.Context, opt options, host string) []supervisor.Status {
	client, err := connectTo(ctx, opt, host)
	if err != nil {
		return nil
	}
	defer func() { _ = client.Close() }()
	statuses, err := fetchStatuses(ctx, client)
	if err != nil {
		return nil
	}
	return runningIn(statuses)
}

// The daemon this binary carries comes from daemon/zips, which `make` writes
// and a bare `go build ./cmd/hostctl` leaves exactly as it found it. Same
// build, same string. Different strings mean the embedded daemon is from
// another build, and then an install sends a version nobody meant and the
// amber alert compares against it — which on 2026-08-27 cost a real hunt for
// a button that was correctly absent. `make` is the fix and the default now;
// this is the belt.
func staleDaemon() string {
	carried := daemon.Version()
	if carried == "" || carried == version.Version {
		return ""
	}
	// One sentence that is true both ways: a stamped binary whose zips are from
	// an older build, and an unstamped `go build` that cannot vouch for zips
	// that may well be its own.
	return fmt.Sprintf(
		"this hostctl is %s and carries hostd %s: the stamps disagree, so the embedded daemon may be from another build — make builds both together",
		version.Version, carried)
}

// "hostd <version> (protocol N, schema N)" is what a daemon answers, but a
// machine can answer anything: what does not read as that answer is reported as
// no version rather than indexed into. Only the field count was checked before,
// so "something went wrong" produced the version "went".
func versionIn(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "hostd" {
		return ""
	}
	return fields[1]
}

// What changed, in the words an operator is asking in: a first install, a
// machine that already had this one, or the version it had before. Not
// "upgraded" and not "downgraded" — naming which direction it went would need
// an order this program deliberately does not compute, and calling a downgrade
// an upgrade is the kind of small lie that gets believed later.
func transition(before, now string) string {
	switch {
	case before == "":
		return "installed"
	case strings.Contains(before, now):
		return "unchanged"
	}
	was := versionIn(before)
	if was == "" {
		return "replaced"
	}
	return "replaced " + was
}

// The daemon for the machine's architecture, or the reason there is none. A
// development build carries no daemon at all, and saying which of the two is
// wrong saves an hour.
func daemonBinary(arch string) ([]byte, error) {
	archive, err := daemon.Zip(arch)
	if err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		opened, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = opened.Close() }()
		// Bounded: the archive is this binary's own, but a reader with no
		// ceiling is a habit that outlives the place it was safe in.
		return io.ReadAll(io.LimitReader(opened, maxDaemonBytes))
	}
	return nil, fmt.Errorf("the embedded hostd archive for linux/%s is empty", arch)
}

// Far above any real daemon; something over it is a mistake, not a build.
const maxDaemonBytes = 256 << 20

// scratchPath is the trust boundary this command has: everything else it sends
// is its own, but the scratch directory is a string the far machine chose, and
// it goes on to be interpolated into a shell command there.
//
// A single quote in it would end the quoting and hand the rest of the line to
// that shell; a login banner or a shell that echoes would pollute it with a
// second line. What a mktemp answers is one absolute path of ordinary
// characters, so anything else is refused rather than repaired — a value that
// has to be cleaned up is a value nobody can reason about.
func scratchPath(answer string) (string, error) {
	path := strings.TrimSpace(answer)
	switch {
	case path == "":
		return "", fmt.Errorf("mktemp -d answered nothing, so there is nowhere to put the daemon")
	case strings.ContainsAny(path, "\n\r"):
		return "", fmt.Errorf("mktemp -d answered more than one line, which is a shell that says something on login: %q", answer)
	case !strings.HasPrefix(path, "/"):
		return "", fmt.Errorf("mktemp -d answered %q, which is not an absolute path", path)
	case strings.Contains(path, ".."):
		return "", fmt.Errorf("mktemp -d answered %q, which walks back up a tree", path)
	}
	// Deliberately narrower than what a filesystem allows: this is a path a
	// mktemp just made, not a path a person chose, and every character outside
	// this set is a sign something else answered.
	for _, r := range path {
		ok := r == '/' || r == '.' || r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return "", fmt.Errorf("mktemp -d answered %q, which carries characters a scratch directory has no reason to: %q", path, r)
		}
	}
	return path, nil
}

// What hostd targets, from what the machine says it is.
func remoteArch(ctx context.Context, host string) (string, error) {
	machine, err := remoteOutputErr(ctx, host, "uname -m", nil)
	if err != nil {
		return "", fmt.Errorf("%s: %w", host, err)
	}
	arch, err := hostdArch(machine)
	if err != nil {
		return "", fmt.Errorf("%s: %w", host, err)
	}
	return arch, nil
}

// hostdArch maps what uname says onto what a release carries. A machine that
// is neither is refused with both names in it: "unsupported" alone leaves the
// operator guessing whether the fault is theirs.
func hostdArch(machine string) (string, error) {
	switch strings.TrimSpace(machine) {
	case "x86_64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("hostd targets x86_64 and aarch64, not %s", strings.TrimSpace(machine))
	}
}

func sendFile(ctx context.Context, host, path string, content []byte) error {
	_, err := remoteOutputErr(ctx, host, "cat > '"+path+"'", content)
	if err != nil {
		return fmt.Errorf("%s: sending %s: %w", host, path, err)
	}
	return nil
}

func remoteOutput(ctx context.Context, host, command string) string {
	answer, err := remoteOutputErr(ctx, host, command, nil)
	if err != nil {
		return ""
	}
	return answer
}

// One ssh, with the same options every other reach uses — api.SSHArguments and
// not a list of its own, which is what this was and what made `-all install`
// look frozen: without ConnectTimeout a switched-off machine spends the
// kernel's tens of seconds, and the operator interrupts a program that is
// merely waiting. It also carries the "--" that keeps an inventory name a name.
// stdin carries what the remote command reads: the file being written, or the
// script being run.
func remoteOutputErr(ctx context.Context, host, command string, stdin []byte) (string, error) {
	// #nosec G204 -- the host comes from the operator's own flags, which is the
	// same trust as the shell they typed them in
	cmd := exec.CommandContext(ctx, "ssh", api.SSHArguments(host, []string{command})...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	// ssh's own complaints are diagnostics and belong where diagnostics go.
	cmd.Stderr = os.Stderr
	answer, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(answer), nil
}

// The remote half. It runs as a whole so a half-finished install cannot look
// like a working one, and it ends by asking the daemon to answer.
var installScript = []byte(`set -eu
sudo=""
if [ "$(id -u)" -ne 0 ]; then
	sudo="sudo -n"
fi
# Keep what is on the machine before replacing it. It is the one binary known
# to have worked HERE, which is worth more than any version number when the new
# one comes up wrong. Copied rather than moved: a machine that loses power
# between the two commands must still have a daemon.
if [ -x /usr/local/bin/hostd ]; then
	$sudo cp -p /usr/local/bin/hostd /usr/local/bin/hostd.previous
fi
$sudo install -m 0755 "$REMOTE_DIR/hostd" /usr/local/bin/hostd
$sudo install -m 0644 "$REMOTE_DIR/hostd.service" /etc/systemd/system/hostd.service

# The group is the permission, and it exists before the daemon starts so the
# socket is created with it. Adding the account doing the install is what lets
# that same account operate the machine afterwards without sudo.
$sudo groupadd -f hostd
$sudo usermod -aG hostd "$(id -un)"

# By name, never recursively: these two files are exactly what was sent, and a
# recursive delete of a path this script did not choose is a way to lose a
# machine over a mktemp that answered something unexpected. rmdir then refuses
# if anything else is in there, which surfaces the surprise instead of removing
# it — and is not worth failing a finished install over.
rm -f "$REMOTE_DIR/hostd" "$REMOTE_DIR/hostd.service"
if ! rmdir "$REMOTE_DIR"; then
	echo "left $REMOTE_DIR behind: something else is in it" >&2
fi

$sudo systemctl daemon-reload
$sudo systemctl enable hostd >/dev/null
$sudo systemctl restart hostd

# Bounded: a daemon that is not listening in five seconds is not starting, and
# waiting forever would turn a failed install into a hung terminal.
ready=0
for _ in $(seq 1 50); do
	if $sudo test -S /run/hostd/hostd.sock; then
		ready=1
		break
	fi
	sleep 0.1
done
if [ "$ready" -eq 0 ]; then
	echo "hostd did not open its socket; systemctl status hostd" >&2
	exit 1
fi
# What the daemon that just came up says it is, from the line it writes on
# start. A version other than the one just installed means the restart did not
# take, and the install would otherwise report success for the binary that was
# already there.
$sudo journalctl -u hostd -n 50 -o cat --no-pager |
	grep 'listening on ' | tail -1 | awk '{print $2}'
`)
