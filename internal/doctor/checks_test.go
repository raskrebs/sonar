package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/install"
	"github.com/raskrebs/sonar/internal/store"
)

// --------------------------------------------------------------- the CLI ---

func TestCLIOnPath(t *testing.T) {
	t.Run("resolves to the running binary", func(t *testing.T) {
		env := fakeEnv(t)
		self := write(t, filepath.Join(t.TempDir(), "sonar"), "#!/bin/sh\n")
		env.Executable = func() (string, error) { return self, nil }
		env.LookPath = func(string) (string, error) { return self, nil }
		got := run(t, env, checkCLIOnPath)
		wantStatus(t, got, StatusOK)
	})

	t.Run("another sonar shadows this one", func(t *testing.T) {
		env := fakeEnv(t)
		dir := t.TempDir()
		self := write(t, filepath.Join(dir, "mine", "sonar"), "#!/bin/sh\n")
		other := write(t, filepath.Join(dir, "theirs", "sonar"), "#!/bin/sh\n")
		env.Executable = func() (string, error) { return self, nil }
		env.LookPath = func(string) (string, error) { return other, nil }

		got := run(t, env, checkCLIOnPath)
		wantStatus(t, got, StatusWarn)
		if !strings.Contains(got.Detail, other) || !strings.Contains(got.Detail, self) {
			t.Errorf("detail = %q, want it to name both binaries", got.Detail)
		}
	})

	t.Run("nothing on PATH at all", func(t *testing.T) {
		env := fakeEnv(t)
		got := run(t, env, checkCLIOnPath)
		wantStatus(t, got, StatusFail)
	})
}

func TestCLIVersionCurrent(t *testing.T) {
	t.Run("a dev build has nothing to compare", func(t *testing.T) {
		env := fakeEnv(t)
		env.Version = "dev"
		wantStatus(t, run(t, env, checkCLIVersionCurrent), StatusSkip)
	})

	t.Run("an unreachable feed skips rather than fails", func(t *testing.T) {
		env := fakeEnv(t)
		wantStatus(t, run(t, env, checkCLIVersionCurrent), StatusSkip)
	})

	t.Run("behind the latest release", func(t *testing.T) {
		env := fakeEnv(t)
		env.LatestRelease = func(context.Context) (string, error) { return "v9.0.0", nil }
		got := run(t, env, checkCLIVersionCurrent)
		wantStatus(t, got, StatusWarn)
		if got.Fix != "sonar update" {
			t.Errorf("fix = %q", got.Fix)
		}
	})

	t.Run("current", func(t *testing.T) {
		env := fakeEnv(t)
		env.LatestRelease = func(context.Context) (string, error) { return "v1.2.3", nil }
		wantStatus(t, run(t, env, checkCLIVersionCurrent), StatusOK)
	})
}

// ---------------------------------------------------------------- config ---

func TestConfigParses(t *testing.T) {
	t.Run("no file is a valid state", func(t *testing.T) {
		wantStatus(t, run(t, fakeEnv(t), checkConfigParses), StatusOK)
	})

	t.Run("a broken file fails with a position and a caret", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, env.ConfigPath, "list: [broken")
		got := run(t, env, checkConfigParses)
		wantStatus(t, got, StatusFail)
		if !got.Fixable {
			t.Error("a broken config is the fix --fix exists for")
		}
		if !strings.Contains(got.Detail, env.ConfigPath+":1:7") {
			t.Errorf("detail = %q, want it to carry <path>:1:7", got.Detail)
		}
		if !strings.Contains(got.Detail, "^") {
			t.Errorf("detail = %q, want a caret under the column", got.Detail)
		}
	})

	t.Run("a valid file parses", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, env.ConfigPath, "list:\n  sort: port\n")
		wantStatus(t, run(t, env, checkConfigParses), StatusOK)
	})

	t.Run("a well-formed file with the wrong types only warns", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, env.ConfigPath, "list:\n  columns: 3\n")
		wantStatus(t, run(t, env, checkConfigParses), StatusWarn)
	})
}

func TestConfigDirWritable(t *testing.T) {
	t.Run("a writable directory", func(t *testing.T) {
		env := fakeEnv(t)
		got := run(t, env, checkConfigDirWritable)
		wantStatus(t, got, StatusOK)
		if _, err := os.Stat(env.ConfigDir); err != nil {
			t.Errorf("the check should have created %s: %v", env.ConfigDir, err)
		}
	})

	t.Run("a read-only directory fails", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Getuid() == 0 {
			t.Skip("file modes do not stop this user from writing")
		}
		env := fakeEnv(t)
		if err := os.MkdirAll(env.ConfigDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(env.ConfigDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(env.ConfigDir, 0o700) })
		wantStatus(t, run(t, env, checkConfigDirWritable), StatusFail)
	})
}

func TestProjectConfig(t *testing.T) {
	t.Run("absent warns and points at sonar init", func(t *testing.T) {
		env := fakeEnv(t)
		got := run(t, env, checkProjectConfig)
		wantStatus(t, got, StatusWarn)
		if !strings.Contains(got.Fix, "sonar init") {
			t.Errorf("fix = %q", got.Fix)
		}
	})

	t.Run("present and valid", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, filepath.Join(env.Project, groups.ConfigName),
			"name: demo\nservices:\n  - name: api\n    port: 3000\n")
		got := run(t, env, checkProjectConfig)
		wantStatus(t, got, StatusOK)
		if !strings.Contains(got.Summary, "demo") || !strings.Contains(got.Summary, "1 service") {
			t.Errorf("summary = %q, want the group name and the service count", got.Summary)
		}
	})

	t.Run("present and invalid fails", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, filepath.Join(env.Project, groups.ConfigName), "name: demo\nservices: [broken")
		wantStatus(t, run(t, env, checkProjectConfig), StatusFail)
	})

	t.Run("the old dotfile name still loads, warns and is fixable", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, filepath.Join(env.Project, groups.LegacyConfigName), "name: demo\n")
		got := run(t, env, checkProjectConfig)
		wantStatus(t, got, StatusWarn)
		if !got.Fixable || !strings.Contains(got.Fix, "git mv "+groups.LegacyConfigName+" "+groups.ConfigName) {
			t.Errorf("fix = %q (fixable %v), want the git mv to the new name", got.Fix, got.Fixable)
		}
		if !strings.Contains(got.Summary, "demo") {
			t.Errorf("summary = %q, want the group it still loads", got.Summary)
		}
	})

	t.Run("a shadowed file warns and is not fixable", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, filepath.Join(env.Project, groups.ConfigName), "name: new\n")
		write(t, filepath.Join(env.Project, groups.LegacyConfigName), "name: old\n")
		got := run(t, env, checkProjectConfig)
		wantStatus(t, got, StatusWarn)
		if got.Fixable {
			t.Error("two files need a person to reconcile them; the check must not be fixable")
		}
		if !strings.Contains(got.Summary, "new") || !strings.Contains(got.Summary, groups.LegacyConfigName) {
			t.Errorf("summary = %q, want the file in use and the ignored one", got.Summary)
		}
	})
}

// ---------------------------------------------------------------- daemon ---

func aliveDaemon() DaemonInfo {
	return DaemonInfo{
		Reachable:       true,
		Version:         "v1.2.3",
		ProtocolVersion: rpc.ProtocolVersion,
		Socket:          "/tmp/sonar.sock",
		PID:             4242,
	}
}

func TestDaemonReachable(t *testing.T) {
	t.Run("no daemon fails and is fixable", func(t *testing.T) {
		got := run(t, fakeEnv(t), checkDaemonReachable)
		wantStatus(t, got, StatusFail)
		if !got.Fixable {
			t.Error("a stopped daemon is exactly what --fix restarts")
		}
	})

	t.Run("a listening daemon", func(t *testing.T) {
		env := fakeEnv(t)
		env.Daemon = func(context.Context) DaemonInfo { return aliveDaemon() }
		got := run(t, env, checkDaemonReachable)
		wantStatus(t, got, StatusOK)
		if !strings.Contains(got.Detail, "4242") {
			t.Errorf("detail = %q, want the pid", got.Detail)
		}
	})

	t.Run("answered from inside the daemon", func(t *testing.T) {
		env := fakeEnv(t)
		env.Daemon = func(context.Context) DaemonInfo {
			d := aliveDaemon()
			d.Local = true
			return d
		}
		wantStatus(t, run(t, env, checkDaemonReachable), StatusOK)
	})
}

func TestDaemonVersionMatches(t *testing.T) {
	t.Run("no daemon skips", func(t *testing.T) {
		wantStatus(t, run(t, fakeEnv(t), checkDaemonVersionMatches), StatusSkip)
	})

	t.Run("same version", func(t *testing.T) {
		env := fakeEnv(t)
		env.Daemon = func(context.Context) DaemonInfo { return aliveDaemon() }
		wantStatus(t, run(t, env, checkDaemonVersionMatches), StatusOK)
	})

	t.Run("a stale daemon warns and names the restart", func(t *testing.T) {
		env := fakeEnv(t)
		env.Daemon = func(context.Context) DaemonInfo {
			d := aliveDaemon()
			d.Version = "v0.9.0"
			return d
		}
		got := run(t, env, checkDaemonVersionMatches)
		wantStatus(t, got, StatusWarn)
		if !strings.Contains(got.Summary, "v0.9.0") || !strings.Contains(got.Summary, "v1.2.3") {
			t.Errorf("summary = %q, want both versions", got.Summary)
		}
		if got.Fix != "sonar daemon restart" {
			t.Errorf("fix = %q", got.Fix)
		}
	})
}

func TestDaemonProtocol(t *testing.T) {
	t.Run("matching majors", func(t *testing.T) {
		env := fakeEnv(t)
		env.Daemon = func(context.Context) DaemonInfo { return aliveDaemon() }
		wantStatus(t, run(t, env, checkDaemonProtocol), StatusOK)
	})

	t.Run("a newer minor still matches", func(t *testing.T) {
		env := fakeEnv(t)
		env.Daemon = func(context.Context) DaemonInfo {
			d := aliveDaemon()
			major, _ := protocolMajor(rpc.ProtocolVersion)
			d.ProtocolVersion = fmt.Sprintf("%d.99.0", major)
			return d
		}
		wantStatus(t, run(t, env, checkDaemonProtocol), StatusOK)
	})

	t.Run("a different major fails", func(t *testing.T) {
		env := fakeEnv(t)
		env.Daemon = func(context.Context) DaemonInfo {
			d := aliveDaemon()
			major, _ := protocolMajor(rpc.ProtocolVersion)
			d.ProtocolVersion = fmt.Sprintf("%d.0.0", major+1)
			return d
		}
		got := run(t, env, checkDaemonProtocol)
		wantStatus(t, got, StatusFail)
		if got.Fix != "sonar daemon restart" {
			t.Errorf("fix = %q", got.Fix)
		}
	})
}

func TestSocketPermissions(t *testing.T) {
	t.Run("windows is a skip, not a failure", func(t *testing.T) {
		env := fakeEnv(t)
		env.GOOS = "windows"
		env.SocketPath = `\\.\pipe\sonar`
		wantStatus(t, run(t, env, checkSocketPermissions), StatusSkip)
	})

	if runtime.GOOS == "windows" {
		return
	}

	t.Run("no socket and no daemon skips", func(t *testing.T) {
		wantStatus(t, run(t, fakeEnv(t), checkSocketPermissions), StatusSkip)
	})

	t.Run("a daemon whose socket vanished fails", func(t *testing.T) {
		env := fakeEnv(t)
		env.Daemon = func(context.Context) DaemonInfo { return aliveDaemon() }
		wantStatus(t, run(t, env, checkSocketPermissions), StatusFail)
	})

	t.Run("0600 is fine, 0666 is not", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, env.SocketPath, "")
		if err := os.Chmod(env.SocketPath, 0o600); err != nil {
			t.Fatal(err)
		}
		wantStatus(t, run(t, env, checkSocketPermissions), StatusOK)

		if err := os.Chmod(env.SocketPath, 0o666); err != nil {
			t.Fatal(err)
		}
		got := run(t, env, checkSocketPermissions)
		wantStatus(t, got, StatusFail)
		if !strings.Contains(got.Detail, "0666") {
			t.Errorf("detail = %q, want the mode it found", got.Detail)
		}
	})
}

func TestDBOK(t *testing.T) {
	t.Run("no database yet warns", func(t *testing.T) {
		got := run(t, fakeEnv(t), checkDBOK)
		wantStatus(t, got, StatusWarn)
	})

	t.Run("a real database reports its schema version", func(t *testing.T) {
		env := fakeEnv(t)
		db, err := store.Open(env.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()

		got := run(t, env, checkDBOK)
		wantStatus(t, got, StatusOK)
		if !strings.Contains(got.Summary, fmt.Sprintf("v%d", store.LatestVersion())) {
			t.Errorf("summary = %q, want schema v%d", got.Summary, store.LatestVersion())
		}
	})
}

// ------------------------------------------------------------ agent tools ---

func TestMCPRegistered(t *testing.T) {
	t.Run("a tool that is not installed skips", func(t *testing.T) {
		env := fakeEnv(t)
		env.fill()
		got := checkMCPRegistered(env, agentTools()[1]) // cursor
		wantStatus(t, got, StatusSkip)
	})

	t.Run("installed but not registered warns and is fixable", func(t *testing.T) {
		env := fakeEnv(t)
		if err := os.MkdirAll(filepath.Join(env.Home, ".cursor"), 0o755); err != nil {
			t.Fatal(err)
		}
		env.fill()
		got := checkMCPRegistered(env, agentTools()[1])
		wantStatus(t, got, StatusWarn)
		if !got.Fixable || got.Fix != "sonar install mcp --cursor" {
			t.Errorf("fix = %q fixable = %v", got.Fix, got.Fixable)
		}
	})

	t.Run("registered in the user config", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, filepath.Join(env.Home, ".cursor", "mcp.json"),
			`{"mcpServers":{"sonar":{"command":"sonar","args":["mcp"]}}}`)
		env.fill()
		got := checkMCPRegistered(env, agentTools()[1])
		wantStatus(t, got, StatusOK)
		if !strings.Contains(got.Summary, "user scope") {
			t.Errorf("summary = %q, want the scope it found", got.Summary)
		}
	})

	t.Run("another server registered is not sonar", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, filepath.Join(env.Home, ".cursor", "mcp.json"),
			`{"mcpServers":{"something-else":{"command":"x"}}}`)
		env.fill()
		wantStatus(t, checkMCPRegistered(env, agentTools()[1]), StatusWarn)
	})

	t.Run("codex is read out of its TOML", func(t *testing.T) {
		env := fakeEnv(t)
		codex := agentTools()[2]
		write(t, filepath.Join(env.Home, ".codex", "config.toml"),
			"model = \"gpt\"\n\n[mcp_servers.sonar]\ncommand = \"sonar\"\nargs = [\"mcp\"]\n")
		env.fill()
		wantStatus(t, checkMCPRegistered(env, codex), StatusOK)
	})

	t.Run("codex without an entry", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, filepath.Join(env.Home, ".codex", "config.toml"), "model = \"gpt\"\n")
		env.fill()
		wantStatus(t, checkMCPRegistered(env, agentTools()[2]), StatusWarn)
	})
}

func TestSkillsInstalled(t *testing.T) {
	t.Run("no Claude Code skips", func(t *testing.T) {
		wantStatus(t, run(t, fakeEnv(t), checkSkillsInstalled), StatusSkip)
	})

	t.Run("installed but missing warns", func(t *testing.T) {
		env := fakeEnv(t)
		if err := os.MkdirAll(filepath.Join(env.Home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := run(t, env, checkSkillsInstalled)
		wantStatus(t, got, StatusWarn)
		if !got.Fixable {
			t.Error("installing the skill is a safe fix")
		}
	})

	t.Run("the bundled skill is installed", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, install.SkillPath(install.ScopeUser, env.Home, ""), install.SkillContent())
		wantStatus(t, run(t, env, checkSkillsInstalled), StatusOK)
	})

	t.Run("a foreign skill is not overwritten silently", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, install.SkillPath(install.ScopeUser, env.Home, ""), "---\nname: sonar\n---\nmine\n")
		got := run(t, env, checkSkillsInstalled)
		wantStatus(t, got, StatusWarn)
		if got.Fixable {
			t.Error("overwriting a file sonar did not write is not a safe fix")
		}
	})
}

func TestHooksInstalled(t *testing.T) {
	t.Run("no Claude Code skips", func(t *testing.T) {
		wantStatus(t, run(t, fakeEnv(t), checkHooksInstalled), StatusSkip)
	})

	t.Run("settings without sonar hooks warns", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, install.SettingsPath(install.ScopeUser, env.Home, ""), `{"hooks":{}}`)
		got := run(t, env, checkHooksInstalled)
		wantStatus(t, got, StatusWarn)
		if !got.Fixable {
			t.Error("installing the hooks is a safe fix")
		}
	})

	t.Run("hooks sonar wrote are found", func(t *testing.T) {
		env := fakeEnv(t)
		path := install.SettingsPath(install.ScopeUser, env.Home, "")
		write(t, path, "{}")
		if _, _, err := install.InstallHooks(path, "sonar", install.ModeAdvise); err != nil {
			t.Fatal(err)
		}
		got := run(t, env, checkHooksInstalled)
		wantStatus(t, got, StatusOK)
		if !strings.Contains(got.Summary, "2 sonar hooks") {
			t.Errorf("summary = %q, want the two hooks install writes", got.Summary)
		}
	})

	t.Run("unreadable settings warn rather than fail the run", func(t *testing.T) {
		env := fakeEnv(t)
		write(t, install.SettingsPath(install.ScopeUser, env.Home, ""), "{not json")
		wantStatus(t, run(t, env, checkHooksInstalled), StatusWarn)
	})
}

// ---------------------------------------------------------------- extras ---

func TestDocker(t *testing.T) {
	t.Run("not installed skips", func(t *testing.T) {
		wantStatus(t, run(t, fakeEnv(t), checkDocker), StatusSkip)
	})

	t.Run("installed but not responding warns", func(t *testing.T) {
		env := fakeEnv(t)
		env.LookPath = func(name string) (string, error) {
			if name == "docker" {
				return "/usr/local/bin/docker", nil
			}
			return "", exec.ErrNotFound
		}
		wantStatus(t, run(t, env, checkDocker), StatusWarn)
	})

	t.Run("hanging is reported as not answering, not as not installed", func(t *testing.T) {
		env := fakeEnv(t)
		env.LookPath = func(string) (string, error) { return "/usr/local/bin/docker", nil }
		env.Docker = func(context.Context) (string, error) {
			return "", fmt.Errorf("%w: `docker info` did not answer within 2s", ErrDockerNotAnswering)
		}
		got := run(t, env, checkDocker)
		wantStatus(t, got, StatusWarn)
		if got.Summary != "docker CLI not answering" {
			t.Errorf("summary = %q, want %q", got.Summary, "docker CLI not answering")
		}
	})

	t.Run("responding is ok", func(t *testing.T) {
		env := fakeEnv(t)
		env.LookPath = func(string) (string, error) { return "/usr/local/bin/docker", nil }
		env.Docker = func(context.Context) (string, error) { return "27.0.1", nil }
		got := run(t, env, checkDocker)
		wantStatus(t, got, StatusOK)
		if !strings.Contains(got.Detail, "27.0.1") {
			t.Errorf("detail = %q", got.Detail)
		}
	})
}

func TestTraySkipsOffMacOS(t *testing.T) {
	env := fakeEnv(t)
	env.GOOS = "linux"
	got := run(t, env, checkTray)
	wantStatus(t, got, StatusSkip)
	if !strings.Contains(got.Summary, "macOS") {
		t.Errorf("summary = %q", got.Summary)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{{0, "0 B"}, {999, "999 B"}, {2048, "2.0 KiB"}, {5 << 20, "5.0 MiB"}} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
