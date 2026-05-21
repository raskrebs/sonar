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
