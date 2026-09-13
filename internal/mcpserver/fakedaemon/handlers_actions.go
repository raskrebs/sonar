package fakedaemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// The write half of the fake daemon: the methods the MCP action tools call
// (`ports.kill`, `groups.kill`, `sessions.kill`, `ports.rename`, `runs.spawn`,
// `runs.list`, `ports.wait`). They are opt-in — RegisterActions installs them —
// because a fake that can kill and spawn is a different thing from a fixture
// that only answers reads, and a test should say which one it wants.
//
// The world they mutate is the fixture itself: a kill removes the rows it
// killed, a rename rewrites the row's name, a spawned run's ports appear when
// the test says they do (Actions.OpenPortForRun). So `kill` followed by
// `list_ports` tells the same story here as against a real daemon.

// Actions is the mutable state behind the write methods: the runs the fake has
// spawned, the pids it refuses to signal, and the streams it is running. It is
// returned by RegisterActions so a test can drive them.
type Actions struct {
	f *Fake

	mu        sync.Mutex
	runs      []rpc.RunRecord
	denied    map[int]bool
	streams   map[string]context.CancelFunc
	seq       int
	streamSeq int
}

// RegisterActions installs the write methods on f and returns the handle a test
// drives them with. It is safe to call on a fake that is already serving.
func (f *Fake) RegisterActions() *Actions {
	a := &Actions{f: f, denied: map[int]bool{}, streams: map[string]context.CancelFunc{}}
	f.Handle("ports.kill", a.portsKill)
	f.Handle("groups.kill", a.groupsKill)
	f.Handle("sessions.kill", a.sessionsKill)
	f.Handle("ports.rename", a.portsRename)
	f.Handle("runs.spawn", a.runsSpawn)
	f.Handle("runs.list", a.runsList)
	f.Handle("ports.wait", a.portsWait)
	f.Handle(rpc.MethodStreamCancel, a.streamCancel)
	return a
}

// DenyKill makes every attempt to signal pid come back as a failed row with the
// killer's own permission_denied wording, the way a port owned by another user
// behaves.
func (a *Actions) DenyKill(pid int) {
	a.mu.Lock()
	a.denied[pid] = true
	a.mu.Unlock()
}

// Runs lists what runs.spawn has started, newest last.
func (a *Actions) Runs() []rpc.RunRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]rpc.RunRecord{}, a.runs...)
}

// OpenPortForRun is the moment a spawned service starts listening: it adds a
// port row attributed to the run, which is what makes a pending `ports.wait`
// finish and what `wait_for_port: "auto"` polls runs.list for.
func (a *Actions) OpenPortForRun(runID string, port int) state.Port {
	a.mu.Lock()
	var rec *rpc.RunRecord
	for i := range a.runs {
		if a.runs[i].ID == runID {
			rec = &a.runs[i]
		}
	}
	if rec != nil && !containsInt(rec.Ports, port) {
		rec.Ports = append(rec.Ports, port)
		rec.Status = "running"
	}
	var group, name string
	var pid int
	if rec != nil {
		group, name, pid = rec.Group, rec.Name, rec.PID
	}
	a.mu.Unlock()

	row := state.Port{
		Port: port, BindAddress: "127.0.0.1", IPVersion: "IPv4",
		URL: fmt.Sprintf("http://localhost:%d", port), PID: pid, PPID: 1,
		Process: "python3", DisplayName: orDefault(name, "spawned"),
		Command: "python3 -m http.server", Cwd: "/home/dev/shop",
		Group: strp(group), GroupSource: srcp(state.SourceStart),
		Type: state.TypeUser, User: "dev",
		Run:         &state.Run{ID: runID, Group: group, Name: name, RootPID: pid},
		ExposedURLs: []string{}, StartedAt: strp(FixtureTime),
	}
	if group == "" {
		row.Group = nil
	}
	a.f.mu.Lock()
	a.f.fixture.Ports = append(a.f.fixture.Ports, row)
	a.f.mu.Unlock()
	return row
}

// ------------------------------------------------------------------ kills ---

func (a *Actions) portsKill(raw json.RawMessage) (any, error) {
	var p rpc.PortsKillParams
	if err := unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Targets) == 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "targets is required",
			`send {"targets": [{"port": 3000}]}`)
	}

	rows := []state.KillResult{}
	for _, sel := range p.Targets {
		if err := checkSelector(sel); err != nil {
			return nil, err
		}
		matches := a.match(sel)
		if len(matches) == 0 {
			rows = append(rows, missingRow(sel))
			continue
		}
		for _, row := range matches {
			rows = append(rows, a.killRows(row, p.Tree, p.Force)...)
		}
	}
	return a.finishKill(rows, p.DryRun), nil
}

func (a *Actions) groupsKill(raw json.RawMessage) (any, error) {
	var p rpc.GroupsKillParams
	if err := unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "name is required", `send {"name": "my-app"}`)
	}
	var matches []state.Port
	for _, row := range a.f.Fixture().Ports {
		if inGroupFold(row, p.Name) {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return nil, rpc.NewError(rpc.CodeNotFound,
			"no listening port belongs to group "+p.Name,
			"run `sonar groups` to see what is grouped right now")
	}

	rows := []state.KillResult{}
	for _, row := range matches {
		// A group kill is always a tree kill of each member (contract §17).
		rows = append(rows, a.killRows(row, true, p.Force)...)
	}
	return a.finishKill(rows, p.DryRun), nil
}

func (a *Actions) sessionsKill(raw json.RawMessage) (any, error) {
	var p rpc.SessionsKillParams
	if err := unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.ID) == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "id is required", `send {"id": "claude-code:9f2c"}`)
	}
	var matches []state.Port
	for _, row := range a.f.Fixture().Ports {
		if row.Session != nil && (row.Session.ID == p.ID || row.Session.Label == p.ID) {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return nil, rpc.NewError(rpc.CodeNotFound,
			"no live run belongs to session "+p.ID,
			"run `sonar sessions` to see which sessions are active")
	}

	rows := []state.KillResult{}
	for _, row := range matches {
		rows = append(rows, a.killRows(row, p.Tree, p.Force)...)
	}
	return a.finishKill(rows, p.DryRun), nil
}

// killRows is what the killer would report for one listening row: a container
// is stopped rather than signalled, and a tree kill of a `sonar start` run
// signals the child before the run root (contract §17).
func (a *Actions) killRows(row state.Port, tree, force bool) []state.KillResult {
	if row.Docker != nil {
		return []state.KillResult{{
			Port: row.Port, BindAddress: row.BindAddress, PID: 0,
			Name: row.Docker.Container, Method: state.MethodDockerStop, OK: true,
		}}
	}

	method := state.MethodSIGTERM
	if force {
		method = state.MethodSIGKILL
	}
	pids := []int{row.PID}
	if tree && row.Run != nil && row.Run.RootPID != 0 && row.Run.RootPID != row.PID {
		pids = append(pids, row.Run.RootPID)
	}

	out := make([]state.KillResult, 0, len(pids))
	for _, pid := range pids {
		res := state.KillResult{
			Port: row.Port, BindAddress: row.BindAddress, PID: pid,
			Name: row.DisplayName, Method: method, OK: true,
		}
		if a.isDenied(pid) {
			res.Method = state.MethodNone
			res.OK = false
			res.Error = fmt.Sprintf("not permitted to signal PID %d", pid)
		}
		out = append(out, res)
	}
	return out
}

// finishKill wraps the rows in the contract §3 envelope and, for a real kill,
// takes the ports it killed out of the fixture — the rescan §22 requires,
// played back at fixture speed.
func (a *Actions) finishKill(rows []state.KillResult, dryRun bool) rpc.KillEnvelope {
	env := rpc.KillEnvelope{Results: rows}
	env.OK = true
	env.Affected = []string{}
	seen := map[string]bool{}
	killed := map[string]bool{}
	for _, r := range rows {
		if !r.OK {
			env.OK = false
			continue
		}
		killed[r.Key()] = true
		key := r.Key()
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		env.Affected = append(env.Affected, key)
	}

	if !dryRun && len(killed) > 0 {
		kept := []state.Port{}
		for _, row := range a.f.Fixture().Ports {
			if !killed[row.Key()] {
				kept = append(kept, row)
			}
		}
		a.f.SetPorts(kept)
	}
	return env
}

// ----------------------------------------------------------------- rename ---

func (a *Actions) portsRename(raw json.RawMessage) (any, error) {
	var p rpc.PortsRenameParams
	if err := unmarshal(raw, &p); err != nil {
		return nil, err
	}
	target, err := resolvePort(a.f.Fixture().Ports, p.Selector)
	if err != nil {
		return nil, err
	}

	a.f.mu.Lock()
	for i := range a.f.fixture.Ports {
		row := &a.f.fixture.Ports[i]
		if row.Key() != target.Key() {
			continue
		}
		if p.Name == nil || strings.TrimSpace(*p.Name) == "" {
			row.Name = nil
			row.DisplayName = row.Process
			continue
		}
		name := strings.TrimSpace(*p.Name)
		row.Name = &name
		row.DisplayName = name
	}
	a.f.mu.Unlock()

	return rpc.PortsRenameResult{
		MutationResult: rpc.MutationResult{OK: true, Affected: []string{target.Key()}},
		Key:            target.Key(),
		Name:           p.Name,
	}, nil
}

// ------------------------------------------------------------------- runs ---

func (a *Actions) runsSpawn(raw json.RawMessage) (any, error) {
	var p rpc.RunsSpawnParams
	if err := unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Argv) == 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "argv is required",
			`send {"argv": ["npm", "run", "dev"], "cwd": "/path/to/repo"}`)
	}
	if strings.TrimSpace(p.Cwd) == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "cwd is required",
			`send the absolute path the command should run in`)
	}

	a.mu.Lock()
	a.seq++
	rec := rpc.RunRecord{
		ID:        fmt.Sprintf("run-fake-%d", a.seq),
		PID:       31000 + a.seq,
		Group:     orDefault(deref(p.Group), filepath.Base(p.Cwd)),
		Name:      orDefault(deref(p.Name), p.Argv[0]),
		Cmd:       strings.Join(p.Argv, " "),
		Cwd:       p.Cwd,
		StartedAt: FixtureTime,
		Ports:     []int{},
		Status:    "starting",
	}
	if p.PortHint != nil {
		hint := *p.PortHint
		rec.PortHint = &hint
	}
	a.runs = append(a.runs, rec)
	a.mu.Unlock()

	return rpc.RunsSpawnResult{
		// Contract §19: a run has no ports at the moment it starts.
		MutationResult: rpc.MutationResult{OK: true, Affected: []string{}},
		RunID:          rec.ID,
		PID:            rec.PID,
		LogPath:        filepath.Join("/home/dev/.config/sonar/logs", rec.ID+".log"),
	}, nil
}

func (a *Actions) runsList(json.RawMessage) (any, error) {
	// Exited is always an array on the wire, even when nothing has ended.
	return rpc.RunsListResult{Runs: a.Runs(), Exited: []rpc.RunRecord{}}, nil
}

// ------------------------------------------------------------------- wait ---

// portsWait is the streaming half of the fake: it replies with a subscription
// id and then polls its own fixture, pushing one chunk per port that starts
// listening and ending with {ready, timed_out}.
func (a *Actions) portsWait(raw json.RawMessage) (any, error) {
	var p rpc.PortsWaitParams
	if err := unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Ports) == 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "ports is required",
			`send {"ports": [3000], "timeout_ms": 30000}`)
	}
	if p.TimeoutMs <= 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "timeout_ms must be positive", "")
	}
	interval := 20 * time.Millisecond
	if p.IntervalMs > 0 {
		interval = time.Duration(p.IntervalMs) * time.Millisecond
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.streamSeq++
	id := fmt.Sprintf("sub-%d", a.streamSeq)
	a.streams[id] = cancel
	a.mu.Unlock()

	go a.pumpWait(ctx, id, p, interval)
	return rpc.StreamStart{SubscriptionID: id}, nil
}

func (a *Actions) pumpWait(ctx context.Context, id string, p rpc.PortsWaitParams, interval time.Duration) {
	defer a.endStream(id)

	pending := map[int]bool{}
	for _, port := range p.Ports {
		pending[port] = true
	}
	ready := []int{}
	deadline := time.NewTimer(time.Duration(p.TimeoutMs) * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		for _, port := range p.Ports {
			if !pending[port] || !a.listening(port) {
				continue
			}
			delete(pending, port)
			ready = append(ready, port)
			raw, _ := json.Marshal(rpc.PortsWaitChunk{Port: port, ReadyAt: FixtureTime})
			a.f.broadcast(rpc.MethodStreamChunk, rpc.StreamChunk{ID: id, Data: raw})
		}
		if len(pending) == 0 || (p.Any && len(ready) > 0) {
			a.finishStream(id, rpc.PortsWaitEnd{Ready: ready, TimedOut: []int{}})
			return
		}
		select {
		case <-ctx.Done():
			a.finishStream(id, rpc.PortsWaitEnd{Ready: ready, TimedOut: stillPending(p.Ports, pending)})
			return
		case <-deadline.C:
			a.finishStream(id, rpc.PortsWaitEnd{Ready: ready, TimedOut: stillPending(p.Ports, pending)})
			return
		case <-tick.C:
		}
	}
}

func (a *Actions) streamCancel(raw json.RawMessage) (any, error) {
	var p rpc.StreamCancel
	if err := unmarshal(raw, &p); err != nil {
		return nil, err
	}
	a.mu.Lock()
	cancel, ok := a.streams[p.ID]
	a.mu.Unlock()
	if !ok {
		return nil, rpc.NewError(rpc.CodeNotFound, "unknown stream "+p.ID, "")
	}
	cancel()
	return rpc.OKResult{OK: true}, nil
}

func (a *Actions) finishStream(id string, end any) {
	raw, _ := json.Marshal(end)
	a.f.broadcast(rpc.MethodStreamEnd, rpc.StreamEnd{ID: id, Data: raw})
}

func (a *Actions) endStream(id string) {
	a.mu.Lock()
	if cancel, ok := a.streams[id]; ok {
		cancel()
		delete(a.streams, id)
	}
	a.mu.Unlock()
}

// broadcast pushes a notification to every open connection. Streams are
// per-connection in the daemon; the fake serves one client per test, so
// addressing them all is the same thing and keeps handlers connection-free.
func (f *Fake) broadcast(method string, params any) {
	f.mu.Lock()
	conns := make([]*conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		c.notify(method, params)
	}
}

// ---------------------------------------------------------------- helpers ---

func (a *Actions) listening(port int) bool {
	for _, row := range a.f.Fixture().Ports {
		if row.Port == port {
			return true
		}
	}
	return false
}

func (a *Actions) isDenied(pid int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.denied[pid]
}

// match resolves a kill selector against the fixture. Unlike ports.inspect a
// kill is not ambiguous over several bind addresses: it kills them all.
func (a *Actions) match(sel rpc.Selector) []state.Port {
	var out []state.Port
	for _, row := range a.f.Fixture().Ports {
		switch {
		case sel.Port != nil:
			if row.Port != *sel.Port {
				continue
			}
			if sel.BindAddress != nil && *sel.BindAddress != "" && row.BindAddress != *sel.BindAddress {
				continue
			}
		case sel.PID != nil:
			if row.PID != *sel.PID {
				continue
			}
		case sel.RunID != nil:
			if row.Run == nil || row.Run.ID != *sel.RunID {
				continue
			}
		default:
			continue
		}
		out = append(out, row)
	}
	return out
}

// missingRow is what the killer reports for a target nothing answers on: a
// failed row, not a failed call, so one bad target does not lose the others.
func missingRow(sel rpc.Selector) state.KillResult {
	res := state.KillResult{Method: state.MethodNone}
	switch {
	case sel.Port != nil:
		res.Port = *sel.Port
		res.Error = fmt.Sprintf("no process found listening on port %d", *sel.Port)
	case sel.PID != nil:
		res.PID = *sel.PID
		res.Error = fmt.Sprintf("no process found with pid %d", *sel.PID)
	case sel.RunID != nil:
		res.Error = "no live run " + *sel.RunID
	}
	return res
}

func checkSelector(sel rpc.Selector) error {
	set := 0
	for _, on := range []bool{sel.Port != nil, sel.PID != nil, sel.RunID != nil, sel.ProxyID != nil} {
		if on {
			set++
		}
	}
	if set != 1 {
		return rpc.NewError(rpc.CodeInvalidSelector,
			"a selector sets exactly one of port, pid, run_id and proxy_id",
			`bind_address only disambiguates port, for example {"port": 3000, "bind_address": "127.0.0.1"}`)
	}
	return nil
}

// inGroupFold is `sonar kill -g`'s matching: the resolved group, a run's group,
// id or name, or a compose project, case-insensitively (contract §17).
func inGroupFold(p state.Port, name string) bool {
	candidates := []string{}
	if p.Group != nil {
		candidates = append(candidates, *p.Group)
	}
	if p.Run != nil {
		candidates = append(candidates, p.Run.Group, p.Run.ID, p.Run.Name)
	}
	if p.Docker != nil {
		candidates = append(candidates, p.Docker.ComposeProject)
	}
	for _, c := range candidates {
		if c != "" && strings.EqualFold(c, name) {
			return true
		}
	}
	return false
}

func stillPending(targets []int, pending map[int]bool) []int {
	out := []int{}
	for _, port := range targets {
		if pending[port] {
			out = append(out, port)
		}
	}
	return out
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
