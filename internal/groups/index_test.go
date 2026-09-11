package groups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/state"
)

func TestIndexObserveWalksUpToTheGitRoot(t *testing.T) {
	base := tempTree(t)
	repo := mkdir(t, base, "repo")
	mkdir(t, repo, ".git")
	deep := mkdir(t, repo, "services", "api")
	writeFile(t, filepath.Join(repo, ConfigName), "name: repo\n")
	// A config above the repository must not be picked up.
	writeFile(t, filepath.Join(base, ConfigName), "name: outer\n")

	x := NewIndex()
	x.Observe(deep)

	if cfg := x.Nearest(deep); cfg == nil || cfg.Name != "repo" {
		t.Fatalf("Nearest(%q) = %+v, want the repo config", deep, cfg)
	}
	if x.At(base) != nil {
		t.Error("Observe walked past the git root")
	}
}

func TestIndexObserveOutsideARepoOnlyLooksAtTheDirectory(t *testing.T) {
	base := tempTree(t)
	dir := mkdir(t, base, "loose")
	writeFile(t, filepath.Join(dir, ConfigName), "name: loose\n")
	writeFile(t, filepath.Join(base, ConfigName), "name: outer\n")

	x := NewIndex()
	x.Observe(dir)
	if cfg := x.At(dir); cfg == nil || cfg.Name != "loose" {
		t.Fatalf("At(%q) = %+v", dir, cfg)
	}
	if x.At(base) != nil {
		t.Error("Observe walked up outside a repository")
	}
}

func TestIndexDeepestConfigWins(t *testing.T) {
	base := tempTree(t)
	repo := mkdir(t, base, "repo")
	mkdir(t, repo, ".git")
	inner := mkdir(t, repo, "frontend")
	writeFile(t, filepath.Join(repo, ConfigName), "name: outer\nservices:\n  - name: api\n    port: 8000\n")
	writeFile(t, filepath.Join(inner, ConfigName), "name: inner\nservices:\n  - name: web\n    port: 8000\n")

	x := NewIndex()
	x.Observe(inner)

	cfg, svc, ok := x.MatchPort(state.Port{Port: 8000, Cwd: inner})
	if !ok || cfg.Name != "inner" || svc != "web" {
		t.Fatalf("MatchPort = (%v, %q, %v), want the inner config", cfg, svc, ok)
	}
	cfg, svc, ok = x.MatchPort(state.Port{Port: 8000, Cwd: repo})
	if !ok || cfg.Name != "outer" || svc != "api" {
		t.Fatalf("MatchPort from the repo root = (%v, %q, %v)", cfg, svc, ok)
	}
	// Two configs claim 8000, and with no cwd there is nothing to tell them
	// apart: an ambiguous claim is not a group.
	if _, _, ok := x.MatchPort(state.Port{Port: 8000}); ok {
		t.Error("a port with no cwd must not pick between two configs that both claim it")
	}
	if _, _, ok := x.MatchPort(state.Port{Port: 9999, Cwd: inner}); ok {
		t.Error("an unclaimed port must not match a config")
	}
}

// TestIndexMatchesADeclaredPortWithoutACwd is the Windows gap: the scanner has
// no per-process working directory there, so a listener arrives with Cwd
// empty. A `.sonar.yaml` that declares the port is still the only claim on it,
// and the group has to form — otherwise `sonar up` starts a group nothing ever
// joins.
func TestIndexMatchesADeclaredPortWithoutACwd(t *testing.T) {
	base := tempTree(t)
	repo := mkdir(t, base, "repo")
	mkdir(t, repo, ".git")
	writeFile(t, filepath.Join(repo, ConfigName),
		"name: demo\nservices:\n  - name: api\n    port: 8000\nports: [8100]\n")

	x := NewIndex()
	x.Observe(repo)

	cfg, svc, ok := x.MatchPort(state.Port{Port: 8000})
	if !ok || cfg.Name != "demo" || svc != "api" {
		t.Fatalf("MatchPort(service port, no cwd) = (%v, %q, %v), want the demo config's api", cfg, svc, ok)
	}
	cfg, svc, ok = x.MatchPort(state.Port{Port: 8100})
	if !ok || cfg.Name != "demo" || svc != "" {
		t.Fatalf("MatchPort(file port, no cwd) = (%v, %q, %v), want the demo config", cfg, svc, ok)
	}
	if _, _, ok := x.MatchPort(state.Port{Port: 9999}); ok {
		t.Error("a port no config declares must not match")
	}

	// The resolver has to reach the same conclusion, since that is what puts
	// the port in the group.
	got := Resolve([]state.Port{{Port: 8000}}, NoPins{}, NoRuns{}, x)[0]
	if deref(got.Group) != "demo" {
		t.Errorf("resolved group = %q, want demo", deref(got.Group))
	}
	if got.GroupSource == nil || *got.GroupSource != state.SourceFile {
		t.Errorf("group source = %v, want file", got.GroupSource)
	}
}

// TestIndexWillNotGuessBetweenTwoConfigsClaimingAPort: without a cwd, two
// projects declaring the same port are indistinguishable, and a wrong group is
// worse than none.
func TestIndexWillNotGuessBetweenTwoConfigsClaimingAPort(t *testing.T) {
	base := tempTree(t)
	one := mkdir(t, base, "one")
	mkdir(t, one, ".git")
	two := mkdir(t, base, "two")
	mkdir(t, two, ".git")
	writeFile(t, filepath.Join(one, ConfigName), "name: one\nservices:\n  - name: api\n    port: 8000\n")
	writeFile(t, filepath.Join(two, ConfigName), "name: two\nservices:\n  - name: api\n    port: 8000\n")

	x := NewIndex()
	x.Observe(one)
	x.Observe(two)

	if cfg, _, ok := x.MatchPort(state.Port{Port: 8000}); ok {
		t.Errorf("MatchPort with no cwd chose %q; two configs claim 8000", cfg.Name)
	}
	// With a cwd the answer is unambiguous again.
	if cfg, _, ok := x.MatchPort(state.Port{Port: 8000, Cwd: two}); !ok || cfg.Name != "two" {
		t.Errorf("MatchPort(cwd two) = (%v, %v), want the two config", cfg, ok)
	}
}

func TestIndexRecordsInvalidConfigsWithoutFailing(t *testing.T) {
	base := tempTree(t)
	repo := mkdir(t, base, "repo")
	mkdir(t, repo, ".git")
	writeFile(t, filepath.Join(repo, ConfigName), "name: a b\n")

	x := NewIndex()
	x.Observe(repo)

	if got := x.Configs(); len(got) != 0 {
		t.Fatalf("Configs = %v, want none", got)
	}
	invalid := x.Invalid()
	if len(invalid) != 1 || !strings.Contains(invalid[0].Err.Error(), "whitespace") {
		t.Fatalf("Invalid = %+v", invalid)
	}
	// The resolver must fall back to the git root, not blow up.
	got := Resolve([]state.Port{nativePort(8123, repo)}, NoPins{}, NoRuns{}, x)[0]
	if deref(got.Group) != "repo" {
		t.Fatalf("group = %q, want the git root name", deref(got.Group))
	}
}

func TestIndexAcceptsTheYmlSpelling(t *testing.T) {
	base := tempTree(t)
	repo := mkdir(t, base, "repo")
	mkdir(t, repo, ".git")
	writeFile(t, filepath.Join(repo, ".sonar.yml"), "name: ymlrepo\n")

	x := NewIndex()
	x.Observe(repo)
	if cfg := x.At(repo); cfg == nil || cfg.Name != "ymlrepo" {
		t.Fatalf("At = %+v", cfg)
	}
}

// TestIndexReadsTheOldDotfile: projects committed before the rename keep
// working, and so do the worktrees that carry a copy of their file.
func TestIndexReadsTheOldDotfile(t *testing.T) {
	base := tempTree(t)
	repo := mkdir(t, base, "repo")
	mkdir(t, repo, ".git")
	writeFile(t, filepath.Join(repo, LegacyConfigName), "name: legacy\n")

	x := NewIndex()
	x.Observe(repo)
	if cfg := x.At(repo); cfg == nil || cfg.Name != "legacy" {
		t.Fatalf("At = %+v", cfg)
	}
}

// TestIndexPrefersTheCurrentName: with both files in one directory, sonar.yaml
// is the one read and the dotfile is shadowed, never merged.
func TestIndexPrefersTheCurrentName(t *testing.T) {
	base := tempTree(t)
	repo := mkdir(t, base, "repo")
	mkdir(t, repo, ".git")
	writeFile(t, filepath.Join(repo, LegacyConfigName), "name: old\n")
	writeFile(t, filepath.Join(repo, ConfigName), "name: new\n")

	x := NewIndex()
	x.Observe(repo)
	if cfg := x.At(repo); cfg == nil || cfg.Name != "new" {
		t.Fatalf("At = %+v, want the sonar.yaml", cfg)
	}
	present := FilesIn(repo)
	if len(present) != 2 || filepath.Base(present[0]) != ConfigName || filepath.Base(present[1]) != LegacyConfigName {
		t.Errorf("FilesIn = %v, want sonar.yaml then .sonar.yaml", present)
	}
}

// TestTargetIn: a write goes to the file already there, whatever its spelling,
// and only a directory without one gets a new sonar.yaml.
func TestTargetIn(t *testing.T) {
	base := tempTree(t)
	empty := mkdir(t, base, "empty")
	if got := TargetIn(empty); got != filepath.Join(empty, ConfigName) {
		t.Errorf("TargetIn(empty) = %s, want a new %s", got, ConfigName)
	}
	legacy := mkdir(t, base, "legacy")
	writeFile(t, filepath.Join(legacy, LegacyConfigName), "name: legacy\n")
	if got := TargetIn(legacy); got != filepath.Join(legacy, LegacyConfigName) {
		t.Errorf("TargetIn(legacy) = %s, want the existing %s", got, LegacyConfigName)
	}
}

func TestIsLegacyName(t *testing.T) {
	for name, want := range map[string]bool{
		".sonar.yaml": true, ".sonar.yml": true,
		"sonar.yaml": false, "sonar.yml": false, ".env": false,
	} {
		if got := IsLegacyName(name); got != want {
			t.Errorf("IsLegacyName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestIndexKeysConfigsCanonically: a config reached through an unresolved path
// — a Reload root the store handed back, or a path a client typed — has to land
// under the same key as the same directory found by walking a process cwd. On
// macOS those two spellings differ for everything under $TMPDIR (/var against
// /private/var), and a mismatch makes the config invisible to Nearest, so
// `sonar start` falls back to the git root or the directory name.
func TestIndexKeysConfigsCanonically(t *testing.T) {
	real := tempTree(t)
	repo := mkdir(t, real, "repo")
	writeFile(t, filepath.Join(repo, ConfigName), "name: my-app\n")

	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	viaLink := filepath.Join(link, "repo")

	t.Run("AddFile", func(t *testing.T) {
		x := NewIndex()
		if err := x.AddFile(filepath.Join(viaLink, ConfigName)); err != nil {
			t.Fatal(err)
		}
		if cfg := x.Nearest(repo); cfg == nil || cfg.Name != "my-app" {
			t.Fatalf("Nearest(%s) = %+v after AddFile through a symlink", repo, cfg)
		}
		if cfg := x.At(viaLink); cfg == nil || cfg.Name != "my-app" {
			t.Fatalf("At(%s) = %+v", viaLink, cfg)
		}
	})

	t.Run("Reload", func(t *testing.T) {
		x := NewIndex()
		if n, bad := x.Reload([]string{viaLink}); n != 1 || len(bad) != 0 {
			t.Fatalf("Reload = %d, %v", n, bad)
		}
		if cfg := x.Nearest(filepath.Join(repo, "sub", "dir")); cfg == nil || cfg.Name != "my-app" {
			t.Fatalf("Nearest below the repo = %+v after Reload through a symlink", cfg)
		}
	})

	t.Run("ByPath", func(t *testing.T) {
		x := NewIndex()
		x.Observe(repo)
		if cfg, ok := x.ByPath(filepath.Join(viaLink, ConfigName)); !ok || cfg.Name != "my-app" {
			t.Fatalf("ByPath through a symlink = %+v, %v", cfg, ok)
		}
	})
}
