package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/profile"
	"github.com/spf13/cobra"

	// The daemon serves groups.start from this package's init(); `sonar serve`
	// runs in this binary, so it has to be linked in.
	_ "github.com/raskrebs/sonar/internal/daemon/groupstart"
)

var (
	upOnly []string
	upJSON bool
)

var upCmd = &cobra.Command{
	Use:   "up [group]",
	Short: "Start a group's services from its sonar.yaml",
	Long: "Start every service the group's sonar.yaml declares, in depends_on\n" +
		"order: a service waits for the ports its dependencies declare before it\n" +
		"is started, and services that are already running are skipped.\n\n" +
		"Each service runs detached in its own process group, with stdout and\n" +
		"stderr in ~/.config/sonar/logs/<group>/<service>.log. Stop them all\n" +
		"again with `sonar kill -g <group>`.\n\n" +
		"With no argument the group comes from the sonar.yaml at or above the\n" +
		"current directory.",
	Args: cobra.MaximumNArgs(1),
	RunE: upRun,
}

func init() {
	upCmd.Flags().StringSliceVar(&upOnly, "only", nil, "Start only these services (comma separated)")
	upCmd.Flags().BoolVar(&upJSON, "json", false, "Output as JSON")
	addHostFlag(upCmd, "Start the group on a registered remote `host` instead of this machine")
	rootCmd.AddCommand(upCmd)
}

func upRun(cmd *cobra.Command, args []string) error {
	params, err := upParams(args)
	if err != nil {
		return err
	}

	c, err := connectForHostWrite(cmd.Context())
	if err != nil {
		return err
	}
	defer c.Close()

	var start rpc.GroupsStartResult
	stream, err := c.Stream(cmd.Context(), "groups.start", params, &start)
	if err != nil {
		return upError(cmd, args, err)
	}
	defer stream.Close()

	return consumeStart(stream)
}

// upParams turns the command line into groups.start params: a name when one was
// given, the config at or above the working directory otherwise.
func upParams(args []string) (rpc.GroupsStartParams, error) {
	params := rpc.GroupsStartParams{HostParams: hostParams(), Only: upOnly}
	if !onRemoteHost() {
		// The services run as if started from this shell. A remote host gets
		// nothing: this machine's PATH means nothing on another one.
		params.Env = callerEnv()
	}
	if len(args) == 1 {
		name := strings.TrimSpace(args[0])
		params.Name = &name
		return params, nil
	}
	if onRemoteHost() {
		// The config path below is a path on this machine, and the remote
		// daemon resolves groups against its own sonar.yaml files. Naming the
		// group is the only thing that can mean the same on both sides.
		return params, fmt.Errorf("name the group to start on %s: `sonar up <group> --host %s`",
			remoteHostFlag, remoteHostFlag)
	}

	wd, err := os.Getwd()
	if err != nil {
		return params, fmt.Errorf("resolving the working directory: %w", err)
	}
	index := groups.NewIndex()
	index.Observe(wd)
	cfg := index.Nearest(wd)
	if cfg == nil {
		// A file that is there but broken is a different problem from no file
		// at all, and saying so is the difference between a two-second fix and
		// a puzzled `ls -a`.
		if bad := index.Invalid(); len(bad) > 0 {
			return params, fmt.Errorf("%s cannot be used: %w", groups.ConfigName, bad[0].Err)
		}
		return params, fmt.Errorf("no %s at or above %s\nhint: `sonar init` writes one, or name a group: `sonar up <group>`",
			groups.ConfigName, shortPath(wd))
	}
	params.ConfigPath = &cfg.Path
	return params, nil
}

// consumeStart prints one line per service as the daemon reports it, then the
// summary. It exits non-zero when any service failed to start (spec, "Error
// handling": a partial failure is still a failure).
func consumeStart(stream *client.Stream) error {
	var chunks []rpc.GroupsStartChunk

	for chunk := range stream.Chunks() {
		var c rpc.GroupsStartChunk
		if err := json.Unmarshal(chunk, &c); err != nil {
			continue
		}
		chunks = append(chunks, c)
		if !upJSON {
			printStartChunk(c)
		}
	}

	end := <-stream.End()
	if end.Err != nil {
		return daemonError(end.Err)
	}
	var summary rpc.GroupsStartEnd
	if err := end.Decode(&summary); err != nil {
		return err
	}

	if upJSON {
		if err := writeJSON(struct {
			Services []rpc.GroupsStartChunk `json:"services"`
			rpc.GroupsStartEnd
		}{Services: chunks, GroupsStartEnd: summary}); err != nil {
			return err
		}
	} else {
		printStartSummary(summary)
	}
	if len(summary.Errors) > 0 {
		return errSilent
	}
	return nil
}

func printStartChunk(c rpc.GroupsStartChunk) {
	switch {
	case c.Error != "":
		fmt.Printf("  %s %s  %s\n", display.Red("x"), display.Bold(c.Service), display.Dim(c.Error))
	case c.Skipped:
		reason := c.Reason
		if reason == "" {
			reason = "already running"
		}
		fmt.Printf("  %s %s  %s\n", display.Dim("-"), display.Bold(c.Service), display.Dim(reason))
	default:
		where := fmt.Sprintf("pid %d  %s", c.PID, shortPath(c.LogPath))
		if c.Port > 0 {
			where = fmt.Sprintf("port %d  %s", c.Port, where)
		}
		fmt.Printf("  %s %s  %s\n", display.Green("✓"), display.Bold(c.Service), display.Dim(where))
	}
}

func printStartSummary(end rpc.GroupsStartEnd) {
	parts := []string{fmt.Sprintf("%d started", len(end.Started))}
	if len(end.Skipped) > 0 {
		parts = append(parts, fmt.Sprintf("%d already running", len(end.Skipped)))
	}
	if len(end.Errors) > 0 {
		parts = append(parts, display.Red(fmt.Sprintf("%d failed", len(end.Errors))))
	}
	fmt.Printf("\n%s\n", display.Dim(strings.Join(parts, ", ")))
}

// upError adds the migration notice for the old `sonar up <profile>`: profiles
// are gone from this command, and someone whose muscle memory still types it
// should be told where they went rather than just "no group".
//
// It goes through Hint, the one notice mechanism the aliases share (§23), so it
// is a single stderr line, printed at most once, and silenced by --json and by
// SONAR_NO_HINTS like every other migration notice.
func upError(cmd *cobra.Command, args []string, err error) error {
	out := daemonError(err)
	var re *rpc.Error
	if len(args) != 1 || !errors.As(err, &re) || re.Data.Code != "not_found" {
		return out
	}
	if hasProfile(args[0]) {
		Hint(cmd, HintUpProfile(args[0]))
	}
	return out
}

// hasProfile reports whether a name still exists as a profile. An unreadable
// profile directory is simply "no profile": the notice is a courtesy, not a
// reason to fail differently.
func hasProfile(name string) bool {
	names, err := profile.List()
	if err != nil {
		return false
	}
	return slices.Contains(names, name)
}

// shortPath renders a path under the home directory as ~/….
func shortPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || path == "" || !strings.HasPrefix(path, home) {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return path
	}
	return filepath.Join("~", rel)
}
