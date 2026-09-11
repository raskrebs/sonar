package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
)

// Step 5A.7: `.sonar.yaml` carries worktree_ports, groups.config.get/set read
// and write it, and claims.acquire uses it when the caller names no count.

// mainCheckout makes a git checkout named repo under parent, with a
// `.sonar.yaml` holding body, and returns the checkout directory.
func mainCheckout(t *testing.T, parent, repo, body string) string {
	t.Helper()
	dir := filepath.Join(parent, repo)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, dir, body)
	return dir
}

// linkedWorktree makes a linked worktree of the checkout at mainDir: a `.git`
// file pointing into mainDir/.git/worktrees/<name>, which is how groups.Find
// tells one apart. A non-empty body is written as its `.sonar.yaml`.
func linkedWorktree(t *testing.T, parent, mainDir, name, body string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(mainDir, ".git", "worktrees", name)
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		writeConfig(t, dir, body)
	}
	return dir
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, groups.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func acquireCount(t *testing.T, c *testClient, p rpc.ClaimsAcquireParams) int {
	t.Helper()
	var res rpc.ClaimsAcquireResult
	if e := c.call("claims.acquire", p, &res); e != nil {
		t.Fatalf("claims.acquire %+v: %v", p, e)
	}
	return len(res.Ports)
}

// TestClaimCountDefaultsToWorktreePorts: an omitted count takes the
// project's worktree_ports; an explicit count wins; a project without the key
// or without a config gets one port. The main checkout's file is the one that
// counts when a linked worktree carries a different copy.
func TestClaimCountDefaultsToWorktreePorts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := storeHarness(t, ctx)
	c := h.dial(ctx)

	parent := resolvedDir(t, t.TempDir())
	shop := mainCheckout(t, parent, "shop", "# the shop\nname: shop\nworktree_ports: 3\n")
	shopWT := linkedWorktree(t, parent, shop, "shop-feature", "name: shop\nworktree_ports: 5\n")
	plain := mainCheckout(t, parent, "plain", "name: plain\n")
	// A repository whose main checkout the daemon has never indexed: its
	// worktree's copy is all there is, and it still answers.
	solo := filepath.Join(parent, "solo")
	soloWT := linkedWorktree(t, parent, solo, "solo-feature", "name: solo\nworktree_ports: 2\n")
	for _, dir := range []string{shopWT, shop, plain, soloWT} {
		if err := h.loop.LoadConfig(filepath.Join(dir, groups.ConfigName)); err != nil {
			t.Fatalf("indexing %s: %v", dir, err)
		}
	}

	cases := []struct {
		name string
		p    rpc.ClaimsAcquireParams
		want int
	}{
		{"omitted count takes the main checkout's worktree_ports", rpc.ClaimsAcquireParams{Project: "shop", Worktree: "feature"}, 3},
		{"a key alone names the project", rpc.ClaimsAcquireParams{Key: "shop/other"}, 3},
		{"an explicit count wins", rpc.ClaimsAcquireParams{Project: "shop", Worktree: "one", Count: 1}, 1},
		{"a config without the key", rpc.ClaimsAcquireParams{Project: "plain", Worktree: "feature"}, 1},
		{"no config at all", rpc.ClaimsAcquireParams{Project: "nowhere", Worktree: "feature"}, 1},
		{"only a worktree's config is known", rpc.ClaimsAcquireParams{Project: "solo", Worktree: "feature"}, 2},
	}
	for _, tc := range cases {
		if got := acquireCount(t, c, tc.p); got != tc.want {
			t.Errorf("%s: %d ports, want %d", tc.name, got, tc.want)
		}
	}
}

// TestGroupsConfigWorktreePortsRoundTrip: set writes the key without
// disturbing comments, get reads it back, a claim picks up the new value at
// once, an edit that does not mention the key leaves it alone, and null
// removes it.
func TestGroupsConfigWorktreePortsRoundTrip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := storeHarness(t, ctx)
	c := h.dial(ctx)

	parent := resolvedDir(t, t.TempDir())
	dir := mainCheckout(t, parent, "demo", configFixture)
	path := filepath.Join(dir, groups.ConfigName)

	get := func() json.RawMessage {
		t.Helper()
		var res struct {
			Config map[string]json.RawMessage `json:"config"`
		}
		if e := c.call("groups.config.get", rpc.GroupsConfigGetParams{Path: &path}, &res); e != nil {
			t.Fatalf("groups.config.get: %v", e)
		}
		v, ok := res.Config["worktree_ports"]
		if !ok {
			t.Fatalf("config has no worktree_ports key: %v", res.Config)
		}
		return v
	}
	set := func(params map[string]any) *rpc.GroupsConfigSetResult {
		t.Helper()
		params["path"] = path
		var res rpc.GroupsConfigSetResult
		if e := c.call("groups.config.set", params, &res); e != nil {
			t.Fatalf("groups.config.set %v: %v", params, e)
		}
		return &res
	}

	if got := string(get()); got != "null" {
		t.Fatalf("worktree_ports before any edit = %s, want null", got)
	}

	res := set(map[string]any{"worktree_ports": 4})
	if res.Config.WorktreePorts == nil || *res.Config.WorktreePorts != 4 {
		t.Fatalf("set result worktree_ports = %v, want 4", res.Config.WorktreePorts)
	}
	if len(res.Affected) != 0 {
		t.Errorf("affected = %v, want no service names", res.Affected)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# demo project", "# the database goes first", "worktree_ports: 4"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("file lost %q after the write:\n%s", want, data)
		}
	}
	if got := string(get()); got != "4" {
		t.Errorf("get after set = %s, want 4", got)
	}
	// The write reloads the index, so the very next claim sees the block.
	if got := acquireCount(t, c, rpc.ClaimsAcquireParams{Project: "demo", Worktree: "feature"}); got != 4 {
		t.Errorf("claim after set = %d ports, want 4", got)
	}

	// An edit that does not mention worktree_ports leaves it alone.
	set(map[string]any{"services": []any{map[string]any{"name": "api", "patch": map[string]any{"icon": "server"}}}})
	if got := string(get()); got != "4" {
		t.Errorf("get after an unrelated edit = %s, want 4 kept", got)
	}

	res = set(map[string]any{"worktree_ports": nil})
	if res.Config.WorktreePorts != nil {
		t.Errorf("set result after null = %d, want nil", *res.Config.WorktreePorts)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "worktree_ports") {
		t.Errorf("null did not remove the key:\n%s", data)
	}
	if got := string(get()); got != "null" {
		t.Errorf("get after null = %s, want null", got)
	}
}

// TestGroupsConfigWorktreePortsErrors: an out-of-range value is
// invalid_config and leaves the file byte-identical; a call carrying nothing
// to change is still invalid_params.
func TestGroupsConfigWorktreePortsErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _, path := configHarness(t, ctx)
	c := h.dial(ctx)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []int{0, 101} {
		e := c.call("groups.config.set", map[string]any{"path": path, "worktree_ports": n}, nil)
		if e == nil {
			t.Fatalf("worktree_ports %d should fail", n)
		}
		if e.Code != rpc.CodeInvalidConfig || !strings.Contains(e.Data.Detail, "worktree_ports") {
			t.Errorf("worktree_ports %d: error = %+v, want invalid_config naming the key", n, e)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("a failed write changed the file:\n%s", after)
	}

	if e := c.call("groups.config.set", map[string]any{"path": path}, nil); e == nil || e.Data.Code != "invalid_params" {
		t.Errorf("an empty edit: error = %+v, want invalid_params", e)
	}
}
