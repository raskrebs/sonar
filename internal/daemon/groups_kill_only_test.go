package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/scanner"
)

// onlyFixture is a checkout with two fixed-port services, both listening, the
// way the kill handler sees them: one row per port, attributed to the file.
func onlyFixture(t *testing.T, ctx context.Context) (*testHarness, *testClient, string) {
	t.Helper()
	h := newHarness(t, ctx)
	c := h.dial(ctx)

	parent := resolvedDir(t, t.TempDir())
	dir := mainCheckout(t, parent, "demo", "name: demo\nservices:\n  - name: api\n    cmd: api\n    port: 4331\n  - name: web\n    cmd: web\n    port: 4332\n")
	path := filepath.Join(dir, groups.ConfigName)
	if err := h.loop.LoadConfig(path); err != nil {
		t.Fatalf("indexing %s: %v", path, err)
	}
	h.setRows(
		ports.ListeningPort{Port: 4331, BindAddress: "127.0.0.1", PID: 5331, Process: "api", Cwd: dir},
		ports.ListeningPort{Port: 4332, BindAddress: "127.0.0.1", PID: 5332, Process: "web", Cwd: dir},
	)
	if _, err := h.loop.Snapshot(scanner.Include{}); err != nil {
		t.Fatalf("priming: %v", err)
	}
	return h, c, path
}

func TestGroupsKillOnlyNarrowsToTheNamedServices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, c, path := onlyFixture(t, ctx)

	// Without only, both ports; with only, just api's — by file path and by
	// the group's name alike, since the daemon knows the file either way.
	var all rpc.KillEnvelope
	if e := c.call("groups.kill", rpc.GroupsKillParams{ConfigPath: &path, DryRun: true}, &all); e != nil {
		t.Fatalf("groups.kill: %v", e)
	}
	if len(all.Results) != 2 {
		t.Fatalf("without only: %d rows, want both ports", len(all.Results))
	}
	for _, p := range []rpc.GroupsKillParams{
		{ConfigPath: &path, Only: []string{"api"}, DryRun: true},
		{Name: "demo", Only: []string{"api"}, DryRun: true},
	} {
		var env rpc.KillEnvelope
		if e := c.call("groups.kill", p, &env); e != nil {
			t.Fatalf("groups.kill %+v: %v", p, e)
		}
		if len(env.Results) != 1 || env.Results[0].Port != 4331 {
			t.Fatalf("groups.kill %+v: results = %+v, want only api's 4331", p, env.Results)
		}
	}
}

func TestGroupsKillOnlyRejectsAnUnknownService(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, c, path := onlyFixture(t, ctx)

	e := c.call("groups.kill", rpc.GroupsKillParams{ConfigPath: &path, Only: []string{"ap"}, DryRun: true}, nil)
	if e == nil {
		t.Fatal("groups.kill with an unknown service succeeded")
	}
	if e.Code != rpc.CodeNotFound || e.Data.Code != "not_found" {
		t.Fatalf("error = %d/%q, want 1001/not_found", e.Code, e.Data.Code)
	}
	if e.Data.Hint == "" {
		t.Error("hint is empty; contract §2 asks for an actionable one")
	}
}

func TestGroupsKillOnlyRefusesAListOfNoNames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, c, path := onlyFixture(t, ctx)

	// Blank names are dropped the way groups.start drops them; a list of
	// nothing but blanks must not fall through to stopping the whole group.
	e := c.call("groups.kill", rpc.GroupsKillParams{ConfigPath: &path, Only: []string{" ", ""}, DryRun: true}, nil)
	if e == nil {
		t.Fatal("groups.kill with only of blank names succeeded")
	}
	if e.Code != rpc.CodeInvalidParams {
		t.Fatalf("error = %d/%q, want invalid_params", e.Code, e.Data.Code)
	}
}

func TestGroupsKillOnlyNeedsAFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, ctx)
	c := h.dial(ctx)

	// A group that exists only through a `sonar start` run has no file to
	// resolve service names against.
	h.setRows(ports.ListeningPort{
		Port: 4341, BindAddress: "127.0.0.1", PID: 5341, Process: "listener",
		RunID: "run-1", Tag: "svc", RunGroup: "loose",
	})
	e := c.call("groups.kill", rpc.GroupsKillParams{Name: "loose", Only: []string{"svc"}, DryRun: true}, nil)
	if e == nil {
		t.Fatal("groups.kill with only on a fileless group succeeded")
	}
	if e.Code != rpc.CodeInvalidParams {
		t.Fatalf("error = %d/%q, want invalid_params", e.Code, e.Data.Code)
	}
}
