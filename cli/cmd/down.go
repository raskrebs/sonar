package cmd

import (
	"fmt"
	"strings"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/profile"
	"github.com/spf13/cobra"
)

var (
	downYesFlag   bool
	downForceFlag bool
)

var downCmd = &cobra.Command{
	Use:   "down <profile>",
	Short: "Stop all ports listed in a profile",
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

		// Build map of port -> ListeningPort
		portMap := make(map[int]*ports.ListeningPort)
		for i := range results {
			portMap[results[i].Port] = &results[i]
		}

		// Find which profile ports are actually running
		var active []ports.ListeningPort
		for _, entry := range prof.Ports {
			if lp, ok := portMap[entry.Port]; ok {
				active = append(active, *lp)
			}
		}

		if len(active) == 0 {
			fmt.Println("No profile ports are currently running.")
			return nil
		}

		// Show what will be stopped
		fmt.Printf("Will stop %d process(es) from profile %s:\n",
			len(active), display.Bold(prof.Name))
		for _, p := range active {
			fmt.Printf("  - %s on port %d\n", display.Bold(p.DisplayName()), p.Port)
		}

		if !downYesFlag {
			fmt.Print("\nProceed? [y/N] ")
			var answer string
			fmt.Scanln(&answer)
			if strings.ToLower(strings.TrimSpace(answer)) != "y" {
				fmt.Println("Aborted.")
				return nil
			}
		}

		var errs []string
		killed := 0
		for _, p := range active {
			if p.Type == ports.PortTypeDocker {
				name := p.DockerContainer
				if p.DockerComposeService != "" {
					name = p.DockerComposeService
				}
				fmt.Printf("Stopping Docker container %s on port %d\n",
					display.Bold(name), p.Port)
				if err := docker.StopContainer(p.DockerContainer); err != nil {
					errs = append(errs, fmt.Sprintf("port %d: %v", p.Port, err))
					continue
				}
			} else {
				sigName := "SIGTERM"
				if downForceFlag {
					sigName = "SIGKILL"
				}
				fmt.Printf("Killing %s (PID %d) on port %d with %s\n",
					display.Bold(p.DisplayName()), p.PID, p.Port, sigName)
				if err := ports.KillPID(p.PID, downForceFlag); err != nil {
					errs = append(errs, fmt.Sprintf("port %d: %v", p.Port, err))
					continue
				}
			}
			fmt.Printf("Freed %s\n", display.Underline(p.URL()))
			killed++
		}

		fmt.Printf("\n%d/%d processes stopped.\n", killed, len(active))
		if len(errs) > 0 {
			return fmt.Errorf("some processes failed to stop:\n  %s", strings.Join(errs, "\n  "))
		}
		return nil
	},
}

func init() {
	downCmd.Flags().BoolVarP(&downYesFlag, "yes", "y", false, "Skip confirmation prompt")
	downCmd.Flags().BoolVarP(&downForceFlag, "force", "f", false, "Send SIGKILL instead of SIGTERM")
	rootCmd.AddCommand(downCmd)
}
