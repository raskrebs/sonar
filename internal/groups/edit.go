package groups

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
)

// rootKeyOrder is where a top-level key belongs in a `.sonar.yaml`. It is the
// same idea as keyOrder one level up: a `services:` list this package has to
// create lands where a hand-written file would put it.
var rootKeyOrder = []string{"name", "services", "ports", FieldWorktreePorts}

// IntChange is an edit to one optional integer key. The zero value leaves the
// key alone; Set with a nil Value removes it; Set with a Value writes it. It
// is how the wire's three states — absent, null, a number — reach the file.
type IntChange struct {
	Set   bool
	Value *int
}

// SetInt is the change that writes n.
func SetInt(n int) IntChange { return IntChange{Set: true, Value: &n} }

// ClearInt is the change that removes the key.
func ClearInt() IntChange { return IntChange{Set: true} }

// ServiceAdd is a service to append to a `.sonar.yaml`. It is the whole
// editable service, not a patch: an added service does not exist yet, so
// "absent" and "null" mean the same thing and the zero value of every field is
// simply left out of the file.
type ServiceAdd struct {
	Name        string   `json:"name"`
	Port        int      `json:"port,omitempty"`
	Cmd         string   `json:"cmd,omitempty"`
	Cwd         string   `json:"cwd,omitempty"`
	Health      string   `json:"health,omitempty"`
	Description string   `json:"description,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Color       string   `json:"color,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

// Service is the config entry the add describes.
func (a ServiceAdd) Service() Service {
	return Service{
		Name:        a.Name,
		Cmd:         a.Cmd,
		Cwd:         a.Cwd,
		Port:        a.Port,
		Health:      a.Health,
		Description: a.Description,
		Icon:        a.Icon,
		Color:       a.Color,
		DependsOn:   append([]string{}, a.DependsOn...),
	}
}

// AddFrom is the add entry that would recreate a service.
func AddFrom(s Service) ServiceAdd {
	return ServiceAdd{
		Name:        s.Name,
		Port:        s.Port,
		Cmd:         s.Cmd,
		Cwd:         s.Cwd,
		Health:      s.Health,
		Description: s.Description,
		Icon:        s.Icon,
		Color:       s.Color,
		DependsOn:   append([]string{}, s.DependsOn...),
	}
}

// AddsFrom is AddFrom over a whole config, which is how a proposal becomes the
// list of services to merge into a file that already exists.
func AddsFrom(cfg *Config) []ServiceAdd {
	out := make([]ServiceAdd, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		out = append(out, AddFrom(s))
	}
	return out
}

// ServiceRename renames a service. Every depends_on entry naming From is
// rewritten too, so the file stays valid across the rename.
type ServiceRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ConfigEdit is one atomic set of changes to a `.sonar.yaml`. The four lists
// are applied in the order the fields are declared — remove, rename, add, then
// the metadata patches — and each one sees the file as the previous ones left
// it. Nothing is written unless every step succeeds and the result still
// parses, so a failed edit leaves the file byte-identical.
//
// Removing first is what makes `{"remove": ["old"], "add": [{"name": "new",
// "port": 8000}]}` work when the two want the same port: the port is free by
// the time the add is checked.
type ConfigEdit struct {
	Remove   []string        `json:"remove,omitempty"`
	Rename   []ServiceRename `json:"rename,omitempty"`
	Add      []ServiceAdd    `json:"add,omitempty"`
	Services []ServiceEdit   `json:"services,omitempty"`
	// WorktreePorts sets or removes the top-level worktree_ports key. It is
	// applied last and touches no service, so it never shows in Affected.
	WorktreePorts IntChange `json:"-"`
}

// Empty reports whether the edit asks for nothing at all.
func (e ConfigEdit) Empty() bool {
	return len(e.Remove) == 0 && len(e.Rename) == 0 && len(e.Add) == 0 && len(e.Services) == 0 &&
		!e.WorktreePorts.Set
}

// Affected is the service names the edit touched, in the order it applied
// them: a removed name, a rename's new name, an added name, a patched name.
// It is what a mutating method reports as `affected` (contract §22).
func (e ConfigEdit) Affected() []string {
	out := make([]string, 0, len(e.Remove)+len(e.Rename)+len(e.Add)+len(e.Services))
	for _, name := range e.Remove {
		out = append(out, name)
	}
	for _, r := range e.Rename {
		out = append(out, r.To)
	}
	for _, a := range e.Add {
		out = append(out, a.Name)
	}
	for _, s := range e.Services {
		out = append(out, s.Name)
	}
	return out
}

// ServiceConflictError is returned when an add or a rename would give a file
// two services with the same name, or two services claiming the same port. It
// maps onto the contract's `conflict`.
type ServiceConflictError struct {
	Path string
	Name string // the service already holding the name or the port
	Port int    // non-zero when the clash is a port rather than a name
}

func (e *ServiceConflictError) Error() string {
	if e.Port != 0 {
		return fmt.Sprintf("port %d is already service %q in %s", e.Port, e.Name, e.Path)
	}
	return fmt.Sprintf("service %q already exists in %s", e.Name, e.Path)
}

// RenderEdit applies an edit to the `.sonar.yaml` at path and returns the bytes
// it would write, without writing them. The rewrite goes through the YAML node
// API rather than marshalling a struct, so comments, key order and the
// author's own formatting survive (contract §13.2).
//
// The returned Config is the file as it would then stand: it is parsed from the
// rendered bytes, so what a later Load sees is what has been validated here.
func RenderEdit(path string, edit ConfigEdit) ([]byte, *Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, &ConfigError{Path: abs, Problems: []string{err.Error()}}
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, nil, &ConfigError{Path: abs, Problems: []string{"the file is not a YAML mapping"}}
	}

	if err := applyEdit(abs, root, edit); err != nil {
		return nil, nil, err
	}

	out, err := encode(&doc)
	if err != nil {
		return nil, nil, err
	}
	// Validate the bytes that would hit the disk, not the node tree.
	cfg, err := parse(abs, out)
	if err != nil {
		return nil, nil, err
	}
	return out, cfg, nil
}

// EditServices applies an edit and writes the file back. Nothing reaches the
// disk unless the whole edit succeeded and the result validates.
func EditServices(path string, edit ConfigEdit) (*Config, error) {
	out, cfg, err := RenderEdit(path, edit)
	if err != nil {
		return nil, err
	}
	if err := WriteConfigFile(cfg.Path, out); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEdit mutates the node tree in place. It returns on the first problem,
// and because the caller only writes after it returns nil, a half-applied tree
// is thrown away rather than rendered.
func applyEdit(abs string, root *yaml.Node, edit ConfigEdit) error {
	for _, name := range edit.Remove {
		if err := removeService(abs, root, name); err != nil {
			return err
		}
	}
	for _, r := range edit.Rename {
		if err := renameService(abs, root, r); err != nil {
			return err
		}
	}
	for _, add := range edit.Add {
		if err := addService(abs, root, add); err != nil {
			return err
		}
	}
	services := mappingValue(root, "services")
	for _, e := range edit.Services {
		// The service is checked even for a patch that changes nothing: a
		// caller naming a service that is not there has made a mistake, and an
		// empty patch is not the place to swallow it.
		svc := findService(services, e.Name)
		if svc == nil {
			return &ServiceNotFoundError{Path: abs, Name: e.Name}
		}
		if e.Patch.Empty() {
			continue
		}
		applyPatch(svc, e.Patch)
	}
	if c := edit.WorktreePorts; c.Set {
		if c.Value == nil {
			removeKey(root, FieldWorktreePorts)
		} else {
			// setKeyIn edits an existing value in place, so a comment beside
			// `worktree_ports:` stays on that line.
			setKeyIn(root, FieldWorktreePorts,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(*c.Value)}, rootKeyOrder)
		}
	}
	return nil
}

// removeService drops a service from the list and from every other service's
// depends_on, so the file that is left still validates.
func removeService(abs string, root *yaml.Node, name string) error {
	services := mappingValue(root, "services")
	at := serviceIndex(services, name)
	if at < 0 {
		return &ServiceNotFoundError{Path: abs, Name: name}
	}
	services.Content = slices.Delete(services.Content, at, at+1)
	for _, item := range services.Content {
		if item.Kind == yaml.MappingNode {
			dropDependency(item, name)
		}
	}
	return nil
}

// renameService rewrites a service's name key and every reference to it.
func renameService(abs string, root *yaml.Node, r ServiceRename) error {
	services := mappingValue(root, "services")
	svc := findService(services, r.From)
	if svc == nil {
		return &ServiceNotFoundError{Path: abs, Name: r.From}
	}
	if r.From != r.To {
		if findService(services, r.To) != nil {
			return &ServiceConflictError{Path: abs, Name: r.To}
		}
	}
	// The name scalar is edited in place, so a comment sitting on that line
	// stays on it and the key keeps its position in the mapping.
	if node := mappingValue(svc, "name"); node != nil {
		node.Value = r.To
	}
	for _, item := range services.Content {
		if item.Kind == yaml.MappingNode {
			renameDependency(item, r.From, r.To)
		}
	}
	return nil
}

// addService appends a service, refusing a name or a port the file already
// uses. A file with no `services:` key at all grows one.
func addService(abs string, root *yaml.Node, add ServiceAdd) error {
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.SequenceNode {
		if services != nil {
			// A `services:` that is not a list is a file this package will not
			// guess at; say so rather than silently replacing it.
			return &ConfigError{Path: abs, Problems: []string{"services is not a list"}}
		}
		services = &yaml.Node{Kind: yaml.SequenceNode}
		setKeyIn(root, "services", services, rootKeyOrder)
	}
	if findService(services, add.Name) != nil {
		return &ServiceConflictError{Path: abs, Name: add.Name}
	}
	if add.Port != 0 {
		if holder := servicePortHolder(services, add.Port); holder != "" {
			return &ServiceConflictError{Path: abs, Name: holder, Port: add.Port}
		}
	}
	services.Content = append(services.Content, serviceNode(add))
	return nil
}

// serviceNode renders one added service as the mapping the encoder writes.
// Keys go in through setKey, so the order matches what `sonar init` produces
// and what a hand-written file looks like.
func serviceNode(add ServiceAdd) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode}
	setKey(m, "name", strNode(add.Name))
	if add.Cmd != "" {
		setKey(m, "cmd", strNode(add.Cmd))
	}
	if add.Cwd != "" {
		setKey(m, "cwd", strNode(add.Cwd))
	}
	if add.Port != 0 {
		setKey(m, FieldPort, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(add.Port)})
	}
	if add.Health != "" {
		setKey(m, FieldHealth, strNode(add.Health))
	}
	if add.Description != "" {
		setKey(m, FieldDescription, strNode(add.Description))
	}
	if add.Icon != "" {
		setKey(m, FieldIcon, strNode(add.Icon))
	}
	if add.Color != "" {
		setKey(m, FieldColor, strNode(add.Color))
	}
	if len(add.DependsOn) > 0 {
		// Flow style, because that is how `depends_on: [db]` is written
		// everywhere else in this file format.
		seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, dep := range add.DependsOn {
			seq.Content = append(seq.Content, strNode(dep))
		}
		setKey(m, "depends_on", seq)
	}
	return m
}

func strNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// serviceIndex is the position of a named service in the services sequence.
func serviceIndex(services *yaml.Node, name string) int {
	if services == nil || services.Kind != yaml.SequenceNode {
		return -1
	}
	for i, item := range services.Content {
		if item.Kind == yaml.MappingNode && mappingName(item) == name {
			return i
		}
	}
	return -1
}

// servicePortHolder is the name of the service already declaring this port.
func servicePortHolder(services *yaml.Node, port int) string {
	if services == nil || services.Kind != yaml.SequenceNode {
		return ""
	}
	want := strconv.Itoa(port)
	for _, item := range services.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if node := mappingValue(item, FieldPort); node != nil && node.Value == want {
			return mappingName(item)
		}
	}
	return ""
}

// dropDependency removes one name from a service's depends_on, and the key
// itself once nothing is left in it.
func dropDependency(svc *yaml.Node, name string) {
	deps := mappingValue(svc, "depends_on")
	if deps == nil || deps.Kind != yaml.SequenceNode {
		return
	}
	kept := make([]*yaml.Node, 0, len(deps.Content))
	for _, dep := range deps.Content {
		if dep.Value != name {
			kept = append(kept, dep)
		}
	}
	if len(kept) == len(deps.Content) {
		return
	}
	if len(kept) == 0 {
		removeKey(svc, "depends_on")
		return
	}
	deps.Content = kept
}

// renameDependency rewrites the depends_on entries naming from.
func renameDependency(svc *yaml.Node, from, to string) {
	deps := mappingValue(svc, "depends_on")
	if deps == nil || deps.Kind != yaml.SequenceNode {
		return
	}
	for _, dep := range deps.Content {
		if dep.Value == from {
			dep.Value = to
		}
	}
}

// Curate replaces a proposal's services with the caller's own list, which is
// how `groups.init` writes a curated file in one call. An entry that names a
// port the proposal also found keeps that proposal's cmd and cwd, so a client
// editing only names, ports and metadata does not throw away the command
// `sonar up` would need to start the service again.
func Curate(cfg *Config, services []ServiceAdd) *Config {
	out := *cfg
	out.Services = make([]Service, 0, len(services))
	for _, add := range services {
		svc := add.Service()
		if svc.Cmd == "" || svc.Cwd == "" {
			if from, ok := proposedForPort(cfg, add.Port); ok {
				if svc.Cmd == "" {
					svc.Cmd = from.Cmd
				}
				if svc.Cwd == "" {
					svc.Cwd = from.Cwd
				}
			}
		}
		out.Services = append(out.Services, svc)
	}
	return &out
}

func proposedForPort(cfg *Config, port int) (Service, bool) {
	if port == 0 {
		return Service{}, false
	}
	for _, s := range cfg.Services {
		if s.Port == port {
			return s, true
		}
	}
	return Service{}, false
}
