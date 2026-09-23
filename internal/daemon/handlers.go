package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/state"
)

// The core method set for step 1A.1. Namespaces owned by later steps register
// their own handlers from their own init(), so this list only grows sideways.
func init() {
	RegisterHandler("daemon.hello", handleHello)
	RegisterHandler("daemon.status", handleStatus)
	RegisterHandler("daemon.shutdown", handleShutdown)
	RegisterHandler("daemon.schema", handleSchema)
	RegisterHandler("daemon.bridged", handleBridged)

	RegisterHandler("state.snapshot", handleSnapshot)
	RegisterHandler("state.subscribe", handleSubscribe)
	RegisterHandler("state.unsubscribe", handleUnsubscribe)

	RegisterHandler("stream.cancel", handleStreamCancel)
}

// parseInclude turns the wire's ["stats","health"] into the scanner's struct.
// An unknown entry is a client error rather than a silent no-op, so a typo in
// `include` is caught immediately.
func parseInclude(in rpc.Include) (scanner.Include, error) {
	var out scanner.Include
	for _, item := range in {
		switch strings.ToLower(strings.TrimSpace(string(item))) {
		case "":
			continue
		case "stats":
			out.Stats = true
		case "health":
			out.Health = true
		default:
			return out, rpc.NewError(rpc.CodeInvalidParams,
				"unknown include "+item, `include accepts "stats" and "health"`)
		}
	}
	return out, nil
}

// handleBridged is how `sonar daemon stdio` tells the daemon that everything
// arriving on this connection comes from another machine. It only ever removes
// what the connection may do, and it cannot be undone, so a remote calling it
// again is harmless and a remote never gets to call it first: the pump sends it
// before it copies a byte.
func handleBridged(_ context.Context, req *Request) (any, error) {
	req.Conn.MarkBridged()
	req.Runtime.Logger.Debug("connection marked as bridged", "conn", req.Conn.ID())
	return rpc.OKResult{OK: true}, nil
}

func handleHello(_ context.Context, req *Request) (any, error) {
	var p rpc.DaemonHelloParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	if p.Client == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "client is required",
			`send {"client": "cli", "client_version": "..."}`)
	}
	req.Conn.setHello(p.Client, p.ClientVersion, p.Keepalive)
	req.Runtime.Logger.Debug("hello", "conn", req.Conn.ID(),
		"client", p.Client, "client_version", p.ClientVersion, "keepalive", p.Keepalive)

	rt := req.Runtime
	return rpc.DaemonHelloResult{
		ProtocolVersion: rpc.ProtocolVersion,
		DaemonVersion:   rt.Version,
		PID:             rt.PID,
		StartedAt:       rt.StartedAt.Format(time.RFC3339),
		Capabilities:    Capabilities(),
		Socket:          rt.Socket,
		BinaryPath:      rt.BinaryPath,
		Keepalive:       p.Keepalive,
	}, nil
}

func handleStatus(_ context.Context, req *Request) (any, error) {
	rt := req.Runtime
	st := rt.Scanner.Status()
	lastScan := ""
	if !st.LastScanAt.IsZero() {
		lastScan = st.LastScanAt.Format(time.RFC3339)
	}
	return rpc.DaemonStatusResult{
		PID:                rt.PID,
		Uptime:             rt.Uptime().Round(time.Second).String(),
		Subscribers:        rt.Subscribers(),
		LastScanAt:         lastScan,
		ScanIntervalMs:     st.IntervalMs,
		ScanBaseIntervalMs: st.BaseIntervalMs,
		StatsIntervalMs:    st.StatsIntervalMs,
		ClaimTTLMs:         int(claimTTLInEffect(rt).Milliseconds()),
		Scans:              st.Scans,
		DBPath:             rt.DBPath(),
	}, nil
}

func handleShutdown(_ context.Context, req *Request) (any, error) {
	req.Runtime.Logger.Info("shutdown requested by client", "conn", req.Conn.ID())
	// The dispatcher stops the daemon once this reply is queued, so the client
	// gets its {ok: true} before the socket goes away.
	req.Conn.requestShutdown()
	return rpc.OKResult{OK: true}, nil
}

func handleSchema(_ context.Context, _ *Request) (any, error) {
	return rpc.DaemonSchemaResult{Schema: rpc.Marshal()}, nil
}

func handleSnapshot(_ context.Context, req *Request) (any, error) {
	var p rpc.StateSnapshotParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	include, err := parseInclude(p.Include)
	if err != nil {
		return nil, err
	}
	snap, err := req.Runtime.Scanner.SnapshotAll(include)
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternal, "scan failed: "+err.Error(),
			"check `sonar daemon log` for the scanner error")
	}
	return filterSnapshot(snap, include, state.ParseHostFilter(p.Hosts)), nil
}

func handleSubscribe(_ context.Context, req *Request) (any, error) {
	var p rpc.StateSubscribeParams
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	include, err := parseInclude(p.Include)
	if err != nil {
		return nil, err
	}

	// No scan here. subscribe replies from the cached snapshot and wakes the
	// loop, so a subscriber that asked for stats or health is answered at once
	// and gets those fields in the delta the woken tick publishes. Scanning
	// first would hold the reply for the length of a stats collection — seconds
	// on a busy machine — and the first snapshot would still be a delta behind
	// by the time it arrived.
	req.Runtime.Server().subscribe(req.Conn, req.ID, include, p.Events, state.ParseHostFilter(p.Hosts))
	return nil, ErrResponseSent
}

func handleUnsubscribe(_ context.Context, req *Request) (any, error) {
	req.Runtime.Server().unsubscribe(req.Conn)
	return rpc.OKResult{OK: true}, nil
}

func handleStreamCancel(_ context.Context, req *Request) (any, error) {
	var p rpc.StreamCancel
	if err := req.Bind(&p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "id is required", "")
	}
	if !req.Conn.CancelStream(p.ID) {
		return nil, rpc.NewError(rpc.CodeNotFound,
			"no stream "+p.ID+" on this connection",
			"streams end on their own with stream.end; cancelling twice is not an error worth retrying")
	}
	return rpc.OKResult{OK: true}, nil
}
