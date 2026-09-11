<div align="center">

```
██████   ███   ██████   ██   ██████
██          ██     ██ ██  ██     ██
██     ██   ██ ██  ██ ██  ██     ██
    ██ ██   ██ ██  ██ ██  ██ ████
    ██ ██      ██  ██ ██  ██ ██  ██
██████   ███   ██  ██ ██  ██ ██  ██
```

Know what's running on your machine.

</div>

Sonar shows everything listening on localhost and puts it in order: every port
belongs to a **group** — normally the repository it was started from — and
inside that group to a named **service**. Start your dev servers with
`sonar start` and the whole project becomes one thing you can list as a tree,
wait for, tail, and stop with a single command. Docker containers, Compose
projects and processes you started by hand are picked up too, without any
configuration.

```
$ sonar list --tree
my-app  (3 ports, running)                        ~/code/my-app
├─ 5432  db          postgres:17                  http://localhost:5432
├─ 5173  frontend    vite (v5.4)                  http://localhost:5173
└─ 8000  api         uvicorn app:app              http://localhost:8000
ungrouped (1 port)
└─ 3000  next-server (v16.1.6)                    http://localhost:3000
```

## Install

### Homebrew (macOS / Linux)

```sh
brew install raskrebs/sonar/sonar
```

Homebrew 6 refuses formulae from third-party taps until you trust the tap once
(`Error: Refusing to load formula raskrebs/sonar/sonar from untrusted tap`):

```sh
brew trust raskrebs/sonar
```

### Install script

```sh
curl -sfL https://raw.githubusercontent.com/raskrebs/sonar/main/scripts/install.sh | bash
```

Downloads the latest binary to `~/.local/bin` and adds it to your PATH if needed. Restart your terminal or `source ~/.zshrc`.

On Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/raskrebs/sonar/main/scripts/install.ps1 | iex
```

Custom install location:

```sh
curl -sfL https://raw.githubusercontent.com/raskrebs/sonar/main/scripts/install.sh | SONAR_INSTALL_DIR=/usr/local/bin bash
```

Install a specific version:

```sh
curl -sfL https://raw.githubusercontent.com/raskrebs/sonar/main/scripts/install.sh | SONAR_VERSION=vX.Y.Z bash
```

```powershell
$env:SONAR_VERSION="vX.Y.Z"; irm https://raw.githubusercontent.com/raskrebs/sonar/main/scripts/install.ps1 | iex
```

### Using Go

```sh
go install github.com/raskrebs/sonar@latest
```

Shell completions (tab-complete port numbers):

```sh
sonar completion zsh > "${fpath[1]}/_sonar"   # zsh
sonar completion bash > /etc/bash_completion.d/sonar  # bash
sonar completion fish | source                 # fish
```

## Sixty seconds

Prefix the commands in your `dev.sh` with `sonar start`:

```sh
#!/usr/bin/env bash
sonar start --name db       --port 5432 -- docker compose up db &
sonar start --name api      --port 8000 -- uv run uvicorn app:app &
sonar start --name frontend --port 5173 -- npm run dev &
wait
```

The group name comes from the repository, so nothing else needs configuring.
In another terminal:

```sh
sonar list --tree
```

```
my-app  (3 ports, running)                        ~/code/my-app
├─ 5432  db          postgres:17                  http://localhost:5432
├─ 5173  frontend    vite (v5.4)                  http://localhost:5173
└─ 8000  api         uvicorn app:app              http://localhost:8000
```

And when you are done, stop the whole project — servers, watchers and workers:

```sh
sonar kill -g my-app
```

Examples below marked `# check` are executed against a fresh build by
`scripts/readme-check.sh` on every CI run.

## Commands

### `sonar list`

```sh
sonar list
sonar list --tree
sonar list --group my-app
sonar list --json
# check
```

```sh
sonar list --stats             # CPU, memory, threads, uptime, state
sonar list --health            # HTTP health checks
sonar list --filter docker     # only Docker ports
sonar list --sort name         # port | pid | name | type
sonar list -a                  # include desktop apps
sonar list -c port,process,group,cpu,mem
sonar list --host user@server  # scan a remote machine over SSH
```

Default columns are `port`, `process`, `group`, `container`, `image`,
`containerport`, `url`, where `process` shows the name you gave the port
(`sonar rename`), then the service name, then what was detected.

Available columns: `port`, `process`, `pid`, `type`, `url`, `group`, `cpu`,
`mem`, `threads`, `uptime`, `state`, `connections`, `health`, `latency`,
`container`, `image`, `containerport`, `compose`, `project`, `user`, `bind`,
`ip`.

Desktop apps and system services that happen to listen — Figma, Discord,
Spotify, ControlCenter, macOS `.app` bundles, `/System/Library/` daemons — are
hidden unless you pass `-a`.

### `sonar start`

Run a command as a named service in a group:

```sh
sonar start -- npm run dev
sonar start --group my-app --name frontend -- npm run dev
sonar start --port 5173 -- npm run dev        # expected port, before it binds
sonar start --detach --name api -- uv run uvicorn app:app
sonar start --list
```

Nothing has to be passed:

- **Group** — `--group`, else the `name` in the nearest `.sonar.yaml`, else the
  git root's directory name (a worktree becomes `repo@worktree`), else the name
  of the current directory.
- **Name** — `--name`, else the `.sonar.yaml` service whose `cmd` matches, else
  inferred from the command (`npm run dev` → `dev`, `uv run api` → `api`,
  `python -m uvicorn` → `uvicorn`, `./dev.sh` → `dev.sh`).
- **Port** — `--port` is a hint, not a binding: the run shows as `starting`
  until the port is actually listening, and the daemon uses it to match the
  process to the port.

The child inherits stdin, stdout, stderr, cwd and environment, plus
`SONAR_GROUP`, `SONAR_NAME` and `SONAR_RUN_ID`. It gets its own process group,
so `sonar kill` takes down the whole tree — a dev server with its watchers and
workers. Ctrl+C is forwarded, and sonar exits with the child's exit code.

`--detach` returns immediately and writes the output to
`~/.config/sonar/logs/<group>/<name>.log`. `--list` shows what sonar started
(`--json` for the machine-readable form):

```sh
sonar start --list
sonar start --detach --name demo --port 8123 -- sleep 5
sonar start --list --json
# check
```

### `.sonar.yaml`

A project names itself and its services in a `.sonar.yaml` at the repository
root. It is optional — sonar groups by git root without it — and it is meant to
be committed:

```yaml
name: my-app
services:
  - name: db
    cmd: docker compose up db
    port: 5432
    health: /
    description: Postgres 17
    icon: database
    color: "#4f8cc9"
  - name: api
    cmd: uv run uvicorn app:app --port 8000
    cwd: backend
    port: 8000
    health: /healthz
    depends_on: [db]
  - name: frontend
    cmd: npm run dev
    port: 5173
    depends_on: [api]
ports: [9229]        # ports that belong to this project without a service
```

- `name` — the group name. No slashes, no whitespace.
- `cmd`, `cwd`, `port` — how `sonar up` starts the service. `cwd` is relative to
  the file and may not escape its directory.
- `health` — an HTTP path the daemon polls while the service is up, so a
  service can be *running* but not yet *healthy*. It reports `ok`, `fail` or
  `unknown`, with the reason for a failure.
- `description`, `icon`, `color` — free-form metadata for the desktop app; sonar
  never infers them.
- `depends_on` — start order. Naming a service that is not in the file, or a
  cycle, is an error; an invalid file is reported once and never stops a scan.

`.sonar.yml` is read if that is how you spell it; `sonar init` always writes
`.sonar.yaml`. The daemon watches the projects it knows about and picks up
edits to the file without a restart. Every edit sonar makes — from the desktop
app, from `sonar groups add`, `rename` and `remove`, from an agent — goes
through the daemon, which re-renders the file from its own syntax tree, so
comments, key order and layout survive an edit that adds, renames or removes a
service just as they survive a metadata change. The one exception: extra spaces
lining a trailing comment up (`cmd: x     # note`) collapse to one, because the
YAML library keeps the comment but not its column.

### `sonar up`

```sh
sonar up                       # the .sonar.yaml at or above this directory
sonar up my-app                # a group by name
sonar up --only api,frontend
sonar up --json
```

Starts every service the group's `.sonar.yaml` declares, in `depends_on` order:
a service waits for the ports its dependencies declare before it is started, and
one that is already listening is skipped. Each runs detached in its own process
group, with its output in `~/.config/sonar/logs/<group>/<service>.log`.

```
  ✓ db        pid 41022  ~/.config/sonar/logs/my-app/db.log
  - api       already running
  ✓ frontend  pid 41108  ~/.config/sonar/logs/my-app/frontend.log

2 started, 1 already running
```

A service that fails to start is reported on its own line and makes the command
exit non-zero, whatever else came up. Stop them all again with
`sonar kill -g my-app`. `sonar up` needs the daemon and starts it if it is not
already running.

### `sonar groups` and `sonar init`

```sh
sonar init --dry-run
sonar init --service api:8000:/healthz --service web:5173
sonar groups
sonar groups --json

group=$(basename "$PWD")           # sonar init names the group after the directory
sonar groups add "$group" worker --port 9000 --cmd 'uv run worker' --depends-on api
sonar groups rename "$group" worker jobs
sonar groups remove "$group" jobs
# check
```

`sonar groups` lists every group sonar can see and where each name came from:
`manual` (you pinned it with `sonar assign`), `start` (a `sonar start` run),
`file` (a `.sonar.yaml`) or `auto` (the git root or the Compose project).
`sonar groups <name>` shows one group's ports and services, and the services
that are declared but not running.

`sonar init` writes a `.sonar.yaml` at the git root from what is listening right
now — desktop apps and ports below 1024 left out. It refuses to overwrite
without `--force`, and `--dry-run` prints the file instead of writing it.
`--merge` appends to a file that is already there instead of refusing, and
`--service name:port[:health]` — repeatable — writes the services you name
instead of the ones it found, keeping the command it guessed for a port you
kept. `--force` and `--merge` are mutually exclusive.

`sonar groups add <group> <name> --port N` appends a service to that group's
`.sonar.yaml`, with `--cmd`, `--cwd`, `--health`, `--description`, `--icon`,
`--color` and a repeatable `--depends-on` for the rest of it. `sonar groups
rename <group> <old> <new>` renames one everywhere in the file, `depends_on`
references included, and `sonar groups remove <group> <name>` deletes one and
drops it from every `depends_on` that named it. All three refuse an edit that
would leave the file invalid — a duplicate name, a port another service already
claims, a service that is not there — and none of them writes a byte until the
whole edit is known to be good.

The daemon does the writing, which is why the file comes back with its comments
and key order intact whether the edit came from the CLI, the desktop app or an
agent. `sonar groups add`, `rename` and `remove` need the daemon and start it if
it is not already running; the three names are subcommands, so a group actually
called `add` is read with `sonar groups --json`.

### `sonar kill`

```sh
sonar kill 3000                            # SIGTERM, then SIGKILL after 5s
sonar kill 3000 5432 -f                    # SIGKILL both straight away
sonar kill 3000 --tree                     # the listener and everything below it
sonar kill --pid 12345 --tree              # by process id
sonar kill -g my-app                       # a whole group, confirms unless -y
sonar kill --all --filter docker -y        # every container publishing a port
sonar kill --all --project my-app          # one Compose project
sonar kill 3000 --ip 127.0.0.1             # one bind address of several
sonar kill --all --dry-run --json          # the plan for the whole machine
```

`--dry-run` takes any selector and changes nothing: it prints the actions the
kill would take, children first, and leaves everything running. End to end,
against a listener of your own:

```sh
sonar start --detach --name plan --port 8231 -- sonar map 3000 8231
sonar wait 8231
sonar kill 8231 --dry-run --json           # the plan; the mapping keeps running
sonar kill 8231 -y                         # and now for real
# check
```

A positional argument is read as a port, and as a pid only when nothing is
listening on that number. `-g` matches the resolved group, a legacy run tag or
id, and the Compose project, case-insensitively.

A process that ignores SIGTERM is sent SIGKILL once the port is still listening
after `--grace` (5s); `--no-escalate` turns that off. Children are signalled
before parents, so a tree comes down in order. Docker containers are stopped
with `docker stop` and never signalled. A listener started by `sonar start` is
always stopped together with its process group.

`--json` prints one row per process:
`{port, bind_address, pid, name, method, ok, error}`, where `method` is
`sigterm`, `sigkill`, `docker_stop`, `map_stop` or `none`. An empty sweep exits
0; an unknown group exits 1.

### `sonar map`

```sh
sonar map 6873 3002        # also serve the service on 6873 from port 3002
```

Runs a TCP proxy in the foreground until you stop it. `sonar kill` reports a
mapping it stopped as `map_stop`.

### `sonar rename`, `sonar assign`, `sonar history`

```sh
sonar rename 3000 storefront     # a name of your own, survives restarts
sonar rename 3000 --clear
sonar assign 3000 my-app         # pin a port to a group by hand
sonar assign 3000 --clear
sonar history                    # everything that came up, went down, restarted
sonar history 3000 --since 24h --limit 20
```

```sh
sonar history --since 1h
sonar history --json
# check
```

Names and pins are stored in sonar's database, keyed by the most specific thing
known about the port: the run (`run:<group>/<name>`), the container
(`docker:<project>/<service>`), the working directory, and the port number
last. A renamed dev server keeps its name across restarts; a name pinned to
port 3000 alone applies to whatever answers there. These three commands need
the daemon and start it if it is not running.

### Reading a port

```sh
sonar info 3000                            # command, user, bind, stats, health
sonar logs 3000                            # tail; docker logs for containers
sonar wait 5432 3000 --timeout 60s         # block until ready
sonar wait 5432 --http=/health             # wait for HTTP 200-399, not just TCP
sonar next 3000                            # first free port from 3000
sonar next 3000-3100 -n 3                  # three consecutive free ports
sonar graph                                # who is connected to whom
sonar graph --dot                          # Graphviz
sonar open 3000                            # open in the browser
sonar attach 3000                          # shell into the container, or TCP
sonar watch                                # live view
sonar watch --stats --notify
```

```sh
sonar next 3000
sonar next 3000-3100 -n 3 --json
sonar graph --json
sonar info --help
# check
```

`sonar wait` exits `0` (ready), `1` (timeout) or `2` (interrupted), which makes
it the thing to put between starting something and testing it:

```sh
docker compose up -d
sonar wait 5432 3000 --timeout 60s && npm run migrate && npm run test
```

**Daemon or direct scan.** Every read command asks the daemon if one is
running, because it already has the answer and does not have to fork `lsof`.
If none is running they scan directly and print one note on stderr saying so.
`sonar kill` follows the same rule: a reachable daemon does the killing, so it
rescans immediately and its next answer — and the port history — already knows
the port is gone. Neither reads nor kills start a daemon behind your back.
`--no-daemon` forces the direct scan silently and works on any command:

```sh
sonar list --no-daemon --json
# check
```

### `sonar host`

```sh
sonar host          # cpu, load, memory and disk of the machine sonar watches
sonar host --json
```

```sh
sonar host
# check
```

The daemon measures its own machine on the scan cadence and publishes it as the
`localhost` row of the snapshot's `hosts` collection: os and kernel, uptime, cpu
percent, load average, memory and the disk holding `/`. CPU percent is the work
done between two scans, so it is null until the daemon has scanned twice; a
figure a platform cannot produce — the load average on Windows, which has none —
is null rather than zero. Every host registered with `sonar remote add` joins
the same table with its own load. The command needs a running daemon: it is the
daemon that holds the previous sample a percentage is measured against.

GPUs are listed under their host with utilization and memory: from `ioreg` on
macOS (Apple silicon memory is unified, so only what the GPU holds is shown),
and from `nvidia-smi` or amdgpu's sysfs counters on Linux. The daemon reads them
every five seconds in the background, never on a command's path; `gpus` is null
where there is no source (Windows, Intel-only Linux) and `[]` on a machine with
no GPU.

### `sonar remote install`

```sh
sonar remote install deploy@203.0.113.7        # same version as this sonar
sonar remote install hetzner --version v0.6.0  # a Host from ~/.ssh/config
sonar remote install deploy@box --no-service   # the binary, no daemon
```

Puts sonar on a host you can already ssh to and starts its daemon there. The
release archive is downloaded and checksummed **on the remote host** — nothing
is copied from this machine — and the binary lands in `~/.local/bin/sonar`, so
none of it needs root. The daemon runs as a systemd user unit where the host
has one (`~/.config/systemd/user/sonar.service`), and detached where it does
not; `loginctl enable-linger` is printed as advice when the user session would
end at logout and take the daemon with it.

The version installed is the version of the sonar you ran it from, so the two
ends speak the same protocol. Running it again upgrades in place and restarts
the daemon, which is what makes an install and an update the same command.

The target goes to `ssh` untouched: a `Host` alias from `~/.ssh/config` works,
and so do the `ProxyJump`, `IdentityFile` and `Port` it sets. `--identity` and
`--ssh-arg` are there for the flags a config does not cover.

### `sonar remote`

```sh
sonar remote add deploy@203.0.113.7            # name taken from the target
sonar remote add hetzner deploy@203.0.113.7    # or given
sonar remote list                              # status, latency, version, load
sonar remote remove hetzner

sonar list --host hetzner                      # that host's ports
sonar list --host "*"                          # every host, with a HOST column
sonar info 3000 --host hetzner
```

A registered host runs the same daemon, and the daemon on this machine keeps
one SSH connection to it — `ssh <target> sonar daemon stdio` — and multiplexes
what it reports into the state every client already reads. Nothing new listens
anywhere: the remote daemon's socket stays private to the SSH user, and clients
never speak SSH themselves.

Every row now carries the host it came from. Local rows say `localhost` and
keep the keys they always had, so nothing that reads sonar today changes;
remote rows say the registered name and are keyed `<host>/<port>:<bind>`, which
is what lets port 3000 on two machines be two rows. A subscriber sees only
localhost unless it asks for more (`state.subscribe {"hosts": ["*"]}`).

The target goes to `ssh` untouched, so `~/.ssh/config` aliases, `ProxyJump` and
identities all apply; `--ssh-arg`, `--identity` and `--port` cover what a config
does not. sonar stores no password and no key. A host that goes away keeps its
row and its status while the daemon retries, backing off from one second to
thirty for as long as it stays registered.

`--host` also still takes a bare `user@host` that sonar knows nothing about: it
falls back to the agentless `ssh` + `ss`/`lsof` scan and prints a hint to
`sonar remote install`.

#### Acting on another machine

Every write takes `--host` too, and does there exactly what it does here:

```sh
sonar kill 3000 --host hetzner                 # stop a port on that machine
sonar kill -g api --host hetzner               # a whole group of its services
sonar kill-all --filter docker --host hetzner  # its containers
sonar up api --host hetzner                    # start a group from its .sonar.yaml
sonar logs 3000 --host hetzner                 # tail its output here
sonar rename 3000 storefront --host hetzner    # its name, in its database
sonar assign 3000 storefront --host hetzner
```

The local daemon forwards the call over that host's bridge and hands back what
the remote daemon answered, in the same envelope a local call returns — every
result row says which host it happened on, and a kill's `affected` carries the
`<host>/<port>:<bind>` keys the stream uses for those rows. A streaming command
streams: `sonar up --host` prints each service as the far side starts it, and
Ctrl-C stops the remote work rather than just this terminal.

Because a row's key already names its host, a client can hand one straight back
as a selector — `{"key": "hetzner/3000:127.0.0.1"}` is the whole of a selector,
host included. One call acts on one machine; naming two is an error rather than
half a kill on each.

Two things stay local. `sonar attach` puts *this* terminal in front of a
process, so it refuses `--host` and says to ssh over and attach there. And an
agent session is state this daemon holds, so `sonar kill --session` has no
remote form. Everything else needs the daemon running here — it is where the
connection to the other machine lives — and says so instead of quietly scanning
this machine instead.

`sonar up --host` needs the group named: the `.sonar.yaml` at your working
directory is a path on this machine, and it is the remote daemon that reads the
file and starts the services.

### The daemon

One background process scans ports, resolves groups, polls health, keeps the
database and streams changes to whoever is subscribed — the CLI, the desktop
app, and editors.

```sh
sonar serve                  # in the foreground
sonar serve --detach         # in the background
sonar daemon status          # pid, uptime, subscribers, scans, intervals
sonar daemon path            # the socket it listens on
sonar daemon log -n 50 -f    # what it is doing
sonar daemon restart
sonar daemon stop
```

```sh
sonar daemon path
sonar daemon status --json
sonar daemon log -n 5
# check
```

| What | Where |
|---|---|
| Socket | `$XDG_RUNTIME_DIR/sonar/daemon.sock`, else `~/.config/sonar/daemon.sock`; `\\.\pipe\sonar` on Windows |
| Database | `~/.config/sonar/sonar.db` (`SONAR_DB` overrides) |
| Daemon log | `~/.config/sonar/daemon.log`, rotated at 5 MiB, three kept |
| Run logs | `~/.config/sonar/logs/<group>/<service>.log` |
| Config | `~/.config/sonar/config.yaml` |

`SONAR_SOCKET` overrides the socket path everywhere, for both the daemon and
its clients — useful for a second isolated instance. The socket is created
0600 in a 0700 directory, so only you can talk to it. Only one daemon runs at a
time; a socket left behind by a crash is cleaned up on the next start.

The daemon stops on its own after 30 minutes with no clients and no
subscribers. Set `daemon.idle_timeout` in the config file to change that, or
`0` to keep it running.

Ports are scanned every 2 seconds while something is changing; when nothing is,
the scanner slows itself down to 5 seconds with a subscriber connected and 10
seconds without one. `daemon.scan_interval` moves that base — minimum 1s — and
both ceilings scale with it, so raising it to `5s` backs off to 12.5s and 25s
rather than pinning the curve at the old caps. `daemon.stats_interval` is the
separate cadence at which cpu, memory and the host load strip refresh while
something is subscribed. Both are read when the daemon starts: edit the file,
then `sonar daemon restart`. `sonar daemon status` prints the values in
effect (`scan base`, `stats tick`) next to the adaptive interval the scanner is
on right now.

A subscriber that asks for `include: ["health"]` makes the daemon probe **every
listening port** on a slower cadence, not only the services that declare a
`health:` path — those are polled on every tick and reach every subscriber
whether or not health was asked for.

### Configuration

`~/.config/sonar/config.yaml` is optional; flags always win.

```sh
sonar config path
sonar config init
# check
```

```sh
sonar config edit       # open it in $EDITOR
```

```yaml
list:
  columns: [port, process, group, container, image, containerport, url]
  sort: port            # port | pid | name | type
  filter: ""            # docker | user | system | "" (all)
  all: false            # include desktop apps by default
daemon:
  idle_timeout: 30m     # 0 keeps the daemon running
  log_level: info       # debug | info | warn | error
  scan_interval: 2s     # base port-scan cadence, minimum 1s
  stats_interval: 1s    # cpu/memory refresh while subscribed, minimum 250ms
color: true
services:               # label custom/unknown ports
  9000: php-fpm
  5050: my-dashboard
```

Invalid values are ignored with a warning and sonar carries on with defaults.
Environment overrides that have no config key: `SONAR_DB`, `SONAR_SOCKET`,
`SONAR_NO_HINTS=1` to silence the migration notices below, and
`SONAR_NO_AUTOSTART=1` to stop any sonar client from starting a daemon it did
not find — useful in CI, where a build should never leave a process behind.

sonar's own test suite sets `SONAR_NO_AUTOSTART=1` for every test binary and,
after the run, looks for a daemon that outlived it. That gate only claims a
`serve` started from the run's private temp root, so two suites running side by
side on one machine leave each other's daemons alone;
`SONAR_TESTENV_GATE_ALL=1` widens it back to every `sonar serve` anywhere under
the temp directory, which is what a CI runner that owns the whole machine
wants.

### Agents: MCP, skills and hooks

```sh
sonar install mcp --claude-code               # merge into <git root>/.mcp.json
sonar install mcp --cursor --scope user       # ~/.cursor/mcp.json
sonar install mcp --codex                     # codex mcp add
sonar install skills --claude-code            # the bundled sonar skill
sonar install hooks --claude-code             # optional, see below
```

```sh
sonar install mcp --generic --print
sonar install skills --print
sonar install hooks --print
# check
```

`install mcp` registers `{"command": "sonar", "args": ["mcp"]}` and leaves every
other server and key in the file alone; running it twice changes nothing, and
`--uninstall` removes exactly what sonar wrote.

`sonar mcp` is that server: a stdio MCP server built into the binary that gives
an agent the daemon's view of the machine. It reads with `list_ports` and
`inspect_port`, waits with `wait_for_port`, picks and reserves ports with
`next_free_port` and `claim_port`, and answers the rest of an agent's questions
with `tail_logs`, `health_check`, `dependency_graph`, `port_history` and
`list_sessions`; actions and resources come next. It starts a daemon if none is
running and reconnects on its own if one goes away; its logs go to stderr,
because stdout carries the protocol.

`install skills` writes the bundled skill, which teaches an agent to start
servers with `sonar start --`, to `sonar wait` instead of sleeping, and to
clean up what it started. `install hooks` adds two Claude Code hooks: one
exports `SONAR_SESSION` so everything a session starts is attributed to it, the
other suggests `sonar start --` when a bare dev server is about to run (it
advises, it never blocks). Both take `--scope project|user`, `--print` and
`--uninstall`.

### `sonar doctor`

One command that checks everything sonar depends on and says what to do about
whatever is wrong. It is what the desktop app runs during onboarding, and what
to run yourself when something is off.

```sh
sonar doctor                       # the table, and a one-line verdict
sonar doctor --json                # {ok, checks, version, daemon_version}
sonar doctor --only db_ok,tray     # just these
sonar doctor --only mcp_registered # a whole family
sonar doctor --project ~/code/api  # a project other than the working directory
sonar doctor --fix --yes           # apply the safe repairs, then check again
```

```sh
# check
sonar doctor --only daemon_reachable,daemon_protocol,socket_permissions,db_ok
sonar doctor --json --only config_parses | grep -q '"status": "ok"'
sonar doctor --only mcp_registered --project . > /dev/null
```

Every check reports `ok`, `warn`, `fail` or `skip`. `skip` means there was
nothing to look at — Cursor is not installed, the machine has no docker, the
socket is a named pipe on Windows — and never counts against you. The exit code
is 0 unless something **failed**, so `sonar doctor` belongs in a setup script.

| check | what it means |
| --- | --- |
| `cli_on_path` | the binary you ran is the one PATH resolves; names the shadowing install if not |
| `cli_version_current` | compared with the newest release, or `skip` when GitHub is not reachable in 2s |
| `config_parses` | your `config.yaml` loads; a syntax error is reported with a line, a column and a caret |
| `config_dir_writable` | the daemon can write its log, lock and database |
| `daemon_reachable` | something is listening on the socket |
| `daemon_version_matches` | the running daemon is the version of the CLI you are using |
| `daemon_protocol` | the daemon's protocol major matches this build's |
| `socket_permissions` | the socket is yours and 0600, in a 0700 directory (`skip` on Windows) |
| `db_ok` | the database opens, is at the newest schema, and how big it is |
| `mcp_registered.{claude_code,cursor,codex}` | sonar's MCP server is in that client's config |
| `skills_installed` | the bundled skill is installed and current |
| `hooks_installed` | the optional Claude Code hooks are installed |
| `project_config` | this project has a `.sonar.yaml` that loads |
| `docker` | the docker CLI is there and its daemon answers |
| `desktop_installed` | the desktop app is installed, and which version (`skip` on Windows) |
| `tray` | the superseded macOS `sonar-tray` binary is still around |

`--fix` applies only the repairs that are safe to make unattended, and asks
first unless you pass `--yes`: it moves an unparseable `config.yaml` to
`config.yaml.broken-<timestamp>` and writes a fresh template (nothing is ever
deleted), restarts a daemon that is not running, and runs the
`sonar install mcp|skills|hooks` command the check names — from the working
directory, the way you would type it, so run `--fix` inside the project you are
repairing rather than pointing `--project` at it. Then it checks again.
Anything it will not touch — a shadowing binary on PATH, a skill sonar did not
write — is left to you with the exact command in the `fix` column.

The desktop app calls the same checks over the daemon's `daemon.doctor` method
instead of shelling out. The daemon runs everything it can from its own
process; the three checks that are about the CLI binary you invoked
(`cli_on_path`, `cli_version_current`, `daemon_version_matches`) come back as
`skip` with a detail saying so.

### The desktop app

The Sonar app is the same picture in a window and in the menu bar or system
tray: groups down the side, ports in a grid with live stats and health, logs,
and the buttons for everything above. It talks to the same daemon, so the CLI
and the app never disagree. `sonar install desktop` installs it and `sonar
tray` launches it.

Until the app ships, macOS release tarballs still carry the old `sonar-tray`
menu bar binary, and `sonar tray` falls back to it when the app is not
installed.

### `sonar install desktop`

The app is in beta and is not signed by Apple yet, so the CLI installs it:

```sh
brew install raskrebs/sonar/sonar && sonar install desktop
```

That is the whole tester setup. Sonar fetches a manifest of published builds,
picks the one for your machine, checks its sha256 and its size, installs it,
and opens it.

**This is why the CLI does the download.** macOS attaches a quarantine
attribute to anything a *browser* saves, and Gatekeeper refuses to open a
quarantined app that Apple has not notarised. A file this CLI downloads never
gets the attribute in the first place, so the beta opens with no prompt and no
right-click-Open dance. Sonar neither sets nor strips quarantine attributes —
there is nothing to strip.

```sh
sonar install desktop                    # install and launch
sonar install desktop --no-launch        # install only
sonar install desktop --update           # update; does nothing if current
sonar install desktop --check            # exit 1 when an update is available
sonar install desktop --version 0.1.0-beta.1
sonar install desktop --force            # ask a running Sonar to quit first
sonar install desktop --json             # for scripts
```

The command needs no network to tell you what it does:

```sh
sonar install desktop --help | grep -- '--no-launch'
# check
```

Where it goes:

| | |
| --- | --- |
| macOS | `/Applications/Sonar.app`, or `~/Applications/Sonar.app` when the first is not writable (sonar never uses `sudo`) |
| Linux | `~/.local/opt/sonar-desktop/Sonar.AppImage`, plus a menu entry in `~/.local/share/applications` and a `sonar-desktop` link in `~/.local/bin` |
| Windows | not yet — the command says so and exits 1 |

`--dir` overrides the directory on both. On Linux, `--deb` installs the `.deb`
through `apt`/`dpkg` instead of the AppImage, where the release publishes one.

The install is atomic: the new app is unpacked beside the old one and swapped
in with a rename, so a failed download never leaves you without a working app.
If the app is open, sonar refuses rather than replacing a bundle underneath it;
`--force` asks it to quit and waits up to ten seconds.

`sonar install desktop` records `desktop.installed_version` and
`desktop.installed_path` in `~/.config/sonar/config.yaml`, which is how `sonar
tray` finds an app installed with `--dir` and how `sonar doctor`'s
`desktop_installed` check knows the version. Where the builds come from is
`desktop.download_base`, overridden by `SONAR_DESKTOP_BASE` and then by
`--base` — point them at your own build to test one.

### `sonar relay`

The relay is the server side of sonar: one small HTTP service, run by us for
the hosted app and published as `ghcr.io/raskrebs/sonar-relay` so you can run
your own. It has nothing to do with the local daemon — `sonar serve` watches
your ports, `sonar relay serve` answers HTTP for a fleet — and it ships in the
same binary only so there is one artefact to deploy.

Today it collects anonymous product telemetry: a batch of named events per
install, no paths, no hostnames, no URLs, refused at the door if a value even
looks like one. It is the same service that will later terminate exposed
tunnels and hold sign-in.

```sh
sonar relay serve --db ./relay.db --project-keys "$(openssl rand -hex 24)"
```

`docs/RELAY.md` has the routes, the exact validation rules, the storage schema
and a one-command deploy behind Caddy on any box with Docker.

## Moving from the old commands

The pre-group commands still work and print a single line on stderr saying what
replaced them. They go away one minor release from now. `SONAR_NO_HINTS=1`
silences the notices, and `--json` output never carries them.

| Old | New |
|---|---|
| `sonar run --tag X -- cmd` | `sonar start --group X -- cmd` |
| `sonar runs` | `sonar start --list` |
| `sonar list --tag X` | `sonar list --group X` |
| `sonar kill-all --filter docker` | `sonar kill --all --filter docker` |
| `sonar down X` | `sonar kill -g X` |
| `sonar profile create X` | `sonar init` |
| `sonar profile show X` | `sonar groups X` |
| `sonar up X` (checked a profile) | `sonar up X` now *starts* the group |
| `sonar tray` (Swift menu bar app) | `sonar tray` launches the desktop app |

Profiles were a per-machine snapshot of ports; `.sonar.yaml` is committed with
the project. Convert one and read it before you keep it — nothing is written
for you:

```sh
sonar profile list
# check
```

```sh
sonar profile export my-app > .sonar.yaml
```

A profile never recorded how a service starts, so the proposal has ports,
names and health paths, and you fill in `cmd`.

## Troubleshooting

**Something is wrong with the daemon.** `sonar daemon log -f` while you
reproduce it, and `sonar daemon status` for pid, uptime and scan count. Stop
it with `sonar daemon stop`; every read command keeps working without it.

**"daemon unavailable, using direct scan".** Nothing is listening on the
socket. That is normal — reads do not start a daemon. Run `sonar serve -d` if
you want one.

**A socket left over from a crash.** `sonar daemon path` shows it; starting a
daemon removes a stale one by itself. If a second daemon refuses to start while
the first is gone, `sonar daemon restart` clears the lock.

**Ports are missing from the list.** Processes owned by another user are
invisible without privileges; sonar says so under the table. Re-run with
`sudo sonar list` to see them. On Linux, `ss` must be installed
(`iproute2`); on Windows, `netstat` is used.

**A kill did nothing.** Docker containers are stopped through the Docker
daemon: check `docker ps`. A process that ignores SIGTERM needs `-f`, and one
supervised by something else (systemd, Compose `restart: always`) comes back
by design — stop the supervisor.

**Nothing works and you are not sure why.** `sonar doctor` checks the binary,
the config, the daemon, the database and every integration in one go, and
prints the command that fixes each thing it finds.

**Reporting a bug.** Include these, plus the last lines of `sonar daemon log`:

```sh
sonar version
sonar daemon status
sonar doctor --json
# check
```

## Supported platforms

- macOS (uses `lsof`)
- Linux (uses `ss`)
- Windows (uses `netstat`)

Grouping needs each process's working directory, and every platform now has
one: `/proc` on Linux, `lsof` on macOS, and on Windows a read of the process's
own PEB. So git-root groups, `project_root` and cwd-based names work the same
everywhere, and `sonar init` can propose a `.sonar.yaml` from what is listening
on any of the three.

The desktop app is narrower for now: `sonar install desktop` installs it on
macOS (Apple Silicon and Intel) and Linux (x86_64 and aarch64). On Windows the
command says the app is not available yet and exits 1.

The one gap is a 32-bit `sonar.exe` on 64-bit Windows: it cannot read a 64-bit
process's memory, so those ports come back without a working directory and fall
out of their git-root group. Use the 64-bit build — it reads 64-bit and 32-bit
processes alike. Elsewhere, a port whose process denies access (a service
running as another user, a protected system process) is simply left without a
working directory; the rest of the scan is unaffected.

## Contributors

Thanks to everyone who has contributed to sonar!

<a href="https://github.com/RasKrebs/sonar/graphs/contributors">
  <img src="https://stg.contrib.rocks/image?repo=RasKrebs/sonar" />
</a>
