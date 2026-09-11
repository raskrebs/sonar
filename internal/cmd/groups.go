package cmd

import (
	"encoding/json"
	"fmt"
	"os"

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
	pp, gg, err := scanGroups()
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
