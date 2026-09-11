// Package groupstart serves `groups.start`: it walks a `sonar.yaml`'s
// services in dependency order and spawns each one detached, streaming a chunk
// per service as it goes (contract §1).
//
// It lives outside internal/daemon because starting a service needs the run
// registry, and the daemon package must not import it (contract §8). Linking
// this package in is what makes `groups.start` exist; internal/cmd does that
// for the `sonar` binary.
package groupstart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/daemon/runsreg"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/spawn"
	"github.com/raskrebs/sonar/internal/state"
)

// DependencyTimeout bounds how long one service waits for something it
// depends_on to start listening. On a timeout the dependent service is
// reported and skipped, and the independent ones still start: a slow database
// must not mean nothing came up.
const DependencyTimeout = 30 * time.Second

// dependencyTimeout is the value actually used, so tests do not have to wait
// out the real one to see a timeout reported.
var dependencyTimeout = DependencyTimeout

// dependencyPoll is how often the wait re-reads the daemon's state. The
// scanner's own cache means a poll costs nothing until it is stale.
const dependencyPoll = 250 * time.Millisecond

func init() {
	daemon.RegisterHandler("groups.start", handleGroupsStart)
	daemon.RegisterCapability("groups")
}

func handleGroupsStart(ctx context.Context, req *daemon.Request) (any, error) {
	var p rpc.GroupsStartParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	cfg, err := resolveConfig(req.Runtime, p)
	if err != nil {
		return nil, err
	}
	// The group the services run under is the one the file's ports are
	// published in: `<project>@<worktree>` for a worktree's copy of a
	// committed file, never the `name:` the copy carries (step 5A.6).
	group := req.Runtime.Scanner.GroupOf(cfg)
	plan, err := groups.Plan(cfg, p.Only)
	if err != nil {
		var unknown *groups.UnknownServiceError
		if errors.As(err, &unknown) {
			return nil, rpc.NewError(rpc.CodeNotFound, unknown.Error(),
				"`sonar groups "+group+"` lists the services this file declares")
		}
		return nil, rpc.NewError(rpc.CodeInternal, err.Error(), "")
	}
	if len(plan) == 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams,
			cfg.Path+" declares no services to start",
			"add a `services:` list, or start the command yourself with `sonar start`")
	}

	rt := req.Runtime
	initial := rpc.GroupsStartResult{MutationResult: rpc.MutationResult{OK: true, Affected: []string{}}}
	return daemon.StartStream(ctx, req, initial, func(ctx context.Context, s *daemon.Stream) (any, error) {
		return run(ctx, rt, s, cfg, group, plan, p.AllowOutsideHome), nil
	})
}

// run starts the planned services one at a time, sending a chunk per service.
// A service that fails never stops the ones after it: the caller asked for the
// group, and a partial group is more useful than none.
func run(ctx context.Context, rt *daemon.Runtime, s *daemon.Stream,
	cfg *groups.Config, group string, plan []groups.Step, allowOutsideHome bool) rpc.GroupsStartEnd {

	end := rpc.GroupsStartEnd{Started: []string{}, Skipped: []string{}, Errors: []string{}}

	for _, step := range plan {
		if ctx.Err() != nil {
			return end
		}
		svc := step.Service

		if reason, up := alreadyRunning(rt, group, svc); up {
			_ = s.Send(rpc.GroupsStartChunk{Service: svc.Name, Skipped: true, Reason: reason})
			end.Skipped = append(end.Skipped, svc.Name)
			continue
		}

		if err := waitFor(ctx, rt, group, step.Waits); err != nil {
			if ctx.Err() != nil {
				return end
			}
			_ = s.Send(rpc.GroupsStartChunk{Service: svc.Name, Error: err.Error()})
			end.Errors = append(end.Errors, svc.Name)
			continue
		}

		h, err := start(ctx, rt, cfg, group, svc, allowOutsideHome)
		if err != nil {
			rt.Logger.Warn("starting a service", "group", group, "service", svc.Name, "error", err)
			_ = s.Send(rpc.GroupsStartChunk{Service: svc.Name, Error: detail(err)})
			end.Errors = append(end.Errors, svc.Name)
			continue
		}
		_ = s.Send(rpc.GroupsStartChunk{Service: svc.Name, PID: h.PID, LogPath: h.LogPath})
		end.Started = append(end.Started, svc.Name)
	}
	return end
}

// start spawns one service through the run registry, so the ports it opens are
// attributed to this group and this service name.
func start(ctx context.Context, rt *daemon.Runtime, cfg *groups.Config, group string,
	svc groups.Service, allowOutsideHome bool) (*spawn.Handle, error) {

	argv := spawn.SplitCmd(svc.Cmd)
	if len(argv) == 0 {
		return nil, fmt.Errorf("service %s has no cmd to run", svc.Name)
	}
	cwd, err := runsreg.CheckCwd(cfg.ServiceDir(svc), allowOutsideHome)
	if err != nil {
		return nil, err
	}
	return runsreg.Spawn(ctx, rt, spawn.Request{
		Argv:     argv,
		Cwd:      cwd,
		Group:    group,
		Name:     svc.Name,
		PortHint: svc.Port,
		LogPath:  spawn.LogPath(group, svc.Name),
	})
}

// alreadyRunning reports whether a service is up, and why we think so.
//
// The run registry is asked first, because it knows the instant a service has
// been spawned while the scanner only knows a second or two later: two
// `sonar up` runs in quick succession must not start the same service twice.
// The group's resolved state answers for everything sonar did not start — it
// already joins declared ports, run names and display names against what is
// listening.
func alreadyRunning(rt *daemon.Runtime, group string, svc groups.Service) (string, bool) {
	for _, rec := range runsreg.Default.List() {
		if rec.Group == group && rec.Name == svc.Name {
			return fmt.Sprintf("already started by sonar (pid %d)", rec.PID), true
		}
	}
	for _, g := range snapshot(rt).Groups {
		if g.Name != group {
			continue
		}
		for _, row := range g.Services {
			if row.Name != svc.Name || !row.Running {
				continue
			}
			if row.PortActual != nil {
				return fmt.Sprintf("already running on port %d", *row.PortActual), true
			}
			return "already running", true
		}
	}
	return "", false
}

// waitFor blocks until every dependency's port is listening, or gives up after
// DependencyTimeout. Waiting on the daemon's own state rather than on a socket
// dial is deliberate: the thing that decides a service is up has to be the
// thing every client reads.
func waitFor(ctx context.Context, rt *daemon.Runtime, group string, deps []groups.Service) error {
	if len(deps) == 0 {
		return nil
	}
	deadline := time.Now().Add(dependencyTimeout)
	for {
		missing := pending(rt, group, deps)
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s",
				dependencyTimeout, strings.Join(missing, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dependencyPoll):
		}
	}
}

// pending lists the dependencies that are not listening yet.
func pending(rt *daemon.Runtime, group string, deps []groups.Service) []string {
	snap := snapshot(rt)
	var missing []string
	for _, dep := range deps {
		if !listening(snap, group, dep) {
			missing = append(missing, fmt.Sprintf("%s on port %d", dep.Name, dep.Port))
		}
	}
	return missing
}

// listening reports whether a dependency's declared port has a listener. The
// group row is consulted first, because it knows a service may have bound a
// different port than the one it declared; the raw port list is the fallback
// for a dependency the resolver has not joined yet.
func listening(snap state.Snapshot, group string, dep groups.Service) bool {
	for _, g := range snap.Groups {
		if g.Name != group {
			continue
		}
		for _, row := range g.Services {
			if row.Name == dep.Name {
				return row.Running
			}
		}
	}
	if dep.Port == 0 {
		return true
	}
	for _, p := range snap.Ports {
		if p.Port == dep.Port {
			return true
		}
	}
	return false
}

// snapshot reads the daemon's state, scanning only when the cache is stale.
func snapshot(rt *daemon.Runtime) state.Snapshot {
	snap, err := rt.Scanner.Snapshot(scanner.Include{})
	if err != nil {
		return rt.Scanner.Cached()
	}
	return snap
}

// resolveConfig finds the `sonar.yaml` this call is about, by path or by
// group name.
func resolveConfig(rt *daemon.Runtime, p rpc.GroupsStartParams) (*groups.Config, error) {
	if p.ConfigPath != nil && strings.TrimSpace(*p.ConfigPath) != "" {
		path := strings.TrimSpace(*p.ConfigPath)
		if cfg, ok := rt.Scanner.ConfigAt(path); ok {
			return cfg, nil
		}
		if err := rt.Scanner.LoadConfig(path); err != nil {
			var bad *groups.ConfigError
			if errors.As(err, &bad) {
				return nil, rpc.NewError(rpc.CodeInvalidConfig, bad.Error(),
					"fix the file and try again")
			}
			return nil, rpc.NewError(rpc.CodeNotFound, "cannot read "+path+": "+err.Error(),
				"`sonar init` writes a "+groups.ConfigName+" at the repository root")
		}
		if cfg, ok := rt.Scanner.ConfigAt(path); ok {
			return cfg, nil
		}
		return nil, rpc.NewError(rpc.CodeNotFound, "no usable "+groups.ConfigName+" at "+path, "")
	}
	if p.Name != nil && strings.TrimSpace(*p.Name) != "" {
		name := strings.TrimSpace(*p.Name)
		if cfg, ok := rt.Scanner.ConfigNamed(name); ok {
			return cfg, nil
		}
		return nil, rpc.NewError(rpc.CodeNotFound,
			"no group named "+name+" has a "+groups.ConfigName,
			"`sonar groups` lists the configs this daemon knows; only a group with a config can be started")
	}
	return nil, rpc.NewError(rpc.CodeInvalidParams, "name or config_path is required",
		`send {"name": "my-app"} or {"config_path": "/repo/sonar.yaml"}`)
}

// detail unwraps an rpc error so a chunk carries the message a user reads
// rather than the JSON-RPC envelope's wrapper.
func detail(err error) string {
	var re *rpc.Error
	if errors.As(err, &re) {
		return re.Data.Detail
	}
	return err.Error()
}
