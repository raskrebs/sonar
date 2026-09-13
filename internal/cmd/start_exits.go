package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
)

// recentExits is the runs sonar started that have since ended, newest first.
// Only the daemon keeps them, so there are none without one.
func recentExits(ctx context.Context) []rpc.RunRecord {
	c, err := connectRunningDaemon(ctx)
	if err != nil {
		return nil
	}
	defer c.Close()
	var out rpc.RunsListResult
	if err := c.Call(ctx, "runs.list", rpc.Empty{}, &out); err != nil {
		return nil
	}
	return out.Exited
}

// printExits lists the runs that ended under the live ones.
func printExits(rows []rpc.RunRecord) {
	if len(rows) == 0 {
		return
	}
	fmt.Printf("\n%s\n", display.Bold("EXITED"))
	for _, r := range rows {
		fmt.Printf("  %-10s %-18s %-14s %-22s %s\n",
			r.ID, display.Cyan(r.Group), r.Name, exitWords(r), display.Dim(ago(r.ExitedAt)))
	}
}

// exitWords says how a run ended, in the words the whole CLI uses for it.
func exitWords(r rpc.RunRecord) string {
	code := 0
	if r.ExitCode != nil {
		code = *r.ExitCode
	}
	switch r.Reason {
	case "crashed":
		return display.Red(fmt.Sprintf("crashed (exit %d)", code))
	case "stopped":
		return display.Dim("stopped")
	default:
		return "exited"
	}
}

// ago renders an RFC 3339 stamp as how long ago it was.
func ago(stamp string) string {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// exitNote is how a service the foreground `sonar start` was following ended.
// The daemon reaped it, so it is the one that knows; it records the exit a
// moment after the process is gone, which is what the retries are for.
func exitNote(c *client.Client, pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
	defer cancel()
	for i := 0; i < 5; i++ {
		var out rpc.RunsListResult
		if err := c.Call(ctx, "runs.list", rpc.Empty{}, &out); err == nil {
			for _, r := range out.Exited {
				if r.PID == pid {
					return exitWords(r)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return display.Dim("exited")
}
