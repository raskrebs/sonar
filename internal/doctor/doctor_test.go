package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
)

// fakeEnv is an Env whose every seam answers without touching the machine: no
// PATH lookup escapes it, no socket is dialled, no release is fetched. Paths
// point into t.TempDir(), so a check that reads a file reads a real one the
// test wrote.
func fakeEnv(t *testing.T) *Env {
	t.Helper()
	home := t.TempDir()
	return &Env{
		Mode:       ModeCLI,
		GOOS:       runtime.GOOS,
		Version:    "v1.2.3",
		Home:       home,
		Project:    home,
		ConfigPath: filepath.Join(home, ".config", "sonar", "config.yaml"),
		ConfigDir:  filepath.Join(home, ".config", "sonar"),
		SocketPath: filepath.Join(home, ".config", "sonar", "daemon.sock"),
		DBPath:     filepath.Join(home, ".config", "sonar", "sonar.db"),
		UID:        os.Getuid(),
		Executable: func() (string, error) { return filepath.Join(home, "bin", "sonar"), nil },
		LookPath:   func(string) (string, error) { return "", exec.ErrNotFound },
		Daemon:     func(context.Context) DaemonInfo { return DaemonInfo{Err: errors.New("no daemon")} },
		LatestRelease: func(context.Context) (string, error) {
			return "", errors.New("no network in tests")
		},
		Docker: func(context.Context) (string, error) { return "", errors.New("no docker in tests") },
		// Only what a test put under its own temp directories exists: a Sonar.app
		// in this machine's /Applications is not part of any case here.
		Exists: func(path string) bool {
			return strings.HasPrefix(path, os.TempDir()) && exists(path)
		},
	}
}

// write puts content at path, creating parents.
func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, env *Env, fn func(context.Context, *Env) rpc.DoctorCheck) rpc.DoctorCheck {
	t.Helper()
	env.fill()
	return fn(context.Background(), env)
}

func wantStatus(t *testing.T, got rpc.DoctorCheck, status string) {
	t.Helper()
	if got.Status != status {
		t.Fatalf("status = %q (%s / %s), want %q", got.Status, got.Summary, got.Detail, status)
	}
}

// -------------------------------------------------------------- selectors ---

func TestOnlySelectsExactIDsAndDottedFamilies(t *testing.T) {
	ids := IDs()
	if len(ids) < 10 {
		t.Fatalf("only %d checks: %v", len(ids), ids)
	}
	if !picked(nil, "db_ok") {
		t.Error("an empty --only should select everything")
	}
	if !picked([]string{"mcp_registered"}, "mcp_registered.cursor") {
		t.Error("--only mcp_registered should select the whole family")
	}
	if picked([]string{"mcp_registered"}, "db_ok") {
		t.Error("--only mcp_registered should not select db_ok")
	}
	if got := UnknownSelectors([]string{"db_ok", "mcp_registered", "nope"}); len(got) != 1 || got[0] != "nope" {
		t.Errorf("unknown selectors = %v, want [nope]", got)
	}
	if got := Prefixes(); len(got) != 1 || got[0] != "mcp_registered" {
		t.Errorf("prefixes = %v, want [mcp_registered]", got)
	}
}

func TestRunKeepsOnlyTheSelectedChecks(t *testing.T) {
	env := *fakeEnv(t)
	got := Run(context.Background(), env, []string{"db_ok,tray"})
	if len(got.Checks) != 2 {
		t.Fatalf("ran %d checks, want 2: %v", len(got.Checks), got.Checks)
	}
	if got.Checks[0].ID != "db_ok" || got.Checks[1].ID != "tray" {
		t.Errorf("ids = %s, %s", got.Checks[0].ID, got.Checks[1].ID)
	}
	if got.Version != "v1.2.3" {
		t.Errorf("version = %q", got.Version)
	}
}

func TestRunDialsTheDaemonOnce(t *testing.T) {
	env := *fakeEnv(t)
	calls := 0
	env.Daemon = func(context.Context) DaemonInfo {
		calls++
		return DaemonInfo{Reachable: true, Version: "v1.2.3", ProtocolVersion: rpc.ProtocolVersion}
	}
	Run(context.Background(), env, nil)
	if calls != 1 {
		t.Errorf("dialled the daemon %d times, want 1", calls)
	}
}

func TestDaemonModeSkipsTheCLIOnlyChecks(t *testing.T) {
	env := *fakeEnv(t)
	env.Mode = ModeDaemon
	got := Run(context.Background(), env, nil)

	cliOnly := map[string]bool{"cli_on_path": true, "cli_version_current": true, "daemon_version_matches": true}
	seen := 0
	for _, c := range got.Checks {
		if !cliOnly[c.ID] {
			continue
		}
		seen++
		wantStatus(t, c, StatusSkip)
		if !strings.Contains(c.Detail, "CLI-only") {
			t.Errorf("%s detail = %q, want it to say the check is CLI-only", c.ID, c.Detail)
		}
	}
	if seen != len(cliOnly) {
		t.Errorf("saw %d CLI-only checks, want %d", seen, len(cliOnly))
	}
	// The list of rows must not change shape with the answering process.
	if len(got.Checks) != len(IDs()) {
		t.Errorf("daemon mode ran %d checks, want all %d", len(got.Checks), len(IDs()))
	}
}

func TestRunIsNotOKOnlyWhenSomethingFailed(t *testing.T) {
	env := *fakeEnv(t)
	// No daemon is a fail.
	if got := Run(context.Background(), env, []string{"daemon_reachable"}); got.OK {
		t.Error("a failing check must make the run not ok")
	}
	// A missing .sonar.yaml is only a warning.
	if got := Run(context.Background(), env, []string{"project_config"}); !got.OK {
		t.Error("a warning must leave the run ok")
	}
}
