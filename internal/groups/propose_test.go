package groups

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/ports"
)

func TestProposeFromRunningPorts(t *testing.T) {
	f := newFixture(t)
	index := NewIndex()
	index.AddComposeProject("sonar", f.composeIn)

	pp := []ports.ListeningPort{
		{Port: 8124, PID: 2, Process: "python3", Command: "python3 -m http.server 8124", Cwd: f.repoSub},
		{Port: 8123, PID: 1, Process: "python3", Command: "python3 -m http.server 8123", Cwd: f.repo},
		// The same listener on a second bind address must not repeat.
		{Port: 8123, PID: 1, Process: "python3", Command: "python3 -m http.server 8123", Cwd: f.repo, BindAddress: "::1"},
		// A Compose container started from inside the repo.
		{Port: 5432, PID: 9, Process: "com.docke", Type: ports.PortTypeDocker,
			DockerContainer: "sonar-db-1", DockerImage: "postgres:17",
			DockerComposeService: "db", DockerComposeProject: "sonar"},
		// Excluded: a desktop app, a system port, and a process outside the repo.
		{Port: 7000, PID: 3, Process: "Figma", IsApp: true, Cwd: f.repo},
		{Port: 22, PID: 4, Process: "sshd", Type: ports.PortTypeSystem, Cwd: f.repo},
		{Port: 9999, PID: 5, Process: "node", Cwd: f.composeOut},
	}

	cfg := Propose(f.repo, pp, index)

	if cfg.Name != "sonar" {
		t.Errorf("name = %q, want the repo directory name", cfg.Name)
	}
	if len(cfg.Services) != 3 {
		t.Fatalf("services = %+v, want three", cfg.Services)
	}
	if got := cfg.Services[0]; got.Port != 5432 || got.Name != "db" || got.Cmd != "docker compose up db" {
		t.Errorf("compose service = %+v", got)
	}
	if got := cfg.Services[0].Cwd; got != "deploy" {
		t.Errorf("compose cwd = %q, want deploy", got)
	}
	if got := cfg.Services[1]; got.Port != 8123 || got.Cwd != "" {
		t.Errorf("repo-root service = %+v", got)
	}
	if got := cfg.Services[2]; got.Port != 8124 || got.Cwd != "backend" {
		t.Errorf("subdirectory service = %+v", got)
	}

	// What init writes must load back cleanly.
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.repo, ConfigName)
	writeFile(t, path, string(data))
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("the proposed config does not validate: %v", err)
	}
	if loaded.Name != cfg.Name || len(loaded.Services) != len(cfg.Services) {
		t.Errorf("round trip = %+v", loaded)
	}
	if !strings.HasPrefix(string(data), "# "+ConfigName) {
		t.Errorf("generated file has no header:\n%s", data)
	}
}

func TestProposeNamesWorktreesAndDedupes(t *testing.T) {
	f := newFixture(t)
	pp := []ports.ListeningPort{
		{Port: 3000, PID: 1, Process: "node", Cwd: f.worktree},
		{Port: 3001, PID: 2, Process: "node", Cwd: f.worktree},
		{Port: 3002, PID: 3, Process: "!!!", Cwd: f.worktree},
	}
	cfg := Propose(f.worktree, pp, nil)
	if cfg.Name != "sonar@feature-x" {
		t.Errorf("name = %q, want the qualified worktree name", cfg.Name)
	}
	names := []string{cfg.Services[0].Name, cfg.Services[1].Name, cfg.Services[2].Name}
	if names[0] != "node" || names[1] != "node-2" || names[2] != "service-3002" {
		t.Errorf("names = %v", names)
	}
}

func TestProposeEmptyRepo(t *testing.T) {
	f := newFixture(t)
	cfg := Propose(f.repo, nil, nil)
	if len(cfg.Services) != 0 {
		t.Fatalf("services = %+v, want none", cfg.Services)
	}
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.repo, ConfigName)
	writeFile(t, path, string(data))
	if _, err := Load(path); err != nil {
		t.Fatalf("an empty proposal does not validate: %v", err)
	}
}

// TestProposeUsesProjectRootWhenTheComposeDirIsGone is the daemon's case. A
// published state.Port carries no Compose working directory — the resolver
// consumes it before publishing — so a proposal built from a snapshot has only
// project_root to go on. Without the fallback every container silently drops
// out of a `groups.init` served by the daemon.
func TestProposeUsesProjectRootWhenTheComposeDirIsGone(t *testing.T) {
	f := newFixture(t)

	pp := []ports.ListeningPort{
		{Port: 5432, PID: 9, Process: "com.docke", Type: ports.PortTypeDocker,
			DockerContainer: "sonar-db-1", DockerImage: "postgres:17",
			DockerComposeService: "db", DockerComposeProject: "sonar",
			ProjectRoot: f.repo},
		// Attributed to a checkout that is not the one being proposed for.
		{Port: 6379, PID: 10, Process: "com.docke", Type: ports.PortTypeDocker,
			DockerContainer: "other-cache-1", DockerComposeService: "cache",
			DockerComposeProject: "other", ProjectRoot: f.composeOut},
	}

	// No index at all: the compose arm of portDir cannot answer.
	cfg := Propose(f.repo, pp, nil)

	if len(cfg.Services) != 1 {
		t.Fatalf("services = %+v, want only the container inside the repo", cfg.Services)
	}
	if got := cfg.Services[0]; got.Port != 5432 || got.Name != "db" || got.Cwd != "" {
		t.Errorf("service = %+v, want db on 5432 at the repo root", got)
	}
}
