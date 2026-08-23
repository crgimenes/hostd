# hostd

A control plane for a small fleet of Linux machines.

`hostd` runs on each machine and supervises the services declared there.
`hostctl` runs on yours and operates the fleet: it is where you look at what
every machine is doing, read logs, start and stop services, and apply
configuration. The daemon is headless — it has no interface of its own.

## What it replaces

**tmux, as a way of holding a server up.** A server started inside a `tmux`
session lives only in that session, and its output is a screen buffer: no
search, no retention, no timestamps, gone after a reboot, and visible in full
to anyone who gets onto the machine. Under `hostd` the process is supervised
properly, and every line it writes to `stdout` or `stderr` becomes a log entry
with a time, a service and a stream — readable from your own machine with
`hostctl log`.

**crontab**, for scheduled work (coming in a later stage): a service with a
schedule runs like any other, with a run history, logs and events in the same
place.

The service does not have to know any of this. It keeps writing to `stdout`.

## Status

Early. What works today:

- services declared one per file in [Filo](https://github.com/crgimenes/filo);
- supervision with restart policies and backoff;
- capture of `stdout` and `stderr`, with search and follow;
- events and output in one timeline, so a failure reads in order;
- a local control socket, and `hostctl` over it;
- **restarting `hostd` does not stop the services**: they keep running and the
  new daemon adopts them, proving each process is the one it started.

Not built yet: the encrypted network protocol with key pairs, containers,
scheduled services, metrics, the graphical mode, migration between machines,
and self-update. The plan is in `TODO.md`; the full design is in `project.md`.

## Try it

```bash
go build -o /tmp/hostd ./cmd/hostd && go build -o /tmp/hostctl ./cmd/hostctl
export HOSTD_ROOT=/tmp/hostd-try
mkdir -p "$HOSTD_ROOT/etc/services"
cat > "$HOSTD_ROOT/etc/services/ticker.filo" <<'EOF'
(service
  (tuple "name" "ticker")
  (tuple "command" "/bin/sh")
  (tuple "args" (list "-c" "i=0; while true; do i=$((i+1)); echo \"tick $i\"; sleep 1; done"))
  (tuple "restart" "always"))
EOF
/tmp/hostd &
/tmp/hostctl status
/tmp/hostctl log --follow
```

`HOSTD_ROOT` redirects every path hostd uses. Without it, the paths are the
real ones: `/etc/hostd`, `/var/lib/hostd`, `/run/hostd`.

## A service is one file

```lisp
(service
  (tuple "name" "api")
  (tuple "command" "/usr/local/bin/api")
  (tuple "args" (list "--listen" ":8080"))
  (tuple "restart" "always"))
```

`name` and `command` are required; the file must be named after the service.
`kind` defaults to `exec`, `state` to `running`, `restart` to `always`.
`dir`, `env` and `stop-timeout` are optional.

Filo is a language, not just a notation, so a file may compute a value — but it
produces a declaration and never touches the machine. There is no builtin for
files, network or processes in a configuration.

## Install on a machine

Copy the binary, add a unit, start it:

```bash
scp hostd root@host:/usr/local/bin/hostd
scp deploy/hostd.service root@host:/etc/systemd/system/
ssh root@host 'systemctl daemon-reload && systemctl enable --now hostd'
```

The unit uses `KillMode=process` on purpose: stopping or restarting `hostd`
must not take the services down with it.

## Command line

```console
hostctl status                    what every service is doing
hostctl describe                  versions and capabilities of the daemon
hostctl service list
hostctl service start <name>
hostctl service stop <name>
hostctl service restart <name>
hostctl apply                     re-read the services directory and converge
hostctl log [pattern]             what the services wrote
hostctl log --follow              keep watching
```

`--filo` makes `stdout` carry a Filo expression and nothing else; progress and
diagnostics go to `stderr`. Exit codes are part of the interface: `0` success,
`1` failed, `2` bad arguments, `3` communication, `4` authorisation, `5`
partial success.

## Build

Go 1.27, no cgo, static. Targets are `linux/amd64` and `linux/arm64`.

```bash
go test -race -count 1 ./...
```

## Licence

MIT. See [LICENSE](LICENSE).
