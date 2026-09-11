package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/config"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/raskrebs/sonar/internal/store"
)

// The write paths that live in the database, plus the settings file behind
// them. The read side of ports.* belongs to step 1A.6 and registers itself.
func init() {
	RegisterHandler("ports.rename", handleRename)
	RegisterHandler("groups.assign", handleAssign)
	RegisterHandler("ports.history", handleHistory)

	RegisterHandler("config.get", handleConfigGet)
	RegisterHandler("config.set", handleConfigSet)
	RegisterHandler("config.path", handleConfigPath)
}

// errNoStore is what every database-backed method reports when the daemon came
// up without one.
func errNoStore() error {
	return rpc.NewError(rpc.CodeInternal,
		"this daemon has no database open",
		"check `sonar daemon log` for the error opening sonar.db, then `sonar daemon restart`")
}

func handleRename(_ context.Context, req *Request) (any, error) {
	var p rpc.PortsRenameParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	st := req.Runtime.Store
	if st == nil {
		return nil, errNoStore()
	}
	target, err := resolveSelector(req.Runtime, p.Selector)
	if err != nil {
		return nil, err
	}
	key := store.PrimaryKey(target)

	if p.Name == nil {
		if err := st.ClearRename(key); err != nil {
			return nil, storeError("clearing the rename", err)
		}
	} else if err := st.SetRename(key, strings.TrimSpace(*p.Name)); err != nil {
		return nil, storeError("saving the rename", err)
	}

	republish(req.Runtime)
	return rpc.PortsRenameResult{
		MutationResult: rpc.MutationResult{OK: true, Affected: []string{target.Key()}},
		Key:            key,
		Name:           p.Name,
	}, nil
}

func handleAssign(_ context.Context, req *Request) (any, error) {
	var p rpc.GroupsAssignParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	st := req.Runtime.Store
	if st == nil {
		return nil, errNoStore()
	}
	target, err := resolveSelector(req.Runtime, p.Selector)
	if err != nil {
		return nil, err
	}
	key := store.PrimaryKey(target)

	if p.Group == nil {
		if err := st.ClearPin(key); err != nil {
			return nil, storeError("clearing the group pin", err)
		}
	} else if err := st.SetPin(key, strings.TrimSpace(*p.Group)); err != nil {
		return nil, storeError("saving the group pin", err)
	}

	republish(req.Runtime)
	return rpc.GroupsAssignResult{
		MutationResult: rpc.MutationResult{OK: true, Affected: []string{target.Key()}},
		Key:            key,
		Group:          p.Group,
	}, nil
}

func handleHistory(_ context.Context, req *Request) (any, error) {
	var p rpc.PortsHistoryParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	st := req.Runtime.Store
	if st == nil {
		return nil, errNoStore()
	}
	since, err := parseSince(p.Since)
	if err != nil {
		return nil, err
	}
	rows, err := st.Query(p.Port, since, p.Limit)
	if err != nil {
		return nil, storeError("reading the history", err)
	}
	events := make([]rpc.HistoryEvent, 0, len(rows))
	for _, r := range rows {
		events = append(events, rpc.HistoryEvent{
			At:          r.At.Format(time.RFC3339),
			Kind:        r.Kind,
			Port:        r.Port,
			PID:         r.PID,
			DisplayName: r.DisplayName,
			Group:       r.Group,
		})
	}
	return rpc.PortsHistoryResult{Events: events}, nil
}

func handleConfigGet(_ context.Context, _ *Request) (any, error) {
	cfg, err := config.Map()
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternal, err.Error(),
			"fix or remove "+config.Path())
	}
	return rpc.ConfigGetResult{Config: cfg}, nil
}

func handleConfigSet(_ context.Context, req *Request) (any, error) {
	var p rpc.ConfigSetParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	if len(p.Patch) == 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "patch is required",
			`send {"patch": {"list": {"sort": "port"}}}; a null value clears a key`)
	}
	cfg, err := config.Apply(p.Patch)
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInvalidParams, err.Error(),
			"the config was left as it was; `sonar config path` shows the file")
	}
	req.Runtime.Logger.Info("config written", "path", config.Path())
	return rpc.ConfigSetResult{OK: true, Config: cfg}, nil
}

func handleConfigPath(_ context.Context, _ *Request) (any, error) {
	return rpc.ConfigPathResult{Path: config.Path()}, nil
}

// republish makes a write visible before its own reply is. It re-attributes
// the last scan and publishes synchronously, so by the time the handler
// returns the delta carrying the change is already queued on every
// subscriber's connection — ahead of this call's response, which the same
// writer queues afterwards — and the caller's own next read is served from a
// snapshot that has it.
//
// Contract §18 only asks that a rename or an assign take effect "in the next
// publish", with an immediate rescan so the caller sees it. Replying *after*
// that publish is what turns "the next delta carries the rename" from a race
// into a guarantee: a subscriber cannot receive the acknowledgement before the
// delta that justifies it, and a client that reads straight after the reply
// cannot be served the state from before its own write.
//
// It does not scan the machine. A rename, a group pin and a `sonar.yaml` edit
// change how the ports the daemon already knows are named and grouped, not
// which ports exist, so re-running attribution over the last scan's own rows
// answers the question — and it does so in microseconds instead of behind
// `lsof`, `ps` and `docker stats` (contract §44). `Republish` wakes the loop,
// so a real scan follows.
func republish(rt *Runtime) {
	if err := rt.Scanner.Republish(); err != nil {
		rt.Logger.Warn("republishing after a write", "error", err)
	}
}

// rescanAfterKill is republish's sibling for the one write that does change
// which ports exist. A kill has to be followed by a real scan: the ports that
// just went away are only gone from the snapshot once the OS has been asked
// again, and their port_down rows reach the history ring from that delta.
func rescanAfterKill(rt *Runtime) {
	if _, err := rt.Scanner.Rescan(scanner.Include{}); err != nil {
		rt.Logger.Warn("rescanning after a kill", "error", err)
	}
}

// storeError wraps a database failure as an internal error with the path.
func storeError(what string, err error) error {
	if errors.Is(err, store.ErrInvalidName) {
		return rpc.NewError(rpc.CodeInvalidParams, err.Error(),
			"names are a single word: no whitespace, no / and no \\")
	}
	return rpc.NewError(rpc.CodeInternal, what+": "+err.Error(),
		"check `sonar daemon log`")
}

// parseSince accepts either an RFC 3339 instant (the wire form in the spec's
// method table) or a Go duration like "24h", which is what `sonar history
// --since` takes and what a human types.
func parseSince(since *string) (time.Time, error) {
	if since == nil || strings.TrimSpace(*since) == "" {
		return time.Time{}, nil
	}
	raw := strings.TrimSpace(*since)
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, rpc.NewError(rpc.CodeInvalidParams,
		"since must be an RFC 3339 timestamp or a duration like 24h, got "+raw, "")
}

// resolveSelector finds the port a write applies to in the current scan.
// Contract §3: exactly one of port and pid, with bind_address only there to
// disambiguate a port bound on several addresses.
func resolveSelector(rt *Runtime, sel rpc.Selector) (state.Port, error) {
	switch {
	case sel.Port == nil && sel.PID == nil:
		return state.Port{}, rpc.NewError(rpc.CodeInvalidSelector,
			"a port or a pid is required", `send {"port": 3000} or {"pid": 1234}`)
	case sel.Port != nil && sel.PID != nil:
		return state.Port{}, rpc.NewError(rpc.CodeInvalidSelector,
			"port and pid are mutually exclusive", "send one of them, not both")
	}

	snap, err := rt.Scanner.Snapshot(scanner.Include{})
	if err != nil {
		return state.Port{}, rpc.NewError(rpc.CodeInternal, "scan failed: "+err.Error(),
			"check `sonar daemon log` for the scanner error")
	}

	var matches []state.Port
	for _, p := range snap.Ports {
		switch {
		case sel.PID != nil:
			if p.PID == *sel.PID {
				matches = append(matches, p)
			}
		case p.Port == *sel.Port:
			if sel.BindAddress == nil || *sel.BindAddress == "" || p.BindAddress == *sel.BindAddress {
				matches = append(matches, p)
			}
		}
	}

	switch len(matches) {
	case 0:
		return state.Port{}, notListening(sel)
	case 1:
		return matches[0], nil
	}
	// Several rows for one port on different addresses are the same service to
	// the store: they share a match key, so any of them will do.
	if sameTarget(matches) {
		return matches[0], nil
	}
	return state.Port{}, ambiguous(sel, matches)
}

// sameTarget reports whether every match would be stored under one key, which
// is what makes a multi-bind row unambiguous for a rename or a pin.
func sameTarget(matches []state.Port) bool {
	key := store.PrimaryKey(matches[0])
	for _, p := range matches[1:] {
		if store.PrimaryKey(p) != key {
			return false
		}
	}
	return true
}

func notListening(sel rpc.Selector) error {
	if sel.PID != nil {
		return rpc.NewError(rpc.CodeNotFound,
			fmt.Sprintf("no listening port owned by pid %d", *sel.PID),
			"`sonar list` shows what is listening")
	}
	return rpc.NewError(rpc.CodeNotFound,
		fmt.Sprintf("nothing is listening on port %d", *sel.Port),
		"`sonar list` shows what is listening")
}

func ambiguous(sel rpc.Selector, matches []state.Port) error {
	var detail string
	if sel.PID != nil {
		list := make([]string, 0, len(matches))
		for _, p := range matches {
			list = append(list, strconv.Itoa(p.Port))
		}
		sort.Strings(list)
		detail = fmt.Sprintf("pid %d listens on %s", *sel.PID, strings.Join(list, ", "))
		return rpc.NewError(rpc.CodeAmbiguous, detail, "name the port instead of the pid")
	}
	binds := make([]string, 0, len(matches))
	for _, p := range matches {
		binds = append(binds, p.BindAddress)
	}
	sort.Strings(binds)
	detail = fmt.Sprintf("port %d is bound on %s", *sel.Port, strings.Join(binds, ", "))
	return rpc.NewError(rpc.CodeAmbiguous, detail,
		"add --ip to pick one, for example --ip "+binds[0])
}
