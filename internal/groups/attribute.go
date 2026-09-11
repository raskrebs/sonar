package groups

import (
	"os"

	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/state"
)

// Attribute runs the resolver over a direct scan. It builds an index from what
// the scan saw — every `sonar.yaml` above a process cwd, every Compose
// project's working directory, plus the config in the caller's own working
// directory — resolves the group of every port, writes Group, GroupSource and
// ProjectRoot back onto the scanner rows, and returns the resolved contract
// rows together with the index.
//
// This is the no-daemon path: `sonar list`, `sonar groups` and `sonar init`
// call it. The daemon's scanner calls AttributeWith instead, with a long-lived
// index, the store's pins and the `sonar start` registry.
func Attribute(pp []ports.ListeningPort) ([]state.Port, *Index) {
	index := NewIndex()
	if wd, err := os.Getwd(); err == nil {
		index.Observe(wd)
	}
	return AttributeWith(pp, NoPins{}, PortRuns{}, index)
}

// AttributeWith is Attribute with the pin set, the run registry and the index
// supplied by the caller, so the daemon can keep one index for its lifetime and
// feed the resolver the pins it loaded from the store this tick. A nil index,
// pins or registry falls back to an empty one.
//
// Unlike Attribute it does not index the process's own working directory: the
// daemon's cwd says nothing about the ports it is watching.
func AttributeWith(pp []ports.ListeningPort, pins Pins, runs Registry, index *Index) ([]state.Port, *Index) {
	if index == nil {
		index = NewIndex()
	}
	if pins == nil {
		pins = NoPins{}
	}
	if runs == nil {
		runs = NoRuns{}
	}

	for i := range pp {
		index.AddComposeProject(pp[i].DockerComposeProject, pp[i].DockerComposeWorkingDir)
	}
	for i := range pp {
		index.Observe(pp[i].Cwd)
	}
	for i := range pp {
		if dir := index.ComposeDir(pp[i].DockerComposeProject); dir != "" {
			index.Observe(dir)
		}
	}

	stampRuns(pp, runs)

	resolved := Resolve(state.FromListeningAll(pp), pins, runs, index)
	stampSessions(resolved, runs)
	for i := range resolved {
		pp[i].ProjectRoot = deref(resolved[i].ProjectRoot)
		pp[i].Group = deref(resolved[i].Group)
		pp[i].GroupSource = ""
		if resolved[i].GroupSource != nil {
			pp[i].GroupSource = string(*resolved[i].GroupSource)
		}
	}
	return resolved, index
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// stampSessions puts the agent session of the owning run onto every port it
// owns, from the same registry and the same ancestry walk that decided the
// run. It runs after Resolve because it stamps the contract rows, not the
// scanner rows: `session` is a daemon concept with no place in `runs.json`, so
// the direct-scan path has nothing to stamp and this is a no-op there.
func stampSessions(resolved []state.Port, runs Registry) {
	reg, ok := runs.(SessionRegistry)
	if !ok {
		return
	}
	for i := range resolved {
		if resolved[i].PID <= 0 {
			continue
		}
		if s, found := reg.Session(resolved[i]); found && s.ID != "" {
			session := s
			resolved[i].Session = &session
		}
	}
}

// stampRuns writes the registry's run attribution onto the scan rows before
// they are converted, so `run`, `display_name` and `group` are all derived
// from the same lookup at the same instant.
//
// Without this the daemon read the run twice from two places: the scanner
// walked the mirrored runs.json while enriching (which is what fills `run` and
// the name `display_name` shows), and the resolver asked the live in-memory
// registry afterwards (which is what sets `group_source: start`). A run
// registered in between the two published a port with the group but a null
// run — the window is milliseconds wide, and on Linux the whole enrich pass is
// milliseconds long, so a `sonar start` racing the scan tick landed in it.
//
// PortRuns, the direct-scan registry, hands back what the row already carries,
// so this is a no-op without a daemon.
func stampRuns(pp []ports.ListeningPort, runs Registry) {
	if runs == nil {
		return
	}
	for i := range pp {
		if pp[i].PID <= 0 {
			continue
		}
		run, ok := runs.Run(state.FromListening(pp[i]))
		if !ok || (run.ID == "" && run.Group == "" && run.Name == "") {
			continue
		}
		pp[i].RunID, pp[i].RunGroup, pp[i].Tag, pp[i].RunRootPID = run.ID, run.Group, run.Name, run.RootPID
	}
}
