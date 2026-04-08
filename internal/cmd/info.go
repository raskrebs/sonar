package cmd

import (
	"fmt"
	"strconv"
	"time"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var infoCmd = &cobra.Command{
	Use:               "info <port>",
	Short:             "Show detailed information about a port",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePort,
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid port: %s", args[0])
		}

		infoHost, _ := cmd.Flags().GetString("host")
		bindIP, _ := cmd.Flags().GetString("ip")

		var lp *ports.ListeningPort
		if infoHost != "" {
			all, scanErr := ports.ScanRemote(infoHost)
			if scanErr != nil {
				return scanErr
			}
			matches := ports.FindAllByPort(port, all)
			if len(matches) == 0 {
				return fmt.Errorf("no process found listening on port %d on %s", port, infoHost)
			}
			if bindIP != "" {
				for i := range matches {
					if matches[i].BindAddress == bindIP {
						lp = &matches[i]
						break
					}
				}
				if lp == nil {
					return fmt.Errorf("no process found listening on %s:%d on %s", bindIP, port, infoHost)
				}
			} else if len(matches) == 1 {
				lp = &matches[0]
			} else {
				lp = &matches[0]
			}
			lp.Type = ports.ClassifyPort(lp.Port)
		} else {
			lp, err = ports.FindByPort(port, bindIP)
			if err != nil {
				return err
			}
			// Enrich
			enriched := []ports.ListeningPort{*lp}
			docker.EnrichPorts(enriched)
			ports.Enrich(enriched)
			ports.EnrichStats(enriched, docker.AllContainerStatsAsEntries())
			ports.EnrichHealth(enriched, 2*time.Second)
			*lp = enriched[0]
		}

		printField("Port", display.BoldCyan(fmt.Sprintf("%d", lp.Port)))
		printField("URL", display.Underline(lp.URL()))
		printField("Process", lp.Process)
		printField("PID", fmt.Sprintf("%d", lp.PID))
		printField("Type", lp.Type.String())

		if lp.Command != "" {
			printField("Command", lp.Command)
		}
		if lp.User != "" {
			printField("User", lp.User)
		}
		if lp.BindAddress != "" {
			printField("Bind Address", lp.BindAddress)
		}
		if lp.IPVersion != "" {
			printField("IP Version", lp.IPVersion)
		}

		fmt.Println()
		fmt.Println(display.Bold("Stats:"))
		printField("  CPU", fmt.Sprintf("%.1f%%", lp.CPUPercent))
		if lp.MemoryRSS > 0 {
			printField("  Memory", ports.FormatBytes(lp.MemoryRSS))
		}
		if lp.ThreadCount > 0 {
			printField("  Threads", fmt.Sprintf("%d", lp.ThreadCount))
		}
		if lp.Uptime != "" {
			printField("  Uptime", lp.Uptime)
		}
		if lp.State != "" {
			printField("  State", lp.State)
		}
		printField("  Connections", fmt.Sprintf("%d", lp.Connections))

		fmt.Println()
		fmt.Println(display.Bold("Health:"))
		printField("  Status", colorHealthInfo(lp.HealthStatus))
		if lp.HealthCode > 0 {
			printField("  Status Code", fmt.Sprintf("%d", lp.HealthCode))
		}
		if lp.HealthLatency > 0 {
			printField("  Latency", fmt.Sprintf("%dms", lp.HealthLatency.Milliseconds()))
		}

		if lp.Type == ports.PortTypeDocker {
			fmt.Println()
			fmt.Println(display.Bold("Docker:"))
			if lp.DockerContainer != "" {
				printField("  Container", lp.DockerContainer)
			}
			if lp.DockerImage != "" {
				printField("  Image", lp.DockerImage)
			}
			if lp.DockerContainerPort > 0 {
				printField("  Container Port", fmt.Sprintf("%d", lp.DockerContainerPort))
			}
			if lp.DockerComposeService != "" {
				printField("  Compose Service", lp.DockerComposeService)
			}
			if lp.DockerComposeProject != "" {
				printField("  Compose Project", lp.DockerComposeProject)
			}
		}

		return nil
	},
}

func colorHealthInfo(status string) string {
	switch status {
	case "healthy":
		return display.Green(status)
	case "unhealthy", "refused", "timeout":
		return display.Red(status)
	case "non-http":
		return display.Yellow(status)
	default:
		return status
	}
}

func printField(label, value string) {
	fmt.Printf("%-16s %s\n", display.Dim(label+":"), value)
}

func init() {
	infoCmd.Flags().String("host", "", "Query a remote host via SSH (e.g. user@hostname)")
	infoCmd.Flags().String("ip", "", "Specify bind address when a port is bound to multiple IPs")
	rootCmd.AddCommand(infoCmd)
}
