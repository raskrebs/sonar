package groups

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

// stamp is what the index remembers about a file so it can tell, with one
// stat, whether re-reading it would change anything.
type stamp struct {
	mod  time.Time
	size int64
}

// note records the current stamp of a config path.
func (x *Index) note(path string) {
	if x.stamps == nil {
		x.stamps = map[string]stamp{}
	}
	info, err := os.Stat(path)
	if err != nil {
		delete(x.stamps, path)
		return
	}
	x.stamps[path] = stamp{mod: info.ModTime(), size: info.Size()}
}

// Stale reports whether any known config file has been written or deleted
// since it was read. The daemon calls it once per scan tick: it is a handful of
// stats, which is cheap enough to do at the scan interval and is the whole
// reason there is no filesystem watcher (and no new dependency) here.
func (x *Index) Stale() bool {
	for path, was := range x.stamps {
		info, err := os.Stat(path)
		if err != nil {
			return true
		}
		if !info.ModTime().Equal(was.mod) || info.Size() != was.size {
			return true
		}
	}
	return false
}

// Known lists every config path the index has an opinion about, valid or not.
func (x *Index) Known() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, cfg := range x.configs {
		if cfg != nil && !seen[cfg.Path] {
			seen[cfg.Path] = true
			out = append(out, cfg.Path)
		}
	}
	for path := range x.invalid {
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// Named returns the valid config whose services are published as this group
// (see GroupOf). The deepest directory wins, matching Configs' ordering, so a
// nested project shadows the repository it sits in. Matching the group rather
// than the file's `name:` is what keeps a linked worktree's copy of a
// committed file from answering for the main checkout: the copy is
// `<project>@<worktree>`, and only the main checkout's file is `<project>`.
func (x *Index) Named(name string) (*Config, bool) {
	for _, cfg := range x.Configs() {
		if x.GroupOf(cfg) == name {
			return cfg, true
		}
	}
	return nil, false
}

// ByPath returns the valid config read from this file. The lookup resolves
// symlinks on both sides, because the index stores the resolved path the
// scanner walked to while a client sends whatever the user typed — on macOS
// that is /var/… against /private/var/….
func (x *Index) ByPath(path string) (*Config, bool) {
	want := Canonical(path)
	if want == "" {
		return nil, false
	}
	for _, cfg := range x.configs {
		if cfg != nil && cfg.Path == want {
			return cfg, true
		}
	}
	return nil, false
}

// Reload re-reads every config the index knows plus every directory in roots,
// picking up files that were created, edited or deleted since the last pass. It
// returns how many valid configs the index now holds and the files that could
// not be used.
//
// This is `groups.reload` and the daemon's own reaction to an mtime change. The
// index is long-lived (contract §18), so this is the only thing that lets a
// running daemon notice a `sonar.yaml` written after it started.
func (x *Index) Reload(roots []string) (int, []InvalidConfig) {
	dirs := map[string]bool{}
	for _, cfg := range x.configs {
		if cfg != nil && cfg.Dir != "" {
			dirs[cfg.Dir] = true
		}
	}
	for path := range x.invalid {
		dirs[filepath.Dir(path)] = true
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		// Roots come back from the store as the user typed them; the index
		// is keyed canonically.
		if abs := Canonical(root); abs != "" {
			dirs[abs] = true
		}
	}

	for dir := range dirs {
		// Forget what we knew about this directory, then look again: a file
		// that was deleted leaves nothing behind, and one that changed is read
		// afresh.
		if cfg, ok := x.configs[dir]; ok && cfg != nil {
			delete(x.stamps, cfg.Path)
			delete(x.configs, dir)
		}
		for _, name := range configNames {
			path := filepath.Join(dir, name)
			delete(x.invalid, path)
			delete(x.stamps, path)
		}
		delete(x.probed, dir)
		x.probeDir(dir)
	}

	return len(x.configs), x.Invalid()
}

// LoadFile re-reads one config into the index, replacing whatever was there.
// `groups.config.set` calls it after a write so the very next delta carries the
// edited services.
func (x *Index) LoadFile(path string) error { return x.AddFile(path) }
