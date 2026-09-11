package groups

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Patch field names, which are also the YAML keys they write.
const (
	FieldDescription = "description"
	FieldIcon        = "icon"
	FieldColor       = "color"
	FieldHealth      = "health"
	FieldPort        = "port"
)

// keyOrder is where a key belongs in a service mapping. A key the patch adds
// is inserted at this position relative to the keys already there, so an
// edited file still reads the way a hand-written one does. Keys nobody here
// knows about sort last and are never moved.
var keyOrder = []string{"name", "cmd", "cwd", "port", "health", "description", "icon", "color", "depends_on"}

// ServicePatch is the editable subset of one `sonar.yaml` service
// (contract §13.2). Three states matter: a field the caller did not mention is
// left alone, a field set to a value is written, and a field set to null is
// removed from the file. JSON's absent/null distinction is lost by pointers
// alone, so UnmarshalJSON records which keys were actually sent.
type ServicePatch struct {
	Description *string `json:"description,omitempty" jsonschema:"nullable"`
	Icon        *string `json:"icon,omitempty" jsonschema:"nullable"`
	Color       *string `json:"color,omitempty" jsonschema:"nullable"`
	Health      *string `json:"health,omitempty" jsonschema:"nullable"`
	Port        *int    `json:"port,omitempty" jsonschema:"nullable"`

	// Sent names the keys the caller included, whether or not their value was
	// null. Go callers set it themselves; the wire form fills it in
	// UnmarshalJSON. A patch with no keys sent is a no-op, not an error.
	Sent []string `json:"-" jsonschema:"-"`
}

// UnmarshalJSON decodes the patch and remembers which keys were present.
func (p *ServicePatch) UnmarshalJSON(data []byte) error {
	type plain ServicePatch
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	*p = ServicePatch(v)
	p.Sent = p.Sent[:0]
	for _, field := range []string{FieldDescription, FieldIcon, FieldColor, FieldHealth, FieldPort} {
		if _, ok := keys[field]; ok {
			p.Sent = append(p.Sent, field)
		}
	}
	return nil
}

// SetString returns the patch with field set to value.
func (p ServicePatch) SetString(field, value string) ServicePatch {
	v := value
	switch field {
	case FieldDescription:
		p.Description = &v
	case FieldIcon:
		p.Icon = &v
	case FieldColor:
		p.Color = &v
	case FieldHealth:
		p.Health = &v
	default:
		return p
	}
	return p.mark(field)
}

// SetPort returns the patch with the service's port hint set.
func (p ServicePatch) SetPort(port int) ServicePatch {
	v := port
	p.Port = &v
	return p.mark(FieldPort)
}

// Clear returns the patch with field marked for removal from the file.
func (p ServicePatch) Clear(field string) ServicePatch {
	switch field {
	case FieldDescription:
		p.Description = nil
	case FieldIcon:
		p.Icon = nil
	case FieldColor:
		p.Color = nil
	case FieldHealth:
		p.Health = nil
	case FieldPort:
		p.Port = nil
	default:
		return p
	}
	return p.mark(field)
}

func (p ServicePatch) mark(field string) ServicePatch {
	if !slices.Contains(p.Sent, field) {
		p.Sent = append(append([]string{}, p.Sent...), field)
	}
	return p
}

// Empty reports whether the patch would change nothing.
func (p ServicePatch) Empty() bool { return len(p.Sent) == 0 }

// value returns the YAML scalar for one sent field, and whether the field
// clears the key instead.
func (p ServicePatch) value(field string) (node *yaml.Node, clear bool) {
	switch field {
	case FieldDescription:
		return strScalar(p.Description), p.Description == nil
	case FieldIcon:
		return strScalar(p.Icon), p.Icon == nil
	case FieldColor:
		return strScalar(p.Color), p.Color == nil
	case FieldHealth:
		return strScalar(p.Health), p.Health == nil
	case FieldPort:
		if p.Port == nil {
			return nil, true
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(*p.Port)}, false
	}
	return nil, false
}

func strScalar(s *string) *yaml.Node {
	if s == nil {
		return nil
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: *s}
}

// ServiceEdit names the service a patch applies to.
type ServiceEdit struct {
	Name  string       `json:"name"`
	Patch ServicePatch `json:"patch"`
}

// ServiceNotFoundError is returned when a patch names a service the file does
// not declare. It maps onto the contract's `not_found`.
type ServiceNotFoundError struct {
	Path string
	Name string
}

func (e *ServiceNotFoundError) Error() string {
	return fmt.Sprintf("no service %q in %s", e.Name, e.Path)
}

// PatchServices applies metadata edits to the services in a `sonar.yaml` and
// writes the file back. It is EditServices with only the patch list filled in;
// see ConfigEdit for adding, renaming and removing services.
func PatchServices(path string, edits []ServiceEdit) (*Config, error) {
	return EditServices(path, ConfigEdit{Services: edits})
}

// applyPatch sets, replaces or removes each sent key of one service mapping.
func applyPatch(svc *yaml.Node, patch ServicePatch) {
	for _, field := range patch.Sent {
		node, clear := patch.value(field)
		if clear {
			removeKey(svc, field)
			continue
		}
		setKey(svc, field, node)
	}
}

// setKey sets a key of one service mapping, in keyOrder.
func setKey(mapping *yaml.Node, key string, value *yaml.Node) {
	setKeyIn(mapping, key, value, keyOrder)
}

// setKeyIn replaces the value of an existing key in place — keeping the key's
// comments and position — or inserts the pair where order says it belongs.
func setKeyIn(mapping *yaml.Node, key string, value *yaml.Node, order []string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		old := mapping.Content[i+1]
		value.HeadComment, value.LineComment, value.FootComment = old.HeadComment, old.LineComment, old.FootComment
		mapping.Content[i+1] = value
		return
	}
	pair := []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value}
	at := insertionPoint(mapping, key, order)
	mapping.Content = slices.Insert(mapping.Content, at, pair...)
}

// insertionPoint is the index in a mapping's Content where a new key belongs:
// before the first key that sorts after it, and at the end otherwise.
func insertionPoint(mapping *yaml.Node, key string, order []string) int {
	rank := keyRank(key, order)
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if keyRank(mapping.Content[i].Value, order) > rank {
			return i
		}
	}
	return len(mapping.Content)
}

func keyRank(key string, order []string) int {
	if i := slices.Index(order, key); i >= 0 {
		return i
	}
	return len(order)
}

// removeKey drops a key and its value from a mapping.
func removeKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = slices.Delete(mapping.Content, i, i+2)
			return
		}
	}
}

// findService returns the mapping node of the service with this name.
func findService(services *yaml.Node, name string) *yaml.Node {
	if services == nil || services.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range services.Content {
		if item.Kind == yaml.MappingNode && mappingName(item) == name {
			return item
		}
	}
	return nil
}

// documentRoot unwraps a document node.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

// mappingValue returns the value node for a key of a mapping, or nil.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// encode renders the node tree back to YAML with the two-space indentation
// `sonar.yaml` is written in.
func encode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("rendering the config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("rendering the config: %w", err)
	}
	return buf.Bytes(), nil
}

// WriteConfigFile replaces a config atomically, keeping its current permissions so a
// file the user chmodded stays that way. A caller that has already rendered the
// bytes — `groups.init` merging into a file it just previewed — writes them
// through here rather than through os.WriteFile, so a crash mid-write can never
// leave a half a config behind.
func WriteConfigFile(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sonar-config-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(name, mode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
