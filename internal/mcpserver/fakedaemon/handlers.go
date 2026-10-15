package fakedaemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// registerCore installs the methods every consumer of the fake needs: the
// handshake, the state collections and the read half of the ports namespace.
// Everything else a slice needs it registers itself with Handle.
func (f *Fake) registerCore() {
	f.Handle("daemon.hello", f.handleHello)
	f.Handle("daemon.status", f.handleStatus)
	f.Handle("state.snapshot", func(json.RawMessage) (any, error) { return f.snapshot(), nil })
	f.Handle("ports.list", f.handlePortsList)
	f.Handle("ports.inspect", f.handlePortsInspect)
	f.Handle("groups.list", func(json.RawMessage) (any, error) {
		return rpc.GroupsListResult{Groups: f.Fixture().Groups}, nil
	})
	f.registerQuery()
	f.registerResourceMethods()
}

func (f *Fake) handleHello(raw json.RawMessage) (any, error) {
	var p rpc.DaemonHelloParams
	if err := unmarshal(raw, &p); err != nil {
		return nil, err
	}
	fx := f.Fixture()
	return rpc.DaemonHelloResult{
		ProtocolVersion: rpc.ProtocolVersion,
		DaemonVersion:   fx.DaemonVersion,
		PID:             os.Getpid(),
		StartedAt:       fx.StartedAt,
		Capabilities:    fx.Capabilities,
		Socket:          f.Addr(),
		BinaryPath:      "fakedaemon",
		Keepalive:       p.Keepalive,
	}, nil
}

func (f *Fake) handleStatus(json.RawMessage) (any, error) {
	return rpc.DaemonStatusResult{
		PID:                os.Getpid(),
		Uptime:             "1m0s",
		Subscribers:        f.Subscribers(),
		LastScanAt:         f.Fixture().StartedAt,
		ScanIntervalMs:     2000,
		ScanBaseIntervalMs: 2000,
		StatsIntervalMs:    1000,
		ClaimTTLMs:         86400000,
		Scans:              1,
	}, nil
}

// snapshot is the state.snapshot / state.subscribe reply. Every collection is
// an array, never null, as a real snapshot is.
func (f *Fake) snapshot() state.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return state.Snapshot{
		Seq:           f.seq,
		At:            f.fixture.StartedAt,
		DaemonVersion: f.fixture.DaemonVersion,
		Ports:         orEmpty(f.fixture.Ports),
		Groups:        orEmpty(f.fixture.Groups),
		Shares:        []state.Share{},
		Tunnels:       []state.Share{},
		Proxies:       []state.Proxy{},
		Sessions:      orEmpty(f.fixture.Sessions),
		Hosts:         orEmpty(f.fixture.Hosts),
	}
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// handlePortsList mirrors the daemon's own filter order: desktop apps out
// unless `all`, then type, then ip_version, then group (which matches a group
// name, a `sonar start` name or a run id).
func (f *Fake) handlePortsList(raw json.RawMessage) (any, error) {
	var p rpc.PortsListParams
	if err := unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Filter != nil {
		switch *p.Filter {
		case "", "docker", "user", "system", "proxy":
		default:
			return nil, rpc.NewError(rpc.CodeInvalidParams,
				"unknown filter "+*p.Filter, `filter accepts "docker", "user", "system" and "proxy"`)
		}
	}
	ipVersion, err := normalizeIPVersion(p.IPVersion)
	if err != nil {
		return nil, err
	}
	include := includeSet(p.Include)

	out := []state.Port{}
	for _, row := range f.Fixture().Ports {
		if !p.All && row.IsApp {
			continue
		}
		if p.Filter != nil && *p.Filter != "" && string(row.Type) != *p.Filter {
			continue
		}
		if ipVersion != "" && row.IPVersion != ipVersion {
			continue
		}
		if p.Group != nil && *p.Group != "" && !inGroup(row, *p.Group) {
			continue
		}
		if p.Session != nil && *p.Session != "" && !inSession(row, *p.Session) {
			continue
		}
		out = append(out, filterPort(row, include))
	}
	return rpc.PortsListResult{Ports: out}, nil
}

func (f *Fake) handlePortsInspect(raw json.RawMessage) (any, error) {
	var sel rpc.Selector
	if err := unmarshal(raw, &sel); err != nil {
		return nil, err
	}
	row, err := resolvePort(f.Fixture().Ports, sel)
	if err != nil {
		return nil, err
	}
	if row.Health == nil {
		row.Health = &state.Health{Status: state.HealthUnknown}
	}
	return rpc.PortsInspectResult{
		Port:        row,
		LogSources:  []string{},
		Connections: []rpc.Connection{},
	}, nil
}

// resolvePort is the daemon's selector resolution, error for error: pid picks
// the first match, a bare port that several addresses answer on is ambiguous.
func resolvePort(rows []state.Port, sel rpc.Selector) (state.Port, error) {
	var matches []state.Port
	switch {
	case sel.PID != nil:
		for _, row := range rows {
			if row.PID == *sel.PID {
				matches = append(matches, row)
			}
		}
		if len(matches) == 0 {
			return state.Port{}, rpc.NewError(rpc.CodeNotFound,
				fmt.Sprintf("no listening port found for pid %d", *sel.PID),
				"run `sonar list` to see what is listening")
		}
		return matches[0], nil
	case sel.Port != nil:
		for _, row := range rows {
			if row.Port != *sel.Port {
				continue
			}
			if sel.BindAddress != nil && *sel.BindAddress != "" && row.BindAddress != *sel.BindAddress {
				continue
			}
			matches = append(matches, row)
		}
	default:
		return state.Port{}, rpc.NewError(rpc.CodeInvalidParams,
			"a selector needs a port or a pid", `send {"port": 3000} or {"pid": 1234}`)
	}

	switch len(matches) {
	case 0:
		return state.Port{}, rpc.NewError(rpc.CodeNotFound,
			fmt.Sprintf("no process found listening on port %d", *sel.Port),
			"run `sonar list` to see what is listening")
	case 1:
		return matches[0], nil
	default:
		addrs := make([]string, 0, len(matches))
		for _, m := range matches {
			addrs = append(addrs, m.BindAddress)
		}
		return state.Port{}, rpc.NewError(rpc.CodeAmbiguous,
			fmt.Sprintf("port %d is bound to multiple addresses: %s", *sel.Port, strings.Join(addrs, ", ")),
			fmt.Sprintf("pass a bind address (e.g. --ip %s)", addrs[0]))
	}
}

// inSession is the daemon's session filter: the id, or a unique prefix of it
// (contract §29), never the label.
func inSession(p state.Port, id string) bool {
	if p.Session == nil {
		return false
	}
	return strings.HasPrefix(p.Session.ID, id)
}

func inGroup(p state.Port, group string) bool {
	if p.Group != nil && *p.Group == group {
		return true
	}
	if p.Run != nil && (p.Run.Name == group || p.Run.ID == group || p.Run.Group == group) {
		return true
	}
	return false
}

func normalizeIPVersion(v *string) (string, error) {
	if v == nil {
		return "", nil
	}
	switch strings.ToLower(strings.TrimSpace(*v)) {
	case "":
		return "", nil
	case "4", "ipv4":
		return "IPv4", nil
	case "6", "ipv6":
		return "IPv6", nil
	default:
		return "", rpc.NewError(rpc.CodeInvalidParams,
			"unknown ip_version "+*v, `ip_version accepts "IPv4" and "IPv6"`)
	}
}

type include struct{ stats, health bool }

func includeSet(inc rpc.Include) include {
	var out include
	for _, v := range inc {
		switch v {
		case "stats":
			out.stats = true
		case "health":
			out.health = true
		}
	}
	return out
}

// filterPort applies the per-subscriber include rule (contract §15): a caller
// that did not ask for stats or health gets null there, even though the
// fixture carries them.
func filterPort(p state.Port, inc include) state.Port {
	if !inc.stats {
		p.Stats = nil
	}
	if !inc.health {
		p.Health = nil
	}
	return p
}

func unmarshal(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return rpc.NewError(rpc.CodeInvalidParams, "decoding params: "+err.Error(), "")
	}
	return nil
}
