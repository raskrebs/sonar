```
  ███████╗ ██████╗ ███╗   ██╗ █████╗ ██████╗
  ██╔════╝██╔═══██╗████╗  ██║██╔══██╗██╔══██╗
  ███████╗██║   ██║██╔██╗ ██║███████║██████╔╝
  ╚════██║██║   ██║██║╚██╗██║██╔══██║██╔══██╗
  ███████║╚██████╔╝██║ ╚████║██║  ██║██║  ██║
  ╚══════╝ ╚═════╝ ╚═╝  ╚═══╝╚═╝  ╚═╝╚═╝  ╚═╝
```

A developer tool for inspecting local open ports. Resolves Docker container names, shows clickable URLs, and provides filtering, sorting, and real-time monitoring. Supports local port mapping.

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

## Features

- Docker-aware — resolves container names, images, compose services, and port mappings
- Colored output with clickable URLs
- Hides desktop apps (Figma, Discord, etc.) by default
- Configurable columns, filtering, and sorting
- JSON output for scripting
- Log streaming and process attach for Docker and native services
- Port forwarding, real-time watch mode, and more

## Install

Requires [Go](https://go.dev/dl/).

```sh
git clone https://github.com/rkrebs/sonar.git
cd sonar
./install.sh
```

This builds the binary and adds `~/.sonar/bin` to your PATH. Restart your terminal, open a new terminal window or run `source ~/.zshrc` to start using it.

To customize the install location:

```sh
SONAR_INSTALL_DIR=/usr/local/bin ./install.sh
```

## Usage

### List ports

```sh
sonar list                     # show all ports
sonar list --filter docker     # only Docker ports
sonar list --sort name         # sort by process name
sonar list --json              # JSON output
sonar list -a                  # include desktop apps
sonar list -c port,compose,image,url  # custom columns
```

Available columns: `port`, `process`, `pid`, `type`, `url`, `container`, `image`, `containerport`, `compose`, `project`, `user`, `bind`, `ip`

### Inspect a port

```sh
sonar info 6873
```

Shows full details: command, user, bind address, Docker container/image/compose info.

### Open in browser

```sh
sonar open 3000
```

### Watch for changes

```sh
sonar watch              # poll every 2s
sonar watch -i 500ms     # poll every 500ms
```

Shows the initial table, then prints diffs as ports come and go.

### View logs

```sh
sonar logs 3000          # stream logs from a process
sonar logs 3000 -f=false # print recent logs without following
```

For Docker containers, streams `docker logs`. For native processes, discovers log files via `lsof` and tails them. Falls back to macOS `log stream` or Linux `/proc/<pid>/fd`.

### Attach to a service

```sh
sonar attach 6873              # interactive shell (Docker) or TCP connection
sonar attach 6873 --shell bash # use a specific shell for Docker exec
```

For Docker containers, opens an interactive shell inside the container. For other services, opens a raw TCP connection to the port.

### Port mapping

```sh
sonar map 6873 3002
```

Makes the service on port 6873 available on port 3002. Useful for accessing Docker services on a friendlier port.

### Kill a process

```sh
sonar kill 3000           # send SIGTERM
sonar kill 3000 -f        # send SIGKILL
```

Warns if the port belongs to a Docker container and suggests `docker stop` instead.

### Kill multiple processes

```sh
sonar kill-all --filter docker              # kill all Docker port processes
sonar kill-all --project my-app             # kill by Compose project
sonar kill-all --filter user -y             # skip confirmation
```

### Global flags

```sh
sonar --no-color           # disable colored output (also respects NO_COLOR env)
```

## Supported platforms

- macOS (uses `lsof`)
- Linux (uses `ss`)
