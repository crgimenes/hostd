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

A container is the same file with another kind:

```lisp
(service
  (tuple "name" "site")
  (tuple "kind" "container")
  (tuple "image" "site:2026-08-23")
  (tuple "volumes" (list "data:/data" "/etc/hostd/site:/etc/site:ro"))
  (tuple "memory-mb" 256)
  (tuple "restart" "always"))
```

The image runs its own command, so `args` only overrides what it takes. What
the container gets from the machine is what the file names and nothing else:
no host network, no host process namespace, no devices, no added capabilities,
never privileged. A port with no address in front of it binds to loopback,
where a reverse proxy on the same host reaches it and the internet does not —
`hostd` publishes the port and leaves the proxy to whoever owns it.

Restarting is `hostd`'s job, so the runtime is told not to do it: two
supervisors with different opinions about one process is how a service flaps.
Restarting `hostd` does not restart the container either — the next daemon asks
the runtime what it already owns, by label, and adopts it. Container output
lands in the same timeline as the events about it, and the started event names
the digest that actually ran, because a tag can be made to mean something else
tomorrow.

Every service of a host shares one network, and answers on it by its own name.
That is why the example above publishes nothing to the machine: a reverse proxy
in front of it reaches `http://site` directly, and the only thing that has to
be published is whatever answers the internet.

```lisp
(service
  (tuple "name" "caddy")
  (tuple "kind" "container")
  (tuple "image" "caddy:2-alpine")
  (tuple "ports" (list "0.0.0.0:80:80" "0.0.0.0:443:443"))
  (tuple "volumes" (list "/etc/hostd/caddy:/etc/caddy:ro" "data:/data"))
  (tuple "restart" "always"))
```

`volumes` says what outlives the container. A source with no slash is storage
the runtime keeps, created if absent and named after the service that asked for
it (`data` becomes `hostd-caddy-data`); anything else is an absolute path on the
machine. Removing a service never removes its storage — deleting somebody's data
is not a decision a converge loop should make. The runtime's own socket cannot
be mounted: handing a container that socket hands it the machine, and it looks
like an ordinary line in an ordinary file.

Docker is the runtime; Podman answers the same API on its own socket, and
whichever is present is the one used.

Filo is a language, not just a notation, so a file may compute a value — but it
produces a declaration and never touches the machine. There is no builtin for
files, network or processes in a configuration.

## Install on a machine

Installing is copying a binary and activating the service:

```bash
deploy/install.sh yuki.local
```

It reads the machine's architecture over ssh, cross-compiles `hostd` for it,
installs it in `/usr/local/bin` with the systemd unit, enables the service and
checks that the daemon which came up reports the version just installed — any
other version means the restart did not take. It needs ssh access and either
root or passwordless sudo, and installs nothing else.

It also puts the account you ssh in as into the `hostd` group, which is the
permission to operate that machine.

Only the daemon goes on the machine. `hostctl` is the operator's client and
runs on the operator's computer; a host that needs the client installed to be
operated is a host you are still administering by ssh.

Running it again is the upgrade path: the daemon is replaced and restarted,
and the services under supervision keep running. The unit uses
`KillMode=process` for exactly that reason — the default would kill every
process in the unit's cgroup, which is every service `hostd` supervises.

## Operating a machine from your own

```bash
hostctl -host yuki.local status
hostctl -host yuki.local log -follow
```

Reaching a machine is `ssh` running `hostd -stdio` on it. Authentication, host
identity and the record of the attempt are `sshd`'s, so **hostd puts no port on
the network at all** — the daemon listens on a unix socket and nothing else.
A key restricted with a forced command in `authorized_keys` is a permission this
program did not have to invent:

```
command="/usr/local/bin/hostd -stdio" ssh-ed25519 AAAA... an-agent
```

Membership of the `hostd` group on the machine is what lets an account open the
socket, and the audit log records that account by name — the kernel's answer to
who is on the other end, not something the caller said about itself. Group
membership is decided at login: after being added, open a new session (`ssh -O
exit <host>` if you multiplex).

## Sending an image

Build it here, send it there, declare it. No registry sits in the middle, and
the machine fetches nothing by itself:

```bash
docker build --platform linux/amd64 -t site:2026-08-23 .
hostctl -host yuki.local image push site:2026-08-23
```

The archive streams out of the runtime here and into the runtime there through
the same ssh the commands travel on — neither machine writes it to disk. An
image built for another architecture is refused before the bytes cross, saying
which platform to build for, because the alternative is an image that loads
perfectly and then fails to start with `exec format error`.

What proves the transfer is the sha256 of the bytes, computed on both sides.
It is not the image id: two daemons reading the same archive arrive at
different ids, because an id is of the config each one writes. That is also why
a service file names the image by the id **that machine** reported, or by tag —
an id is not portable across the fleet.

## The fleet

The fleet is a file you keep — which machines exist, and what to call groups of
them. Reaching any of them is ssh's business, and ssh already knows its own
hosts.

```bash
hostctl -all status                    every machine in the inventory
hostctl -hosts yuki.local,m1.local plan
hostctl -tag arm64 describe
```

`inventory.filo`, in the hostd directory of your config:

```lisp
(inventory
  (host (tuple "name" "yuki.local") (tuple "tags" (list "amd64" "docker")))
  (host (tuple "name" "cronos.local") (tuple "tags" (list "arm64" "dashboard"))))
```

Machines are asked at once, eight at a time, and each answer is printed whole
under its host; a machine that did not answer says so in the same place, because
which host is missing is part of the fleet's state. Exit `5` means some answered
and some did not — an outcome of its own, since reading it as failure retries
what already worked and reading it as success acts on a picture with a hole in
it. With `-filo` the whole fleet comes back as one expression, each machine with
its own exit code and body.

`-follow` and `-expect-generation` name one machine: watching is a stream, and a
generation from one host means nothing on another.

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
