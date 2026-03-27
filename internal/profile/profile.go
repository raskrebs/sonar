package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// validateName rejects profile names that contain path separators or traversal components.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("profile name must not be empty")
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, `/\`) || name == ".." || name == "." {
		return fmt.Errorf("invalid profile name %q: must not contain path separators or traversal", name)
	}
	return nil
}

// PortEntry describes a single port within a profile.
type PortEntry struct {
	Port       int    `yaml:"port"`
	Name       string `yaml:"name"`
	Health     bool   `yaml:"health,omitempty"`
	HealthPath string `yaml:"health_path,omitempty"`
}

// Profile is a named collection of expected ports for a project.
type Profile struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description,omitempty"`
	Ports       []PortEntry `yaml:"ports"`
}

// ProfileDir returns the directory where profiles are stored.
func ProfileDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "sonar", "profiles")
}

// Load reads a profile by name from the profiles directory.
func Load(name string) (*Profile, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(ProfileDir(), name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("profile %q not found: %w", name, err)
	}

	var p Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid profile %q: %w", name, err)
	}
	p.Name = name
	return &p, nil
}

// List returns the names of all saved profiles.
func List() ([]string, error) {
	dir := ProfileDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
			names = append(names, strings.TrimSuffix(strings.TrimSuffix(n, ".yaml"), ".yml"))
		}
	}
	return names, nil
}

// Save writes a profile to the profiles directory.
func Save(p *Profile) error {
	if err := validateName(p.Name); err != nil {
		return err
	}
	dir := ProfileDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not create profiles directory: %w", err)
	}

	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("failed to marshal profile: %w", err)
	}

	path := filepath.Join(dir, p.Name+".yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write profile: %w", err)
	}
	return nil
}

// Delete removes a profile by name.
func Delete(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	path := filepath.Join(ProfileDir(), name+".yaml")
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("could not delete profile %q: %w", name, err)
	}
	return nil
}
