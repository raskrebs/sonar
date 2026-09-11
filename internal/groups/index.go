package groups

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/raskrebs/sonar/internal/state"
)

// Index is everything the resolver knows about where projects live: the
// `.sonar.yaml` files seen so far, the ones that failed to parse, and the
// working directory of each Compose project.
//
// It is in-memory and rebuilt per command in the CLI's direct-scan path; the
// daemon keeps one for its lifetime and the store step (1A.4) persists the
// known roots so a config is remembered across restarts.
type Index struct {
	configs map[string]*Config // config directory -> config
	invalid map[string]error   // config path -> parse/validation error
	compose map[string]string  // compose project -> working_dir label
	probed  map[string]bool    // directories already looked at
	stamps  map[string]stamp   // config path -> mtime and size when it was read
	aliases map[string]string  // main checkout root -> project name (groups.rename)
	heads   map[string]headEntry
}

// InvalidConfig is one config file that could not be used, reported by
// `sonar groups` and ignored by the resolver.
type InvalidConfig struct {
	Path string
	Err  error
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{
		configs: map[string]*Config{},
		invalid: map[string]error{},
		compose: map[string]string{},
		probed:  map[string]bool{},
		stamps:  map[string]stamp{},
		aliases: map[string]string{},
		heads:   map[string]headEntry{},
	}
}

// Add records an already-parsed config, replacing any config for the same
// directory.
func (x *Index) Add(cfg *Config) {
	if cfg == nil || cfg.Dir == "" {
		return
	}
	x.configs[cfg.Dir] = cfg
	x.probed[cfg.Dir] = true
	delete(x.invalid, cfg.Path)
	x.note(cfg.Path)
}

// AddFile loads one config path into the index. A file that does not parse or
// does not validate is recorded as invalid and reported by Invalid; the error
// is returned as well but is never fatal to the caller.
func (x *Index) AddFile(path string) error {
	cfg, err := Load(path)
	if err != nil {
		abs := Canonical(path)
		if abs == "" {
			abs = path
		}
		x.invalid[abs] = err
		x.probed[filepath.Dir(abs)] = true
		// A file that does not parse is still watched: fixing it is exactly
		// the moment the daemon should pick it up.
		x.note(abs)
		return err
	}
	x.Add(cfg)
	return nil
}

// Observe indexes every `.sonar.yaml` between dir and the git root that
// contains it (inclusive). This is the "seen via a process cwd" arm of the
// spec's discovery rules: a dev server running in a repo is enough for its
// project's config to be known, without anyone running `sonar init`.
func (x *Index) Observe(dir string) {
	if dir == "" {
		return
	}
	abs := Canonical(dir)
	co, hasRoot := Locate(abs)
	root := co.Root
	if hasRoot && co.Linked() {
		// The main checkout names every worktree of it (step 5A.6), so a
		// worktree coming into view brings the main checkout's config with it
		// — and with it the main checkout's group, running or not.
		x.probeDir(co.Main)
	}
	cur := abs
	for i := 0; i < maxWalk; i++ {
		x.probeDir(cur)
		if hasRoot && cur == root {
			return
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return
		}
		if !hasRoot {
			// Outside a repository, only the directory itself is interesting;
			// walking to / would index unrelated files.
			return
		}
		cur = parent
	}
}

// probeDir loads the config in one directory at most once per index.
func (x *Index) probeDir(dir string) {
	if x.probed[dir] {
		return
	}
	x.probed[dir] = true
	for _, name := range []string{ConfigName, altConfigName} {
		path := filepath.Join(dir, name)
		if info, err := os.Lstat(path); err == nil && !info.IsDir() {
			_ = x.AddFile(path)
			return
		}
	}
}

// AddComposeProject records the working directory of a Compose project, read
// from the `com.docker.compose.project.working_dir` label. It is what lets a
// Compose container merge into the git-root group of the repo it was started
// from instead of forming a group of its own.
func (x *Index) AddComposeProject(project, workingDir string) {
	if project == "" || workingDir == "" {
		return
	}
	x.compose[project] = workingDir
}

// ComposeDir returns the recorded working directory of a Compose project.
func (x *Index) ComposeDir(project string) string { return x.compose[project] }

// At returns the config in exactly this directory, or nil.
func (x *Index) At(dir string) *Config {
	if dir == "" {
		return nil
	}
	return x.configs[Canonical(dir)]
}

// Nearest returns the config in dir or the closest ancestor of it, or nil.
func (x *Index) Nearest(dir string) *Config {
	if dir == "" {
		return nil
	}
	// Configs are keyed by the canonical directory, so a caller passing the
	// unresolved path (/var/… against /private/var/… on macOS) has to be
	// normalised the same way or nothing matches.
	cur := Canonical(dir)
	for i := 0; i < maxWalk; i++ {
		if cfg, ok := x.configs[cur]; ok {
			return cfg
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
	return nil
}

// Configs returns every valid config, deepest directory first so that a nested
// project wins over the repository it sits in.
func (x *Index) Configs() []*Config {
	out := make([]*Config, 0, len(x.configs))
	for _, cfg := range x.configs {
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := out[i].Dir, out[j].Dir
		if len(di) != len(dj) {
			return len(di) > len(dj)
		}
		return di < dj
	})
	return out
}

// Invalid returns the files that could not be used, sorted by path.
func (x *Index) Invalid() []InvalidConfig {
	out := make([]InvalidConfig, 0, len(x.invalid))
	for path, err := range x.invalid {
		out = append(out, InvalidConfig{Path: path, Err: err})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// MatchPort finds the config that claims a port: one whose `ports:` list
// contains it, or one of whose services declares it, with the process cwd
// under the file's directory. The deepest matching config wins.
//
// A port with no cwd is not out of reach. Every platform can read a working
// directory now (see ports.batchGetCwds), but plenty of individual rows still
// arrive without one: a Docker container has no cwd of its own, and a process
// that denies the scanner access keeps its own. Requiring one left every
// `.sonar.yaml` unable to claim the ports it declares — the group existed in
// the index and no listener ever joined it. When the cwd is missing the
// question that remains is still answerable: is there exactly one known config
// claiming this port? One is an answer. Two is a guess, and a guess is worse
// than no group at all.
func (x *Index) MatchPort(p state.Port) (*Config, string, bool) {
	if p.Cwd != "" {
		for _, cfg := range x.Configs() {
			if !under(p.Cwd, cfg.Dir) {
				continue
			}
			if svc, ok := claims(cfg, p.Port); ok {
				return cfg, svc, true
			}
		}
		return nil, "", false
	}

	var (
		match *Config
		name  string
		found int
	)
	for _, cfg := range x.Configs() {
		if svc, ok := claims(cfg, p.Port); ok {
			match, name, found = cfg, svc, found+1
		}
	}
	if found == 1 {
		return match, name, true
	}
	return nil, "", false
}

// claims reports whether cfg declares this port, and under which service name
// ("" when the port comes from the file's own `ports:` list).
func claims(cfg *Config, port int) (string, bool) {
	for _, svc := range cfg.Services {
		if svc.Port != 0 && svc.Port == port {
			return svc.Name, true
		}
	}
	for _, p := range cfg.Ports {
		if p == port {
			return "", true
		}
	}
	return "", false
}

// under reports whether path is dir or lives inside it.
func under(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(Canonical(dir), Canonical(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !filepathHasParentPrefix(rel))
}

func filepathHasParentPrefix(rel string) bool {
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' && rel[2] == filepath.Separator
}
