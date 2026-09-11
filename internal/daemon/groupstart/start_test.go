package groupstart

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/testenv"
)

// The child side of every test here: re-running this binary with
// SONAR_TEST_LISTEN set makes it a tiny service that binds the port sonar told
// it to and waits. Using the test binary rather than a compiled helper keeps
// the test hermetic and works the same on every OS.
const (
	envListen = "SONAR_TEST_LISTEN"
	envDelay  = "SONAR_TEST_LISTEN_DELAY"
	// envEcho names variables, comma separated, the child prints as NAME=value
	// before it listens, so a test can read the environment a service got.
	envEcho = "SONAR_TEST_ECHO"
)

// TestMain doubles as the service a group starts. A service is not a test run,
// so only the test branch is isolated; the child inherits the environment its
// parent was already isolated into.
func TestMain(m *testing.M) {
	if os.Getenv(envListen) == "" {
		os.Exit(testenv.Run(m))
	}
	if d, err := time.ParseDuration(os.Getenv(envDelay)); err == nil && d > 0 {
		time.Sleep(d)
	}
	for _, name := range strings.Split(os.Getenv(envEcho), ",") {
		if name != "" {
			fmt.Printf("%s=%s\n", name, os.Getenv(name))
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("SONAR_PORT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ln.Close()
	fmt.Println("listening on", os.Getenv("SONAR_PORT"))
	// Accept forever. `select {}` would be a deadlock the runtime panics on;
	// blocking on the listener is what a real service does anyway.
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}

// serviceCmd is the `cmd:` a test writes into a `.sonar.yaml`: this very test
// binary, in listen mode.
func serviceCmd(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Single-quoted for YAML, double-quoted for the argv splitter: the value
	// is one scalar that splits into [binary, -test.run=...].
	return fmt.Sprintf("'%q -test.run=TestNothing'", self)
}

// dumpLogs prints what the started services wrote, so a failure says why the
// child did not come up instead of only that it did not.
func dumpLogs(t *testing.T, chunks []rpc.GroupsStartChunk) {
	t.Helper()
	for _, c := range chunks {
		if c.LogPath == "" {
			continue
		}
		if raw, err := os.ReadFile(c.LogPath); err == nil {
			t.Logf("%s log:\n%s", c.Service, raw)
		}
	}
}

// freePort returns a port nothing is listening on right now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// probeScan is the daemon's scan in these tests: it dials the ports the test
// cares about and reports the ones that answer, attributed to dir. Real
// sockets, deterministic result.
func probeScan(dir string, watch []int) func(scanner.Include) ([]ports.ListeningPort, error) {
	return func(scanner.Include) ([]ports.ListeningPort, error) {
		var out []ports.ListeningPort
		for _, port := range watch {
			conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 100*time.Millisecond)
			if err != nil {
				continue
			}
			conn.Close()
			out = append(out, ports.ListeningPort{
				Port: port, BindAddress: "127.0.0.1", IPVersion: "ipv4",
				PID: os.Getpid(), Process: "listener", Cwd: dir,
			})
		}
		return out, nil
	}
}

// startDaemon runs a real daemon on a temp socket with the given scan, and
// returns a connected client.
func startDaemon(t *testing.T, ctx context.Context, dir string, watch []int) *client.Client {
	t.Helper()
	return startDaemonWith(t, ctx, probeScan(dir, watch))
}

// watchList is a scan's watch list that grows while a test runs: a `port:
// auto` service's port is only known once groups.start reports it.
type watchList struct {
	mu   sync.Mutex
	pids map[int]int // port -> pid of the service listening on it
}

func (w *watchList) add(port, pid int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pids == nil {
		w.pids = map[int]int{}
	}
	w.pids[port] = pid
}

// watchScan dials every watched port and reports the ones that answer,
// attributed to the service's own pid so the run registry claims them the way
// it claims a real listener in a service's process tree.
func watchScan(dir string, w *watchList) func(scanner.Include) ([]ports.ListeningPort, error) {
	return func(scanner.Include) ([]ports.ListeningPort, error) {
		w.mu.Lock()
		watched := make(map[int]int, len(w.pids))
		for port, pid := range w.pids {
			watched[port] = pid
		}
		w.mu.Unlock()
		var out []ports.ListeningPort
		for port, pid := range watched {
			if !dialable(port) {
				continue
			}
			out = append(out, ports.ListeningPort{
				Port: port, BindAddress: "127.0.0.1", IPVersion: "ipv4",
				PID: pid, Process: "listener", Cwd: dir,
			})
		}
		return out, nil
	}
}

// startDaemonWith is startDaemon with the scan supplied by the caller.
func startDaemonWith(t *testing.T, ctx context.Context, scan func(scanner.Include) ([]ports.ListeningPort, error)) *client.Client {
	t.Helper()
	// A unix socket path is capped at ~104 bytes, and t.TempDir() spells the
	// test's name into the path, so the socket gets its own short directory.
	sockDir, err := os.MkdirTemp("", "sn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "d.sock")

	srv := daemon.New(daemon.Options{
		Socket:  socket,
		Version: "test",
		DBPath:  filepath.Join(sockDir, "sonar.db"),
		Scanner: scanner.New(scanner.Options{
			DaemonVersion: "test",
			Scan:          scan,
		}),
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		srv.Shutdown()
		<-srv.Done()
	})

	var c *client.Client
	for i := 0; i < 100; i++ {
		c, err = client.Dial(ctx, client.ClientInfo{Name: "cli", Version: "test", Socket: socket})
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		select {
		case serr := <-serveErr:
			t.Fatalf("the test daemon did not start: %v (dial: %v)", serr, err)
		default:
		}
		t.Fatalf("connecting to the test daemon: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// isolate points HOME, the log directory and the run mirror at a temp tree, so
// a test never writes into the developer's real config. It returns a project
// directory *inside* that home: the daemon refuses to start a command outside
// the user's home unless asked to, and these tests should exercise the ordinary
// path rather than the opt-out.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SONAR_LOG_DIR", filepath.Join(home, "logs"))
	t.Setenv(envListen, "1")

	dir := filepath.Join(home, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// collect drains a groups.start stream into its chunks and its end payload.
func collect(t *testing.T, s *client.Stream) ([]rpc.GroupsStartChunk, rpc.GroupsStartEnd) {
	t.Helper()
	var chunks []rpc.GroupsStartChunk
	done := make(chan struct{})
	go func() {
		defer close(done)
		for raw := range s.Chunks() {
			var c rpc.GroupsStartChunk
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Errorf("decoding a chunk: %v", err)
				continue
			}
			chunks = append(chunks, c)
		}
	}()

	select {
	case end := <-s.End():
		<-done
		if end.Err != nil {
			t.Fatalf("stream ended with an error: %v", end.Err)
		}
		var summary rpc.GroupsStartEnd
		if err := end.Decode(&summary); err != nil {
			t.Fatalf("decoding the end payload: %v", err)
		}
		return chunks, summary
	case <-time.After(60 * time.Second):
		t.Fatal("groups.start never ended")
		return nil, rpc.GroupsStartEnd{}
	}
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, groups.ConfigName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary re-exec helper and unix socket are not set up for Windows here")
	}
}

// TestNothing is the -test.run target the spawned child uses; TestMain turns
// into a listener before it would ever run.
func TestNothing(t *testing.T) {}

// TestStartsInDependencyOrderAndWaits is the heart of `sonar up`: db is
// started, api waits for db's port before it is spawned, and every service is
// reported as it happens.
func TestStartsInDependencyOrderAndWaits(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPort, apiPort := freePort(t), freePort(t)
	cmd := serviceCmd(t)
	path := writeConfig(t, dir, fmt.Sprintf(`name: itest
services:
  - name: api
    cmd: %s
    port: %d
    depends_on: [db]
  - name: db
    cmd: %s
    port: %d
`, cmd, apiPort, cmd, dbPort))

	c := startDaemon(t, ctx, dir, []int{dbPort, apiPort})

	var start rpc.GroupsStartResult
	s, err := c.Stream(ctx, "groups.start", rpc.GroupsStartParams{ConfigPath: &path}, &start)
	if err != nil {
		t.Fatalf("groups.start: %v", err)
	}
	defer s.Close()
	if !start.OK || start.SubscriptionID == "" {
		t.Fatalf("reply = %+v, want ok with a subscription id", start)
	}

	chunks, end := collect(t, s)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %+v, want one per service", chunks)
	}
	if chunks[0].Service != "db" || chunks[1].Service != "api" {
		t.Fatalf("order = %s, %s; want db then api", chunks[0].Service, chunks[1].Service)
	}
	for _, ch := range chunks {
		if ch.Error != "" || ch.PID == 0 || ch.LogPath == "" {
			dumpLogs(t, chunks)
			t.Fatalf("chunk %+v should carry a pid and a log path", ch)
		}
		if !strings.Contains(ch.LogPath, filepath.Join("itest", ch.Service+".log")) {
			t.Errorf("log path %q is not under the group directory", ch.LogPath)
		}
	}
	if len(end.Started) != 2 || len(end.Errors) != 0 {
		t.Fatalf("end = %+v", end)
	}

	// api was only spawned once db was actually listening, which is the whole
	// point of depends_on.
	if !dialable(dbPort) {
		t.Error("db is not listening after groups.start reported it started")
	}

	t.Cleanup(func() { killPIDs(chunks) })
}

// TestAutoPortsAreAssignedAndReferenced: two `port: auto` services get ports
// from the claim range, api waits for db on db's assigned port, and api's env
// carries db's port through ${db.port} next to what the caller sent.
func TestAutoPortsAreAssignedAndReferenced(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := serviceCmd(t)
	path := writeConfig(t, dir, fmt.Sprintf(`name: autotest
services:
  - name: api
    cmd: %s
    port: auto
    depends_on: [db]
    env:
      SONAR_TEST_ECHO: DB_URL,FROM_CALLER,PORT
      DB_URL: postgres://localhost:${db.port}/app
  - name: db
    cmd: %s
    port: auto
`, cmd, cmd))

	watch := &watchList{}
	c := startDaemonWith(t, ctx, watchScan(dir, watch))

	var start rpc.GroupsStartResult
	s, err := c.Stream(ctx, "groups.start", rpc.GroupsStartParams{
		ConfigPath: &path,
		Env:        map[string]string{"FROM_CALLER": "yes"},
	}, &start)
	if err != nil {
		t.Fatalf("groups.start: %v", err)
	}
	defer s.Close()

	chunks, end := collectWatching(t, s, watch)
	t.Cleanup(func() { killPIDs(chunks) })
	if len(end.Started) != 2 || len(end.Errors) != 0 {
		dumpLogs(t, chunks)
		t.Fatalf("end = %+v, chunks = %+v", end, chunks)
	}
	db, api := chunks[0], chunks[1]
	if db.Service != "db" || api.Service != "api" {
		t.Fatalf("order = %s, %s; want db then api", db.Service, api.Service)
	}
	for _, ch := range chunks {
		if ch.Port < 10000 || ch.Port > 32699 {
			t.Errorf("%s port = %d, want one from the claim range", ch.Service, ch.Port)
		}
	}
	if db.Port == api.Port {
		t.Fatalf("db and api were both given %d", db.Port)
	}

	want := []string{
		fmt.Sprintf("DB_URL=postgres://localhost:%d/app", db.Port),
		"FROM_CALLER=yes",
		fmt.Sprintf("PORT=%d", api.Port),
		fmt.Sprintf("listening on %d", api.Port),
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, _ := os.ReadFile(api.LogPath)
		missing := ""
		for _, w := range want {
			if !strings.Contains(string(raw), w) {
				missing = w
				break
			}
		}
		if missing == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("api log is missing %q:\n%s", missing, raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// collectWatching is collect for a scan whose watch list grows: every service
// reported started is added to it, so the next scan sees its port.
func collectWatching(t *testing.T, s *client.Stream, w *watchList) ([]rpc.GroupsStartChunk, rpc.GroupsStartEnd) {
	t.Helper()
	var chunks []rpc.GroupsStartChunk
	done := make(chan struct{})
	go func() {
		defer close(done)
		for raw := range s.Chunks() {
			var c rpc.GroupsStartChunk
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Errorf("decoding a chunk: %v", err)
				continue
			}
			if c.Port > 0 && c.PID > 0 {
				w.add(c.Port, c.PID)
			}
			chunks = append(chunks, c)
		}
	}()
	select {
	case end := <-s.End():
		<-done
		if end.Err != nil {
			t.Fatalf("stream ended with an error: %v", end.Err)
		}
		var summary rpc.GroupsStartEnd
		if err := end.Decode(&summary); err != nil {
			t.Fatalf("decoding the end payload: %v", err)
		}
		return chunks, summary
	case <-time.After(60 * time.Second):
		t.Fatal("groups.start never ended")
		return nil, rpc.GroupsStartEnd{}
	}
}

// TestSkipsServicesThatAreAlreadyRunning: `sonar up` is safe to run twice.
func TestSkipsServicesThatAreAlreadyRunning(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Something else is already on the port the service declares.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port

	path := writeConfig(t, dir, fmt.Sprintf(`name: itest
services:
  - name: web
    cmd: %s
    port: %d
`, serviceCmd(t), taken))

	c := startDaemon(t, ctx, dir, []int{taken})

	var start rpc.GroupsStartResult
	s, err := c.Stream(ctx, "groups.start", rpc.GroupsStartParams{ConfigPath: &path}, &start)
	if err != nil {
		t.Fatalf("groups.start: %v", err)
	}
	defer s.Close()

	chunks, end := collect(t, s)
	if len(chunks) != 1 || !chunks[0].Skipped {
		t.Fatalf("chunks = %+v, want one skip", chunks)
	}
	if !strings.Contains(chunks[0].Reason, "already running") {
		t.Errorf("reason = %q", chunks[0].Reason)
	}
	if len(end.Skipped) != 1 || len(end.Started) != 0 {
		t.Fatalf("end = %+v", end)
	}
}

// TestReportsADependencyTimeout: a dependency that never comes up is reported
// against the service that waited, and everything independent still starts.
func TestReportsADependencyTimeout(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)

	old := dependencyTimeout
	dependencyTimeout = 600 * time.Millisecond
	t.Cleanup(func() { dependencyTimeout = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deadPort, livePort := freePort(t), freePort(t)
	path := writeConfig(t, dir, fmt.Sprintf(`name: itest
services:
  - name: ghost
    cmd: %s --never-listens
    port: %d
  - name: api
    cmd: %s
    port: %d
    depends_on: [ghost]
`, exitsImmediately(t), deadPort, serviceCmd(t), livePort))

	c := startDaemon(t, ctx, dir, []int{deadPort, livePort})

	var start rpc.GroupsStartResult
	s, err := c.Stream(ctx, "groups.start", rpc.GroupsStartParams{ConfigPath: &path}, &start)
	if err != nil {
		t.Fatalf("groups.start: %v", err)
	}
	defer s.Close()

	chunks, end := collect(t, s)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %+v", chunks)
	}
	api := chunks[1]
	if api.Service != "api" || api.Error == "" {
		t.Fatalf("api chunk = %+v, want a timeout error", api)
	}
	if !strings.Contains(api.Error, "timed out") || !strings.Contains(api.Error, "ghost") {
		t.Errorf("error = %q, want it to name the dependency", api.Error)
	}
	if len(end.Errors) != 1 || end.Errors[0] != "api" {
		t.Fatalf("end = %+v", end)
	}
}

// TestOnlyFiltersAndReportsATypo.
func TestOnlyFiltersAndReportsATypo(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	one, two := freePort(t), freePort(t)
	cmd := serviceCmd(t)
	path := writeConfig(t, dir, fmt.Sprintf(`name: itest
services:
  - name: one
    cmd: %s
    port: %d
  - name: two
    cmd: %s
    port: %d
`, cmd, one, cmd, two))

	c := startDaemon(t, ctx, dir, []int{one, two})

	var start rpc.GroupsStartResult
	s, err := c.Stream(ctx, "groups.start", rpc.GroupsStartParams{
		ConfigPath: &path, Only: []string{"two"},
	}, &start)
	if err != nil {
		t.Fatalf("groups.start: %v", err)
	}
	defer s.Close()

	chunks, end := collect(t, s)
	if len(chunks) != 1 || chunks[0].Service != "two" {
		t.Fatalf("chunks = %+v, want only two", chunks)
	}
	if len(end.Started) != 1 {
		t.Fatalf("end = %+v", end)
	}
	t.Cleanup(func() { killPIDs(chunks) })

	// A name the file does not declare is an error, not a silent no-op.
	_, err = c.Stream(ctx, "groups.start", rpc.GroupsStartParams{
		ConfigPath: &path, Only: []string{"thre"},
	}, nil)
	var re *rpc.Error
	if err == nil {
		t.Fatal("an unknown --only entry should fail")
	}
	if !asRPCError(err, &re) || re.Data.Code != "not_found" {
		t.Fatalf("error = %v, want not_found", err)
	}
}

// TestStartNeedsAName rejects a call that names neither a group nor a file.
func TestStartNeedsAName(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := startDaemon(t, ctx, dir, nil)
	_, err := c.Stream(ctx, "groups.start", rpc.GroupsStartParams{}, nil)
	var re *rpc.Error
	if err == nil || !asRPCError(err, &re) || re.Data.Code != "invalid_params" {
		t.Fatalf("error = %v, want invalid_params", err)
	}
}

func asRPCError(err error, out **rpc.Error) bool {
	e, ok := err.(*rpc.Error)
	if ok {
		*out = e
	}
	return ok
}

// exitsImmediately is a command that starts, does nothing and stops, standing
// in for a service that never manages to listen.
func exitsImmediately(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/usr/bin/true", "/bin/true"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("no true(1) on this system")
	return ""
}

func dialable(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// killPIDs stops the children a test started, so a failed run does not leave
// listeners behind.
func killPIDs(chunks []rpc.GroupsStartChunk) {
	for _, c := range chunks {
		if c.PID <= 0 {
			continue
		}
		if p, err := os.FindProcess(c.PID); err == nil {
			_ = p.Kill()
		}
	}
}
