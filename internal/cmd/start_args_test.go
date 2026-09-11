package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/groups"
)

// startProjectDir is a checkout holding a sonar.yaml with api and web, plus a
// dev.sh, a subdirectory, and a sibling checkout with no config at all.
func startProjectDir(t *testing.T) (repo, bare string) {
	t.Helper()
	base := t.TempDir()
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	repo = filepath.Join(base, "repo")
	bare = filepath.Join(base, "bare")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "backend"), filepath.Join(bare, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "name: shop\nservices:\n  - name: api\n    cmd: run-api\n  - name: web\n    cmd: run-web\n"
	if err := os.WriteFile(filepath.Join(repo, groups.ConfigName), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "dev.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, bare
}

func TestClassifyStart(t *testing.T) {
	repo, _ := startProjectDir(t)
	backend := filepath.Join(repo, "backend")

	for _, tt := range []struct {
		name     string
		cwd      string
		args     []string
		dash     int
		project  bool
		services string
	}{
		{"no arguments is the nearest file", repo, nil, -1, true, ""},
		{"from a subdirectory too", backend, nil, -1, true, ""},
		{"a dot", backend, []string{"."}, -1, true, ""},
		{"a path and services", backend, []string{"..", "web"}, -1, true, "web"},
		{"the config file itself", backend, []string{"../sonar.yaml"}, -1, true, ""},
		{"service names", repo, []string{"api", "web"}, -1, true, "api,web"},
		{"a command without --", repo, []string{"npm", "run", "dev"}, -1, false, ""},
		{"a service name followed by a command word", repo, []string{"api", "--verbose"}, -1, false, ""},
		{"a script path", repo, []string{"./dev.sh"}, -1, false, ""},
		{"a command after --", repo, []string{"npm", "run", "dev"}, 0, false, ""},
		{"a folder that is not a project", repo, []string{"backend"}, -1, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := classifyStart(tt.cwd, tt.args, tt.dash)
			if err != nil {
				t.Fatalf("classifyStart: %v", err)
			}
			if (req != nil) != tt.project {
				t.Fatalf("project = %v, want %v", req != nil, tt.project)
			}
			if req == nil {
				return
			}
			if req.cfg.Name != "shop" {
				t.Errorf("config = %s, want the shop project", req.cfg.Path)
			}
			if got := strings.Join(req.services, ","); got != tt.services {
				t.Errorf("services = %q, want %q", got, tt.services)
			}
		})
	}
}

func TestClassifyStartErrors(t *testing.T) {
	repo, bare := startProjectDir(t)

	if _, err := classifyStart(repo, []string{"api", "npm"}, 1); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("services and a command: err = %v", err)
	}
	if _, err := classifyStart(repo, []string{".", "nope"}, -1); err == nil || !strings.Contains(err.Error(), `no service "nope"`) {
		t.Errorf("an unknown service after a path: err = %v", err)
	}
	if _, err := classifyStart(bare, nil, -1); err == nil || !strings.Contains(err.Error(), "sonar init") {
		t.Errorf("no config: err = %v, want the sonar init hint", err)
	}
	// With no config, words are a command, as they always were.
	if req, err := classifyStart(bare, []string{"npm", "run", "dev"}, -1); err != nil || req != nil {
		t.Errorf("a command in a project with no config = %+v, %v", req, err)
	}
}
