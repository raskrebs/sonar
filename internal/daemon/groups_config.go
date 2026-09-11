package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/state"
)

// The `.sonar.yaml` read/write namespace (contract §13.2) plus the reload that
// keeps the daemon's long-lived index in step with the disk. `groups.start`
// lives in internal/daemon/groupstart, because starting a service needs the
// run registry and this package must not import it (contract §8).
func init() {
	RegisterHandler("groups.config.get", handleGroupsConfigGet)
	RegisterHandler("groups.config.set", handleGroupsConfigSet)
	RegisterHandler("groups.reload", handleGroupsReload)
	RegisterCapability("groups")
}

func handleGroupsConfigGet(_ context.Context, req *Request) (any, error) {
	var p rpc.GroupsConfigGetParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	cfg, err := resolveConfig(req.Runtime, p.Name, p.Path)
	if err != nil {
		return nil, err
	}
	return rpc.GroupsConfigGetResult{
		Path:   cfg.Path,
		Config: configRow(req.Runtime, cfg),
	}, nil
}

func handleGroupsConfigSet(_ context.Context, req *Request) (any, error) {
	var p rpc.GroupsConfigSetParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	path := strings.TrimSpace(p.Path)
	if path == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "path is required",
			`send {"path": "/repo/.sonar.yaml", "services": [{"name": "api", "patch": {"icon": "server"}}]}`)
	}
	edit := groups.ConfigEdit{
		Remove:   p.Remove,
		Rename:   p.Rename,
		Add:      p.Add,
		Services: p.Services,

		WorktreePorts: p.WorktreePortsChange(),
	}
	if edit.Empty() {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "services, add, rename, remove or worktree_ports is required",
			`send {"path": "…", "add": [{"name": "worker", "port": 9000}]}, or a rename, a remove, `+
				`{"services": [{"name": "api", "patch": {...}}]} or {"worktree_ports": 4}`)
	}
	if err := checkEditNames(edit); err != nil {
		return nil, err
	}
	if err := checkConfigPath(path); err != nil {
		return nil, err
	}

	// The file is re-rendered from its own node tree, so comments, key order
	// and formatting survive an edit — with one exception: yaml.v3 keeps the
	// text of a trailing comment but not the padding in front of it, so
	// `cmd: x     # note` comes back as `cmd: x # note`. Node carries no column
	// for a LineComment and the emitter always writes exactly one space, so
	// preserving it would mean re-rendering the file by hand; the comment
	// itself is never lost, and that is where this stops.
	//
	// The whole edit is applied to the node tree and validated before a byte is
	// written, so a rename that clashes half way through a batch leaves the
	// file exactly as it was.
	cfg, err := groups.EditServices(path, edit)
	if err != nil {
		return nil, configError(path, err)
	}

	// Index the file the daemon just wrote, then republish: the edit reaches
	// subscribers as a groups delta before the caller's own next read
	// (contract §13.2, §38, §44).
	if err := req.Runtime.Scanner.LoadConfig(cfg.Path); err != nil {
		req.Runtime.Logger.Warn("reloading a config after writing it", "path", cfg.Path, "error", err)
	}
	affected := edit.Affected()
	req.Runtime.Logger.Info("config written", "path", cfg.Path, "services", len(affected))
	republish(req.Runtime)

	return rpc.GroupsConfigSetResult{
		MutationResult: rpc.MutationResult{OK: true, Affected: affected},
		Path:           cfg.Path,
		Config:         configRow(req.Runtime, cfg),
	}, nil
}

// checkEditNames rejects an edit whose shape is wrong before anything is read
// from the disk. An empty service name is a caller bug, not a config problem,
// so it is `invalid_params` rather than the `invalid_config` a nameless service
// would earn once written.
func checkEditNames(edit groups.ConfigEdit) error {
	for _, name := range edit.Remove {
		if strings.TrimSpace(name) == "" {
			return rpc.NewError(rpc.CodeInvalidParams, "remove names an empty service",
				`each entry is the name of a service in the file: {"remove": ["worker"]}`)
		}
	}
	for _, r := range edit.Rename {
		if strings.TrimSpace(r.From) == "" || strings.TrimSpace(r.To) == "" {
			return rpc.NewError(rpc.CodeInvalidParams, "rename needs a from and a to",
				`each entry is {"from": "<current name>", "to": "<new name>"}`)
		}
	}
	for _, a := range edit.Add {
		if strings.TrimSpace(a.Name) == "" {
			return rpc.NewError(rpc.CodeInvalidParams, "add names an empty service",
				`each entry is {"name": "worker", "port": 9000}`)
		}
	}
	for _, e := range edit.Services {
		if strings.TrimSpace(e.Name) == "" {
			return rpc.NewError(rpc.CodeInvalidParams, "services names an empty service",
				`each entry is {"name": "<service>", "patch": {...}}; a null value clears a key`)
		}
	}
	return nil
}

func handleGroupsReload(_ context.Context, req *Request) (any, error) {
	loaded, bad := req.Runtime.Scanner.ReloadConfigs()
	problems := make([]rpc.ConfigProblem, 0, len(bad))
	for _, b := range bad {
		problems = append(problems, rpc.ConfigProblem{Path: b.Path, Error: b.Err.Error()})
	}
	req.Runtime.Logger.Info("reloaded .sonar.yaml files", "configs", loaded, "invalid", len(problems))
	republish(req.Runtime)
	return rpc.GroupsReloadResult{Loaded: loaded, Errors: problems}, nil
}

// resolveConfig finds the config a request names, by path or by group name.
// A path the index has not seen is loaded on the spot, so the desktop can open
// a project the daemon has never scanned a process in.
func resolveConfig(rt *Runtime, name, path *string) (*groups.Config, error) {
	if path != nil && strings.TrimSpace(*path) != "" {
		p := strings.TrimSpace(*path)
		if err := checkConfigPath(p); err != nil {
			return nil, err
		}
		if cfg, ok := rt.Scanner.ConfigAt(p); ok {
			return cfg, nil
		}
		if err := rt.Scanner.LoadConfig(p); err != nil {
			return nil, configError(p, err)
		}
		if cfg, ok := rt.Scanner.ConfigAt(p); ok {
			return cfg, nil
		}
		return nil, rpc.NewError(rpc.CodeNotFound, "no usable "+groups.ConfigName+" at "+p,
			"`sonar groups` lists the configs this daemon knows")
	}
	if name != nil && strings.TrimSpace(*name) != "" {
		n := strings.TrimSpace(*name)
		if cfg, ok := rt.Scanner.ConfigNamed(n); ok {
			return cfg, nil
		}
		return nil, rpc.NewError(rpc.CodeNotFound, "no group named "+n+" has a "+groups.ConfigName,
			"`sonar groups` lists the configs this daemon knows; `groups.reload` re-reads them")
	}
	return nil, rpc.NewError(rpc.CodeInvalidParams, "name or path is required",
		`send {"name": "my-app"} or {"path": "/repo/.sonar.yaml"}`)
}

// checkConfigPath refuses to read or write anything that is not a `.sonar.yaml`.
// The daemon writes files on a client's say-so, so the one filename it will
// touch is spelled out here rather than left to the caller.
func checkConfigPath(path string) error {
	base := filepath.Base(path)
	if base == groups.ConfigName || base == ".sonar.yml" {
		return nil
	}
	return rpc.NewError(rpc.CodeInvalidParams,
		path+" is not a "+groups.ConfigName,
		"sonar only reads and writes "+groups.ConfigName)
}

// configError maps a config failure onto the contract's error registry: a
// missing file or service is `not_found`, a name or a port an added or renamed
// service would take twice is `1011 conflict`, and a file that no longer
// validates is `1006 invalid_config` with the problems in data.detail
// (contract §13.2, step 5A.4).
func configError(path string, err error) error {
	var missing *groups.ServiceNotFoundError
	if errors.As(err, &missing) {
		return rpc.NewError(rpc.CodeNotFound, missing.Error(),
			"`groups.config.get` lists the service names this file declares")
	}
	var clash *groups.ServiceConflictError
	if errors.As(err, &clash) {
		hint := "pick another name, or rename the service that has it"
		if clash.Port != 0 {
			hint = "pick another port, or remove " + clash.Name + " in the same call"
		}
		return rpc.NewError(rpc.CodeConflict, clash.Error(), hint)
	}
	var bad *groups.ConfigError
	if errors.As(err, &bad) {
		return rpc.NewError(rpc.CodeInvalidConfig, bad.Error(),
			"the file was left as it was; fix the values and try again")
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return rpc.NewError(rpc.CodeNotFound, "no such file: "+path,
			"`sonar init` writes a "+groups.ConfigName+" at the repository root")
	}
	return rpc.NewError(rpc.CodeInternal, err.Error(), "check `sonar daemon log`")
}

// configRow renders a config for the wire. Running and port_actual are joined
// from the state the daemon already has, so an editor reading a config back
// sees the same service rows `groups.list` publishes.
func configRow(rt *Runtime, cfg *groups.Config) rpc.GroupConfig {
	out := rpc.GroupConfig{
		Name:     cfg.Name,
		Services: groups.ServiceRows(cfg),
		Ports:    append([]int{}, cfg.Ports...),
	}
	if out.Ports == nil {
		out.Ports = []int{}
	}
	if cfg.WorktreePorts != nil {
		// A copy, so the row never aliases the index's config.
		n := *cfg.WorktreePorts
		out.WorktreePorts = &n
	}
	live := liveServices(rt, cfg.Name)
	for i := range out.Services {
		if l, ok := live[out.Services[i].Name]; ok {
			out.Services[i].Running, out.Services[i].PortActual = l.Running, l.PortActual
		}
	}
	return out
}

// liveServices is the group's services as the last snapshot saw them.
func liveServices(rt *Runtime, name string) map[string]state.Service {
	out := map[string]state.Service{}
	for _, g := range rt.Scanner.Cached().Groups {
		if g.Name != name {
			continue
		}
		for _, svc := range g.Services {
			out[svc.Name] = svc
		}
	}
	return out
}
