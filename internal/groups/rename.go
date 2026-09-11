package groups

import (
	"fmt"
	"strings"

	"github.com/raskrebs/sonar/internal/state"
)

// RenamePlan is what renaming a project changes (`groups.rename`, step 5A.6).
//
// A rename always applies to the project, whichever of its checkouts named it:
// the main checkout's group becomes Project and every linked worktree's group
// becomes `<Project>@<worktree>`.
type RenamePlan struct {
	// Main is the main checkout's root, the key an alias is stored under. It is
	// empty for a `.sonar.yaml` project that is not a checkout's root.
	Main string
	// ConfigPath is the `.sonar.yaml` to write the new `name:` into. Empty
	// means the project has no file of its own and the name is stored as an
	// alias of Main instead.
	ConfigPath string
	// Project is the new project name.
	Project string
	// Renames maps every group name that changes to its new name. It is empty
	// when the project already has the name asked for.
	Renames map[string]string
}

// RenameErrorKind says why a rename cannot be done.
type RenameErrorKind int

const (
	// RenameNotFound: no group has the name the caller gave.
	RenameNotFound RenameErrorKind = iota
	// RenameRefused: the group exists but its name is not the project's to
	// change — a pin or a run's --group names it, or nothing does.
	RenameRefused
	// RenameConflict: a group outside the project already has a name the
	// rename would give.
	RenameConflict
)

// RenameError is a rename that cannot be done.
type RenameError struct {
	Kind RenameErrorKind
	Msg  string
	Hint string
}

func (e *RenameError) Error() string { return e.Msg }

// ValidProjectName checks a new project name. On top of the rules every group
// name follows it refuses `@`, which separates the project from the worktree
// in a checkout's group name.
func ValidProjectName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("the new name is empty")
	case strings.ContainsAny(name, "@"):
		return fmt.Errorf("%q contains @, which separates a project from its worktree", name)
	case strings.ContainsAny(name, "/\\ \t\n\r"):
		return fmt.Errorf("%q contains a slash or whitespace; names are a single word", name)
	}
	return nil
}

// PlanRename works out what renaming the group from to `to` means, against
// the published groups gg and the index they were built from. It changes
// nothing: the caller writes the file or the alias and republishes.
func PlanRename(gg []state.Group, x *Index, from, to string) (*RenamePlan, error) {
	var g *state.Group
	for i := range gg {
		if gg[i].Name == from {
			g = &gg[i]
			break
		}
	}
	if g == nil {
		return nil, &RenameError{Kind: RenameNotFound,
			Msg: "no group named " + from, Hint: "`sonar groups` lists the names"}
	}
	if g.Source == state.SourceManual {
		return nil, refusedManual(from)
	}

	var (
		co     Checkout
		inRepo bool
	)
	if g.RootDir != nil {
		co, inRepo = Locate(*g.RootDir)
	}
	if inRepo && g.Name == x.CheckoutName(co) {
		return planProject(gg, x, co.Main, to)
	}

	// Not a checkout's own group: whatever named it is not the project.
	if g.Source == state.SourceStart {
		return nil, &RenameError{Kind: RenameRefused,
			Msg:  from + " is named by the --group of the `sonar start` run in it, not by a project",
			Hint: "restart the run with `sonar start --group " + to + "`"}
	}
	if g.ConfigPath != nil && !(inRepo && co.Linked()) {
		if cfg, ok := x.ByPath(*g.ConfigPath); ok && x.GroupOf(cfg) == g.Name {
			// A `.sonar.yaml` project that is not a checkout's root: nested in
			// one, or outside git altogether. Its file is its name.
			plan := &RenamePlan{ConfigPath: cfg.Path, Project: to, Renames: map[string]string{}}
			if from != to {
				plan.Renames[from] = to
			}
			return plan, conflicts(gg, plan)
		}
	}
	if inRepo && co.Linked() {
		return nil, &RenameError{Kind: RenameRefused,
			Msg:  from + " is a project nested in the worktree " + co.Worktree,
			Hint: "rename it in the main checkout's copy of its " + ConfigName}
	}
	return nil, &RenameError{Kind: RenameRefused,
		Msg:  from + " is not a git checkout or a " + ConfigName + " project, so it has no name to change",
		Hint: "a Compose project is named by Compose; `sonar assign` pins ports to a group of your choosing"}
}

// planProject renames the project whose main checkout is at main, and with it
// every checkout group of that project that is published right now.
func planProject(gg []state.Group, x *Index, main, to string) (*RenamePlan, error) {
	plan := &RenamePlan{Main: main, Project: to, Renames: map[string]string{}}
	if cfg := x.At(main); cfg != nil {
		plan.ConfigPath = cfg.Path
	}
	if x.ProjectName(main) == to {
		return plan, nil
	}
	for _, h := range gg {
		if h.RootDir == nil {
			continue
		}
		hc, ok := Locate(*h.RootDir)
		if !ok || hc.Main != main || h.Name != x.CheckoutName(hc) {
			continue
		}
		if h.Source == state.SourceManual {
			return nil, refusedManual(h.Name)
		}
		next := to
		if hc.Linked() {
			next = to + "@" + hc.Worktree
		}
		plan.Renames[h.Name] = next
	}
	return plan, conflicts(gg, plan)
}

// conflicts refuses a rename that would give a group a name some group outside
// the rename already has. The project name itself is checked even when the
// main checkout is not published right now, because it will be.
func conflicts(gg []state.Group, plan *RenamePlan) error {
	taken := map[string]bool{}
	for _, h := range gg {
		if _, moving := plan.Renames[h.Name]; !moving {
			taken[h.Name] = true
		}
	}
	names := []string{plan.Project}
	for _, next := range plan.Renames {
		names = append(names, next)
	}
	for _, n := range names {
		if taken[n] {
			return &RenameError{Kind: RenameConflict,
				Msg: "a group named " + n + " already exists", Hint: "pick another name"}
		}
	}
	return nil
}

func refusedManual(name string) error {
	return &RenameError{Kind: RenameRefused,
		Msg:  name + " is a manual group: ports pinned with `sonar assign` name it",
		Hint: "pin them to the new name with `sonar assign <port> <name>` instead"}
}
