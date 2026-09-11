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
// `daemon.hello`, `daemon.shutdown` and the `remote.*` family. It is embedded
// in the params types below where it is worth spelling out in the schema.
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
}

type RunsListResult struct {
	Runs []RunRecord `json:"runs"`
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
	// PortHint is the port `sonar start --port` said this run would bind.
	PortHint *int `json:"port_hint,omitempty"`
	// Status is "starting" while a run with a port hint has not bound it yet,
	// and "running" otherwise.
	Status string `json:"status"`
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

// ---------------------------------------------------------------- expose ---

type ExposeCreateParams struct {
	Target   string  `json:"target"`
	Provider *string `json:"provider,omitempty"`
	Name     *string `json:"name,omitempty"`
	Auth     *string `json:"auth,omitempty"`
	TTL      *string `json:"ttl,omitempty"`
	Persist  bool    `json:"persist,omitempty"`
	Scope    *string `json:"scope,omitempty"`
}

type ExposeCreateResult struct {
	MutationResult
	Tunnel state.Tunnel `json:"tunnel"`
}

type ExposeStopParams struct {
	ID   *string `json:"id,omitempty"`
	Port *int    `json:"port,omitempty"`
	All  bool    `json:"all,omitempty"`
}

type ExposeStopResult struct {
	MutationResult
	Stopped []string `json:"stopped"`
}

type ExposeListResult struct {
	Tunnels []state.Tunnel `json:"tunnels"`
}

type ExposeLogsParams struct {
	ID   string `json:"id"`
	Tail int    `json:"tail,omitempty"`
}

type ExposeLogsResult struct {
	Lines []string `json:"lines"`
}

type ExposeProvidersResult struct {
	Providers []ProviderInfo `json:"providers"`
}

type ProviderInfo struct {
	Name         string   `json:"name"`
	Available    string   `json:"available"`
	Capabilities []string `json:"capabilities"`
	Hint         string   `json:"hint,omitempty"`
}

type ExposeInstallProviderParams struct {
	Provider string `json:"provider"`
	Confirm  bool   `json:"confirm"`
}

type ExposeInstallProviderResult struct {
	MutationResult
	SubscriptionID string `json:"subscription_id"`
}

type ExposeInstallProviderChunk struct {
	Phase   string `json:"phase"`
	Percent int    `json:"percent"`
	Message string `json:"message,omitempty"`
}

type ExposeInstallProviderEnd struct {
	Path    string `json:"path"`
	Version string `json:"version"`
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
