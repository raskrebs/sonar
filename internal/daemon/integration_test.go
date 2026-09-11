//go:build integration

// Integration tests for the daemon, run with `go test -tags integration
// ./internal/daemon/...`. They build the real `sonar` binary, start it as
// `sonar serve` in a temp HOME on a temp socket, and drive it with the Go
// client — the same path the CLI, the MCP server and the desktop app take.
//
// Covered here: spec integration test 1 (hello, version, capabilities), spec
// integration test 5 (second instance refused, stale socket cleaned after
// SIGKILL), and the step's acceptance demo (a subscriber sees a listener
// appear and disappear).
package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/runs"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/raskrebs/sonar/internal/testenv"
)

// buildBinary compiles the sonar CLI once per test run.
// The binary goes under testenv.Root(): the leak gate only claims a `serve`
// whose executable lives inside this run's own temp root, so a daemon this
// harness starts has to be built there to be recognised — and one another run
// built has to be somewhere else to be left alone.
var buildBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp(testenv.Root(), "sonar-itest-bin")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "sonar")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = repoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building sonar: %v: %s", err, out)
	}
	return bin, nil
})

// repoRoot walks up from this file to the module root.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return "."
}

// env is one isolated daemon: its own HOME, its own socket, its own log.
type env struct {
	t      *testing.T
	bin    string
	home   string
	socket string
	// extra is appended to every command's environment, after everything
	// else, so it wins: a test puts a fake tool first on PATH with it.
	extra []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	// macOS caps a unix socket path at ~104 bytes and t.TempDir() is long, so
	// the socket goes in a short directory of its own.
	sockDir, err := os.MkdirTemp("", "snr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })

	socket := filepath.Join(sockDir, "d.sock")
	if runtime.GOOS == "windows" {
		// Named pipes share one flat namespace, so the pipe is numbered as
		// well as pid-stamped: two envs in one test binary must not land on
		// the same address, or a daemon still shutting down from an earlier
		// test owns the name this one is about to bind.
		socket = fmt.Sprintf(`\\.\pipe\sonar-itest-%d-%d`, os.Getpid(), pipeSeq.Add(1))
	}
	return &env{t: t, bin: bin, home: home, socket: socket}
}

// waitDelay bounds how long Wait may block on a pipe after the process it was
// waiting for has exited. A stray grandchild holding the inherited stdout
// keeps that pipe open forever, so without this a test that has already failed
// hangs until `go test -timeout` shoots it.
const waitDelay = 5 * time.Second

// pipeSeq numbers this test binary's named pipes.
var pipeSeq atomic.Int64

// lockPath is where this env's daemon takes its single-instance lock: beside
// the socket, or in the config dir when the transport is a named pipe
// (contract §15).
func (e *env) lockPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(e.home, ".config", "sonar", "daemon.lock")
	}
	return filepath.Join(filepath.Dir(e.socket), "daemon.lock")
}

// stopDaemon stops a daemon this env started detached and waits until it is
// really gone, not merely unreachable.
//
// Windows refuses to delete a file another process still has open, so a test
// whose temp HOME holds the daemon's lock and log has to see that process exit
// before t.TempDir's own cleanup runs — otherwise the test fails in teardown
// with "the process cannot access the file because it is being used by another
// process". pid may be 0 when the caller does not know it; the socket and the
// lock are then the only evidence.
func (e *env) stopDaemon(pid int) {
	_ = e.command("daemon", "stop").Run()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !daemon.SocketAlive(e.socket) && (pid <= 0 || !runs.PIDAlive(pid)) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The lock is the last thing a daemon lets go of.
	if err := daemon.WaitForLockRelease(e.lockPath(), 10*time.Second); err != nil {
		e.t.Logf("the daemon still holds %s: %v", e.lockPath(), err)
	}
}

// command builds a `sonar` invocation pinned to this env.
func (e *env) command(args ...string) *exec.Cmd {
	cmd := exec.Command(e.bin, args...)
	cmd.WaitDelay = waitDelay
	cmd.Env = append(os.Environ(),
		"HOME="+e.home,
		"USERPROFILE="+e.home,
		"SONAR_SOCKET="+e.socket,
		// XDG_RUNTIME_DIR would otherwise outrank HOME for the log path.
		"XDG_RUNTIME_DIR=",
		// The test binary is isolated with one SONAR_DB for the whole run
		// (internal/testenv); clearing it puts this env's database under this
		// env's HOME, which is what a test that restarts a daemon and expects
		// its own rows back is asking for.
		"SONAR_DB=",
		// These children are the real `sonar`, and the tests here are the ones
		// that drive its autostart on purpose.
		testenv.ChildEnv(),
	)
	cmd.Env = append(cmd.Env, e.extra...)
	return cmd
}

// serve starts `sonar serve` in the background and waits for the socket.
func (e *env) serve() *exec.Cmd {
	e.t.Helper()
	cmd := e.command("serve")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("starting sonar serve: %v", err)
	}
	e.t.Cleanup(func() {
		stopCommand(cmd)
		if e.t.Failed() && out.Len() > 0 {
			e.t.Logf("daemon output:\n%s", out.String())
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.WaitForSocket(ctx, e.socket, 10*time.Second); err != nil {
		e.t.Fatalf("daemon never came up: %v\n%s", err, out.String())
	}
	return cmd
}

// connect dials the daemon as a test client.
func (e *env) connect(ctx context.Context) *client.Client {
	e.t.Helper()
	c, err := client.Dial(ctx, client.ClientInfo{
		Name: "cli", Version: "itest", Socket: e.socket,
	})
	if err != nil {
		e.t.Fatalf("connecting to the daemon: %v", err)
	}
	e.t.Cleanup(func() { c.Close() })
	return c
}

// TestHelloReportsVersionAndCapabilities is spec integration test 1.
func TestHelloReportsVersionAndCapabilities(t *testing.T) {
	e := newEnv(t)
	e.serve()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := e.connect(ctx)

	hello := c.Hello()
	if hello.ProtocolVersion != rpc.ProtocolVersion {
		t.Errorf("protocol_version = %q, want %q", hello.ProtocolVersion, rpc.ProtocolVersion)
	}
	if hello.DaemonVersion == "" {
		t.Error("daemon_version is empty")
	}
	if hello.PID <= 0 {
		t.Errorf("pid = %d, want a live process id", hello.PID)
	}
	if hello.StartedAt == "" {
		t.Error("started_at is empty")
	}
	if hello.Socket != e.socket {
		t.Errorf("socket = %q, want %q", hello.Socket, e.socket)
	}
	if hello.BinaryPath == "" {
		t.Error("binary_path is empty; contract §4 requires it for autostart")
	}
	want := map[string]bool{"state": true, "ports.read": true}
	for _, cap := range hello.Capabilities {
		delete(want, cap)
	}
	if len(want) > 0 {
		t.Errorf("capabilities = %v, missing %v", hello.Capabilities, want)
	}

	var status rpc.DaemonStatusResult
	if err := c.Call(ctx, "daemon.status", rpc.Empty{}, &status); err != nil {
		t.Fatalf("daemon.status: %v", err)
	}
	if status.PID != hello.PID {
		t.Errorf("daemon.status pid = %d, hello pid = %d", status.PID, hello.PID)
	}
}

// TestSubscriberSeesAPortAppearAndDisappear is the step's acceptance demo, run
// in-process: a listener on a free port shows up in a state.delta, and its
// close shows up in a later one.
func TestSubscriberSeesAPortAppearAndDisappear(t *testing.T) {
	e := newEnv(t)
	e.serve()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := e.connect(ctx)

	sub, err := c.Subscribe(ctx, client.SubscribeOptions{Events: true})
	if err != nil {
		t.Fatalf("state.subscribe: %v", err)
	}
	t.Logf("subscribed at seq %d with %d ports", sub.Snapshot.Seq, len(sub.Snapshot.Ports))

	// A real listener on a real free port, so the daemon's own lsof/ss scan
	// has to find it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("opening a test listener: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	t.Logf("test listener is on port %d", port)

	if !waitForAdded(t, sub, port, 30*time.Second) {
		ln.Close()
		t.Fatalf("no state.delta added port %d within 30s", port)
	}

	ln.Close()
	if !waitForRemoved(t, sub, port, 30*time.Second) {
		t.Fatalf("no state.delta removed port %d within 30s", port)
	}

	var status rpc.DaemonStatusResult
	if err := c.Call(ctx, "daemon.status", rpc.Empty{}, &status); err != nil {
		t.Fatalf("daemon.status: %v", err)
	}
	if status.Subscribers != 1 {
		t.Errorf("daemon.status subscribers = %d, want 1", status.Subscribers)
	}
}

func waitForAdded(t *testing.T, sub *client.Subscription, port int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case d, ok := <-sub.Deltas:
			if !ok {
				return false
			}
			for _, p := range d.Ports.Added {
				if p.Port == port {
					t.Logf("delta seq %d added %d (%s)", d.Seq, p.Port, p.DisplayName)
					return true
				}
			}
		case <-deadline:
			return false
		}
	}
}

func waitForRemoved(t *testing.T, sub *client.Subscription, port int, timeout time.Duration) bool {
	t.Helper()
	prefix := fmt.Sprintf("%d:", port)
	deadline := time.After(timeout)
	for {
		select {
		case d, ok := <-sub.Deltas:
			if !ok {
				return false
			}
			for _, key := range d.Ports.Removed {
				if strings.HasPrefix(key, prefix) {
					t.Logf("delta seq %d removed %s", d.Seq, key)
					return true
				}
			}
		case <-deadline:
			return false
		}
	}
}

// TestSecondServeRefusesAndStaleSocketIsCleaned is spec integration test 5.
func TestSecondServeRefusesAndStaleSocketIsCleaned(t *testing.T) {
	e := newEnv(t)
	first := e.serve()

	// A second `sonar serve` must refuse rather than steal the socket.
	second := e.command("serve")
	out, err := second.CombinedOutput()
	if err == nil {
		t.Fatalf("a second `sonar serve` succeeded; output:\n%s", out)
	}
	if !strings.Contains(string(out), "already running") {
		t.Errorf("second serve said %q, want an \"already running\" message", out)
	}
	if pid := first.Process.Pid; !strings.Contains(string(out), fmt.Sprint(pid)) {
		t.Errorf("second serve did not name the holder pid %d: %q", pid, out)
	}

	// The first daemon is still serving.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	e.connect(ctx)

	// SIGKILL leaves the socket file behind with no chance to clean up.
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("killing the daemon: %v", err)
	}
	_ = first.Wait()

	if runtime.GOOS != "windows" {
		if _, err := os.Stat(e.socket); err != nil {
			t.Fatalf("expected a stale socket file at %s after SIGKILL: %v", e.socket, err)
		}
		if _, err := daemon.Dial(e.socket); err == nil {
			t.Fatal("the stale socket still accepts connections")
		}
	}

	// A fresh daemon must remove the stale socket and take the lock.
	e.serve()
	c2 := e.connect(ctx)
	if c2.Hello().PID == first.Process.Pid {
		t.Error("the new daemon reports the dead daemon's pid")
	}
}

// TestDaemonStatusAndStopThroughTheCLI exercises the `sonar daemon` surface the
// demo uses.
func TestDaemonStatusAndStopThroughTheCLI(t *testing.T) {
	e := newEnv(t)
	e.serve()

	out, err := e.command("daemon", "status").CombinedOutput()
	if err != nil {
		t.Fatalf("sonar daemon status: %v\n%s", err, out)
	}
	for _, want := range []string{"running       yes", "pid", "subscribers", "socket"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("`sonar daemon status` output is missing %q:\n%s", want, out)
		}
	}

	pathOut, err := e.command("daemon", "path").CombinedOutput()
	if err != nil {
		t.Fatalf("sonar daemon path: %v\n%s", err, pathOut)
	}
	if strings.TrimSpace(string(pathOut)) != e.socket {
		t.Errorf("`sonar daemon path` = %q, want %q", strings.TrimSpace(string(pathOut)), e.socket)
	}

	stopOut, err := e.command("daemon", "stop").CombinedOutput()
	if err != nil {
		t.Fatalf("sonar daemon stop: %v\n%s", err, stopOut)
	}
	if daemon.SocketAlive(e.socket) {
		t.Error("the daemon still accepts connections after `sonar daemon stop`")
	}

	// With no daemon, `status` exits non-zero and says so.
	downOut, err := e.command("daemon", "status").CombinedOutput()
	if err == nil {
		t.Errorf("`sonar daemon status` with no daemon exited 0:\n%s", downOut)
	}
	if !strings.Contains(string(downOut), "not running") {
		t.Errorf("status output = %q, want a \"not running\" message", downOut)
	}
}

// TestAutostart is contract §7's autostart path: no daemon, one Connect, a
// daemon within 3 s.
func TestAutostart(t *testing.T) {
	e := newEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if daemon.SocketAlive(e.socket) {
		t.Fatal("something is already listening on the test socket")
	}

	// Connect uses the running executable by default; point it at the built
	// binary and give the child the same isolated HOME. Autostart is off for
	// the test binary as a whole, and this is the test that exists to drive it.
	testenv.AllowAutostart(t)
	t.Setenv("HOME", e.home)
	t.Setenv("USERPROFILE", e.home)
	t.Setenv("SONAR_SOCKET", e.socket)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("SONAR_DB", "")

	c, err := client.Connect(ctx, client.ClientInfo{
		Name: "cli", Version: "itest", Socket: e.socket, BinaryPath: e.bin,
	})
	if err != nil {
		t.Fatalf("autostart: %v", err)
	}
	defer c.Close()

	if c.Hello().PID <= 0 {
		t.Errorf("autostarted daemon reported pid %d", c.Hello().PID)
	}
	// The daemon autostart created is detached: nothing else in this test owns
	// it. Cleanups run after the test's own defers, so this one cannot reuse c
	// (already closed) — it stops the daemon through the CLI and waits for the
	// process, so the temp HOME it holds the lock and log in can be removed.
	daemonPID := c.Hello().PID
	t.Cleanup(func() { e.stopDaemon(daemonPID) })

	var snap state.Snapshot
	if err := c.Call(ctx, "state.snapshot", rpc.StateSnapshotParams{}, &snap); err != nil {
		t.Fatalf("state.snapshot against the autostarted daemon: %v", err)
	}
	if snap.Ports == nil {
		t.Error("snapshot ports marshalled as null")
	}
}

// TestProtocolMismatchIsReported checks the client-side version guard without
// needing a second daemon build.
func TestProtocolMismatchIsReported(t *testing.T) {
	err := client.CheckProtocol("9.0.0")
	var mismatch *client.ProtocolMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("CheckProtocol(9.0.0) = %v, want a ProtocolMismatchError", err)
	}
}

// TestDaemonRestartIsRepeatable is the smoke-test regression: `sonar daemon
// restart` waited only for the socket to stop accepting, but a daemon releases
// its single-instance lock after it closes the socket. The replacement raced
// that release, lost the lock and exited "already running" — two of three
// attempts left nothing running at all. Five restarts in a row must each leave
// a daemon up.
func TestDaemonRestartIsRepeatable(t *testing.T) {
	e := newEnv(t)
	e.serve()
	// After the first restart the daemon is detached, so the env's own cleanup
	// (which kills the foreground child) no longer owns it.
	t.Cleanup(func() { e.stopDaemon(0) })

	lastPID := ""
	for i := 1; i <= 5; i++ {
		out, err := e.command("daemon", "restart").CombinedOutput()
		if err != nil {
			t.Fatalf("restart %d of 5 failed: %v\n%s", i, err, out)
		}

		statusOut, err := e.command("daemon", "status").CombinedOutput()
		if err != nil {
			t.Fatalf("no daemon running after restart %d of 5: %v\nrestart said:\n%s\nstatus said:\n%s",
				i, err, out, statusOut)
		}
		if !strings.Contains(string(statusOut), "running       yes") {
			t.Fatalf("after restart %d of 5 the daemon does not report itself running:\n%s", i, statusOut)
		}

		pid := statusField(string(statusOut), "pid")
		if pid == "" {
			t.Fatalf("`sonar daemon status` reported no pid after restart %d:\n%s", i, statusOut)
		}
		if pid == lastPID {
			t.Errorf("restart %d reported the same pid %s as the previous one; the daemon was not replaced", i, pid)
		}
		lastPID = pid

		// The running daemon must also own the single-instance lock. When the
		// replacement raced the old daemon's teardown the lock file was left
		// holding a dead pid, or removed from under the live daemon.
		if runtime.GOOS != "windows" {
			lockPath := e.lockPath()
			if holder := daemon.LockHolderPID(lockPath); fmt.Sprint(holder) != pid {
				t.Errorf("after restart %d the lock at %s records pid %d, but the daemon reports pid %s",
					i, lockPath, holder, pid)
			}
		}
	}
}

// statusField pulls one value out of `sonar daemon status`'s aligned output.
func statusField(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == name {
			return fields[1]
		}
	}
	return ""
}
