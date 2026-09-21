package share

import (
	"context"
	"sync"

	"github.com/raskrebs/sonar/internal/config"
	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/session"
	"github.com/raskrebs/sonar/internal/state"
)

// The `share.*` methods, and the daemon wiring that installs them.
//
// Same shape as internal/session next door: the manager is process-global
// because the daemon is, the OnStart hook builds it and the OnShutdown hook
// ends every share it was holding. The daemon package never imports this one
// (contract §8).

var (
	managerMu sync.RWMutex
	manager   *Manager
)

// Current returns the running manager, or nil before the daemon has started one.
func Current() *Manager {
	managerMu.RLock()
	defer managerMu.RUnlock()
	return manager
}

// SetManager installs a manager. Exported for the tests that drive the handlers
// against a fake relay without starting a daemon.
func SetManager(m *Manager) {
	managerMu.Lock()
	manager = m
	managerMu.Unlock()
}

func init() {
	daemon.RegisterHandler("share.create", handleCreate)
	daemon.RegisterHandler("share.project", handleProject)
	daemon.RegisterHandler("share.stop", handleStop)
	daemon.RegisterHandler("share.list", handleList)
	daemon.RegisterHandler("share.extend", handleExtend)
	daemon.RegisterHandler("share.logs", handleLogs)
	daemon.RegisterCapability("share")

	daemon.OnStart(start)
	daemon.OnShutdown(func(bool) {
		if m := Current(); m != nil {
			m.StopAll()
		}
		SetManager(nil)
	})
}

func start(rt *daemon.Runtime) {
	cfg, warnings := config.Load()
	for _, w := range warnings {
		rt.Logger.Warn(w)
	}
	tunnelURL := ""
	if cfg.Tunnel() != cfg.Relay() {
		tunnelURL = cfg.Tunnel()
	}
	SetManager(New(Options{Logger: rt.Logger, TunnelURL: tunnelURL}))
	rt.Logger.Debug("sharing ready", "relay", cfg.Relay(), "tunnel", cfg.Tunnel())
}

// sessionAdapter is internal/session seen as this package's `caller`. It exists
// so a test can stand in a fake relay without a credentials store, and so the
// session token has exactly one road out of that package.
type sessionAdapter struct{}

func (sessionAdapter) manager() (*session.Manager, error) {
	m := session.Current()
	if m == nil {
		return nil, rpc.NewError(rpc.CodeInternal, "the relay session manager is not running", "")
	}
	return m, nil
}

func (a sessionAdapter) Call(ctx context.Context, method, path string, payload any) (sessionResponse, error) {
	m, err := a.manager()
	if err != nil {
		return sessionResponse{}, err
	}
	got, err := m.Call(ctx, method, path, payload)
	return sessionResponse{Status: got.Status, Body: got.Body}, err
}

func (a sessionAdapter) Token() (string, error) {
	m, err := a.manager()
	if err != nil {
		return "", err
	}
	return m.Token()
}

func (a sessionAdapter) Relay() string {
	m, err := a.manager()
	if err != nil {
		return ""
	}
	return m.Relay()
}

func requireManager() (*Manager, error) {
	m := Current()
	if m == nil {
		return nil, rpc.NewError(rpc.CodeInternal, "sharing is not running on this daemon", "")
	}
	return m, nil
}

func handleCreate(ctx context.Context, req *daemon.Request) (any, error) {
	var p rpc.ShareCreateParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	m, err := requireManager()
	if err != nil {
		return nil, err
	}
	snap, err := snapshot(req)
	if err != nil {
		return nil, err
	}
	share, notes, err := m.Create(ctx, snap, p)
	if err != nil {
		return nil, err
	}
	return rpc.ShareCreateResult{
		MutationResult: rpc.MutationResult{OK: true, Affected: affected(share)},
		Share:          share,
		Notes:          notes,
	}, nil
}

func handleStop(ctx context.Context, req *daemon.Request) (any, error) {
	var p rpc.ShareStopParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	m, err := requireManager()
	if err != nil {
		return nil, err
	}
	stopped, err := m.Stop(ctx, p)
	if err != nil {
		return nil, err
	}
	return rpc.ShareStopResult{
		MutationResult: rpc.MutationResult{OK: true},
		Stopped:        stopped,
	}, nil
}

func handleList(_ context.Context, _ *daemon.Request) (any, error) {
	m, err := requireManager()
	if err != nil {
		return nil, err
	}
	return rpc.ShareListResult{Shares: m.List()}, nil
}

func handleExtend(ctx context.Context, req *daemon.Request) (any, error) {
	var p rpc.ShareExtendParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	m, err := requireManager()
	if err != nil {
		return nil, err
	}
	share, err := m.Extend(ctx, p)
	if err != nil {
		return nil, err
	}
	return rpc.ShareExtendResult{Share: share}, nil
}

func handleLogs(_ context.Context, req *daemon.Request) (any, error) {
	var p rpc.ShareLogsParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	m, err := requireManager()
	if err != nil {
		return nil, err
	}
	lines, err := m.Logs(p)
	if err != nil {
		return nil, err
	}
	return rpc.ShareLogsResult{Lines: lines}, nil
}

// snapshot is the port table a share is resolved against. It is read fresh
// rather than from the cache: a share names something that is listening right
// now, and a two-second-old table can name a port that has just gone.
func snapshot(req *daemon.Request) (state.Snapshot, error) {
	if req == nil || req.Runtime == nil || req.Runtime.Scanner == nil {
		return state.Snapshot{}, rpc.NewError(rpc.CodeInternal, "the scanner is not running", "")
	}
	snap, err := req.Runtime.Scanner.Snapshot(scanner.Include{})
	if err != nil {
		return state.Snapshot{}, rpc.NewError(rpc.CodeInternal, "scan failed: "+err.Error(),
			"check `sonar daemon log` for the scanner error")
	}
	return snap, nil
}

// affected is the mutation's port key, which is what contract §3 asks a
// mutating method to name.
func affected(share state.Share) []string {
	if share.TargetPort == 0 {
		return []string{}
	}
	return []string{state.Port{Host: state.LocalhostName, Port: share.TargetPort}.Key()}
}

func handleProject(ctx context.Context, req *daemon.Request) (any, error) {
	var p rpc.ShareProjectParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	m, err := requireManager()
	if err != nil {
		return nil, err
	}
	snap, err := snapshot(req)
	if err != nil {
		return nil, err
	}
	return m.Preview(ctx, snap, p)
}
