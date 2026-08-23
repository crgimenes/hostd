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
- events with stable codes, in the same timeline as the output, so a failure
  reads in order;
- durable logs in SQLite with full-text search, with retention by age and by
  row count from the first day;
- a plan you can review before applying it — the same computation `apply`
  carries out, so a dry run is not decoration;
- generations, so two operators or an agent cannot overwrite each other, and an
  audit log that records refusals as well as changes;
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
/tmp/hostctl log -follow
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

Installing is copying a binary and activating the service:

```bash
deploy/install.sh yuki.local
```

It reads the machine's architecture over ssh, cross-compiles `hostd` and
`hostctl` for it, installs them in `/usr/local/bin` with the systemd unit,
enables the service and asks the running daemon to describe itself — an
answer with a different version means the restart did not take. It needs ssh
access and either root or passwordless sudo, and installs nothing else.

Running it again is the upgrade path: the daemon is replaced and restarted,
and the services under supervision keep running. The unit uses
`KillMode=process` for exactly that reason — the default would kill every
process in the unit's cgroup, which is every service `hostd` supervises.

## Command line

```console
hostctl status                    what every service is doing
hostctl describe                  versions and capabilities of the daemon
hostctl service list
hostctl service start <name>
hostctl service stop <name>
hostctl service restart <name>
hostctl plan                      what an apply would do, without doing it
hostctl apply                     re-read the services directory and converge
hostctl audit                     who changed what, and when
hostctl log [pattern]             what the services wrote
hostctl log -follow               keep watching
hostctl metrics                   what the host and its services are using
hostctl metrics -window 1h        the same, over a window
```

A change that would stop a service nothing declares any more is refused unless
you pass `-allow-destructive`; the refusal names what would go. Pass
`-expect-generation N` to a change and it is refused if the host has moved
since you looked, instead of overwriting somebody else's work.

`-filo` makes `stdout` carry a Filo expression and nothing else; progress and
diagnostics go to `stderr`. `-debug` adds one `key=value` line per request on
`stderr`, and `hostd -debug` writes one every 30 seconds with the convergence
percentiles and what the log store is holding or losing. Exit codes are part of the interface: `0` success,
`1` failed, `2` bad arguments, `3` communication, `4` authorisation, `5`
partial success, `6` refused and nothing changed.

## Metrics

`hostd` samples the host and every supervised service every 10 seconds: CPU,
memory, disk, network and the load average, plus CPU and resident memory per
service. The series live in SQLite next to the logs, at full resolution for six
hours and as one-minute averages after that; retention and a row ceiling are
set from the first day, and both are configurable in `hostd.filo`
(`metrics-retention-days`, `metrics-max-rows`).

```console
hostctl metrics                                       the newest value of everything
hostctl metrics -window 1h                            min, average, max and last
hostctl metrics -service api -metric cpu-percent -window 30m -filo
```

The counters come from `/proc` and are read on Linux only; anywhere else the
daemon says so once instead of inventing a graph. Service CPU is a percentage
of one core, the way `top` reports it, so a service using two cores fully
reads as 200.

## Build

Go 1.27, `CGO_ENABLED=0`, static. Targets are `linux/amd64` and `linux/arm64`.
The only dependencies are Filo and a pure-Go SQLite driver.

```bash
go test -race -count 1 -timeout 400s ./...
```

## Licence

MIT. See [LICENSE](LICENSE).
