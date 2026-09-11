package mcpserver

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/raskrebs/sonar/internal/claims"
	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// The query tools (spec 2 §1.1): the questions an agent asks between starting
// something and acting on it — is it up yet, which port may I use, is that port
// mine, what did it print, is it healthy, what talks to what, what happened
// here, and who else is working on this machine.
//
// Every one of them is a single daemon call whose structured content is the
// daemon's own JSON, except where the spec fixes a different shape for the
// tool: `wait_for_port` adds the elapsed time it measured, `health_check`
// flattens one row and names the URL it probed, and `dependency_graph` folds
// repeated edges into a connection count.

func init() { registerTools((*Server).addQueryTools) }

// Waiting is the one thing an agent asks the daemon to do slowly, so it has
// its own budget rather than the connection's ten seconds (spec 2 §1).
const (
	// DefaultWaitSeconds is `wait_for_port`'s timeout when the caller omits one.
	DefaultWaitSeconds = 30
	// MaxWaitSeconds is the longest wait the tool accepts.
	MaxWaitSeconds = 300
	// waitCancelGrace is how long a cancelled wait is given to end politely
	// before the tool gives up on the daemon's final answer.
	waitCancelGrace = 2 * time.Second
)

// Log tails are capped twice: the daemon is asked for at most MaxLogLines, and
// the text block shows at most MaxRows of them. Structured content carries
// every line the daemon returned.
const (
	// DefaultLogLines is the backlog `tail_logs` reads when asked for no size.
	DefaultLogLines = 100
	// MaxLogLines is the most lines the tool will ask the daemon for.
	MaxLogLines = 2000
)

// DefaultHistorySince and DefaultHistoryLimit are `port_history`'s window.
const (
	DefaultHistorySince = "24h"
	DefaultHistoryLimit = 50
)

// DefaultClaimTTLSeconds is `claim_port`'s reservation life: one day (spec 2 §4).
const DefaultClaimTTLSeconds = 86400

// ---------------------------------------------------------------- inputs ---

// WaitForPortInput is the argument set of wait_for_port.
type WaitForPortInput struct {
	Ports          []int `json:"ports" jsonschema:"The ports to wait for. All of them must become ready before the tool returns, unless the timeout expires first."`
	TimeoutSeconds int   `json:"timeout_seconds,omitempty" jsonschema:"How long to wait, in seconds. Defaults to 30; 300 is the maximum."`
	HTTP           any   `json:"http,omitempty" jsonschema:"How ready is defined. false (the default) accepts the port when a TCP connection succeeds; true requires an HTTP response on /; a string like \"/health\" requires an HTTP response on that path."`
}

// WaitForPortOutput is `{ready, timed_out, elapsed_ms}`.
type WaitForPortOutput struct {
	Ready     []int `json:"ready"`
	TimedOut  []int `json:"timed_out"`
	ElapsedMs int64 `json:"elapsed_ms"`
}

// NextFreePortInput is the argument set of next_free_port.
type NextFreePortInput struct {
	Start int    `json:"start,omitempty" jsonschema:"The lowest port to consider. Defaults to 3000."`
	Count int    `json:"count,omitempty" jsonschema:"How many consecutive free ports to return. Defaults to 1."`
	Range string `json:"range,omitempty" jsonschema:"A window to search instead of start, written \"3000-3999\". It overrides start."`
}

// NextFreePortOutput is `{ports: [int]}`, the shape of ports.next.
type NextFreePortOutput = rpc.PortsNextResult

// ClaimPortInput is the argument set of claim_port.
type ClaimPortInput struct {
	Project    string `json:"project,omitempty" jsonschema:"The project the ports belong to. Defaults to the git checkout containing this server's working directory."`
	Worktree   string `json:"worktree,omitempty" jsonschema:"The worktree the ports belong to. Defaults to this checkout's worktree, or main for a primary checkout."`
	Count      int    `json:"count,omitempty" jsonschema:"How many ports to reserve. Defaults to the project's worktree_ports from sonar.yaml, or 1 when it sets none."`
	TTLSeconds int64  `json:"ttl_seconds,omitempty" jsonschema:"How long the reservation lives, in seconds. Defaults to 86400 (one day). Claiming again refreshes it."`
	Release    bool   `json:"release,omitempty" jsonschema:"Give this key's ports back instead of claiming. Do this when the work is finished."`
}

// ClaimPortOutput is `{key, ports, expires_at}`; a release also reports how
// many ports it gave back.
type ClaimPortOutput struct {
	Key       string `json:"key"`
	Ports     []int  `json:"ports"`
	ExpiresAt string `json:"expires_at"`
	Released  int    `json:"released,omitempty"`
}

// TailLogsInput is the argument set of tail_logs.
type TailLogsInput struct {
	Port  int    `json:"port,omitempty" jsonschema:"The listening port whose process to read."`
	PID   int    `json:"pid,omitempty" jsonschema:"The pid whose process to read, when you have that instead of a port."`
	RunID string `json:"run_id,omitempty" jsonschema:"The run id sonar reported when the service was started."`
	Lines int    `json:"lines,omitempty" jsonschema:"How many lines from the end of the log to read. Defaults to 100; 2000 is the maximum."`
}

// TailLogsOutput is `{source, lines, truncated}`, the unary shape of ports.logs.
type TailLogsOutput struct {
	Source    string   `json:"source"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
}

// HealthCheckInput is the argument set of health_check.
type HealthCheckInput struct {
	Port int    `json:"port" jsonschema:"The port to probe."`
	Path string `json:"path,omitempty" jsonschema:"The path to request. Only \"/\" is supported; use wait_for_port with http set to a path to probe anything else."`
}

// HealthCheckOutput is `{status, code, latency_ms, url}` plus the probe's own
// verdict when it has one (contract §22).
type HealthCheckOutput struct {
	Status    string `json:"status" jsonschema:"enum=ok,enum=fail,enum=unknown"`
	Code      int    `json:"code"`
	LatencyMs int64  `json:"latency_ms"`
	URL       string `json:"url"`
	Reason    string `json:"reason,omitempty"`
}

// DependencyGraphInput has no arguments; the graph is the whole machine.
type DependencyGraphInput struct{}

// DependencyEdge is one aggregated edge: how many established connections run
// from one listening port to another.
type DependencyEdge struct {
	FromPort    int    `json:"from_port"`
	ToPort      int    `json:"to_port"`
	FromName    string `json:"from_name"`
	ToName      string `json:"to_name"`
	Connections int    `json:"connections"`
}

// DependencyGraphOutput is `{edges: [...]}`.
type DependencyGraphOutput struct {
	Edges []DependencyEdge `json:"edges"`
}

// PortHistoryInput is the argument set of port_history.
type PortHistoryInput struct {
	Port  int    `json:"port,omitempty" jsonschema:"Only events for this port. Leave it out for everything."`
	Since string `json:"since,omitempty" jsonschema:"How far back to look: a duration like \"24h\" or \"30m\", or an RFC 3339 timestamp. Defaults to 24h."`
	Limit int    `json:"limit,omitempty" jsonschema:"How many events to return, newest first. Defaults to 50."`
}

// PortHistoryOutput is `{events: [...]}`, the shape of ports.history.
type PortHistoryOutput = rpc.PortsHistoryResult

// ListSessionsInput is the argument set of list_sessions.
type ListSessionsInput struct {
	ActiveOnly *bool `json:"active_only,omitempty" jsonschema:"Only sessions that still have something running. Defaults to true; pass false to include finished ones."`
}

// ListSessionsOutput is `{sessions: [SessionRecord]}`, the shape of sessions.list.
type ListSessionsOutput = rpc.SessionsListResult

// ---------------------------------------------------------- descriptions ---

const waitForPortDescription = `Wait until one or more ports accept connections, then return which became ready and which did not.

Use this after starting a dev server instead of sleeping, polling with curl in a loop, or guessing how long a build takes. It returns as soon as every port is up, so the common case costs a second, not the full timeout.

Pass http: true (or a path like "/health") when "accepting TCP" is not enough — a server that has bound the port but is still compiling answers the socket and not the request. The tool never starts anything: a port that is not ready comes back in timed_out.`

const nextFreePortDescription = `Pick a port nothing is listening on and nothing else has claimed.

Use this instead of hardcoding 3000 or retrying after "address already in use". The daemon checks the live scan and every unexpired claim from other agents and worktrees, so two agents asking at the same time get different answers.

This is a suggestion, not a reservation: nothing stops another process taking the port a second later. When you want the same ports every time and want them held, use claim_port.`

const claimPortDescription = `Reserve ports for this project and worktree, so this checkout gets the same ports every time and parallel agents never collide.

Use it once at the start of work: the ports come back derived from the project and worktree names, so re-running the tool returns the same ports rather than new ones, and next_free_port steps over every claim but yours. Pass release: true when the work is done.

A claim is sonar's book-keeping, not an OS-level bind: a process outside sonar can still take the port, so still start your server on it and wait for it. Project and worktree default to the git checkout this MCP server was started in.`

const tailLogsDescription = `Read the last lines a listening process wrote — a container's log, the file its output was redirected to, or the file descriptors it holds open.

Use this to see why a service is failing before killing or restarting it, and prefer it to guessing at log paths or attaching to the terminal that started the process. Identify the service by port, by pid, or by the run id sonar returned when it started it.

The reply says which source it read, so you can tell "the log is empty" from "there is no log for this process".`

const healthCheckDescription = `Probe one listening port over HTTP and report whether it answers, with the status code and the round-trip time.

Use it to tell a port that is bound from a service that works: "address in use" and "the app is up" are different claims, and a proxy or a half-started dev server can bind long before it serves. status is ok, fail or unknown, with the probe's own verdict (refused, timeout, non-http) in reason.

It only probes "/". To check a specific endpoint, call wait_for_port with http set to that path.`

const dependencyGraphDescription = `Show which listening ports on this machine are talking to which, with the number of established connections on each edge.

Use it to work out what a service depends on before stopping it — a database with four connections from the api is not a safe thing to kill — and to see the shape of a stack you did not start yourself.

Edges come from the machine's current connections, so an idle dependency that has opened no socket yet does not appear.`

const portHistoryDescription = `List what has happened on this machine's ports: services that appeared, and services that went away.

Use it to answer "what was running on 3000 an hour ago" and "did my server die or did something take its port", which a live listing cannot tell you. Filter by port, and set since to the window you care about ("24h", "30m", or an RFC 3339 timestamp).

Events are newest first and come from the daemon's own record, so they include what other tools and other agents did.`

const listSessionsDescription = `List the agent sessions that have started services on this machine, with their tool, worktree, branch and how much each has running.

Use it before touching a port you did not start: a port belonging to another session is another agent's or another worktree's, and the answer to a conflict is a different port, not a kill. The ids here are the ones list_ports accepts as its session filter.

Sessions with nothing running are hidden unless you pass active_only: false.`

// ------------------------------------------------------------ registration ---

func (s *Server) addQueryTools() {
	readOnly := func() *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:         "wait_for_port",
		Title:        "Wait for ports to accept connections",
		Description:  waitForPortDescription,
		Annotations:  readOnly(),
		OutputSchema: outputSchema(WaitForPortOutput{}),
	}, s.waitForPort)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:         "next_free_port",
		Title:        "Find a free port",
		Description:  nextFreePortDescription,
		Annotations:  readOnly(),
		OutputSchema: outputSchema(NextFreePortOutput{}),
	}, s.nextFreePort)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "claim_port",
		Title:       "Reserve ports for this worktree",
		Description: claimPortDescription,
		Annotations: &mcp.ToolAnnotations{
			// Claiming the same key twice returns the same ports, and
			// releasing twice is a no-op.
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
		OutputSchema: outputSchema(ClaimPortOutput{}),
	}, s.claimPort)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:         "tail_logs",
		Title:        "Read a service's log",
		Description:  tailLogsDescription,
		Annotations:  readOnly(),
		OutputSchema: outputSchema(TailLogsOutput{}),
	}, s.tailLogs)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:         "health_check",
		Title:        "Probe a port over HTTP",
		Description:  healthCheckDescription,
		Annotations:  readOnly(),
		OutputSchema: outputSchema(HealthCheckOutput{}),
	}, s.healthCheck)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:         "dependency_graph",
		Title:        "Show which ports talk to which",
		Description:  dependencyGraphDescription,
		Annotations:  readOnly(),
		OutputSchema: outputSchema(DependencyGraphOutput{}),
	}, s.dependencyGraph)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:         "port_history",
		Title:        "List past port events",
		Description:  portHistoryDescription,
		Annotations:  readOnly(),
		OutputSchema: outputSchema(PortHistoryOutput{}),
	}, s.portHistory)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:         "list_sessions",
		Title:        "List agent sessions",
		Description:  listSessionsDescription,
		Annotations:  readOnly(),
		OutputSchema: outputSchema(ListSessionsOutput{}),
	}, s.listSessions)
}

// --------------------------------------------------------------- handlers ---

// waitForPort is the ports.wait stream, drained to its end. The stream is
// cancelled when the MCP client goes away, so a 300-second wait does not
// outlive the request that asked for it.
func (s *Server) waitForPort(ctx context.Context, _ *mcp.CallToolRequest, in WaitForPortInput) (*mcp.CallToolResult, any, error) {
	if len(in.Ports) == 0 {
		return errorResult(invalidArguments(
			"wait_for_port needs at least one port",
			`pass {"ports": [3000]}; next_free_port picks one if you do not have it yet`)), nil, nil
	}
	for _, port := range in.Ports {
		if port < 1 || port > 65535 {
			return errorResult(invalidArguments(
				fmt.Sprintf("%d is not a port number", port),
				"ports are 1-65535")), nil, nil
		}
	}
	seconds := in.TimeoutSeconds
	switch {
	case seconds == 0:
		seconds = DefaultWaitSeconds
	case seconds < 0 || seconds > MaxWaitSeconds:
		return errorResult(invalidArguments(
			fmt.Sprintf("timeout_seconds must be between 1 and %d, got %d", MaxWaitSeconds, seconds),
			"a longer wait belongs in your own loop, so you can report progress")), nil, nil
	}
	path, derr := waitHTTPPath(in.HTTP)
	if derr != nil {
		return errorResult(derr), nil, nil
	}

	started := time.Now()
	st, err := s.daemon.Stream(ctx, "ports.wait", rpc.PortsWaitParams{
		Ports:     in.Ports,
		HTTP:      path,
		TimeoutMs: seconds * 1000,
	})
	if err != nil {
		return s.failed(err)
	}
	defer st.Close()

	end, err := drainWait(ctx, st)
	if err != nil {
		return s.failed(err)
	}
	out := WaitForPortOutput{
		Ready:     orEmptyInts(end.Ready),
		TimedOut:  orEmptyInts(end.TimedOut),
		ElapsedMs: time.Since(started).Milliseconds(),
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderWait(out, path)}},
	}, out, nil
}

// nextFreePort is ports.next. It is a suggestion, not a reservation: the
// description says so, and claim_port is the tool that holds a port.
func (s *Server) nextFreePort(ctx context.Context, _ *mcp.CallToolRequest, in NextFreePortInput) (*mcp.CallToolResult, any, error) {
	params := rpc.PortsNextParams{Start: in.Start, Count: in.Count}
	if in.Range != "" {
		start, end, derr := parseRange(in.Range)
		if derr != nil {
			return errorResult(derr), nil, nil
		}
		params.Start, params.End = start, end
	}
	if params.Count < 0 {
		return errorResult(invalidArguments(
			fmt.Sprintf("count must be at least 1, got %d", params.Count), "")), nil, nil
	}

	var res rpc.PortsNextResult
	if err := s.daemon.Call(ctx, "ports.next", params, &res); err != nil {
		return s.failed(err)
	}
	if res.Ports == nil {
		res.Ports = []int{}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderFreePorts(res.Ports)}},
	}, res, nil
}

// claimPort is claims.acquire, or claims.release when the caller is done. The
// key is derived from the MCP server's own working directory, which is the
// agent's checkout: the daemon cannot see it, so the identity is computed here
// exactly as `sonar claim` computes it.
func (s *Server) claimPort(ctx context.Context, _ *mcp.CallToolRequest, in ClaimPortInput) (*mcp.CallToolResult, any, error) {
	if !s.daemon.Has("claims") {
		return errorResult(capabilityMissing("claims",
			"claim_port needs a daemon that keeps port claims")), nil, nil
	}
	if in.Count < 0 {
		return errorResult(invalidArguments(
			fmt.Sprintf("count must be at least 1, got %d", in.Count), "")), nil, nil
	}
	if in.TTLSeconds < 0 {
		return errorResult(invalidArguments(
			fmt.Sprintf("ttl_seconds must be positive, got %d", in.TTLSeconds), "")), nil, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	project, worktree := claims.Identity(cwd, in.Project, in.Worktree)
	key := claims.Key(project, worktree)

	if in.Release {
		var res rpc.ClaimsReleaseResult
		if err := s.daemon.Call(ctx, "claims.release", rpc.ClaimsReleaseParams{Key: key}, &res); err != nil {
			return s.failed(err)
		}
		out := ClaimPortOutput{Key: key, Ports: []int{}, Released: res.Released}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: renderRelease(out)}},
		}, out, nil
	}

	ttl := in.TTLSeconds
	if ttl == 0 {
		ttl = DefaultClaimTTLSeconds
	}
	var res rpc.ClaimsAcquireResult
	if err := s.daemon.Call(ctx, "claims.acquire", rpc.ClaimsAcquireParams{
		Project:    project,
		Worktree:   worktree,
		Count:      in.Count,
		TTLSeconds: ttl,
	}, &res); err != nil {
		return s.failed(err)
	}
	out := ClaimPortOutput{Key: res.Key, Ports: orEmptyInts(res.Ports), ExpiresAt: res.ExpiresAt}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderClaim(out)}},
	}, out, nil
}

// tailLogs is the unary half of ports.logs. run_id is resolved here, through
// runs.list, because the daemon's log selector takes a port or a pid.
func (s *Server) tailLogs(ctx context.Context, _ *mcp.CallToolRequest, in TailLogsInput) (*mcp.CallToolResult, any, error) {
	selectors := 0
	for _, given := range []bool{in.Port > 0, in.PID > 0, in.RunID != ""} {
		if given {
			selectors++
		}
	}
	if selectors != 1 {
		return errorResult(invalidArguments(
			"tail_logs needs exactly one of port, pid and run_id",
			`pass {"port": 3000}; list_ports shows the port, pid and run of everything listening`)), nil, nil
	}

	lines, capped := in.Lines, false
	switch {
	case lines <= 0:
		lines = DefaultLogLines
	case lines > MaxLogLines:
		lines, capped = MaxLogLines, true
	}

	sel := rpc.Selector{}
	switch {
	case in.Port > 0:
		sel.Port = &in.Port
	case in.PID > 0:
		sel.PID = &in.PID
	default:
		resolved, err := s.selectorForRun(ctx, in.RunID)
		if err != nil {
			return s.failed(err)
		}
		sel = resolved
	}

	var res rpc.PortsLogsResult
	if err := s.daemon.Call(ctx, "ports.logs", rpc.PortsLogsParams{
		Selector: sel,
		Lines:    lines,
	}, &res); err != nil {
		return s.failed(err)
	}
	out := TailLogsOutput{Source: res.Source, Lines: res.Lines, Truncated: res.Truncated}
	if out.Lines == nil {
		out.Lines = []string{}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderLogs(out, lines, capped)}},
	}, out, nil
}

// healthCheck is ports.health for one port, flattened to the single row the
// tool promises.
func (s *Server) healthCheck(ctx context.Context, _ *mcp.CallToolRequest, in HealthCheckInput) (*mcp.CallToolResult, any, error) {
	if in.Port < 1 || in.Port > 65535 {
		return errorResult(invalidArguments(
			"health_check needs a port",
			`pass {"port": 3000}; list_ports shows what is listening`)), nil, nil
	}
	path := strings.TrimSpace(in.Path)
	if path != "" && path != "/" {
		return errorResult(invalidArguments(
			fmt.Sprintf("health_check probes \"/\", not %q", path),
			`call wait_for_port with {"ports": [`+strconv.Itoa(in.Port)+`], "http": "`+path+`"} to probe another path`)), nil, nil
	}

	var res rpc.PortsHealthResult
	if err := s.daemon.Call(ctx, "ports.health", rpc.PortsHealthParams{Ports: []int{in.Port}}, &res); err != nil {
		return s.failed(err)
	}

	out := HealthCheckOutput{
		Status: state.HealthUnknown,
		URL:    fmt.Sprintf("http://localhost:%d/", in.Port),
	}
	for _, row := range res.Results {
		if row.Port != in.Port {
			continue
		}
		out.Status, out.Code, out.LatencyMs, out.Reason = normalizeHealth(row.Status), row.Code, row.LatencyMs, row.Reason
		break
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderHealth(in.Port, out)}},
	}, out, nil
}

// dependencyGraph is ports.graph with its repeated edges folded into counts:
// the daemon reports one row per established connection, and what an agent
// wants to know is how many there are between two services.
func (s *Server) dependencyGraph(ctx context.Context, _ *mcp.CallToolRequest, _ DependencyGraphInput) (*mcp.CallToolResult, any, error) {
	var res rpc.PortsGraphResult
	if err := s.daemon.Call(ctx, "ports.graph", rpc.Empty{}, &res); err != nil {
		return s.failed(err)
	}
	out := DependencyGraphOutput{Edges: aggregateEdges(res.Connections)}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderGraph(out.Edges)}},
	}, out, nil
}

// portHistory is ports.history. `since` is passed through untouched: the
// daemon owns the grammar (a duration or an RFC 3339 timestamp, contract §22)
// and rejecting it here would be a second, drifting copy of that rule.
func (s *Server) portHistory(ctx context.Context, _ *mcp.CallToolRequest, in PortHistoryInput) (*mcp.CallToolResult, any, error) {
	if in.Limit < 0 {
		return errorResult(invalidArguments(
			fmt.Sprintf("limit must be positive, got %d", in.Limit), "")), nil, nil
	}
	since := strings.TrimSpace(in.Since)
	if since == "" {
		since = DefaultHistorySince
	}
	limit := in.Limit
	if limit == 0 {
		limit = DefaultHistoryLimit
	}

	params := rpc.PortsHistoryParams{Since: &since, Limit: limit}
	if in.Port > 0 {
		params.Port = &in.Port
	}

	var res rpc.PortsHistoryResult
	if err := s.daemon.Call(ctx, "ports.history", params, &res); err != nil {
		return s.failed(err)
	}
	if res.Events == nil {
		res.Events = []rpc.HistoryEvent{}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderHistory(res.Events, since)}},
	}, res, nil
}

// listSessions is sessions.list.
func (s *Server) listSessions(ctx context.Context, _ *mcp.CallToolRequest, in ListSessionsInput) (*mcp.CallToolResult, any, error) {
	if !s.daemon.Has("sessions") {
		return errorResult(capabilityMissing("sessions",
			"list_sessions needs a daemon that records agent sessions")), nil, nil
	}
	activeOnly := true
	if in.ActiveOnly != nil {
		activeOnly = *in.ActiveOnly
	}

	var res rpc.SessionsListResult
	if err := s.daemon.Call(ctx, "sessions.list",
		rpc.SessionsListParams{ActiveOnly: activeOnly}, &res); err != nil {
		return s.failed(err)
	}
	if res.Sessions == nil {
		res.Sessions = []state.SessionRecord{}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderSessions(res.Sessions, activeOnly)}},
	}, res, nil
}

// ---------------------------------------------------------------- helpers ---

// drainWait reads a ports.wait stream to its end. A cancelled MCP request
// cancels the stream at the daemon (contract §20: a cancelled stream still
// ends) rather than abandoning it, so the daemon stops probing as soon as the
// agent stops caring.
func drainWait(ctx context.Context, st *client.Stream) (rpc.PortsWaitEnd, error) {
	chunks := st.Chunks()
	done := ctx.Done()
	var giveUp <-chan time.Time

	for {
		select {
		case <-done:
			done = nil
			cancelCtx, cancel := context.WithTimeout(context.Background(), waitCancelGrace)
			_ = st.Cancel(cancelCtx)
			cancel()
			giveUp = time.After(waitCancelGrace)
		case <-giveUp:
			return rpc.PortsWaitEnd{}, ctx.Err()
		case _, ok := <-chunks:
			// Chunks are progress, not results: the end carries the answer.
			if !ok {
				chunks = nil
			}
		case end := <-st.End():
			if end.Err != nil {
				return rpc.PortsWaitEnd{}, end.Err
			}
			var out rpc.PortsWaitEnd
			if err := end.Decode(&out); err != nil {
				return rpc.PortsWaitEnd{}, err
			}
			return out, nil
		}
	}
}

// waitHTTPPath reads the `http` argument's three forms: false (a TCP connect),
// true ("/"), or a path.
func waitHTTPPath(v any) (*string, *DomainError) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case bool:
		if !t {
			return nil, nil
		}
		path := "/"
		return &path, nil
	case string:
		path := strings.TrimSpace(t)
		if path == "" {
			return nil, nil
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		return &path, nil
	default:
		return nil, invalidArguments(
			fmt.Sprintf("http must be true, false or a path, got %v", v),
			`use {"http": true} for a request to "/" or {"http": "/health"} for a path`)
	}
}

// parseRange reads the "3000-3999" form of next_free_port's range argument.
func parseRange(raw string) (start, end int, derr *DomainError) {
	bad := func() *DomainError {
		return invalidArguments(
			fmt.Sprintf("range %q is not a port window", raw),
			`write it as "3000-3999"`)
	}
	lo, hi, found := strings.Cut(strings.TrimSpace(raw), "-")
	if !found {
		return 0, 0, bad()
	}
	start, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return 0, 0, bad()
	}
	end, err = strconv.Atoi(strings.TrimSpace(hi))
	if err != nil {
		return 0, 0, bad()
	}
	if start < 1 || end > 65535 || start > end {
		return 0, 0, invalidArguments(
			fmt.Sprintf("range %q is not a usable port window", raw),
			"ports are 1-65535 and the low end comes first")
	}
	return start, end, nil
}

// selectorForRun turns a run id into the log selector the daemon understands:
// the run's listening port, or its pid while it has none.
func (s *Server) selectorForRun(ctx context.Context, runID string) (rpc.Selector, error) {
	if !s.daemon.Has("runs") {
		return rpc.Selector{}, capabilityMissing("runs",
			"this daemon does not keep a run registry, so run_id cannot be resolved")
	}
	var res rpc.RunsListResult
	if err := s.daemon.Call(ctx, "runs.list", rpc.Empty{}, &res); err != nil {
		return rpc.Selector{}, err
	}
	for _, run := range res.Runs {
		if run.ID != runID && run.Name != runID && !strings.HasPrefix(run.ID, runID) {
			continue
		}
		if len(run.Ports) > 0 {
			port := run.Ports[0]
			return rpc.Selector{Port: &port}, nil
		}
		pid := run.PID
		return rpc.Selector{PID: &pid}, nil
	}
	return rpc.Selector{}, Domain("not_found",
		"no run matches "+runID,
		"list_ports shows the run behind every port it started")
}

// capabilityMissing is the answer of a tool whose daemon capability is absent.
// The tool stays registered so the client's tool list can be cached (spec 2 §1).
func capabilityMissing(capability, what string) *DomainError {
	return Domain(CodeCapabilityMissing, what,
		fmt.Sprintf("this daemon does not advertise %q; upgrade sonar and run `sonar daemon restart`", capability))
}

// normalizeHealth keeps the wire's three-value vocabulary (contract §22) even
// if an older daemon publishes a probe verdict where a status belongs.
func normalizeHealth(status string) string {
	switch status {
	case state.HealthOK, state.HealthFail, state.HealthUnknown:
		return status
	case "healthy":
		return state.HealthOK
	case "unhealthy", "refused", "timeout", "non-http":
		return state.HealthFail
	case "":
		return state.HealthUnknown
	default:
		return state.HealthUnknown
	}
}

// aggregateEdges folds the daemon's per-connection rows into one edge per
// (from, to) pair, ordered by port so the same graph renders the same way
// twice.
func aggregateEdges(rows []rpc.GraphEdge) []DependencyEdge {
	type key struct{ from, to int }
	index := map[key]*DependencyEdge{}
	order := []key{}
	for _, row := range rows {
		k := key{row.FromPort, row.ToPort}
		edge, ok := index[k]
		if !ok {
			index[k] = &DependencyEdge{
				FromPort: row.FromPort, ToPort: row.ToPort,
				FromName: row.FromProcess, ToName: row.ToProcess,
			}
			order = append(order, k)
			edge = index[k]
		}
		edge.Connections++
		// A later row may know a name an earlier one did not.
		if edge.FromName == "" {
			edge.FromName = row.FromProcess
		}
		if edge.ToName == "" {
			edge.ToName = row.ToProcess
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].from != order[j].from {
			return order[i].from < order[j].from
		}
		return order[i].to < order[j].to
	})
	out := make([]DependencyEdge, 0, len(order))
	for _, k := range order {
		out = append(out, *index[k])
	}
	return out
}

func orEmptyInts(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}

// -------------------------------------------------------------- rendering ---

func renderWait(out WaitForPortOutput, path *string) string {
	how := "accepting connections"
	if path != nil {
		how = "answering HTTP on " + *path
	}
	elapsed := time.Duration(out.ElapsedMs) * time.Millisecond

	total := len(out.Ready) + len(out.TimedOut)
	if total == 1 {
		if len(out.Ready) == 1 {
			return fmt.Sprintf("port %d is %s after %s", out.Ready[0], how, elapsed)
		}
		return fmt.Sprintf("port %d is still not %s after %s", out.TimedOut[0], how, elapsed)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d %s %s after %s\n", len(out.Ready), total,
		plural(total, "port is", "ports are"), how, elapsed)
	if len(out.Ready) > 0 {
		fmt.Fprintf(&b, "ready: %s\n", joinIntsInOrder(out.Ready))
	}
	if len(out.TimedOut) > 0 {
		fmt.Fprintf(&b, "timed out: %s\n", joinIntsInOrder(out.TimedOut))
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderFreePorts(ports []int) string {
	if len(ports) == 0 {
		return "no free port was found in that range"
	}
	if len(ports) == 1 {
		return fmt.Sprintf("port %d is free", ports[0])
	}
	return fmt.Sprintf("ports %s are free", joinIntsInOrder(ports))
}

func renderClaim(out ClaimPortOutput) string {
	if len(out.Ports) == 0 {
		return "no ports are claimed for " + out.Key
	}
	return fmt.Sprintf("%s claims %s until %s",
		out.Key, joinIntsInOrder(out.Ports), dash(out.ExpiresAt))
}

func renderRelease(out ClaimPortOutput) string {
	return fmt.Sprintf("released %d %s held by %s",
		out.Released, plural(out.Released, "port", "ports"), out.Key)
}

func renderLogs(out TailLogsOutput, asked int, capped bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s from %s", len(out.Lines), plural(len(out.Lines), "line", "lines"), dash(out.Source))
	if out.Truncated {
		b.WriteString(" (older lines were not read)")
	}
	if capped {
		fmt.Fprintf(&b, " (capped at %d lines)", asked)
	}
	b.WriteString("\n")
	if len(out.Lines) == 0 {
		return strings.TrimRight(b.String(), "\n")
	}

	shown := out.Lines
	// The text block keeps the newest lines, which are the ones a failure is
	// explained by; structured content still carries every line.
	if len(shown) > MaxRows {
		shown = shown[len(shown)-MaxRows:]
		fmt.Fprintf(&b, "showing the last %d of %d lines\n", MaxRows, len(out.Lines))
	}
	b.WriteString("\n")
	b.WriteString(strings.Join(shown, "\n"))
	return b.String()
}

func renderHealth(port int, out HealthCheckOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "port %d is %s", port, out.Status)
	if out.Reason != "" && out.Reason != out.Status {
		fmt.Fprintf(&b, " (%s)", out.Reason)
	}
	if out.Code != 0 {
		fmt.Fprintf(&b, ", http %d", out.Code)
	}
	if out.LatencyMs != 0 {
		fmt.Fprintf(&b, ", %dms", out.LatencyMs)
	}
	fmt.Fprintf(&b, " — %s", out.URL)
	return b.String()
}

func renderGraph(edges []DependencyEdge) string {
	if len(edges) == 0 {
		return "no port on this machine is connected to another"
	}
	shown, truncated := edges, false
	if len(shown) > MaxRows {
		shown, truncated = shown[:MaxRows], true
	}

	rows := make([][]string, 0, len(shown))
	for _, e := range shown {
		rows = append(rows, []string{
			strconv.Itoa(e.FromPort), dash(e.FromName),
			strconv.Itoa(e.ToPort), dash(e.ToName),
			strconv.Itoa(e.Connections),
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s\n\n", len(edges), plural(len(edges), "edge", "edges"))
	b.WriteString(table([]string{"FROM", "NAME", "TO", "NAME", "CONNECTIONS"}, rows))
	if truncated {
		b.WriteString("\n" + truncationNote(len(shown), len(edges)))
	}
	return b.String()
}

func renderHistory(events []rpc.HistoryEvent, since string) string {
	if len(events) == 0 {
		return "nothing has happened on these ports since " + since
	}
	shown, truncated := events, false
	if len(shown) > MaxRows {
		shown, truncated = shown[:MaxRows], true
	}

	rows := make([][]string, 0, len(shown))
	for _, e := range shown {
		rows = append(rows, []string{
			dash(e.At), dash(e.Kind), strconv.Itoa(e.Port),
			strconv.Itoa(e.PID), dash(e.DisplayName), dash(e.Group),
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s since %s\n\n", len(events), plural(len(events), "event", "events"), since)
	b.WriteString(table([]string{"AT", "KIND", "PORT", "PID", "NAME", "GROUP"}, rows))
	if truncated {
		b.WriteString("\n" + truncationNote(len(shown), len(events)))
	}
	return b.String()
}

func renderSessions(sessions []state.SessionRecord, activeOnly bool) string {
	if len(sessions) == 0 {
		if activeOnly {
			return "no agent session has anything running; pass active_only: false for finished ones"
		}
		return "no agent session has been recorded on this machine"
	}
	shown, truncated := sessions, false
	if len(shown) > MaxRows {
		shown, truncated = shown[:MaxRows], true
	}

	rows := make([][]string, 0, len(shown))
	for _, s := range shown {
		rows = append(rows, []string{
			s.ID, dash(s.Tool), dash(s.Worktree), dash(s.Branch),
			strconv.Itoa(s.Runs), strconv.Itoa(s.Ports), strconv.Itoa(s.Groups),
			yesNo(s.Active), dash(s.LastSeen),
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s\n\n", len(sessions), plural(len(sessions), "session", "sessions"))
	b.WriteString(table(
		[]string{"ID", "TOOL", "WORKTREE", "BRANCH", "RUNS", "PORTS", "GROUPS", "ACTIVE", "LAST SEEN"}, rows))
	if truncated {
		b.WriteString("\n" + truncationNote(len(shown), len(sessions)))
	}
	return b.String()
}

func joinIntsInOrder(v []int) string {
	parts := make([]string, 0, len(v))
	for _, n := range v {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
