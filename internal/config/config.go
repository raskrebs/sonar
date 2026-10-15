package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/display"
	"gopkg.in/yaml.v3"
)

// Config holds user preferences loaded from ~/.config/sonar/config.yaml.
type Config struct {
	List     ListConfig     `yaml:"list"`
	Daemon   DaemonConfig   `yaml:"daemon"`
	Color    *bool          `yaml:"color"`    // pointer: nil = unset, distinguishes from explicit false
	Services map[int]string `yaml:"services"` // port -> label, merged over built-in table
	// Remote holds the registered SSH hosts the daemon bridges to (see
	// remote.go).
	Remote RemoteConfig `yaml:"remote"`
	// Desktop holds where the desktop app comes from and what
	// `sonar install desktop` last put on this machine.
	Desktop DesktopConfig `yaml:"desktop"`
	// Share holds the sharing settings; today that is the relay this machine
	// signs in to (see share.go).
	Share ShareConfig `yaml:"share"`
	// Claims holds the port-claim settings (see ClaimsConfig).
	Claims ClaimsConfig `yaml:"claims"`
}

// ClaimsConfig is the claims book's corner of the config.
type ClaimsConfig struct {
	// TTL is how long a claim lives from its last acquire, as a Go duration
	// ("24h", "168h"): the life of a `port: auto` service's port between
	// starts, and of a `sonar claim` or `claim_port` reservation. A call that
	// names its own ttl wins. Empty means DefaultClaimTTL.
	TTL string `yaml:"ttl"`
}

// ResolvedTTL returns the parsed claim life, falling back to DefaultClaimTTL
// when the setting is absent or unusable.
func (c ClaimsConfig) ResolvedTTL() time.Duration {
	v, err := time.ParseDuration(strings.TrimSpace(c.TTL))
	if err != nil || v <= 0 {
		return DefaultClaimTTL
	}
	return v
}

// DesktopConfig is the desktop app's corner of the config. download_base is a
// setting; the two installed_* keys are a record `sonar install desktop`
// writes and `sonar tray`, `sonar doctor` and `--check` read, so an app
// installed with --dir is still found and an update knows what it is
// replacing.
type DesktopConfig struct {
	// DownloadBase overrides where the manifest and artifacts are fetched
	// from. Empty means the built-in default.
	DownloadBase string `yaml:"download_base"`
	// InstalledVersion is the version of the app on this machine.
	InstalledVersion string `yaml:"installed_version"`
	// InstalledPath is where it was installed.
	InstalledPath string `yaml:"installed_path"`
}

// DefaultIdleTimeout is how long `sonar serve` stays up with no clients, no
// subscribers and no keepalive connection.
const DefaultIdleTimeout = 30 * time.Minute

// DefaultStatsInterval and MinStatsInterval bound `daemon.stats_interval`, the
// cadence of the daemon's stats-only tick. DefaultScanInterval and
// MinScanInterval do the same for `daemon.scan_interval`, the base of the
// port-scan cadence. All four mirror the scanner's own constants;
// config does not import the scanner, which would drag the whole port
// pipeline into every CLI command that reads a config file.
const (
	DefaultStatsInterval = 1 * time.Second
	MinStatsInterval     = 250 * time.Millisecond
	DefaultScanInterval  = 2 * time.Second
	MinScanInterval      = 1 * time.Second
)

// DefaultClaimTTL is how long a port claim lives when neither the caller nor
// `claims.ttl` says: one day (spec 2 §4). It mirrors claims.DefaultTTL, for
// the same reason the intervals above mirror the scanner's.
const DefaultClaimTTL = 24 * time.Hour

// DaemonConfig holds `sonar serve` settings.
type DaemonConfig struct {
	// IdleTimeout is a Go duration ("30m", "2h"). "0" disables idle shutdown;
	// empty means DefaultIdleTimeout.
	IdleTimeout string `yaml:"idle_timeout"`
	// LogLevel is debug, info, warn or error. Empty means info.
	LogLevel string `yaml:"log_level"`
	// StatsInterval is how often the daemon refreshes per-process cpu/memory
	// and the machine's own load row while something is subscribed, as a Go
	// duration ("1s", "500ms"). Empty means DefaultStatsInterval; anything
	// below MinStatsInterval is clamped to it. It does not affect how often
	// ports are scanned — that cadence is adaptive and owned by the scanner.
	StatsInterval string `yaml:"stats_interval"`
	// ScanInterval is the base cadence of the port scan, as a Go duration
	// ("2s", "5s"). Empty means DefaultScanInterval; anything below
	// MinScanInterval is clamped to it. It sets the *base*: the scanner still
	// backs off on unchanged scans, and the two ceilings it backs off to
	// scale with this value, so raising it makes the whole curve slower
	// rather than only its fastest step.
	ScanInterval string `yaml:"scan_interval"`
}

// ResolvedIdleTimeout returns the parsed idle timeout, falling back to
// DefaultIdleTimeout when the setting is absent. A zero return means "never".
func (d DaemonConfig) ResolvedIdleTimeout() time.Duration {
	if strings.TrimSpace(d.IdleTimeout) == "" {
		return DefaultIdleTimeout
	}
	v, err := time.ParseDuration(strings.TrimSpace(d.IdleTimeout))
	if err != nil || v < 0 {
		return DefaultIdleTimeout
	}
	return v
}

// ResolvedStatsInterval returns the parsed stats cadence, falling back to
// DefaultStatsInterval and never returning less than MinStatsInterval.
func (d DaemonConfig) ResolvedStatsInterval() time.Duration {
	v, err := time.ParseDuration(strings.TrimSpace(d.StatsInterval))
	if err != nil || v <= 0 {
		// Including the unset case. There is no "off": the sampler already
		// parks itself whenever nothing is subscribed, so a zero here would
		// only mean a busy loop.
		return DefaultStatsInterval
	}
	if v < MinStatsInterval {
		return MinStatsInterval
	}
	return v
}

// ResolvedScanInterval returns the parsed base scan cadence, falling back to
// DefaultScanInterval and never returning less than MinScanInterval.
func (d DaemonConfig) ResolvedScanInterval() time.Duration {
	v, err := time.ParseDuration(strings.TrimSpace(d.ScanInterval))
	if err != nil || v <= 0 {
		// Including the unset case. There is no "off": the loop already parks
		// itself whenever nothing is subscribed and nothing is reading.
		return DefaultScanInterval
	}
	if v < MinScanInterval {
		return MinScanInterval
	}
	return v
}

// ResolvedLogLevel returns the configured level, defaulting to "info".
func (d DaemonConfig) ResolvedLogLevel() string {
	level := strings.ToLower(strings.TrimSpace(d.LogLevel))
	if !validLogLevels[level] {
		return "info"
	}
	return level
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
func Load() (*Config, []string) { return LoadFrom(Path()) }

// LoadFrom is Load against an explicit file. `sonar doctor` uses it: its
// checks read the config path in its Env, which a test points at a fixture,
// and going through Path() would read the developer's own file instead.
func LoadFrom(path string) (*Config, []string) {
	cfg := &Config{}

	data, err := os.ReadFile(path)
	if err != nil {
		// Missing/unreadable file is not an error — run with defaults.
		return cfg, nil
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return &Config{}, []string{fmt.Sprintf("ignoring %s: %v", path, err)}
	}

	warnings := validate(cfg)
	hosts, hostWarnings := validateHosts(cfg.Remote.Hosts)
	cfg.Remote.Hosts = hosts
	return cfg, append(warnings, hostWarnings...)
}

const template = `# sonar configuration
# All settings are optional. Uncomment and edit to override built-in defaults.
# Explicit command-line flags always take precedence over this file.

# list:
#   # Columns shown by default. Available: port, process, pid, type, url,
#   # group, cpu, mem, threads, uptime, state, connections, container, image,
#   # containerport, compose, project, user, bind, ip, health, latency
#   # (tag is accepted as an alias of group)
#   columns: [port, process, group, container, image, containerport, url]
#   sort: port      # port | pid | name | type
#   filter: ""      # docker | user | system | "" (all)
#   all: false      # include desktop apps by default

# daemon:
#   # sonar serve stops after this long with no clients, no subscribers and
#   # no keepalive connection. 0 keeps it running until you stop it.
#   idle_timeout: 30m
#   # How much the daemon writes to ~/.config/sonar/daemon.log.
#   log_level: info     # debug | info | warn | error
#   # How often cpu, memory and the host load strip refresh while the app (or
#   # any other subscriber) is watching. Minimum 250ms.
#   stats_interval: 1s
#   # The base cadence of the port scan. The scanner still slows itself down
#   # on unchanged scans — to 2.5x this while something is subscribed and 5x
#   # when only RPC reads are served — so this moves the whole curve.
#   # Minimum 1s. Both intervals are read at startup: restart the daemon to
#   # apply a change, and 'sonar daemon status' shows what is in effect.
#   scan_interval: 2s

# claims:
#   # How long a port claim lives from its last acquire: the port a
#   # 'port: auto' service keeps between starts, and a 'sonar claim' or
#   # 'claim_port' reservation. A call that names its own ttl wins. Read at
#   # startup: restart the daemon to apply a change.
#   ttl: 24h

# color: true       # set false to disable colored output

# services:         # label custom/unknown ports (port: name)
#   9000: php-fpm
#   5050: my-dashboard

# remote:           # hosts reached over SSH; written by 'sonar remote add'
#   hosts:
#     - name: hetzner            # [a-z0-9-]+, how you address it everywhere
#       target: deploy@203.0.113.7   # what ssh receives, verbatim
#       ssh_args: ["-J", "bastion"]
#       identity: ~/.ssh/id_ed25519
#       remote_bin: ~/.local/bin/sonar

# share:            # sharing a local port through the relay
#   # The relay sonar signs in to and shares through. Change it only for a
#   # self-hosted relay; SONAR_RELAY overrides it for one process.
#   relay: https://relay.trysonar.dev

# desktop:          # the Sonar desktop app
#   # Where 'sonar install desktop' fetches desktop.json and the artifacts it
#   # names. --base and SONAR_DESKTOP_BASE both win over this.
#   download_base: https://github.com/raskrebs/sonar-desktop-releases/releases/latest/download
#   # Written by 'sonar install desktop'; read by 'sonar tray', 'sonar doctor'
#   # and 'sonar install desktop --check'. Edit only to point them elsewhere.
#   installed_version: 0.1.0-beta.1
#   installed_path: /Applications/Sonar.app

# Environment overrides (no config key; set them in your shell):
#   SONAR_DB       path to the database of names, pins and history
#                  (default: sonar.db next to this file)
#   SONAR_SOCKET   path the daemon listens on and every client dials
#                  (default: what 'sonar daemon path' prints)
#   SONAR_NO_AUTOSTART
#                  set to 1 to stop clients starting a daemon that is not
#                  already running; they report it as unavailable instead
#   SONAR_DESKTOP_BASE
#                  where 'sonar install desktop' looks, overriding
#                  desktop.download_base
#   SONAR_RELAY    the relay to sign in to and share through, overriding
#                  share.relay
#   SONAR_CREDENTIALS_STORE
#                  set to 'file' to keep the relay session in
#                  ~/.config/sonar/credentials.json instead of the OS keychain
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

var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

var validSorts = map[string]bool{"port": true, "pid": true, "name": true, "type": true}
var validFilters = map[string]bool{"docker": true, "user": true, "system": true}

// validate checks list settings and service ports, dropping any invalid value
// and returning a warning for each. Valid neighboring settings are preserved.
func validate(cfg *Config) []string {
	var warnings []string

	// The relay is validated here rather than in Relay() so that
	// `sonar config set share.relay …` refuses a bad value at the moment it
	// is written, instead of accepting it and ignoring it on every load.
	if _, w := ResolveRelay("", cfg.Share.Relay); w != "" {
		warnings = append(warnings, w)
		cfg.Share.Relay = ""
	}

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

	if v := strings.TrimSpace(cfg.Daemon.IdleTimeout); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d < 0 {
			warnings = append(warnings, fmt.Sprintf("config: invalid daemon.idle_timeout %q — using %s", v, DefaultIdleTimeout))
			cfg.Daemon.IdleTimeout = ""
		}
	}

	if v := strings.TrimSpace(cfg.Daemon.StatsInterval); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			warnings = append(warnings, fmt.Sprintf("config: invalid daemon.stats_interval %q — using %s", v, DefaultStatsInterval))
			cfg.Daemon.StatsInterval = ""
		} else if d < MinStatsInterval {
			warnings = append(warnings, fmt.Sprintf("config: daemon.stats_interval %q is below the %s minimum — using %s", v, MinStatsInterval, MinStatsInterval))
			cfg.Daemon.StatsInterval = MinStatsInterval.String()
		}
	}

	if v := strings.TrimSpace(cfg.Daemon.ScanInterval); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			warnings = append(warnings, fmt.Sprintf("config: invalid daemon.scan_interval %q — using %s", v, DefaultScanInterval))
			cfg.Daemon.ScanInterval = ""
		} else if d < MinScanInterval {
			warnings = append(warnings, fmt.Sprintf("config: daemon.scan_interval %q is below the %s minimum — using %s", v, MinScanInterval, MinScanInterval))
			cfg.Daemon.ScanInterval = MinScanInterval.String()
		}
	}

	if v := strings.TrimSpace(cfg.Daemon.LogLevel); v != "" && !validLogLevels[strings.ToLower(v)] {
		warnings = append(warnings, fmt.Sprintf("config: invalid daemon.log_level %q — using info", v))
		cfg.Daemon.LogLevel = ""
	}

	if v := strings.TrimSpace(cfg.Claims.TTL); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			warnings = append(warnings, fmt.Sprintf("config: invalid claims.ttl %q — using %s", v, DefaultClaimTTL))
			cfg.Claims.TTL = ""
		}
	}

	for port := range cfg.Services {
		if port < 1 || port > 65535 {
			warnings = append(warnings, fmt.Sprintf("config: invalid service port %d — ignoring", port))
			delete(cfg.Services, port)
		}
	}

	return warnings
}
