//go:build integration

package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// replyBudget is what "does not wait on Docker" means below: far under the
// fake docker's hang, and far under the 10 s a client gives up after.
const replyBudget = 2 * time.Second

// fakeDockerOnPath writes a `docker` shell script that logs its arguments to
// calls and then runs body, and returns the PATH entry that puts it first.
func fakeDockerOnPath(t *testing.T, body string) (path, calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker is a shell script")
	}
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\n" + body
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), calls
}

// dockerCalls is how many times the fake docker ran with first argument verb.
func dockerCalls(calls, verb string) int {
	b, err := os.ReadFile(calls)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if line == verb || strings.HasPrefix(line, verb+" ") {
			n++
		}
	}
	return n
}

// TestAHungDockerDoesNotStallKillOrSnapshot is step 5A.8's regression test.
// With a Docker whose backend never answers, `docker ps` hangs; the daemon's
// scans used to wait out the CLI timeout on every call, so a `ports.kill` took
// over 25 s and the `state.snapshot` calls queued behind it timed out.
func TestAHungDockerDoesNotStallKillOrSnapshot(t *testing.T) {
	path, calls := fakeDockerOnPath(t, "exec sleep 60\n")
	listener, err := buildListener()
	if err != nil {
		t.Fatal(err)
	}

	e := newEnv(t)
	e.extra = []string{path}
	e.serve()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c := e.connect(ctx)

	port := freePort(t)
	startListener(t, listener, e.home, port)

	// The kill should land while a `docker ps` is hanging: give the daemon a
	// moment to ask the fake. A daemon that only asks Docker from inside a
	// scan (the bug) has not asked yet, and its kill pays for the hang itself.
	deadline := time.Now().Add(5 * time.Second)
	for dockerCalls(calls, "ps") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	begin := time.Now()
	var killed rpc.KillEnvelope
	if err := c.Call(ctx, "ports.kill", rpc.PortsKillParams{
		Targets: []rpc.Selector{{Port: &port}},
	}, &killed); err != nil {
		t.Fatalf("ports.kill: %v", err)
	}
	killTook := time.Since(begin)
	if len(killed.Results) == 0 || !killed.Results[0].OK {
		t.Fatalf("ports.kill did not kill the listener: %+v", killed.Results)
	}
	if m := killed.Results[0].Method; m == state.MethodDockerStop {
		t.Errorf("the listener was stopped with %s", m)
	}
	t.Logf("ports.kill took %s", killTook)
	if killTook > replyBudget {
		t.Errorf("ports.kill took %s with docker hanging, want under %s", killTook, replyBudget)
	}

	// Three snapshots, spaced past the RPC cache so each one scans.
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(2100 * time.Millisecond)
		}
		begin := time.Now()
		var snap state.Snapshot
		if err := c.Call(ctx, "state.snapshot", rpc.StateSnapshotParams{}, &snap); err != nil {
			t.Fatalf("state.snapshot %d: %v", i+1, err)
		}
		took := time.Since(begin)
		t.Logf("state.snapshot %d took %s", i+1, took)
		if took > replyBudget {
			t.Errorf("state.snapshot %d took %s with docker hanging, want under %s", i+1, took, replyBudget)
		}
		for _, p := range snap.Ports {
			if p.Port == port {
				t.Errorf("snapshot %d still lists the killed port %d", i+1, port)
			}
		}
	}

	// Backoff: one hung attempt, then 5 s before the next. Scans poked the
	// watcher dozens of times meanwhile; none of them may have reached Docker.
	n := dockerCalls(calls, "ps")
	t.Logf("docker ps ran %d times in the %s since the kill began", n, time.Since(begin))
	if n == 0 {
		t.Error("the daemon never ran docker ps, so this test proved nothing")
	}
	if n > 3 {
		t.Errorf("docker ps ran %d times against a hung Docker; the watcher is not backing off", n)
	}
}

// TestAHealthyDockerStillEnrichesAndGroups is the other half: with a Docker
// that answers, a port it publishes still carries the container, the compose
// labels and the compose project's group — now from the watcher's cache.
func TestAHealthyDockerStillEnrichesAndGroups(t *testing.T) {
	listener, err := buildListener()
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	composeDir := t.TempDir()
	ps := "itest-web-1\tnginx:itest\t0.0.0.0:" + strconv.Itoa(port) + "->80/tcp\tweb\titestproj\t" + composeDir
	path, calls := fakeDockerOnPath(t, "case \"$1\" in\nps) cat <<'EOF'\n"+ps+"\nEOF\n;;\n*) exit 1 ;;\nesac\n")

	e := newEnv(t)
	e.extra = []string{path}
	e.serve()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c := e.connect(ctx)

	startListener(t, listener, e.home, port)

	var row *state.Port
	deadline := time.Now().Add(20 * time.Second)
	for row == nil || row.Docker == nil {
		if time.Now().After(deadline) {
			t.Fatalf("port %d was never enriched from docker (docker ps ran %d times); last row: %+v",
				port, dockerCalls(calls, "ps"), row)
		}
		time.Sleep(250 * time.Millisecond)
		var snap state.Snapshot
		if err := c.Call(ctx, "state.snapshot", rpc.StateSnapshotParams{}, &snap); err != nil {
			t.Fatalf("state.snapshot: %v", err)
		}
		row = nil
		for i := range snap.Ports {
			if snap.Ports[i].Port == port {
				row = &snap.Ports[i]
			}
		}
	}

	d := row.Docker
	if row.Type != state.TypeDocker {
		t.Errorf("type = %s, want docker", row.Type)
	}
	if d.Container != "itest-web-1" || d.Image != "nginx:itest" || d.ComposeService != "web" ||
		d.ComposeProject != "itestproj" || d.ContainerPort != 80 {
		t.Errorf("docker = %+v", *d)
	}
	if row.Group == nil || *row.Group != "itestproj" {
		t.Errorf("group = %v, want the compose project itestproj", deref(row.Group))
	}
}
