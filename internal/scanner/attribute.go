package scanner

import (
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/raskrebs/sonar/internal/store"
)

// Store is the slice of the daemon's SQLite store the scan tick uses: the
// renames and pins it applies, the `.sonar.yaml` roots it remembers and the
// history ring it appends to. It is an interface so the loop can run without a
// database — the CLI's direct-scan path and most tests do.
type Store interface {
	Renames() (map[string]string, error)
	Pins() (map[string]string, error)
	Roots() ([]string, error)
	AddRoot(path string) error
	AppendBatch(events []store.HistoryEvent) error
	GroupAliases() (map[string]string, error)
}

// attribution is the per-loop group state: the index of known `.sonar.yaml`
// files, which lives as long as the daemon, and the roots already written to
// the store.
type attribution struct {
	mu     sync.Mutex
	index  *groups.Index
	seeded bool
	roots  map[string]bool
}

// SetStore installs the store after construction. The daemon does it from
// Serve, once the database is open; a nil store leaves the loop unpersisted.
func (l *Loop) SetStore(s Store) {
	if s == nil {
		return
	}
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	l.opts.Store = s
	l.attr.seeded = false
}

// SetRuns installs the run-registry accessor. It is a function because
// `sonar start` (step 1A.5) hands the daemon its registry after the loop is
// already scanning.
func (l *Loop) SetRuns(f func() groups.Registry) {
	if f == nil {
		return
	}
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	l.opts.Runs = f
}

// SetSessions installs the sessions-collection builder. Like SetRuns it is a
// setter because the daemon extension that owns sessions registers itself
// after the loop is already scanning.
func (l *Loop) SetSessions(f func(ports []state.Port) []state.SessionRecord) {
	if f == nil {
		return
	}
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	l.opts.Sessions = f
}

// sessions builds the snapshot's sessions collection, never nil.
func (l *Loop) sessions(rows []state.Port) []state.SessionRecord {
	l.attr.mu.Lock()
	build := l.opts.Sessions
	l.attr.mu.Unlock()
	if build == nil {
		return []state.SessionRecord{}
	}
	if out := build(rows); out != nil {
		return out
	}
	return []state.SessionRecord{}
}

// Invalidate drops the cached scan so the next read rescans instead of serving
// state from before a write. `ports.rename` and `groups.assign` call it so the
// caller sees its own change in the very next list.
func (l *Loop) Invalidate() {
	l.mu.Lock()
	l.lastScanAt = time.Time{}
	l.interval = l.base
	l.mu.Unlock()
	l.Wake()
}

// attribute is the group and rename half of a scan tick. It resolves every
// port's group with the pins loaded from the store, applies the stored renames
// to display_name, remembers any newly seen `.sonar.yaml` root and builds the
// group collection.
func (l *Loop) attribute(pp []ports.ListeningPort) ([]state.Port, []state.Group) {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()

	if l.attr.index == nil {
		l.attr.index = groups.NewIndex()
		l.attr.roots = map[string]bool{}
	}
	st := l.opts.Store
	l.seedRoots(st)
	// A config edited or deleted since the last tick is re-read here, so the
	// daemon's long-lived index tracks the disk without a filesystem watcher.
	l.refreshStaleConfigs()

	pins, renames := l.load(st)
	l.loadAliases(st)

	resolved, index := groups.AttributeWith(pp, pins, l.registry(), l.attr.index)
	l.attr.index = index

	applyRenames(renames, resolved, pp)
	l.rememberRoots(st, index)

	return resolved, groups.Groups(resolved, index)
}

// seedRoots loads the known `.sonar.yaml` roots into the index once, so a
// project configured before the last restart is a group again immediately,
// without waiting for one of its processes to be seen.
func (l *Loop) seedRoots(st Store) {
	if st == nil || l.attr.seeded {
		return
	}
	l.attr.seeded = true
	roots, err := st.Roots()
	if err != nil {
		l.opts.Logger.Warn("reading known .sonar.yaml roots", "error", err)
		return
	}
	for _, root := range roots {
		l.attr.roots[root] = true
		l.attr.index.Observe(root)
	}
}

// load reads the pin and rename tables for this tick. A read failure is logged
// and treated as "no pins this tick" rather than failing the scan: the daemon
// keeps reporting ports even with an unreadable database.
func (l *Loop) load(st Store) (groups.Pins, map[string]string) {
	if st == nil {
		return groups.NoPins{}, nil
	}
	pins, err := st.Pins()
	if err != nil {
		l.opts.Logger.Warn("reading group pins", "error", err)
	}
	renames, err := st.Renames()
	if err != nil {
		l.opts.Logger.Warn("reading renames", "error", err)
	}
	return pinSet(pins), renames
}

// loadAliases hands the index this tick's project names from `groups.rename`.
// It runs under the ordering gate like the pin read, so a republish after a
// rename always resolves with the name it just stored. A failed read keeps the
// last tick's names rather than flapping every renamed project back to its
// directory name.
func (l *Loop) loadAliases(st Store) {
	if st == nil {
		l.attr.index.SetAliases(nil)
		return
	}
	aliases, err := st.GroupAliases()
	if err != nil {
		l.opts.Logger.Warn("reading project aliases", "error", err)
		return
	}
	l.attr.index.SetAliases(aliases)
}

// registry is the run registry to attribute with: the one `sonar start`
// installed, or the attribution the scanner already put on the port itself.
func (l *Loop) registry() groups.Registry {
	if l.opts.Runs == nil {
		return groups.PortRuns{}
	}
	if r := l.opts.Runs(); r != nil {
		return r
	}
	return groups.PortRuns{}
}

// rememberRoots persists a `.sonar.yaml` directory the first time it is seen,
// so the next daemon start knows about the project without walking the disk.
func (l *Loop) rememberRoots(st Store, index *groups.Index) {
	if st == nil {
		return
	}
	for _, cfg := range index.Configs() {
		if cfg == nil || cfg.Dir == "" || l.attr.roots[cfg.Dir] {
			continue
		}
		l.attr.roots[cfg.Dir] = true
		if err := st.AddRoot(cfg.Dir); err != nil {
			l.opts.Logger.Warn("recording a .sonar.yaml root", "dir", cfg.Dir, "error", err)
		}
	}
}

// pinSet adapts the store's key→group map to the resolver's Pins interface.
type pinSet map[string]string

// Group returns the pin that applies to p, matched down the key ladder.
func (p pinSet) Group(port state.Port) (string, bool) { return store.Lookup(p, port) }

// applyRenames puts the stored rename on every port it matches. The rename is
// what clients render, so it wins for display_name; `name` carries the rename
// itself and stays null for a port nobody has renamed.
func applyRenames(renames map[string]string, resolved []state.Port, pp []ports.ListeningPort) {
	if len(renames) == 0 {
		return
	}
	for i := range resolved {
		name, ok := store.Lookup(renames, resolved[i])
		if !ok || name == "" {
			continue
		}
		n := name
		resolved[i].DisplayName = n
		resolved[i].Name = &n
		if i < len(pp) {
			pp[i].Name = n
		}
	}
}

// record writes the port transitions of one published delta to the history
// ring. Only the three port-scoped kinds are persisted; health_changed and
// scan_error stay in-memory notifications.
func (l *Loop) record(events []state.Event) {
	st := l.store()
	if st == nil || len(events) == 0 {
		return
	}
	rows := make([]store.HistoryEvent, 0, len(events))
	for _, ev := range events {
		if ev.Port == nil {
			continue
		}
		switch ev.Kind {
		case store.EventPortUp, store.EventPortDown, store.EventPortRestarted:
		default:
			continue
		}
		p := *ev.Port
		rows = append(rows, store.HistoryEvent{
			At:          eventTime(ev.At, l.now()),
			Kind:        ev.Kind,
			Port:        p.Port,
			PID:         p.PID,
			DisplayName: p.DisplayName,
			Group:       deref(p.Group),
			Bind:        p.BindAddress,
			ProjectRoot: deref(p.ProjectRoot),
			Command:     p.Command,
		})
	}
	if err := st.AppendBatch(rows); err != nil {
		l.opts.Logger.Warn("recording port history", "error", err)
	}
}

// store returns the installed store under the attribution lock.
func (l *Loop) store() Store {
	l.attr.mu.Lock()
	defer l.attr.mu.Unlock()
	return l.opts.Store
}

func eventTime(at string, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return t
	}
	return fallback
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
