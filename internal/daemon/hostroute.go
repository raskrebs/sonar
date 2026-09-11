package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// Acting on a remote host (remote-hosts spec, "Actions on a remote host").
//
// Every method this daemon serves takes an optional `host`. Absent means this
// machine and nothing changes; a registered host name means the call is
// forwarded to that host's daemon over its bridge and its answer comes back in
// the same envelope. The handlers know nothing about it: routing happens once,
// in front of the dispatcher, which is why `host` works on a method the day it
// is registered rather than the day someone remembers to thread it through.
//
// The forwarded call is the caller's call minus what named the host: the `host`
// field is dropped and a `"<host>/…"` key is un-prefixed, so the far side reads
// params it would have received from a client on its own machine.

// remoteCancelTimeout bounds the stream.cancel a relay sends to the far side
// once its own client has gone. The relay is finished either way; this is only
// how long it is worth waiting to be polite about it.
const remoteCancelTimeout = 5 * time.Second

// Router is the seam internal/remote fills in. It is an interface here so this
// package never imports the bridge (contract §8) and so a test can route
// somewhere that is not SSH.
type Router interface {
	// Known reports whether name is a registered remote host.
	Known(name string) bool
	// Forward sends one method to a host's daemon. A non-nil stream means the
	// far side answered with a subscription id and the chunks are still
	// coming; the reply is returned either way.
	Forward(ctx context.Context, host, method string, params json.RawMessage) (json.RawMessage, RemoteStream, error)
}

// RemoteStream is a streaming method running on another host: its chunks, its
// single end, and the way to stop it early.
type RemoteStream interface {
	Chunks() <-chan json.RawMessage
	End() <-chan RemoteStreamEnd
	Cancel(ctx context.Context) error
}

// RemoteStreamEnd is the far side's stream.end: the method's final payload, or
// the error it failed with.
type RemoteStreamEnd struct {
	Data json.RawMessage
	Err  *rpc.Error
}

var (
	routerMu sync.RWMutex
	router   Router
)

// SetRouter installs the remote router. Passing nil removes it, which is what
// a daemon with no remote support looks like: `host` then only ever names
// localhost.
func SetRouter(r Router) {
	routerMu.Lock()
	router = r
	routerMu.Unlock()
}

// currentRouter returns the installed router, or nil.
func currentRouter() Router {
	routerMu.RLock()
	defer routerMu.RUnlock()
	return router
}

// knownHost reports whether name is a registered remote host right now.
func knownHost(name string) bool {
	r := currentRouter()
	return r != nil && r.Known(name)
}

// hostRoute is what one method's params and result look like from the routing
// layer: where a host may be named, and whether the keys coming back are port
// keys that need the host prefix a client's stream would have given them.
type hostRoute struct {
	// selector marks params that are, or embed, a Selector at the top level.
	selector bool
	// targets marks params carrying a `targets` array of Selectors.
	targets bool
	// nameFields are params fields whose value may carry a `"<host>/"` prefix,
	// because the client is handing back a group or session key.
	nameFields []string
	// portKeys marks a result whose `affected` entries are port keys, so a
	// forwarded answer namespaces them the way the stream does. (No result's
	// `key` field is a port key: `ports.rename` and `groups.assign` report the
	// store's own identity there, and `claims.*` a claim key.)
	portKeys bool
}

// hostRoutes is the per-method detail. A method that is not listed still takes
// a top-level `host`; the table only says where else a host can hide and what
// the result's keys mean.
var hostRoutes = map[string]hostRoute{
	"ports.inspect":     {selector: true},
	"ports.logs":        {selector: true},
	"ports.rename":      {selector: true, portKeys: true},
	"groups.assign":     {selector: true, portKeys: true},
	"ports.kill":        {targets: true, portKeys: true},
	"groups.kill":       {nameFields: []string{"name"}, portKeys: true},
	"groups.start":      {nameFields: []string{"name"}, portKeys: true},
	"groups.inspect":    {nameFields: []string{"name"}},
	"groups.config.get": {nameFields: []string{"name"}},
	"groups.rename":     {nameFields: []string{"name"}},
	"sessions.kill":     {nameFields: []string{"id"}, portKeys: true},
	"sessions.inspect":  {nameFields: []string{"id"}},
	"claims.acquire":    {portKeys: true},
	"runs.spawn":        {portKeys: true},
}

// unroutable reports the methods that never take a host.
//
// `state.*` has its own, richer host selection (`{hosts: [...]}`, §39) and is
// the one place a client sees several machines at once. `stream.cancel` is
// about a subscription this connection owns, and a relayed stream is cancelled
// through it, not around it. `daemon.hello` and `daemon.shutdown` are about the
// daemon the client is attached to. And `remote.*` is the bridge itself:
// forwarding it would chain hosts, which the design rules out.
func unroutable(method string) bool {
	switch {
	case strings.HasPrefix(method, "state."),
		strings.HasPrefix(method, "stream."),
		strings.HasPrefix(method, "remote."):
		return true
	case method == "daemon.hello", method == "daemon.shutdown":
		return true
	}
	return false
}

// routeRequest resolves and strips the host a request names. It returns the
// host ("" for this machine) and the params the callee should see, which for a
// local call are the caller's own params with any `key` selector expanded.
func routeRequest(method string, params json.RawMessage) (string, json.RawMessage, error) {
	if unroutable(method) {
		return "", params, nil
	}
	fields, ok := decodeObject(params)
	if !ok {
		return "", params, nil
	}

	route := hostRoutes[method]
	hosts := newHostSet()
	rewrote := false

	if raw, present := fields["host"]; present {
		name, err := decodeString(raw)
		if err != nil {
			return "", nil, rpc.NewError(rpc.CodeInvalidParams, "host must be a string", "")
		}
		if strings.TrimSpace(name) != "" {
			hosts.add(name)
		}
		delete(fields, "host")
		rewrote = true
	}

	if route.selector {
		changed, err := normalizeSelector(fields, hosts)
		if err != nil {
			return "", nil, err
		}
		rewrote = rewrote || changed
	}
	if route.targets {
		changed, err := normalizeTargets(fields, hosts)
		if err != nil {
			return "", nil, err
		}
		rewrote = rewrote || changed
	}
	for _, name := range route.nameFields {
		if stripNamePrefix(fields, name, hosts) {
			rewrote = true
		}
	}

	host, err := hosts.one()
	if err != nil {
		return "", nil, err
	}
	if state.IsLocalhost(host) {
		host = ""
	}
	if !rewrote {
		// Nothing named a host and nothing needed expanding, which is the
		// overwhelmingly common case: hand the handler the client's own bytes.
		return host, params, nil
	}

	out, err := json.Marshal(fields)
	if err != nil {
		return "", nil, rpc.NewError(rpc.CodeInternal, "rewriting params: "+err.Error(), "")
	}
	return host, out, nil
}

// hostSet collects the hosts one request names, so naming two is an error
// rather than a coin toss.
type hostSet struct {
	names []string
	seen  map[string]bool
}

func newHostSet() *hostSet { return &hostSet{seen: map[string]bool{}} }

func (h *hostSet) add(name string) {
	name = state.HostOf(strings.TrimSpace(name))
	if h.seen[name] {
		return
	}
	h.seen[name] = true
	h.names = append(h.names, name)
}

// one returns the single host the request named, or an error when it named
// more than one. A call acts on one machine: a kill that half succeeded on two
// hosts has no envelope that could describe it.
func (h *hostSet) one() (string, error) {
	switch len(h.names) {
	case 0:
		return "", nil
	case 1:
		return h.names[0], nil
	}
	sorted := append([]string(nil), h.names...)
	sort.Strings(sorted)
	return "", rpc.NewError(rpc.CodeInvalidParams,
		"one call acts on one host, but this one names "+strings.Join(sorted, " and "),
		"send one call per host")
}

// normalizeSelector reads the host out of a top-level selector and expands its
// `key` into port and bind_address.
func normalizeSelector(fields map[string]json.RawMessage, hosts *hostSet) (bool, error) {
	raw, present := fields["key"]
	if !present {
		return false, nil
	}
	delete(fields, "key")

	key, err := decodeString(raw)
	if err != nil {
		return true, rpc.NewError(rpc.CodeInvalidParams, "key must be a string", "")
	}
	if strings.TrimSpace(key) == "" {
		return true, nil
	}
	for _, field := range []string{"port", "pid", "run_id", "proxy_id"} {
		if _, dup := fields[field]; dup {
			return true, rpc.NewError(rpc.CodeInvalidSelector,
				"key cannot be combined with "+field,
				`a key is the whole selector: {"key": "3000:127.0.0.1"}`)
		}
	}

	host, rest := state.SplitHostPrefix(key, knownHost)
	if host != "" {
		hosts.add(host)
	}
	port, bind, ok := state.ParsePortKey(rest)
	if !ok {
		return true, rpc.NewError(rpc.CodeInvalidSelector,
			"cannot read "+key+" as a port key",
			`keys look like "3000", "3000:127.0.0.1" or "hetzner/3000:127.0.0.1"`)
	}
	fields["port"] = json.RawMessage(strconv.Itoa(port))
	if bind != "" {
		encoded, _ := json.Marshal(bind)
		fields["bind_address"] = encoded
	}
	return true, nil
}

// normalizeTargets does the same for every selector in a `targets` array.
func normalizeTargets(fields map[string]json.RawMessage, hosts *hostSet) (bool, error) {
	raw, present := fields["targets"]
	if !present {
		return false, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return false, nil // let the handler produce the shape error
	}
	rewrote := false
	for i, item := range list {
		sel, ok := decodeObject(item)
		if !ok {
			continue
		}
		if name, present := sel["host"]; present {
			value, err := decodeString(name)
			if err != nil {
				return true, rpc.NewError(rpc.CodeInvalidParams, "host must be a string", "")
			}
			if strings.TrimSpace(value) != "" {
				hosts.add(value)
			}
			delete(sel, "host")
			rewrote = true
		}
		changed, err := normalizeSelector(sel, hosts)
		if err != nil {
			return true, err
		}
		if !changed && !rewrote {
			continue
		}
		rewrote = true
		encoded, err := json.Marshal(sel)
		if err != nil {
			return true, rpc.NewError(rpc.CodeInternal, "rewriting a selector: "+err.Error(), "")
		}
		list[i] = encoded
	}
	if !rewrote {
		return false, nil
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return true, rpc.NewError(rpc.CodeInternal, "rewriting targets: "+err.Error(), "")
	}
	fields["targets"] = encoded
	return true, nil
}

// stripNamePrefix takes a `"<host>/"` off a group or session name.
func stripNamePrefix(fields map[string]json.RawMessage, field string, hosts *hostSet) bool {
	raw, present := fields[field]
	if !present {
		return false
	}
	value, err := decodeString(raw)
	if err != nil {
		return false // not a string: the handler's business, not ours
	}
	host, rest := state.SplitHostPrefix(value, knownHost)
	if host == "" {
		return false
	}
	hosts.add(host)
	encoded, _ := json.Marshal(rest)
	fields[field] = encoded
	return true
}

// serveRequest dispatches one request: to the local handler, or across a bridge
// when the params named a registered host.
func serveRequest(ctx context.Context, req *Request, h Handler) (any, error) {
	host, params, err := routeRequest(req.Method, req.Params)
	if err != nil {
		return nil, err
	}
	req.Params = params
	if host == "" {
		return h(ctx, req)
	}
	return ForwardTo(ctx, req, host, req.Method, params)
}

// ForwardTo sends one method to a registered host and answers this connection
// with what came back: the result verbatim for a plain call, and a relayed
// stream — local subscription id, chunks as they arrive, `stream.cancel`
// propagated — for a streaming one.
//
// The result is retagged on the way out: rows that call themselves "localhost"
// are that host's rows from here, and port keys are namespaced the way the
// state stream namespaces them, so a client can act on what it is handed
// without knowing which side produced it.
func ForwardTo(ctx context.Context, req *Request, host, method string, params json.RawMessage) (any, error) {
	r := currentRouter()
	if r == nil {
		return nil, rpc.NewError(rpc.CodeNotFound,
			"no remote host named "+host,
			"this daemon has no remote support built in")
	}
	if !r.Known(host) {
		// A typo must not look like an empty machine, and it must not look
		// like an internal error either.
		return nil, rpc.NewError(rpc.CodeNotFound,
			"no remote host named "+host,
			"`sonar remote list` shows the registered hosts; `sonar remote add <user@host>` adds one")
	}
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}

	reply, stream, err := r.Forward(ctx, host, method, params)
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return rpc.RemoteCallResult(retagResult(method, host, reply)), nil
	}
	return relayStream(ctx, req, host, method, reply, stream)
}

// relayStream replies with a local subscription id and pumps the far side's
// chunks onto this connection until it ends.
func relayStream(ctx context.Context, req *Request, host, method string, reply json.RawMessage, remote RemoteStream) (any, error) {
	initial := json.RawMessage(retagResult(method, host, reply))
	return StartStream(ctx, req, initial, func(ctx context.Context, s *Stream) (any, error) {
		// The local stream is cancelled by stream.cancel, by this connection
		// dropping and by shutdown. All three mean the same thing to the far
		// side, and it hears about it as its own stream.cancel.
		relayed := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remoteCancelTimeout)
				_ = remote.Cancel(cancelCtx)
				cancel()
			case <-relayed:
			}
		}()

		// Every chunk is read, whatever happens to the sending half: the far
		// side's end is only delivered once its chunks have been, so a relay
		// that stopped draining would never learn that the stream is over.
		// A cancelled stream, and one whose client has gone, simply stop
		// forwarding what they still read.
		stopped := false
		for chunk := range remote.Chunks() {
			if stopped || ctx.Err() != nil {
				continue
			}
			if err := s.Send(json.RawMessage(retagResult(method, host, chunk))); err != nil {
				stopped = true
			}
		}
		close(relayed)

		end := <-remote.End()
		if end.Err != nil {
			return nil, end.Err
		}
		if len(end.Data) == 0 {
			return nil, nil
		}
		return json.RawMessage(retagResult(method, host, end.Data)), nil
	})
}

// retagResult rewrites a forwarded payload so it describes the host it came
// from rather than the machine that produced it.
//
// Two rules, both mechanical. Every `host` field that says "localhost" (or says
// nothing) becomes this host's name, at any depth, because a row's `host` is
// the disambiguator the whole protocol leans on (remote-hosts spec, decision
// 1). And for the methods whose `affected` holds port keys, those gain the
// `"<host>/"` prefix the state stream would have given them, so the key in a
// reply and the key in the stream are the same string.
//
// It is a best-effort rewrite: a payload that cannot be parsed is passed
// through untouched rather than failing a call that already succeeded.
func retagResult(method, host string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || host == "" {
		return raw
	}
	value, err := decodeAny(raw)
	if err != nil {
		return raw
	}
	value = retagHosts(value, host)
	if hostRoutes[method].portKeys {
		if obj, ok := value.(map[string]any); ok {
			prefixPortKeys(obj, host)
		}
	}
	out, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return out
}

// retagHosts renames every localhost `host` field in a decoded payload.
func retagHosts(value any, host string) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "host" {
				if name, ok := item.(string); ok && state.IsLocalhost(name) {
					v[key] = host
					continue
				}
			}
			v[key] = retagHosts(item, host)
		}
		return v
	case []any:
		for i := range v {
			v[i] = retagHosts(v[i], host)
		}
		return v
	default:
		return value
	}
}

// prefixPortKeys namespaces the port keys in a mutation result.
func prefixPortKeys(obj map[string]any, host string) {
	list, ok := obj["affected"].([]any)
	if !ok {
		return
	}
	for i, item := range list {
		if key, ok := item.(string); ok && key != "" {
			list[i] = state.PrefixKey(host, key)
		}
	}
}

// decodeObject reads params as a JSON object, reporting whether they were one.
// Absent params are an empty object, which is how a method whose fields are all
// optional is called.
func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]json.RawMessage{}, true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	return fields, true
}

func decodeString(raw json.RawMessage) (string, error) {
	if string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

// decodeAny decodes a payload with numbers left as they were written, so a pid
// or a byte count survives the round trip through the retagger unchanged.
func decodeAny(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}
