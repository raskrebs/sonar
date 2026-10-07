// Package doctor runs sonar's self-diagnosis: one list of independent checks
// that say whether the CLI, the daemon, the database, the agent integrations
// and this project's config are in the state sonar expects.
//
// The same list backs `sonar doctor` and the `daemon.doctor` RPC the desktop
// app calls, so the app never has to shell out to learn what is wrong. A few
// checks are about the CLI process itself (which binary PATH resolves, how it
// compares with the daemon and with the latest release); those the daemon
// cannot answer from its own process and reports as `skip`, saying so in the
// check's detail.
package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/raskrebs/sonar/internal/config"
	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/store"
)

// Status values, in the order a reader should worry about them.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
	StatusSkip = "skip"
)

// Mode says which process is running the checks. It only changes what the
// CLI-only checks report, never the list of checks: a caller that asked for
// `cli_on_path` gets a row for it either way, so a client's table does not
// change shape depending on who answered.
type Mode string

const (
	// ModeCLI is a `sonar doctor` run in the CLI process.
	ModeCLI Mode = "cli"
	// ModeDaemon is a `daemon.doctor` call answered inside the daemon.
	ModeDaemon Mode = "daemon"
)

// cliOnlyDetail is what a CLI-only check says when the daemon answered.
const cliOnlyDetail = "CLI-only: this check is about the sonar binary you invoked, " +
	"which the daemon cannot see from its own process. Run `sonar doctor` for it."

// DaemonInfo is what the doctor knows about the daemon. The CLI fills it in by
// dialling; the daemon fills it in from its own runtime, which is also why
// this is a plain struct rather than a client handle.
type DaemonInfo struct {
	// Reachable is false when nothing answered on the socket.
	Reachable bool
	// Err is why it was not reachable.
	Err error
	// Local is true when the answer came from inside the daemon process.
	Local bool

	Version         string
	ProtocolVersion string
	Socket          string
	PID             int
	DBPath          string

	// Session is the daemon's answer to `session.status`: who this machine is
	// signed in to the relay as, and where that session is kept. Nil when the
	// daemon could not be asked, or is older than the session methods.
	//
	// It rides along with the daemon probe rather than being a seam of its own
	// because a doctor run dials the daemon exactly once, and because only the
	// daemon may ask: it reads its keychain once per launch, and a second
	// asker on macOS is a second password dialog.
	Session *rpc.SessionStatusResult
}

// Env is the slice of the world the checks read. Everything that a test would
// otherwise have to fake on the real machine — the executable's own path, what
// PATH resolves, the network, docker — is a field here; plain files are not,
// because every test binary already runs with an isolated HOME (internal/testenv)
// and reading a real temp file is more honest than faking os.ReadFile.
type Env struct {
	// Mode is who is running the checks.
	Mode Mode
	// GOOS overrides runtime.GOOS, so the Windows skips can be exercised on
	// any host.
	GOOS string
	// Version is the running process's own version string.
	Version string
	// Project is the directory the project-scoped checks look at. Empty means
	// the process's working directory.
	Project string

	// Home is the user's home directory.
	Home string
	// ConfigPath is the user's config.yaml.
	ConfigPath string
	// ConfigDir is ~/.config/sonar.
	ConfigDir string
	// SocketPath is the address the daemon listens on.
	SocketPath string
	// DBPath is the SQLite database file.
	DBPath string

	// Executable is the running binary's path.
	Executable func() (string, error)
	// LookPath resolves a name against PATH.
	LookPath func(string) (string, error)
	// UID is the current user's numeric id on unix; -1 when unknown.
	UID int
	// Exists reports whether a path is on disk. Nil means the real disk; a
	// test sets it so the checks that look in fixed places, like
	// /Applications, see the machine it describes and not the one it runs on.
	Exists func(string) bool

	// Daemon reports the daemon's identity, or why it could not be reached.
	Daemon func(context.Context) DaemonInfo
	// LatestRelease returns the newest published release tag. An error means
	// GitHub was unreachable within the budget, which is a `skip`, never a
	// failure: a machine offline on purpose is not broken.
	LatestRelease func(context.Context) (string, error)
	// Docker reports the docker daemon's server version, or an error when the
	// CLI is there but the daemon is not answering.
	Docker func(context.Context) (string, error)
}

// check is one diagnostic: its id, whether only the CLI can answer it, and the
// function that looks.
type check struct {
	id      string
	cliOnly bool
	run     func(context.Context, *Env) rpc.DoctorCheck
}

// checks is the whole list, in the order the table prints them: the binary,
// then the daemon, then its storage, then the integrations, then the project,
// then the optional extras.
func checks() []check {
	list := []check{
		{id: "cli_on_path", cliOnly: true, run: checkCLIOnPath},
		{id: "cli_version_current", cliOnly: true, run: checkCLIVersionCurrent},
		{id: "config_parses", run: checkConfigParses},
		{id: "config_dir_writable", run: checkConfigDirWritable},
		{id: "daemon_reachable", run: checkDaemonReachable},
		{id: "daemon_version_matches", cliOnly: true, run: checkDaemonVersionMatches},
		{id: "daemon_protocol", run: checkDaemonProtocol},
		{id: "socket_permissions", run: checkSocketPermissions},
		{id: "db_ok", run: checkDBOK},
		{id: "relay_session", run: checkRelaySession},
	}
	for _, tool := range mcpTools() {
		list = append(list, check{id: tool.id, run: tool.run})
	}
	return append(list,
		check{id: "skills_installed", run: checkSkillsInstalled},
		check{id: "hooks_installed", run: checkHooksInstalled},
		check{id: "project_config", run: checkProjectConfig},
		check{id: "docker", run: checkDocker},
		check{id: "desktop_installed", run: checkDesktopInstalled},
		check{id: "tray", run: checkTray},
	)
}

// IDs lists every check id, in table order. `sonar doctor --only` validates
// against it.
func IDs() []string {
	all := checks()
	out := make([]string, 0, len(all))
	for _, c := range all {
		out = append(out, c.id)
	}
	return out
}

// Prefixes lists the selectors `--only` accepts that are not check ids: the
// part before the dot of every dotted id, so `--only mcp_registered` means all
// three MCP checks.
func Prefixes() []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range IDs() {
		prefix, _, ok := strings.Cut(id, ".")
		if !ok || seen[prefix] {
			continue
		}
		seen[prefix] = true
		out = append(out, prefix)
	}
	sort.Strings(out)
	return out
}

// UnknownSelectors returns the entries of only that name no check.
func UnknownSelectors(only []string) []string {
	var bad []string
	for _, sel := range normalize(only) {
		if !selects([]string{sel}, IDs()) {
			bad = append(bad, sel)
		}
	}
	return bad
}

// selects reports whether any of ids is picked by only.
func selects(only, ids []string) bool {
	for _, id := range ids {
		if picked(only, id) {
			return true
		}
	}
	return false
}

// picked reports whether only selects id. An empty only selects everything.
func picked(only []string, id string) bool {
	if len(only) == 0 {
		return true
	}
	for _, sel := range only {
		if sel == id || strings.HasPrefix(id, sel+".") {
			return true
		}
	}
	return false
}

func normalize(only []string) []string {
	out := make([]string, 0, len(only))
	for _, sel := range only {
		for _, part := range strings.Split(sel, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// Run executes the selected checks and assembles the reply both `sonar doctor`
// and `daemon.doctor` return.
func Run(ctx context.Context, env Env, only []string) rpc.DaemonDoctorResult {
	env.fill()
	sel := normalize(only)

	// The daemon is dialled at most once per run, however many checks ask
	// about it, and once more at the end for daemon_version — a doctor run
	// must not open a connection per check.
	var info DaemonInfo
	var haveInfo bool
	probe := env.Daemon
	daemonOnce := func(ctx context.Context) DaemonInfo {
		if !haveInfo {
			info = probe(ctx)
			haveInfo = true
		}
		return info
	}
	env.Daemon = daemonOnce

	result := rpc.DaemonDoctorResult{OK: true, Checks: []rpc.DoctorCheck{}, Version: env.Version}
	for _, c := range checks() {
		if !picked(sel, c.id) {
			continue
		}
		var got rpc.DoctorCheck
		if c.cliOnly && env.Mode == ModeDaemon {
			got = rpc.DoctorCheck{
				ID:      c.id,
				Status:  StatusSkip,
				Summary: "not checked from the daemon",
				Detail:  cliOnlyDetail,
			}
		} else {
			got = c.run(ctx, &env)
			got.ID = c.id
		}
		if got.Status == StatusFail {
			result.OK = false
		}
		result.Checks = append(result.Checks, got)
	}
	if d := daemonOnce(ctx); d.Reachable {
		result.DaemonVersion = d.Version
	}
	return result
}

// fill installs the real world behind every seam the caller left empty.
func (e *Env) fill() {
	if e.Mode == "" {
		e.Mode = ModeCLI
	}
	if e.GOOS == "" {
		e.GOOS = runtime.GOOS
	}
	if e.Executable == nil {
		e.Executable = os.Executable
	}
	if e.LookPath == nil {
		e.LookPath = exec.LookPath
	}
	if e.UID == 0 {
		e.UID = os.Getuid()
	}
	if e.Home == "" {
		if home, err := os.UserHomeDir(); err == nil {
			e.Home = home
		}
	}
	if e.Project == "" {
		if wd, err := os.Getwd(); err == nil {
			e.Project = wd
		}
	}
	// The four paths default to what every other command resolves, so a
	// caller that only wants the standard run passes none of them and a test
	// that wants a fixture passes exactly the one it cares about.
	if e.ConfigPath == "" {
		e.ConfigPath = config.Path()
	}
	if e.ConfigDir == "" {
		e.ConfigDir = daemon.ConfigDir()
	}
	if e.SocketPath == "" {
		e.SocketPath = daemon.SocketPath()
	}
	if e.DBPath == "" {
		e.DBPath = store.Path()
	}
	if e.Daemon == nil {
		e.Daemon = func(context.Context) DaemonInfo {
			return DaemonInfo{Err: errNoProbe}
		}
	}
	if e.LatestRelease == nil {
		e.LatestRelease = func(context.Context) (string, error) { return "", errNoProbe }
	}
	if e.Docker == nil {
		e.Docker = func(context.Context) (string, error) { return "", errNoProbe }
	}
}

// errNoProbe is what an unwired seam reports: the caller did not give the
// doctor a way to look, so the check that needs it skips.
var errNoProbe = errors.New("not available in this process")

// gitRootOf is the repository root above dir, or "" when dir is not in one.
func gitRootOf(dir string) string {
	root, _, ok := groups.Find(dir)
	if !ok {
		return ""
	}
	return root
}

// home joins a path under the user's home directory, or "" without one.
func (e *Env) homePath(parts ...string) string {
	if e.Home == "" {
		return ""
	}
	return filepath.Join(append([]string{e.Home}, parts...)...)
}

// exists is Exists, or the real disk when it is not set.
func (e *Env) exists(path string) bool {
	if e.Exists != nil {
		return e.Exists(path)
	}
	return exists(path)
}

func exists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
