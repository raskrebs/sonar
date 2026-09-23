package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/spf13/cobra"
)

var (
	downForceFlag bool
	downJSONFlag  bool
	downOnlyFlag  []string
)

// downCmd is the other half of `sonar start`: it stops a project and gives back
// the ports sonar picked for it.
var downCmd = &cobra.Command{
	Use:   "down [group]",
	Short: "Stop a project's services and release the ports sonar picked for them",
	Long: "Stop every service of the project in the nearest sonar.yaml, or of the\n" +
		"group named: every port it listens on, and every service sonar started\n" +
		"for it that holds no port. The ports sonar claimed for its `port: auto`\n" +
		"services are released.\n\n" +
		"--only narrows all of that to the named services, the way\n" +
		"`sonar up --only` starts only some.\n\n" +
		"`sonar kill -g <group>` stops the ports and releases nothing.",
	Args: cobra.MaximumNArgs(1),
	RunE: downRun,
}

func init() {
	downCmd.Flags().BoolVarP(&downForceFlag, "force", "f", false, "Send SIGKILL instead of SIGTERM")
	downCmd.Flags().BoolVar(&downJSONFlag, "json", false, "Output as JSON")
	downCmd.Flags().StringSliceVar(&downOnlyFlag, "only", nil, "Stop only these services (comma separated)")
	addHostFlag(downCmd, "Stop the group on a registered remote `host`")
	rootCmd.AddCommand(downCmd)
}

func downRun(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	// An empty --only would reach the daemon as no only at all, and stop
	// everything the caller meant to narrow.
	if cmd.Flags().Changed("only") && len(downOnlyFlag) == 0 {
		return fmt.Errorf("--only needs at least one service name")
	}
	params := rpc.GroupsKillParams{HostParams: hostParams(), Force: downForceFlag, Release: true, Only: downOnlyFlag}
	var local *groups.Config
	if len(args) == 1 {
		params.Name = strings.TrimSpace(args[0])
	} else {
		if onRemoteHost() {
			return fmt.Errorf("name the group to stop on %s: `sonar down <group> --host %s`",
				remoteHostFlag, remoteHostFlag)
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfg, err := nearestConfig(wd)
		if err != nil {
			return err
		}
		path := cfg.Path
		params.ConfigPath = &path
		local = cfg
	}

	c, err := connectForHostWrite(cmd.Context())
	if err != nil {
		return err
	}
	defer c.Close()

	if err := requireConfigSupport(c, local); err != nil {
		return err
	}
	if len(downOnlyFlag) > 0 {
		if err := requireKillOnlySupport(c); err != nil {
			return err
		}
	}

	snapshot, err := hostSnapshot(cmd.Context(), c)
	if err != nil {
		return cliError(err)
	}

	var env rpc.KillEnvelope
	if err := c.Call(cmd.Context(), "groups.kill", params, &env); err != nil {
		return cliError(err)
	}
	if downJSONFlag {
		return writeJSON(env)
	}

	var reportErr error
	if len(env.Results) == 0 {
		fmt.Println("Nothing was running.")
	} else {
		reportErr = reportKill(os.Stdout, env.Results, snapshot, false, false)
	}
	if env.Released > 0 {
		fmt.Println(display.Dim(fmt.Sprintf("released %d claimed %s", env.Released, pluralWord(env.Released, "port"))))
	}
	return reportErr
}

// requireKillOnlySupport refuses a daemon whose `groups.kill` predates `only`,
// the way requireEnvSupport refuses one that predates `groups.env`: such a
// daemon drops the field it does not know and stops the whole group.
func requireKillOnlySupport(c *client.Client) error {
	hello := c.Hello()
	for _, capability := range hello.Capabilities {
		if capability == rpc.CapabilityKillOnly {
			return nil
		}
	}
	version := hello.DaemonVersion
	if version == "" {
		version = "an older version"
	}
	return fmt.Errorf("the running daemon (%s) does not know `sonar down --only`\nhint: restart it with `sonar daemon restart`", version)
}
