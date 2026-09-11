package runsreg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/sessions"
	"github.com/raskrebs/sonar/internal/spawn"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/raskrebs/sonar/internal/store"
)

// Default is the registry this daemon serves. One daemon, one registry: the
// handlers close over it and the OnStart hook publishes it to the runtime so
// the group resolver can attribute ports through it.
var Default = New()

// pruneInterval matches the scanner's base tick: a run whose process is gone
// disappears from `runs.list` about as fast as its port disappears from a
// snapshot.
const pruneInterval = scanner.BaseInterval

func init() {
	daemon.RegisterHandler("runs.register", handleRegister)
	daemon.RegisterHandler("runs.unregister", handleUnregister)
	daemon.RegisterHandler("runs.list", handleList)
	daemon.RegisterHandler("runs.spawn", handleSpawn)
	daemon.RegisterCapability("runs")
	daemon.OnGroupRename(func(renames map[string]string) { Default.RenameGroups(renames) })

	daemon.OnStart(func(rt *daemon.Runtime) {
		if n := Default.ImportLegacy(); n > 0 {
			rt.Logger.Info("imported runs.json into the run registry", "runs", n)
		}
		rt.SetRuns(Default)
		startPruning(rt)
	})
	daemon.OnShutdown(func(bool) { stopPruning() })
}

var pruneStop chan struct{}

// startPruning drops dead runs on the scanner's cadence. The scanner itself
// stays untouched: it publishes snapshots and knows nothing about runs.
func startPruning(rt *daemon.Runtime) {
	stopPruning()
	stop := make(chan struct{})
	pruneStop = stop
	go func() {
		t := time.NewTicker(pruneInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				rt.Runs().Prune()
			}
		}
	}()
}

func stopPruning() {
	if pruneStop != nil {
		close(pruneStop)
		pruneStop = nil
	}
}

func handleRegister(_ context.Context, req *daemon.Request) (any, error) {
	var p rpc.RunsRegisterParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	if p.PID <= 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "pid is required",
			`send {"pid": <pid>, "group": "...", "name": "..."}`)
	}
	cwd, err := checkCwd(p.Cwd, p.AllowOutsideHome)
	if err != nil {
		return nil, err
	}

	rec := Record{
		PID:   p.PID,
		PPID:  p.PPID,
		Group: strings.TrimSpace(p.Group),
		Name:  strings.TrimSpace(p.Name),
		Cmd:   p.Cmd,
		Cwd:   cwd,
	}
	if p.ID != nil {
		rec.ID = *p.ID
	}
	if rec.ID == "" {
		rec.ID = spawn.NewID()
	}
	if p.PortHint != nil {
		rec.PortHint = *p.PortHint
	}
	if t, err := time.Parse(time.RFC3339, p.StartedAt); err == nil {
		rec.StartedAt = t
	}
	if p.Session != nil && p.Session.ID != "" {
		rec.Session = *p.Session
	}

	rec = Default.Register(rec)
	rememberSession(req.Runtime, rec.Session)
	req.Runtime.Logger.Debug("run registered",
		"id", rec.ID, "pid", rec.PID, "group", rec.Group, "name", rec.Name)
	// The next scan should see the new run's ports, not wait out the backoff.
	req.Runtime.Scanner.Wake()
	return rpc.RunsRegisterResult{ID: rec.ID}, nil
}

func handleUnregister(_ context.Context, req *daemon.Request) (any, error) {
	var p rpc.RunsUnregisterParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	if p.PID <= 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "pid is required", `send {"pid": <pid>}`)
	}
	Default.Unregister(p.PID)
	req.Runtime.Scanner.Wake()
	// Unregistering a run nobody registered is not an error: `sonar start`
	// always cleans up, whether or not the daemon saw the registration.
	return rpc.OKResult{OK: true}, nil
}

func handleList(_ context.Context, req *daemon.Request) (any, error) {
	snap := snapshot(req.Runtime)
	records := Default.List()
	rows := make([]rpc.RunRecord, 0, len(records))
	for _, rec := range records {
		rows = append(rows, row(rec, snap))
	}
	return rpc.RunsListResult{Runs: rows}, nil
}

func handleSpawn(ctx context.Context, req *daemon.Request) (any, error) {
	var p rpc.RunsSpawnParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	if len(p.Argv) == 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "argv is required",
			`send {"argv": ["npm", "run", "dev"], "cwd": "/path/to/repo"}`)
	}
	cwd, err := checkCwd(p.Cwd, p.AllowOutsideHome)
	if err != nil {
		return nil, err
	}

	res := spawn.Resolve(cwd, p.Argv, deref(p.Group), deref(p.Name))
	hint := 0
	if p.PortHint != nil {
		hint = *p.PortHint
	}

	// The daemon's own environment says nothing about the agent that called
	// it, so a session has to be sent: `runs.spawn {session}` when the caller
	// captured one, otherwise whatever the request's own env carries.
	session := state.Session{}
	if p.Session != nil {
		session = *p.Session
	}
	if session.ID == "" {
		if s, ok := sessions.DetectFromEnv(envSlice(p.Env), sessions.Options{}); ok {
			s.Worktree, s.Branch = sessions.GitContext(cwd)
			session = s
		}
	}

	h, err := Spawn(ctx, req.Runtime, spawn.Request{
		Argv:     p.Argv,
		Cwd:      cwd,
		Env:      mergeEnv(p.Env),
		Group:    res.Group,
		Name:     res.Name,
		PortHint: hint,
		Session:  session,
		Detach:   true,
	})
	if err != nil {
		return nil, err
	}

	return rpc.RunsSpawnResult{
		// A run has no ports yet at the moment it starts, so `affected` is
		// empty; the ports arrive in the next state.delta.
		MutationResult: rpc.MutationResult{OK: true, Affected: []string{}},
		RunID:          h.ID,
		PID:            h.PID,
		LogPath:        h.LogPath,
	}, nil
}

// Spawn starts a detached run, registers it and reaps it. It is the one spawn
// path the daemon has: `runs.spawn` calls it for a single command and
// `groups.start` calls it once per service, so a service started from the
// desktop is attributed exactly like one started from the CLI (contract §4).
//
// The caller has already resolved the group, the name and the working
// directory; CheckCwd is the home-directory guard.
func Spawn(ctx context.Context, rt *daemon.Runtime, req spawn.Request) (*spawn.Handle, error) {
	req.Detach = true
	h, err := spawn.Spawn(ctx, req)
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternal, err.Error(),
			"check the command and its working directory")
	}

	Default.Register(Record{
		ID:        h.ID,
		PID:       h.PID,
		PPID:      h.PPID,
		Group:     h.Group,
		Name:      h.Name,
		Cmd:       h.Cmd,
		Cwd:       h.Cwd,
		PortHint:  h.PortHint,
		StartedAt: h.StartedAt,
		Session:   h.Session,
	})
	rememberSession(rt, h.Session)
	rt.Logger.Info("spawned a run",
		"id", h.ID, "pid", h.PID, "group", h.Group, "name", h.Name, "log", h.LogPath)
	rt.Scanner.Wake()

	// Reap it: an unwaited child stays a zombie, and a zombie pid still looks
	// alive to every liveness test, so the run would never be pruned.
	go func(rt *daemon.Runtime, h *spawn.Handle) {
		code, err := h.Wait()
		Default.Unregister(h.PID)
		rt.Logger.Info("run exited", "id", h.ID, "pid", h.PID, "code", code, "error", err)
		rt.Scanner.Wake()
	}(rt, h)
	return h, nil
}

// rememberSession writes a session through to the store the moment a run
// carries one, so an agent's session survives a daemon restart and stays
// readable for the seven days after its last run exits.
func rememberSession(rt *daemon.Runtime, s state.Session) {
	if s.ID == "" || rt == nil || rt.Store == nil {
		return
	}
	now := time.Now()
	err := rt.Store.Sessions().Upsert(store.SessionRow{
		ID: s.ID, Tool: s.Tool, Label: s.Label,
		Worktree: s.Worktree, Branch: s.Branch, Detected: s.Detected,
		FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		rt.Logger.Warn("recording an agent session", "session", s.ID, "error", err)
	}
}

// envSlice renders a wire env map as the KEY=VALUE slice detection reads.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// CheckCwd cleans a working directory and enforces the home-directory rule.
// `groups.start` reuses it so one place decides what the daemon may run.
func CheckCwd(cwd string, allowOutsideHome bool) (string, error) {
	return checkCwd(cwd, allowOutsideHome)
}

// row renders one run for `runs.list`, including the ports it currently holds
// and whether it is still coming up.
func row(rec Record, snap state.Snapshot) rpc.RunRecord {
	out := rpc.RunRecord{
		ID:        rec.ID,
		PID:       rec.PID,
		Group:     rec.Group,
		Name:      rec.Name,
		Cmd:       rec.Cmd,
		Cwd:       rec.Cwd,
		StartedAt: rec.StartedAt.Format(time.RFC3339),
		Ports:     portsOf(rec, snap),
		Status:    "running",
	}
	if rec.PortHint > 0 {
		hint := rec.PortHint
		out.PortHint = &hint
		if !contains(out.Ports, hint) {
			// The expected port is not listening yet: the desktop and
			// `daemon.status` show the run as coming up rather than missing.
			out.Status = "starting"
		}
	}
	return out
}

// portsOf collects the ports a run owns from the last snapshot.
func portsOf(rec Record, snap state.Snapshot) []int {
	out := []int{}
	seen := map[int]bool{}
	for i := range snap.Ports {
		p := snap.Ports[i]
		if p.Run == nil {
			continue
		}
		if p.Run.RootPID != rec.PID && (rec.ID == "" || p.Run.ID != rec.ID) {
			continue
		}
		if !seen[p.Port] {
			seen[p.Port] = true
			out = append(out, p.Port)
		}
	}
	return out
}

// snapshot reads the daemon's current state without forcing a scan when the
// cache is warm.
func snapshot(rt *daemon.Runtime) state.Snapshot {
	snap, err := rt.Scanner.Snapshot(scanner.Include{})
	if err != nil {
		return rt.Scanner.Cached()
	}
	return snap
}

// checkCwd cleans a client-supplied working directory and enforces the spec's
// home-directory rule: the daemon refuses to touch anything outside the user's
// home unless the caller explicitly opts in (daemon spec, "Transport details").
func checkCwd(cwd string, allowOutsideHome bool) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", rpc.NewError(rpc.CodeInvalidParams, "cwd is required", "")
		}
		cwd = wd
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", rpc.NewError(rpc.CodeInvalidParams, "cwd is not a usable path: "+cwd, "")
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if allowOutsideHome {
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return abs, nil
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	if !underHome(abs, home) {
		return "", rpc.NewError(rpc.CodeOutsideHome,
			fmt.Sprintf("%s is outside %s", abs, home),
			"pass allow_outside_home: true to start commands outside your home directory")
	}
	return abs, nil
}

// underHome reports whether path is home or inside it.
func underHome(path, home string) bool {
	if path == home {
		return true
	}
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// mergeEnv layers a client's environment overrides onto the daemon's own.
func mergeEnv(over map[string]string) []string {
	if len(over) == 0 {
		return nil
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		if key, _, ok := strings.Cut(kv, "="); ok {
			if _, replaced := over[key]; replaced {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range over {
		out = append(out, k+"="+v)
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
