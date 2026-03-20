<div align="center">

```
███████╗ ██████╗ ███╗   ██╗ █████╗ ██████╗
██╔════╝██╔═══██╗████╗  ██║██╔══██╗██╔══██╗
███████╗██║   ██║██╔██╗ ██║███████║██████╔╝
╚════██║██║   ██║██║╚██╗██║██╔══██║██╔══██╗
███████║╚██████╔╝██║ ╚████║██║  ██║██║  ██║
╚══════╝ ╚═════╝ ╚═╝  ╚═══╝╚═╝  ╚═╝╚═╝  ╚═╝
```

Know what's running on your machine.

</div>

I got tired of running `lsof -iTCP -sTCP:LISTEN | grep ...` every time a port was already taken, then spending another minute figuring out if it was a Docker container or some orphaned dev server from another worktree. So I built sonar.

It shows everything listening on localhost, with Docker container names, Compose projects, resource usage, and clickable URLs. You can kill processes, tail logs, shell into containers, and more — all by port number.

```
$ sonar list
PORT   PROCESS                      CONTAINER                    IMAGE             CPORT   URL
1780   proxy (traefik:3.0)          my-app-proxy-1               traefik:3.0       80      http://localhost:1780
3000   next-server (v16.1.6)                                                               http://localhost:3000
5432   db (postgres:17)             my-app-db-1                  postgres:17       5432    http://localhost:5432
6873   frontend (frontend:latest)   my-app-frontend-1            frontend:latest   5173    http://localhost:6873
9700   backend (backend:latest)     my-app-backend-1             backend:latest    8000    http://localhost:9700

5 ports (4 docker, 1 user)
```

## Install

```sh
curl -sfL https://raw.githubusercontent.com/raskrebs/sonar/main/install.sh | bash
```

Downloads the latest binary to `~/.local/bin` and adds it to your PATH if needed. Restart your terminal or `source ~/.zshrc`.

Custom install location:

```sh
curl -sfL https://raw.githubusercontent.com/raskrebs/sonar/main/install.sh | SONAR_INSTALL_DIR=/usr/local/bin bash
```

Shell completions (tab-complete port numbers):

```sh
sonar completion zsh > "${fpath[1]}/_sonar"   # zsh
sonar completion bash > /etc/bash_completion.d/sonar  # bash
sonar completion fish | source                 # fish
```

## Usage

### List ports

```sh
sonar list                     # show all ports
sonar list --stats             # include CPU, memory, state, uptime
sonar list --filter docker     # only Docker ports
sonar list --sort name         # sort by process name
sonar list --json              # JSON output
sonar list -a                  # include desktop apps
sonar list -c port,cpu,mem,uptime,state  # custom columns
sonar list --health            # run HTTP health checks
sonar list --host user@server  # scan a remote machine via SSH
```

By default, sonar hides desktop apps and system services that listen on TCP ports but aren't relevant to development — things like Figma, Discord, Spotify, ControlCenter, AirPlay, and other macOS `.app` bundles and `/System/Library/` daemons. Use `-a` to include them.

Available columns: `port`, `process`, `pid`, `type`, `url`, `cpu`, `mem`, `threads`, `uptime`, `state`, `connections`, `health`, `latency`, `container`, `image`, `containerport`, `compose`, `project`, `user`, `bind`, `ip`

### Inspect a port

```sh
sonar info 3000
```

Shows everything about a port: full command, user, bind address, CPU/memory/threads, uptime, health check result, and Docker details if applicable.

### Kill processes

```sh
sonar kill 3000                            # SIGTERM
sonar kill 3000 -f                         # SIGKILL
sonar kill-all --filter docker             # stop all Docker containers
sonar kill-all --project my-app            # stop a Compose project
sonar kill-all --filter user -y            # skip confirmation
```

Docker containers are stopped with `docker stop` instead of sending signals.

### View logs

```sh
sonar logs 3000
```

For Docker containers, runs `docker logs -f`. For native processes, discovers log files via `lsof` and tails them. Falls back to macOS `log stream` or Linux `/proc/<pid>/fd`.

### Attach to a service

```sh
sonar attach 3000                          # shell into Docker container, or TCP connect
sonar attach 3000 --shell bash             # specific shell
```

### Watch for changes

```sh
sonar watch                                # poll every 2s, show diffs
sonar watch --stats                        # live resource stats (like docker stats)
sonar watch -i 500ms                       # faster polling
sonar watch --notify                       # desktop notifications when ports go up/down
sonar watch --host user@server             # watch a remote machine
```

### Dependency graph

```sh
sonar graph                                # show which services talk to each other
sonar graph --json                         # structured output
sonar graph --dot                          # Graphviz DOT format
```

Shows established connections between listening ports (e.g. your backend connecting to postgres).

### Profiles

Save a set of expected ports for a project, then check if they're all up or tear them down:

```sh
sonar profile create my-app                # snapshot current ports
sonar profile list                         # list saved profiles
sonar profile show my-app                  # show profile details
sonar up my-app                            # check which expected ports are running
sonar down my-app                          # stop all ports in the profile
```

### Port mapping

```sh
sonar map 6873 3002
```

Proxies traffic so the service on port 6873 is also available on port 3002.

### Other

```sh
sonar open 3000                            # open in browser
sonar tray                                 # menu bar app with live stats (macOS)
sonar --no-color                           # disable colors (also respects NO_COLOR env)
```

The `--stats` flag fetches per-process and per-container resource usage. For Docker containers, it uses the Docker Engine API for accurate per-container metrics. Without `--stats`, sonar returns instantly.

## Supported platforms

- macOS (uses `lsof`)
- Linux (uses `ss`)
