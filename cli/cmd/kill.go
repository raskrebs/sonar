package cmd

import (
	"fmt"
	"strconv"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var forceFlag bool

var killCmd = &cobra.Command{
	Use:               "kill <port>",
	Short:             "Kill the process listening on a given port",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePort,
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid port: %s", args[0])
		}

		lp, err := ports.FindByPort(port)
		if err != nil {
			return err
		}

		// Enrich with Docker info to detect containers
		enriched := []ports.ListeningPort{*lp}
		docker.EnrichPorts(enriched)
		*lp = enriched[0]

		if lp.Type == ports.PortTypeDocker {
			name := lp.DockerContainer
			if lp.DockerComposeService != "" {
				name = lp.DockerComposeService
			}
			fmt.Printf("Stopping Docker container %s on port %d\n",
				display.Bold(name), port)

			if err := docker.StopContainer(lp.DockerContainer); err != nil {
				return err
			}

			fmt.Printf("Freed %s\n", display.Underline(lp.URL()))
			return nil
		}

		sigName := "SIGTERM"
		if forceFlag {
			sigName = "SIGKILL"
		}
		fmt.Printf("Killing %s (PID %d) on port %d with %s\n",
			display.Bold(lp.DisplayName()), lp.PID, port, sigName)

		if err := ports.Kill(port, forceFlag); err != nil {
			return err
		}

		fmt.Printf("Freed %s\n", display.Underline(lp.URL()))
		return nil
	},
}

func init() {
	killCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Send SIGKILL instead of SIGTERM")
	rootCmd.AddCommand(killCmd)
}
