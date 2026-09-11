package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/sessions"
	"github.com/raskrebs/sonar/internal/state"
)

// The action tools: the four that change something (`kill`, `stop_group`,
// `rename_port`, `start_service`) and the group listing they are usually paired
// with. Each is still one daemon call — or, for `start_service`, a spawn
// followed by a wait — and the daemon does the work; what lives here is the
// selector rule, the shape spec 2 §1.1 fixes for the result, and a sentence a
// model can read without parsing JSON.

func init() { registerTools((*Server).addActionTools) }

// CapabilitySessions is the daemon capability behind `kill {session}`. Until
// the daemon advertises it, the selector is answered with capability_missing
// rather than a method-not-found from the wire (contract §21).
const CapabilitySessions = "sessions"

// defaultWaitSeconds and maxWaitSeconds bound start_service's readiness wait
// (spec 2, "Concurrency and timeouts").
const (
	defaultWaitSeconds = 30
	maxWaitSeconds     = 300
)

// autoPort is the wait_for_port value that means "whatever port the service
// opens": the tool waits for the run's process tree to start listening and
// reports the port it found.
const autoPort = "auto"

// ------------------------------------------------------------------- kill ---

// KillInput is the argument set of kill. Exactly one selector must be set
// (spec 2, "Error model"); there is deliberately no "all".
type KillInput struct {
	Port    int    `json:"port,omitempty" jsonschema:"Stop whatever is listening on this TCP port."`
	PID     int    `json:"pid,omitempty" jsonschema:"Stop this process. Use it when a port is bound by more than one process and list_ports told you which pid you mean."`
	Group   string `json:"group,omitempty" jsonschema:"Stop every port in this group: a project, a compose project or a sonar start run. Same as stop_group."`
	Session string `json:"session,omitempty" jsonschema:"Stop everything an agent session started, by session id. Use it to clean up after yourself at the end of a task."`
	RunID   string `json:"run_id,omitempty" jsonschema:"Stop the run with this id, as reported by start_service."`
	Tree    *bool  `json:"tree,omitempty" jsonschema:"Kill the whole process tree, so npm to vite to esbuild chains leave no orphans. Defaults to true; set it to false only to signal the single process you named."`
	Force   bool   `json:"force,omitempty" jsonschema:"Send SIGKILL instead of SIGTERM. Skips the process's own shutdown, so it can lose unsaved state; try without it first."`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"Report what would be stopped, and stop nothing. Use this first when you did not start the process yourself."`
}

// KilledProcess is one process (or container) that was stopped, or that a dry
// run says would be stopped.
type KilledProcess struct {
	Port   int    `json:"port"`
	PID    int    `json:"pid"`
	Name   string `json:"name"`
	Method string `json:"method"`
}

// FailedProcess is one target that could not be stopped, with the daemon's
// reason: usually another user's process, or nothing listening at all.
type FailedProcess struct {
	Port  int    `json:"port"`
	PID   int    `json:"pid"`
	Error string `json:"error"`
}

// KillOutput is the shape spec 2 §1.1 gives kill and stop_group. It is the
// daemon's kill envelope (contract §3) split by outcome, because that is the
// question the caller has: what stopped, and what did not.
type KillOutput struct {
	Killed []KilledProcess `json:"killed"`
	Failed []FailedProcess `json:"failed"`
	// DryRun echoes the argument, so a result read on its own cannot be
	// mistaken for a kill that happened.
	DryRun bool `json:"dry_run"`
}

const killDescription = `Stop whatever is listening on a port, or an entire group or agent session.

Kills the full process tree by default so ` + "`npm → vite → esbuild`" + ` chains do not leave orphans. Prefer dry_run: true first when you did not start the process yourself. Never use this to free a port you have not inspected.

Pass exactly one of port, pid, group, session or run_id. There is no "kill everything": a port owned by another worktree, another agent session or a human is not yours to free — pick a different port and say so instead.`

const stopGroupDescription = `Stop every service in one group: a project's api, web and workers in a single call.

A group is what sonar attributes ports to — a sonar.yaml project, a Docker Compose project, or a sonar start run. This is the cleanup you want at the end of a task that brought a project up; list_groups shows the names.

Kills the process tree of every member. Pass dry_run: true first if you did not start the group yourself.`

// ------------------------------------------------------------ rename_port ---

// RenamePortInput is the argument set of rename_port.
type RenamePortInput struct {
	Port int    `json:"port,omitempty" jsonschema:"The listening port to rename."`
	PID  int    `json:"pid,omitempty" jsonschema:"The pid to rename, when several processes answer on one port."`
	Name string `json:"name" jsonschema:"The label to show for this port from now on, for example \"checkout-api\". An empty string removes a label set earlier."`
}

// RenamePortOutput is `{port: Port}`: the row as it now reads.
type RenamePortOutput struct {
	Port state.Port `json:"port"`
}

const renamePortDescription = `Give a port a lasting, human name, so it reads as "checkout-api" instead of "node" in every later listing.

The name is stored by the daemon and survives restarts of the process and of the daemon. Use it when a process's own command line says nothing useful — a bare node, python or java — and you or the user will look at this machine's ports again.

It renames a label, not a process: nothing is signalled and nothing restarts. Calling it twice with the same name changes nothing.`

// ---------------------------------------------------------- start_service ---

// StartServiceInput is the argument set of start_service. Command is argv, not
// a shell line: the daemon never passes it through a shell (spec 2, §5).
type StartServiceInput struct {
	Command        []string          `json:"command" jsonschema:"The command as an argv array, for example [\"npm\", \"run\", \"dev\"]. It is executed directly, never through a shell, so pipes, redirections and && do not work here."`
	Cwd            string            `json:"cwd,omitempty" jsonschema:"Directory to run in. Defaults to the directory this MCP server was started in, which is usually the repository root."`
	Group          string            `json:"group,omitempty" jsonschema:"Group to attribute the service to. Defaults to the project sonar infers from cwd, which is normally what you want."`
	Name           string            `json:"name,omitempty" jsonschema:"Short name for this service, shown in listings, for example \"api\"."`
	Env            map[string]string `json:"env,omitempty" jsonschema:"Extra environment variables. They are added to the daemon's spawn environment, never replace it."`
	WaitForPort    any               `json:"wait_for_port,omitempty" jsonschema:"Block until the service is accepting connections: a port number, or \"auto\" to wait for whatever port it opens. Leave it out to return as soon as the process has started."`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"How long to wait for wait_for_port, in seconds. Default 30, maximum 300."`
}

// StartServiceOutput is the shape spec 2 §1.1 gives start_service. Session is
// the object every other tool reports (contract §5) and is null — like
// Port.session — when nothing in this server's environment names an agent,
// which is the normal case outside a coding agent. TimedOut and Error are set together
// when wait_for_port ran out: the run is still there and its id, pid and log
// path are how you go and look at it.
type StartServiceOutput struct {
	RunID    string         `json:"run_id"`
	PID      int            `json:"pid"`
	Group    string         `json:"group"`
	Name     string         `json:"name"`
	Session  *state.Session `json:"session" jsonschema:"nullable"`
	Ports    []state.Port   `json:"ports"`
	LogPath  string         `json:"log_path"`
	TimedOut bool           `json:"timed_out,omitempty"`
	Error    *ErrorInfo     `json:"error,omitempty"`
}

// ErrorInfo is the {code, message, hint} object every failed tool result
// carries. start_service embeds it beside the run it did start, so a timeout
// does not cost the caller the run id it needs to investigate.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

const startServiceDescription = `Start a dev server or service under sonar's supervision instead of running it bare in a shell.

The process survives this tool call, is attributed to your session and the repo's group, and its logs are captured for tail_logs. Pass wait_for_port to block until it is accepting connections.

Use this instead of a background shell command: nothing here needs & or nohup, and a service started this way can be found and stopped later with list_ports, list_groups and kill. Pass the command as an argv array; it is not run through a shell.`

// ------------------------------------------------------------ list_groups ---

// ListGroupsOutput is `{groups: [Group]}`, the shape of groups.list.
type ListGroupsOutput = rpc.GroupsListResult

const listGroupsDescription = `List the projects sonar knows about: their ports, their status, and the services declared in each sonar.yaml.

A group is a project — a directory with a sonar.yaml, a Docker Compose project, or a repo something was started in. Use it to see what a project is meant to run and what of that is up, and to learn the group names kill and stop_group take.

The services list is what the project declares, with running and port_actual joined from what is listening now, so a service with running: false is one nobody has started.`

func (s *Server) addActionTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "kill",
		Title:       "Stop what is on a port, a group or a session",
		Description: killDescription,
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
		OutputSchema: outputSchema(KillOutput{}),
	}, s.kill)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "stop_group",
		Title:       "Stop a whole group",
		Description: stopGroupDescription,
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
		OutputSchema: outputSchema(KillOutput{}),
	}, s.stopGroup)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "rename_port",
		Title:       "Name a port",
		Description: renamePortDescription,
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
		OutputSchema: outputSchema(RenamePortOutput{}),
	}, s.renamePort)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "start_service",
		Title:       "Start a service under sonar",
		Description: startServiceDescription,
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
		OutputSchema: outputSchema(StartServiceOutput{}),
	}, s.startService)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_groups",
		Title:       "List groups and their services",
		Description: listGroupsDescription,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
		OutputSchema: outputSchema(ListGroupsOutput{}),
	}, s.listGroups)
}

// kill routes one selector to the daemon method that owns it: ports.kill for a
// port, a pid or a run, groups.kill for a group, sessions.kill for a session.
func (s *Server) kill(ctx context.Context, _ *mcp.CallToolRequest, in KillInput) (*mcp.CallToolResult, any, error) {
	target, derr := in.target()
	if derr != nil {
		return errorResult(derr), nil, nil
	}
	tree := true
	if in.Tree != nil {
		tree = *in.Tree
	}

	var env rpc.KillEnvelope
	switch target.kind {
	case "group":
		params := rpc.GroupsKillParams{Name: in.Group, Force: in.Force, DryRun: in.DryRun}
		if err := s.daemon.Call(ctx, "groups.kill", params, &env); err != nil {
			return s.failed(err)
		}
	case "session":
		if !s.daemon.Has(CapabilitySessions) {
			return errorResult(Domain(CodeCapabilityMissing,
				"this daemon does not track agent sessions",
				"upgrade sonar, then `sonar daemon restart`; until then kill by group, port or run_id")), nil, nil
		}
		params := rpc.SessionsKillParams{ID: in.Session, Tree: tree, Force: in.Force, DryRun: in.DryRun}
		if err := s.daemon.Call(ctx, "sessions.kill", params, &env); err != nil {
			return s.failed(err)
		}
	default:
		params := rpc.PortsKillParams{
			Targets: []rpc.Selector{target.selector},
			Tree:    tree, Force: in.Force, DryRun: in.DryRun,
		}
		if err := s.daemon.Call(ctx, "ports.kill", params, &env); err != nil {
			return s.failed(err)
		}
	}

	out := killOutput(env, in.DryRun)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderKill(out, target.phrase)}},
	}, out, nil
}

// StopGroupInput is the argument set of stop_group.
type StopGroupInput struct {
	Group  string `json:"group" jsonschema:"The group to stop, as listed by list_groups."`
	Force  bool   `json:"force,omitempty" jsonschema:"Send SIGKILL instead of SIGTERM to every member. Try without it first."`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"Report what would be stopped, and stop nothing."`
}

func (s *Server) stopGroup(ctx context.Context, _ *mcp.CallToolRequest, in StopGroupInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Group) == "" {
		return errorResult(invalidArguments(
			"stop_group needs a group",
			`pass {"group": "shop"}; list_groups shows the names`)), nil, nil
	}

	var env rpc.KillEnvelope
	params := rpc.GroupsKillParams{Name: in.Group, Force: in.Force, DryRun: in.DryRun}
	if err := s.daemon.Call(ctx, "groups.kill", params, &env); err != nil {
		return s.failed(err)
	}

	out := killOutput(env, in.DryRun)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderKill(out, "in group "+in.Group)}},
	}, out, nil
}

// renamePort stores the label and then reads the row back, because the row is
// what the caller wants to see and only the daemon knows how the rename
// interacts with everything else on it (contract §19: display_name).
func (s *Server) renamePort(ctx context.Context, _ *mcp.CallToolRequest, in RenamePortInput) (*mcp.CallToolResult, any, error) {
	if in.Port <= 0 && in.PID <= 0 {
		return errorResult(invalidArguments(
			"rename_port needs a port or a pid",
			`pass {"port": 3000, "name": "checkout-api"}`)), nil, nil
	}

	sel := rpc.Selector{}
	if in.PID > 0 {
		sel.PID = &in.PID
	} else {
		sel.Port = &in.Port
	}

	params := rpc.PortsRenameParams{Selector: sel}
	name := strings.TrimSpace(in.Name)
	if name != "" {
		params.Name = &name
	}
	var renamed rpc.PortsRenameResult
	if err := s.daemon.Call(ctx, "ports.rename", params, &renamed); err != nil {
		return s.failed(err)
	}

	var inspected rpc.PortsInspectResult
	if err := s.daemon.Call(ctx, "ports.inspect", sel, &inspected); err != nil {
		return s.failed(err)
	}

	out := RenamePortOutput{Port: inspected.Port}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{
			Text: renderRename(inspected.Port, name) + "\n\n" + renderPort(inspected.Port),
		}},
	}, out, nil
}

// startService spawns through the daemon so the service outlives this call,
// then, if asked, waits for it to accept connections. A wait that runs out is
// not a failed start: the run is reported with the timeout beside it.
func (s *Server) startService(ctx context.Context, _ *mcp.CallToolRequest, in StartServiceInput) (*mcp.CallToolResult, any, error) {
	if len(in.Command) == 0 || strings.TrimSpace(in.Command[0]) == "" {
		return errorResult(invalidArguments(
			"start_service needs a command",
			`pass argv, for example {"command": ["npm", "run", "dev"]}`)), nil, nil
	}
	wantPort, auto, derr := parseWaitForPort(in.WaitForPort)
	if derr != nil {
		return errorResult(derr), nil, nil
	}
	cwd := strings.TrimSpace(in.Cwd)
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return errorResult(invalidArguments(
				"start_service needs a cwd: this server has no working directory of its own",
				`pass {"cwd": "/path/to/repo"}`)), nil, nil
		}
		cwd = wd
	}

	params := rpc.RunsSpawnParams{Argv: in.Command, Cwd: cwd, Env: in.Env}
	if g := strings.TrimSpace(in.Group); g != "" {
		params.Group = &g
	}
	if n := strings.TrimSpace(in.Name); n != "" {
		params.Name = &n
	}
	if wantPort > 0 {
		params.PortHint = &wantPort
	}
	// The session is detected here, in the process the agent started: the
	// daemon's own environment is a service manager's, not an agent's, so it
	// could never detect this (spec 2 §3).
	session, hasSession := sessions.Capture(cwd, sessions.Options{})
	if hasSession {
		params.Session = &session
	}

	var spawned rpc.RunsSpawnResult
	if err := s.daemon.Call(ctx, "runs.spawn", params, &spawned); err != nil {
		return s.failed(err)
	}

	out := StartServiceOutput{
		RunID:   spawned.RunID,
		PID:     spawned.PID,
		Group:   strings.TrimSpace(in.Group),
		Name:    strings.TrimSpace(in.Name),
		Ports:   []state.Port{},
		LogPath: spawned.LogPath,
	}
	if hasSession {
		captured := session
		out.Session = &captured
	}
	// The daemon resolves the group and the name from cwd and argv when the
	// caller left them out, so they are read back rather than guessed here.
	if rec, ok := s.runRecord(ctx, spawned.RunID); ok {
		out.Group, out.Name = rec.Group, rec.Name
	}

	if wantPort <= 0 && !auto {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: renderStarted(out, in.Command)}},
		}, out, nil
	}

	timeout := waitTimeout(in.TimeoutSeconds)
	ready, err := s.waitForService(ctx, spawned.RunID, wantPort, auto, timeout)
	if err != nil {
		return s.failed(err)
	}
	out.Ports = s.servicePorts(ctx, spawned.RunID, ready)

	if len(ready) == 0 {
		out.TimedOut = true
		out.Error = &ErrorInfo{
			Code: CodeTimeout,
			Message: fmt.Sprintf("%s started as run %s (pid %d) but %s within %s",
				strings.Join(in.Command, " "), out.RunID, out.PID, notListening(wantPort, auto), timeout),
			Hint: "the run is still alive: read its output with tail_logs, or stop it with kill {run_id}",
		}
		text := out.Error.Code + ": " + out.Error.Message + "\n" + out.Error.Hint
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, out, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderStarted(out, in.Command)}},
	}, out, nil
}

func (s *Server) listGroups(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	var res rpc.GroupsListResult
	if err := s.daemon.Call(ctx, "groups.list", rpc.Empty{}, &res); err != nil {
		return s.failed(err)
	}
	if res.Groups == nil {
		res.Groups = []state.Group{}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderGroups(res.Groups)}},
	}, res, nil
}

// ------------------------------------------------------------- selectors ---

// killTarget is the one selector a kill was given: which daemon method owns it,
// the wire selector for ports.kill, and how to name it in a sentence.
type killTarget struct {
	kind     string
	selector rpc.Selector
	phrase   string
}

func (in KillInput) target() (killTarget, *DomainError) {
	var set []killTarget
	if in.Port > 0 {
		port := in.Port
		set = append(set, killTarget{"port", rpc.Selector{Port: &port}, fmt.Sprintf("on port %d", port)})
	}
	if in.PID > 0 {
		pid := in.PID
		set = append(set, killTarget{"pid", rpc.Selector{PID: &pid}, fmt.Sprintf("for pid %d", pid)})
	}
	if g := strings.TrimSpace(in.Group); g != "" {
		set = append(set, killTarget{kind: "group", phrase: "in group " + g})
	}
	if sess := strings.TrimSpace(in.Session); sess != "" {
		set = append(set, killTarget{kind: "session", phrase: "for session " + sess})
	}
	if run := strings.TrimSpace(in.RunID); run != "" {
		id := run
		set = append(set, killTarget{"run_id", rpc.Selector{RunID: &id}, "for run " + id})
	}

	switch len(set) {
	case 1:
		return set[0], nil
	case 0:
		return killTarget{}, invalidArguments(
			"kill needs exactly one of port, pid, group, session or run_id",
			`pass {"port": 3000}, or {"group": "shop"} for a whole project; there is no "kill everything"`)
	default:
		return killTarget{}, invalidArguments(
			"kill takes exactly one selector, and got "+strconv.Itoa(len(set)),
			"call it once per target: a kill acts on what you named, not on the union")
	}
}

// parseWaitForPort reads the int-or-"auto" argument. A model that sends the
// port as a string gets what it meant rather than an argument error; anything
// else is refused, because guessing at a readiness check is worse than saying
// what the two accepted forms are.
func parseWaitForPort(v any) (port int, auto bool, err *DomainError) {
	switch t := v.(type) {
	case nil:
		return 0, false, nil
	case bool:
		return 0, false, waitForPortError(fmt.Sprint(t))
	case float64:
		return checkedPort(int(t))
	case int:
		return checkedPort(t)
	case json.Number:
		n, cErr := t.Int64()
		if cErr != nil {
			return 0, false, waitForPortError(t.String())
		}
		return checkedPort(int(n))
	case string:
		if strings.EqualFold(strings.TrimSpace(t), autoPort) {
			return 0, true, nil
		}
		if n, cErr := strconv.Atoi(strings.TrimSpace(t)); cErr == nil {
			return checkedPort(n)
		}
		return 0, false, waitForPortError(t)
	default:
		return 0, false, waitForPortError(fmt.Sprint(t))
	}
}

func checkedPort(n int) (int, bool, *DomainError) {
	if n < 1 || n > 65535 {
		return 0, false, waitForPortError(strconv.Itoa(n))
	}
	return n, false, nil
}

func waitForPortError(got string) *DomainError {
	return invalidArguments(
		fmt.Sprintf("wait_for_port must be a port number or \"auto\", and got %q", got),
		`pass {"wait_for_port": 3000} for a known port, or {"wait_for_port": "auto"} to take whatever the service opens`)
}

func waitTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = defaultWaitSeconds
	}
	if seconds > maxWaitSeconds {
		seconds = maxWaitSeconds
	}
	return time.Duration(seconds) * time.Second
}

// ------------------------------------------------------------------ waits ---

// waitForService blocks until the service is listening. A known port is waited
// for with ports.wait, which the daemon probes for us; "auto" is polled from
// runs.list, because the daemon does not take a run id in ports.wait yet
// (contract §20). It returns the ports that came up, empty on a timeout.
func (s *Server) waitForService(ctx context.Context, runID string, port int, auto bool, timeout time.Duration) ([]int, error) {
	if auto {
		return s.pollRunPorts(ctx, runID, timeout)
	}
	end, err := s.awaitPorts(ctx, rpc.PortsWaitParams{
		Ports:     []int{port},
		TimeoutMs: int(timeout / time.Millisecond),
	}, timeout)
	if err != nil {
		return nil, err
	}
	return end.Ready, nil
}

// awaitPorts runs one ports.wait stream to its end. The stream's own timeout is
// what stops it; the context here is the outer bound, and cancelling it (an MCP
// client going away mid-wait) cancels the daemon's side too.
func (s *Server) awaitPorts(ctx context.Context, params rpc.PortsWaitParams, timeout time.Duration) (rpc.PortsWaitEnd, error) {
	var out rpc.PortsWaitEnd

	c := s.daemon.current()
	if c == nil {
		return out, s.daemon.unavailable()
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout+DefaultTimeout)
	defer cancel()

	stream, err := c.Stream(waitCtx, "ports.wait", params, nil)
	if err != nil {
		if _, ok := asDomain(err); ok {
			return out, err
		}
		return out, s.daemon.unavailable()
	}
	defer stream.Close()

	// Chunks are progress, not the answer: the end carries {ready, timed_out}.
	go func() {
		for range stream.Chunks() {
		}
	}()

	select {
	case end, ok := <-stream.End():
		if !ok {
			return out, s.daemon.unavailable()
		}
		if end.Err != nil {
			return out, end.Err
		}
		if err := end.Decode(&out); err != nil {
			return out, Domain(CodeDaemonUnavailable, "the daemon's wait result could not be read: "+err.Error(),
				"retry; if it keeps happening, check `sonar daemon status`")
		}
		return out, nil
	case <-waitCtx.Done():
		cancelCtx, cancelWait := context.WithTimeout(context.Background(), DefaultTimeout)
		defer cancelWait()
		_ = stream.Cancel(cancelCtx)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, Domain(CodeTimeout, "the daemon did not finish waiting for the port",
			"retry with a smaller timeout_seconds, or check `sonar daemon status`")
	}
}

// pollRunPorts is wait_for_port: "auto" — the run's process tree opening any
// listener. It polls runs.list, whose rows carry the ports attributed to each
// run, because that attribution is what "the port this service opened" means.
func (s *Server) pollRunPorts(ctx context.Context, runID string, timeout time.Duration) ([]int, error) {
	deadline := time.Now().Add(timeout)
	for {
		rec, ok := s.runRecord(ctx, runID)
		if ok && len(rec.Ports) > 0 {
			return rec.Ports, nil
		}
		if !time.Now().Before(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval(deadline)):
		}
	}
}

// pollInterval is the run poll's cadence, shortened so the last poll lands on
// the deadline rather than after it.
func pollInterval(deadline time.Time) time.Duration {
	const cadence = 250 * time.Millisecond
	if left := time.Until(deadline); left < cadence {
		return max(left, 10*time.Millisecond)
	}
	return cadence
}

// runRecord reads one run back from runs.list. A daemon without the runs
// capability, or a run that has already exited, is not an error here: the
// caller falls back to what it already knows.
func (s *Server) runRecord(ctx context.Context, runID string) (rpc.RunRecord, bool) {
	var res rpc.RunsListResult
	if err := s.daemon.Call(ctx, "runs.list", rpc.Empty{}, &res); err != nil {
		s.log.Debug("runs.list failed while reporting a spawn", "run_id", runID, "error", err)
		return rpc.RunRecord{}, false
	}
	for _, rec := range res.Runs {
		if rec.ID == runID {
			return rec, true
		}
	}
	return rpc.RunRecord{}, false
}

// servicePorts is the port rows for a run that has just come up. The daemon
// attributes a port to its run on the next scan, so the read is retried for a
// moment before falling back to inspecting the port that was waited for.
func (s *Server) servicePorts(ctx context.Context, runID string, ready []int) []state.Port {
	if len(ready) == 0 {
		return []state.Port{}
	}
	for attempt := range 4 {
		var res rpc.PortsListResult
		if err := s.daemon.Call(ctx, "ports.list", rpc.PortsListParams{Group: &runID}, &res); err == nil {
			if rows := keepPorts(res.Ports, ready); len(rows) > 0 {
				return rows
			}
		}
		if attempt == 3 || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			return []state.Port{}
		case <-time.After(400 * time.Millisecond):
		}
	}

	// Attribution has not caught up (or the run is not the port's owner):
	// report the row the caller waited for rather than nothing.
	rows := []state.Port{}
	for _, port := range ready {
		p := port
		var res rpc.PortsInspectResult
		if err := s.daemon.Call(ctx, "ports.inspect", rpc.Selector{Port: &p}, &res); err == nil {
			rows = append(rows, res.Port)
		}
	}
	return rows
}

// keepPorts narrows a run's ports to the ones that just became ready, so a run
// that owns several ports reports the one the caller asked about first.
func keepPorts(rows []state.Port, ready []int) []state.Port {
	want := map[int]bool{}
	for _, port := range ready {
		want[port] = true
	}
	out := []state.Port{}
	for _, row := range rows {
		if want[row.Port] {
			out = append(out, row)
		}
	}
	return out
}

// --------------------------------------------------------------- results ---

// killOutput splits the daemon's envelope into the tool's two lists. Rows that
// failed keep the daemon's own message, which is where permission_denied and
// "nothing was listening" arrive.
func killOutput(env rpc.KillEnvelope, dryRun bool) KillOutput {
	out := KillOutput{Killed: []KilledProcess{}, Failed: []FailedProcess{}, DryRun: dryRun}
	for _, row := range env.Results {
		if row.OK {
			out.Killed = append(out.Killed, KilledProcess{
				Port: row.Port, PID: row.PID, Name: row.Name, Method: string(row.Method),
			})
			continue
		}
		out.Failed = append(out.Failed, FailedProcess{
			Port: row.Port, PID: row.PID, Error: row.Error,
		})
	}
	return out
}

// renderKill is the action tools' one sentence: what stopped, where, and how.
func renderKill(out KillOutput, phrase string) string {
	verb := "Stopped"
	if out.DryRun {
		verb = "Dry run: would stop"
	}

	var b strings.Builder
	if len(out.Killed) == 0 {
		fmt.Fprintf(&b, "%s nothing %s", verb, phrase)
	} else {
		fmt.Fprintf(&b, "%s %d %s %s (%s)", verb, len(out.Killed),
			plural(len(out.Killed), "process", "processes"), phrase, killMethods(out.Killed))
	}
	if len(out.Failed) > 0 {
		fmt.Fprintf(&b, "; %d failed: %s", len(out.Failed), failureReasons(out.Failed))
	}
	b.WriteString(".")
	return b.String()
}

// killMethods lists the distinct methods used, so "sigterm" or
// "sigterm/docker_stop" says whether anything was escalated or stopped as a
// container.
func killMethods(rows []KilledProcess) string {
	seen := map[string]bool{}
	var methods []string
	for _, row := range rows {
		if row.Method == "" || seen[row.Method] {
			continue
		}
		seen[row.Method] = true
		methods = append(methods, row.Method)
	}
	if len(methods) == 0 {
		return string(state.MethodNone)
	}
	return strings.Join(methods, "/")
}

func failureReasons(rows []FailedProcess) string {
	seen := map[string]bool{}
	var reasons []string
	for _, row := range rows {
		if row.Error == "" || seen[row.Error] {
			continue
		}
		seen[row.Error] = true
		reasons = append(reasons, row.Error)
	}
	return strings.Join(reasons, "; ")
}

func renderRename(p state.Port, name string) string {
	if name == "" {
		return fmt.Sprintf("Cleared the name of port %d; it now reads as %s.", p.Port, dash(p.DisplayName))
	}
	return fmt.Sprintf("Port %d is now called %q.", p.Port, name)
}

// renderStarted is start_service's sentence: what was started, where it landed,
// and the URL if it is listening yet.
func renderStarted(out StartServiceOutput, argv []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Started `%s` as run %s (pid %d)", strings.Join(argv, " "), out.RunID, out.PID)
	if out.Group != "" {
		fmt.Fprintf(&b, " in group %s", out.Group)
	}
	switch {
	case len(out.Ports) == 1:
		fmt.Fprintf(&b, ", listening on %s", portTarget(out.Ports[0]))
	case len(out.Ports) > 1:
		fmt.Fprintf(&b, ", listening on %s", strings.Join(portNumbers(out.Ports), ", "))
	}
	if out.LogPath != "" {
		fmt.Fprintf(&b, "; logs at %s", out.LogPath)
	}
	b.WriteString(".")
	return b.String()
}

func portTarget(p state.Port) string {
	if p.URL != "" {
		return p.URL
	}
	return "port " + strconv.Itoa(p.Port)
}

func portNumbers(rows []state.Port) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, strconv.Itoa(row.Port))
	}
	return out
}

func notListening(port int, auto bool) string {
	if auto {
		return "opened no port"
	}
	return fmt.Sprintf("nothing was listening on port %d", port)
}

// renderGroups is the list rendering for list_groups: one row per project, with
// its members and the services its config declares.
func renderGroups(groups []state.Group) string {
	if len(groups) == 0 {
		return "no groups: nothing sonar can attribute to a project is running"
	}

	shown := groups
	truncated := false
	if len(shown) > MaxRows {
		shown, truncated = shown[:MaxRows], true
	}

	header := []string{"GROUP", "STATUS", "SOURCE", "PORTS", "SERVICES", "ROOT"}
	rows := make([][]string, 0, len(shown))
	for _, g := range shown {
		rows = append(rows, []string{
			g.Name,
			dash(g.Status),
			dash(string(g.Source)),
			dash(joinInts(g.Members)),
			dash(serviceSummary(g.Services)),
			dash(deref(g.RootDir)),
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d %s\n\n", len(groups), plural(len(groups), "group", "groups"))
	b.WriteString(table(header, rows))
	if truncated {
		b.WriteString("\n" + truncationNote(len(shown), len(groups)))
	}
	return b.String()
}

// serviceSummary names the declared services, marking the ones that are not
// running — the difference between "this project has a worker" and "the worker
// is up" is the reason to read this column.
func serviceSummary(services []state.Service) string {
	if len(services) == 0 {
		return ""
	}
	out := make([]string, 0, len(services))
	for _, svc := range services {
		if svc.Running {
			out = append(out, svc.Name)
			continue
		}
		out = append(out, svc.Name+" (stopped)")
	}
	return strings.Join(out, ", ")
}

func joinInts(xs []int) string {
	if len(xs) == 0 {
		return ""
	}
	sorted := append([]int{}, xs...)
	sort.Ints(sorted)
	out := make([]string, 0, len(sorted))
	for _, x := range sorted {
		out = append(out, strconv.Itoa(x))
	}
	return strings.Join(out, ", ")
}
