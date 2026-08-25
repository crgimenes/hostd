#!/usr/bin/env bash

# Installing hostd is copying a binary and activating the service. Anything
# more would be a playbook, and needing a playbook means the install already
# failed its purpose.
#
# Re-running this is also the upgrade path: hostd is replaced and restarted,
# and the services it supervises keep running, because the next daemon adopts
# them by pid and start time.

set -euo pipefail

usage() {
	cat <<'EOF'
deploy/install.sh puts hostd on a machine and starts it.

usage:
  deploy/install.sh <host>...

For each host it reads the architecture over ssh, cross-compiles hostd for it,
installs it in /usr/local/bin with the systemd unit, enables the service and
checks that the daemon which came up is the one just installed. Running it
again upgrades in place: the supervised services are not interrupted.

It also puts the account you ssh in as into the hostd group, which is the
permission to operate the machine: hostctl reaches the daemon by running
"hostd -stdio" there over ssh, so there is no port, no key of ours and no
handshake to configure. What sshd already does is not done again.

Only hostd is installed. hostctl is the operator's client and belongs on the
operator's machine; the daemon is headless and has no interface of its own.

Needs ssh access and either root or passwordless sudo on the host. Nothing is
installed on the machine beyond the binary and the unit.

output:
  one line per host on stdout, fields separated by ';'; diagnostics on stderr.

exit status:
  0 every host answered   1 at least one host failed

example:
  deploy/install.sh yuki.local selene.local
EOF
}

case "${1:-}" in
-h | --help | -help)
	usage
	exit 0
	;;
"")
	usage >&2
	exit 2
	;;
-*)
	echo "unknown flag $1" >&2
	usage >&2
	exit 2
	;;
esac

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"

# No tags yet, so the commit is what the daemon can honestly report.
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

# The remote half. It runs as a whole so a half-finished install cannot look
# like a working one, and it ends by asking the daemon to answer.
remote_script() {
	cat <<'EOF'
set -eu
sudo=""
if [ "$(id -u)" -ne 0 ]; then
	sudo="sudo -n"
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
# already there. The journal is the right place to ask: hostctl is not on this
# machine, and putting it here to check an install would install the client on
# every host in the fleet.
version=$($sudo journalctl -u hostd -n 50 -o cat --no-pager |
	grep 'listening on ' | tail -1 | awk '{print $2}')
printf '%s\n' "$version"
EOF
}

install_host() {
	local host=$1
	local machine arch work remote_dir described

	machine=$(ssh -o BatchMode=yes "$host" uname -m) || return 1
	case "$machine" in
	x86_64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*)
		echo "$host: hostd targets x86_64 and aarch64, not $machine" >&2
		return 1
		;;
	esac

	work=$(mktemp -d)
	GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w -X main.Version=$version" -o "$work/hostd" ./cmd/hostd

	remote_dir=$(ssh -o BatchMode=yes "$host" mktemp -d) || return 1
	scp -q "$work/hostd" daemon/hostd.service "$host:$remote_dir/" || return 1
	# The one file this function put there, by name. mktemp gave the directory,
	# but a recursive delete of a variable is a habit, and habits outlive the
	# place they were safe in.
	rm -f "$work/hostd"
	rmdir "$work" || echo "left $work behind: something else is in it" >&2

	described=$(remote_script | ssh -o BatchMode=yes "$host" "REMOTE_DIR='$remote_dir' sh -s") || return 1
	# Group membership is decided at login, and a multiplexed connection keeps
	# the groups of the session that opened it — with ControlPersist that can
	# be ten minutes of "permission denied" nobody can explain. Ending the
	# master makes the next command a fresh login.
	ssh -O exit "$host" 2>/dev/null || true
	if [ "$described" != "$version" ]; then
		echo "$host: the daemon that came up reports $described, not the $version just installed; the restart did not take" >&2
		return 1
	fi
	printf '%s;%s;%s;%s\n' "$host" "$arch" "$version" running
}

failed=0
for host in "$@"; do
	# One host that fails does not stop the rest; the status at the end says
	# whether everything answered.
	if ! install_host "$host"; then
		echo "$host: install failed" >&2
		failed=1
	fi
done
exit "$failed"
