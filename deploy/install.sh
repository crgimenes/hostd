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

For each host it reads the architecture over ssh, cross-compiles hostd and
hostctl for it, installs them in /usr/local/bin with the systemd unit, enables
the service and asks the running daemon to describe itself. Running it again
upgrades in place: the supervised services are not interrupted.

Needs ssh access and either root or passwordless sudo on the host. Nothing is
installed on the machine beyond the two binaries and the unit.

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
$sudo install -m 0755 "$REMOTE_DIR/hostctl" /usr/local/bin/hostctl
$sudo install -m 0644 "$REMOTE_DIR/hostd.service" /etc/systemd/system/hostd.service
rm -rf "$REMOTE_DIR"
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
# What the daemon itself says it is. A version other than the one just
# installed means the restart did not take, and the install would otherwise
# report success for the binary that was already there.
$sudo hostctl -filo describe | sed -n 's/.*(tuple "version" "\([^"]*\)").*/\1/p'
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
	GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w -X main.Version=$version" -o "$work/hostctl" ./cmd/hostctl

	remote_dir=$(ssh -o BatchMode=yes "$host" mktemp -d) || return 1
	scp -q "$work/hostd" "$work/hostctl" deploy/hostd.service "$host:$remote_dir/" || return 1
	rm -rf "$work"

	described=$(remote_script | ssh -o BatchMode=yes "$host" "REMOTE_DIR='$remote_dir' sh -s") || return 1
	if [ "$described" != "$version" ]; then
		echo "$host: the daemon reports $described, not the $version just installed; the restart did not take" >&2
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
