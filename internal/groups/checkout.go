package groups

import (
	"os"
	"path/filepath"
	"strings"
)

// Checkout is where a directory sits in git.
//
// A repository can be checked out in several places at once: its main checkout
// and any number of linked worktrees (`git worktree add`). Each checkout is a
// group of its own — `<project>` for the main checkout, `<project>@<worktree>`
// for a linked one — and all of them share the project name, which only the
// main checkout decides (step 5A.6). A worktree's copy of a committed
// `sonar.yaml` describes its services, never its name, so a stale copy cannot
// fold the worktree back into the main checkout's group.
type Checkout struct {
	// Root is the directory holding the `.git` entry.
	Root string
	// Main is the main checkout's root: Root itself unless this is a linked
	// worktree. A submodule, and a `.git` file nothing can parse, are their
	// own main checkout.
	Main string
	// Worktree is a linked worktree's name, the base name of Root. It is
	// empty for a main checkout.
	Worktree string
	// head is the HEAD file the checkout's branch is read from, "" when there
	// is nothing to read.
	head string
}

// Linked reports whether the checkout is a linked worktree.
func (c Checkout) Linked() bool { return c.Worktree != "" }

// Locate finds the checkout that contains dir. It walks up the way Find does
// and answers the same root, plus the main checkout a linked worktree belongs
// to and the HEAD its branch is read from. Paths are canonical.
func Locate(dir string) (Checkout, bool) {
	if dir == "" {
		return Checkout{}, false
	}
	cur := Canonical(dir)
	if cur == "" {
		return Checkout{}, false
	}
	for i := 0; i < maxWalk; i++ {
		gitPath := filepath.Join(cur, ".git")
		if info, err := os.Lstat(gitPath); err == nil {
			return checkoutAt(cur, gitPath, info.Mode().IsRegular()), true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return Checkout{}, false
		}
		cur = parent
	}
	return Checkout{}, false
}

// checkoutAt describes the checkout rooted at root, whose `.git` entry is a
// directory or, for a linked worktree or a submodule, a file.
func checkoutAt(root, gitPath string, isFile bool) Checkout {
	co := Checkout{Root: root, Main: root}
	if !isFile {
		co.head = filepath.Join(gitPath, "HEAD")
		return co
	}
	target := gitdirTarget(root, gitPath)
	if target == "" {
		return co
	}
	co.head = filepath.Join(target, "HEAD")
	if main := worktreeMain(target); main != "" {
		co.Main = Canonical(main)
		co.Worktree = filepath.Base(root)
	}
	return co
}

// headEntry is one HEAD file's branch, with the stamp it was read at.
type headEntry struct {
	at     stamp
	branch string
}

// Branch is the branch checked out in co: "" when HEAD is detached, when it
// cannot be read, or when co has none. HEAD is re-read only when its mtime or
// size moves, so a scan costs one stat per checkout and never runs `git`.
func (x *Index) Branch(co Checkout) string {
	if co.head == "" {
		return ""
	}
	info, err := os.Stat(co.head)
	if err != nil {
		delete(x.heads, co.head)
		return ""
	}
	now := stamp{mod: info.ModTime(), size: info.Size()}
	if e, ok := x.heads[co.head]; ok && e.at.mod.Equal(now.mod) && e.at.size == now.size {
		return e.branch
	}
	data, err := os.ReadFile(co.head)
	if err != nil {
		return ""
	}
	branch := parseHead(data)
	if x.heads == nil {
		x.heads = map[string]headEntry{}
	}
	x.heads[co.head] = headEntry{at: now, branch: branch}
	return branch
}

// parseHead reads the branch out of a HEAD file: `ref: refs/heads/<branch>`.
// A detached HEAD holds a commit id instead, and a ref outside refs/heads is
// not a branch; both answer "".
func parseHead(data []byte) string {
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ref:")
	if !ok {
		return ""
	}
	branch, ok := strings.CutPrefix(strings.TrimSpace(ref), "refs/heads/")
	if !ok {
		return ""
	}
	return branch
}

// SetAliases installs the project names the daemon stores for projects that
// have no `sonar.yaml` to write a name into (`groups.rename`), keyed by the
// main checkout's root. The scanner sets them every tick from the store; a nil
// map clears them.
func (x *Index) SetAliases(aliases map[string]string) {
	x.aliases = make(map[string]string, len(aliases))
	for root, name := range aliases {
		if root != "" && name != "" {
			x.aliases[filepath.Clean(root)] = name
		}
	}
}

// fileProjectName is the project name before any alias: the name the main
// checkout's `sonar.yaml` gives it, else the main checkout's directory name.
// The main checkout is probed here, because a process in a linked worktree is
// what usually brings a project to the index's attention, and its walk stops
// at the worktree's own root.
func (x *Index) fileProjectName(main string) string {
	x.probeDir(main)
	if cfg := x.Nearest(main); cfg != nil {
		return cfg.Name
	}
	return filepath.Base(main)
}

// ProjectName is the name every checkout of one repository shares, decided by
// the main checkout at main: a stored alias, else the `name:` of the main
// checkout's `sonar.yaml`, else its directory name.
func (x *Index) ProjectName(main string) string {
	if alias := x.aliases[main]; alias != "" {
		return alias
	}
	return x.fileProjectName(main)
}

// CheckoutName is a checkout's group name: the project name, qualified as
// `<project>@<worktree>` for a linked worktree.
func (x *Index) CheckoutName(co Checkout) string {
	name := x.ProjectName(co.Main)
	if co.Linked() {
		return name + "@" + co.Worktree
	}
	return name
}

// GroupOf is the group a config's services are published under. A config at a
// checkout's root is that checkout's own, so it takes the checkout's group
// name rather than its `name:` — which is exactly what keeps a worktree's copy
// of a committed file from claiming the main checkout's name. A config below
// the root is a nested project with a name of its own, qualified with the
// worktree when it sits in one. A config outside any checkout is named by its
// file.
func (x *Index) GroupOf(cfg *Config) string {
	co, ok := Locate(cfg.Dir)
	if !ok {
		return cfg.Name
	}
	if cfg.Dir == co.Root {
		return x.CheckoutName(co)
	}
	if co.Linked() {
		return cfg.Name + "@" + co.Worktree
	}
	return cfg.Name
}

// NearestFor is Nearest confined to dir's own checkout when that checkout is a
// linked worktree. Tools put worktrees inside the main checkout (for example
// `<repo>/.claude/worktrees/<name>`), and an unconfined walk from one of them
// climbs straight into the main checkout's `sonar.yaml`.
func (x *Index) NearestFor(dir string) *Config {
	if co, ok := Locate(dir); ok {
		cfg := x.Nearest(dir)
		if cfg == nil || !x.within(cfg, co) {
			return nil
		}
		return cfg
	}
	return x.Nearest(dir)
}

// checkoutConfig is the config that makes a checkout's group a `file` group:
// NearestFor its root, for a checkout already located.
func (x *Index) checkoutConfig(co Checkout) *Config {
	cfg := x.Nearest(co.Root)
	if cfg == nil || !x.within(cfg, co) {
		return nil
	}
	return cfg
}

// within reports whether cfg may describe something in co: always for a main
// checkout, and only from inside the worktree for a linked one.
func (x *Index) within(cfg *Config, co Checkout) bool {
	return !co.Linked() || under(cfg.Dir, co.Root)
}

// runGroup is the post-step that keeps a `sonar start` run in its checkout's
// group. A run records the group its client inferred when it started, and a
// client knows neither the daemon's aliases nor, if it predates step 5A.6,
// that a worktree is not its main checkout. So a run group that is the
// project's own name — with or without the worktree — is the checkout's group
// under its current name. Any other name was chosen with --group and stays.
func (x *Index) runGroup(group string, co Checkout) string {
	base := x.fileProjectName(co.Main)
	alias := x.aliases[co.Main]
	if co.Linked() {
		switch group {
		case base, base + "@" + co.Worktree:
			return x.CheckoutName(co)
		}
		if alias != "" && (group == alias || group == alias+"@"+co.Worktree) {
			return x.CheckoutName(co)
		}
		return group
	}
	if group == base {
		return x.ProjectName(co.Main)
	}
	return group
}
