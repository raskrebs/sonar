package cmd

import (
	"fmt"
	"runtime"
	"strconv"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var logsFollow bool

var logsCmd = &cobra.Command{
	Use:               "logs <port>",
	Short:             "Attach to a process and view its log output",
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

		// Enrich to get Docker info and full command
		enriched := []ports.ListeningPort{*lp}
		docker.EnrichPorts(enriched)
		ports.Enrich(enriched)
		*lp = enriched[0]

		fmt.Printf("%s %s (PID %s)\n\n",
			display.Dim("Attaching to"),
			display.Bold(lp.DisplayName()),
			display.Cyan(fmt.Sprintf("%d", lp.PID)))

		// Docker containers: use docker logs
		if lp.Type == ports.PortTypeDocker && lp.DockerContainer != "" {
			return execDockerLogs(lp.DockerContainer)
		}

		// Windows: log discovery is not supported
		if runtime.GOOS == "windows" {
			return fmt.Errorf("log viewing is not supported on Windows for non-Docker processes")
		}

		// Regular processes: find log sources via lsof
		sources := ports.FindLogSources(lp.PID)
		if len(sources) > 0 {
			return tailLogSources(sources)
		}

		// Fallback: macOS log stream
		if ports.SupportsLogStream() {
			fmt.Println(display.Dim("No log files found, falling back to system log stream..."))
			fmt.Println()
			return execLogStream(lp.PID)
		}

		// Linux fallback: try /proc/<pid>/fd/1
		return tailProcFD(lp.PID)
	},
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", true, "Follow log output (stream continuously)")
	rootCmd.AddCommand(logsCmd)
}

