package cmd

import (
	"fmt"
	"os/exec"
	"strconv"

	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var attachShell string

var attachCmd = &cobra.Command{
	Use:   "attach <port>",
	Short: "Attach to a running service on a port",
	Long: `Attach to a running service on a port.

For Docker containers, opens an interactive shell inside the container.
For other services, opens a raw TCP connection to the port.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePort,
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid port: %s", args[0])
		}

		bindIP, _ := cmd.Flags().GetString("ip")

		lp, err := ports.FindByPort(port, bindIP)
		if err != nil {
			return err
		}

		// Enrich to get Docker info
		enriched := []ports.ListeningPort{*lp}
		docker.EnrichPorts(enriched)
		ports.Enrich(enriched)
		*lp = enriched[0]

		// Docker containers: exec into the container
		if lp.Type == ports.PortTypeDocker && lp.DockerContainer != "" {
			return execDockerShell(lp.DockerContainer)
		}

		// Non-Docker: open a raw TCP connection
		return execTCPConnect(port)
	},
}

func init() {
	attachCmd.Flags().StringVar(&attachShell, "shell", "", "Shell to use for Docker exec (default: auto-detect sh/bash)")
	attachCmd.Flags().String("ip", "", "Specify bind address when a port is bound to multiple IPs")
	rootCmd.AddCommand(attachCmd)
}

// detectContainerShell tries bash first, falls back to sh.
func detectContainerShell(container string) string {
	cmd := exec.Command("docker", "exec", container, "bash", "-c", "exit 0")
	if err := cmd.Run(); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}
