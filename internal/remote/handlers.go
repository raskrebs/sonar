package remote

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/raskrebs/sonar/internal/config"
	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// Sentinel errors the handlers turn into protocol errors.
var (
	ErrDuplicateHost = errors.New("a remote host with that name is already registered")
	ErrUnknownHost   = errors.New("no remote host with that name")
)

// The manager is process-global because the daemon is: one daemon, one set of
// bridges. It is installed by the OnStart hook and torn down by OnShutdown.
var (
	managerMu sync.RWMutex
	manager   *Manager
)

// Manager returns the running connection manager, or nil before the daemon has
// started one.
func Current() *Manager {
	managerMu.RLock()
	defer managerMu.RUnlock()
	return manager
}

func setManager(m *Manager) {
	managerMu.Lock()
	manager = m
	managerMu.Unlock()
}

func init() {
	daemon.RegisterHandler("remote.list", handleList)
	daemon.RegisterHandler("remote.add", handleAdd)
	daemon.RegisterHandler("remote.remove", handleRemove)
	daemon.RegisterHandler("remote.call", handleCall)
	daemon.RegisterCapability("remote")

	daemon.OnStart(start)
	daemon.OnShutdown(func(bool) {
		daemon.SetRouter(nil)
		if m := Current(); m != nil {
			m.Stop()
			setManager(nil)
		}
	})
}

// start builds the manager from the user config and wires it into the scan
// loop. A remote host's state change republishes without a local scan, and a
// remote event is broadcast to whoever asked to see that host.
func start(rt *daemon.Runtime) {
	hosts, warnings := config.RemoteHosts()
	for _, w := range warnings {
		rt.Logger.Warn(w)
	}

	m := NewManager(Options{
		Version:  rt.Version,
		Logger:   rt.Logger,
		OnChange: rt.Scanner.RemoteChanged,
		OnEvent:  rt.Server().BroadcastEvent,
	})
	setManager(m)
	daemon.SetRouter(m)
	rt.Scanner.SetRemote(m.Rows)
	m.Start(context.Background(), hosts)
	if len(hosts) > 0 {
		rt.Logger.Info("connecting to remote hosts", "hosts", len(hosts))
	}
}

// saveHosts is the manager's default persistence: `remote.hosts` in the user
// config.
func saveHosts(hosts []Host) error { return config.SaveRemoteHosts(hosts) }

// requireManager is the "the daemon is not running remote support" guard. It
// cannot normally fail — the hook installs the manager before the first
// connection is accepted — but a handler must never dereference nil.
func requireManager() (*Manager, error) {
	m := Current()
	if m == nil {
		return nil, rpc.NewError(rpc.CodeInternal, "the remote host manager is not running", "")
	}
	return m, nil
}

func handleList(_ context.Context, _ *daemon.Request) (any, error) {
	m, err := requireManager()
	if err != nil {
		return nil, err
	}
	return rpc.RemoteListResult{Hosts: m.Hosts()}, nil
}

func handleAdd(_ context.Context, req *daemon.Request) (any, error) {
	var p rpc.RemoteAddParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	m, err := requireManager()
	if err != nil {
		return nil, err
	}

	h, err := NormalizeHost(Host{
		Name:      p.Name,
		Target:    TargetOf(p),
		SSHArgs:   p.SSHArgs,
		Identity:  p.Identity,
		Port:      p.Port,
		RemoteBin: p.RemoteBin,
	})
	if err != nil {
		return nil, err
	}
	if err := m.Add(h); err != nil {
		if errors.Is(err, ErrDuplicateHost) {
			return nil, rpc.NewError(rpc.CodeInvalidParams,
				"remote host "+h.Name+" is already registered",
				"pick another --name, or `sonar remote remove "+h.Name+"` first")
		}
		return nil, rpc.NewError(rpc.CodeInternal, err.Error(), "")
	}
	return rpc.RemoteAddResult{OK: true, Host: hostRowFor(m, h)}, nil
}

func handleRemove(_ context.Context, req *daemon.Request) (any, error) {
	var p rpc.RemoteRemoveParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	m, err := requireManager()
	if err != nil {
		return nil, err
	}
	if err := m.Remove(strings.TrimSpace(p.Name)); err != nil {
		if errors.Is(err, ErrUnknownHost) {
			return nil, unknownHostError(m, p.Name)
		}
		return nil, rpc.NewError(rpc.CodeInternal, err.Error(), "")
	}
	return rpc.OKResult{OK: true}, nil
}

// handleCall forwards a method to a host's daemon. Writes are forwarded like
// any other method: the remote daemon owns its own `sonar.yaml` under the
// same rules as the local one, so there is no read-only mode (remote-hosts
// spec, decision 3).
//
// It is the generic form of the `host` field every method now takes, and takes
// exactly the same path (contract §43): a streaming method streams, and the
// rows that come back say which host they are from rather than calling
// themselves "localhost". The params are still passed through untouched — a
// caller that spells the method out is spelling its params out too.
//
// `remote.call` is deliberately not a recursion guard's business: the far side
// subscribes to nothing but its own machine, so forwarding `remote.call` to a
// host that has hosts of its own reaches exactly one level, which is what a
// caller asked for.
func handleCall(ctx context.Context, req *daemon.Request) (any, error) {
	var p rpc.RemoteCallParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Host) == "" || strings.TrimSpace(p.Method) == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "host and method are required",
			`{"host": "hetzner", "method": "ports.list", "params": {}}`)
	}
	m, err := requireManager()
	if err != nil {
		return nil, err
	}
	if state.IsLocalhost(p.Host) {
		return nil, rpc.NewError(rpc.CodeInvalidParams,
			"remote.call cannot target localhost",
			"call "+p.Method+" directly instead")
	}

	if !m.Has(p.Host) {
		return nil, unknownHostError(m, p.Host)
	}

	params := p.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	// The typed path and the generic one are the same path: `remote.call
	// {host, method}` and `<method> {host}` both end in daemon.ForwardTo, so a
	// streaming method streams either way and a result is retagged either way.
	out, err := daemon.ForwardTo(ctx, req, p.Host, p.Method, params)
	if err != nil {
		if errors.Is(err, ErrUnknownHost) {
			return nil, unknownHostError(m, p.Host)
		}
		return nil, err
	}
	return out, nil
}

// TargetOf is the SSH destination a remote.add call names. Some clients spell
// the field `host` rather than `target`; both mean the string ssh receives,
// and `target` wins when a client sends both.
func TargetOf(p rpc.RemoteAddParams) string {
	if t := strings.TrimSpace(p.Target); t != "" {
		return t
	}
	return strings.TrimSpace(p.Host)
}

// unknownHostError names the hosts that do exist, which is what a user who
// mistyped one wants to see.
func unknownHostError(m *Manager, name string) error {
	known := make([]string, 0, 4)
	for _, h := range m.Configs() {
		known = append(known, h.Name)
	}
	hint := "`sonar remote add <user@host>` registers one"
	if len(known) > 0 {
		hint = "registered hosts: " + strings.Join(known, ", ")
	}
	return rpc.NewError(rpc.CodeNotFound, "no remote host named "+name, hint)
}

// hostRowFor is the Host row for a just-registered host: whatever the bridge
// already knows, which right after Add is "connecting".
func hostRowFor(m *Manager, h Host) state.Host {
	for _, row := range m.Hosts() {
		if row.Name == h.Name {
			return row
		}
	}
	return state.Host{Name: h.Name, Address: h.Target, Status: state.HostConnecting}
}

// NormalizeHost fills in the name from the target when none was given and
// checks what the spec fixes: names are [a-z0-9-]+, unique, and never resolved
// as DNS by sonar — the target is what `ssh` receives.
func NormalizeHost(h Host) (Host, error) {
	h.Target = strings.TrimSpace(h.Target)
	h.Name = strings.TrimSpace(h.Name)
	if h.Target == "" {
		return h, rpc.NewError(rpc.CodeInvalidParams, "target is required",
			"the target is what ssh receives: `deploy@203.0.113.7`, or a ~/.ssh/config alias")
	}
	if h.Name == "" {
		h.Name = config.DefaultHostName(h.Target)
	}
	if !config.ValidHostName(h.Name) {
		return h, rpc.NewError(rpc.CodeInvalidParams,
			"invalid host name "+h.Name,
			"names are lowercase letters, digits and dashes")
	}
	if state.IsLocalhost(h.Name) {
		return h, rpc.NewError(rpc.CodeInvalidParams,
			`"localhost" names this machine and cannot be registered`,
			"pick another --name")
	}
	if h.Port < 0 || h.Port > 65535 {
		return h, rpc.NewError(rpc.CodeInvalidParams, "invalid ssh port", "")
	}
	return h, nil
}
