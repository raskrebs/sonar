package daemon

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/state"
)

// `groups.rename` (step 5A.6) renames a project and, with it, every checkout of
// it: the main checkout's group and each linked worktree's `<project>@<wt>`.
// The name lives where the project already keeps it — the `name:` of the main
// checkout's `sonar.yaml` — or, for a project with no file, in the daemon's
// store, keyed by the main checkout's root.
func init() {
	RegisterHandler("groups.rename", handleGroupsRename)
}

var (
	renameHooksMu sync.RWMutex
	renameHooks   []func(renames map[string]string)
)

// OnGroupRename registers a function that is told, old name → new, about every
// group a `groups.rename` renamed. State that recorded a group name when it was
// created follows the rename through it — a run's group does, from
// internal/daemon/runsreg — without this package importing its owner
// (contract §8).
func OnGroupRename(f func(renames map[string]string)) {
	if f == nil {
		return
	}
	renameHooksMu.Lock()
	renameHooks = append(renameHooks, f)
	renameHooksMu.Unlock()
}

func notifyGroupRename(renames map[string]string) {
	if len(renames) == 0 {
		return
	}
	renameHooksMu.RLock()
	hooks := append([]func(map[string]string){}, renameHooks...)
	renameHooksMu.RUnlock()
	for _, f := range hooks {
		f(renames)
	}
}

func handleGroupsRename(_ context.Context, req *Request) (any, error) {
	var p rpc.GroupsRenameParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	from, to := strings.TrimSpace(p.Name), strings.TrimSpace(p.To)
	if from == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "name is required",
			`send {"name": "<project or checkout group>", "to": "<new project name>"}`)
	}
	if err := groups.ValidProjectName(to); err != nil {
		return nil, rpc.NewError(rpc.CodeInvalidParams, err.Error(),
			"a project name is one word with no @, / or whitespace; a worktree's group adds @<worktree> to it")
	}

	rt := req.Runtime
	snap, err := rt.Scanner.Snapshot(scanner.Include{})
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternal, "scan failed: "+err.Error(),
			"check `sonar daemon log` for the scanner error")
	}
	local := make([]state.Group, 0, len(snap.Groups))
	for _, g := range snap.Groups {
		if state.IsLocalhost(g.Host) {
			local = append(local, g)
		}
	}
	plan, err := rt.Scanner.PlanGroupRename(local, from, to)
	if err != nil {
		return nil, renameError(err)
	}
	if len(plan.Renames) == 0 {
		// Already called that: nothing to write and nothing to publish.
		return rpc.GroupsRenameResult{
			MutationResult: rpc.MutationResult{OK: true, Affected: []string{}},
			Name:           plan.Project,
		}, nil
	}

	if plan.ConfigPath != "" {
		cfg, err := groups.SetConfigName(plan.ConfigPath, to)
		if err != nil {
			return nil, configError(plan.ConfigPath, err)
		}
		if err := rt.Scanner.LoadConfig(cfg.Path); err != nil {
			rt.Logger.Warn("reloading a config after renaming its project", "path", cfg.Path, "error", err)
		}
		// The file names the project from now on. An alias stored before the
		// file existed would still outrank it, so it goes.
		if st := rt.Store; st != nil && plan.Main != "" {
			if err := st.ClearGroupAlias(plan.Main); err != nil {
				rt.Logger.Warn("clearing a project alias", "root", plan.Main, "error", err)
			}
		}
	} else {
		st := rt.Store
		if st == nil {
			return nil, errNoStore()
		}
		if err := st.SetGroupAlias(plan.Main, to); err != nil {
			return nil, storeError("saving the project name", err)
		}
	}

	notifyGroupRename(plan.Renames)
	rt.Logger.Info("project renamed", "from", from, "to", to, "groups", len(plan.Renames))
	// Publish before replying, like every other write: the delta that removes
	// the old names and adds the new ones is queued ahead of this response.
	republish(rt)

	affected := make([]string, 0, len(plan.Renames))
	for _, next := range plan.Renames {
		affected = append(affected, next)
	}
	sort.Strings(affected)
	return rpc.GroupsRenameResult{
		MutationResult: rpc.MutationResult{OK: true, Affected: affected},
		Name:           plan.Project,
	}, nil
}

// renameError maps a refused rename onto the contract's error registry.
func renameError(err error) error {
	var re *groups.RenameError
	if !errors.As(err, &re) {
		return rpc.NewError(rpc.CodeInternal, err.Error(), "check `sonar daemon log`")
	}
	switch re.Kind {
	case groups.RenameNotFound:
		return rpc.NewError(rpc.CodeNotFound, re.Msg, re.Hint)
	case groups.RenameConflict:
		return rpc.NewError(rpc.CodeConflict, re.Msg, re.Hint)
	default:
		return rpc.NewError(rpc.CodeInvalidParams, re.Msg, re.Hint)
	}
}
