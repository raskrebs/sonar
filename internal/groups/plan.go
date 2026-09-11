package groups

import (
	"fmt"
	"sort"
	"strings"
)

// Step is one service in a start plan, with the dependencies `groups.start`
// has to see listening before it spawns this one. Waits only names
// dependencies that declare a port: waiting on a service with no port would be
// waiting on nothing.
type Step struct {
	Service Service
	Waits   []Service
}

// UnknownServiceError names an `only` entry that is not in the file. It maps
// onto the contract's `not_found`.
type UnknownServiceError struct {
	Path  string
	Name  string
	Known []string
}

func (e *UnknownServiceError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("no service %q in %s", e.Name, e.Path)
	}
	return fmt.Sprintf("no service %q in %s (it declares %s)",
		e.Name, e.Path, strings.Join(e.Known, ", "))
}

// Plan orders a config's services for `sonar up`: dependencies first, then
// everything that depends on them, with services at the same depth kept in the
// order the file lists them so a plan reads like the file.
//
// only restricts the plan to the named services; an unknown name is an error
// rather than a silent no-op, because "sonar up --only ap" should say so. A
// service kept by `only` still waits for the dependencies it declares — they
// may already be running, which is the common case, and `groups.start` skips
// what is up.
//
// Load has already rejected cycles and unknown depends_on, so the sort here
// cannot loop; a config built by hand that still contains one degrades to file
// order rather than dropping services.
func Plan(cfg *Config, only []string) ([]Step, error) {
	if cfg == nil {
		return nil, nil
	}
	byName := make(map[string]Service, len(cfg.Services))
	order := make(map[string]int, len(cfg.Services))
	for i, s := range cfg.Services {
		byName[s.Name] = s
		order[s.Name] = i
	}

	keep, err := selection(cfg, byName, only)
	if err != nil {
		return nil, err
	}

	sorted := topoSort(cfg, order)

	steps := make([]Step, 0, len(keep))
	for _, s := range sorted {
		if !keep[s.Name] {
			continue
		}
		step := Step{Service: s}
		for _, dep := range s.DependsOn {
			d, ok := byName[dep]
			if !ok || !d.HasPort() {
				continue
			}
			step.Waits = append(step.Waits, d)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// selection is the set of services the plan keeps.
func selection(cfg *Config, byName map[string]Service, only []string) (map[string]bool, error) {
	keep := make(map[string]bool, len(cfg.Services))
	if len(only) == 0 {
		for _, s := range cfg.Services {
			keep[s.Name] = true
		}
		return keep, nil
	}
	for _, name := range only {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := byName[name]; !ok {
			return nil, &UnknownServiceError{Path: cfg.Path, Name: name, Known: names(cfg)}
		}
		keep[name] = true
	}
	return keep, nil
}

// topoSort returns the services in dependency order, breaking ties by the
// order the file lists them.
func topoSort(cfg *Config, order map[string]int) []Service {
	state := map[string]int{} // 0 unvisited, 1 in progress, 2 done
	out := make([]Service, 0, len(cfg.Services))

	byName := make(map[string]Service, len(cfg.Services))
	for _, s := range cfg.Services {
		byName[s.Name] = s
	}

	var visit func(Service)
	visit = func(s Service) {
		switch state[s.Name] {
		case 1, 2:
			// Already emitted, or an edge back into the branch we are in: a
			// cycle Load would have rejected. Either way, do not recurse.
			return
		}
		state[s.Name] = 1
		deps := append([]string{}, s.DependsOn...)
		sort.SliceStable(deps, func(i, j int) bool { return order[deps[i]] < order[deps[j]] })
		for _, dep := range deps {
			if d, ok := byName[dep]; ok {
				visit(d)
			}
		}
		state[s.Name] = 2
		out = append(out, s)
	}

	for _, s := range cfg.Services {
		visit(s)
	}
	return out
}

// names lists a config's service names in file order.
func names(cfg *Config) []string {
	out := make([]string, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		out = append(out, s.Name)
	}
	return out
}
