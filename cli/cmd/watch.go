package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/notify"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var intervalFlag time.Duration
var notifyFlag bool

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch for port changes in real-time",
	RunE: func(cmd *cobra.Command, args []string) error {
		showAll, _ := cmd.Flags().GetBool("all")
		notifyFlag, _ = cmd.Flags().GetBool("notify")
		watchHost, _ := cmd.Flags().GetString("host")

		// Initial scan
		current, err := scanAndEnrichWithHost(watchHost)
		if err != nil {
			return err
		}
		if !showAll {
			current = excludeApps(current)
		}

		display.RenderTable(os.Stdout, current, display.TableOptions{})

		fmt.Println()
		fmt.Println(display.Dim("Watching for changes... (Ctrl+C to stop)"))
		fmt.Println()

		// Set up signal handling
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)

		ticker := time.NewTicker(intervalFlag)
		defer ticker.Stop()

		for {
			select {
			case <-sigCh:
				fmt.Println()
				return nil
			case <-ticker.C:
				next, err := scanAndEnrichWithHost(watchHost)
				if err != nil {
					continue
				}
				if !showAll {
					next = excludeApps(next)
				}
				printDiff(current, next)
				current = next
			}
		}
	},
}

func init() {
	watchCmd.Flags().DurationVarP(&intervalFlag, "interval", "i", 2*time.Second, "Poll interval (e.g. 2s, 500ms)")
	watchCmd.Flags().BoolP("all", "a", false, "Include desktop apps (hidden by default)")
	watchCmd.Flags().BoolP("notify", "n", false, "Send desktop notifications on port changes")
	watchCmd.Flags().String("host", "", "Watch a remote host via SSH (e.g. user@hostname)")
	rootCmd.AddCommand(watchCmd)
}

func scanAndEnrich() ([]ports.ListeningPort, error) {
	results, err := ports.Scan()
	if err != nil {
		return nil, err
	}
	docker.EnrichPorts(results)
	ports.Enrich(results)
	return results, nil
}

func scanAndEnrichWithHost(host string) ([]ports.ListeningPort, error) {
	if host == "" {
		return scanAndEnrich()
	}
	results, err := ports.ScanRemote(host)
	if err != nil {
		return nil, err
	}
	for i := range results {
		results[i].Type = ports.ClassifyPort(results[i].Port)
	}
	return results, nil
}

func printDiff(old, new []ports.ListeningPort) {
	oldMap := make(map[int]ports.ListeningPort)
	for _, p := range old {
		oldMap[p.Port] = p
	}

	newMap := make(map[int]ports.ListeningPort)
	for _, p := range new {
		newMap[p.Port] = p
	}

	now := time.Now().Format("15:04:05")

	// New ports
	for _, p := range new {
		if _, exists := oldMap[p.Port]; !exists {
			fmt.Printf("%s %s %-5d  %-20s  %s\n",
				display.Dim("["+now+"]"),
				display.Green("+ "+fmt.Sprintf("%-5d", p.Port)),
				p.PID,
				p.DisplayName(),
				display.Underline(p.URL()))
			if notifyFlag {
				notify.Send("Port Opened", fmt.Sprintf("Port %d opened (%s)", p.Port, p.DisplayName()))
			}
		}
	}

	// Removed ports
	for _, p := range old {
		if _, exists := newMap[p.Port]; !exists {
			fmt.Printf("%s %s %-5d  %-20s  %s\n",
				display.Dim("["+now+"]"),
				display.Red("- "+fmt.Sprintf("%-5d", p.Port)),
				p.PID,
				p.DisplayName(),
				display.Dim(p.URL()))
			if notifyFlag {
				notify.Send("Port Closed", fmt.Sprintf("Port %d closed (%s)", p.Port, p.DisplayName()))
			}
		}
	}
}
