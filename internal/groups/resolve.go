package groups

import (
	"fmt"

	"github.com/raskrebs/sonar/internal/state"
)

// Pins supplies manual group assignments (`sonar assign`), the top of the
// precedence chain. The store step (1A.4) provides the persistent
// implementation; NoPins stands in until then.
type Pins interface {
	// Group returns the pinned group for a port, matched against the port's
	// match keys (see MatchKeys).
	Group(p state.Port) (string, bool)
}

// NoPins is the empty pin set.
type NoPins struct{}

// Group never matches.
func (NoPins) Group(state.Port) (string, bool) { return "", false }

// Registry attributes a port to a process sonar started. It answers with the
// whole run, not just its group, because `run`, `display_name` and
// `group_source: start` all have to come out of one lookup: a daemon that
// resolved the group from its live registry but the run from the runs.json
// mirror could publish `group_source: "start"` next to a null `run`.
type Registry interface {
	// Run returns the run that owns this port.
	Run(p state.Port) (run state.Run, ok bool)
}

// SessionRegistry is the optional half of Registry: a registry that also knows
// which agent session started a run reports it here, and AttributeWith stamps
// it onto the port (spec 2 §3, contract §5). A registry that does not
// implement it simply publishes ports with a null session.
type SessionRegistry interface {
	Session(p state.Port) (session state.Session, ok bool)
}

// PortRuns reads the run attribution the scanner put on the port itself. It is
// the direct-scan path: no daemon, so `runs.json` is the only registry there is.
type PortRuns struct{}

// Run returns the run the scanner already attributed. Until `sonar start`
// records a group (step 1A.5, contract §11.3) run.group is empty, so this
// reports ok with only the name; the resolver then falls through to the file
// and git-root rules.
func (PortRuns) Run(p state.Port) (state.Run, bool) {
	if p.Run == nil {
		return state.Run{}, false
	}
	return *p.Run, true
}

// NoRuns is the empty run registry.
type NoRuns struct{}

// Run never matches.
func (NoRuns) Run(state.Port) (state.Run, bool) { return state.Run{}, false }

// MatchKeys returns the keys a rename or a pin can be stored under for this
// port, most stable first. The store step matches a stored key against all of
// them on every scan so a label survives a restart or a new PID.
func MatchKeys(p state.Port) []string {
	var keys []string
	if p.Run != nil && p.Run.Name != "" {
		group := p.Run.Group
		if group == "" {
			group = "-"
		}
		keys = append(keys, fmt.Sprintf("run:%s/%s", group, p.Run.Name))
	}
	if p.Docker != nil {
		if p.Docker.ComposeProject != "" && p.Docker.ComposeService != "" {
			keys = append(keys, fmt.Sprintf("docker:%s/%s", p.Docker.ComposeProject, p.Docker.ComposeService))
		} else if p.Docker.Container != "" {
			keys = append(keys, "docker:"+p.Docker.Container)
		}
	}
	if p.ProjectRoot != nil && *p.ProjectRoot != "" {
		keys = append(keys, fmt.Sprintf("cwd:%s:%d", *p.ProjectRoot, p.Port))
	}
	keys = append(keys, fmt.Sprintf("port:%d", p.Port))
	return keys
}

// Resolve fills Group, GroupSource and ProjectRoot on a copy of pp, applying
// the precedence chain from the daemon spec, first match wins:
//
//  1. manual — a pin from `sonar assign`
//  2. start  — a `sonar start` run that owns the process
//  3. file   — a known `sonar.yaml` that claims the port
//  4. compose — the Compose project, unless its working directory is inside a
//     git checkout, in which case the container merges into that checkout's
//     group so a Compose db and a native api are one group
//  5. gitroot — the checkout containing the process cwd, named `<project>` or
//     `<project>@<worktree>`, where the project name comes from the main
//     checkout only: an alias from `groups.rename`, else the `name:` of its
//     `sonar.yaml`, else its directory name. A `sonar.yaml` at the
//     checkout's root makes the source `file`; a linked worktree's own copy
//     supplies services but never the name (step 5A.6)
//  6. none — group stays null
//
// A run's group is kept, except that a run recorded under the project's own
// name is moved to its checkout's current group name (see runGroup), and a
// config never claims a port across a linked worktree's boundary.
//
// pins, runs and index may all be nil.
func Resolve(pp []state.Port, pins Pins, runs Registry, index *Index) []state.Port {
	out := make([]state.Port, len(pp))
	copy(out, pp)
	if index == nil {
		index = NewIndex()
	}
	for i := range out {
		resolveOne(&out[i], pins, runs, index)
	}
	return out
}

func resolveOne(p *state.Port, pins Pins, runs Registry, index *Index) {
	co, inRepo := projectCheckout(p, index)
	if inRepo {
		r := co.Root
		p.ProjectRoot = &r
	}
	p.Group, p.GroupSource = nil, nil

	if pins != nil {
		if g, ok := pins.Group(*p); ok && g != "" {
			assign(p, g, state.SourceManual)
			return
		}
	}
	if runs != nil {
		if run, ok := runs.Run(*p); ok && run.Group != "" {
			group := run.Group
			if inRepo {
				group = index.runGroup(group, co)
			}
			assign(p, group, state.SourceStart)
			return
		}
	}
	// The deepest config claiming the port wins. If that one lies outside the
	// linked worktree the port runs in, none inside it claims the port — they
	// would be deeper — so the checkout's own group takes it below.
	if cfg, _, ok := index.MatchPort(*p); ok && (!inRepo || index.within(cfg, co)) {
		assign(p, index.GroupOf(cfg), state.SourceFile)
		return
	}
	if inRepo {
		// Name first: naming probes the main checkout, which is where a
		// main-checkout port's own config may still be waiting to be read.
		name := index.CheckoutName(co)
		source := state.SourceAuto
		if index.checkoutConfig(co) != nil {
			source = state.SourceFile
		}
		assign(p, name, source)
		return
	}
	if p.Docker != nil && p.Docker.ComposeProject != "" {
		assign(p, p.Docker.ComposeProject, state.SourceAuto)
	}
}

// projectCheckout is the checkout a port belongs to: the one containing the
// process cwd, or — for a Compose container, which has no cwd of its own — the
// one containing the project's working directory.
func projectCheckout(p *state.Port, index *Index) (Checkout, bool) {
	if p.Cwd != "" {
		if co, ok := Locate(p.Cwd); ok {
			return co, true
		}
	}
	if p.Docker != nil && p.Docker.ComposeProject != "" {
		if dir := index.ComposeDir(p.Docker.ComposeProject); dir != "" {
			if co, ok := Locate(dir); ok {
				return co, true
			}
		}
	}
	return Checkout{}, false
}

func assign(p *state.Port, name string, source state.GroupSource) {
	if name == "" {
		return
	}
	n, s := name, source
	p.Group, p.GroupSource = &n, &s
}
