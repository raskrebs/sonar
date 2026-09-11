package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/runs"
	"github.com/raskrebs/sonar/internal/selfupdate"
	"github.com/raskrebs/sonar/internal/sessions"
	"github.com/raskrebs/sonar/internal/spawn"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/spf13/cobra"

	// The daemon serves runs.register/unregister/list/spawn from this package's
	// init(); `sonar serve` runs in this binary, so it has to be linked in.
	_ "github.com/raskrebs/sonar/internal/daemon/runsreg"
)

var (
	startGroup  string
	startName   string
	startPort   int
	startDetach bool
	startList   bool
	startJSON   bool

	// startSession is the agent session this invocation belongs to, detected
	// once in startRun and carried into whichever spawn path runs.
	startSession state.Session
)

// registerTimeout bounds the daemon round-trips around a run. A slow or absent
// daemon must never delay the command the user actually asked for.
const registerTimeout = 5 * time.Second

var startCmd = &cobra.Command{
	Use:   "start [--group <name>] [--name <name>] [--port <port>] [--detach] -- <command> [args...]",
	Short: "Start a command as a named service in a group",
	Long: "Start <command> and record it so sonar can attribute every port it (or\n" +
		"anything it spawns) opens to a group and a service name.\n\n" +
		"The group is --group, else the nearest sonar.yaml, else the git\n" +
		"checkout the command runs in, else the directory name. The name is\n" +
		"--name, else the matching sonar.yaml service, else inferred from the\n" +
		"command (`npm run dev` is `dev`).\n\n" +
		"The child runs in its own process group with SONAR_GROUP, SONAR_NAME\n" +
		"and SONAR_RUN_ID in its environment. Ctrl+C goes to the whole tree and\n" +
		"sonar exits with the command's own exit code.\n\n" +
		"Everything after -- is the command, passed through verbatim.",
	Args:                  cobra.ArbitraryArgs,
	DisableFlagsInUseLine: true,
	RunE:                  startRun,
}

func init() {
	startCmd.Flags().StringVar(&startGroup, "group", "", "Group to attribute this run to (default: sonar.yaml, git root, or directory name)")
	startCmd.Flags().StringVar(&startName, "name", "", "Service name for this run (default: inferred from the command)")
	startCmd.Flags().IntVar(&startPort, "port", 0, "Port this command is expected to bind; the run shows as starting until it does")
	startCmd.Flags().BoolVar(&startDetach, "detach", false, "Run in the background, logging to ~/.config/sonar/logs/<group>/<name>.log")
	startCmd.Flags().BoolVar(&startList, "list", false, "List the runs sonar started and exit")
	startCmd.Flags().BoolVar(&startJSON, "json", false, "Output as JSON (with --list)")
	rootCmd.AddCommand(startCmd)
}

func startRun(cmd *cobra.Command, args []string) error {
	if startList {
		return listRuns(cmd.Context())
	}
	if len(args) == 0 {
		return errors.New("no command given; usage: sonar start [flags] -- <command> [args...]")
	}
	if startPort < 0 || startPort > 65535 {
		return fmt.Errorf("--port %d is not a port number", startPort)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolving the working directory: %w", err)
	}
	res := spawn.Resolve(cwd, args, startGroup, startName)
	// The agent session is detected here, in the process the agent actually
	// spawned: the daemon's own environment is a service manager's, not an
	// agent's, so it could never detect this (spec 2 §3).
	session, _ := sessions.Capture(cwd, sessions.Options{})

	startSession = session

	if startDetach {
		return startDetached(cmd, args, cwd, res)
	}
	return startAttached(cmd, args, cwd, res)
}

// startAttached runs the command in the foreground: stdio passes through, the
// child owns its own process group, signals are forwarded to it and sonar exits
// with the child's code.
func startAttached(cmd *cobra.Command, argv []string, cwd string, res spawn.Resolution) error {
	// Catch the interrupts before the child exists: a `sonar start` line in a
	// dev.sh runs as a background job with SIGINT ignored, and installing a
	// handler here is what gives the child a working Ctrl+C again.
	fwd := spawn.CatchSignals()
	defer fwd.Stop()

	h, err := spawn.Spawn(cmd.Context(), spawn.Request{
		Argv:     argv,
		Cwd:      cwd,
		Group:    res.Group,
		Name:     res.Name,
		PortHint: startPort,
		Session:  startSession,
	})
	if err != nil {
		return err
	}
	fwd.Forward(h)

	daemonKnows := registerRun(h)
	defer unregisterRun(h.PID, daemonKnows)

	code, err := h.Wait()
	if err != nil {
		return fmt.Errorf("running %q: %w", argv[0], err)
	}
	if code != 0 {
		// Mirror the child's exit code without cobra printing usage over it.
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		unregisterRun(h.PID, daemonKnows)
		fwd.Stop()
		os.Exit(code)
	}
	return nil
}

// startDetached hands the run to the daemon so it is parented by something that
// outlives this shell, falling back to spawning it here when there is no daemon.
func startDetached(cmd *cobra.Command, argv []string, cwd string, res spawn.Resolution) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), registerTimeout)
	defer cancel()

	if c, err := connectDaemon(ctx); err == nil {
		defer c.Close()
		params := rpc.RunsSpawnParams{
			Argv:  argv,
			Cwd:   cwd,
			Group: &res.Group,
			Name:  &res.Name,
			// The CLI is the user: it may start commands anywhere.
			AllowOutsideHome: true,
		}
		if startSession.ID != "" {
			s := startSession
			params.Session = &s
		}
		if startPort > 0 {
			hint := startPort
			params.PortHint = &hint
		}
		var out rpc.RunsSpawnResult
		if err := c.Call(ctx, "runs.spawn", params, &out); err == nil {
			printStarted(res, out.PID, out.LogPath)
			return nil
		} else if !errors.Is(err, client.ErrNotRunning) {
			return err
		}
	}

	// No daemon: start it here anyway. The run is recorded in runs.json and
	// adopted by the next daemon that starts.
	h, err := spawn.Spawn(cmd.Context(), spawn.Request{
		Argv:     argv,
		Cwd:      cwd,
		Group:    res.Group,
		Name:     res.Name,
		PortHint: startPort,
		Session:  startSession,
		Detach:   true,
	})
	if err != nil {
		return err
	}
	registerRun(h)
	printStarted(res, h.PID, h.LogPath)
	return nil
}

func printStarted(res spawn.Resolution, pid int, logPath string) {
	fmt.Printf("%s %s (pid %d)\n",
		display.Dim("started"), display.Cyan(res.Group+"/"+res.Name), pid)
	if logPath != "" {
		fmt.Printf("%s %s\n", display.Dim("logs"), logPath)
	}
}

// registerRun records the run with the daemon, falling back to runs.json when
// there is none. It reports whether the daemon took it.
func registerRun(h *spawn.Handle) bool {
	ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
	defer cancel()

	c, err := connectDaemon(ctx)
	if err == nil {
		defer c.Close()
		params := rpc.RunsRegisterParams{
			PID:              h.PID,
			PPID:             h.PPID,
			Group:            h.Group,
			Name:             h.Name,
			Cmd:              h.Cmd,
			Cwd:              h.Cwd,
			StartedAt:        h.StartedAt.Format(time.RFC3339),
			ID:               &h.ID,
			AllowOutsideHome: true,
		}
		if h.Session.ID != "" {
			s := h.Session
			params.Session = &s
		}
		if h.PortHint > 0 {
			hint := h.PortHint
			params.PortHint = &hint
		}
		var out rpc.RunsRegisterResult
		if err := c.Call(ctx, "runs.register", params, &out); err == nil {
			return true
		}
	}

	if err := runs.Add(fallbackEntry(h)); err != nil {
		fmt.Fprintf(os.Stderr, "sonar: warning: could not record this run: %v\n", err)
	}
	return false
}

// unregisterRun removes the run again, wherever it was recorded.
func unregisterRun(pid int, daemonKnows bool) {
	if daemonKnows {
		ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
		defer cancel()
		if c, err := connectRunningDaemon(ctx); err == nil {
			defer c.Close()
			var out rpc.OKResult
			if err := c.Call(ctx, "runs.unregister", rpc.RunsUnregisterParams{PID: pid}, &out); err == nil {
				return
			}
		}
	}
	_ = runs.Remove(pid)
}

// fallbackEntry is the runs.json row for a run the daemon never saw. Tag holds
// the group so an older `sonar list` still attributes the ports.
func fallbackEntry(h *spawn.Handle) runs.Entry {
	return runs.Entry{
		PID:       h.PID,
		Tag:       h.Group,
		ID:        h.ID,
		Cmd:       h.Cmd,
		StartedAt: h.StartedAt.Format(time.RFC3339),
		Group:     h.Group,
		Name:      h.Name,
		Cwd:       h.Cwd,
		PPID:      h.PPID,
		PortHint:  h.PortHint,
	}
}

// connectDaemon dials the daemon, starting one if needed: a run has to be
// registered somewhere that outlives the command.
func connectDaemon(ctx context.Context) (*client.Client, error) {
	return client.Connect(ctx, client.ClientInfo{Name: "cli", Version: selfupdate.Version})
}

// connectRunningDaemon dials without autostarting: cleaning up a run is no
// reason to start a daemon.
func connectRunningDaemon(ctx context.Context) (*client.Client, error) {
	return client.Dial(ctx, client.ClientInfo{Name: "cli", Version: selfupdate.Version})
}

// listRuns is `sonar start --list`: the daemon's registry when there is one,
// runs.json when there is not.
func listRuns(ctx context.Context) error {
	rows, err := runRows(ctx)
	if err != nil {
		return err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Group == rows[j].Group {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Group < rows[j].Group
	})

	if startJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"runs": rows})
	}
	if len(rows) == 0 {
		fmt.Println("No runs started by sonar.")
		return nil
	}

	fmt.Printf("%-6s %-10s %-18s %-14s %-9s %-12s %s\n",
		display.Bold("PID"), display.Bold("ID"), display.Bold("GROUP"),
		display.Bold("NAME"), display.Bold("STATUS"), display.Bold("PORTS"), display.Bold("CMD"))
	for _, r := range rows {
		fmt.Printf("%-6d %-10s %-18s %-14s %-9s %-12s %s\n",
			r.PID, r.ID, display.Cyan(r.Group), r.Name, r.Status, portList(r.Ports), r.Cmd)
	}
	return nil
}

// runRows reads runs.list from the daemon, or rebuilds the same rows from
// runs.json when no daemon is running.
func runRows(ctx context.Context) ([]rpc.RunRecord, error) {
	c, err := connectRunningDaemon(ctx)
	if err == nil {
		defer c.Close()
		var out rpc.RunsListResult
		if err := c.Call(ctx, "runs.list", rpc.Empty{}, &out); err == nil {
			return out.Runs, nil
		}
	} else if !errors.Is(err, client.ErrNotRunning) {
		return nil, err
	}

	fmt.Fprintln(os.Stderr, "note: daemon unavailable, reading the local run registry")
	held := scanRunPorts()
	var rows []rpc.RunRecord
	for _, e := range runs.Load().Active() {
		row := rpc.RunRecord{
			ID: e.ID, PID: e.PID, Group: e.GroupOf(), Name: e.NameOf(),
			Cmd: e.Cmd, Cwd: e.Cwd, StartedAt: e.StartedAt,
			Ports: held[e.PID], Status: "running",
		}
		if row.Ports == nil {
			row.Ports = []int{}
		}
		if e.PortHint > 0 {
			hint := e.PortHint
			row.PortHint = &hint
			if !slices.Contains(row.Ports, hint) {
				row.Status = "starting"
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// scanRunPorts maps a run's pid to the ports its process tree is listening on,
// the direct-scan equivalent of what the daemon reads from its last snapshot.
func scanRunPorts() map[int][]int {
	scan, err := ports.Scan()
	if err != nil {
		return nil
	}
	ports.Enrich(scan)
	out := map[int][]int{}
	for i := range scan {
		root := scan[i].RunRootPID
		if root <= 0 || slices.Contains(out[root], scan[i].Port) {
			continue
		}
		out[root] = append(out[root], scan[i].Port)
	}
	return out
}

func portList(ports []int) string {
	if len(ports) == 0 {
		return "-"
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, strconv.Itoa(p))
	}
	return strings.Join(out, ",")
}
