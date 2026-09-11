package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/state"
)

// `groups.init` is `sonar init` over the wire (contract §4): it proposes a
// `sonar.yaml` for a checkout from what is listening in it right now, and
// optionally writes it.
//
// Both paths share one proposer, groups.Propose, and nothing else. The CLI
// keeps scanning the OS itself and writing the file itself, because `sonar
// init` has to work on a machine with no daemon running; routing it through
// this handler would trade that away for no gain. What must not drift is the
// *proposal* — which ports become services, what they are named, what command
// is guessed — and that lives in one function both callers hand their ports to.
//
// The daemon has no working directory of the caller's, so unlike the CLI it
// cannot default the root: `root_dir` is required (contract §4).
func init() {
	RegisterHandler("groups.init", handleGroupsInit)
	// The capability is already announced by groups_config.go; registering it
	// again is a no-op, and saying it here keeps the method self-contained.
	RegisterCapability("groups")
}

func handleGroupsInit(_ context.Context, req *Request) (any, error) {
	var p rpc.GroupsInitParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	if p.Force && p.Merge {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "force and merge cannot both be set",
			"force replaces the file, merge appends to it; pick one")
	}
	for _, svc := range p.Services {
		if strings.TrimSpace(svc.Name) == "" {
			return nil, rpc.NewError(rpc.CodeInvalidParams, "services names an empty service",
				`each entry is {"name": "api", "port": 8000}`)
		}
	}
	root, err := initRoot(p.RootDir)
	if err != nil {
		return nil, err
	}
	// A project that already has a config, in any spelling, is merged into or
	// overwritten in place; only a project without one gets a new file.
	target := groups.TargetIn(root)
	// The daemon writes exactly one filename on a client's say-so, and the
	// check is made here too rather than trusted to the join above.
	if err := checkConfigPath(target); err != nil {
		return nil, err
	}

	snap, err := initSnapshot(req.Runtime)
	if err != nil {
		return nil, err
	}
	cfg := groups.Propose(root, state.ToListeningAll(snap.Ports), nil)
	if len(p.Services) > 0 {
		// The caller curated the proposal: names, ports and metadata are
		// theirs, and the cmd the proposal guessed is carried onto an entry
		// that kept the port it was guessed for.
		cfg = groups.Curate(cfg, p.Services)
	}

	_, statErr := os.Lstat(target)
	exists := statErr == nil
	if p.Merge && exists {
		return mergeInit(req, target, cfg, snap, p.Write)
	}

	data, err := groups.Marshal(cfg)
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternal, "rendering "+groups.ConfigName+": "+err.Error(),
			"check `sonar daemon log`")
	}
	// A curated list is the caller's, so it is validated here rather than
	// discovered by the next scan. The pure proposal cannot fail this.
	if _, err := groups.Parse(target, data); err != nil {
		return nil, configError(target, err)
	}

	result := rpc.GroupsInitResult{
		MutationResult: rpc.MutationResult{OK: true, Affected: serviceNames(cfg)},
		Path:           target,
		YAML:           string(data),
		Proposal:       proposalGroup(cfg, snapshotPorts(snap)),
	}
	if !p.Write {
		// The default is a preview: the caller gets the exact bytes a write
		// would put on disk and decides.
		return result, nil
	}

	// §16: `sonar init` refuses to overwrite without --force, and so does this.
	// The registry (contract §2) has no `already_exists`, and inventing a code
	// for one method is worse than the accurate hint, so this is
	// `invalid_params` naming the parameters that unblock it.
	if exists && !p.Force {
		return nil, rpc.NewError(rpc.CodeInvalidParams, target+" already exists",
			`pass {"force": true} to overwrite it or {"merge": true} to append to it, `+
				"or read it with groups.config.get")
	}
	if err := groups.WriteConfigFile(target, data); err != nil {
		return nil, rpc.NewError(rpc.CodeInternal, "writing "+target+": "+err.Error(),
			"check that the daemon may write to "+root)
	}
	publishConfig(req, target, cfg.Name, len(cfg.Services))
	return result, nil
}

// mergeInit appends a proposal into a `sonar.yaml` that is already there,
// rather than refusing it. The append goes through the same node-level edit
// `groups.config.set` uses, so the file's comments and order survive and a
// service name or port the file already has is a `conflict` — and, because the
// bytes are rendered before anything is written, `write: false` previews the
// merged file exactly.
func mergeInit(req *Request, target string, cfg *groups.Config, snap state.Snapshot, write bool) (any, error) {
	adds := groups.AddsFrom(cfg)
	data, merged, err := groups.RenderEdit(target, groups.ConfigEdit{Add: adds})
	if err != nil {
		return nil, configError(target, err)
	}
	result := rpc.GroupsInitResult{
		MutationResult: rpc.MutationResult{OK: true, Affected: serviceNames(cfg)},
		Path:           target,
		YAML:           string(data),
		Proposal:       proposalGroup(merged, snapshotPorts(snap)),
	}
	if !write {
		return result, nil
	}
	if err := groups.WriteConfigFile(merged.Path, data); err != nil {
		return nil, rpc.NewError(rpc.CodeInternal, "writing "+target+": "+err.Error(),
			"check that the daemon may write to "+filepath.Dir(target))
	}
	publishConfig(req, merged.Path, merged.Name, len(adds))
	return result, nil
}

// publishConfig indexes a file the daemon just wrote and republishes, so the
// group reaches subscribers as a groups delta before the caller's own next read
// (the same order `groups.config.set` keeps, contract §38 and §44).
func publishConfig(req *Request, path, group string, services int) {
	if err := req.Runtime.Scanner.LoadConfig(path); err != nil {
		req.Runtime.Logger.Warn("reloading a config after writing it", "path", path, "error", err)
	}
	req.Runtime.Logger.Info("wrote "+groups.ConfigName, "path", path, "group", group, "services", services)
	republish(req.Runtime)
}

// snapshotPorts is the set of ports the snapshot this call read saw listening.
func snapshotPorts(snap state.Snapshot) map[int]bool {
	out := make(map[int]bool, len(snap.Ports))
	for _, p := range snap.Ports {
		out[p.Port] = true
	}
	return out
}

// initRoot validates the caller's root and resolves it the way `sonar init`
// resolves its own working directory: the git root above it when there is one,
// so the file lands next to `.git` and matches the group the resolver already
// publishes (§16).
func initRoot(raw string) (string, error) {
	dir := strings.TrimSpace(raw)
	if dir == "" {
		return "", rpc.NewError(rpc.CodeInvalidParams, "root_dir is required",
			`the daemon has no working directory of yours; send {"root_dir": "/path/to/repo"}`)
	}
	if !filepath.IsAbs(dir) {
		return "", rpc.NewError(rpc.CodeInvalidParams, "root_dir must be an absolute path: "+dir,
			"resolve it against the caller's working directory first")
	}
	abs := groups.Canonical(dir)
	info, err := os.Stat(abs)
	if err != nil {
		return "", rpc.NewError(rpc.CodeNotFound, "no such directory: "+dir,
			"root_dir is the project directory the "+groups.ConfigName+" goes in")
	}
	if !info.IsDir() {
		return "", rpc.NewError(rpc.CodeInvalidParams, dir+" is not a directory",
			"root_dir is the project directory, not the "+groups.ConfigName+" itself")
	}
	if root, _, ok := groups.Find(abs); ok {
		return root, nil
	}
	return abs, nil
}

// initSnapshot is the state the proposal is built from: the same cache
// `groups.list` reads, refreshed when nobody is keeping it warm (contract §20).
func initSnapshot(rt *Runtime) (state.Snapshot, error) {
	if rt.Subscribers() > 0 {
		if snap := rt.Scanner.Cached(); snap.Seq > 0 {
			return snap, nil
		}
	}
	snap, err := rt.Scanner.Snapshot(scanner.Include{})
	if err != nil {
		return state.Snapshot{}, rpc.NewError(rpc.CodeInternal, "scan failed: "+err.Error(),
			"check `sonar daemon log` for the scanner error")
	}
	return snap, nil
}

// serviceNames is the affected list: this method mutates a file, so what it
// reports is service names, exactly as `groups.config.set` does (contract §22).
func serviceNames(cfg *groups.Config) []string {
	out := make([]string, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		out = append(out, s.Name)
	}
	return out
}

// proposalGroup renders the config this call would write as the group row it
// produces. A service is marked running when its port is listening in the
// snapshot the call read: every service of a plain proposal came from such a
// port, so that path is unchanged, while a curated or merged file can honestly
// carry services that are declared but down.
func proposalGroup(cfg *groups.Config, listening map[int]bool) state.Group {
	dir, path := cfg.Dir, cfg.Path
	g := state.Group{
		Name:       cfg.Name,
		Source:     state.SourceFile,
		RootDir:    &dir,
		ConfigPath: &path,
		Status:     "stopped",
		Members:    []int{},
		Services:   groups.ServiceRows(cfg),
	}
	up := 0
	for i := range g.Services {
		if g.Services[i].Port == nil || !listening[*g.Services[i].Port] {
			continue
		}
		port := *g.Services[i].Port
		g.Services[i].Running, g.Services[i].PortActual = true, &port
		g.Members = append(g.Members, port)
		up++
	}
	sort.Ints(g.Members)
	switch {
	case up == 0:
		g.Status = "stopped"
	case up == len(g.Services):
		g.Status = "running"
	default:
		g.Status = "partial"
	}
	return g
}
