package docker

import (
	"context"
	"os/exec"
	"time"
)

// CLITimeout bounds one `docker` invocation.
//
// The docker CLI talks to a daemon over a socket and has no timeout of its
// own: when Docker Desktop is starting, paused or wedged, `docker stats` does
// not fail, it waits. Ten seconds is far longer than a healthy
// `docker stats --no-stream` (about two) and short enough that a wedged daemon
// degrades to "no container data" instead of a hung command (contract §44).
//
// The daemon's scans no longer pay it: they read the container list a Watcher
// keeps in the background (step 5A.8). It still bounds the one-shot CLI paths
// and explicit actions such as `docker stop`.
const CLITimeout = 10 * time.Second

// waitDelay bounds how long a killed `docker` may keep its output pipe open.
// A timed-out command is killed, but anything it started that inherited the
// pipe would otherwise keep Output waiting after the deadline.
const waitDelay = time.Second

// cli builds a `docker` command that cannot outlive CLITimeout. The returned
// stop must be called once the output has been read.
func cli(args ...string) (cmd *exec.Cmd, stop func()) {
	ctx, cancel := context.WithTimeout(context.Background(), CLITimeout)
	return command(ctx, args...), cancel
}

// command builds a `docker` command bound to ctx.
func command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = waitDelay
	return cmd
}

// output runs one `docker` command under CLITimeout and returns its stdout.
func output(args ...string) ([]byte, error) {
	cmd, stop := cli(args...)
	defer stop()
	return cmd.Output()
}
