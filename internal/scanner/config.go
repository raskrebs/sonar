package scanner

import (
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/state"
)

// The daemon's `.sonar.yaml` index lives here, on the scan loop, because the
// loop is the one thing that reads it every tick and the contract makes it
// long-lived (contract §18). These accessors are how the group handlers
// (`groups.config.get`, `groups.config.set`, `groups.reload`, `groups.start`)
// reach it without a second copy going stale behind the first.

// index returns the loop's index, creating it if the first scan has not run
// yet. The caller holds attr.mu.
func (l *Loop) index() *groups.Index {
	if l.attr.index == nil {
		l.attr.index = groups.NewIndex()
		if l.attr.roots == nil {
			l.attr.roots = map[string]bool{}
		}
	}
	return l.attr.index
}

// Configs returns every valid `.sonar.yaml` the daemon knows, deepest first.
func (l *Loop) Configs() []*groups.Config {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	return l.index().Configs()
}

// ConfigNamed returns the config that names this group.
func (l *Loop) ConfigNamed(name string) (*groups.Config, bool) {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	return l.index().Named(name)
}

// GroupOf returns the group a config's services are published under. For a
// config at a checkout's root that is the checkout's group, whatever the file's
// own `name:` says: a linked worktree's copy names `<project>@<worktree>`.
func (l *Loop) GroupOf(cfg *groups.Config) string {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	return l.index().GroupOf(cfg)
}

// PlanGroupRename works out what `groups.rename` changes, against the groups
// the caller read and the index they were built from.
func (l *Loop) PlanGroupRename(gg []state.Group, from, to string) (*groups.RenamePlan, error) {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	return groups.PlanRename(gg, l.index(), from, to)
}

// ConfigAt returns the config read from this file.
func (l *Loop) ConfigAt(path string) (*groups.Config, bool) {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	return l.index().ByPath(path)
}

// ObserveConfig indexes the `.sonar.yaml` files at and above a directory, the
// way a process cwd does during a scan. `groups.start --config-path` uses it so
// a project the daemon has never seen still starts.
func (l *Loop) ObserveConfig(dir string) {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	l.index().Observe(dir)
}

// LoadConfig re-reads one config file into the index, replacing whatever was
// there. `groups.config.set` calls it after its write so the next delta already
// carries the edit.
func (l *Loop) LoadConfig(path string) error {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	return l.index().LoadFile(path)
}

// ReloadConfigs re-reads every known root: the ones in the store and the ones
// the index already holds. It picks up files created, edited and deleted since
// the daemon started, and returns how many valid configs remain plus the files
// that could not be used.
func (l *Loop) ReloadConfigs() (int, []groups.InvalidConfig) {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	return l.reloadLocked()
}

// reloadLocked is ReloadConfigs with attr.mu already held.
func (l *Loop) reloadLocked() (int, []groups.InvalidConfig) {
	var roots []string
	if st := l.opts.Store; st != nil {
		known, err := st.Roots()
		if err != nil {
			l.opts.Logger.Warn("reading known .sonar.yaml roots", "error", err)
		}
		roots = known
	}
	return l.index().Reload(roots)
}

// refreshStaleConfigs re-reads the index when a known config file has been
// written or deleted since it was last read. It runs once per scan tick and
// costs one stat per known file, which is what buys the daemon config
// hot-reloading without a filesystem watcher or a new dependency. The caller
// holds attr.mu.
func (l *Loop) refreshStaleConfigs() {
	if l.attr.index == nil || !l.attr.index.Stale() {
		return
	}
	loaded, bad := l.reloadLocked()
	l.opts.Logger.Info("reloaded .sonar.yaml after a change on disk",
		"configs", loaded, "invalid", len(bad))
}
