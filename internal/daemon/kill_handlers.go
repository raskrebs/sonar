package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/killer"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/state"
)

// The kill namespace. Both methods are thin: they turn wire selectors into
// killer targets against the daemon's latest snapshot and hand the work to
// killer.KillPorts, which is the same code path `sonar kill` takes, so the CLI,
// the app and MCP cannot drift apart (contract §3, §17).
func init() {
	RegisterHandler("ports.kill", handlePortsKill)
	RegisterHandler("groups.kill", handleGroupsKill)
}

func handlePortsKill(ctx context.Context, req *Request) (any, error) {
	var p rpc.PortsKillParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	if len(p.Targets) == 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "targets is required",
			`send {"targets": [{"port": 3000}]}`)
	}

	snap, err := killSnapshot(req)
	if err != nil {
		return nil, err
	}

	targets := make([]killer.Target, 0, len(p.Targets))
	for _, sel := range p.Targets {
		t, err := selectorTarget(sel, snap)
		if err != nil {
			return nil, killRPCError(err)
		}
		targets = append(targets, t)
	}

	opts := killer.Options{
		Tree:     p.Tree,
		Force:    p.Force,
		Grace:    time.Duration(p.GraceMs) * time.Millisecond,
		Escalate: p.Escalate,
		DryRun:   p.DryRun,
		Ports:    killerRows(snap),
	}
	rows := killer.KillPorts(ctx, targets, opts)
	afterKill(req, opts.DryRun)
	return killEnvelope(rows), nil
}

// afterKill rescans and publishes, so the ports that just went away are gone
// from the very next read and their port_down rows reach the history ring even
// when nobody is subscribed. Without it the caller's own `sonar list` would
// still be served from the snapshot taken before the kill (step 1A.7).
func afterKill(req *Request, dryRun bool) {
	if dryRun {
		return
	}
	rescanAfterKill(req.Runtime)
}

// killSnapshot is the state selectors and group members are resolved against.
//
// It always scans: the cache may be up to CacheTTL old, and a group that gained
// a service in that window would be killed only in part — which is exactly what
// the direct `sonar kill` path, scanning at the moment it is asked, never does.
// A kill is rare and deliberate; one scan is the right price for acting on what
// is listening now.
//
// The scan it asks for is bare. A kill needs the pid and port of what is
// listening, never a health verdict or a cpu reading, so it does not inherit a
// subscriber's `include` — the published snapshot carries those forward
// instead of collecting them again (contract §44). That is the difference
// between a dry run answering in a moment and one waiting out `docker stats`
// and a round of health probes.
func killSnapshot(req *Request) (state.Snapshot, error) {
	snap, err := req.Runtime.Scanner.Rescan(scanner.Include{})
	if err != nil {
		return state.Snapshot{}, rpc.NewError(rpc.CodeInternal, "scan failed: "+err.Error(),
			"check `sonar daemon log` for the scanner error")
	}
	return snap, nil
}

// killerRows hands the killer the scan the handler just took.
//
// Without it killer.KillPorts scans the machine again from inside the RPC, so
// one `ports.kill` cost two full scans: the §25 rescan-before-select and the
// killer's own. The second one is pure duplication — the rows are the same
// enriched pipeline (ports.Scan + Docker + Enrich) the daemon's scanner ran a
// moment earlier — and it is what made a dry run of an unknown port take
// seconds. Every other caller of the killer (`sonar kill`, `kill-all`, `down`)
// already passes the snapshot it resolved its targets against; the daemon now
// does the same, which is also what keeps the two paths' output identical.
func killerRows(snap state.Snapshot) []ports.ListeningPort {
	rows := state.ToListeningAll(snap.Ports)
	if rows == nil {
		// Never nil: a nil slice tells the killer to go and scan for itself.
		rows = []ports.ListeningPort{}
	}
	return rows
}

// selectorTarget validates one wire selector and turns it into a killer target.
// A port that the snapshot binds to exactly one address is pinned to it, so a
// second listener appearing between the scan and the kill cannot widen the
// request; anything the snapshot does not know about is passed through and
// comes back as a failed row rather than a failed call.
func selectorTarget(sel rpc.Selector, snap state.Snapshot) (killer.Target, error) {
	set := 0
	for _, on := range []bool{sel.Port != nil, sel.PID != nil, sel.RunID != nil, sel.ProxyID != nil} {
		if on {
			set++
		}
	}
	if set != 1 {
		return killer.Target{}, &killer.CodedError{
			Code:   killer.CodeInvalidSelector,
			Detail: "a selector sets exactly one of port, pid, run_id and proxy_id",
			Hint:   `bind_address only disambiguates port, for example {"port": 3000, "bind_address": "127.0.0.1"}`,
		}
	}

	switch {
	case sel.ProxyID != nil:
		return killer.Target{ProxyID: *sel.ProxyID}, nil
	case sel.RunID != nil:
		return killer.Target{RunID: *sel.RunID}, nil
	case sel.PID != nil:
		if *sel.PID <= 0 {
			return killer.Target{}, &killer.CodedError{
				Code:   killer.CodeInvalidSelector,
				Detail: "pid must be positive",
			}
		}
		return killer.Target{PID: *sel.PID}, nil
	}

	port := *sel.Port
	if port <= 0 || port > 65535 {
		return killer.Target{}, &killer.CodedError{
			Code:   killer.CodeInvalidSelector,
			Detail: "port must be between 1 and 65535",
		}
	}
	bind := ""
	if sel.BindAddress != nil {
		bind = *sel.BindAddress
	}
	if bind == "" {
		if binds := bindAddresses(snap, port); len(binds) == 1 {
			bind = binds[0]
		}
	}
	return killer.Target{Port: port, BindAddress: bind}, nil
}

// bindAddresses lists the distinct addresses a port is bound to in the snapshot.
func bindAddresses(snap state.Snapshot, port int) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range snap.Ports {
		if p.Port != port || seen[p.BindAddress] {
			continue
		}
		seen[p.BindAddress] = true
		out = append(out, p.BindAddress)
	}
	return out
}

// groupTargets resolves a group name against the snapshot. The resolved group
// is the primary signal; a run's group or id and a Docker Compose project are
// accepted too, matching what `sonar kill -g` accepts (contract §17).
func groupTargets(snap state.Snapshot, name string) []killer.Target {
	var out []killer.Target
	for _, p := range snap.Ports {
		if inSnapshotGroup(p, name) {
			out = append(out, killer.Target{Port: p.Port, BindAddress: p.BindAddress})
		}
	}
	return out
}

func inSnapshotGroup(p state.Port, name string) bool {
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

// killEnvelope wraps the killer's rows in the contract §3 mutation result.
// affected lists the port key of every row that succeeded, once each; ok is
// true only when nothing failed.
func killEnvelope(rows []state.KillResult) rpc.KillEnvelope {
	if rows == nil {
		rows = []state.KillResult{}
	}
	env := rpc.KillEnvelope{Results: rows}
	env.OK = true
	env.Affected = []string{}
	seen := map[string]bool{}
	for _, r := range rows {
		if !r.OK {
			env.OK = false
			continue
		}
		key := r.Key()
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		env.Affected = append(env.Affected, key)
	}
	return env
}

// killRPCError maps a killer error onto the contract's numeric error registry,
// carrying its hint through to error.data.hint (contract §2).
func killRPCError(err error) error {
	code := rpc.CodeInternal
	switch killer.Code(err) {
	case killer.CodeNotFound:
		code = rpc.CodeNotFound
	case killer.CodeAmbiguous:
		code = rpc.CodeAmbiguous
	case killer.CodePermissionDenied:
		code = rpc.CodePermission
	case killer.CodeInvalidSelector:
		code = rpc.CodeInvalidSelector
	}
	return rpc.NewError(code, err.Error(), killer.Hint(err))
}
