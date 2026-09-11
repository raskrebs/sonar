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
	"os"
	"sort"
	"strconv"
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
		return run(ctx, rt, s, cfg, group, plan, p), nil
	})
}

// run starts the planned services one at a time, sending a chunk per service.
// A service that fails never stops the ones after it: the caller asked for the
// group, and a partial group is more useful than none.
func run(ctx context.Context, rt *daemon.Runtime, s *daemon.Stream,
	cfg *groups.Config, group string, plan []groups.Step, p rpc.GroupsStartParams) rpc.GroupsStartEnd {

	end := rpc.GroupsStartEnd{Started: []string{}, Skipped: []string{}, Errors: []string{}}
	book := newAddressBook(rt, cfg, group)

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

		if err := waitFor(ctx, rt, group, step.Waits, book); err != nil {
			if ctx.Err() != nil {
				return end
			}
			_ = s.Send(rpc.GroupsStartChunk{Service: svc.Name, Error: err.Error()})
			end.Errors = append(end.Errors, svc.Name)
			continue
		}

		h, err := start(ctx, rt, cfg, group, svc, book, p)
		if err != nil {
			rt.Logger.Warn("starting a service", "group", group, "service", svc.Name, "error", err)
			_ = s.Send(rpc.GroupsStartChunk{Service: svc.Name, Error: detail(err)})
			end.Errors = append(end.Errors, svc.Name)
			continue
		}
		_ = s.Send(rpc.GroupsStartChunk{Service: svc.Name, PID: h.PID, Port: h.PortHint, LogPath: h.LogPath})
		end.Started = append(end.Started, svc.Name)
	}
	return end
}

// start spawns one service through the run registry, so the ports it opens are
// attributed to this group and this service name. Its references are expanded
// and its environment built here, once every port it names is known.
func start(ctx context.Context, rt *daemon.Runtime, cfg *groups.Config, group string,
	svc groups.Service, book *addressBook, p rpc.GroupsStartParams) (*spawn.Handle, error) {

	argv := spawn.SplitCmd(svc.Cmd)
	if len(argv) == 0 {
		return nil, fmt.Errorf("service %s has no cmd to run", svc.Name)
	}
	ports, err := book.forService(svc)
	if err != nil {
		return nil, err
	}
	// Expanded after splitting, so a value can never change how the command
	// splits into arguments.
	for i := range argv {
		argv[i] = groups.Expand(argv[i], svc.Name, ports)
	}
	cwd, err := runsreg.CheckCwd(cfg.ServiceDir(svc), p.AllowOutsideHome)
	if err != nil {
		return nil, err
	}
	port := ports[svc.Name]
	return runsreg.Spawn(ctx, rt, spawn.Request{
		Argv:     argv,
		Cwd:      cwd,
		Env:      serviceEnv(p.Env, svc, port, ports),
		Group:    group,
		Name:     svc.Name,
		PortHint: port,
		LogPath:  spawn.LogPath(group, svc.Name),
	})
}

// serviceEnv is the environment a service starts in, each layer winning over
// the one before: the daemon's own, the caller's (the CLI sends its shell's),
// PORT for a service with a port, and the service's own `env:` with its
// references expanded. SONAR_PORT and the other run variables are added by
// spawn on top of all of it.
func serviceEnv(caller map[string]string, svc groups.Service, port int, ports map[string]int) []string {
	over := make(map[string]string, len(caller)+len(svc.Env)+1)
	for k, v := range caller {
		over[k] = v
	}
	if port > 0 {
		over["PORT"] = strconv.Itoa(port)
	}
	for k, v := range svc.Env {
		over[k] = groups.Expand(v, svc.Name, ports)
	}
	return layer(os.Environ(), over)
}

// layer returns base with every key in over replaced or added.
func layer(base []string, over map[string]string) []string {
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		if key, _, ok := strings.Cut(kv, "="); ok {
			if _, replaced := over[key]; replaced {
				continue
			}
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+over[k])
	}
	return out
}

// addressBook is the port every service of one file runs on, worked out once
// per groups.start and only for the services this start needs: the ones it
// spawns and the ones their cmd and env refer to. It is what ${port} and
// ${<service>.port} expand to, what PORT is set to, and what a dependent waits
// for.
type addressBook struct {
	rt    *daemon.Runtime
	cfg   *groups.Config
	group string
	live  map[string]int
	ports map[string]int
	errs  map[string]error
}

func newAddressBook(rt *daemon.Runtime, cfg *groups.Config, group string) *addressBook {
	b := &addressBook{
		rt: rt, cfg: cfg, group: group,
		live: map[string]int{}, ports: map[string]int{}, errs: map[string]error{},
	}
	// A service that is already up keeps the port it is on: the run registry
	// knows the port sonar started it on, and the group row knows where
	// anything else is listening.
	for _, rec := range runsreg.Default.List() {
		if rec.Group == group && rec.PortHint > 0 {
			b.live[rec.Name] = rec.PortHint
		}
	}
	for _, g := range snapshot(rt).Groups {
		if g.Name != group {
			continue
		}
		for _, row := range g.Services {
			if _, known := b.live[row.Name]; !known && row.Running && row.PortActual != nil {
				b.live[row.Name] = *row.PortActual
			}
		}
	}
	return b
}

// port is the port a service runs on, or 0 for one that declares none. A fixed
// port is itself. A `port: auto` service that is already up keeps its port;
// one that is not gets its claim.
func (b *addressBook) port(name string) (int, error) {
	if port, ok := b.ports[name]; ok {
		return port, nil
	}
	if err, ok := b.errs[name]; ok {
		return 0, err
	}
	svc, ok := b.cfg.ServiceNamed(name)
	var (
		port int
		err  error
	)
	switch {
	case !ok || !svc.HasPort():
	case svc.Port != 0:
		port = svc.Port
	case b.live[name] != 0:
		port = b.live[name]
	default:
		port, err = daemon.AcquireServicePort(b.rt, b.cfg.Dir, name)
	}
	if err != nil {
		b.errs[name] = err
		return 0, err
	}
	b.ports[name] = port
	return port, nil
}

// forService resolves the ports a service's cmd and env need: its own, and
// every service they refer to.
func (b *addressBook) forService(svc groups.Service) (map[string]int, error) {
	names := groups.Refs(svc.Cmd, svc.Name)
	for _, v := range svc.Env {
		names = append(names, groups.Refs(v, svc.Name)...)
	}
	if svc.HasPort() {
		names = append(names, svc.Name)
	}
	out := make(map[string]int, len(names))
	for _, name := range names {
		port, err := b.port(name)
		if err != nil {
			return nil, fmt.Errorf("no port for %s: %s", name, detail(err))
		}
		out[name] = port
	}
	return out, nil
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
func waitFor(ctx context.Context, rt *daemon.Runtime, group string, deps []groups.Service, book *addressBook) error {
	if len(deps) == 0 {
		return nil
	}
	want := make(map[string]int, len(deps))
	for _, dep := range deps {
		port, err := book.port(dep.Name)
		if err != nil {
			return fmt.Errorf("%s has no port to wait for: %s", dep.Name, detail(err))
		}
		want[dep.Name] = port
	}
	deadline := time.Now().Add(dependencyTimeout)
	for {
		missing := pending(rt, group, deps, want)
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
func pending(rt *daemon.Runtime, group string, deps []groups.Service, want map[string]int) []string {
	snap := snapshot(rt)
	var missing []string
	for _, dep := range deps {
		if !listening(snap, group, dep, want[dep.Name]) {
			missing = append(missing, fmt.Sprintf("%s on port %d", dep.Name, want[dep.Name]))
		}
	}
	return missing
}

// listening reports whether a dependency is up on port. The group row is
// consulted first, because it knows a service may have bound a different port
// than the one it declared. For a fixed port the row is the answer; for an
// assigned one the raw port list is asked too, because the scanner may not
// have joined the listener to the service yet. The raw list is also the
// fallback for a dependency the resolver has no row for.
func listening(snap state.Snapshot, group string, dep groups.Service, port int) bool {
	for _, g := range snap.Groups {
		if g.Name != group {
			continue
		}
		for _, row := range g.Services {
			if row.Name != dep.Name {
				continue
			}
			if row.Running || !dep.PortAuto {
				return row.Running
			}
		}
	}
	if port == 0 {
		return true
	}
	for _, p := range snap.Ports {
		if p.Port == port {
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
