package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/spf13/cobra"
)

var hostJSONFlag bool

var hostCmd = &cobra.Command{
	Use:   "host",
	Short: "Show the machine sonar is watching and its load",
	Long: `Show the hosts sonar knows about: their cpu, load average, memory and disk.

This machine is published as ` + "`localhost`" + `; every host registered with
` + "`sonar remote add`" + ` joins the same table with its own load and the state of
its connection.

Examples:
  sonar host          # the table
  sonar host --json   # the rows state.snapshot publishes`,
	Args: cobra.NoArgs,
	RunE: runHost,
}

func init() {
	hostCmd.Flags().BoolVar(&hostJSONFlag, "json", false, "Output as JSON")
	rootCmd.AddCommand(hostCmd)
}

func runHost(cmd *cobra.Command, _ []string) error {
	cmd.SilenceUsage = true
	hosts, err := readHosts(cmd.Context())
	if err != nil {
		return err
	}
	if hostJSONFlag {
		return writeJSON(map[string]any{"hosts": hosts})
	}
	renderHosts(os.Stdout, hosts)
	return nil
}

// readHosts asks the daemon for the `hosts` collection.
//
// There is no direct-scan fallback, unlike the read commands of contract §20.
// CPU percent is a delta between two samples across the scan interval, and the
// daemon is what holds the previous one: a one-shot CLI could only report a
// different number under the same name.
func readHosts(ctx context.Context) ([]state.Host, error) {
	c, err := dialDaemon(ctx)
	if err != nil {
		if errors.Is(err, client.ErrNotRunning) {
			return nil, errors.New("host stats need a running daemon; start one with `sonar serve --detach`")
		}
		return nil, err
	}
	defer c.Close()

	var snap state.Snapshot
	if err := c.Call(ctx, "state.snapshot", rpc.StateSnapshotParams{}, &snap); err != nil {
		return nil, cliError(err)
	}
	if snap.Hosts == nil {
		return []state.Host{}, nil
	}
	return snap.Hosts, nil
}

// renderHosts prints the host table.
func renderHosts(w io.Writer, hosts []state.Host) {
	if len(hosts) == 0 {
		fmt.Fprintln(w, "No hosts.")
		return
	}
	fmt.Fprintf(w, "%-14s %-12s %-15s %-10s %-7s %-19s %-16s %s\n",
		display.Bold("NAME"), display.Bold("STATUS"), display.Bold("OS/ARCH"),
		display.Bold("UPTIME"), display.Bold("CPU"), display.Bold("LOAD"),
		display.Bold("MEMORY"), display.Bold("DISK"))
	for _, h := range hosts {
		fmt.Fprintf(w, "%-14s %-12s %-15s %-10s %-7s %-19s %-16s %s\n",
			display.Cyan(h.Name), hostStatus(h.Status), osArch(h),
			hostUptime(h.UptimeS), hostPercent(h.CPUPercent), hostLoad(h.Load),
			hostUsage(h.MemoryUsed, h.MemoryTotal), hostUsage(h.DiskUsed, h.DiskTotal))
		for _, g := range h.GPUs {
			fmt.Fprintf(w, "  %s %s  %s  %s\n", display.Dim("gpu"), g.Name,
				gpuPercent(g.UtilizationPercent), gpuMemory(g.MemoryUsedBytes, g.MemoryTotalBytes))
		}
	}
}

// gpuPercent prints a GPU's utilization; every source counts whole percent.
func gpuPercent(pct *float64) string {
	if pct == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *pct)
}

// gpuMemory prints "used/total" when the GPU has memory of its own, and just
// what it holds on unified memory, where there is no total to compare with.
func gpuMemory(used, total *int64) string {
	if used == nil {
		return "-"
	}
	if total != nil && *total > 0 {
		return hostUsage(used, total)
	}
	unit, name := sizeUnit(*used)
	return fmt.Sprintf("%.1f %s in use", float64(*used)/unit, name)
}

func hostStatus(status string) string {
	switch status {
	case state.HostConnected:
		return display.Green(status)
	case state.HostConnecting:
		return display.Yellow(status)
	case "":
		return "-"
	default:
		return display.Red(status)
	}
}

func osArch(h state.Host) string {
	if h.OS == "" && h.Arch == "" {
		return "-"
	}
	return h.OS + "/" + h.Arch
}

// hostUptime is the coarse form a load table wants: the two largest units,
// never seconds once a machine has been up for an hour.
func hostUptime(secs *int64) string {
	if secs == nil || *secs < 0 {
		return "-"
	}
	d := time.Duration(*secs) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

func hostPercent(pct *float64) string {
	if pct == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *pct)
}

// hostLoad prints the three averages. Windows has no load average at all, and
// prints a dash rather than three zeroes that would read as an idle machine.
func hostLoad(load []float64) string {
	if len(load) == 0 {
		return "-"
	}
	parts := make([]string, len(load))
	for i, v := range load {
		parts[i] = fmt.Sprintf("%.2f", v)
	}
	return strings.Join(parts, " ")
}

// hostUsage prints "used/total" in one unit, chosen from the total so the two
// numbers can be compared at a glance.
func hostUsage(used, total *int64) string {
	if used == nil || total == nil || *total <= 0 {
		return "-"
	}
	unit, name := sizeUnit(*total)
	return fmt.Sprintf("%.1f/%.1f %s", float64(*used)/unit, float64(*total)/unit, name)
}

func sizeUnit(total int64) (float64, string) {
	const k = 1024.0
	switch {
	case float64(total) >= k*k*k*k:
		return k * k * k * k, "TiB"
	case float64(total) >= k*k*k:
		return k * k * k, "GiB"
	case float64(total) >= k*k:
		return k * k, "MiB"
	default:
		return k, "KiB"
	}
}
