package cmd

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var (
	waitTimeoutFlag  time.Duration
	waitIntervalFlag time.Duration
	waitHTTPFlag     bool
	waitQuietFlag    bool
)

var waitCmd = &cobra.Command{
	Use:   "wait <port> [port...]",
	Short: "Block until one or more ports are ready",
	Long: `Block until the specified ports are accepting connections.

By default, sonar wait checks for a TCP connection. Use --http to wait
for an HTTP 200-399 response instead (useful for services that accept
TCP connections before they are truly ready).

Exit codes:
  0  all ports are ready
  1  timeout exceeded
  2  interrupted (Ctrl+C)

Examples:
  sonar wait 5432
  sonar wait 5432 --timeout 30s
  sonar wait 5432 --http
  sonar wait 5432 3000 6379
  sonar wait 5432 --quiet
  sonar wait 5432 --interval 500ms`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Parse port numbers
		portList := make([]int, 0, len(args))
		for _, arg := range args {
			p, err := strconv.Atoi(arg)
			if err != nil || p < 1 || p > 65535 {
				return fmt.Errorf("invalid port: %s", arg)
			}
			portList = append(portList, p)
		}

		// Set up interrupt handling
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		defer signal.Stop(sigCh)

		if !waitQuietFlag {
			label := "TCP"
			if waitHTTPFlag {
				label = "HTTP"
			}
			fmt.Printf("%s Waiting for %s on port(s) %v (timeout %s)\n",
				display.Dim("⏳"), label, portList, waitTimeoutFlag)
		}

		deadline := time.After(waitTimeoutFlag)
		ticker := time.NewTicker(waitIntervalFlag)
		defer ticker.Stop()

		// Track which ports are still pending
		pending := make(map[int]bool, len(portList))
		for _, p := range portList {
			pending[p] = true
		}

		// Check immediately, then on each tick
		for {
			for p := range pending {
				if isPortReady(p, waitHTTPFlag) {
					delete(pending, p)
					if !waitQuietFlag {
						fmt.Printf("  %s port %d ready\n", display.Green("✓"), p)
					}
				}
			}

			if len(pending) == 0 {
				if !waitQuietFlag {
					fmt.Printf("%s All ports ready\n", display.Green("✔"))
				}
				return nil
			}

			select {
			case <-sigCh:
				if !waitQuietFlag {
					fmt.Println()
					fmt.Fprintf(os.Stderr, "%s Interrupted\n", display.Red("✗"))
				}
				os.Exit(2)
			case <-deadline:
				remaining := make([]int, 0, len(pending))
				for p := range pending {
					remaining = append(remaining, p)
				}
				if !waitQuietFlag {
					fmt.Fprintf(os.Stderr, "%s Timeout waiting for port(s) %v\n",
						display.Red("✗"), remaining)
				}
				os.Exit(1)
			case <-ticker.C:
				// next iteration
			}
		}
	},
}

// isPortReady checks whether a port is ready via TCP or HTTP.
func isPortReady(port int, httpCheck bool) bool {
	if httpCheck {
		result := ports.ProbeHealth(port, 2*time.Second)
		return result.Status == "healthy"
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func init() {
	waitCmd.Flags().DurationVar(&waitTimeoutFlag, "timeout", 30*time.Second, "Maximum time to wait (e.g. 30s, 1m)")
	waitCmd.Flags().DurationVarP(&waitIntervalFlag, "interval", "i", 1*time.Second, "Polling interval (e.g. 1s, 500ms)")
	waitCmd.Flags().BoolVar(&waitHTTPFlag, "http", false, "Wait for HTTP 200-399 instead of TCP connection")
	waitCmd.Flags().BoolVarP(&waitQuietFlag, "quiet", "q", false, "No output, just exit code (for scripts)")
	rootCmd.AddCommand(waitCmd)
}
