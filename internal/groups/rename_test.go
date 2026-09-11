package groups

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/state"
)

// renameWorld is a project with a main checkout and a worktree, both running,
// plus an unrelated repository called "store", resolved and built the way the
// daemon publishes them.
func renameWorld(t *testing.T, withConfig bool) (checkouts, *Index, []state.Group) {
	t.Helper()
	c := newCheckouts(t, "shop", "feat")
	if withConfig {
		writeFile(t, filepath.Join(c.main, ConfigName), "# the shop\nname: shop\nports: [8000]\n")
		writeFile(t, filepath.Join(c.wt, ConfigName), "name: shop\nports: [8000]\n")
	}
	other := mkdir(t, filepath.Dir(c.main), "store")
	mkdir(t, other, ".git")

	x := NewIndex()
	for _, dir := range []string{c.main, c.wt, other} {
		x.Observe(dir)
	}
	pp := Resolve([]state.Port{
		nativePort(8000, c.main), nativePort(8001, c.wt), nativePort(9000, other),
	}, fakePins{7000: "pinned"}, fakeRuns{7100: {group: "started", name: "job"}}, x)
	pp = append(pp, Resolve([]state.Port{nativePort(7000, c.main), nativePort(7100, c.main)},
		fakePins{7000: "pinned"}, fakeRuns{7100: {group: "started", name: "job"}}, x)...)
	return c, x, Groups(pp, x)
}

func renameKind(err error) (RenameErrorKind, bool) {
	var re *RenameError
	if errors.As(err, &re) {
		return re.Kind, true
	}
	return 0, false
}

func TestPlanRenameFileProject(t *testing.T) {
	c, x, gg := renameWorld(t, true)

	// Named from the worktree: the rename is still the project's.
	plan, err := PlanRename(gg, x, "shop@feat", "market")
	if err != nil {
		t.Fatalf("PlanRename: %v", err)
	}
	if plan.ConfigPath != filepath.Join(c.main, ConfigName) {
		t.Errorf("config path = %q, want the main checkout's file", plan.ConfigPath)
	}
	if plan.Main != c.main || plan.Project != "market" {
		t.Errorf("plan = %+v", plan)
	}
	if len(plan.Renames) != 2 || plan.Renames["shop"] != "market" || plan.Renames["shop@feat"] != "market@feat" {
		t.Errorf("renames = %v", plan.Renames)
	}
}

func TestPlanRenameAutoProject(t *testing.T) {
	c, x, gg := renameWorld(t, false)
	plan, err := PlanRename(gg, x, "shop", "market")
	if err != nil {
		t.Fatalf("PlanRename: %v", err)
	}
	if plan.ConfigPath != "" || plan.Main != c.main {
		t.Errorf("plan = %+v, want an alias of the main checkout", plan)
	}
	if plan.Renames["shop"] != "market" || plan.Renames["shop@feat"] != "market@feat" {
		t.Errorf("renames = %v", plan.Renames)
	}

	// Applied as an alias, the resolver agrees with the plan.
	x.SetAliases(map[string]string{plan.Main: plan.Project})
	pp := Resolve([]state.Port{nativePort(8000, c.main), nativePort(8001, c.wt)}, NoPins{}, NoRuns{}, x)
	if deref(pp[0].Group) != "market" || deref(pp[1].Group) != "market@feat" {
		t.Errorf("after the alias: %q, %q", deref(pp[0].Group), deref(pp[1].Group))
	}
}

func TestPlanRenameRefusals(t *testing.T) {
	_, x, gg := renameWorld(t, false)

	tests := []struct {
		name, from, to string
		want           RenameErrorKind
	}{
		{"unknown group", "nope", "market", RenameNotFound},
		{"collides with another project", "shop", "store", RenameConflict},
		{"a manual group", "pinned", "market", RenameRefused},
		{"a run's --group", "started", "market", RenameRefused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PlanRename(gg, x, tt.from, tt.to)
			kind, ok := renameKind(err)
			if !ok || kind != tt.want {
				t.Fatalf("err = %v, want kind %d", err, tt.want)
			}
		})
	}

	// Renaming to the current name is a no-op, not a conflict with itself.
	plan, err := PlanRename(gg, x, "shop@feat", "shop")
	if err != nil || len(plan.Renames) != 0 {
		t.Fatalf("same name = %+v, %v", plan, err)
	}
}

func TestPlanRenameConflictsOnAWorktreeName(t *testing.T) {
	_, x, gg := renameWorld(t, false)
	gg = append(gg, state.Group{Name: "market@feat", Source: state.SourceManual})
	if _, err := PlanRename(gg, x, "shop", "market"); err == nil {
		t.Fatal("renaming onto an existing <project>@<worktree> succeeded")
	} else if kind, _ := renameKind(err); kind != RenameConflict {
		t.Fatalf("err = %v, want a conflict", err)
	}
}

func TestPlanRenameConfigOutsideACheckout(t *testing.T) {
	dir := mkdir(t, tempTree(t), "loose")
	writeFile(t, filepath.Join(dir, ConfigName), "name: loose\nports: [8000]\n")
	x := NewIndex()
	x.Observe(dir)
	gg := Groups(nil, x)

	plan, err := PlanRename(gg, x, "loose", "tidy")
	if err != nil {
		t.Fatalf("PlanRename: %v", err)
	}
	if plan.ConfigPath != filepath.Join(dir, ConfigName) || plan.Main != "" || plan.Renames["loose"] != "tidy" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestValidProjectName(t *testing.T) {
	for _, name := range []string{"", "a@b", "a/b", `a\b`, "a b", "tab\tname"} {
		if ValidProjectName(name) == nil {
			t.Errorf("ValidProjectName(%q) accepted it", name)
		}
	}
	for _, name := range []string{"shop", "my-app", "app_2.0"} {
		if err := ValidProjectName(name); err != nil {
			t.Errorf("ValidProjectName(%q) = %v", name, err)
		}
	}
}

func TestSetConfigNameKeepsTheFile(t *testing.T) {
	dir := tempTree(t)
	path := filepath.Join(dir, ConfigName)
	writeFile(t, path, "# the shop\nname: shop # short\nservices:\n  # first\n  - name: api\n    port: 8000\n")

	cfg, err := SetConfigName(path, "market")
	if err != nil {
		t.Fatalf("SetConfigName: %v", err)
	}
	if cfg.Name != "market" {
		t.Errorf("config name = %q", cfg.Name)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	for _, want := range []string{"# the shop", "name: market # short", "# first", "port: 8000"} {
		if !strings.Contains(got, want) {
			t.Errorf("file lost %q:\n%s", want, got)
		}
	}

	// A file with no name: key gets one, at the top.
	bare := filepath.Join(tempTree(t), ConfigName)
	writeFile(t, bare, "services:\n  - name: api\n")
	if _, err := SetConfigName(bare, "market"); err != nil {
		t.Fatalf("SetConfigName without a name key: %v", err)
	}
	data, _ = os.ReadFile(bare)
	if !strings.HasPrefix(string(data), "name: market\n") {
		t.Errorf("name not inserted first:\n%s", data)
	}

	// A name that does not validate leaves the file alone.
	before, _ := os.ReadFile(path)
	if _, err := SetConfigName(path, "bad name"); err == nil {
		t.Fatal("wrote a name with a space")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("a refused rename changed the file")
	}
}
