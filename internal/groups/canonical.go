package groups

import "path/filepath"

// Canonical is the single spelling of a path the resolver uses everywhere: it
// is absolute, cleaned, and has every symlink resolved.
//
// The index is a map keyed by directory, so a directory reached two ways is
// two entries unless every path is normalised the same way. On macOS that
// happens constantly: $TMPDIR is /var/folders/… while a process's cwd, read
// back from the kernel, is /private/var/folders/…. A config indexed under one
// spelling is then invisible to a lookup with the other, and `sonar start`
// falls through to the git root or the directory name instead of the group the
// `sonar.yaml` asked for.
//
// Callers normalise once, at the boundary where a path enters the resolver —
// Load, Index.Observe/Nearest/AddFile/Reload, Find and spawn.Resolve — and pass
// canonical paths from there on.
//
// A path that does not exist yet (a `sonar.yaml` about to be written, or one
// just deleted) still normalises: the deepest existing ancestor is resolved and
// the remaining names are appended. An empty path stays empty.
func Canonical(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}

	// EvalSymlinks needs every component to exist. Walk up to the part that
	// does, resolve that, and re-join the rest.
	rest := ""
	dir := abs
	for i := 0; i < maxWalk; i++ {
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		if rest == "" {
			rest = filepath.Base(dir)
		} else {
			rest = filepath.Join(filepath.Base(dir), rest)
		}
		dir = parent
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
	}
	return abs
}
