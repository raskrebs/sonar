package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/selfupdate"
	"github.com/spf13/cobra"

	// The relay session registers its handlers from its own init(). Every
	// other namespace is pulled in by the command that fronts it; this one has
	// no command and never will — signing in is a step of `share.create`, not
	// a `sonar login` — so the daemon's own command is what imports it.
	_ "github.com/raskrebs/sonar/internal/session"
)

// detachedEnv marks the child `sonar serve` that `serve --detach` forked. The
// child is not a different command, only a differently plumbed one.
const detachedEnv = "SONAR_DAEMON_DETACHED"

var serveDetach bool

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the sonar daemon",
	Long: "Run the daemon that owns port scanning. Every interface (CLI, tray,\n" +
		"desktop app, MCP server) reads cached state and deltas from it instead\n" +
		"of scanning on its own.\n\n" +
		"Foreground by default, logging to stderr and to the log file.\n" +
		"`sonar daemon path` prints the socket it listens on.",
	Args: cobra.NoArgs,
	RunE: serveRun,
}

func init() {
	serveCmd.Flags().BoolVarP(&serveDetach, "detach", "d", false,
		"Start the daemon in the background and return once it accepts connections")
	rootCmd.AddCommand(serveCmd)
}

func serveRun(cmd *cobra.Command, _ []string) error {
	if err := refuseTestBinaryDaemon(); err != nil {
		return err
	}

	socket := daemon.SocketPath()

	if serveDetach {
		return detachDaemon(cmd.Context(), socket)
	}

	// A detached child's stderr is already redirected into the log file, so
	// mirroring the logger there too would double every line.
	alsoStderr := os.Getenv(detachedEnv) == ""

	logger, closeLog, err := daemon.NewLogger(daemon.LogPath(),
		daemon.ParseLogLevel(loadedConfig.Daemon.ResolvedLogLevel()), alsoStderr)
	if err != nil {
		return err
	}
	defer closeLog.Close()

	srv := daemon.New(daemon.Options{
		Socket:        socket,
		Version:       selfupdate.Version,
		IdleTimeout:   loadedConfig.Daemon.ResolvedIdleTimeout(),
		StatsInterval: loadedConfig.Daemon.ResolvedStatsInterval(),
		ScanInterval:  loadedConfig.Daemon.ResolvedScanInterval(),
		ClaimTTL:      loadedConfig.Claims.ResolvedTTL(),
		Logger:        logger,
	})

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx); err != nil {
		if daemon.IsAlreadyRunning(err) {
			// Not a crash: another daemon is doing the job.
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintf(os.Stderr, "hint: `sonar daemon status` shows it, `sonar daemon restart` replaces it\n")
			os.Exit(1)
		}
		return err
	}
	return nil
}

// refuseTestBinaryDaemon stops a Go test binary from becoming the daemon.
//
// `go test` links a package into <pkg>.test, and Go's flag parser stops at the
// first non-flag argument — so `cmd.test serve --detach` silently ignores both
// words and runs the entire test suite in the background against the config
// directory it inherited. Every such process is a stray test run holding the
// user's real ~/.config/sonar open, not a daemon (step 1A.20). Both entry
// points are covered: `serve` refuses here, and `serve --detach` re-execs this
// same binary, so it never gets far enough to fork.
func refuseTestBinaryDaemon() error {
	exe, err := os.Executable()
	if err != nil || !daemon.IsTestBinary(exe) || daemon.TestDaemonAllowed() {
		return nil
	}
	return fmt.Errorf("refusing to run %s as the sonar daemon: it is a Go test binary, so this would re-run the test suite rather than serve\nhint: start a daemon in-process from the test, or set %s=1 if you really mean it",
		filepath.Base(exe), daemon.AllowTestDaemonEnv)
}

// detachDaemon re-execs this binary as a background `sonar serve` and waits for
// the socket to come up, so the caller can rely on the daemon being ready when
// the command returns (contract §7: within 3 s).
func detachDaemon(ctx context.Context, socket string) error {
	if daemon.SocketAlive(socket) {
		pid := daemon.LockHolderPID(daemon.LockPath())
		if pid > 0 {
			return fmt.Errorf("daemon already running (pid %d)", pid)
		}
		return fmt.Errorf("daemon already running")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the sonar binary: %w", err)
	}

	// The child inherits nothing but the environment: its output goes to the
	// daemon log so a panic before the logger is up is still recoverable.
	if err := os.MkdirAll(filepath.Dir(daemon.LogPath()), 0o700); err != nil {
		return fmt.Errorf("creating the log directory: %w", err)
	}
	logFile, err := os.OpenFile(daemon.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", daemon.LogPath(), err)
	}
	defer logFile.Close()

	child := exec.Command(exe, "serve")
	child.Env = append(os.Environ(), detachedEnv+"=1")
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	detachProcess(child)

	if err := child.Start(); err != nil {
		return fmt.Errorf("starting the daemon: %w", err)
	}
	// Do not wait for the child: it is the daemon. Release its resources.
	go func() { _ = child.Wait() }()

	if err := client.WaitForSocket(ctx, socket, client.AutostartTimeout); err != nil {
		return fmt.Errorf("%w (see %s)", err, daemon.LogPath())
	}
	fmt.Printf("sonar daemon started (pid %d), listening on %s\n", child.Process.Pid, socket)
	return nil
}

// stopTimeout bounds how long `daemon stop` and `daemon restart` wait for the
// previous daemon to let go of the socket and the lock.
const stopTimeout = 10 * time.Second

// waitForSocketGone is the inverse of WaitForSocket, used by `daemon restart`.
func waitForSocketGone(socket string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !daemon.SocketAlive(socket) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForDaemonGone waits until the previous daemon has both stopped accepting
// connections and released the single-instance lock. The socket closes first
// and the lock is released last, so a restart that only waited for the socket
// raced the old daemon's teardown: the replacement started, lost the lock, and
// exited "already running", leaving nothing running (contract §21).
func waitForDaemonGone(socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	waitForSocketGone(socket, timeout)

	remaining := time.Until(deadline)
	if remaining < time.Second {
		remaining = time.Second
	}
	if err := daemon.WaitForLockRelease(daemon.LockPath(), remaining); err != nil {
		return fmt.Errorf("the running daemon did not shut down: %w\nhint: `sonar daemon status` shows it; kill it and retry", err)
	}
	return nil
}
