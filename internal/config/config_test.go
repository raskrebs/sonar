package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPath(t *testing.T) {
	t.Setenv("HOME", "/tmp/fakehome")
	want := filepath.Join("/tmp/fakehome", ".config", "sonar", "config.yaml")
	if got := Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no config.yaml inside
	cfg, warnings := Load()
	if cfg == nil {
		t.Fatal("Load() returned nil config")
	}
	if len(warnings) != 0 {
		t.Errorf("missing file produced warnings: %v", warnings)
	}
	if len(cfg.List.Columns) != 0 || cfg.Color != nil || len(cfg.Services) != 0 {
		t.Errorf("missing file should yield empty config, got %+v", cfg)
	}
}

func TestLoadValidFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "sonar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "list:\n  columns: [port, process]\n  sort: name\nservices:\n  9000: php-fpm\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, warnings := Load()
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(cfg.List.Columns) != 2 || cfg.List.Columns[0] != "port" {
		t.Errorf("columns = %v", cfg.List.Columns)
	}
	if cfg.List.Sort != "name" {
		t.Errorf("sort = %q", cfg.List.Sort)
	}
	if cfg.Services[9000] != "php-fpm" {
		t.Errorf("services = %v", cfg.Services)
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "sonar")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("list: [this is not: valid"), 0o644)
	cfg, warnings := Load()
	if cfg == nil {
		t.Fatal("Load() returned nil config")
	}
	if len(warnings) == 0 {
		t.Error("malformed YAML should produce a warning")
	}
}

func TestLoadEmptyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "sonar")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("   \n"), 0o644)
	cfg, warnings := Load()
	if len(warnings) != 0 {
		t.Errorf("empty file produced warnings: %v", warnings)
	}
	if len(cfg.List.Columns) != 0 {
		t.Errorf("empty file should yield empty config")
	}
}

func writeConfig(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "sonar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateUnknownColumn(t *testing.T) {
	writeConfig(t, "list:\n  columns: [port, bogus, url]\n")
	cfg, warnings := Load()
	if len(warnings) == 0 {
		t.Fatal("expected a warning for unknown column")
	}
	if len(cfg.List.Columns) != 0 {
		t.Errorf("columns should be cleared on bad value, got %v", cfg.List.Columns)
	}
}

func TestValidateBadSortKeepsOtherSettings(t *testing.T) {
	writeConfig(t, "list:\n  sort: sideways\n  filter: docker\n")
	cfg, warnings := Load()
	if len(warnings) == 0 {
		t.Fatal("expected a warning for bad sort")
	}
	if cfg.List.Sort != "" {
		t.Errorf("bad sort should be cleared, got %q", cfg.List.Sort)
	}
	if cfg.List.Filter != "docker" {
		t.Errorf("valid filter should survive, got %q", cfg.List.Filter)
	}
}

func TestValidateBadFilter(t *testing.T) {
	writeConfig(t, "list:\n  filter: nonsense\n")
	cfg, warnings := Load()
	if len(warnings) == 0 || cfg.List.Filter != "" {
		t.Errorf("bad filter should warn and clear; warnings=%v filter=%q", warnings, cfg.List.Filter)
	}
}

func TestValidateEmptyColumnsTreatedAsUnset(t *testing.T) {
	writeConfig(t, "list:\n  columns: []\n")
	cfg, warnings := Load()
	if len(warnings) != 0 {
		t.Errorf("empty columns should not warn: %v", warnings)
	}
	if len(cfg.List.Columns) != 0 {
		t.Errorf("empty columns stays empty (falls back to defaults)")
	}
}

func TestValidateServicePortRange(t *testing.T) {
	writeConfig(t, "services:\n  0: bad\n  70000: bad\n  8000: good\n")
	cfg, warnings := Load()
	if len(warnings) == 0 {
		t.Fatal("expected warnings for out-of-range ports")
	}
	if _, ok := cfg.Services[0]; ok {
		t.Error("port 0 should be removed")
	}
	if _, ok := cfg.Services[70000]; ok {
		t.Error("port 70000 should be removed")
	}
	if cfg.Services[8000] != "good" {
		t.Error("valid service should survive")
	}
}

func TestWriteTemplateCreatesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := WriteTemplate(false); err != nil {
		t.Fatalf("WriteTemplate: %v", err)
	}
	data, err := os.ReadFile(Path())
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	if len(data) == 0 {
		t.Error("template is empty")
	}
	cfg, warnings := Load()
	if len(warnings) != 0 {
		t.Errorf("template produced warnings: %v", warnings)
	}
	if len(cfg.List.Columns) != 0 {
		t.Errorf("template should be all-commented, got columns %v", cfg.List.Columns)
	}
}

func TestWriteTemplateRefusesOverwrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := WriteTemplate(false); err != nil {
		t.Fatal(err)
	}
	if err := WriteTemplate(false); err == nil {
		t.Error("expected error when file exists and force=false")
	}
	if err := WriteTemplate(true); err != nil {
		t.Errorf("force=true should overwrite, got %v", err)
	}
}

// The template is the only documentation many users read, so the settings the
// daemon actually honours have to be in it.
func TestTemplateDocumentsTheDaemonAndItsEnvironment(t *testing.T) {
	for _, want := range []string{
		"# daemon:", "idle_timeout: 30m", "log_level: info", "stats_interval: 1s",
		"scan_interval: 2s",
		"# claims:", "ttl: 24h",
		"SONAR_DB", "SONAR_SOCKET",
	} {
		if !strings.Contains(template, want) {
			t.Errorf("config template does not mention %q", want)
		}
	}
}

// daemon.stats_interval defaults to 1 s, is clamped rather than trusted, and
// never leaves the daemon with a zero cadence.
func TestResolvedStatsInterval(t *testing.T) {
	tests := []struct {
		name string
		set  string
		want time.Duration
	}{
		{"unset", "", DefaultStatsInterval},
		{"explicit", "2s", 2 * time.Second},
		{"sub-second", "500ms", 500 * time.Millisecond},
		{"below the floor", "10ms", MinStatsInterval},
		{"zero", "0", DefaultStatsInterval},
		{"nonsense", "soon", DefaultStatsInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DaemonConfig{StatsInterval: tt.set}.ResolvedStatsInterval()
			if got != tt.want {
				t.Errorf("ResolvedStatsInterval(%q) = %s, want %s", tt.set, got, tt.want)
			}
		})
	}
}

// An out-of-range stats_interval is repaired with a warning, the way every
// other bad setting is, and its neighbours survive.
func TestValidateClampsStatsInterval(t *testing.T) {
	cfg := &Config{Daemon: DaemonConfig{StatsInterval: "5ms", LogLevel: "debug"}}
	warnings := validate(cfg)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "stats_interval") {
		t.Fatalf("warnings = %v, want one about stats_interval", warnings)
	}
	if got := cfg.Daemon.ResolvedStatsInterval(); got != MinStatsInterval {
		t.Errorf("stats_interval = %s, want the %s floor", got, MinStatsInterval)
	}
	if cfg.Daemon.LogLevel != "debug" {
		t.Errorf("log_level = %q, a valid neighbour was dropped", cfg.Daemon.LogLevel)
	}
}

// daemon.scan_interval defaults to the scanner's 2 s base, is clamped rather
// than trusted, and never leaves the loop with a zero cadence.
func TestResolvedScanInterval(t *testing.T) {
	tests := []struct {
		name string
		set  string
		want time.Duration
	}{
		{"unset", "", DefaultScanInterval},
		{"explicit", "5s", 5 * time.Second},
		{"at the floor", "1s", MinScanInterval},
		{"below the floor", "200ms", MinScanInterval},
		{"zero", "0", DefaultScanInterval},
		{"nonsense", "whenever", DefaultScanInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DaemonConfig{ScanInterval: tt.set}.ResolvedScanInterval()
			if got != tt.want {
				t.Errorf("ResolvedScanInterval(%q) = %s, want %s", tt.set, got, tt.want)
			}
		})
	}
}

// An out-of-range scan_interval is repaired with a warning, the way
// stats_interval is, and its neighbours survive.
func TestValidateClampsScanInterval(t *testing.T) {
	cfg := &Config{Daemon: DaemonConfig{ScanInterval: "50ms", StatsInterval: "1s"}}
	warnings := validate(cfg)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "scan_interval") {
		t.Fatalf("warnings = %v, want one about scan_interval", warnings)
	}
	if got := cfg.Daemon.ResolvedScanInterval(); got != MinScanInterval {
		t.Errorf("scan_interval = %s, want the %s floor", got, MinScanInterval)
	}
	if cfg.Daemon.StatsInterval != "1s" {
		t.Errorf("stats_interval = %q, a valid neighbour was dropped", cfg.Daemon.StatsInterval)
	}
}

// A scan_interval that does not parse is dropped with a warning rather than
// taken literally, and the daemon falls back to the default.
func TestValidateDropsUnparseableScanInterval(t *testing.T) {
	cfg := &Config{Daemon: DaemonConfig{ScanInterval: "every so often"}}
	warnings := validate(cfg)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "scan_interval") {
		t.Fatalf("warnings = %v, want one about scan_interval", warnings)
	}
	if cfg.Daemon.ScanInterval != "" {
		t.Errorf("scan_interval = %q, want it dropped", cfg.Daemon.ScanInterval)
	}
	if got := cfg.Daemon.ResolvedScanInterval(); got != DefaultScanInterval {
		t.Errorf("scan_interval = %s, want the %s default", got, DefaultScanInterval)
	}
}

// claims.ttl is used as written when it is a positive duration; anything else
// is repaired with a warning, the way every other bad setting is, and its
// neighbours survive.
func TestClaimsTTLResolvesAndWarns(t *testing.T) {
	writeConfig(t, "claims:\n  ttl: 168h\n")
	cfg, warnings := Load()
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if got := cfg.Claims.ResolvedTTL(); got != 168*time.Hour {
		t.Errorf("ResolvedTTL = %s, want 168h", got)
	}

	for _, bad := range []string{"soon", "0", "-1h"} {
		writeConfig(t, "claims:\n  ttl: "+bad+"\ndaemon:\n  log_level: warn\n")
		cfg, warnings := Load()
		if len(warnings) != 1 || !strings.Contains(warnings[0], "claims.ttl") {
			t.Errorf("ttl %q: warnings = %v, want one about claims.ttl", bad, warnings)
		}
		if cfg.Claims.TTL != "" {
			t.Errorf("ttl %q should be cleared, got %q", bad, cfg.Claims.TTL)
		}
		if got := cfg.Claims.ResolvedTTL(); got != DefaultClaimTTL {
			t.Errorf("ttl %q: ResolvedTTL = %s, want the default %s", bad, got, DefaultClaimTTL)
		}
		if cfg.Daemon.LogLevel != "warn" {
			t.Errorf("ttl %q: a valid neighbour should survive, got log_level %q", bad, cfg.Daemon.LogLevel)
		}
	}

	if (ClaimsConfig{}).ResolvedTTL() != DefaultClaimTTL {
		t.Error("an unset ttl should resolve to the default")
	}
}
