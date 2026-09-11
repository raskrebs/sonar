package groups

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigName is the file a project commits at its repository root to name its
// group and describe its services.
const ConfigName = ".sonar.yaml"

// altConfigName is accepted on read so a project that spells the extension the
// other way is not silently ignored. `sonar init` always writes ConfigName.
const altConfigName = ".sonar.yml"

// Service is one entry of a config's `services:` list. Description, Icon and
// Color are user-authored metadata the daemon never infers and never
// interprets: it carries them onto state.Service so a client can render them
// (contract §13.1).
type Service struct {
	Name        string   `yaml:"name"`
	Cmd         string   `yaml:"cmd,omitempty"`
	Cwd         string   `yaml:"cwd,omitempty"`
	Port        int      `yaml:"port,omitempty"`
	Health      string   `yaml:"health,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Icon        string   `yaml:"icon,omitempty"`
	Color       string   `yaml:"color,omitempty"`
	DependsOn   []string `yaml:"depends_on,omitempty"`
}

// MaxWorktreePorts is the largest block `worktree_ports` may ask for. It is
// also the cap on a claim's explicit count, so a config can never ask for a
// block the claims manager would refuse.
const MaxWorktreePorts = 100

// FieldWorktreePorts is the top-level key naming the block size a worktree
// claims.
const FieldWorktreePorts = "worktree_ports"

// Config is a parsed `.sonar.yaml`. Path and Dir are filled by Load and are
// not part of the file format.
type Config struct {
	Name     string    `yaml:"name"`
	Services []Service `yaml:"services,omitempty"`
	Ports    []int     `yaml:"ports,omitempty"`
	// WorktreePorts is how many ports a claim for this project takes when the
	// caller does not name a count (step 5A.7). Nil means the key is absent
	// and the claims default applies.
	WorktreePorts *int `yaml:"worktree_ports,omitempty"`

	Path string `yaml:"-"` // absolute path of the file it was read from
	Dir  string `yaml:"-"` // directory containing the file
}

// ConfigError reports every problem found in one file at once, so a user fixing
// a config sees the whole list instead of one error per run.
type ConfigError struct {
	Path     string
	Problems []string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, strings.Join(e.Problems, "; "))
}

// Load reads and validates a `.sonar.yaml`. The returned error is a
// *ConfigError listing every validation problem; callers report it and carry
// on, because an invalid config must never be fatal to a scan.
func Load(path string) (*Config, error) {
	// Canonical once, here: Path and Dir are the keys the index is built on,
	// so a config read through a symlinked path must land under the same key
	// as the same config found by walking a process's cwd.
	abs := Canonical(path)
	if abs == "" {
		return nil, fmt.Errorf("no config path given")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return parse(abs, data)
}

// Parse validates config bytes as if they had been read from path. It is what
// a caller rendering a config in memory — `groups.init` writing a curated
// proposal — checks before putting anything on disk.
func Parse(path string, data []byte) (*Config, error) {
	abs := Canonical(path)
	if abs == "" {
		return nil, fmt.Errorf("no config path given")
	}
	return parse(abs, data)
}

func parse(abs string, data []byte) (*Config, error) {
	dir := filepath.Dir(abs)

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &ConfigError{Path: abs, Problems: []string{err.Error()}}
	}
	if err := rejectExpose(&doc); err != nil {
		return nil, &ConfigError{Path: abs, Problems: []string{err.Error()}}
	}

	cfg := &Config{Path: abs, Dir: dir}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, &ConfigError{Path: abs, Problems: []string{err.Error()}}
	}
	cfg.Path, cfg.Dir = abs, dir
	if cfg.Name == "" {
		cfg.Name = filepath.Base(dir)
	}

	if problems := cfg.validate(); len(problems) > 0 {
		return nil, &ConfigError{Path: abs, Problems: problems}
	}
	return cfg, nil
}

// validate implements the rules from the daemon spec: a usable group name,
// unique service names, cwd confined to the file's directory, ports in range
// and an acyclic depends_on graph.
func (c *Config) validate() []string {
	var problems []string

	if c.Name == "" {
		problems = append(problems, "name is empty")
	} else if i := strings.IndexAny(c.Name, "/\\ \t\n"); i >= 0 {
		problems = append(problems, fmt.Sprintf("name %q contains %q; names may not contain slashes or whitespace", c.Name, c.Name[i:i+1]))
	}

	for _, p := range c.Ports {
		if p < 1 || p > 65535 {
			problems = append(problems, fmt.Sprintf("ports: %d is out of range 1-65535", p))
		}
	}
	if n := c.WorktreePorts; n != nil && (*n < 1 || *n > MaxWorktreePorts) {
		problems = append(problems, fmt.Sprintf("%s: %d is out of range 1-%d", FieldWorktreePorts, *n, MaxWorktreePorts))
	}

	seen := map[string]bool{}
	for i, s := range c.Services {
		where := fmt.Sprintf("services[%d]", i)
		if s.Name == "" {
			problems = append(problems, where+": name is empty")
		} else {
			where = "service " + s.Name
			if seen[s.Name] {
				problems = append(problems, fmt.Sprintf("service %s: duplicate name", s.Name))
			}
			seen[s.Name] = true
		}
		if s.Port != 0 && (s.Port < 1 || s.Port > 65535) {
			problems = append(problems, fmt.Sprintf("%s: port %d is out of range 1-65535", where, s.Port))
		}
		if s.Cwd != "" && !c.cwdInside(s.Cwd) {
			problems = append(problems, fmt.Sprintf("%s: cwd %q escapes the directory holding %s", where, s.Cwd, ConfigName))
		}
	}

	for _, s := range c.Services {
		for _, dep := range s.DependsOn {
			if !seen[dep] {
				problems = append(problems, fmt.Sprintf("service %s: depends_on %q is not a service in this file", s.Name, dep))
			}
		}
	}
	if cycle := c.findCycle(); len(cycle) > 0 {
		problems = append(problems, "depends_on has a cycle: "+strings.Join(cycle, " -> "))
	}

	sort.Strings(problems)
	return problems
}

// cwdInside reports whether a service cwd stays inside the config's directory.
// Absolute paths are rejected outright: the field is documented as relative to
// the file.
func (c *Config) cwdInside(cwd string) bool {
	if filepath.IsAbs(cwd) || strings.HasPrefix(cwd, "/") || strings.HasPrefix(cwd, `\`) {
		return false
	}
	rel := filepath.Clean(filepath.FromSlash(cwd))
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// ServiceDir returns the absolute working directory for a service.
func (c *Config) ServiceDir(s Service) string {
	if s.Cwd == "" {
		return c.Dir
	}
	return filepath.Join(c.Dir, filepath.FromSlash(s.Cwd))
}

// findCycle returns one depends_on cycle as a readable chain, or nil.
func (c *Config) findCycle() []string {
	deps := map[string][]string{}
	for _, s := range c.Services {
		deps[s.Name] = append(deps[s.Name], s.DependsOn...)
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var walk func(string) []string
	walk = func(n string) []string {
		color[n] = grey
		stack = append(stack, n)
		for _, d := range deps[n] {
			switch color[d] {
			case grey:
				// Cut the stack down to the repeated node.
				for i, s := range stack {
					if s == d {
						return append(append([]string{}, stack[i:]...), d)
					}
				}
				return []string{d, d}
			case white:
				if cyc := walk(d); cyc != nil {
					return cyc
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return nil
	}
	names := make([]string, 0, len(deps))
	for n := range deps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if color[n] == white {
			if cyc := walk(n); cyc != nil {
				return cyc
			}
		}
	}
	return nil
}

// rejectExpose fails a config that carries an `expose:` key. Expose is not part
// of `.sonar.yaml`; saying so explicitly is friendlier than ignoring a key the
// author expects to do something.
func rejectExpose(doc *yaml.Node) error {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	if hasKey(root, "expose") {
		return exposeError("")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "services" {
			continue
		}
		list := root.Content[i+1]
		if list.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range list.Content {
			if item.Kind == yaml.MappingNode && hasKey(item, "expose") {
				return exposeError(mappingName(item))
			}
		}
	}
	return nil
}

func exposeError(service string) error {
	where := ConfigName
	if service != "" {
		where = "service " + service
	}
	return fmt.Errorf("%s has an `expose:` key: exposing a port is not configured in %s. "+
		"Expose is created at runtime and is not part of this file", where, ConfigName)
}

func hasKey(mapping *yaml.Node, key string) bool {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return true
		}
	}
	return false
}

func mappingName(mapping *yaml.Node) string {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "name" {
			return mapping.Content[i+1].Value
		}
	}
	return ""
}
