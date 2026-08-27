# hostd

A control plane for a small fleet of Linux machines.

`hostd` runs on each machine and reconciles the containers declared there.
`hostctl` runs on yours and operates the fleet: it is where you look at what
every machine is doing, read logs, and put services onto machines and take them
off again — from a window or from the command line, over the same operations.
The daemon is headless: it has no interface of its own.

## What it replaces

**tmux, as a way of holding a server up.** A server started inside a `tmux`
session lives only in that session, and its output is a screen buffer: no
search, no retention, no timestamps, gone after a reboot, and visible in full
to anyone who gets onto the machine. Under `hostd` the server runs in a
container held by Docker or Podman, and every line it writes to `stdout` or
`stderr` becomes a log entry with a time, a service and a stream — readable
from your own machine with `hostctl log`.

**crontab**, for scheduled work: a service with `every` runs on the wall-clock
schedule, with run history, logs and events in the same place.

The service does not have to know any of this. It keeps writing to `stdout`.

## Status

The core is running on the Linux bench. What works today:

- one Filo declaration per container service, as a file or a directory with
  `init.filo` and its artifacts;
- restart policies delegated to Docker or Podman, which already survive a
  daemon restart and a machine reboot;
- capture of `stdout` and `stderr`, with search and follow;
- events with stable codes, in the same timeline as the output, so a failure
  reads in order;
- durable logs in SQLite with full-text search, with retention by age and by
  row count from the first day;
- a plan you can review before applying it — the same computation `apply`
  carries out, so a dry run is not decoration;
- generations, so two operators or an agent cannot overwrite each other, and an
  audit log that records refusals as well as changes;
- jobs aligned to the wall clock, with overlap policy and a parallelism ceiling;
- host and service metrics with full detail for six hours and minute rollups
  after that;
- fleet operation over SSH, with no hostd port or key system of its own;
- image transfer between runtimes, streamed and verified by SHA-256;
- a graphical panel that operates the fleet over the same API as the command
  line: two inventories (the machines, and the services the tree describes),
  one click per action, and everything it does narrated in the log;
- **restarting `hostd` does not stop the services**: they keep running and the
  new daemon asks the runtime what it already holds.

Not built yet: service rollback and image cleanup, migration between machines,
self-update, and mutation from the graphical panel. The queue is in `TODO.md`;
the full design is in `project.md`.

## Try it

```bash
go build -o /tmp/hostd ./cmd/hostd && go build -o /tmp/hostctl ./cmd/hostctl
docker pull alpine:latest
export HOSTD_ROOT=/tmp/hostd-try
mkdir -p "$HOSTD_ROOT/etc/services"
cat > "$HOSTD_ROOT/etc/services/ticker.filo" <<'EOF'
(service
  (tuple "name" "ticker")
  (tuple "image" "alpine:latest")
  (tuple "args" (list "sh" "-c" "i=0; while true; do i=$((i+1)); echo tick-$i; sleep 1; done"))
  (tuple "restart" "always"))
EOF
/tmp/hostd &
hostd_pid=$!
trap 'kill "$hostd_pid"' EXIT
/tmp/hostctl status
/tmp/hostctl log -follow
```

`HOSTD_ROOT` redirects every path hostd uses. Without it, the paths are the
real ones: `/etc/hostd`, `/var/lib/hostd`, `/run/hostd`.

## Your fleet is a directory you version

Everything `hostctl` needs is a directory you keep under version control, and
everyone who has it and an authorised key can operate the fleet:

```text
fleet/
├── inventory.filo      the machines, and what to call groups of them
├── site.filo           a service that needs nothing else
└── caddy/              a service that does
    ├── init.filo       the declaration
    └── Caddyfile       what it needs
```

A service is one file until it needs something beside it; then it is a
directory with an `init.filo` and its files, and nothing else about it changes.
The directory is the name.

The tree is a **catalogue**: it describes what there is to run. **Where each
service runs is not written in any file** — it is your decision, per machine,
at the moment you make it:

```bash
hostctl -host yuki.local service deploy site     put it there
hostctl -host yuki.local service remove site     take it off there
```

A deploy sends the declaration and the files beside it, gets the image onto the
machine, and starts a fresh container. **Deploying again overwrites** — that is
how a new version goes live, not an error. The image arrives by whichever path
is true, and the command says which one it used: built here, it is sent from
here; already on the machine, it stays; otherwise the machine pulls it from its
registry.

A remove stops the service and takes its container, its declaration and the
image hostd put there off that machine. **Volumes and their data are never
touched**, and the catalogue still describes the service — so a deploy puts it
back. An image hostd did not put there (a public base image, which cannot be
pushed at all) is reported and left alone: the same rule the image cleanup
follows.

What a machine keeps in `/etc/hostd/services/` is the record of what was put on
it — that machine's desired state, written by deploy and erased by remove. The
files land there (with `caddy.d/` for what travels with `caddy.filo`), read only
inside the container at the path the declaration names.

```bash
hostctl -host yuki.local push     refresh the descriptions of what it already
                                  runs, from the tree — it neither places nor
                                  removes anything
hostctl -host yuki.local apply    converge that machine with what it holds
```

`apply` is the same computation `plan` shows, and it is what the drift round
runs on its own every fifteen seconds: a container removed by hand comes back
without anyone connected.

## A service is one file

```lisp
(service
  (tuple "name" "api")
  (tuple "image" "api:2026-08-24")
  (tuple "args" (list "serve" "--listen" ":8080"))
  (tuple "restart" "always"))
```

`name` and `image` are required; the file must be named after the service.
`kind` defaults to `container`, and no other kind exists. `state` defaults to
`running`, `restart` to `always`. `args`, `dir`, `env`, `ports`, `volumes`,
`memory-mb`, `cpus` and `stop-timeout` are optional.

The image runs its own command, so `args` only overrides what it takes. What
the container gets from the machine is what the file names and nothing else:
no host network, no host process namespace, no devices, no added capabilities,
never privileged. A port with no address in front of it binds to loopback,
where a reverse proxy on the same host reaches it and the internet does not —
`hostd` publishes the port and leaves the proxy to whoever owns it.

Keeping a service alive is the runtime's job, not `hostd`'s: it already
survives its own restart and the machine's reboot, and two supervisors with
different opinions about one process is how a service flaps. `restart` in the
file becomes the runtime's policy, and a policy that drifted is corrected in
place rather than by recreating the container.

That is also how a stop is remembered without `hostd` writing anything down:
under a policy that keeps containers alive, one that is exited and not coming
back is one a hand stopped, because the runtime would have restarted anything
else. Restarting `hostd` restarts nothing — the next daemon asks the runtime
what it holds and picks the log up where its own store stopped. Container output
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
  (tuple "image" "caddy:2-alpine")
  (tuple "ports" (list "0.0.0.0:80:80" "0.0.0.0:443:443"))
  (tuple "volumes" (list "data:/data"))
  (tuple "config" "/etc/caddy")
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

`hostctl` carries the daemon inside it, one build per architecture, so
installing needs no toolchain at either end — not on the machine, and not on
yours:

```bash
hostctl -host yuki.local install
hostctl -all install
```

It copies the `hostd` that this `hostctl` was built with, installs it in
`/usr/local/bin` with the systemd unit, enables the service and checks that the
daemon which came up reports the version just installed — any other version
means the restart did not take. It needs ssh access and either root or
passwordless sudo, and installs nothing else. `hostctl -version` says which
daemon a given client carries.

It also puts the account you ssh in as into the `hostd` group, which is the
permission to operate that machine.

From a clone, `make dist` builds a `hostctl` with the daemon embedded, and that
binary installs the machine the same way.

Only the daemon goes on the machine. `hostctl` is the operator's client and
runs on the operator's computer; a host that needs the client installed to be
operated is a host you are still administering by ssh.

Running it again is the upgrade path: the daemon is replaced and restarted,
and the containers keep running because they belong to the runtime, not to the
daemon's process tree. The install checks that for itself — it records which
services were running and with which process before the restart, waits for the
new daemon to settle, and puts the previous binary back if anything it was
carrying did not survive.

## Putting a service behind a proxy

A declaration can say what a reverse proxy answers on its behalf, and `hostctl`
writes the Caddyfile from it:

```bash
hostctl -host yuki.local caddyfile > ~/.config/hostd/caddy/Caddyfile
```

```
(service
  (tuple "name" "site")
  (tuple "image" "site:laptop")
  (tuple "domain" (list "example.com" "www.example.com")))
```

The address does not have to be a public name: a bare port (`:80`) is for a
machine that serves one thing and matches no name, and `http://name.internal`
serves a name on a network with no public DNS without the proxy going after a
certificate it can never obtain. `upstream-port` says which port the container
listens on, defaulting to 80; the proxy is sent to the container's alias on the
managed network, because a service behind a proxy usually publishes nothing to
the machine at all.

It goes to stdout and stops there. Nothing installs it, nothing regenerates it,
and hostd renders nothing at runtime — the file lives beside the declaration,
travels in `push` like any other artifact, and is yours to edit afterwards.
What runs is a file somebody looked at and committed.

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
them, for asking several at once. Reaching any of them is ssh's business, and
ssh already knows its own hosts. **Where a service runs is not in this file
either**: a machine's tags group machines, they do not place services.

```bash
hostctl -all status                    every machine in the inventory
hostctl -hosts yuki.local,m1.local plan
hostctl -tag arm64 describe
```

`inventory.filo`, in the hostd directory of your config:

```lisp
(inventory
  (host (tuple "name" "yuki.local") (tuple "tags" (list "amd64" "docker")))
  (host (tuple "name" "web1") (tuple "tags" (list "web")))
  (host (tuple "name" "cronos.local") (tuple "tags" (list "arm64" "dashboard"))))
```

A machine is called here what you call it in `~/.ssh/config` — `web1` is a
`Host web1` there, and where that goes, who it logs in as, on which port,
through which jump host and with which key is that file's business. It is the
file you already keep, that every other tool on your machine already honours.
There is no second place here to write any of it, and so no second place to
disagree with.

A machine that does not answer is reported and left alone: no retry, no
reconnection. A client that kept trying would hide which machine went away, and
deciding what to do about it is the operator's.

Machines are asked at once, eight at a time, and each answer is printed whole
under its host; a machine that did not answer says so in the same place, because
which host is missing is part of the fleet's state. Exit `5` means some answered
and some did not — an outcome of its own, since reading it as failure retries
what already worked and reading it as success acts on a picture with a hole in
it. With `-filo` the whole fleet comes back as one expression, each machine with
its own exit code and body.

`-follow` and `-expect-generation` name one machine: watching is a stream, and a
generation from one host means nothing on another.

## Watching the fleet

```bash
hostctl gui
```

Two inventories: the machines, with what each one is running, its CPU and
memory over the last hour; and the services the tree describes, one small card
each with the machines in a dropdown beside a deploy. The log of the whole fleet
sits under both. Each machine is asked on its own loop, so one that is switched
off spends its ssh timeout on its own line and delays nothing else; only changed
fragments reach the page. `-host`, `-hosts` and `-tag` narrow it.

**It operates the fleet.** One click is one action — deploy, remove, restart,
stop, start, image cleanup, installing the daemon — with no confirmation and no
dialog: what it is doing arrives in the **log**, line by line, and the button
that started it stays held down until it is over. A progress indicator with
nothing behind it is somewhere for impatience to pile up; a log going by is
somebody watching their machine work, and able to act when it goes wrong. Every
action is the same API operation the command line calls, over its own ssh
connection, so the panel and the terminal cannot disagree — and the equivalent
command is printed beside each screen for whoever prefers the terminal.

A machine asking for attention says so three ways at once: an amber dot beside
it, a line in the log, and a button beside its name. Today that means a daemon
that differs from the one this window carries, or a machine ssh reached where no
hostd answered — the button installs it.

The page is served from a custom `app://` scheme out of the binary: nothing on
your machine listens on a port, and there is no CDN, no framework and no file on
disk behind it. The daemon does not carry any of this — `hostd` has no graphical
dependency at all.

## Command line

```console
hostctl status                    what every service is doing
hostctl describe                  versions and capabilities of the daemon
hostctl service list
hostctl service deploy <name>     put a service from the tree on this machine
hostctl service remove <name>     take it off this machine
hostctl service start <name>
hostctl service stop <name>
hostctl service restart <name>
hostctl service redeploy <name>   a fresh container from the declaration
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

## Jobs

A service with `every` runs and ends, over and over, instead of staying up:

```lisp
(service
  (tuple "name" "retry-webhooks")
  (tuple "image" "worker:2026-08-23")
  (tuple "every" "30s")
  (tuple "max-parallel" 8))
```

The cron this replaces stops at the minute; a duration says what it means and
goes below it, down to a second. Runs are aligned to the wall clock, so a job
every two minutes runs at even minutes and restarting `hostd` does not shift its
schedule. Starting the daemon is not one of the times a job was asked to run.

**Runs overlap by default**, because that is what cron does and what a worker
pool wants: if the last run has not finished when the next is due, another one
starts and they share the work. Nothing here coordinates them — they agree with
each other in whatever queue they read from, and a scheduler that tried to be
clever about it would be a second opinion about somebody else's work. Declare
`(tuple "overlap" "skip")` for work that must not run twice at once.

`max-parallel` is the ceiling, ten by default. Scaling without one is not
elasticity: it is a machine dying slowly while a new run starts every turn
because the old ones stopped finishing. Hitting it is reported in the timeline,
never silent.

Every run is recorded there too — when it started, what it printed, its exit
code and how long it took — under the run it belongs to, which is what a
`crontab` cannot answer:

```console
hostctl -host yuki.local log -service retry-webhooks
2026-08-23 14:11:40 retry-webhooks * run 1787505100000 started as container 6e7d5126699a
2026-08-23 14:11:40 retry-webhooks | working
2026-08-23 14:11:43 retry-webhooks * run 1787505100000 finished with exit 0 after 2.626s
```

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
The daemon uses Filo and a pure-Go SQLite driver. The graphical client uses
[glaze](https://github.com/crgimenes/glaze).

```bash
go test -race -count 1 -timeout 400s ./...
```

## Licence

MIT. See [LICENSE](LICENSE).
