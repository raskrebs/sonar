package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"
	"github.com/raskrebs/sonar/internal/state"
)

//go:generate go run ../../../cmd/gen-schema -o ../../../docs/schema/protocol.schema.json

// Description is one method's wire contract. Chunk and End are non-nil only
// for streaming methods (contract §1): those reply with a subscription id, push
// stream.chunk notifications carrying Chunk, and finish with a stream.end
// carrying End.
type Description struct {
	Method string
	Params any
	Result any
	Chunk  any
	End    any
}

// NotificationDescription is one server-pushed message's wire contract: the
// notification's method name and the type of its params. Notifications have no
// id and no result — nothing replies to them — so params is all there is.
type NotificationDescription struct {
	Method string
	Params any
}

var registry = map[string]Description{}

var notificationRegistry = map[string]NotificationDescription{}

// DescribeNotification registers a notification the daemon pushes. Without
// this the schema described only the request/response half of the protocol and
// every client had to hand-write `state.delta`, `state.event`, `stream.chunk`
// and `stream.end` (contract §1).
func DescribeNotification(method string, params any) {
	notificationRegistry[method] = NotificationDescription{Method: method, Params: params}
}

// Notifications returns a copy of the notification registry.
func Notifications() map[string]NotificationDescription {
	out := make(map[string]NotificationDescription, len(notificationRegistry))
	for k, v := range notificationRegistry {
		out[k] = v
	}
	return out
}

// NotificationNames returns every registered notification name, sorted.
func NotificationNames() []string {
	names := make([]string, 0, len(notificationRegistry))
	for k := range notificationRegistry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Describe registers a method's wire contract. Called from init() here for the
// core namespaces; the packages that own the expose, map, sessions and claims
// namespaces call it from their own init() once they exist.
func Describe(method string, params, result, chunk, end any) {
	registry[method] = Description{
		Method: method,
		Params: params,
		Result: result,
		Chunk:  chunk,
		End:    end,
	}
}

// Methods returns a copy of the registry.
func Methods() map[string]Description {
	out := make(map[string]Description, len(registry))
	for k, v := range registry {
		out[k] = v
	}
	return out
}

// MethodNames returns every registered method name, sorted.
func MethodNames() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// MethodSchema is one entry of the document's methods map.
type MethodSchema struct {
	Params      json.RawMessage `json:"params"`
	Result      json.RawMessage `json:"result"`
	StreamChunk json.RawMessage `json:"stream_chunk,omitempty"`
	StreamEnd   json.RawMessage `json:"stream_end,omitempty"`
}

// NotificationSchema is one entry of the document's notifications map: the
// params a client decodes when that notification arrives.
type NotificationSchema struct {
	Params json.RawMessage `json:"params"`
}

// Document is the bundle `sonar daemon schema` prints and
// docs/schema/protocol.schema.json holds (contract §6).
type Document struct {
	Schema          string                        `json:"$schema"`
	ID              string                        `json:"$id"`
	Title           string                        `json:"title"`
	Description     string                        `json:"description"`
	ProtocolVersion string                        `json:"protocol_version"`
	Definitions     map[string]json.RawMessage    `json:"definitions"`
	Methods         map[string]MethodSchema       `json:"methods"`
	Notifications   map[string]NotificationSchema `json:"notifications"`
}

// namedTypes are the data-model definitions contract §6 requires by name.
// Reflecting them explicitly guarantees they exist even when no method
// references them directly yet.
func namedTypes() []any {
	return []any{
		state.Port{}, state.Group{}, state.Tunnel{}, state.Proxy{},
		state.Session{}, state.SessionRecord{}, state.Claim{}, state.Host{},
		state.Snapshot{}, state.Delta{}, state.Event{}, state.Service{},
		state.Stats{}, state.Health{}, state.Docker{}, state.Run{},
		state.KillResult{},
		Error{}, MutationResult{}, KillEnvelope{}, DoctorCheck{},
		// The streaming envelopes (contract §1). A client needs the shape of
		// {id, data} to read any stream at all, so it is a named definition
		// rather than something every generator re-invents.
		StreamChunk{}, StreamEnd{}, StreamStart{},
	}
}

// BuildSchema reflects every registered type into one JSON Schema document.
func BuildSchema() *Document {
	b := newBuilder()

	for _, v := range namedTypes() {
		b.add(v)
	}

	methods := make(map[string]MethodSchema, len(registry))
	for name, d := range registry {
		methods[name] = MethodSchema{
			Params:      b.add(d.Params),
			Result:      b.add(d.Result),
			StreamChunk: b.add(d.Chunk),
			StreamEnd:   b.add(d.End),
		}
	}

	notifications := make(map[string]NotificationSchema, len(notificationRegistry))
	for name, d := range notificationRegistry {
		notifications[name] = NotificationSchema{Params: b.add(d.Params)}
	}

	return &Document{
		Schema:          "https://json-schema.org/draft/2020-12/schema",
		ID:              "https://github.com/raskrebs/sonar/docs/schema/protocol.schema.json",
		Title:           "Sonar daemon protocol",
		Description:     "Generated from the Go types in internal/state and internal/daemon/rpc. Do not edit by hand; run `go generate ./...`.",
		ProtocolVersion: ProtocolVersion,
		Definitions:     b.defs,
		Methods:         methods,
		Notifications:   notifications,
	}
}

// Marshal returns the canonical bytes written to
// docs/schema/protocol.schema.json and printed by `sonar daemon schema`.
func Marshal() []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(BuildSchema()); err != nil {
		panic(fmt.Sprintf("rpc: marshalling protocol schema: %v", err))
	}
	return buf.Bytes()
}

// builder accumulates definitions across many reflections, rewriting invopop's
// "$defs" references to the "definitions" the contract asks for.
type builder struct {
	r    *jsonschema.Reflector
	defs map[string]json.RawMessage
}

func newBuilder() *builder {
	return &builder{
		// Additional properties are allowed so that additive protocol changes
		// (contract §7) and the deprecated flat fields `sonar list --json`
		// still carries do not fail validation.
		r:    &jsonschema.Reflector{AllowAdditionalProperties: true},
		defs: map[string]json.RawMessage{},
	}
}

// reflected is the subset of invopop's output the builder needs.
type reflected struct {
	Ref  string                     `json:"$ref"`
	Defs map[string]json.RawMessage `json:"$defs"`
}

// add reflects v, merges its definitions, and returns a $ref to it. It returns
// nil for a nil value so optional streaming fields stay absent.
func (b *builder) add(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(b.r.Reflect(v))
	if err != nil {
		panic(fmt.Sprintf("rpc: reflecting %T: %v", v, err))
	}
	raw = []byte(strings.ReplaceAll(string(raw), `#/$defs/`, `#/definitions/`))
	raw = refPattern.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := string(refPattern.FindSubmatch(m)[1])
		return []byte(`"#/definitions/` + sanitizeDefName(name) + `"`)
	})

	var out reflected
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("rpc: decoding reflected schema for %T: %v", v, err))
	}
	for name, def := range out.Defs {
		b.defs[sanitizeDefName(name)] = def
	}
	if out.Ref == "" {
		panic(fmt.Sprintf("rpc: %T did not reflect to a referenced definition", v))
	}
	return json.RawMessage(fmt.Sprintf("{%q:%q}", "$ref", out.Ref))
}

var refPattern = regexp.MustCompile(`"#/definitions/([^"]+)"`)

// sanitizeDefName turns Go's generic instantiation names into plain identifiers
// a TypeScript or Rust generator can use:
// "Change[github.com/raskrebs/sonar/internal/state.Port]" becomes "ChangePort".
func sanitizeDefName(name string) string {
	open := strings.IndexByte(name, '[')
	if open < 0 {
		return name
	}
	base := name[:open]
	args := strings.TrimSuffix(name[open+1:], "]")
	var b strings.Builder
	b.WriteString(base)
	for _, arg := range strings.Split(args, ",") {
		arg = strings.TrimSpace(arg)
		if i := strings.LastIndexByte(arg, '.'); i >= 0 {
			arg = arg[i+1:]
		}
		b.WriteString(sanitizeDefName(arg))
	}
	return b.String()
}

func init() {
	// Daemon lifecycle.
	Describe("daemon.hello", DaemonHelloParams{}, DaemonHelloResult{}, nil, nil)
	Describe("daemon.shutdown", Empty{}, OKResult{}, nil, nil)
	Describe("daemon.status", HostParams{}, DaemonStatusResult{}, nil, nil)
	Describe("daemon.schema", Empty{}, DaemonSchemaResult{}, nil, nil)
	Describe("daemon.doctor", DaemonDoctorParams{}, DaemonDoctorResult{}, nil, nil)

	// State.
	Describe("state.snapshot", StateSnapshotParams{}, state.Snapshot{}, nil, nil)
	Describe("state.subscribe", StateSubscribeParams{}, state.Snapshot{}, nil, nil)
	Describe("state.unsubscribe", Empty{}, OKResult{}, nil, nil)
	Describe("stream.cancel", StreamCancel{}, OKResult{}, nil, nil)

	// Ports.
	Describe("ports.list", PortsListParams{}, PortsListResult{}, nil, nil)
	Describe("ports.inspect", Selector{}, PortsInspectResult{}, nil, nil)
	Describe("ports.kill", PortsKillParams{}, KillEnvelope{}, nil, nil)
	Describe("ports.rename", PortsRenameParams{}, PortsRenameResult{}, nil, nil)
	Describe("ports.next", PortsNextParams{}, PortsNextResult{}, nil, nil)
	Describe("ports.wait", PortsWaitParams{}, StreamStart{}, PortsWaitChunk{}, PortsWaitEnd{})
	Describe("ports.health", PortsHealthParams{}, PortsHealthResult{}, nil, nil)
	Describe("ports.logs", PortsLogsParams{}, PortsLogsResult{}, PortsLogsChunk{}, StreamEnd{})
	Describe("ports.graph", HostParams{}, PortsGraphResult{}, nil, nil)
	Describe("ports.history", PortsHistoryParams{}, PortsHistoryResult{}, nil, nil)

	// Groups.
	Describe("groups.list", HostParams{}, GroupsListResult{}, nil, nil)
	Describe("groups.inspect", GroupsInspectParams{}, GroupsInspectResult{}, nil, nil)
	Describe("groups.kill", GroupsKillParams{}, KillEnvelope{}, nil, nil)
	Describe("groups.start", GroupsStartParams{}, GroupsStartResult{}, GroupsStartChunk{}, GroupsStartEnd{})
	Describe("groups.assign", GroupsAssignParams{}, GroupsAssignResult{}, nil, nil)
	Describe("groups.rename", GroupsRenameParams{}, GroupsRenameResult{}, nil, nil)
	Describe("groups.reload", HostParams{}, GroupsReloadResult{}, nil, nil)
	Describe("groups.config.get", GroupsConfigGetParams{}, GroupsConfigGetResult{}, nil, nil)
	Describe("groups.config.set", GroupsConfigSetParams{}, GroupsConfigSetResult{}, nil, nil)
	Describe("groups.init", GroupsInitParams{}, GroupsInitResult{}, nil, nil)

	// Runs.
	Describe("runs.register", RunsRegisterParams{}, RunsRegisterResult{}, nil, nil)
	Describe("runs.unregister", RunsUnregisterParams{}, OKResult{}, nil, nil)
	Describe("runs.list", HostParams{}, RunsListResult{}, nil, nil)
	Describe("runs.spawn", RunsSpawnParams{}, RunsSpawnResult{}, nil, nil)

	// Claims (spec 2, slice M5).
	Describe("claims.acquire", ClaimsAcquireParams{}, ClaimsAcquireResult{}, nil, nil)
	Describe("claims.release", ClaimsReleaseParams{}, ClaimsReleaseResult{}, nil, nil)
	Describe("claims.list", HostParams{}, ClaimsListResult{}, nil, nil)

	// Sessions (spec 2, slice M4).
	Describe("sessions.list", SessionsListParams{}, SessionsListResult{}, nil, nil)
	Describe("sessions.inspect", SessionsInspectParams{}, SessionsInspectResult{}, nil, nil)
	Describe("sessions.kill", SessionsKillParams{}, KillEnvelope{}, nil, nil)

	// Config.
	Describe("config.get", HostParams{}, ConfigGetResult{}, nil, nil)
	Describe("config.set", ConfigSetParams{}, ConfigSetResult{}, nil, nil)
	Describe("config.path", HostParams{}, ConfigPathResult{}, nil, nil)

	// Remote hosts.
	Describe("remote.scan", RemoteScanParams{}, RemoteScanResult{}, nil, nil)
	Describe("remote.install", RemoteInstallParams{}, RemoteInstallResult{},
		RemoteInstallChunk{}, RemoteInstallEnd{})
	Describe("remote.list", Empty{}, RemoteListResult{}, nil, nil)
	Describe("remote.add", RemoteAddParams{}, RemoteAddResult{}, nil, nil)
	Describe("remote.remove", RemoteRemoveParams{}, OKResult{}, nil, nil)
	Describe("remote.call", RemoteCallParams{}, RemoteCallResult{}, nil, nil)

	// Expose (spec 3).
	Describe("expose.create", ExposeCreateParams{}, ExposeCreateResult{}, nil, nil)
	Describe("expose.stop", ExposeStopParams{}, ExposeStopResult{}, nil, nil)
	Describe("expose.list", Empty{}, ExposeListResult{}, nil, nil)
	Describe("expose.logs", ExposeLogsParams{}, ExposeLogsResult{}, nil, nil)
	Describe("expose.providers", Empty{}, ExposeProvidersResult{}, nil, nil)
	Describe("expose.install_provider", ExposeInstallProviderParams{}, ExposeInstallProviderResult{},
		ExposeInstallProviderChunk{}, ExposeInstallProviderEnd{})

	// Daemon-owned proxies (spec 3).
	Describe("map.create", MapCreateParams{}, MapCreateResult{}, nil, nil)
	Describe("map.stop", MapStopParams{}, MapStopResult{}, nil, nil)
	Describe("map.list", Empty{}, MapListResult{}, nil, nil)
	Describe("map.requests", MapRequestsParams{}, MapRequestsResult{}, MapRequestsChunk{}, MapRequestsEnd{})

	// Notifications the daemon pushes (contract §1). state.subscribe keeps its
	// own broadcast names; every streaming method shares stream.chunk and
	// stream.end, whose `data` is the chunk or end type of the method that
	// opened the stream.
	DescribeNotification(MethodStateDelta, state.Delta{})
	DescribeNotification(MethodStateEvent, state.Event{})
	DescribeNotification(MethodStreamChunk, StreamChunk{})
	DescribeNotification(MethodStreamEnd, StreamEnd{})
}
