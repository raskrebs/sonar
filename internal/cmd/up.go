package cmd

import (
	"fmt"
	"time"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/profile"
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up <profile>",
	Short: "Check status of all ports in a profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prof, err := profile.Load(args[0])
		if err != nil {
			return err
		}

		// Scan current ports
		results, err := ports.Scan()
		if err != nil {
			return err
		}
		docker.EnrichPorts(results)
		ports.Enrich(results)

		// Build a map of port -> ListeningPorts for quick lookup
		// A port number may have multiple entries (different bind addresses).
		portMap := make(map[int][]*ports.ListeningPort)
		for i := range results {
			portMap[results[i].Port] = append(portMap[results[i].Port], &results[i])
		}

		// Collect ports that need health checks
		var healthTargets []ports.ListeningPort
		healthEntryIndices := make(map[int]int) // port -> index in healthTargets
		for _, entry := range prof.Ports {
			if entry.Health {
				if lps, ok := portMap[entry.Port]; ok && len(lps) > 0 {
					healthEntryIndices[entry.Port] = len(healthTargets)
					healthTargets = append(healthTargets, *lps[0])
				}
			}
		}

		// Run health checks in batch
		if len(healthTargets) > 0 {
			ports.EnrichHealth(healthTargets, 2*time.Second)
		}

		// Print table
		fmt.Printf("\n  %s  %s\n\n",
			display.Bold(prof.Name),
			display.Dim("profile status"))

		fmt.Printf("  %-8s %-18s %-14s %s\n",
			display.Dim("PORT"),
			display.Dim("NAME"),
			display.Dim("STATUS"),
			display.Dim("HEALTH"))

		allUp := true
		for _, entry := range prof.Ports {
			portStr := fmt.Sprintf("%d", entry.Port)
			name := entry.Name

			var status, health string
			if lps, ok := portMap[entry.Port]; ok && len(lps) > 0 {
				status = display.Green("\u2713 up")

				if entry.Health {
					if idx, exists := healthEntryIndices[entry.Port]; exists {
						h := healthTargets[idx].HealthStatus
						switch h {
						case "healthy":
							health = display.Green("healthy")
						case "unhealthy":
							health = display.Red("unhealthy")
						case "timeout":
							health = display.Yellow("timeout")
						default:
							health = display.Dim(h)
						}
					} else {
						health = display.Dim("-")
					}
				} else {
					health = display.Dim("non-http")
				}
			} else {
				status = display.Red("\u2717 missing")
				health = display.Dim("-")
				allUp = false
			}

			fmt.Printf("  %-8s %-18s %-14s %s\n", portStr, name, status, health)
		}

		fmt.Println()
		if allUp {
			fmt.Printf("  %s\n\n", display.Green("All ports are up."))
		} else {
			fmt.Printf("  %s\n\n", display.Yellow("Some ports are missing."))
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(upCmd)
}
