package spawn

import (
	"path/filepath"
	"testing"
)

// TestResolveWorktreeWithACopiedConfig: `sonar start` in a linked worktree that
// carries a copy of the main checkout's committed `.sonar.yaml` records the
// worktree's own group, named by the main checkout — not the copy's `name:`,
// and not the main checkout's group (step 5A.6).
func TestResolveWorktreeWithACopiedConfig(t *testing.T) {
	main := tempRepo(t, "shop-dir", map[string]string{
		".git/worktrees/feat/.keep": "x",
		".sonar.yaml":               "name: shop\nservices:\n  - name: web\n    cmd: npm run dev\n",
	})
	wt := tempRepo(t, "feat", map[string]string{
		".git":        "gitdir: " + filepath.Join(main, ".git", "worktrees", "feat") + "\n",
		".sonar.yaml": "name: stale\nservices:\n  - name: web\n    cmd: npm run dev\n",
	})

	got := Resolve(wt, []string{"npm", "run", "dev"}, "", "")
	if got.Group != "shop@feat" || got.Name != "web" {
		t.Fatalf("worktree run = %q/%q, want shop@feat/web", got.Group, got.Name)
	}
	if got.ConfigPath != filepath.Join(wt, ".sonar.yaml") {
		t.Errorf("config path = %q, want the worktree's own copy", got.ConfigPath)
	}

	if got := Resolve(main, []string{"npm", "run", "dev"}, "", ""); got.Group != "shop" {
		t.Errorf("main checkout run group = %q, want shop", got.Group)
	}
}
