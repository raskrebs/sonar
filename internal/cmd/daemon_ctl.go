package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/selfupdate"
	"github.com/spf13/cobra"
)

var (
	daemonJSONFlag   bool
	daemonFollowFlag bool
	daemonLinesFlag  int
)

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether the daemon is running, and what it is doing",
	Args:  cobra.NoArgs,
	RunE:  daemonStatusRun,
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	Args:  cobra.NoArgs,
	RunE:  daemonStopRun,
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Stop the daemon if it is running, then start it detached",
	Args:  cobra.NoArgs,
	RunE:  daemonRestartRun,
}

var daemonPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the socket path the daemon listens on",
	Args:  cobra.NoArgs,
	RunE:  daemonPathRun,
}

var daemonLogCmd = &cobra.Command{
	Use:   "log",
	Short: "Print the daemon log",
	Args:  cobra.NoArgs,
	RunE:  daemonLogRun,
}

func init() {
	daemonStatusCmd.Flags().BoolVar(&daemonJSONFlag, "json", false, "Output as JSON")
	daemonPathCmd.Flags().BoolVar(&daemonJSONFlag, "json", false, "Output as JSON")
	daemonLogCmd.Flags().BoolVarP(&daemonFollowFlag, "follow", "f", false, "Follow the log as it grows")
	daemonLogCmd.Flags().IntVarP(&daemonLinesFlag, "lines", "n", 50, "Number of trailing lines to print")

	daemonCmd.AddCommand(daemonStatusCmd, daemonStopCmd, daemonRestartCmd, daemonPathCmd, daemonLogCmd)
}

// connectRunning dials an already-running daemon. `sonar daemon` never
// autostarts: asking a daemon about itself must not create one.
func connectRunning(ctx context.Context) (*client.Client, error) {
	return client.Dial(ctx, client.ClientInfo{
		Name:        "cli",
		Version:     selfupdate.Version,
		NoAutostart: true,
	})
}

// notRunning prints the standard "no daemon" message and exits 1.
func notRunning(socket string) error {
	if daemonJSONFlag {
		out, _ := json.MarshalIndent(map[string]any{
			"running": false,
			"socket":  socket,
		}, "", "  ")
		fmt.Println(string(out))
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "error: sonar daemon is not running")
	fmt.Fprintln(os.Stderr, "hint: start it with `sonar serve` (or `sonar serve -d` to detach)")
	os.Exit(1)
	return nil
}

func daemonStatusRun(cmd *cobra.Command, _ []string) error {
	socket := daemon.SocketPath()
	c, err := connectRunning(cmd.Context())
	if err != nil {
		if errors.Is(err, client.ErrNotRunning) {
			return notRunning(socket)
		}
		return err
	}
	defer c.Close()

	var status rpc.DaemonStatusResult
	if err := c.Call(cmd.Context(), "daemon.status", rpc.Empty{}, &status); err != nil {
		return err
	}
	hello := c.Hello()

	if daemonJSONFlag {
		out, err := json.MarshalIndent(map[string]any{
			"running":               true,
			"pid":                   status.PID,
			"uptime":                status.Uptime,
			"subscribers":           status.Subscribers,
			"last_scan_at":          status.LastScanAt,
			"scan_interval_ms":      status.ScanIntervalMs,
			"scan_base_interval_ms": status.ScanBaseIntervalMs,
			"stats_interval_ms":     status.StatsIntervalMs,
			"claim_ttl_ms":          status.ClaimTTLMs,
			"scans":                 status.Scans,
			"db_path":               status.DBPath,
			"socket":                hello.Socket,
			"daemon_version":        hello.DaemonVersion,
			"protocol_version":      hello.ProtocolVersion,
			"capabilities":          hello.Capabilities,
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}

	fmt.Printf("running       yes\n")
	fmt.Printf("pid           %d\n", status.PID)
	fmt.Printf("version       %s (protocol %s)\n", hello.DaemonVersion, hello.ProtocolVersion)
	fmt.Printf("uptime        %s\n", status.Uptime)
	fmt.Printf("subscribers   %d\n", status.Subscribers)
	fmt.Printf("scan interval %dms\n", status.ScanIntervalMs)
	fmt.Printf("scan base     %dms\n", status.ScanBaseIntervalMs)
	fmt.Printf("stats tick    %dms\n", status.StatsIntervalMs)
	fmt.Printf("claim ttl     %dms\n", status.ClaimTTLMs)
	fmt.Printf("scans         %d\n", status.Scans)
	if status.LastScanAt != "" {
		fmt.Printf("last scan     %s\n", status.LastScanAt)
	}
	fmt.Printf("socket        %s\n", hello.Socket)
	if status.DBPath != "" {
		fmt.Printf("database      %s\n", status.DBPath)
	}
	fmt.Printf("capabilities  %v\n", hello.Capabilities)
	return nil
}

func daemonStopRun(cmd *cobra.Command, _ []string) error {
	socket := daemon.SocketPath()
	c, err := connectRunning(cmd.Context())
	if err != nil {
		if errors.Is(err, client.ErrNotRunning) {
			return notRunning(socket)
		}
		return err
	}
	defer c.Close()

	var ok rpc.OKResult
	if err := c.Call(cmd.Context(), "daemon.shutdown", rpc.Empty{}, &ok); err != nil {
		return err
	}
	if err := waitForDaemonGone(socket, stopTimeout); err != nil {
		return err
	}
	fmt.Println("sonar daemon stopped")
	return nil
}

func daemonRestartRun(cmd *cobra.Command, _ []string) error {
	socket := daemon.SocketPath()
	if c, err := connectRunning(cmd.Context()); err == nil {
		var ok rpc.OKResult
		_ = c.Call(cmd.Context(), "daemon.shutdown", rpc.Empty{}, &ok)
		c.Close()
		// The old daemon releases its lock after it closes its socket, so the
		// replacement must wait for the lock, not for the socket.
		if err := waitForDaemonGone(socket, stopTimeout); err != nil {
			return err
		}
	}
	return detachDaemon(cmd.Context(), socket)
}

func daemonPathRun(*cobra.Command, []string) error {
	socket := daemon.SocketPath()
	if daemonJSONFlag {
		out, err := json.MarshalIndent(map[string]any{
			"socket": socket,
			"lock":   daemon.LockPath(),
			"log":    daemon.LogPath(),
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	fmt.Println(socket)
	return nil
}

func daemonLogRun(cmd *cobra.Command, _ []string) error {
	path := daemon.LogPath()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no daemon log at %s yet — start the daemon with `sonar serve`", path)
		}
		return err
	}
	defer f.Close()

	if err := printTail(f, daemonLinesFlag); err != nil {
		return err
	}
	if !daemonFollowFlag {
		return nil
	}
	return followFile(cmd.Context(), f)
}

// printTail writes the last n lines of f to stdout and leaves the file offset
// at the end, ready for follow mode.
func printTail(f *os.File, n int) error {
	if n <= 0 {
		_, err := f.Seek(0, io.SeekEnd)
		return err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ring := make([]string, 0, n)
	for scanner.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, line := range ring {
		fmt.Println(line)
	}
	return nil
}

// followFile polls for appended bytes, like `tail -f`. Polling keeps this the
// same code on every platform.
func followFile(ctx context.Context, f *os.File) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}
