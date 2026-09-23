package rpc

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/state"
)

// This file declares the params and results of every daemon method named in
// the daemon spec's method table and in cross-spec contract §4. In slice F0
// they are placeholders: they fix the wire shape and drive schema generation,
// and the slices that implement each method fill in the behaviour behind them.
// Changing a field here changes the published protocol, so keep it in step
// with the contract.

// ---------------------------------------------------------------- shared ---

// HostParams is the optional `host` a method takes to act on a registered
// remote host instead of this machine (remote-hosts spec, "Actions on a remote
// host"). Absent means localhost, which is what every client written before
// remote hosts existed sends, so their calls keep meaning what they meant
// (contract §39).
//
// The daemon accepts it on every method it serves except `state.*`, `stream.*`,
// `daemon.hello`, `daemon.shutdown`, `daemon.bridged` and the `remote.*`
// family. It is embedded in the params types below where it is worth spelling
// out in the schema. `session.register` takes it and refuses it: a session
// token must never be forwarded to a machine someone else administers.
type HostParams struct {
	// Host names a registered remote host. Empty or "localhost" is this
	// machine.
	Host string `json:"host,omitempty"`
}

// Selector addresses one port (contract §3). Exactly one of Port, PID, RunID,
// ProxyID and Key is set; BindAddress only disambiguates Port.
type Selector struct {
	HostParams
	Port        *int    `json:"port,omitempty"`
	PID         *int    `json:"pid,omitempty"`
	BindAddress *string `json:"bind_address,omitempty"`
	RunID       *string `json:"run_id,omitempty"`
	ProxyID     *string `json:"proxy_id,omitempty"`
	// Key is a delta key handed straight back as a selector: `"<port>"`,
	// `"<port>:<bind_address>"`, or either of those behind a `"<host>/"`
	// prefix naming a registered host. A client that holds the key the stream
	// gave it does not have to take it apart to act on the row. The daemon
	// expands it into Host, Port and BindAddress before any handler sees it,
	// so it never combines with them.
	Key string `json:"key,omitempty"`
}

// MutationResult is the minimum every mutating method returns (contract §3).
// Affected holds port keys ("<port>:<bind_address>").
type MutationResult struct {
	OK       bool     `json:"ok"`
	Affected []string `json:"affected"`
}

// KillEnvelope is the result of every kill-shaped method (contract §3). The row
// type is state.KillResult, the single Go type the killer, the CLI and the
// daemon all produce, so it keeps the plain name in the generated schema
// (contract §17) and the envelope around it is named for what it is.
type KillEnvelope struct {
	MutationResult
	Results []state.KillResult `json:"results"`
	// Released is how many claimed ports a `groups.kill` with release gave
	// back (`sonar down`). Zero for every other kill.
	Released int `json:"released,omitempty"`
}

// Empty is the params or result of a method that takes or returns nothing.
type Empty struct{}

// OKResult is a bare acknowledgement.
type OKResult struct {
	OK bool `json:"ok"`
}

// CapabilityAutoPorts is announced by a daemon that understands the config
// format v0.8.0 introduced: `port: auto`, `env:` and ${…} references. A client
// holding such a file checks for it before handing the file over, because an
// older daemon parses it itself and reports it as invalid — blaming the file
// for the daemon's age.
const CapabilityAutoPorts = "groups.autoports"

// CapabilityEnv is announced by a daemon that serves `groups.env`. The CLI
// checks for it before calling, so a daemon from before the method answers
// with "restart it" rather than the dispatcher's generic unknown-method error.
const CapabilityEnv = "groups.env"

// CapabilityKillOnly is announced by a daemon whose `groups.kill` takes
// `only`. The CLI checks for it before sending the field, because an older
// daemon drops a field it does not know and would stop the whole group.
const CapabilityKillOnly = "groups.kill.only"

// Include lists the optional per-subscriber enrichments ("stats", "health").
type Include []string

// ---------------------------------------------------------------- daemon ---

type DaemonHelloParams struct {
	Client        string `json:"client"` // cli | app | mcp | tray
	ClientVersion string `json:"client_version"`
	Keepalive     bool   `json:"keepalive,omitempty"`
}

type DaemonHelloResult struct {
	ProtocolVersion string   `json:"protocol_version"`
	DaemonVersion   string   `json:"daemon_version"`
	PID             int      `json:"pid"`
	StartedAt       string   `json:"started_at"`
	Capabilities    []string `json:"capabilities"`
	Socket          string   `json:"socket"`
	BinaryPath      string   `json:"binary_path"`
	Keepalive       bool     `json:"keepalive"`
}

type DaemonStatusResult struct {
	PID         int    `json:"pid"`
	Uptime      string `json:"uptime"`
	Subscribers int    `json:"subscribers"`
	LastScanAt  string `json:"last_scan_at"`
	// ScanIntervalMs is the adaptive port-scan cadence right now; it moves
	// between the base and a ceiling as scans come back unchanged.
	ScanIntervalMs int `json:"scan_interval_ms"`
	// ScanBaseIntervalMs and StatsIntervalMs are the effective settings
	// behind it: `daemon.scan_interval` and `daemon.stats_interval` as this
	// daemon resolved them at startup. Both are read once, so a config edit
	// needs a daemon restart and these are how you check that it took.
	ScanBaseIntervalMs int `json:"scan_base_interval_ms"`
	StatsIntervalMs    int `json:"stats_interval_ms"`
	// Scans counts the port scans this daemon has run. Two clients reading
	// through the daemon must not make it grow faster than one does.
	Scans  int64  `json:"scans"`
	DBPath string `json:"db_path"`
}

// DaemonSchemaResult is the JSON Schema bundle this package generates.
type DaemonSchemaResult struct {
	Schema json.RawMessage `json:"schema"`
}

// DoctorCheck is one diagnostic `sonar doctor` and `daemon.doctor` report. It
// is the single Go type behind the table, the CLI's `--json` and the wire
// result (contract §17), so a client that renders one renders the other.
type DoctorCheck struct {
	// ID names the check. Checks that exist once per external tool are
	// dotted: `mcp_registered.claude_code`.
	ID string `json:"id"`
	// Status is ok, warn, fail or skip. Only `fail` makes the run not ok.
	Status string `json:"status"`
	// Summary is the one line a table row shows.
	Summary string `json:"summary"`
	// Detail is the evidence: paths, versions, the parse error. It is also
	// where a check the daemon cannot run from its own process says so.
	Detail string `json:"detail,omitempty"`
	// Fix is the human hint: what to run, or what to edit.
	Fix string `json:"fix,omitempty"`
	// Fixable says `sonar doctor --fix` can repair this one unattended.
	Fixable bool `json:"fixable"`
}

type DaemonDoctorParams struct {
	HostParams
	// Only keeps just these checks. An entry matches a check id exactly, or
	// every check under it when the id is dotted (`mcp_registered`).
	Only []string `json:"only,omitempty"`
	// Project is the directory the project-scoped checks look at. Empty means
	// the process's own working directory, which for the daemon is wherever it
	// was started, so a client that cares always sends it.
	Project string `json:"project,omitempty"`
}

// DaemonDoctorResult is what both `sonar doctor --json` and `daemon.doctor`
// return. OK is false when any check failed.
type DaemonDoctorResult struct {
	OK     bool          `json:"ok"`
	Checks []DoctorCheck `json:"checks"`
	// Version is the version of the process that ran the checks: the CLI for
	// `sonar doctor`, the daemon for `daemon.doctor`.
	Version string `json:"version"`
	// DaemonVersion is the daemon's version, empty when it is not reachable.
	DaemonVersion string `json:"daemon_version"`
}

// ----------------------------------------------------------------- state ---

type StateSnapshotParams struct {
	Include Include `json:"include,omitempty"`
	// Hosts selects which machines' rows the reply carries. Absent or empty is
	// localhost only, which is what every client written before remote hosts
	// existed asks for; ["*"] is every registered host; any other list is
	// exactly that set of names, so a client that wants localhost alongside a
	// remote host names both (remote-hosts spec, "Multiplexing").
	Hosts []string `json:"hosts,omitempty"`
}

type StateSubscribeParams struct {
	Include Include `json:"include,omitempty"`
	Events  bool    `json:"events,omitempty"`
	// Hosts is StateSnapshotParams.Hosts, per subscriber.
	Hosts []string `json:"hosts,omitempty"`
}

// ----------------------------------------------------------------- ports ---

type PortsListParams struct {
	HostParams
	Group     *string `json:"group,omitempty"`
	Filter    *string `json:"filter,omitempty"` // docker | user | system
	All       bool    `json:"all,omitempty"`
	IPVersion *string `json:"ip_version,omitempty"`
	Include   Include `json:"include,omitempty"`
	// Session keeps only the ports an agent session started (spec 2 §3). It
	// is a daemon-side filter because a session lives in the daemon, not in
	// the scan.
	Session *string `json:"session,omitempty"`
}

type PortsListResult struct {
	Ports []state.Port `json:"ports"`
}

type PortsInspectResult struct {
	Port        state.Port   `json:"port"`
	LogSources  []string     `json:"log_sources"`
	Connections []Connection `json:"connections"`
}

// Connection is one established peer of a listening socket.
type Connection struct {
	RemotePort int `json:"remote_port"`
	RemotePID  int `json:"remote_pid"`
}

type PortsKillParams struct {
	HostParams
	Targets  []Selector `json:"targets"`
	Tree     bool       `json:"tree,omitempty"`
	Force    bool       `json:"force,omitempty"`
	GraceMs  int        `json:"grace_ms,omitempty"`
	Escalate *bool      `json:"escalate,omitempty"`
	DryRun   bool       `json:"dry_run,omitempty"`
}

type PortsRenameParams struct {
	Selector
	Name *string `json:"name" jsonschema:"nullable"`
}

type PortsRenameResult struct {
	MutationResult
	Key  string  `json:"key"`
	Name *string `json:"name" jsonschema:"nullable"`
}

type PortsNextParams struct {
	HostParams
	Start    int     `json:"start,omitempty"`
	End      int     `json:"end,omitempty"`
	Count    int     `json:"count,omitempty"`
	ClaimKey *string `json:"claim_key,omitempty"`
}

type PortsNextResult struct {
	Ports []int `json:"ports"`
}

type PortsWaitParams struct {
	HostParams
	Ports      []int   `json:"ports,omitempty"`
	RunID      *string `json:"run_id,omitempty"`
	Any        bool    `json:"any,omitempty"`
	HTTP       *string `json:"http,omitempty"`
	TimeoutMs  int     `json:"timeout_ms"`
	IntervalMs int     `json:"interval_ms,omitempty"`
}

type PortsWaitChunk struct {
	Port    int    `json:"port"`
	ReadyAt string `json:"ready_at"`
}

type PortsWaitEnd struct {
	Ready    []int `json:"ready"`
	TimedOut []int `json:"timed_out"`
}

type PortsHealthParams struct {
	HostParams
	Ports []int `json:"ports,omitempty"`
}

type PortsHealthResult struct {
	Results []PortHealth `json:"results"`
}

type PortHealth struct {
	Port      int    `json:"port"`
	Status    string `json:"status" jsonschema:"enum=ok,enum=fail,enum=unknown"`
	Code      int    `json:"code"`
	LatencyMs int64  `json:"latency_ms"`
	// Reason is the probe's own verdict ("refused", "timeout", "non-http")
	// behind a `fail`. Advisory: clients branch on Status.
	Reason string `json:"reason,omitempty"`
}

type PortsLogsParams struct {
	Selector
	Lines  int  `json:"lines,omitempty"`
	Follow bool `json:"follow,omitempty"`
}

// PortsLogsResult is the unary reply (follow: false). With follow: true the
// method also returns a subscription_id and pushes PortsLogsChunk.
type PortsLogsResult struct {
	Source         string   `json:"source"`
	Lines          []string `json:"lines"`
	Truncated      bool     `json:"truncated"`
	SubscriptionID string   `json:"subscription_id,omitempty"`
}

type PortsLogsChunk struct {
	Source string `json:"source"`
	Line   string `json:"line"`
}

type PortsGraphResult struct {
	Connections []GraphEdge `json:"connections"`
}

type GraphEdge struct {
	FromPort    int    `json:"from_port"`
	FromPID     int    `json:"from_pid"`
	FromProcess string `json:"from_process"`
	ToPort      int    `json:"to_port"`
	ToPID       int    `json:"to_pid"`
	ToProcess   string `json:"to_process"`
}

type PortsHistoryParams struct {
	HostParams
	Port  *int    `json:"port,omitempty"`
	Since *string `json:"since,omitempty"`
	Limit int     `json:"limit,omitempty"`
}

type PortsHistoryResult struct {
	Events []HistoryEvent `json:"events"`
}

type HistoryEvent struct {
	At          string `json:"at"`
	Kind        string `json:"kind"`
	Port        int    `json:"port"`
	PID         int    `json:"pid"`
	DisplayName string `json:"display_name"`
	Group       string `json:"group"`
}

// ---------------------------------------------------------------- groups ---

type GroupsListResult struct {
	Groups []state.Group `json:"groups"`
}

type GroupsInspectParams struct {
	HostParams
	Name string `json:"name"`
}

type GroupsInspectResult struct {
	state.Group
	Ports []state.Port `json:"ports"`
}

type GroupsKillParams struct {
	HostParams
	Name string `json:"name"`
	// ConfigPath names the group by its sonar.yaml instead of by name, for a
	// caller that knows the file but not the name the daemon publishes its
	// group under (`<project>@<worktree>` in a linked worktree, an alias).
	ConfigPath *string `json:"config_path,omitempty"`
	Force      bool    `json:"force,omitempty"`
	GraceMs    int     `json:"grace_ms,omitempty"`
	DryRun     bool    `json:"dry_run,omitempty"`
	// Release is `sonar down`: besides the group's listening ports it stops
	// every run sonar started in the group, port or not, and releases the
	// claims the group's `port: auto` services hold. A group with a config
	// and nothing running is not an error then: its claims are still released.
	Release bool `json:"release,omitempty"`
	// Only restricts the kill to these services of the group's `sonar.yaml`,
	// the way groups.start takes it: their listening ports, and with Release
	// the runs sonar started under their names and the claims they hold. A
	// name the file does not declare is not_found; a group with no file
	// cannot take it.
	Only []string `json:"only,omitempty"`
}

type GroupsStartParams struct {
	HostParams
	Name             *string  `json:"name,omitempty"`
	ConfigPath       *string  `json:"config_path,omitempty"`
	Only             []string `json:"only,omitempty"`
	AllowOutsideHome bool     `json:"allow_outside_home,omitempty"`
	// Env is the environment the services start in, layered over the
	// daemon's own. The CLI sends its shell's, so a service sees the PATH,
	// toolchain and virtualenv it was started from rather than the daemon's.
	// Omitted, the services get the daemon's environment.
	Env map[string]string `json:"env,omitempty"`
}

type GroupsStartResult struct {
	MutationResult
	SubscriptionID string `json:"subscription_id"`
	// StartID is shared by every run this call starts, so the services
	// brought up together can be told apart from the ones already running.
	StartID string `json:"start_id,omitempty"`
}

// GroupsEnvParams names the file `groups.env` reads, the way groups.start
// takes it: a group by name, or a config by path.
type GroupsEnvParams struct {
	HostParams
	Name       *string `json:"name,omitempty"`
	ConfigPath *string `json:"config_path,omitempty"`
}

// GroupsEnvResult is every service of the file, in the file's order, with the
// port and the environment groups.start would give it — and nothing started.
// A `port: auto` service that is not running has its port claimed by this
// call, the same claim a start makes, so the answer holds until the claim
// expires or the service comes up elsewhere.
type GroupsEnvResult struct {
	MutationResult
	Group      string             `json:"group"`
	ConfigPath string             `json:"config_path"`
	Services   []GroupsEnvService `json:"services"`
}

// The sources a resolved port can have, the values GroupsEnvService.Source
// takes. A fixed port is the file's; a running service keeps the port it is
// on; a `port: auto` service that is not running is given its claim.
const (
	PortSourceFixed   = "fixed"
	PortSourceRunning = "running"
	PortSourceClaimed = "claimed"
)

// AllPortSources is the enum used by schema generation; the schema test pins
// the struct tag on GroupsEnvService.Source to it.
var AllPortSources = []string{PortSourceFixed, PortSourceRunning, PortSourceClaimed}

// GroupsEnvService is one service's resolved port and environment.
type GroupsEnvService struct {
	Name string `json:"name"`
	// Port is the port the service would be told to bind: its fixed port,
	// the one it is already listening on, or its claim. Zero for a service
	// that declares none.
	Port int `json:"port,omitempty"`
	// URL is http://localhost:<port>, what ${url} expands to. Empty with no
	// port.
	URL string `json:"url,omitempty"`
	// Source says where Port came from: "fixed" (the file), "running" (the
	// service is up and keeps its port) or "claimed" (a `port: auto` service
	// that is not running). Empty for a service with no port.
	Source string `json:"source,omitempty" jsonschema:"enum=fixed,enum=running,enum=claimed"`
	// Env is PORT, for a service with a port, and the service's own `env:`
	// with its references expanded: what the service sees on top of the
	// caller's environment when it is started.
	Env map[string]string `json:"env"`
}

// GroupsStartChunk is one service's outcome, pushed as it happens: it was
// started (pid and log_path), it was skipped (with the reason), or it could not
// be started (error).
type GroupsStartChunk struct {
	Service string `json:"service"`
	PID     int    `json:"pid,omitempty"`
	// Port is the port a started service was told to bind: its fixed port,
	// or the one assigned for `port: auto`. Zero for a service with none.
	Port    int    `json:"port,omitempty"`
	LogPath string `json:"log_path,omitempty"`
	// RunID is the run a started service became: the id `runs.list` reports
	// it under and `ports.kill {run_id}` stops it by.
	RunID string `json:"run_id,omitempty"`
	// LogOffset is how long log_path already was before this start. The file
	// is appended to across runs, so a client following it starts here to
	// show this run and not the ones before.
	LogOffset int64  `json:"log_offset,omitempty"`
	Skipped   bool   `json:"skipped,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Error     string `json:"error,omitempty"`
}

type GroupsStartEnd struct {
	Started []string `json:"started"`
	Skipped []string `json:"skipped"`
	Errors  []string `json:"errors"`
}

type GroupsAssignParams struct {
	Selector
	Group *string `json:"group" jsonschema:"nullable"`
}

type GroupsAssignResult struct {
	MutationResult
	Key   string  `json:"key"`
	Group *string `json:"group" jsonschema:"nullable"`
}

// GroupsRenameParams renames a project (step 5A.6). Name is the project's name
// or the name of any of its checkout groups: the rename always applies to the
// project, so the main checkout's group becomes To and every linked worktree's
// group becomes `<To>@<worktree>`. A project whose main checkout has a
// `sonar.yaml` is renamed by writing `name:` into that file; one without keeps
// the new name in the daemon.
//
// Errors: invalid_params for an empty name or a To that is empty or holds `@`,
// `/` or whitespace, and for a group whose name is not a project's to change —
// a manual group (pins name it), a group a `sonar start --group` named, or a
// Compose project; not_found for an unknown group; conflict when a group
// outside the project already has a name the rename would give; invalid_config
// when the edited file would no longer validate.
type GroupsRenameParams struct {
	HostParams
	Name string `json:"name"`
	To   string `json:"to"`
}

// GroupsRenameResult carries in Affected the new name of every group the rename
// changed — the project's and each checkout's — sorted, and in Name the
// project's new name. Affected is empty when the project already had that name.
type GroupsRenameResult struct {
	MutationResult
	Name string `json:"name"`
}

// GroupConfig is a `sonar.yaml` as the protocol carries it: the group name,
// the services as contract rows, and the extra ports the file claims. It is
// the `config` of groups.config.get and groups.config.set (contract §13.2).
type GroupConfig struct {
	Name     string          `json:"name"`
	Services []state.Service `json:"services"`
	Ports    []int           `json:"ports"`
	// WorktreePorts is the file's worktree_ports: how many ports a claim for
	// this project takes when it names no count (step 5A.7). Null when the
	// file has no such key.
	WorktreePorts *int `json:"worktree_ports" jsonschema:"nullable"`
}

type GroupsConfigGetParams struct {
	HostParams
	Name *string `json:"name,omitempty"`
	Path *string `json:"path,omitempty"`
}

type GroupsConfigGetResult struct {
	Path   string      `json:"path"`
	Config GroupConfig `json:"config"`
}

// GroupsConfigSetParams is one atomic edit of a `sonar.yaml` (contract §13.2,
// extended by step 5A.4). The four lists may be combined in a single call and
// are applied in this order — Remove, Rename, Add, then the Services metadata
// patches — with each list seeing the file as the previous ones left it.
// Nothing is written unless the whole edit succeeds and the result still
// validates, so a call that fails leaves the file byte-identical.
type GroupsConfigSetParams struct {
	HostParams
	Path string `json:"path"`
	// Services patches the metadata of services that already exist.
	Services []groups.ServiceEdit `json:"services,omitempty"`
	// Add appends services. A name or a port the file already uses is
	// `conflict`.
	Add []groups.ServiceAdd `json:"add,omitempty"`
	// Rename renames services everywhere in the file, depends_on references
	// included. An unknown `from` is `not_found`, a `to` already in the file is
	// `conflict`.
	Rename []groups.ServiceRename `json:"rename,omitempty"`
	// Remove deletes services and drops them from every other service's
	// depends_on. An unknown name is `not_found`.
	Remove []string `json:"remove,omitempty"`
	// WorktreePorts edits the top-level worktree_ports key (step 5A.7):
	// omitted leaves it alone, a number (1-100) writes it, and null removes
	// it. A value out of range is `invalid_config`, like any edit that would
	// leave the file invalid.
	WorktreePorts *int `json:"worktree_ports,omitempty" jsonschema:"nullable"`

	// WorktreePortsSent records that worktree_ports was present, null
	// included, because a pointer alone cannot tell null from absent.
	// UnmarshalJSON fills it; a Go caller sets it by hand.
	WorktreePortsSent bool `json:"-" jsonschema:"-"`
}

// UnmarshalJSON decodes the params and remembers whether worktree_ports was
// sent at all.
func (p *GroupsConfigSetParams) UnmarshalJSON(data []byte) error {
	type plain GroupsConfigSetParams
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	*p = GroupsConfigSetParams(v)
	_, p.WorktreePortsSent = keys[groups.FieldWorktreePorts]
	return nil
}

// WorktreePortsChange is the edit the worktree_ports field asks for.
func (p GroupsConfigSetParams) WorktreePortsChange() groups.IntChange {
	switch {
	case !p.WorktreePortsSent:
		return groups.IntChange{}
	case p.WorktreePorts == nil:
		return groups.ClearInt()
	default:
		return groups.SetInt(*p.WorktreePorts)
	}
}

// GroupsConfigSetResult is the file after the write. Affected carries the
// service names the edit touched, in the order it applied them — a removed
// name, a rename's new name, an added name, a patched name: this method mutates
// a config, not a port, so there is no port key to report (step 1A.7).
type GroupsConfigSetResult struct {
	MutationResult
	Path   string      `json:"path"`
	Config GroupConfig `json:"config"`
}

type GroupsReloadResult struct {
	Loaded int             `json:"loaded"`
	Errors []ConfigProblem `json:"errors"`
}

// ConfigProblem is one `sonar.yaml` that could not be used.
type ConfigProblem struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// GroupsInitParams asks for a proposed `sonar.yaml` for the checkout at
// RootDir. Write is the contract's opt-in to actually writing it, so the
// default is a preview; Force is the wire form of `sonar init --force` and
// overwrites an existing file, while Merge appends into one instead (contract
// §4, §16, step 5A.4). Force and Merge are mutually exclusive.
type GroupsInitParams struct {
	HostParams
	RootDir string `json:"root_dir"`
	Write   bool   `json:"write,omitempty"`
	Force   bool   `json:"force,omitempty"`
	Merge   bool   `json:"merge,omitempty"`
	// Services replaces the proposed service list with the caller's own, so a
	// curated file can be written in one call. An entry that names a port the
	// proposal also found keeps that proposal's cmd and cwd.
	Services []groups.ServiceAdd `json:"services,omitempty"`
}

type GroupsInitResult struct {
	MutationResult
	Path     string      `json:"path"`
	YAML     string      `json:"yaml"`
	Proposal state.Group `json:"proposal"`
}

// ------------------------------------------------------------------ runs ---

type RunsRegisterParams struct {
	HostParams
	PID       int     `json:"pid"`
	PPID      int     `json:"ppid"`
	Group     string  `json:"group"`
	Name      string  `json:"name"`
	Cmd       string  `json:"cmd"`
	Cwd       string  `json:"cwd"`
	PortHint  *int    `json:"port_hint,omitempty"`
	StartedAt string  `json:"started_at"`
	ID        *string `json:"id,omitempty"`
	// Session is the agent session that asked for this run (spec 2 §3). The
	// caller detects it: `sonar start` reads its own environment, which is the
	// agent's, while the daemon's is not.
	Session *state.Session `json:"session,omitempty"`
	// AllowOutsideHome opts out of the daemon's refusal to record a run whose
	// cwd is outside the user's home (daemon spec, "Transport details"). The
	// CLI sets it; the MCP server does not.
	AllowOutsideHome bool `json:"allow_outside_home,omitempty"`
}

type RunsRegisterResult struct {
	ID string `json:"id"`
}

type RunsUnregisterParams struct {
	HostParams
	PID int `json:"pid"`
	// ExitCode is how the run ended, for a caller that waited on it —
	// `sonar start` without --detach. Absent simply forgets the run.
	ExitCode *int `json:"exit_code,omitempty"`
	// Stopped says the run was asked to stop (a Ctrl+C), so a non-zero exit
	// code is not a crash.
	Stopped bool `json:"stopped,omitempty"`
}

type RunsListResult struct {
	Runs []RunRecord `json:"runs"`
	// Exited is the runs that have ended, newest first, with their exit code
	// and the last lines they logged. The daemon keeps a bounded history in
	// memory, so it starts over when the daemon restarts.
	Exited []RunRecord `json:"exited"`
}

type RunRecord struct {
	ID        string `json:"id"`
	PID       int    `json:"pid"`
	Group     string `json:"group"`
	Name      string `json:"name"`
	Cmd       string `json:"cmd"`
	Cwd       string `json:"cwd"`
	StartedAt string `json:"started_at"`
	Ports     []int  `json:"ports"`
	// PortHint is the port `sonar start --port` said this run would bind, or
	// the one groups.start assigned a `port: auto` service.
	PortHint *int `json:"port_hint,omitempty"`
	// URL is where that port answers, for a run that has one.
	URL string `json:"url,omitempty"`
	// Status is "starting" while a run with a port hint has not bound it yet,
	// "running" otherwise, and "exited" for a run in the exited list.
	Status string `json:"status"`
	// ConfigPath, StartID and Origin say where the run came from: the
	// sonar.yaml it was started from, the `groups.start` that started it with
	// its siblings, and the client that asked (cli, app, mcp).
	ConfigPath string `json:"config_path,omitempty"`
	StartID    string `json:"start_id,omitempty"`
	Origin     string `json:"origin,omitempty"`
	// LogPath is the file a detached run's output goes to.
	LogPath string `json:"log_path,omitempty"`
	// ExitCode, Reason, ExitedAt and LastLines are filled in for a run that
	// has ended. Reason is exited (code 0), crashed (any other code) or
	// stopped (sonar or the user asked it to stop).
	ExitCode  *int     `json:"exit_code,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	ExitedAt  string   `json:"exited_at,omitempty"`
	LastLines []string `json:"last_lines,omitempty"`
}

type RunsSpawnParams struct {
	HostParams
	Argv             []string          `json:"argv"`
	Cwd              string            `json:"cwd"`
	Env              map[string]string `json:"env,omitempty"`
	Group            *string           `json:"group,omitempty"`
	Name             *string           `json:"name,omitempty"`
	PortHint         *int              `json:"port_hint,omitempty"`
	Session          *state.Session    `json:"session,omitempty"`
	AllowOutsideHome bool              `json:"allow_outside_home,omitempty"`
}

type RunsSpawnResult struct {
	MutationResult
	RunID   string `json:"run_id"`
	PID     int    `json:"pid"`
	LogPath string `json:"log_path"`
}

// ---------------------------------------------------------------- claims ---

// ClaimsAcquireParams is the `claim_port` tool's shape (spec 2 §4). Either a
// project or an explicit key identifies the claim: the CLI derives both from
// the git root of its own cwd, which the daemon cannot see.
//
// TTLSeconds is the spec's field and wins; TTLMs is kept because the generated
// schema has always carried it and every other duration on this wire is in
// milliseconds. Neither set means DefaultTTL (24h).
//
// An omitted Count takes the worktree_ports of the project's `sonar.yaml`
// when the daemon knows one that sets it, and one port otherwise; an explicit
// Count always wins (step 5A.7).
type ClaimsAcquireParams struct {
	HostParams
	Project    string `json:"project,omitempty"`
	Worktree   string `json:"worktree,omitempty"`
	Key        string `json:"key,omitempty"`
	Count      int    `json:"count,omitempty"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"`
	TTLMs      int64  `json:"ttl_ms,omitempty"`
}

type ClaimsAcquireResult struct {
	MutationResult
	Key       string `json:"key"`
	Ports     []int  `json:"ports"`
	ExpiresAt string `json:"expires_at"`
}

type ClaimsReleaseParams struct {
	HostParams
	Key string `json:"key"`
}

type ClaimsReleaseResult struct {
	OK       bool `json:"ok"`
	Released int  `json:"released"`
}

type ClaimsListResult struct {
	Claims []state.Claim `json:"claims"`
}

// -------------------------------------------------------------- sessions ---

type SessionsListParams struct {
	HostParams
	ActiveOnly bool `json:"active_only,omitempty"`
}

type SessionsListResult struct {
	Sessions []state.SessionRecord `json:"sessions"`
}

type SessionsInspectParams struct {
	HostParams
	ID string `json:"id"`
}

type SessionsInspectResult struct {
	Session state.SessionRecord `json:"session"`
	Runs    []RunRecord         `json:"runs"`
	Ports   []state.Port        `json:"ports"`
}

type SessionsKillParams struct {
	HostParams
	ID     string `json:"id"`
	Tree   bool   `json:"tree,omitempty"`
	Force  bool   `json:"force,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// ---------------------------------------------------------------- config ---

type ConfigGetResult struct {
	Config map[string]any `json:"config"`
}

type ConfigSetParams struct {
	HostParams
	Patch map[string]any `json:"patch"`
}

type ConfigSetResult struct {
	OK     bool           `json:"ok"`
	Config map[string]any `json:"config"`
}

type ConfigPathResult struct {
	Path string `json:"path"`
}

// ---------------------------------------------------------------- remote ---

type RemoteScanParams struct {
	Host string `json:"host"`
}

type RemoteScanResult struct {
	Ports []state.Port `json:"ports"`
}

// RemoteInstallParams installs sonar on an SSH target and starts its daemon
// (spec 3 §"sonar remote install"). Target is what `ssh` receives, verbatim:
// a `user@host`, or a Host alias from the caller's `~/.ssh/config`.
//
// Version defaults to the version of the daemon serving the call, so the two
// ends match; a daemon that is itself a development build has no release to
// name and fails rather than guessing one.
type RemoteInstallParams struct {
	Target    string   `json:"target"`
	Name      string   `json:"name,omitempty"`
	Version   string   `json:"version,omitempty"`
	Identity  string   `json:"identity,omitempty"`
	SSHArgs   []string `json:"ssh_args,omitempty"`
	NoService bool     `json:"no_service,omitempty"`
}

type RemoteInstallResult struct {
	MutationResult
	SubscriptionID string `json:"subscription_id"`
}

// RemoteInstallChunk is one step of the install as it happens: `download`,
// `verify`, `extract`, `install`, `service`, `linger`, `check`, plus `connect`,
// `detect` and `resolve` from the local side. A line the remote printed that
// sonar did not tag arrives as step `remote`.
type RemoteInstallChunk struct {
	Step   string `json:"step"`
	Detail string `json:"detail,omitempty"`
}

// RemoteInstallEnd describes the host after a successful install. It is not a
// state.Host: the fields a Host carries beyond these come from the daemon
// bridge, which `remote.add` opens, not from the install.
type RemoteInstallEnd struct {
	Name    string `json:"name"`
	Target  string `json:"target"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	BinPath string `json:"bin_path"`
	// Service is how the daemon starts on the remote: "systemd" for a user
	// unit, "detached" for `sonar serve --detach`, "none" for --no-service.
	Service       string `json:"service"`
	DaemonRunning bool   `json:"daemon_running"`
	DaemonPID     int    `json:"daemon_pid,omitempty"`
	// LingerHint is the `loginctl enable-linger` command to run when the
	// remote's systemd user session would end at logout, taking the daemon
	// with it. Empty when lingering is already on or does not apply.
	LingerHint string `json:"linger_hint,omitempty"`
}

// RemoteListResult is every registered host, whatever its connection state,
// with the load the bridge last read from it.
type RemoteListResult struct {
	Hosts []state.Host `json:"hosts"`
}

// RemoteAddParams registers an SSH host. Target is what `ssh` receives,
// verbatim — a `user@host` or a `~/.ssh/config` alias; sonar never resolves it
// as DNS. Name defaults to the host part of the target and is the key
// everything else uses: `--host <name>`, the `host` field on every row, and
// the "<name>/" prefix on that host's delta keys.
type RemoteAddParams struct {
	Target string `json:"target"`
	// Host is an accepted alias of Target, for clients that call the SSH
	// destination "host" rather than "target". Target wins when both are set
	// and they differ.
	Host      string   `json:"host,omitempty"`
	Name      string   `json:"name,omitempty"`
	SSHArgs   []string `json:"ssh_args,omitempty"`
	Identity  string   `json:"identity,omitempty"`
	Port      int      `json:"port,omitempty"`
	RemoteBin string   `json:"remote_bin,omitempty"`
}

type RemoteAddResult struct {
	OK   bool       `json:"ok"`
	Host state.Host `json:"host"`
}

type RemoteRemoveParams struct {
	Name string `json:"name"`
}

// RemoteCallParams forwards one method to a registered host's daemon. Writes
// are forwarded like any other method (remote-hosts spec, decision 3).
type RemoteCallParams struct {
	Host   string          `json:"host"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// RemoteCallResult is whatever the remote daemon returned for the forwarded
// method, passed through unchanged. Its shape is `method`'s result, so the
// schema leaves it unconstrained rather than pretending it is one type.
type RemoteCallResult json.RawMessage

func (r RemoteCallResult) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

func (r *RemoteCallResult) UnmarshalJSON(b []byte) error {
	*r = append((*r)[:0], b...)
	return nil
}

// JSONSchema keeps the generated document honest: the result of a forwarded
// call is the result of whatever method was forwarded.
func (RemoteCallResult) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Description: "The remote daemon's own result for `method`, returned verbatim.",
	}
}

// ----------------------------------------------------------------- share ---
//
// Described so the protocol has the shape before anything serves it: no
// package registers a handler for share.* yet (docs/SHARE.md in sonar-relay).

// ShareCreateParams makes one listening service reachable. Reach is always
// written down — "lan" or "public" — because there is no safe default between
// "this room" and "the internet". ListenPort applies to a LAN share only.
// Replace stops a share already live for the same key rather than failing with
// share_limit_reached.
type ShareCreateParams struct {
	Target     Selector `json:"target"`
	Reach      string   `json:"reach" jsonschema:"enum=lan,enum=public"`
	TTL        *string  `json:"ttl,omitempty"`
	ListenPort *int     `json:"listen_port,omitempty"`
	Replace    bool     `json:"replace,omitempty"`
}

type ShareCreateResult struct {
	MutationResult
	Share state.Share `json:"share"`
	// Notes are sentences worth putting in front of the person who ran this,
	// about the share that was just made. Never failures — a failure is an
	// error — and never more than a line each.
	Notes []string `json:"notes,omitempty"`
}

// ShareStopParams names the shares to stop: one by id, the ones on a target,
// or all of them.
type ShareStopParams struct {
	ID     *string   `json:"id,omitempty"`
	Target *Selector `json:"target,omitempty"`
	All    bool      `json:"all,omitempty"`
}

type ShareStopResult struct {
	MutationResult
	Stopped []string `json:"stopped"`
}

type ShareListResult struct {
	Shares []state.Share `json:"shares"`
}

// ShareExtendParams pushes a live share's expiry out by TTL.
type ShareExtendParams struct {
	ID  string `json:"id"`
	TTL string `json:"ttl"`
}

type ShareExtendResult struct {
	Share state.Share `json:"share"`
}

// ShareLogsParams tails the relay's view of a share's recent requests.
type ShareLogsParams struct {
	ID   string `json:"id"`
	Tail int    `json:"tail,omitempty"`
}

type ShareLogsResult struct {
	Lines []string `json:"lines"`
}

// ------------------------------------------------------------------- map ---

type MapCreateParams struct {
	ServicePort int  `json:"service_port"`
	ListenPort  int  `json:"listen_port"`
	HTTP        bool `json:"http,omitempty"`
	Persist     bool `json:"persist,omitempty"`
}

type MapCreateResult struct {
	MutationResult
	Proxy state.Proxy `json:"proxy"`
}

type MapStopParams struct {
	ID         *string `json:"id,omitempty"`
	ListenPort *int    `json:"listen_port,omitempty"`
	All        bool    `json:"all,omitempty"`
}

type MapStopResult struct {
	MutationResult
	Stopped []string `json:"stopped"`
}

type MapListResult struct {
	Proxies []state.Proxy `json:"proxies"`
}

type MapRequestsParams struct {
	ID string `json:"id"`
}

type MapRequestsResult struct {
	SubscriptionID string `json:"subscription_id"`
}

type MapRequestsChunk struct {
	TS         string `json:"ts"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Bytes      int64  `json:"bytes"`
}

type MapRequestsEnd struct {
	Requests int64 `json:"requests"`
}

// --------------------------------------------------------------- session ---

// The daemon's relay session (sonar-relay/docs/AUTH.md, "Who holds the
// session"). The daemon is authoritative: it owns the stored session, it is
// what `share.create` will consult, and signing out here signs out everywhere.
//
// There is deliberately no CLI command behind any of this. Sign-in is a step
// of `share.create`, not a `sonar login`, so these methods are reachable only
// over RPC — from the desktop app, and later from the CLI's inline prompt.

// SessionAccount is who a session belongs to, as the relay's `GET /v1/me` and
// the device-token `200` both answer it. Nothing in it is a secret; the token
// itself never crosses this protocol in this direction.
type SessionAccount struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	Provider    string `json:"provider,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// SessionStatusParams asks who this machine is signed in as.
type SessionStatusParams struct {
	// Refresh checks the stored session against the relay (`GET /v1/me`)
	// before answering, so a session revoked from another machine is noticed
	// here instead of being believed until the next share.
	//
	// It is opt-in because the plain answer is free and this one is a round
	// trip: a screen that shows who you are on every mount asks without it,
	// and asks with it when it is the screen *about* the session, or at
	// launch. A relay that cannot be reached leaves the stored answer
	// standing — being offline is not being signed out — and only a 401 is
	// taken as the end of the session.
	Refresh bool `json:"refresh,omitempty"`
}

// SessionStatusResult is what this machine is signed in as.
type SessionStatusResult struct {
	SignedIn bool `json:"signed_in"`
	// Account is present only when SignedIn.
	Account *SessionAccount `json:"account,omitempty"`
	// StoredIn is "keychain" or "file", and empty when nothing is stored.
	StoredIn string `json:"stored_in,omitempty"`
	// Reason says why a stored session was refused: a credentials file others
	// could read or replace, a record a newer sonar wrote, a keychain that did
	// not answer. Empty when there simply is no session. It is prose for a
	// person — `sonar doctor` prints it — not a code to branch on.
	Reason string `json:"reason,omitempty"`
	// Relay is the relay this daemon signs in to, so a client can tell that it
	// and the daemon disagree before it tries to register a token.
	Relay string `json:"relay"`
}

// SessionRegisterParams hands the daemon a session token the caller already
// holds — the desktop app's, which it obtained from its own device flow and
// keeps in the OS keychain (AUTH.md, "Who holds the session", step 2).
//
// There is no `account` field, and that is a deliberate departure from the
// desktop's shape: the daemon asks the relay `GET /v1/me` rather than believing
// a caller about whose session this is. A client that could name the account
// could label someone else's token with its own name, and the round trip is
// also the only proof the token is live.
type SessionRegisterParams struct {
	// Token is the relay session token. An RPC field and never a flag: a token
	// in argv is readable by every user on the machine through `ps`
	// (sonar-relay/docs/SHARE.md, "Auth, and what this changes in AUTH.md").
	Token string `json:"token"`
	// Relay is the origin the token came from. Optional; when given it must be
	// the relay this daemon uses, so an app pointed at another one fails
	// loudly instead of storing a token nothing can spend.
	Relay string `json:"relay,omitempty"`
}

type SessionRegisterResult struct {
	Account  SessionAccount `json:"account"`
	StoredIn string         `json:"stored_in"`
}

// SessionStartResult is the relay's answer to `POST /v1/device/code`, minus the
// `device_code`. That half never leaves the daemon: the caller polls with
// `session.poll`, so a screen cannot leak a code it was never given, and a
// client cannot poll a flow it did not start.
type SessionStartResult struct {
	// FlowID names this flow, so a client polls the code it was handed and
	// not whichever one started last. It is not a credential — the device code
	// it stands for never leaves the daemon — and a client that ignores it
	// still polls its own flow, because a flow also remembers the connection
	// that started it.
	FlowID string `json:"flow_id"`
	// UserCode is `XXXX-XXXX`, the thing a person types.
	UserCode string `json:"user_code"`
	// VerificationURI is the page to open; VerificationURIComplete is the same
	// page with the code already in it.
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	// ExpiresIn is how many seconds the code is good for.
	ExpiresIn int `json:"expires_in"`
	// Interval is how many seconds to wait between polls. The relay states it
	// here and re-states it on every slow_down; its number is the one to
	// honour.
	Interval int `json:"interval"`
}

// Session poll states. RFC 8628's vocabulary, because every client already
// knows it.
const (
	// SessionPending: nobody has approved the code yet.
	SessionPending = "pending"
	// SessionSlowDown: polled too fast. Not a failure — it carries the new
	// interval, and a client that treats it as one backs off into a number it
	// never reads.
	SessionSlowDown = "slow_down"
	// SessionDenied: the person declined.
	SessionDenied = "denied"
	// SessionExpired: the code is no longer valid. The relay answers 410 for a
	// code that aged out, one already claimed and one that never existed —
	// deliberately indistinguishable — so Detail says "no longer valid" rather
	// than "expired".
	SessionExpired = "expired"
	// SessionSignedIn: the relay issued a session and the daemon stored it.
	SessionSignedIn = "signed_in"
)

// SessionPollParams names the flow to poll. Both fields of the pair a client
// needs are optional, and the fallbacks are what make an older client safe:
// with no flow_id the daemon polls the newest flow this connection started,
// and only failing that the newest flow on the daemon.
type SessionPollParams struct {
	// FlowID is the id `session.start` handed back. Absent means "mine".
	FlowID string `json:"flow_id,omitempty"`
}

// SessionPollResult is one poll of the flow the daemon is holding.
type SessionPollResult struct {
	// State is one of the Session* constants above.
	State string `json:"state"`
	// Interval is the seconds to wait before the next poll, on pending and
	// slow_down.
	Interval int `json:"interval,omitempty"`
	// Account is present only on signed_in.
	Account *SessionAccount `json:"account,omitempty"`
	// StoredIn is where the new session went, on signed_in.
	StoredIn string `json:"stored_in,omitempty"`
	// Detail is a sentence for a person, on the states that end a flow.
	Detail string `json:"detail,omitempty"`
}

// SessionClearResult is what signing out did. `revoked: false` with a Detail is
// the ordinary offline answer and not a failure: the local session is gone
// either way, because someone who asked to sign out must end up signed out.
type SessionClearResult struct {
	Revoked bool `json:"revoked"`
	// Detail is why the relay was not told. Never a token.
	Detail string `json:"detail,omitempty"`
}
