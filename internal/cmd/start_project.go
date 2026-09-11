package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/runs"
	"github.com/spf13/cobra"
)

// exitPoll is how often the foreground `sonar start` checks whether the
// services it started are still running.
const exitPoll = 500 * time.Millisecond

// stopStartedTimeout bounds the kill a Ctrl+C sends. The killer's own escalation
// finishes well inside it.
const stopStartedTimeout = 30 * time.Second

// startProjectIfAsked runs `sonar start` for a project when that is what the
// arguments ask for, and reports whether it did.
func startProjectIfAsked(cmd *cobra.Command, cwd string, args []string) (bool, error) {
	req, err := classifyStart(cwd, args, cmd.ArgsLenAtDash())
	if err != nil {
		return true, err
	}
	if req == nil {
		return false, nil
	}
	if startGroup != "" || startName != "" || startPort != 0 {
		return true, fmt.Errorf("--group, --name and --port are for a single command; a project's services are named in %s",
			shortPath(req.cfg.Path))
	}
	return true, startProject(cmd, req)
}

// startProject starts a project's services through the daemon, exactly as
// `sonar up` does. With -d it returns once they are started; otherwise it
// stays in the foreground and follows them.
func startProject(cmd *cobra.Command, req *projectRequest) error {
	ctx := cmd.Context()

	// Caught before anything starts: a Ctrl+C while the first service is
	// coming up must still stop it.
	sigs := make(chan os.Signal, 2)
	if !startDetach {
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigs)
	}

	c, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	path := req.cfg.Path
	params := rpc.GroupsStartParams{ConfigPath: &path, Only: req.services, Env: callerEnv()}
	var res rpc.GroupsStartResult
	stream, err := c.Stream(ctx, "groups.start", params, &res)
	if err != nil {
		return daemonError(err)
	}
	defer stream.Close()

	if startDetach {
		return consumeStart(stream, startJSON)
	}
	return followProject(c, stream, req.cfg, sigs)
}

// followProject is the foreground `sonar start`: it prints each service as the
// daemon starts it, then follows the logs of the ones it started with their
// names in front, the way `docker compose up` does. Ctrl+C stops those
// services — and only those: one that was already running is left alone. It
// also returns once every service it started has exited.
func followProject(c *client.Client, stream *client.Stream, cfg *groups.Config, sigs <-chan os.Signal) error {
	width := 0
	colour := map[string]int{}
	for i, s := range cfg.Services {
		width = max(width, len(s.Name))
		colour[s.Name] = i
	}
	prefix := func(name string) string { return followPrefix(name, width, colour[name]) }

	out := &lineWriter{w: os.Stdout}
	var (
		started   []rpc.GroupsStartChunk
		followers []*logFollower
	)
	stopFollowing := func() {
		for _, f := range followers {
			f.Stop()
		}
	}

	chunks := stream.Chunks()
	for chunks != nil {
		select {
		case raw, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			var ch rpc.GroupsStartChunk
			if err := json.Unmarshal(raw, &ch); err != nil {
				continue
			}
			out.mu.Lock()
			printStartChunk(ch)
			out.mu.Unlock()
			if ch.PID <= 0 || ch.Error != "" || ch.Skipped {
				continue
			}
			started = append(started, ch)
			if ch.LogPath != "" {
				f := newLogFollower(out, prefix(ch.Service), ch.LogPath, ch.LogOffset)
				f.Start()
				followers = append(followers, f)
			}
		case <-sigs:
			ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
			_ = stream.Cancel(ctx)
			cancel()
			return stopStarted(c, started, stopFollowing, sigs)
		}
	}

	end := <-stream.End()
	var summary rpc.GroupsStartEnd
	failed := false
	switch {
	case end.Err != nil:
		failed = true
		if len(started) == 0 {
			stopFollowing()
			return daemonError(end.Err)
		}
		fmt.Fprintln(os.Stderr, daemonError(end.Err))
	case end.Decode(&summary) == nil:
		failed = len(summary.Errors) > 0
		out.mu.Lock()
		printStartSummary(summary)
		out.mu.Unlock()
	}

	if len(started) == 0 {
		stopFollowing()
		if failed {
			return errSilent
		}
		out.println(display.Dim("nothing new was started; `sonar logs <port>` follows a service that is running"))
		return nil
	}
	out.println(display.Dim(fmt.Sprintf("following the logs; Ctrl+C stops the %d %s started here",
		len(started), pluralWord(len(started), "service"))))

	alive := make(map[int]bool, len(started))
	for _, s := range started {
		alive[s.PID] = true
	}
	t := time.NewTicker(exitPoll)
	defer t.Stop()
	for {
		select {
		case <-sigs:
			return stopStarted(c, started, stopFollowing, sigs)
		case <-t.C:
			left := 0
			for _, s := range started {
				if !alive[s.PID] {
					continue
				}
				if !runs.PIDAlive(s.PID) {
					alive[s.PID] = false
					out.println(prefix(s.Service) + display.Dim("exited"))
					continue
				}
				left++
			}
			if left == 0 {
				stopFollowing()
				if failed {
					return errSilent
				}
				return nil
			}
		}
	}
}

// stopStarted stops the services this `sonar start` started, through the
// daemon, each with its whole process tree. The logs keep being followed while
// they shut down, so what a service prints on its way out is still shown. A
// second Ctrl+C stops waiting.
func stopStarted(c *client.Client, started []rpc.GroupsStartChunk, stopFollowing func(), sigs <-chan os.Signal) error {
	var targets []rpc.Selector
	for _, s := range started {
		if runs.PIDAlive(s.PID) {
			pid := s.PID
			targets = append(targets, rpc.Selector{PID: &pid})
		}
	}
	if len(targets) == 0 {
		stopFollowing()
		return nil
	}
	fmt.Fprintf(os.Stderr, "\nstopping %d %s…\n", len(targets), pluralWord(len(targets), "service"))

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), stopStartedTimeout)
		defer cancel()
		var env rpc.KillEnvelope
		done <- c.Call(ctx, "ports.kill", rpc.PortsKillParams{Targets: targets, Tree: true}, &env)
	}()

	select {
	case err := <-done:
		stopFollowing()
		if err != nil {
			return daemonError(err)
		}
		fmt.Fprintln(os.Stderr, display.Dim("stopped"))
		return nil
	case <-sigs:
		fmt.Fprintln(os.Stderr, "not waiting any longer; `sonar down` stops whatever is left")
		return errSilent
	}
}
