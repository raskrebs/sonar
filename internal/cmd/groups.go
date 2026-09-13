package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/spf13/cobra"
)

var groupsJSONFlag bool

var groupsCmd = &cobra.Command{
	Use:   "groups [name]",
	Short: "List the groups ports resolve to, or inspect one",
	Long: "Groups are never created explicitly: a port belongs to one because a\n" +
		"pin, a `sonar start` run, a sonar.yaml, a Compose project or a git\n" +
		"checkout puts it there.",
	Args: cobra.MaximumNArgs(1),
	RunE: groupsRun,
}

func init() {
	groupsCmd.Flags().BoolVar(&groupsJSONFlag, "json", false, "Output as JSON")
	rootCmd.AddCommand(groupsCmd)
}

func groupsRun(cmd *cobra.Command, args []string) error {
	pp, gg, err := groupRows(cmd.Context())
	if err != nil {
		return err
	}

	if len(args) == 0 {
		if groupsJSONFlag {
			return writeJSON(gg)
		}
		display.RenderGroups(os.Stdout, gg)
		return nil
	}

	for _, g := range gg {
		if g.Name != args[0] {
			continue
		}
		if groupsJSONFlag {
			return writeJSON(g)
		}
		display.RenderGroup(os.Stdout, g, pp)
		return nil
	}
	return fmt.Errorf("no group named %q (run `sonar groups` to see them)", args[0])
}

// groupRows is the groups to render: the daemon's own when one is running, and
// a direct scan otherwise.
//
// The daemon's rows know things a scan from here cannot work out: the port it
// assigned a `port: auto` service, and how the last run of a service that is
// down ended. A daemon that cannot be reached is not an error — it is the
// no-daemon path, which is the one `--no-daemon` asks for outright.
func groupRows(ctx context.Context) ([]ports.ListeningPort, []state.Group, error) {
	if !noDaemonFlag {
		if c, err := dialDaemon(ctx); err == nil {
			defer c.Close()
			var snap state.Snapshot
			if err := c.Call(ctx, "state.snapshot", rpc.StateSnapshotParams{}, &snap); err == nil {
				return state.ToListeningAll(snap.Ports), snap.Groups, nil
			}
		}
	}
	return scanGroups()
}

// scanGroups runs a direct scan, resolves every port's group and builds the
// group collection. Configs that failed to load are reported on stderr and
// otherwise ignored, so a broken file never blocks the command.
func scanGroups() ([]ports.ListeningPort, []state.Group, error) {
	results, err := ports.Scan()
	if err != nil {
		return nil, nil, err
	}
	docker.EnrichPorts(results)
	ports.Enrich(results)
	results = excludeApps(results)

	resolved, index := groups.Attribute(results)
	reportInvalidConfigs(index)
	return results, groups.Groups(resolved, index), nil
}

func reportInvalidConfigs(index *groups.Index) {
	for _, bad := range index.Invalid() {
		fmt.Fprintf(os.Stderr, "warning: ignoring %s\n", bad.Err)
	}
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
