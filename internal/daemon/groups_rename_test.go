package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/state"
)

// renameTree is a project checked out twice — the main checkout and one linked
// worktree — next to an unrelated repository called "taken".
type renameTree struct{ main, wt, other string }

func newRenameTree(t *testing.T, withConfig bool) renameTree {
	t.Helper()
	base := resolvedDir(t, t.TempDir())
	tree := renameTree{
		main:  filepath.Join(base, "shop"),
		wt:    filepath.Join(base, "wt", "feat"),
		other: filepath.Join(base, "taken"),
	}
	admin := filepath.Join(tree.main, ".git", "worktrees", "feat")
	for _, dir := range []string{admin, tree.wt, filepath.Join(tree.other, ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(tree.main, ".git", "HEAD"): "ref: refs/heads/main\n",
		filepath.Join(admin, "HEAD"):             "ref: refs/heads/feat\n",
		filepath.Join(tree.wt, ".git"):           "gitdir: " + admin + "\n",
	}
	if withConfig {
		files[filepath.Join(tree.main, groups.ConfigName)] = "# the shop\nname: shop\n"
		files[filepath.Join(tree.wt, groups.ConfigName)] = "name: shop\n"
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tree
}

func (tree renameTree) rows() []ports.ListeningPort {
	return []ports.ListeningPort{
		{Port: 3000, BindAddress: "127.0.0.1", PID: 501, Process: "node", Cwd: tree.main},
		{Port: 3001, BindAddress: "127.0.0.1", PID: 502, Process: "node", Cwd: tree.wt},
		{Port: 4000, BindAddress: "127.0.0.1", PID: 503, Process: "node", Cwd: tree.other},
	}
}

// listGroups reads groups.list keyed by name.
func (c *testClient) listGroups(t *testing.T) map[string]state.Group {
	t.Helper()
	var res rpc.GroupsListResult
	if e := c.call("groups.list", rpc.Empty{}, &res); e != nil {
		t.Fatalf("groups.list: %v", e)
	}
	out := map[string]state.Group{}
	for _, g := range res.Groups {
		if _, dup := out[g.Name]; dup {
			t.Fatalf("groups.list has two groups named %q", g.Name)
		}
		out[g.Name] = g
	}
	return out
}

// renameGroupCollectingDelta sends groups.rename and returns its result with
// the last groups-moving state.delta that arrived before the reply — the
// delta republish promises is queued ahead of the response.
func (c *testClient) renameGroupCollectingDelta(t *testing.T, p rpc.GroupsRenameParams) (rpc.GroupsRenameResult, state.Delta) {
	t.Helper()
	id := c.send("groups.rename", p)
	var (
		res   rpc.GroupsRenameResult
		delta state.Delta
		seen  bool
	)
	for i := 0; i < 50; i++ {
		m := c.read()
		switch {
		case m.IsNotification() && m.Method == rpc.MethodStateDelta:
			var d state.Delta
			if err := json.Unmarshal(m.Params, &d); err != nil {
				t.Fatalf("decoding state.delta: %v", err)
			}
			if len(d.Groups.Added)+len(d.Groups.Removed) == 0 {
				continue
			}
			delta, seen = d, true
		case m.IsResponse() && string(m.ID) == id:
			if m.Error != nil {
				t.Fatalf("groups.rename: %v", m.Error)
			}
			if err := json.Unmarshal(m.Result, &res); err != nil {
				t.Fatalf("decoding the rename result: %v", err)
			}
			if !seen {
				t.Fatal("the reply arrived before any delta carrying the renamed groups")
			}
			return res, delta
		}
	}
	t.Fatal("no reply to groups.rename")
	return res, delta
}

func sortedNames(gg []state.Group) string {
	out := make([]string, 0, len(gg))
	for _, g := range gg {
		out = append(out, g.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestGroupsRenameStoresAnAliasForAProjectWithoutAFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree := newRenameTree(t, false)
	h, st := storeHarness(t, ctx, tree.rows()...)
	c := h.dial(ctx)
	c.subscribeAndSettle(rpc.StateSubscribeParams{})

	before := c.listGroups(t)
	wt, ok := before["shop@feat"]
	if !ok || wt.Repo != "shop" || wt.Worktree != "feat" || wt.Branch != "feat" {
		t.Fatalf("worktree group before = %+v (%v)", wt, ok)
	}
	if main := before["shop"]; main.Repo != "shop" || main.Worktree != "" || main.Branch != "main" {
		t.Fatalf("main group before = %+v", main)
	}

	// Named by a checkout, routed through an explicit localhost.
	res, delta := c.renameGroupCollectingDelta(t, rpc.GroupsRenameParams{
		HostParams: rpc.HostParams{Host: "localhost"}, Name: "shop@feat", To: "market",
	})
	if !res.OK || res.Name != "market" || strings.Join(res.Affected, ",") != "market,market@feat" {
		t.Fatalf("result = %+v", res)
	}
	removed := append([]string{}, delta.Groups.Removed...)
	sort.Strings(removed)
	if strings.Join(removed, ",") != "shop,shop@feat" || sortedNames(delta.Groups.Added) != "market,market@feat" {
		t.Errorf("delta groups: removed %v, added %s", removed, sortedNames(delta.Groups.Added))
	}

	aliases, err := st.GroupAliases()
	if err != nil || aliases[tree.main] != "market" {
		t.Fatalf("aliases = %v, %v; want the main checkout named market", aliases, err)
	}
	after := c.listGroups(t)
	if _, ok := after["shop"]; ok {
		t.Error("the old project name is still listed")
	}
	if g := after["market@feat"]; g.Repo != "market" || g.Worktree != "feat" || len(g.Members) != 1 || g.Members[0] != 3001 {
		t.Errorf("renamed worktree = %+v", g)
	}
	if g := after["market"]; len(g.Members) != 1 || g.Members[0] != 3000 {
		t.Errorf("renamed main = %+v", g)
	}
	if _, ok := after["taken"]; !ok {
		t.Error("the unrelated project went missing")
	}
}

func TestGroupsRenameWritesTheMainCheckoutsFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree := newRenameTree(t, true)
	h, st := storeHarness(t, ctx, tree.rows()...)
	c := h.dial(ctx)

	if g := c.listGroups(t)["shop@feat"]; g.Source != state.SourceFile ||
		g.ConfigPath == nil || *g.ConfigPath != filepath.Join(tree.wt, groups.ConfigName) {
		t.Fatalf("worktree group before = %+v", g)
	}

	var res rpc.GroupsRenameResult
	if e := c.call("groups.rename", rpc.GroupsRenameParams{Name: "shop", To: "market"}, &res); e != nil {
		t.Fatalf("groups.rename: %v", e)
	}
	if strings.Join(res.Affected, ",") != "market,market@feat" {
		t.Fatalf("affected = %v", res.Affected)
	}

	mainFile, _ := os.ReadFile(filepath.Join(tree.main, groups.ConfigName))
	if !strings.Contains(string(mainFile), "# the shop") || !strings.Contains(string(mainFile), "name: market") {
		t.Errorf("main checkout's file =\n%s", mainFile)
	}
	// The worktree's copy is not the project's name; it is left alone.
	if wtFile, _ := os.ReadFile(filepath.Join(tree.wt, groups.ConfigName)); string(wtFile) != "name: shop\n" {
		t.Errorf("worktree copy was rewritten:\n%s", wtFile)
	}
	if aliases, _ := st.GroupAliases(); len(aliases) != 0 {
		t.Errorf("a file project stored an alias: %v", aliases)
	}
	after := c.listGroups(t)
	for _, name := range []string{"market", "market@feat"} {
		if _, ok := after[name]; !ok {
			t.Errorf("%s missing after the rename: %v", name, after)
		}
	}
	if _, ok := after["shop"]; ok {
		t.Error("the old project name is still listed")
	}

	// And by name, the renamed project's config is found again.
	name := "market@feat"
	var got rpc.GroupsConfigGetResult
	if e := c.call("groups.config.get", rpc.GroupsConfigGetParams{Name: &name}, &got); e != nil {
		t.Fatalf("groups.config.get market@feat: %v", e)
	}
	if got.Path != filepath.Join(tree.wt, groups.ConfigName) {
		t.Errorf("config for market@feat = %s, want the worktree's copy", got.Path)
	}
}

func TestGroupsRenameRefuses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree := newRenameTree(t, false)
	h, _ := storeHarness(t, ctx, tree.rows()...)
	c := h.dial(ctx)

	tests := []struct {
		name string
		p    rpc.GroupsRenameParams
		want string
	}{
		{"no name", rpc.GroupsRenameParams{To: "market"}, "invalid_params"},
		{"no new name", rpc.GroupsRenameParams{Name: "shop"}, "invalid_params"},
		{"an @ in the new name", rpc.GroupsRenameParams{Name: "shop", To: "a@b"}, "invalid_params"},
		{"a slash in the new name", rpc.GroupsRenameParams{Name: "shop", To: "a/b"}, "invalid_params"},
		{"an unknown group", rpc.GroupsRenameParams{Name: "nope", To: "market"}, "not_found"},
		{"another project's name", rpc.GroupsRenameParams{Name: "shop", To: "taken"}, "conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := c.call("groups.rename", tt.p, nil)
			if e == nil || e.Data.Code != tt.want {
				t.Fatalf("error = %+v, want %s", e, tt.want)
			}
		})
	}
	if _, ok := c.listGroups(t)["shop"]; !ok {
		t.Error("a refused rename renamed the project anyway")
	}
}
