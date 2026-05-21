package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/raskrebs/sonar/internal/display"
	"gopkg.in/yaml.v3"
)

// Config holds user preferences loaded from ~/.config/sonar/config.yaml.
type Config struct {
	List     ListConfig     `yaml:"list"`
	Color    *bool          `yaml:"color"`    // pointer: nil = unset, distinguishes from explicit false
	Services map[int]string `yaml:"services"` // port -> label, merged over built-in table
}

// ListConfig holds defaults for the `sonar list` command.
type ListConfig struct {
	Columns []string `yaml:"columns"`
	Sort    string   `yaml:"sort"`
	Filter  string   `yaml:"filter"`
	All     *bool    `yaml:"all"`
}

// Path returns the absolute path to the config file.
func Path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "sonar", "config.yaml")
}

// Load reads and validates the config file. It never returns an error: a
// missing file yields an empty Config with no warnings; a malformed file or
// invalid values yield a Config with the bad settings dropped plus
// human-readable warning strings for the caller to print.
func Load() (*Config, []string) {
	cfg := &Config{}

	data, err := os.ReadFile(Path())
	if err != nil {
		// Missing/unreadable file is not an error — run with defaults.
		return cfg, nil
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return &Config{}, []string{fmt.Sprintf("ignoring %s: %v", Path(), err)}
	}

	return cfg, validate(cfg)
}

const template = `# sonar configuration
# All settings are optional. Uncomment and edit to override built-in defaults.
# Explicit command-line flags always take precedence over this file.

# list:
#   # Columns shown by default. Available: port, process, pid, type, url,
#   # cpu, mem, threads, uptime, state, connections, container, image,
#   # containerport, compose, project, user, bind, ip, health, latency
#   columns: [port, process, container, image, containerport, url]
#   sort: port      # port | pid | name | type
#   filter: ""      # docker | user | system | "" (all)
#   all: false      # include desktop apps by default

# color: true       # set false to disable colored output

# services:         # label custom/unknown ports (port: name)
#   9000: php-fpm
#   5050: my-dashboard
`

// WriteTemplate writes a commented starter config to Path(), creating the
// parent directory if needed. It refuses to overwrite an existing file unless
// force is true.
func WriteTemplate(force bool) error {
	path := Path()
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists at %s (use --force to overwrite)", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("could not create config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(template), 0o644); err != nil {
		return fmt.Errorf("could not write config: %w", err)
	}
	return nil
}

var validSorts = map[string]bool{"port": true, "pid": true, "name": true, "type": true}
var validFilters = map[string]bool{"docker": true, "user": true, "system": true}

// validate checks list settings and service ports, dropping any invalid value
// and returning a warning for each. Valid neighboring settings are preserved.
func validate(cfg *Config) []string {
	var warnings []string

	// Columns: every entry must be a known display column.
	known := make(map[string]bool, len(display.AllColumns))
	for _, c := range display.AllColumns {
		known[c] = true
	}
	for _, c := range cfg.List.Columns {
		if !known[c] {
			warnings = append(warnings, fmt.Sprintf("config: unknown column %q — using default columns", c))
			cfg.List.Columns = nil
			break
		}
	}

	if cfg.List.Sort != "" && !validSorts[cfg.List.Sort] {
		warnings = append(warnings, fmt.Sprintf("config: invalid sort %q — using default", cfg.List.Sort))
		cfg.List.Sort = ""
	}

	if cfg.List.Filter != "" && !validFilters[cfg.List.Filter] {
		warnings = append(warnings, fmt.Sprintf("config: invalid filter %q — ignoring", cfg.List.Filter))
		cfg.List.Filter = ""
	}

	for port := range cfg.Services {
		if port < 1 || port > 65535 {
			warnings = append(warnings, fmt.Sprintf("config: invalid service port %d — ignoring", port))
			delete(cfg.Services, port)
		}
	}

	return warnings
}
