package groups

import (
	"path/filepath"
	"testing"

	"github.com/raskrebs/sonar/internal/state"
)

// checkouts is a main checkout with a real-looking .git (HEAD on main) and one
// linked worktree whose admin directory holds its own HEAD.
type checkouts struct {
	main, wt, admin string
}

func newCheckouts(t *testing.T, mainName, wtName string) checkouts {
	t.Helper()
	base := tempTree(t)
	main := mkdir(t, base, mainName)
	writeFile(t, filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	admin := filepath.Join(main, ".git", "worktrees", wtName)
	writeFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/feature/x\n")
	wt := mkdir(t, base, "wt", wtName)
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
	return checkouts{main: main, wt: wt, admin: admin}
}

func TestLocate(t *testing.T) {
	c := newCheckouts(t, "shop", "feat")
	deep := mkdir(t, c.wt, "web", "src")

	co, ok := Locate(mkdir(t, c.main, "api"))
	if !ok || co.Root != c.main || co.Main != c.main || co.Linked() {
		t.Fatalf("main checkout = %+v, %v", co, ok)
	}

	co, ok = Locate(deep)
	if !ok || co.Root != c.wt || co.Main != c.main || co.Worktree != "feat" || !co.Linked() {
		t.Fatalf("worktree = %+v, %v", co, ok)
	}

	// A relative gitdir resolves against the worktree root.
	rel := mkdir(t, filepath.Dir(c.wt), "relwt")
	target, err := filepath.Rel(rel, filepath.Join(c.main, ".git", "worktrees", "relwt"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(rel, ".git"), "gitdir: "+target+"\n")
	if co, ok := Locate(rel); !ok || co.Main != c.main || co.Worktree != "relwt" {
		t.Fatalf("relative worktree = %+v, %v", co, ok)
	}

	// A submodule's .git file points into .git/modules: it is its own main
	// checkout, not a worktree of the superproject.
	sub := mkdir(t, c.main, "vendor", "lib")
	writeFile(t, filepath.Join(sub, ".git"), "gitdir: ../../.git/modules/lib\n")
	if co, ok := Locate(sub); !ok || co.Root != sub || co.Main != sub || co.Linked() {
		t.Fatalf("submodule = %+v, %v", co, ok)
	}

	if _, ok := Locate(tempTree(t)); ok {
		t.Fatal("found a checkout outside any repository")
	}
	if _, ok := Locate(""); ok {
		t.Fatal("found a checkout for an empty path")
	}
}

// TestLocateAcceptsEverySpellingOfTheWorktree is the worktree half of the
// Windows spellings test: with a trailing separator or a redundant `.`, a
// worktree still names one root and one main checkout.
func TestLocateAcceptsEverySpellingOfTheWorktree(t *testing.T) {
	c := newCheckouts(t, "shop", "feat")
	sep := string(filepath.Separator)
	for _, dir := range []string{c.wt, c.wt + sep, filepath.Join(c.wt, "."), filepath.Join(c.wt, "x", "..")} {
		co, ok := Locate(dir)
		if !ok || co.Root != c.wt || co.Main != c.main || co.Worktree != "feat" {
			t.Errorf("Locate(%q) = %+v, %v", dir, co, ok)
		}
	}
}

func TestBranch(t *testing.T) {
	c := newCheckouts(t, "shop", "feat")
	x := NewIndex()
	main, _ := Locate(c.main)
	wt, _ := Locate(c.wt)

	if got := x.Branch(main); got != "main" {
		t.Errorf("main branch = %q", got)
	}
	if got := x.Branch(wt); got != "feature/x" {
		t.Errorf("worktree branch = %q, want the HEAD in the main .git/worktrees/feat", got)
	}

	// A checkout switch rewrites HEAD; the cache notices the new size.
	writeFile(t, filepath.Join(c.admin, "HEAD"), "ref: refs/heads/fix\n")
	if got := x.Branch(wt); got != "fix" {
		t.Errorf("after switching, worktree branch = %q", got)
	}
	// Detached.
	writeFile(t, filepath.Join(c.admin, "HEAD"), "4b825dc642cb6eb9a060e54bf8d69288fbee4904\n")
	if got := x.Branch(wt); got != "" {
		t.Errorf("detached HEAD branch = %q, want empty", got)
	}

	// A checkout with no readable HEAD, like the resolver fixtures.
	bare := mkdir(t, tempTree(t), "nohead")
	mkdir(t, bare, ".git")
	co, _ := Locate(bare)
	if got := x.Branch(co); got != "" {
		t.Errorf("branch without HEAD = %q", got)
	}
}

func TestParseHead(t *testing.T) {
	for in, want := range map[string]string{
		"ref: refs/heads/main\n":       "main",
		"ref:refs/heads/feature/a-b":   "feature/a-b",
		"ref: refs/remotes/origin/x\n": "",
		"0123456789abcdef\n":           "",
		"":                             "",
	} {
		if got := parseHead([]byte(in)); got != want {
			t.Errorf("parseHead(%q) = %q, want %q", in, got, want)
		}
	}
}

func resolveOneAt(t *testing.T, x *Index, p state.Port) (string, state.GroupSource) {
	t.Helper()
	got := Resolve([]state.Port{p}, NoPins{}, NoRuns{}, x)[0]
	var source state.GroupSource
	if got.GroupSource != nil {
		source = *got.GroupSource
	}
	return deref(got.Group), source
}

// TestWorktreeNaming is the step 5A.6 rule: every linked worktree is
// `<project>@<worktree>`, and only the main checkout decides the project.
func TestWorktreeNaming(t *testing.T) {
	t.Run("no config anywhere", func(t *testing.T) {
		c := newCheckouts(t, "app", "feat")
		x := NewIndex()
		x.Observe(c.wt)
		if g, s := resolveOneAt(t, x, nativePort(3000, c.main)); g != "app" || s != state.SourceAuto {
			t.Errorf("main = %q/%s", g, s)
		}
		if g, s := resolveOneAt(t, x, nativePort(3001, c.wt)); g != "app@feat" || s != state.SourceAuto {
			t.Errorf("worktree = %q/%s", g, s)
		}
	})

	t.Run("a worktree's stale copy of the config never names it", func(t *testing.T) {
		c := newCheckouts(t, "shop-dir", "feat")
		writeFile(t, filepath.Join(c.main, ConfigName), "name: shop\nservices:\n  - name: api\n    port: 8000\n")
		writeFile(t, filepath.Join(c.wt, ConfigName), "name: old-shop\nservices:\n  - name: api\n    port: 8100\n")
		x := NewIndex()
		x.Observe(c.wt)

		if g, s := resolveOneAt(t, x, nativePort(8000, c.main)); g != "shop" || s != state.SourceFile {
			t.Errorf("main = %q/%s", g, s)
		}
		// Claimed by the worktree's own file, named by the main checkout's.
		if g, s := resolveOneAt(t, x, nativePort(8100, c.wt)); g != "shop@feat" || s != state.SourceFile {
			t.Errorf("worktree port the copy claims = %q/%s", g, s)
		}
		if g, s := resolveOneAt(t, x, nativePort(9999, c.wt)); g != "shop@feat" || s != state.SourceFile {
			t.Errorf("worktree port nothing claims = %q/%s", g, s)
		}
	})

	t.Run("a copy with the same name as main is still its own group", func(t *testing.T) {
		c := newCheckouts(t, "shop", "feat")
		cfg := "name: shop\nservices:\n  - name: api\n    port: 8000\n"
		writeFile(t, filepath.Join(c.main, ConfigName), cfg)
		writeFile(t, filepath.Join(c.wt, ConfigName), cfg)
		x := NewIndex()
		x.Observe(c.main)
		x.Observe(c.wt)
		// Both checkouts run the api on 8000; each in its own group.
		if g, _ := resolveOneAt(t, x, nativePort(8000, c.main)); g != "shop" {
			t.Errorf("main = %q", g)
		}
		if g, _ := resolveOneAt(t, x, nativePort(8000, c.wt)); g != "shop@feat" {
			t.Errorf("worktree = %q", g)
		}
	})

	t.Run("a worktree kept inside the main checkout", func(t *testing.T) {
		base := tempTree(t)
		main := mkdir(t, base, "shop")
		mkdir(t, main, ".git")
		admin := filepath.Join(main, ".git", "worktrees", "feat")
		mkdir(t, admin)
		wt := mkdir(t, main, ".claude", "worktrees", "feat")
		writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
		writeFile(t, filepath.Join(main, ConfigName), "name: shop\nports: [3000]\n")
		x := NewIndex()
		x.Observe(main)
		x.Observe(wt)

		// The main checkout's file claims 3000 and sits above the worktree,
		// but it does not reach across into it.
		if g, s := resolveOneAt(t, x, nativePort(3000, wt)); g != "shop@feat" || s != state.SourceAuto {
			t.Errorf("worktree port = %q/%s, want shop@feat/auto", g, s)
		}
		if g, _ := resolveOneAt(t, x, nativePort(3000, main)); g != "shop" {
			t.Errorf("main port = %q", g)
		}
	})

	t.Run("an alias names the project and every worktree", func(t *testing.T) {
		c := newCheckouts(t, "app", "feat")
		x := NewIndex()
		x.Observe(c.wt)
		x.SetAliases(map[string]string{c.main: "store"})
		if g, _ := resolveOneAt(t, x, nativePort(3000, c.main)); g != "store" {
			t.Errorf("main = %q", g)
		}
		if g, _ := resolveOneAt(t, x, nativePort(3001, c.wt)); g != "store@feat" {
			t.Errorf("worktree = %q", g)
		}
		x.SetAliases(nil)
		if g, _ := resolveOneAt(t, x, nativePort(3001, c.wt)); g != "app@feat" {
			t.Errorf("worktree after clearing = %q", g)
		}
	})

	t.Run("submodules keep their own name", func(t *testing.T) {
		c := newCheckouts(t, "shop", "feat")
		sub := mkdir(t, c.main, "vendor", "lib")
		writeFile(t, filepath.Join(sub, ".git"), "gitdir: ../../.git/modules/lib\n")
		x := NewIndex()
		x.Observe(sub)
		if g, _ := resolveOneAt(t, x, nativePort(4000, sub)); g != "lib" {
			t.Errorf("submodule = %q, want lib", g)
		}
	})
}

// TestRunGroupFollowsTheCheckout: a run recorded under the project's own name
// joins its checkout's group; one started with a --group of its own keeps it.
func TestRunGroupFollowsTheCheckout(t *testing.T) {
	c := newCheckouts(t, "app", "feat")
	x := NewIndex()
	x.Observe(c.wt)
	runs := fakeRuns{
		5000: {group: "app", name: "web"},      // a pre-5A.6 client in the worktree
		5001: {group: "app@feat", name: "api"}, // the current spelling
		5002: {group: "custom", name: "job"},   // --group custom
		5003: {group: "app", name: "web"},      // in the main checkout
	}
	pp := Resolve([]state.Port{
		nativePort(5000, c.wt), nativePort(5001, c.wt), nativePort(5002, c.wt), nativePort(5003, c.main),
	}, NoPins{}, runs, x)
	want := []string{"app@feat", "app@feat", "custom", "app"}
	for i, p := range pp {
		if deref(p.Group) != want[i] {
			t.Errorf("port %d group = %q, want %q", p.Port, deref(p.Group), want[i])
		}
	}

	// After an alias, both spellings of the old name move with the project.
	x.SetAliases(map[string]string{c.main: "store"})
	pp = Resolve([]state.Port{nativePort(5001, c.wt), nativePort(5003, c.main)}, NoPins{}, runs, x)
	if deref(pp[0].Group) != "store@feat" || deref(pp[1].Group) != "store" {
		t.Errorf("aliased run groups = %q, %q", deref(pp[0].Group), deref(pp[1].Group))
	}
}

// TestGroupsPublishEachCheckoutOnce is the groups.list half: the main checkout
// and the worktree are two groups with distinct names — even stopped ones
// known only from the index — and each says which project, worktree and branch
// it is.
func TestGroupsPublishEachCheckoutOnce(t *testing.T) {
	c := newCheckouts(t, "shop-dir", "feat")
	writeFile(t, filepath.Join(c.main, ConfigName), "name: shop\nservices:\n  - name: api\n    port: 8000\n")
	writeFile(t, filepath.Join(c.wt, ConfigName), "name: shop\nservices:\n  - name: api\n    port: 8000\n")

	for _, running := range []bool{false, true} {
		x := NewIndex()
		x.Observe(c.wt)
		var pp []state.Port
		if running {
			pp = Resolve([]state.Port{nativePort(8000, c.wt)}, NoPins{}, NoRuns{}, x)
		}
		gg := Groups(pp, x)

		byName := map[string]state.Group{}
		for _, g := range gg {
			if _, dup := byName[g.Name]; dup {
				t.Fatalf("running=%v: two groups named %q in %+v", running, g.Name, gg)
			}
			byName[g.Name] = g
		}
		main, ok := byName["shop"]
		if !ok {
			t.Fatalf("running=%v: no main group in %v", running, groupNames(gg))
		}
		wt, ok := byName["shop@feat"]
		if !ok {
			t.Fatalf("running=%v: no worktree group in %v", running, groupNames(gg))
		}
		if main.Repo != "shop" || main.Worktree != "" || main.Branch != "main" {
			t.Errorf("main checkout fields = repo %q worktree %q branch %q", main.Repo, main.Worktree, main.Branch)
		}
		if wt.Repo != "shop" || wt.Worktree != "feat" || wt.Branch != "feature/x" {
			t.Errorf("worktree fields = repo %q worktree %q branch %q", wt.Repo, wt.Worktree, wt.Branch)
		}
		if wt.ConfigPath == nil || *wt.ConfigPath != filepath.Join(c.wt, ConfigName) {
			t.Errorf("worktree config_path = %v, want its own copy", wt.ConfigPath)
		}
		if running && (len(wt.Services) != 1 || !wt.Services[0].Running || len(main.Members) != 0) {
			t.Errorf("the worktree's api is the worktree's: main %+v, worktree %+v", main, wt)
		}
	}
}

func TestGroupsFieldsForAGroupThatIsNotACheckout(t *testing.T) {
	gg := Groups(Resolve([]state.Port{composePort(5433, "loose", "db")}, NoPins{}, NoRuns{}, NewIndex()), NewIndex())
	if len(gg) != 1 || gg[0].Repo != "loose" || gg[0].Worktree != "" || gg[0].Branch != "" {
		t.Fatalf("groups = %+v", gg)
	}
}

func TestNamedMatchesTheGroupNotTheFile(t *testing.T) {
	c := newCheckouts(t, "shop", "feat")
	writeFile(t, filepath.Join(c.main, ConfigName), "name: shop\n")
	writeFile(t, filepath.Join(c.wt, ConfigName), "name: shop\n")
	x := NewIndex()
	x.Observe(c.wt)

	if cfg, ok := x.Named("shop"); !ok || cfg.Dir != c.main {
		t.Errorf("Named(shop) = %+v, %v; want the main checkout's file", cfg, ok)
	}
	if cfg, ok := x.Named("shop@feat"); !ok || cfg.Dir != c.wt {
		t.Errorf("Named(shop@feat) = %+v, %v; want the worktree's copy", cfg, ok)
	}
}

func groupNames(gg []state.Group) []string {
	out := make([]string, 0, len(gg))
	for _, g := range gg {
		out = append(out, g.Name)
	}
	return out
}
