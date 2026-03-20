package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var (
	nextCountFlag int
	nextJSONFlag  bool
)

var nextCmd = &cobra.Command{
	Use:   "next [port-or-range]",
	Short: "Find the next available (free) TCP port",
	Long: `Find the next available TCP port not currently in use.

By default, searches starting from port 3000. You can specify a start port
or a range (e.g. 3000-3100). Use --count to find multiple consecutive free ports.

Results reflect a point-in-time snapshot; a port could be allocated by another process before you bind it.

Examples:
  sonar next              # first free port starting from 3000
  sonar next 8000         # first free port starting from 8000
  sonar next 3000-3100    # first free port in range 3000-3100
  sonar next --count 3    # find 3 consecutive free ports from 3000
  sonar next 8000 --json  # JSON output`,
	Args: cobra.MaximumNArgs(1),
	RunE: nextRun,
}

func init() {
	nextCmd.Flags().IntVar(&nextCountFlag, "count", 1, "Number of consecutive free ports to find")
	nextCmd.Flags().BoolVar(&nextJSONFlag, "json", false, "Output as JSON")
	rootCmd.AddCommand(nextCmd)
}

func nextRun(cmd *cobra.Command, args []string) error {
	startPort := 3000
	endPort := 65535

	if len(args) == 1 {
		arg := args[0]
		if strings.Contains(arg, "-") {
			parts := strings.SplitN(arg, "-", 2)
			lo, err := strconv.Atoi(parts[0])
			if err != nil {
				return fmt.Errorf("invalid range start: %s", parts[0])
			}
			hi, err := strconv.Atoi(parts[1])
			if err != nil {
				return fmt.Errorf("invalid range end: %s", parts[1])
			}
			if lo > hi || lo < 1 || hi > 65535 {
				return fmt.Errorf("invalid port range: %d-%d", lo, hi)
			}
			startPort = lo
			endPort = hi
		} else {
			p, err := strconv.Atoi(arg)
			if err != nil || p < 1 || p > 65535 {
				return fmt.Errorf("invalid port: %s", arg)
			}
			startPort = p
		}
	}

	if nextCountFlag < 1 {
		return fmt.Errorf("--count must be at least 1")
	}

	results, err := ports.Scan()
	if err != nil {
		return err
	}

	occupied := make(map[int]bool, len(results))
	for _, r := range results {
		occupied[r.Port] = true
	}

	freePorts := findFreePorts(occupied, startPort, endPort, nextCountFlag)
	if len(freePorts) < nextCountFlag {
		if endPort < 65535 {
			return fmt.Errorf("no %d consecutive free port(s) in range %d-%d", nextCountFlag, startPort, endPort)
		}
		return fmt.Errorf("no %d consecutive free port(s) starting from %d", nextCountFlag, startPort)
	}

	if nextJSONFlag {
		out := struct {
			Ports []int `json:"ports"`
		}{Ports: freePorts}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(out)
	}

	for _, p := range freePorts {
		fmt.Println(p)
	}
	return nil
}

// findFreePorts finds count consecutive free ports in [start, end].
func findFreePorts(occupied map[int]bool, start, end, count int) []int {
	var consecutive []int
	for p := start; p <= end; p++ {
		if occupied[p] {
			consecutive = nil
			continue
		}
		consecutive = append(consecutive, p)
		if len(consecutive) == count {
			return consecutive
		}
	}
	return consecutive
}
