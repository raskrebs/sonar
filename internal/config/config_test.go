package config

import (
	"os"
	"path/filepath"
	"testing"
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
