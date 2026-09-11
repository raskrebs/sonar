package groups

import (
	"errors"
	"strings"
	"testing"
)

// oddlyOrdered is the file the add/rename/remove round trips start from:
// comments above, beside and inside the services list, top-level keys in an
// order nobody would choose, and one service whose own keys are back to front.
// Every assertion below is about this file coming back recognisable.
const oddlyOrdered = `# The sonar repo itself.
ports: [9229] # the debugger
services:
  # The database has to be up before anything else.
  - port: 5432
    name: db
    cmd: docker compose up db
  - name: api
    cmd: uv run api # served by uvicorn
    cwd: backend
    port: 8000
    health: /health
    depends_on: [db, cache]
  # Optional, but everything is slower without it.
  - name: cache
    port: 6379
name: sonar
`

// allComments are the comments this file carries; none of them may be lost by
// an edit, wherever in the file the edit lands.
var allComments = []string{
	"# The sonar repo itself.",
	"# the debugger",
	"# The database has to be up before anything else.",
	"# served by uvicorn",
	"# Optional, but everything is slower without it.",
}

func requireComments(t *testing.T, out string) {
	t.Helper()
	for _, want := range allComments {
		if !strings.Contains(out, want) {
			t.Errorf("comment %q was lost:\n%s", want, out)
		}
	}
}

// topLevelKeys is the order of the mapping keys at column zero.
func topLevelKeys(out string) []string {
	var keys []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '#' || line[0] == '-' {
			continue
		}
		if i := strings.Index(line, ":"); i > 0 {
			keys = append(keys, line[:i])
		}
	}
	return keys
}

// serviceEntry is one service's lines, found by its name key wherever in the
// entry that key sits. patch_test.go's serviceBlock only finds a service whose
// first key is `name`, and half the point of this file is that it need not be.
func serviceEntry(t *testing.T, yaml, name string) []string {
	t.Helper()
	var blocks [][]string
	var cur []string
	for _, line := range strings.Split(yaml, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "- "):
			if cur != nil {
				blocks = append(blocks, cur)
			}
			cur = []string{strings.TrimPrefix(trimmed, "- ")}
		case cur != nil && strings.HasPrefix(line, "    ") && trimmed != "":
			cur = append(cur, trimmed)
		case cur != nil:
			blocks = append(blocks, cur)
			cur = nil
		}
	}
	if cur != nil {
		blocks = append(blocks, cur)
	}
	for _, block := range blocks {
		for _, line := range block {
			if line == "name: "+name {
				return block
			}
		}
	}
	t.Fatalf("no service %q in:\n%s", name, yaml)
	return nil
}

// serviceNamesInFile is the services in the order the file lists them.
func serviceNamesInFile(t *testing.T, path string) []string {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := make([]string, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		out = append(out, s.Name)
	}
	return out
}

// TestAddServiceKeepsCommentsAndOrder: a service appended by the daemon must
// leave every other line of the file where the author put it.
func TestAddServiceKeepsCommentsAndOrder(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)

	cfg, err := EditServices(path, ConfigEdit{Add: []ServiceAdd{{
		Name:        "worker",
		Port:        9000,
		Cmd:         "uv run worker",
		Health:      "/live",
		Description: "background jobs",
		Icon:        "gear",
		Color:       "#7c3aed",
		DependsOn:   []string{"db"},
	}}})
	if err != nil {
		t.Fatalf("EditServices: %v", err)
	}
	out := read(t, path)
	requireComments(t, out)

	if got := topLevelKeys(out); !equalStrings(got, []string{"ports", "services", "name"}) {
		t.Errorf("top-level key order = %v, want the file's own ports, services, name:\n%s", got, out)
	}
	// db still leads with the port key it was written with.
	if got := keysOf(serviceEntry(t, out, "db")); !equalStrings(got, []string{"port", "name", "cmd"}) {
		t.Errorf("db keys = %v, want the file's own order", got)
	}
	// The new service is last, with its keys in the order a hand-written file
	// would use.
	if got := serviceNamesInFile(t, path); !equalStrings(got, []string{"db", "api", "cache", "worker"}) {
		t.Errorf("services = %v, want worker appended last", got)
	}
	want := []string{"name", "cmd", "port", "health", "description", "icon", "color", "depends_on"}
	if got := keysOf(serviceEntry(t, out, "worker")); !equalStrings(got, want) {
		t.Errorf("worker keys = %v, want %v:\n%s", got, want, out)
	}
	if !strings.Contains(out, "depends_on: [db]") {
		t.Errorf("depends_on should be written in flow style:\n%s", out)
	}

	svc := cfg.Services[3]
	if svc.Name != "worker" || svc.Port != 9000 || svc.Color != "#7c3aed" || len(svc.DependsOn) != 1 {
		t.Errorf("returned service = %+v", svc)
	}
}

// TestAddServiceConflicts: a name or a port the file already has is refused,
// and the file is not touched.
func TestAddServiceConflicts(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)
	before := read(t, path)

	_, err := EditServices(path, ConfigEdit{Add: []ServiceAdd{{Name: "api", Port: 9000}}})
	var clash *ServiceConflictError
	if !errors.As(err, &clash) {
		t.Fatalf("adding a duplicate name = %v, want *ServiceConflictError", err)
	}
	if clash.Name != "api" || clash.Port != 0 {
		t.Errorf("conflict = %+v, want the name api", clash)
	}

	_, err = EditServices(path, ConfigEdit{Add: []ServiceAdd{{Name: "worker", Port: 8000}}})
	if !errors.As(err, &clash) {
		t.Fatalf("adding a duplicate port = %v, want *ServiceConflictError", err)
	}
	if clash.Port != 8000 || clash.Name != "api" {
		t.Errorf("conflict = %+v, want port 8000 held by api", clash)
	}

	// Two adds of the same name in one call clash with each other.
	_, err = EditServices(path, ConfigEdit{Add: []ServiceAdd{
		{Name: "worker", Port: 9000},
		{Name: "worker", Port: 9001},
	}})
	if !errors.As(err, &clash) {
		t.Fatalf("adding the same name twice = %v, want *ServiceConflictError", err)
	}

	if after := read(t, path); after != before {
		t.Errorf("a refused add changed the file:\n%s", after)
	}
}

// TestAddServiceCreatesTheServicesKey: a file that only names the group grows a
// services list in the right place.
func TestAddServiceCreatesTheServicesKey(t *testing.T) {
	path := writePatchable(t, "# just a name\nname: sonar\nports: [9229]\n")

	if _, err := EditServices(path, ConfigEdit{Add: []ServiceAdd{{Name: "api", Port: 8000}}}); err != nil {
		t.Fatalf("EditServices: %v", err)
	}
	out := read(t, path)
	if !strings.Contains(out, "# just a name") {
		t.Errorf("the comment was lost:\n%s", out)
	}
	if got := topLevelKeys(out); !equalStrings(got, []string{"name", "services", "ports"}) {
		t.Errorf("top-level key order = %v, want services between name and ports:\n%s", got, out)
	}
}

// TestRenameServiceRewritesEveryReference is the point of doing this in the
// daemon: a rename that missed a depends_on would leave an invalid file behind.
func TestRenameServiceRewritesEveryReference(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)

	if _, err := EditServices(path, ConfigEdit{Rename: []ServiceRename{{From: "db", To: "database"}}}); err != nil {
		t.Fatalf("EditServices: %v", err)
	}
	out := read(t, path)
	requireComments(t, out)

	if got := serviceNamesInFile(t, path); !equalStrings(got, []string{"database", "api", "cache"}) {
		t.Errorf("services = %v, want db renamed in place", got)
	}
	if !strings.Contains(out, "depends_on: [database, cache]") {
		t.Errorf("the depends_on reference was not rewritten:\n%s", out)
	}
	// The renamed service keeps its own odd key order.
	if got := keysOf(serviceEntry(t, out, "database")); !equalStrings(got, []string{"port", "name", "cmd"}) {
		t.Errorf("database keys = %v, want the file's own order", got)
	}
}

func TestRenameServiceErrors(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)
	before := read(t, path)

	_, err := EditServices(path, ConfigEdit{Rename: []ServiceRename{{From: "worker", To: "jobs"}}})
	var missing *ServiceNotFoundError
	if !errors.As(err, &missing) || missing.Name != "worker" {
		t.Fatalf("renaming an unknown service = %v, want *ServiceNotFoundError", err)
	}

	_, err = EditServices(path, ConfigEdit{Rename: []ServiceRename{{From: "db", To: "api"}}})
	var clash *ServiceConflictError
	if !errors.As(err, &clash) || clash.Name != "api" {
		t.Fatalf("renaming onto an existing name = %v, want *ServiceConflictError", err)
	}

	if after := read(t, path); after != before {
		t.Errorf("a refused rename changed the file:\n%s", after)
	}
}

// TestRenameToItselfIsAllowed: a no-op rename is not a conflict with itself.
func TestRenameToItselfIsAllowed(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)
	if _, err := EditServices(path, ConfigEdit{Rename: []ServiceRename{{From: "db", To: "db"}}}); err != nil {
		t.Fatalf("renaming a service to its own name: %v", err)
	}
}

// TestRemoveServiceDropsDependsOn: the removed name may not survive anywhere,
// or the file it leaves behind would not validate.
func TestRemoveServiceDropsDependsOn(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)

	if _, err := EditServices(path, ConfigEdit{Remove: []string{"db"}}); err != nil {
		t.Fatalf("EditServices: %v", err)
	}
	out := read(t, path)

	if got := serviceNamesInFile(t, path); !equalStrings(got, []string{"api", "cache"}) {
		t.Errorf("services = %v, want db gone", got)
	}
	if !strings.Contains(out, "depends_on: [cache]") {
		t.Errorf("db was not dropped from depends_on:\n%s", out)
	}
	// The comments belonging to the services that are left stay put; the one
	// introducing db goes with it.
	for _, want := range []string{"# The sonar repo itself.", "# served by uvicorn", "# Optional, but everything is slower without it."} {
		if !strings.Contains(out, want) {
			t.Errorf("comment %q was lost:\n%s", want, out)
		}
	}
	if strings.Contains(out, "# The database has to be up") {
		t.Errorf("the removed service's own comment should go with it:\n%s", out)
	}

	// Removing the last dependency takes the now-empty key with it.
	if _, err := EditServices(path, ConfigEdit{Remove: []string{"cache"}}); err != nil {
		t.Fatalf("EditServices: %v", err)
	}
	if out := read(t, path); strings.Contains(out, "depends_on") {
		t.Errorf("an emptied depends_on should be removed, not left as []:\n%s", out)
	}
}

func TestRemoveServiceUnknown(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)
	before := read(t, path)

	var missing *ServiceNotFoundError
	if _, err := EditServices(path, ConfigEdit{Remove: []string{"worker"}}); !errors.As(err, &missing) {
		t.Fatalf("removing an unknown service = %v, want *ServiceNotFoundError", err)
	}
	if after := read(t, path); after != before {
		t.Errorf("a refused remove changed the file:\n%s", after)
	}
}

// TestEditIsAtomic: the four lists are one change. A rename that clashes after
// an add has already been applied to the tree must leave the disk untouched.
func TestEditIsAtomic(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)
	before := read(t, path)

	_, err := EditServices(path, ConfigEdit{
		Add:      []ServiceAdd{{Name: "worker", Port: 9000}},
		Rename:   []ServiceRename{{From: "db", To: "api"}},
		Services: []ServiceEdit{{Name: "cache", Patch: ServicePatch{}.SetString(FieldIcon, "zap")}},
	})
	var clash *ServiceConflictError
	if !errors.As(err, &clash) {
		t.Fatalf("the clashing rename should fail the whole edit, got %v", err)
	}
	if after := read(t, path); after != before {
		t.Errorf("a failed edit wrote to the file:\n%s", after)
	}
}

// TestEditRemoveThenAddReusesThePort is why remove runs first: swapping a
// service for another on the same port is one call, not two.
func TestEditRemoveThenAddReusesThePort(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)

	cfg, err := EditServices(path, ConfigEdit{
		Remove: []string{"cache"},
		Add:    []ServiceAdd{{Name: "redis", Port: 6379}},
	})
	if err != nil {
		t.Fatalf("EditServices: %v", err)
	}
	if got := serviceNamesInFile(t, path); !equalStrings(got, []string{"db", "api", "redis"}) {
		t.Errorf("services = %v, want cache replaced by redis", got)
	}
	if cfg.Services[2].Port != 6379 {
		t.Errorf("redis = %+v, want port 6379", cfg.Services[2])
	}
}

// TestEditCombinesEveryList: rename, add and patch in one call, applied in the
// documented order so each list sees what the last one did.
func TestEditCombinesEveryList(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)

	cfg, err := EditServices(path, ConfigEdit{
		Remove: []string{"cache"},
		Rename: []ServiceRename{{From: "db", To: "database"}},
		Add:    []ServiceAdd{{Name: "worker", Port: 9000, DependsOn: []string{"database"}}},
		Services: []ServiceEdit{
			{Name: "worker", Patch: ServicePatch{}.SetString(FieldColor, "green")},
		},
	})
	if err != nil {
		t.Fatalf("EditServices: %v", err)
	}
	if got := serviceNamesInFile(t, path); !equalStrings(got, []string{"database", "api", "worker"}) {
		t.Errorf("services = %v", got)
	}
	if cfg.Services[2].Color != "green" {
		t.Errorf("the patch did not reach the service the add created: %+v", cfg.Services[2])
	}
	// The patch list may name a service the add list just created, and the add
	// may depend on a service the rename just renamed.
	if got := cfg.Services[2].DependsOn; len(got) != 1 || got[0] != "database" {
		t.Errorf("worker depends_on = %v, want the renamed database", got)
	}
	// Every comment but the one introducing the removed service survives.
	out := read(t, path)
	for _, want := range []string{"# The sonar repo itself.", "# the debugger", "# served by uvicorn"} {
		if !strings.Contains(out, want) {
			t.Errorf("comment %q was lost:\n%s", want, out)
		}
	}
}

// TestEditRejectsAnInvalidResult: validation runs on the rendered bytes, so a
// depends_on pointing at nothing never reaches the disk.
func TestEditRejectsAnInvalidResult(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)
	before := read(t, path)

	_, err := EditServices(path, ConfigEdit{Add: []ServiceAdd{{
		Name: "worker", Port: 9000, DependsOn: []string{"nope"},
	}}})
	var bad *ConfigError
	if !errors.As(err, &bad) {
		t.Fatalf("a dangling depends_on = %v, want *ConfigError", err)
	}
	if after := read(t, path); after != before {
		t.Errorf("an invalid edit was written:\n%s", after)
	}
}

// TestRenderEditDoesNotWrite is what `groups.init --merge` previews with.
func TestRenderEditDoesNotWrite(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)
	before := read(t, path)

	data, cfg, err := RenderEdit(path, ConfigEdit{Add: []ServiceAdd{{Name: "worker", Port: 9000}}})
	if err != nil {
		t.Fatalf("RenderEdit: %v", err)
	}
	if !strings.Contains(string(data), "name: worker") {
		t.Errorf("the rendered bytes do not carry the add:\n%s", data)
	}
	if len(cfg.Services) != 4 {
		t.Errorf("rendered config has %d services, want 4", len(cfg.Services))
	}
	if after := read(t, path); after != before {
		t.Errorf("RenderEdit wrote to the file:\n%s", after)
	}
}

func TestConfigEditAffectedAndEmpty(t *testing.T) {
	if !(ConfigEdit{}).Empty() {
		t.Error("an edit with no lists should be empty")
	}
	edit := ConfigEdit{
		Remove:   []string{"old"},
		Rename:   []ServiceRename{{From: "db", To: "database"}},
		Add:      []ServiceAdd{{Name: "worker"}},
		Services: []ServiceEdit{{Name: "api"}},
	}
	if edit.Empty() {
		t.Error("an edit with four lists should not be empty")
	}
	want := []string{"old", "database", "worker", "api"}
	if got := edit.Affected(); !equalStrings(got, want) {
		t.Errorf("Affected() = %v, want %v", got, want)
	}
}

// TestCurateKeepsTheProposedCommand: a client that edits names, ports and
// colours must not throw away the command the proposal guessed.
func TestCurateKeepsTheProposedCommand(t *testing.T) {
	proposal := &Config{
		Name: "demo",
		Services: []Service{
			{Name: "node-8000", Port: 8000, Cmd: "npm run dev", Cwd: "web"},
			{Name: "postgres-5432", Port: 5432, Cmd: "docker compose up db"},
		},
	}
	cfg := Curate(proposal, []ServiceAdd{
		{Name: "web", Port: 8000, Color: "blue"},
		{Name: "db", Port: 5432, Cmd: "docker start db"},
		{Name: "worker", Port: 9000},
	})

	if len(cfg.Services) != 3 || cfg.Name != "demo" {
		t.Fatalf("curated config = %+v", cfg)
	}
	if cfg.Services[0].Cmd != "npm run dev" || cfg.Services[0].Cwd != "web" || cfg.Services[0].Color != "blue" {
		t.Errorf("web = %+v, want the proposal's cmd and cwd kept", cfg.Services[0])
	}
	if cfg.Services[1].Cmd != "docker start db" {
		t.Errorf("db = %+v, want the caller's own cmd to win", cfg.Services[1])
	}
	if cfg.Services[2].Cmd != "" {
		t.Errorf("worker = %+v, want no cmd: its port was not in the proposal", cfg.Services[2])
	}
	// The proposal itself is not modified.
	if proposal.Services[0].Name != "node-8000" {
		t.Errorf("Curate mutated the proposal: %+v", proposal.Services[0])
	}
}

func TestAddsFromRoundTrips(t *testing.T) {
	cfg := &Config{Services: []Service{
		{Name: "api", Port: 8000, Cmd: "uv run api", Cwd: "backend", Health: "/health",
			Description: "the api", Icon: "server", Color: "blue", DependsOn: []string{"db"}},
	}}
	adds := AddsFrom(cfg)
	if len(adds) != 1 {
		t.Fatalf("AddsFrom returned %d entries", len(adds))
	}
	if got := adds[0].Service(); got.Name != "api" || got.Port != 8000 || got.Cmd != "uv run api" ||
		got.Cwd != "backend" || got.Health != "/health" || got.Description != "the api" ||
		got.Icon != "server" || got.Color != "blue" || len(got.DependsOn) != 1 {
		t.Errorf("round trip = %+v, want the whole service back", got)
	}
}

// TestWorktreePortsAddKeepsCommentsAndOrder: a file with no worktree_ports
// grows the key at the end, and every other line stays where it was.
func TestWorktreePortsAddKeepsCommentsAndOrder(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)

	cfg, err := EditServices(path, ConfigEdit{WorktreePorts: SetInt(4)})
	if err != nil {
		t.Fatalf("EditServices: %v", err)
	}
	if cfg.WorktreePorts == nil || *cfg.WorktreePorts != 4 {
		t.Fatalf("returned worktree_ports = %v, want 4", cfg.WorktreePorts)
	}
	out := read(t, path)
	requireComments(t, out)
	if got := strings.Join(topLevelKeys(out), ","); got != "ports,services,name,worktree_ports" {
		t.Errorf("top-level keys = %s, want the author's order with worktree_ports last:\n%s", got, out)
	}
	if got := serviceNamesInFile(t, path); strings.Join(got, ",") != "db,api,cache" {
		t.Errorf("services = %v, want them untouched", got)
	}
	reloaded, err := Load(path)
	if err != nil || reloaded.WorktreePorts == nil || *reloaded.WorktreePorts != 4 {
		t.Fatalf("reloaded worktree_ports = %v, %v; want 4", reloaded, err)
	}
}

// TestWorktreePortsReplaceAndRemove: an existing value is edited in place, so
// its comment and position survive; a clear removes the key and nothing else.
func TestWorktreePortsReplaceAndRemove(t *testing.T) {
	path := writePatchable(t, `name: shop
worktree_ports: 3 # api, web and the worker
# the services
services:
  - name: api
    port: 8000
`)

	if _, err := EditServices(path, ConfigEdit{WorktreePorts: SetInt(6)}); err != nil {
		t.Fatalf("setting worktree_ports: %v", err)
	}
	out := read(t, path)
	if !strings.Contains(out, "worktree_ports: 6 # api, web and the worker") {
		t.Errorf("the line comment did not stay with the value:\n%s", out)
	}
	if got := strings.Join(topLevelKeys(out), ","); got != "name,worktree_ports,services" {
		t.Errorf("top-level keys = %s, want the key kept where it was", got)
	}

	cfg, err := EditServices(path, ConfigEdit{WorktreePorts: ClearInt()})
	if err != nil {
		t.Fatalf("clearing worktree_ports: %v", err)
	}
	if cfg.WorktreePorts != nil {
		t.Errorf("worktree_ports = %d after a clear, want nil", *cfg.WorktreePorts)
	}
	out = read(t, path)
	if strings.Contains(out, "worktree_ports") {
		t.Errorf("the key survived a clear:\n%s", out)
	}
	if !strings.Contains(out, "# the services") || !strings.Contains(out, "name: api") {
		t.Errorf("a clear touched more than its own key:\n%s", out)
	}

	// Clearing a key that is not there changes nothing and is not an error.
	if _, err := EditServices(path, ConfigEdit{WorktreePorts: ClearInt()}); err != nil {
		t.Errorf("clearing an absent worktree_ports: %v", err)
	}
}

// TestWorktreePortsOutOfRangeLeavesTheFile: a value the file would not
// validate with is a ConfigError and the file stays byte-identical.
func TestWorktreePortsOutOfRangeLeavesTheFile(t *testing.T) {
	path := writePatchable(t, oddlyOrdered)
	for _, n := range []int{0, MaxWorktreePorts + 1} {
		_, err := EditServices(path, ConfigEdit{WorktreePorts: SetInt(n)})
		var bad *ConfigError
		if !errors.As(err, &bad) {
			t.Fatalf("worktree_ports %d: err = %v, want a *ConfigError", n, err)
		}
		if got := read(t, path); got != oddlyOrdered {
			t.Fatalf("worktree_ports %d changed the file:\n%s", n, got)
		}
	}
}

func TestWorktreePortsChangeIsNotEmpty(t *testing.T) {
	for _, c := range []IntChange{SetInt(2), ClearInt()} {
		e := ConfigEdit{WorktreePorts: c}
		if e.Empty() {
			t.Errorf("%+v reads as an empty edit", c)
		}
		if len(e.Affected()) != 0 {
			t.Errorf("affected = %v, want no service names", e.Affected())
		}
	}
	if !(ConfigEdit{}).Empty() {
		t.Error("the zero edit should be empty")
	}
}
