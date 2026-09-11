package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/killer"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/profile"
	"github.com/spf13/cobra"
)

var (
	downYesFlag   bool
	downForceFlag bool
	downJSONFlag  bool
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
		"`sonar kill -g <group>` stops the ports and releases nothing.",
	Args: cobra.MaximumNArgs(1),
	RunE: downRun,
}

func init() {
	downCmd.Flags().BoolVarP(&downYesFlag, "yes", "y", false, "Skip the confirmation prompt (for the old profile form)")
	downCmd.Flags().BoolVarP(&downForceFlag, "force", "f", false, "Send SIGKILL instead of SIGTERM")
	downCmd.Flags().BoolVar(&downJSONFlag, "json", false, "Output as JSON")
	addHostFlag(downCmd, "Stop the group on a registered remote `host`")
	rootCmd.AddCommand(downCmd)
}

func downRun(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	params := rpc.GroupsKillParams{HostParams: hostParams(), Force: downForceFlag, Release: true}
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
	}

	c, err := connectForHostWrite(cmd.Context())
	if err != nil {
		return err
	}
	defer c.Close()

	snapshot, err := hostSnapshot(cmd.Context(), c)
	if err != nil {
		return cliError(err)
	}

	var env rpc.KillEnvelope
	if err := c.Call(cmd.Context(), "groups.kill", params, &env); err != nil {
		// `sonar down <profile>` was the old command. A name that is only a
		// profile still means that, with the notice saying what replaced it.
		var re *rpc.Error
		if len(args) == 1 && errors.As(err, &re) && re.Data.Code == "not_found" && hasProfile(args[0]) {
			return downProfile(cmd, args[0])
		}
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

// downProfile is the old `sonar down <profile>`: an alias for `sonar kill` over
// a profile's ports. It selects the targets and hands them to the same killer.
func downProfile(cmd *cobra.Command, name string) error {
	Hint(cmd, HintDownToKill(name))
	prof, err := profile.Load(name)
	if err != nil {
		return err
	}

	selectTargets := func(snapshot []ports.ListeningPort) ([]killer.Target, error) {
		var targets []killer.Target
		for _, entry := range prof.Ports {
			for _, p := range snapshot {
				if p.Port == entry.Port {
					targets = append(targets, killer.Target{Port: p.Port, BindAddress: p.BindAddress})
				}
			}
		}
		return targets, nil
	}

	if onRemoteHost() {
		fmt.Printf("Profile %s on %s:\n", display.Bold(prof.Name), display.Bold(remoteHostFlag))
		return killSweepThroughDaemon(cmd.Context(), selectTargets,
			killer.Options{Force: downForceFlag}, !downYesFlag, false)
	}

	snapshot := scanForKill()
	targets, _ := selectTargets(snapshot)
	if len(targets) == 0 {
		fmt.Println("No profile ports are currently running.")
		return nil
	}

	fmt.Printf("Profile %s:\n", display.Bold(prof.Name))
	opts := killer.Options{Force: downForceFlag, Ports: snapshot}
	return killRun(cmd.Context(), targets, snapshot, opts, !downYesFlag, false)
}
