package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/killer"
	"github.com/raskrebs/sonar/internal/state"
)

// handleGroupsKill stops a group's listening ports. With release it is
// `sonar down`: it also stops the runs sonar started in the group that hold no
// port, and gives back the claims the group's `port: auto` services hold.
func handleGroupsKill(ctx context.Context, req *Request) (any, error) {
	var p rpc.GroupsKillParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	name, cfg, err := killGroupName(req.Runtime, p)
	if err != nil {
		return nil, err
	}

	snap, err := killSnapshot(req)
	if err != nil {
		return nil, err
	}

	targets := groupTargets(snap, name)
	var runPIDs []int
	if p.Release {
		runPIDs = req.Runtime.Runs().GroupPIDs(name)
		if cfg == nil {
			cfg = configForGroup(req.Runtime, name)
		}
	}
	if len(targets) == 0 && len(runPIDs) == 0 {
		if !p.Release || cfg == nil {
			return nil, killRPCError(&killer.CodedError{
				Code:   killer.CodeNotFound,
				Detail: "no listening port belongs to group " + name,
				Hint:   "run `sonar groups` to see what is grouped right now",
			})
		}
		// Nothing is running, but a project with a config still has claims
		// to give back.
		env := killEnvelope(nil)
		if !p.DryRun {
			n, err := ReleaseServicePorts(req.Runtime, cfg)
			if err != nil {
				return nil, err
			}
			env.Released = n
		}
		return env, nil
	}

	opts := killer.Options{
		Force:  p.Force,
		Grace:  time.Duration(p.GraceMs) * time.Millisecond,
		DryRun: p.DryRun,
		Ports:  killerRows(snap),
	}
	var rows []state.KillResult
	if len(targets) > 0 {
		rows = killer.KillPorts(ctx, targets, opts)
	}
	if p.Release && !p.DryRun {
		// Runs that held no port — a worker, a service still starting — are
		// stopped by pid, each with its whole tree. The registry is read again
		// after the port kill: most runs went down with their ports.
		var pidTargets []killer.Target
		for _, pid := range req.Runtime.Runs().GroupPIDs(name) {
			pidTargets = append(pidTargets, killer.Target{PID: pid})
		}
		if len(pidTargets) > 0 {
			tree := opts
			tree.Tree = true
			rows = append(rows, killer.KillPorts(ctx, pidTargets, tree)...)
		}
	}
	afterKill(req, opts.DryRun)

	env := killEnvelope(rows)
	if p.Release && !p.DryRun && cfg != nil {
		n, err := ReleaseServicePorts(req.Runtime, cfg)
		if err != nil {
			// The services are down; a claim left behind expires on its own.
			req.Runtime.Logger.Warn("releasing a group's claims", "group", name, "error", err)
		}
		env.Released = n
	}
	return env, nil
}

// killGroupName is the group a groups.kill call is about: the name it sent, or
// the group the daemon publishes a config's services under. The config comes
// back too when the call named one.
func killGroupName(rt *Runtime, p rpc.GroupsKillParams) (string, *groups.Config, error) {
	if name := strings.TrimSpace(p.Name); name != "" {
		return name, nil, nil
	}
	if p.ConfigPath != nil && strings.TrimSpace(*p.ConfigPath) != "" {
		path := strings.TrimSpace(*p.ConfigPath)
		cfg, ok := rt.Scanner.ConfigAt(path)
		if !ok {
			if err := rt.Scanner.LoadConfig(path); err != nil {
				return "", nil, configError(path, err)
			}
			if cfg, ok = rt.Scanner.ConfigAt(path); !ok {
				return "", nil, rpc.NewError(rpc.CodeNotFound, "no usable "+groups.ConfigName+" at "+path, "")
			}
		}
		return rt.Scanner.GroupOf(cfg), cfg, nil
	}
	return "", nil, rpc.NewError(rpc.CodeInvalidParams, "name or config_path is required",
		`send {"name": "my-app"} or {"config_path": "/repo/sonar.yaml"}`)
}
