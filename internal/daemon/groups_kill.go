package daemon

import (
	"context"
	"errors"
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
// With only, all of that is narrowed to the named services of the group's
// `sonar.yaml`.
func handleGroupsKill(ctx context.Context, req *Request) (any, error) {
	var p rpc.GroupsKillParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	name, cfg, err := killGroupName(req.Runtime, p)
	if err != nil {
		return nil, err
	}

	// `only` names services, and only the file knows them: without one the
	// names have nothing to be checked against, and the daemon has no row to
	// map them onto.
	var only []groups.Service
	if len(p.Only) > 0 {
		if cfg == nil {
			cfg = configForGroup(req.Runtime, name)
		}
		if cfg == nil {
			return nil, rpc.NewError(rpc.CodeInvalidParams,
				"only names services, and no "+groups.ConfigName+" is known for group "+name,
				"check the name with `sonar groups`, or stop one port with `sonar kill <port>`")
		}
		only, err = onlyServices(cfg, name, p.Only)
		if err != nil {
			return nil, err
		}
		if len(only) == 0 {
			return nil, rpc.NewError(rpc.CodeInvalidParams, "only names no service",
				`send {"only": ["api"]}, or leave only out to stop the whole group`)
		}
	}

	snap, err := killSnapshot(req)
	if err != nil {
		return nil, err
	}

	targets := groupTargets(snap, name)
	if only != nil {
		targets = serviceTargets(snap, name, targets, only)
	}
	var runPIDs []int
	if p.Release {
		runPIDs = groupRunPIDs(req.Runtime, name, only)
		if cfg == nil {
			cfg = configForGroup(req.Runtime, name)
		}
	}
	if len(targets) == 0 && len(runPIDs) == 0 {
		if !p.Release || cfg == nil {
			detail := "no listening port belongs to group " + name
			hint := "run `sonar groups` to see what is grouped right now"
			if only != nil {
				detail = "no listening port belongs to " + strings.Join(onlyNames(only), ", ") + " in group " + name
				hint = "run `sonar groups " + name + "` to see what is running right now"
			}
			return nil, killRPCError(&killer.CodedError{
				Code:   killer.CodeNotFound,
				Detail: detail,
				Hint:   hint,
			})
		}
		// Nothing is running, but a project with a config still has claims
		// to give back.
		env := killEnvelope(nil)
		if !p.DryRun {
			n, err := ReleaseServicePorts(req.Runtime, cfg, onlyNames(only))
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
		if !opts.DryRun {
			req.Runtime.Runs().Stopping(runRoots(snap, targets))
		}
		rows = killer.KillPorts(ctx, targets, opts)
	}
	if p.Release && !p.DryRun {
		// Runs that held no port — a worker, a service still starting — are
		// stopped by pid, each with its whole tree. The registry is read again
		// after the port kill: most runs went down with their ports.
		var pidTargets []killer.Target
		for _, pid := range groupRunPIDs(req.Runtime, name, only) {
			pidTargets = append(pidTargets, killer.Target{PID: pid})
		}
		if len(pidTargets) > 0 {
			tree := opts
			tree.Tree = true
			req.Runtime.Runs().Stopping(runRoots(snap, pidTargets))
			rows = append(rows, killer.KillPorts(ctx, pidTargets, tree)...)
		}
	}
	afterKill(req, opts.DryRun)

	env := killEnvelope(rows)
	if p.Release && !p.DryRun && cfg != nil {
		n, err := ReleaseServicePorts(req.Runtime, cfg, onlyNames(only))
		if err != nil {
			// The services are down; a claim left behind expires on its own.
			req.Runtime.Logger.Warn("releasing a group's claims", "group", name, "error", err)
		}
		env.Released = n
	}
	return env, nil
}

// onlyServices resolves an `only` list against the file, the way groups.start
// does: every name has to be one the file declares, and an unknown one is
// not_found rather than a silent no-op.
func onlyServices(cfg *groups.Config, group string, only []string) ([]groups.Service, error) {
	plan, err := groups.Plan(cfg, only)
	if err != nil {
		var unknown *groups.UnknownServiceError
		if errors.As(err, &unknown) {
			return nil, rpc.NewError(rpc.CodeNotFound, unknown.Error(),
				"`sonar groups "+group+"` lists the services this file declares")
		}
		return nil, rpc.NewError(rpc.CodeInternal, err.Error(), "")
	}
	out := make([]groups.Service, 0, len(plan))
	for _, step := range plan {
		out = append(out, step.Service)
	}
	return out, nil
}

// serviceTargets keeps the group's targets that belong to one of the named
// services, joined the way the group's service rows are: the port the file
// declares, the port the row says the service is on, and a run or a display
// name that carries the service's name.
func serviceTargets(snap state.Snapshot, group string, targets []killer.Target, only []groups.Service) []killer.Target {
	names := make(map[string]bool, len(only))
	wantPort := map[int]bool{}
	for _, svc := range only {
		names[svc.Name] = true
		if svc.Port != 0 {
			wantPort[svc.Port] = true
		}
	}
	for _, g := range snap.Groups {
		if g.Name != group {
			continue
		}
		for _, row := range g.Services {
			if names[row.Name] && row.PortActual != nil {
				wantPort[*row.PortActual] = true
			}
		}
	}
	var out []killer.Target
	for _, t := range targets {
		if wantPort[t.Port] {
			out = append(out, t)
			continue
		}
		for _, p := range snap.Ports {
			if p.Port != t.Port || p.BindAddress != t.BindAddress {
				continue
			}
			if (p.Run != nil && names[p.Run.Name]) || names[p.DisplayName] {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// groupRunPIDs lists the runs a release stops: every run in the group, or
// with only the runs started under the named services.
func groupRunPIDs(rt *Runtime, group string, only []groups.Service) []int {
	if only == nil {
		return rt.Runs().GroupPIDs(group)
	}
	var out []int
	for _, svc := range only {
		out = append(out, rt.Runs().ServicePIDs(group, svc.Name)...)
	}
	return out
}

// onlyNames is the names of services resolved from `only`, trimmed and
// checked against the file; nil when there is no `only`.
func onlyNames(services []groups.Service) []string {
	if services == nil {
		return nil
	}
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}
	return names
}

// runRoots lists the runs a kill is about to stop: every target given by pid,
// and the run that owns each targeted port or run id. The registry marks them
// so their exit is recorded as stopped rather than as a crash.
func runRoots(snap state.Snapshot, targets []killer.Target) []int {
	var out []int
	for _, t := range targets {
		if t.PID > 0 {
			out = append(out, t.PID)
			continue
		}
		for _, p := range snap.Ports {
			if p.Run == nil || p.Run.RootPID <= 0 {
				continue
			}
			if t.RunID != "" && p.Run.ID != t.RunID {
				continue
			}
			if t.RunID == "" {
				if p.Port != t.Port {
					continue
				}
				if t.BindAddress != "" && p.BindAddress != t.BindAddress {
					continue
				}
			}
			out = append(out, p.Run.RootPID)
		}
	}
	return out
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
