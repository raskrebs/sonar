package groups

import (
	"sort"
	"strings"

	"github.com/raskrebs/sonar/internal/state"
)

// Groups builds the group collection from resolved ports. Every group that at
// least one port resolves to appears, plus every valid `sonar.yaml` the index
// knows about — a project whose services are all down is still a group, it is
// just stopped.
//
// index may be nil, in which case groups carry no services and no config path.
func Groups(pp []state.Port, index *Index) []state.Group { return GroupsWith(pp, index, nil) }

// PortHints is the optional half of Registry that knows the port sonar started
// each service to bind — for a `port: auto` service, the one it assigned. The
// builder joins a service to that port even when the listener is not in the
// run's process tree: a `docker compose up` service is served by Docker, not
// by the command sonar started.
type PortHints interface {
	PortHint(group, service string) (int, bool)
}

// GroupsWith is Groups with the ports sonar assigned the services it started.
// hints may be nil.
func GroupsWith(pp []state.Port, index *Index, hints PortHints) []state.Group {
	if index == nil {
		index = NewIndex()
	}
	byName := map[string]*state.Group{}
	members := map[string][]state.Port{}

	order := func(name string) *state.Group {
		g, ok := byName[name]
		if !ok {
			g = &state.Group{Name: name, Source: state.SourceAuto, Members: []int{}, Services: []state.Service{}}
			byName[name] = g
		}
		return g
	}

	for _, p := range pp {
		if p.Group == nil || *p.Group == "" {
			continue
		}
		g := order(*p.Group)
		if p.GroupSource != nil && rank(*p.GroupSource) > rank(g.Source) {
			g.Source = *p.GroupSource
		}
		if !containsInt(g.Members, p.Port) {
			g.Members = append(g.Members, p.Port)
		}
		if g.RootDir == nil && p.ProjectRoot != nil {
			root := *p.ProjectRoot
			g.RootDir = &root
		}
		members[*p.Group] = append(members[*p.Group], p)
	}

	for _, cfg := range index.Configs() {
		// Named the way the resolver names the config's ports, so a stopped
		// worktree with a copy of the main checkout's file is `<repo>@<wt>`
		// here too, not a second group with the main checkout's name.
		name := index.GroupOf(cfg)
		g := order(name)
		path, dir := cfg.Path, cfg.Dir
		g.ConfigPath = &path
		g.RootDir = &dir
		if rank(state.SourceFile) > rank(g.Source) {
			g.Source = state.SourceFile
		}
		g.Services = services(cfg, name, members[name], hints)
	}

	out := make([]state.Group, 0, len(byName))
	for _, g := range byName {
		sort.Ints(g.Members)
		g.Status = status(*g)
		describeCheckout(g, index)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// describeCheckout fills in which project a group is a checkout of, which
// linked worktree it is, and the branch checked out there. A group named
// `<repo>@<worktree>` for the linked worktree it lives in is that worktree of
// repo; every other group is its own project.
func describeCheckout(g *state.Group, index *Index) {
	g.Repo, g.Worktree, g.Branch = g.Name, "", ""
	if g.RootDir == nil {
		return
	}
	co, ok := Locate(*g.RootDir)
	if !ok {
		return
	}
	g.Branch = index.Branch(co)
	if !co.Linked() {
		return
	}
	if repo, found := strings.CutSuffix(g.Name, "@"+co.Worktree); found && repo != "" {
		g.Repo, g.Worktree = repo, co.Worktree
	}
}

// ServiceRow adapts one `sonar.yaml` service to the published contract row,
// without any knowledge of what is running. `groups.config.get` returns the
// file through it, so what the editor reads back is exactly what the resolver
// publishes.
func ServiceRow(s Service) state.Service {
	svc := state.Service{
		Name:      s.Name,
		Cmd:       s.Cmd,
		Cwd:       s.Cwd,
		DependsOn: append([]string{}, s.DependsOn...),
	}
	if svc.DependsOn == nil {
		svc.DependsOn = []string{}
	}
	if s.Port != 0 {
		port := s.Port
		svc.Port = &port
	}
	svc.PortAuto = s.PortAuto
	svc.Health = optional(s.Health)
	svc.Description = optional(s.Description)
	svc.Icon = optional(s.Icon)
	svc.Color = optional(s.Color)
	return svc
}

// ServiceRows adapts a whole config's services list.
func ServiceRows(cfg *Config) []state.Service {
	out := make([]state.Service, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		out = append(out, ServiceRow(s))
	}
	return out
}

// optional turns an empty metadata string into the contract's null.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// services joins a config's declared services against the ports actually
// listening in that group: by declared port first, then by the port sonar
// assigned a `port: auto` service, then by the name the scanner shows for the
// port.
func services(cfg *Config, group string, member []state.Port, hints PortHints) []state.Service {
	out := make([]state.Service, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		svc := ServiceRow(s)
		assigned := 0
		if s.PortAuto && hints != nil {
			assigned, _ = hints.PortHint(group, s.Name)
		}
		for _, p := range member {
			if (s.Port != 0 && p.Port == s.Port) || (assigned != 0 && p.Port == assigned) || p.DisplayName == s.Name ||
				(p.Run != nil && p.Run.Name == s.Name) {
				actual := p.Port
				svc.Running, svc.PortActual = true, &actual
				break
			}
		}
		out = append(out, svc)
	}
	return out
}

// status is running when everything the group declares is up, stopped when
// nothing is, and partial in between. A group with no services is running as
// long as it has a listening port.
func status(g state.Group) string {
	if len(g.Services) == 0 {
		if len(g.Members) > 0 {
			return "running"
		}
		return "stopped"
	}
	up := 0
	for _, s := range g.Services {
		if s.Running {
			up++
		}
	}
	switch {
	case up == len(g.Services):
		return "running"
	case up == 0:
		if len(g.Members) > 0 {
			return "partial"
		}
		return "stopped"
	default:
		return "partial"
	}
}

// rank orders group sources by precedence so a group takes the strongest
// source any of its members resolved with.
func rank(s state.GroupSource) int {
	switch s {
	case state.SourceManual:
		return 3
	case state.SourceStart:
		return 2
	case state.SourceFile:
		return 1
	default:
		return 0
	}
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
