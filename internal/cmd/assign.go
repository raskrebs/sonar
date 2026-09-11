package cmd

import (
	"fmt"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/spf13/cobra"
)

var (
	assignClear bool
	assignPID   bool
	assignJSON  bool
)

var assignCmd = &cobra.Command{
	Use:   "assign <port|pid> [group]",
	Short: "Pin a port to a group by hand",
	Long: "A pin outranks every other way a group is decided — a `sonar start`\n" +
		"run, a sonar.yaml, a Compose project, the git checkout — and is\n" +
		"remembered per machine until you clear it.",
	Args:              cobra.RangeArgs(1, 2),
	ValidArgsFunction: completePort,
	RunE:              assignRun,
}

func init() {
	assignCmd.Flags().BoolVar(&assignClear, "clear", false, "Remove the pin and go back to the inferred group")
	assignCmd.Flags().BoolVar(&assignPID, "pid", false, "Read the argument as a pid instead of a port")
	assignCmd.Flags().String("ip", "", "Specify bind address when a port is bound to multiple IPs")
	assignCmd.Flags().BoolVar(&assignJSON, "json", false, "Output as JSON")
	addHostFlag(assignCmd, "Pin a port on a registered remote `host` instead of this machine")
	rootCmd.AddCommand(assignCmd)
}

func assignRun(cmd *cobra.Command, args []string) error {
	sel, err := selectorFrom(args[0], assignPID, cmd)
	if err != nil {
		return err
	}
	group, err := writeValue(args, assignClear, "group", "sonar assign 3000 storefront")
	if err != nil {
		return err
	}

	c, err := connectForHostWrite(cmd.Context())
	if err != nil {
		return err
	}
	defer c.Close()

	var res rpc.GroupsAssignResult
	if err := c.Call(cmd.Context(), "groups.assign",
		rpc.GroupsAssignParams{Selector: sel, Group: group}, &res); err != nil {
		return daemonError(err)
	}
	if assignJSON {
		return writeJSON(res)
	}
	if group == nil {
		fmt.Printf("cleared the group pin for %s\n", display.Bold(args[0]))
		return nil
	}
	fmt.Printf("%s is now in group %s\n", args[0], display.Bold(*group))
	return nil
}
