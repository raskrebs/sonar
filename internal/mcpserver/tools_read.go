package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// The read tools. Both are one daemon call, and both return the daemon's own
// JSON as structured content: `{ports: [Port]}` and `{port: Port}` are the
// shapes `sonar list --json` and `sonar info --json` print.

// ListPortsInput is the argument set of list_ports.
type ListPortsInput struct {
	Group       string `json:"group,omitempty" jsonschema:"Only ports in this group. A group is a project (a sonar.yaml, a compose project or a git root); a sonar start name or run id also selects that run's ports."`
	Session     string `json:"session,omitempty" jsonschema:"Only ports started by this agent session id, as reported in each port's session object. A unique prefix of the id works too; list_sessions shows the ids."`
	Type        string `json:"type,omitempty" jsonschema:"Only ports of one kind: user (a process someone started), docker (a published container port) or system (a service the machine runs)."`
	IncludeApps bool   `json:"include_apps,omitempty" jsonschema:"Include desktop applications that happen to listen (Chrome, Docker Desktop, Spotlight). Off by default because they are noise for development work."`
	Stats       bool   `json:"stats,omitempty" jsonschema:"Collect CPU, memory, thread and uptime stats for each port. Costs an extra scan; leave off unless you were asked about resource use."`
}

// ListPortsOutput is `{ports: [Port]}`, the shape of ports.list and of
// `sonar list --json`.
type ListPortsOutput = rpc.PortsListResult

// InspectPortInput is the argument set of inspect_port.
type InspectPortInput struct {
	Port int `json:"port" jsonschema:"The listening port number to inspect."`
	PID  int `json:"pid,omitempty" jsonschema:"The pid that owns the port, when more than one process answers on it. Leave it out unless a previous call reported the port as ambiguous."`
}

// InspectPortOutput is `{port: Port}`: one full row, with the cwd, project
// root, group, session, docker and health a list omits.
type InspectPortOutput struct {
	Port state.Port `json:"port"`
}

const listPortsDescription = `List everything listening on this machine's TCP ports, with the project, group, run and agent session behind each one.

Use this instead of lsof, netstat, ss or ps: those show sockets, this shows services — which repo a port belongs to, whether sonar started it, and which agent session owns it. Start here whenever you need to know what is running, before starting a server on a port, or when a command reports "address already in use".

Filter with group, session or type rather than listing everything and reading past it; the text block is capped at 200 rows.`

const inspectPortDescription = `Inspect one listening port: its process, command, working directory, project root, group, agent session, container and health.

Call this before you act on a port you did not start. It answers the only question worth asking about a busy port — whose is it — and the answer is usually "not yours": another worktree, another session, or a human's editor. A port owned by someone else is a reason to pick a different port, not to kill anything.

If the port is bound by more than one process the call reports it as ambiguous; pass the pid from list_ports to pick one.`

func init() { registerTools((*Server).addReadTools) }

func (s *Server) addReadTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_ports",
		Title:       "List listening ports",
		Description: listPortsDescription,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
			// The domain is this machine's daemon and nothing else.
			OpenWorldHint: boolPtr(false),
		},
		OutputSchema: outputSchema(ListPortsOutput{}),
	}, s.listPorts)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "inspect_port",
		Title:       "Inspect one port",
		Description: inspectPortDescription,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
		OutputSchema: outputSchema(InspectPortOutput{}),
	}, s.inspectPort)
}

// listPorts is ports.list. Every filter is the daemon's own, including
// session: it matches an id or a unique prefix of one (contract §29), which is
// the daemon's rule and not a second one invented here.
func (s *Server) listPorts(ctx context.Context, _ *mcp.CallToolRequest, in ListPortsInput) (*mcp.CallToolResult, any, error) {
	params := rpc.PortsListParams{All: in.IncludeApps}
	if in.Group != "" {
		params.Group = &in.Group
	}
	if in.Type != "" {
		switch in.Type {
		case "user", "docker", "system", "proxy":
			params.Filter = &in.Type
		default:
			return errorResult(unsupportedValue("type", in.Type, "user", "docker", "system")), nil, nil
		}
	}
	if in.Session != "" {
		params.Session = &in.Session
	}
	if in.Stats {
		params.Include = rpc.Include{"stats"}
	}

	var res rpc.PortsListResult
	if err := s.daemon.Call(ctx, "ports.list", params, &res); err != nil {
		return s.failed(err)
	}
	if res.Ports == nil {
		res.Ports = []state.Port{}
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderPorts(res.Ports)}},
	}, res, nil
}

// inspectPort is ports.inspect. The daemon's selector rule is "exactly one of
// port and pid" (contract §3), so a call carrying both is sent as the pid —
// the narrower of the two — and the answer is checked against the port the
// caller asked about.
func (s *Server) inspectPort(ctx context.Context, _ *mcp.CallToolRequest, in InspectPortInput) (*mcp.CallToolResult, any, error) {
	if in.Port <= 0 && in.PID <= 0 {
		return errorResult(invalidArguments(
			"inspect_port needs a port",
			`pass {"port": 3000}; list_ports shows what is listening`)), nil, nil
	}

	sel := rpc.Selector{}
	switch {
	case in.PID > 0:
		sel.PID = &in.PID
	default:
		sel.Port = &in.Port
	}

	var res rpc.PortsInspectResult
	if err := s.daemon.Call(ctx, "ports.inspect", sel, &res); err != nil {
		return s.failed(err)
	}
	if in.PID > 0 && in.Port > 0 && res.Port.Port != in.Port {
		return errorResult(Domain("not_found",
			describeMismatch(in.Port, in.PID, res.Port.Port),
			"call list_ports and use the pid printed next to the port you meant")), nil, nil
	}

	out := InspectPortOutput{Port: res.Port}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderPort(res.Port)}},
	}, out, nil
}

// failed turns a daemon call's error into the right kind of tool outcome: a
// domain result the model can read and recover from, or — for anything that is
// not a daemon answer at all, such as the MCP client cancelling — a protocol
// error.
func (s *Server) failed(err error) (*mcp.CallToolResult, any, error) {
	if domain, ok := asDomain(err); ok {
		return errorResult(domain), nil, nil
	}
	return nil, nil, err
}

func describeMismatch(wantPort, pid, gotPort int) string {
	return fmt.Sprintf("pid %d listens on port %d, not on port %d", pid, gotPort, wantPort)
}
