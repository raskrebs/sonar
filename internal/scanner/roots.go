package scanner

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/raskrebs/sonar/internal/groups"
)

// Known roots only ever grow from the scan: every `sonar.yaml` directory the
// daemon sees is remembered. Deleted projects, removed git worktrees and the
// temp dirs of test runs whose listeners a live daemon happened to scan would
// pile up forever, so whenever the roots are loaded the ones that are
// definitely gone are forgotten.

// liveRoots returns the roots that may still hold a config and removes the
// rest from the store, logging one line for the lot. The caller holds attr.mu.
func (l *Loop) liveRoots(st Store, roots []string) []string {
	live := roots[:0:0]
	forgotten := 0
	for _, root := range roots {
		if !rootGone(root) {
			live = append(live, root)
			continue
		}
		if err := st.RemoveRoot(root); err != nil {
			l.opts.Logger.Warn("forgetting a sonar.yaml root", "dir", root, "error", err)
			live = append(live, root)
			continue
		}
		delete(l.attr.roots, root)
		forgotten++
	}
	if forgotten > 0 {
		l.opts.Logger.Info("forgot sonar.yaml roots that no longer exist",
			"forgotten", forgotten, "kept", len(live))
	}
	return live
}

// rootGone reports whether a root is definitely gone: the directory does not
// exist, or it exists and holds none of the config spellings. Anything short
// of a clean "not there" — a permission error, an I/O error, a path under a
// volume that is not mounted right now — keeps the root.
func rootGone(dir string) bool {
	info, err := os.Stat(dir)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return !underMissingMount(dir)
	default:
		return false
	}
	if !info.IsDir() {
		return true
	}
	for _, name := range groups.ConfigNames() {
		_, err := os.Lstat(filepath.Join(dir, name))
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

// mountParents are the directories removable and network volumes are mounted
// under. A root whose missing part starts right below one of them is on a
// volume that is not mounted, not a deleted project.
var mountParents = []string{"/Volumes", "/media", "/mnt", "/run/media"}

// underMissingMount reports whether dir is missing because a volume it lives
// on is not mounted, or because something above it cannot be read. It walks up
// to the first ancestor that exists and looks at the component just below it.
func underMissingMount(dir string) bool {
	missing := filepath.Clean(dir)
	for {
		parent := filepath.Dir(missing)
		if parent == missing {
			return false
		}
		_, err := os.Stat(parent)
		if err == nil {
			return isMountParent(parent)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return true
		}
		missing = parent
	}
}

// isMountParent reports whether volumes are mounted directly in dir. Under
// /media and /run/media they also sit one level down, in a per-user directory.
func isMountParent(dir string) bool {
	for _, p := range mountParents {
		if dir == p {
			return true
		}
		if (p == "/media" || p == "/run/media") && filepath.Dir(dir) == p {
			return true
		}
	}
	return false
}
