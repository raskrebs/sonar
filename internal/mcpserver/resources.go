package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// The resources (spec 2, section 1.2). A resource is the same daemon read a
// tool does, minus the argument set: a client that wants "what is listening"
// as context rather than as an answer to a question subscribes to
// `sonar://ports` once and re-reads it when the server says it changed.
//
// The bodies are the daemon's own JSON, indented the way `sonar list --json`
// prints it, so a person reading a transcript and a model reading the resource
// see the same document.
const (
	// URIPorts is `ports.list` with no apps and no stats: the ports a
	// developer means when they say "what is running".
	URIPorts = "sonar://ports"
	// URIGroups is `groups.list`.
	URIGroups = "sonar://groups"
	// URIGroupTemplate addresses one group: `groups.inspect`.
	URIGroupTemplate = "sonar://groups/{name}"
	// URISessions is `sessions.list`.
	URISessions = "sonar://sessions"

	// groupURIPrefix is the literal half of URIGroupTemplate.
	groupURIPrefix = "sonar://groups/"

	// ResourceMIME is the content type of every sonar resource.
	ResourceMIME = "application/json"
)

// resourceRegistrars and promptRegistrars are the registration lists the
// server walks at startup. A file that owns a resource or a prompt registers
// it from its own init(), so adding one never means editing server.go — the
// same convention the tool files use for toolRegistrars.
var (
	resourceRegistrars []func(*Server)
	promptRegistrars   []func(*Server)
)

// registerResources adds a resource registrar. Call it from an init().
func registerResources(f func(*Server)) { resourceRegistrars = append(resourceRegistrars, f) }

// registerPrompts adds a prompt registrar. Call it from an init().
func registerPrompts(f func(*Server)) { promptRegistrars = append(promptRegistrars, f) }

func init() { registerResources((*Server).addResources) }

const portsResourceDescription = `Everything listening on this machine's TCP ports right now, as JSON: the same rows list_ports returns, with the project, group, run and agent session behind each port. Desktop applications and per-process stats are left out.

Subscribe to this resource to keep it fresh: the server sends a resources/updated notification when the daemon's port table changes, at most once a second.`

const groupsResourceDescription = `Every group the daemon knows, as JSON: projects with a sonar.yaml, compose projects, git roots and ` + "`sonar start`" + ` runs, each with its member ports and declared services.

Read this before starting a project's services: it says what the project declares and what part of it is already up.`

const groupResourceDescription = `One group in full, as JSON: its services, their declared and actual ports, and every port that belongs to it. The URI is sonar://groups/<name>, where <name> is a group name from sonar://groups or list_groups.`

const sessionsResourceDescription = `The agent sessions the daemon has seen, as JSON: which tool and worktree each one is, when it was last active, and how many runs, ports and groups it owns.

Use it to tell your own ports from another session's before you free anything.`

// sessionsUnavailableNote replaces the second half of the sessions description
// when the daemon does not track sessions. The resource stays registered, and
// stays an empty list, so a client can cache the resource list across daemon
// versions the way it caches the tool list (spec 2, "Runtime").
const sessionsUnavailableNote = `

This daemon does not track sessions (its daemon.hello does not advertise the "sessions" capability), so the list is empty until it is upgraded.`

// addResources registers the four resources and the group template.
func (s *Server) addResources() {
	s.mcp.AddResource(&mcp.Resource{
		URI:         URIPorts,
		Name:        "ports",
		Title:       "Listening ports",
		Description: portsResourceDescription,
		MIMEType:    ResourceMIME,
	}, s.readPortsResource)

	s.mcp.AddResource(&mcp.Resource{
		URI:         URIGroups,
		Name:        "groups",
		Title:       "Groups",
		Description: groupsResourceDescription,
		MIMEType:    ResourceMIME,
	}, s.readGroupsResource)

	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: URIGroupTemplate,
		Name:        "group",
		Title:       "One group",
		Description: groupResourceDescription,
		MIMEType:    ResourceMIME,
	}, s.readGroupResource)

	description := sessionsResourceDescription
	if !s.daemon.Has("sessions") {
		description += sessionsUnavailableNote
	}
	s.mcp.AddResource(&mcp.Resource{
		URI:         URISessions,
		Name:        "sessions",
		Title:       "Agent sessions",
		Description: description,
		MIMEType:    ResourceMIME,
	}, s.readSessionsResource)
}

func (s *Server) readPortsResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	var res rpc.PortsListResult
	if err := s.daemon.Call(ctx, "ports.list", rpc.PortsListParams{}, &res); err != nil {
		return nil, s.resourceFailed(req.Params.URI, err)
	}
	if res.Ports == nil {
		res.Ports = []state.Port{}
	}
	return jsonResource(req.Params.URI, res)
}

func (s *Server) readGroupsResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	var res rpc.GroupsListResult
	if err := s.daemon.Call(ctx, "groups.list", rpc.Empty{}, &res); err != nil {
		return nil, s.resourceFailed(req.Params.URI, err)
	}
	if res.Groups == nil {
		res.Groups = []state.Group{}
	}
	return jsonResource(req.Params.URI, res)
}

// readGroupResource is the template's handler. The daemon's own `not_found`
// for an unknown group is what the client sees, so a model that guessed a
// group name gets the same answer here as from a tool call.
func (s *Server) readGroupResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	name, ok := GroupNameFromURI(req.Params.URI)
	if !ok || name == "" {
		return nil, resourceError(req.Params.URI, Domain("not_found",
			fmt.Sprintf("%s does not name a group", req.Params.URI),
			"group URIs are sonar://groups/<name>; read sonar://groups for the names"))
	}

	var res rpc.GroupsInspectResult
	if err := s.daemon.Call(ctx, "groups.inspect", rpc.GroupsInspectParams{Name: name}, &res); err != nil {
		return nil, s.resourceFailed(req.Params.URI, err)
	}
	if res.Ports == nil {
		res.Ports = []state.Port{}
	}
	return jsonResource(req.Params.URI, res)
}

// readSessionsResource serves `sessions.list` when the daemon has sessions,
// and an empty list when it does not. An older daemon is not an error: the
// resource list is advertised once and cached, so a resource that disappeared
// would look like a broken server rather than an older one.
func (s *Server) readSessionsResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	res := rpc.SessionsListResult{Sessions: []state.SessionRecord{}}
	if !s.daemon.Has("sessions") {
		return jsonResource(req.Params.URI, res)
	}
	if err := s.daemon.Call(ctx, "sessions.list", rpc.SessionsListParams{}, &res); err != nil {
		return nil, s.resourceFailed(req.Params.URI, err)
	}
	if res.Sessions == nil {
		res.Sessions = []state.SessionRecord{}
	}
	return jsonResource(req.Params.URI, res)
}

// GroupURI is the resource URI of one group. The name is escaped, because a
// group name is a project name and may carry anything a directory name can.
func GroupURI(name string) string { return groupURIPrefix + url.PathEscape(name) }

// GroupNameFromURI is GroupURI's inverse. It reports false for a URI that is
// not a group URI at all.
func GroupNameFromURI(uri string) (string, bool) {
	rest, ok := strings.CutPrefix(uri, groupURIPrefix)
	if !ok {
		return "", false
	}
	name, err := url.PathUnescape(rest)
	if err != nil {
		// A malformed escape is not a name; the raw text is the best guess
		// and the daemon will say not_found for it.
		name = rest
	}
	return name, true
}

// knownResourceURI reports whether uri is one this server serves. It is the
// check resources/subscribe makes: the SDK does not validate the URI of a
// subscription, and a subscription to a URI that will never change is a
// silent hang rather than an error.
func knownResourceURI(uri string) bool {
	switch uri {
	case URIPorts, URIGroups, URISessions:
		return true
	}
	name, ok := GroupNameFromURI(uri)
	return ok && name != ""
}

// jsonResource is the one body format: the daemon's result, indented two
// spaces the way the CLI's --json output is.
func jsonResource(uri string, v any) (*mcp.ReadResourceResult, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mcpserver: encoding %s: %w", uri, err)
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI:      uri,
		MIMEType: ResourceMIME,
		Text:     string(body),
	}}}, nil
}

// resourceFailed turns a daemon call's failure into a resource read failure.
// A resource read has no IsError channel the way a tool result does, so every
// domain failure is a JSON-RPC error here — but it keeps the tools' wording,
// "<code>: <message> (<hint>)", and carries the same {code, message, hint} in
// the error's data, so a client branches on the same vocabulary either way.
func (s *Server) resourceFailed(uri string, err error) error {
	domain, ok := asDomain(err)
	if !ok {
		return err
	}
	return resourceError(uri, domain)
}

func resourceError(uri string, e *DomainError) error {
	code := int64(jsonrpc.CodeInternalError)
	if e.Code == "not_found" {
		code = mcp.CodeResourceNotFound
	}
	data, err := json.Marshal(map[string]any{
		"uri": uri,
		"error": map[string]string{
			"code":    e.Code,
			"message": e.Message,
			"hint":    e.Hint,
		},
	})
	if err != nil {
		data = nil
	}
	return &jsonrpc.Error{Code: code, Message: e.Error(), Data: data}
}
