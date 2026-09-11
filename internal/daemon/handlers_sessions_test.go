package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/killer"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/sessions"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/raskrebs/sonar/internal/store"
)

// fakeRuns is a run registry carrying sessions, standing in for
// internal/daemon/runsreg — which this package must not import.
type fakeRuns struct {
	live []sessions.Live
}

func (f *fakeRuns) Run(p state.Port) (state.Run, bool) {
	for _, l := range f.live {
		if l.PID == p.PID {
			return state.Run{ID: l.RunID, Group: l.Group, Name: l.Name, RootPID: l.PID}, true
		}
	}
	return state.Run{}, false
}

func (f *fakeRuns) Prune() {}

func (f *fakeRuns) GroupPIDs(group string) []int {
	var out []int
	for _, l := range f.live {
		if l.Group == group {
			out = append(out, l.PID)
		}
	}
	return out
}

func (f *fakeRuns) Session(p state.Port) (state.Session, bool) {
	for _, l := range f.live {
		if l.PID == p.PID {
			return l.Session, true
		}
	}
	return state.Session{}, false
}

func (f *fakeRuns) SessionRuns() []sessions.Live { return f.live }

func testSession(id string) state.Session {
	return state.Session{
		ID: id, Tool: sessions.ToolClaudeCode, Label: "ship it",
		Worktree: "feature-x", Branch: "feature/x", Detected: true,
	}
}

// sessionHarness is a daemon whose fake scan shows one listener owned by one
// agent session, with the run registry and the store both wired up.
func sessionHarness(t *testing.T, ctx context.Context) (*testHarness, *fakeRuns) {
	t.Helper()
	h := newHarness(t, ctx)
	runs := &fakeRuns{live: []sessions.Live{{
		RunID: "run1", PID: 4242, Group: "shop", Name: "web",
		Cmd: "python3 -m http.server", Cwd: "/home/me/code/shop",
		StartedAt: time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC),
		Session:   testSession("claude-code:abc"),
	}}}
	h.srv.runtime.SetRuns(runs)
	h.setRows(ports.ListeningPort{
		Port: 3000, BindAddress: "127.0.0.1", PID: 4242, Process: "python3",
		Command: "python3 -m http.server", IPVersion: "IPv4",
	})
	h.loop.Invalidate()
	if _, err := h.loop.Snapshot(scanner.Include{}); err != nil {
		t.Fatalf("priming the snapshot: %v", err)
	}
	return h, runs
}

// openSessionStore gives a harness a temp database with the sessions table in
// it. The directory is the caller's, claimed before the harness exists so the
// store is closed before it is removed.
func openSessionStore(t *testing.T, h *testHarness, dir string) {
	t.Helper()
	h.withStore(filepath.Join(dir, "sonar.db"))
}

func TestSessionsListReportsRunsPortsAndActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := sessionHarness(t, ctx)
	c := h.dial(ctx)

	var out rpc.SessionsListResult
	if e := c.call("sessions.list", rpc.SessionsListParams{}, &out); e != nil {
		t.Fatalf("sessions.list: %v", e)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want one", out.Sessions)
	}
	got := out.Sessions[0]
	if got.ID != "claude-code:abc" || got.Tool != sessions.ToolClaudeCode {
		t.Errorf("identity = %+v", got.Session)
	}
	if got.Worktree != "feature-x" || got.Branch != "feature/x" || !got.Detected {
		t.Errorf("git context did not survive the wire: %+v", got.Session)
	}
	if got.Runs != 1 || got.Ports != 1 || !got.Active {
		t.Errorf("counts = runs %d, ports %d, active %v", got.Runs, got.Ports, got.Active)
	}
}

func TestSessionsListActiveOnlyHidesFinishedSessions(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := sessionHarness(t, ctx)
	openSessionStore(t, h, dir)

	// A session recorded but with nothing running.
	if err := h.srv.runtime.Store.Sessions().Upsert(store.SessionRow{
		ID: "codex:done", Tool: sessions.ToolCodex,
		FirstSeen: time.Now().Add(-time.Hour), LastSeen: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("recording a finished session: %v", err)
	}

	c := h.dial(ctx)
	var all rpc.SessionsListResult
	if e := c.call("sessions.list", rpc.SessionsListParams{}, &all); e != nil {
		t.Fatalf("sessions.list: %v", e)
	}
	if len(all.Sessions) != 2 {
		t.Fatalf("sessions.list = %+v, want both", all.Sessions)
	}

	var active rpc.SessionsListResult
	if e := c.call("sessions.list", rpc.SessionsListParams{ActiveOnly: true}, &active); e != nil {
		t.Fatalf("sessions.list active_only: %v", e)
	}
	if len(active.Sessions) != 1 || active.Sessions[0].ID != "claude-code:abc" {
		t.Errorf("active_only = %+v", active.Sessions)
	}
}

func TestSessionsInspectListsRunsAndPorts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := sessionHarness(t, ctx)
	c := h.dial(ctx)

	var out rpc.SessionsInspectResult
	if e := c.call("sessions.inspect", rpc.SessionsInspectParams{ID: "claude-code:abc"}, &out); e != nil {
		t.Fatalf("sessions.inspect: %v", e)
	}
	if out.Session.ID != "claude-code:abc" {
		t.Errorf("session = %+v", out.Session)
	}
	if len(out.Runs) != 1 || out.Runs[0].ID != "run1" || out.Runs[0].PID != 4242 {
		t.Fatalf("runs = %+v", out.Runs)
	}
	if len(out.Runs[0].Ports) != 1 || out.Runs[0].Ports[0] != 3000 {
		t.Errorf("run ports = %+v", out.Runs[0].Ports)
	}
	if len(out.Ports) != 1 || out.Ports[0].Port != 3000 {
		t.Fatalf("ports = %+v", out.Ports)
	}
	if out.Ports[0].Session == nil || out.Ports[0].Session.ID != "claude-code:abc" {
		t.Errorf("the port came back without its session: %+v", out.Ports[0].Session)
	}
}

// A prefix is enough, because the ids are long and people type them.
func TestSessionsInspectAcceptsAUniquePrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := sessionHarness(t, ctx)
	c := h.dial(ctx)

	var out rpc.SessionsInspectResult
	if e := c.call("sessions.inspect", rpc.SessionsInspectParams{ID: "claude-code:a"}, &out); e != nil {
		t.Fatalf("sessions.inspect by prefix: %v", e)
	}
	if out.Session.ID != "claude-code:abc" {
		t.Errorf("session = %q", out.Session.ID)
	}
}

func TestSessionsInspectUnknownIDIsSessionNotFound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := sessionHarness(t, ctx)
	c := h.dial(ctx)

	e := c.call("sessions.inspect", rpc.SessionsInspectParams{ID: "nope"}, nil)
	if e == nil {
		t.Fatal("want an error for an unknown session")
	}
	if e.Code != rpc.CodeSessionNotFound {
		t.Errorf("code = %d, want %d (session_not_found)", e.Code, rpc.CodeSessionNotFound)
	}
}

func TestSessionsKillRequiresAnID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := sessionHarness(t, ctx)
	c := h.dial(ctx)

	if e := c.call("sessions.kill", rpc.SessionsKillParams{}, nil); e == nil {
		t.Fatal("want invalid_params for a missing id")
	}
}

// A dry run reports the plan and changes nothing, in the same envelope
// groups.kill returns (contract §3).
func TestSessionsKillDryRunReturnsTheKillEnvelope(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := sessionHarness(t, ctx)
	c := h.dial(ctx)

	var env rpc.KillEnvelope
	if e := c.call("sessions.kill", rpc.SessionsKillParams{ID: "claude-code:abc", DryRun: true}, &env); e != nil {
		t.Fatalf("sessions.kill --dry-run: %v", e)
	}
	if len(env.Results) != 1 {
		t.Fatalf("results = %+v, want the session's one port", env.Results)
	}
	if env.Results[0].Port != 3000 {
		t.Errorf("planned target = %+v", env.Results[0])
	}
	if env.Affected == nil {
		t.Error("affected is null; contract §3 wants an array")
	}
}

func TestSessionTargetsCoversPortsThenPidlessRuns(t *testing.T) {
	s := testSession("s1")
	snap := state.Snapshot{Ports: []state.Port{
		{Port: 3000, BindAddress: "127.0.0.1", PID: 100, Session: &s,
			Run: &state.Run{ID: "run1", RootPID: 100}},
		{Port: 5432, BindAddress: "127.0.0.1", PID: 999},
	}}
	live := []sessions.Live{
		{RunID: "run1", PID: 100, Session: s},
		{RunID: "run2", PID: 200, Session: s}, // started, nothing listening yet
	}

	got := sessionTargets(snap, live, "s1")
	if len(got) != 2 {
		t.Fatalf("targets = %+v, want the port and the silent run", got)
	}
	if got[0] != (killer.Target{Port: 3000, BindAddress: "127.0.0.1"}) {
		t.Errorf("first target = %+v", got[0])
	}
	if got[1] != (killer.Target{PID: 200}) {
		t.Errorf("second target = %+v, want the pid of the run with no port", got[1])
	}
}

func TestSweepSessionsTouchesLiveAndPrunesOld(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := sessionHarness(t, ctx)
	openSessionStore(t, h, dir)

	table := h.srv.runtime.Store.Sessions()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	long := now.Add(-30 * 24 * time.Hour)

	// The live session, recorded long ago, and a stale one nothing runs.
	if err := table.Upsert(store.SessionRow{ID: "claude-code:abc", FirstSeen: long, LastSeen: long}); err != nil {
		t.Fatal(err)
	}
	if err := table.Upsert(store.SessionRow{ID: "codex:gone", FirstSeen: long, LastSeen: long}); err != nil {
		t.Fatal(err)
	}

	SweepSessions(h.srv.runtime, now)

	rows, err := table.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "claude-code:abc" {
		t.Fatalf("after the sweep: %+v", rows)
	}
	if !rows[0].LastSeen.Equal(now.UTC()) {
		t.Errorf("the live session was not touched: %v", rows[0].LastSeen)
	}
}

func TestSessionsCapabilityIsAnnounced(t *testing.T) {
	for _, c := range Capabilities() {
		if c == "sessions" {
			return
		}
	}
	t.Errorf("daemon.hello capabilities = %v, want sessions", Capabilities())
}
