package scanner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestRootGone covers the decision: only a clean "not there" forgets a root.
func TestRootGone(t *testing.T) {
	base := resolved(t, t.TempDir())

	withConfig := filepath.Join(base, "with-config")
	mkdir(t, withConfig)
	if err := os.WriteFile(filepath.Join(withConfig, "sonar.yml"), []byte("name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(base, "empty")
	mkdir(t, empty)

	cases := []struct {
		name string
		dir  string
		gone bool
	}{
		{"directory deleted", filepath.Join(base, "deleted"), true},
		{"parent deleted too", filepath.Join(base, "deleted", "worktree"), true},
		{"directory without a config", empty, true},
		{"directory with a config", withConfig, false},
	}
	for _, c := range cases {
		if got := rootGone(c.dir); got != c.gone {
			t.Errorf("%s: rootGone(%s) = %v, want %v", c.name, c.dir, got, c.gone)
		}
	}
}

// TestRootGoneKeepsUnreadableRoots: a permission error is not proof the
// project is gone, whether it hides the directory or what is in it.
func TestRootGoneKeepsUnreadableRoots(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through permissions")
	}
	base := resolved(t, t.TempDir())

	locked := filepath.Join(base, "locked")
	hidden := filepath.Join(locked, "project")
	mkdir(t, hidden)
	sealed := filepath.Join(base, "sealed")
	mkdir(t, sealed)
	for _, dir := range []string{locked, sealed} {
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}

	if rootGone(hidden) {
		t.Error("a root behind an unreadable parent was forgotten")
	}
	if rootGone(sealed) {
		t.Error("a root whose contents cannot be listed was forgotten")
	}
}

// TestRootGoneKeepsRootsOnAnUnmountedVolume: /Volumes/<disk>/project is
// missing while the disk is unplugged, which says nothing about the project.
func TestRootGoneKeepsRootsOnAnUnmountedVolume(t *testing.T) {
	mounts := resolved(t, t.TempDir())
	saved := mountParents
	mountParents = []string{mounts}
	t.Cleanup(func() { mountParents = saved })

	if rootGone(filepath.Join(mounts, "Backup", "project")) {
		t.Error("a root on an unmounted volume was forgotten")
	}
	mkdir(t, filepath.Join(mounts, "Backup"))
	if !rootGone(filepath.Join(mounts, "Backup", "project")) {
		t.Error("a root deleted from a mounted volume was kept")
	}
}

// TestReloadForgetsDeadRoots goes through the loop and the store: roots left
// by deleted projects disappear from known_roots, live ones stay.
func TestReloadForgetsDeadRoots(t *testing.T) {
	st := openStore(t)
	live := resolved(t, t.TempDir())
	writeConfig(t, live, "name: live\nservices:\n  - name: api\n")
	deleted := filepath.Join(resolved(t, t.TempDir()), "gone")
	for _, root := range []string{live, deleted} {
		if err := st.AddRoot(root); err != nil {
			t.Fatal(err)
		}
	}

	l := loopWith(st)
	if _, err := l.Snapshot(Include{}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if roots, _ := st.Roots(); !slices.Equal(roots, []string{live}) {
		t.Fatalf("roots after start = %v, want only %s", roots, live)
	}

	if err := os.Remove(filepath.Join(live, ".sonar.yaml")); err != nil {
		t.Fatal(err)
	}
	l.ReloadConfigs()
	if roots, _ := st.Roots(); len(roots) != 0 {
		t.Fatalf("roots after the config was deleted = %v, want none", roots)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}
