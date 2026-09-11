// Package runsreg is the daemon's registry of processes started through
// `sonar start` (and `runs.spawn`). It answers three questions: what is
// running, which run owns a listening port, and where does a detached run log.
//
// The registry is in memory and authoritative while the daemon lives. It also
// mirrors itself into the legacy ~/.config/sonar/runs.json, because the port
// scanner attributes listeners by walking their PPID ancestry against that
// file; keeping one writer (the daemon) and one reader (the scanner) is what
// makes `sonar list` show `group_source: start` with or without a daemon.
package runsreg

import (
	"os"
	"sort"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/runs"
	"github.com/raskrebs/sonar/internal/sessions"
	"github.com/raskrebs/sonar/internal/state"
)

// parentsTTL bounds how long one process table is reused while attributing a
// snapshot's worth of ports.
const parentsTTL = 2 * time.Second

// maxAncestry guards the PPID walk against a cyclic process table.
const maxAncestry = 64

// Record is one registered run.
type Record struct {
	ID        string
	PID       int
	PPID      int
	Group     string
	Name      string
	Cmd       string
	Cwd       string
	PortHint  int
	StartedAt time.Time
	// Session is the agent session that asked for this run, or the zero value
	// when nothing did (spec 2 §3). It travels with the run so every port the
	// run opens can be stamped with it.
	Session state.Session
}

// Registry holds the live runs. The zero value is not usable; call New.
type Registry struct {
	mu   sync.Mutex
	runs map[int]Record

	// Alive reports whether a pid is still running. Tests replace it.
	Alive func(pid int) bool
	// Parents returns a pid -> ppid table for the ancestry walk. Tests replace
	// it; production reads the same process table the scanner builds.
	Parents func() map[int]int
	// Mirror writes every change through to runs.json. Off in tests.
	Mirror bool

	parents   map[int]int
	parentsAt time.Time
	now       func() time.Time
}

// New returns an empty registry that mirrors to runs.json.
func New() *Registry {
	return &Registry{
		runs:    map[int]Record{},
		Alive:   runs.PIDAlive,
		Parents: ports.ParentTable,
		Mirror:  true,
		now:     time.Now,
	}
}

// Register records a run and returns it with its id filled in. Registering a
// pid twice replaces the entry: a re-registered pid is the same process.
func (r *Registry) Register(rec Record) Record {
	if rec.StartedAt.IsZero() {
		rec.StartedAt = r.clock()
	}
	r.mu.Lock()
	if existing, ok := r.runs[rec.PID]; ok && rec.ID == "" {
		rec.ID = existing.ID
	}
	r.runs[rec.PID] = rec
	r.mu.Unlock()

	r.mirrorAdd(rec)
	return rec
}

// Unregister drops the run with this pid, reporting whether there was one.
func (r *Registry) Unregister(pid int) bool {
	r.mu.Lock()
	_, ok := r.runs[pid]
	delete(r.runs, pid)
	r.mu.Unlock()
	if ok {
		r.mirrorRemove(pid)
	}
	return ok
}

// RenameGroups moves every run recorded under an old group name to its new one
// (`groups.rename`), so a service started before its project was renamed stays
// in the project's group instead of keeping a group of the old name to itself.
// It reports how many runs moved.
func (r *Registry) RenameGroups(renames map[string]string) int {
	if len(renames) == 0 {
		return 0
	}
	r.mu.Lock()
	var moved []Record
	for pid, rec := range r.runs {
		next, ok := renames[rec.Group]
		if !ok || next == "" {
			continue
		}
		rec.Group = next
		r.runs[pid] = rec
		moved = append(moved, rec)
	}
	r.mu.Unlock()
	for _, rec := range moved {
		r.mirrorAdd(rec)
	}
	return len(moved)
}

// List returns the live runs, oldest first, after pruning dead ones.
func (r *Registry) List() []Record {
	r.Prune()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, 0, len(r.runs))
	for _, rec := range r.runs {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].PID < out[j].PID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// Lookup returns the run registered for a pid.
func (r *Registry) Lookup(pid int) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.runs[pid]
	return rec, ok
}

// Prune drops every run whose process has exited. The scanner calls it each
// tick; List and the resolver call it too, so a stale run never survives a
// read.
func (r *Registry) Prune() {
	alive := r.Alive
	if alive == nil {
		alive = runs.PIDAlive
	}

	r.mu.Lock()
	var dead []int
	for pid := range r.runs {
		if !alive(pid) {
			dead = append(dead, pid)
		}
	}
	for _, pid := range dead {
		delete(r.runs, pid)
	}
	r.mu.Unlock()

	for _, pid := range dead {
		r.mirrorRemove(pid)
	}
}

// Run implements groups.Registry: it attributes a listening port to the run
// that owns it by walking the port's PPID ancestry, so `npm run dev` -> vite ->
// esbuild all resolve to the run that started them.
//
// It answers with the whole run because the resolver stamps it onto the row:
// this registry, not the runs.json mirror the scanner reads, is what a daemon
// knows about its own runs, and it knows it the moment `runs.register` returns.
func (r *Registry) Run(p state.Port) (state.Run, bool) {
	if rec, found := r.ancestor(p.PID, p.PPID); found {
		return state.Run{ID: rec.ID, Group: rec.Group, Name: rec.Name, RootPID: rec.PID}, true
	}
	// The scanner already walked the ancestry against the mirrored file while
	// enriching this port; trust that rather than walking a process table that
	// has moved on since.
	if p.Run != nil && (p.Run.Group != "" || p.Run.Name != "") {
		return *p.Run, true
	}
	return state.Run{}, false
}

// PortHint implements groups.PortHints: the port a live run of this service
// was started to bind, which for a `port: auto` service is the one groups.start
// assigned it.
func (r *Registry) PortHint(group, service string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.runs {
		if rec.Group == group && rec.Name == service && rec.PortHint > 0 {
			return rec.PortHint, true
		}
	}
	return 0, false
}

// GroupPIDs lists the live runs recorded under a group, so `sonar down` can
// stop the ones that hold no port — a worker, or a service still starting —
// which a kill by port never reaches.
func (r *Registry) GroupPIDs(group string) []int {
	var out []int
	for _, rec := range r.List() {
		if rec.Group == group {
			out = append(out, rec.PID)
		}
	}
	return out
}

// Session implements groups.SessionRegistry: it reports the agent session that
// started the run owning this port, using the same PPID walk the run
// attribution uses, so a port and its run can never disagree about who started
// them.
func (r *Registry) Session(p state.Port) (state.Session, bool) {
	rec, found := r.ancestor(p.PID, p.PPID)
	if !found || rec.Session.ID == "" {
		return state.Session{}, false
	}
	return rec.Session, true
}

// SessionRuns lists every live run that carries a session. The daemon's
// sessions handlers read it through an interface assertion on the installed
// run registry, which is how package daemon reaches this data without
// importing the package that registers itself into it.
func (r *Registry) SessionRuns() []sessions.Live {
	out := []sessions.Live{}
	for _, rec := range r.List() {
		if rec.Session.ID == "" {
			continue
		}
		out = append(out, sessions.Live{
			RunID:     rec.ID,
			PID:       rec.PID,
			Group:     rec.Group,
			Name:      rec.Name,
			Cmd:       rec.Cmd,
			Cwd:       rec.Cwd,
			StartedAt: rec.StartedAt,
			Session:   rec.Session,
		})
	}
	return out
}

// ancestor walks up from pid looking for a registered run. hintPPID is the
// parent the scanner already resolved, used before the process table is read.
func (r *Registry) ancestor(pid, hintPPID int) (Record, bool) {
	if pid <= 0 {
		return Record{}, false
	}
	if rec, ok := r.Lookup(pid); ok {
		return rec, true
	}
	if hintPPID > 1 {
		if rec, ok := r.Lookup(hintPPID); ok {
			return rec, true
		}
	}

	r.mu.Lock()
	empty := len(r.runs) == 0
	r.mu.Unlock()
	if empty {
		return Record{}, false
	}

	parents := r.parentTable()
	cur := pid
	for i := 0; i < maxAncestry; i++ {
		next, ok := parents[cur]
		if !ok || next <= 1 || next == cur {
			return Record{}, false
		}
		if rec, ok := r.Lookup(next); ok {
			return rec, true
		}
		cur = next
	}
	return Record{}, false
}

// parentTable returns a recent pid -> ppid map, rebuilding it at most once
// every parentsTTL so attributing a whole snapshot costs one process listing.
func (r *Registry) parentTable() map[int]int {
	r.mu.Lock()
	fresh := r.parents != nil && r.clock().Sub(r.parentsAt) < parentsTTL
	table := r.parents
	load := r.Parents
	r.mu.Unlock()
	if fresh || load == nil {
		return table
	}

	table = load()
	r.mu.Lock()
	r.parents, r.parentsAt = table, r.clock()
	r.mu.Unlock()
	return table
}

// ImportLegacy takes ownership of ~/.config/sonar/runs.json: every live entry
// becomes a run in this registry and the file is deleted, then rewritten from
// the registry, so a `sonar start` that ran without a daemon is not lost when
// one appears (daemon spec, migration table).
func (r *Registry) ImportLegacy() int {
	reg := runs.Load() // prunes dead pids on the way in
	imported := 0
	for _, e := range reg.Active() {
		rec := Record{
			ID:       e.ID,
			PID:      e.PID,
			PPID:     e.PPID,
			Group:    e.GroupOf(),
			Name:     e.NameOf(),
			Cmd:      e.Cmd,
			Cwd:      e.Cwd,
			PortHint: e.PortHint,
		}
		if t, err := time.Parse(time.RFC3339, e.StartedAt); err == nil {
			rec.StartedAt = t
		}
		r.mu.Lock()
		r.runs[rec.PID] = rec
		r.mu.Unlock()
		imported++
	}
	_ = os.Remove(runs.Path())
	if imported > 0 {
		for _, rec := range r.List() {
			r.mirrorAdd(rec)
		}
	}
	return imported
}

// mirrorAdd writes one run through to runs.json.
func (r *Registry) mirrorAdd(rec Record) {
	if !r.Mirror {
		return
	}
	_ = runs.Add(runs.Entry{
		PID:       rec.PID,
		Tag:       rec.Group,
		ID:        rec.ID,
		Cmd:       rec.Cmd,
		StartedAt: rec.StartedAt.Format(time.RFC3339),
		Group:     rec.Group,
		Name:      rec.Name,
		Cwd:       rec.Cwd,
		PPID:      rec.PPID,
		PortHint:  rec.PortHint,
	})
}

// mirrorRemove drops one run from runs.json.
func (r *Registry) mirrorRemove(pid int) {
	if !r.Mirror {
		return
	}
	_ = runs.Remove(pid)
}

func (r *Registry) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}
