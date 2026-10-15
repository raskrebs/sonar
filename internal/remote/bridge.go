package remote

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// bridge is one registered host: the connect/subscribe/reconnect goroutine,
// the rows that host currently contributes, and the Host row describing the
// connection itself.
//
// Everything a reader needs is behind mu and is a value copy, so the manager
// can assemble the published rows without ever waiting on a network.
type bridge struct {
	cfg Host
	// version is what this daemon calls itself in daemon.hello on the far
	// side. Immutable for the life of the bridge.
	version string
	logger  *slog.Logger
	dial    Dialer
	// onChange is called whenever this host's rows or status changed. The
	// manager forwards it to the scanner, which republishes.
	onChange func()
	// onEvent forwards one of the remote daemon's events into the local
	// stream, already tagged with this host's name.
	onEvent func(state.Event)
	// now and after are the clock seams the reconnect tests drive.
	now func() time.Time

	cancel context.CancelFunc
	done   chan struct{}

	mu sync.RWMutex
	// snap is the remote daemon's own state, as it describes itself: rows
	// tagged "localhost", keys unprefixed. Tagging happens on the way out, in
	// Rows, so the delta arithmetic on this side stays the remote's own.
	snap    state.Snapshot
	haveSub bool
	status  state.Host
	// cli is the live client, used to forward remote.call. Nil while
	// disconnected.
	cli *client.Client
}

func newBridge(cfg Host, version string, dial Dialer, logger *slog.Logger, onChange func(), onEvent func(state.Event)) *bridge {
	b := &bridge{
		cfg:      cfg,
		version:  version,
		logger:   logger,
		dial:     dial,
		onChange: onChange,
		onEvent:  onEvent,
		now:      time.Now,
		done:     make(chan struct{}),
	}
	b.status = state.Host{
		Name:     cfg.Name,
		Address:  cfg.Target,
		Status:   state.HostConnecting,
		LastSeen: b.now().Format(time.RFC3339),
	}
	return b
}

// start runs the connect loop until stop is called.
func (b *bridge) start(ctx context.Context) {
	ctx, b.cancel = context.WithCancel(ctx)
	go func() {
		defer close(b.done)
		b.run(ctx)
	}()
}

// stop tears the bridge down and waits for its goroutine.
func (b *bridge) stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.closeClient()
	<-b.done
}

// Rows is this host's contribution to the published state: its ports, groups
// and sessions tagged with the host name, plus exactly one Host row.
//
// A disconnected host contributes no ports and no groups — showing the last
// state of a machine we can no longer see would be a lie clients cannot detect
// — but it always contributes its Host row, so a host stays in the switcher
// with its status while it is unreachable.
func (b *bridge) Rows() state.Rows {
	b.mu.RLock()
	defer b.mu.RUnlock()

	host := b.status
	if !b.haveSub {
		return state.Rows{Hosts: []state.Host{host}}
	}
	rows := state.Rows{
		Ports:    b.snap.Ports,
		Groups:   b.snap.Groups,
		Shares:   b.snap.Shares,
		Proxies:  b.snap.Proxies,
		Sessions: b.snap.Sessions,
	}.Tag(b.cfg.Name)
	host.Ports, host.Groups = len(rows.Ports), len(rows.Groups)
	rows.Hosts = []state.Host{host}
	return rows
}

// HostRow is the connection's own status row.
func (b *bridge) HostRow() state.Host {
	b.mu.RLock()
	defer b.mu.RUnlock()
	h := b.status
	if b.haveSub {
		h.Ports, h.Groups = len(b.snap.Ports), len(b.snap.Groups)
	}
	return h
}

// Config is the host as it is registered.
func (b *bridge) Config() Host {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cfg
}

// Call forwards one method to this host's daemon and returns the result
// verbatim. It fails fast while the bridge is down rather than queueing: a
// caller wants to know that the host is unreachable, not to block on it.
func (b *bridge) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	cli, err := b.client()
	if err != nil {
		return nil, err
	}
	var out json.RawMessage
	if err := cli.Call(ctx, method, params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// client is the live connection, or the error a caller should see instead of
// one: which state the host is in, and what it said last.
func (b *bridge) client() (*client.Client, error) {
	b.mu.RLock()
	cli := b.cli
	status, reason := b.status.Status, b.status.StatusReason
	b.mu.RUnlock()

	if cli == nil {
		detail := "host " + b.cfg.Name + " is " + status
		if reason != nil && *reason != "" {
			detail += ": " + *reason
		}
		return nil, rpc.NewError(rpc.CodeNotFound, detail,
			"`sonar remote list` shows the connection; `sonar remote install "+b.cfg.Name+"` puts a daemon on it, and `remote_bin` names a sonar that is somewhere else")
	}
	return cli, nil
}

// run is the connect / subscribe / reconnect loop. It never gives up: a host
// the user registered stays registered until they remove it, and a laptop that
// is closed for the night reconnects when it opens.
func (b *bridge) run(ctx context.Context) {
	backoff := ReconnectMin
	for {
		if ctx.Err() != nil {
			return
		}
		b.setStatus(state.HostConnecting, "")

		err := b.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// A clean end of stream is still a disconnect; retry promptly,
			// because a bridge that ended cleanly is one whose remote daemon
			// idled out or was restarted.
			backoff = ReconnectMin
		} else {
			b.logger.Debug("remote host disconnected",
				"host", b.cfg.Name, "error", err, "retry_in", backoff.String())
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff)
	}
}

// nextBackoff doubles the delay, capped at ReconnectMax.
func nextBackoff(d time.Duration) time.Duration {
	next := d * ReconnectFactor
	if next > ReconnectMax {
		return ReconnectMax
	}
	if next < ReconnectMin {
		return ReconnectMin
	}
	return next
}

// session is one connection's whole life: dial, handshake, subscribe, follow
// the stream. It returns when the bridge is gone.
//
// A session that got as far as subscribing resets the caller's backoff, so a
// host that works and then drops retries in a second rather than in whatever
// the last failure had backed off to.
func (b *bridge) session(ctx context.Context) error {
	stream, err := b.dial(ctx, b.cfg)
	if err != nil {
		b.fail(state.HostUnreachable, err)
		return err
	}

	helloCtx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	started := b.now()
	cli, err := client.Attach(helloCtx, stream, "ssh://"+b.cfg.Target, client.ClientInfo{
		Name:      "daemon",
		Version:   b.localVersion(),
		Keepalive: true,
	})
	cancel()
	if err != nil {
		_ = stream.Close()
		var mismatch *client.ProtocolMismatchError
		if errors.As(err, &mismatch) {
			b.fail(state.HostIncompatible, err)
		} else {
			b.fail(state.HostUnreachable, err)
		}
		return err
	}
	latency := b.now().Sub(started)
	defer func() {
		cli.Close()
		b.closeClient()
	}()

	hello := cli.Hello()
	b.mu.Lock()
	b.cli = cli
	b.status.DaemonVersion = hello.DaemonVersion
	b.status.ProtocolVersion = hello.ProtocolVersion
	b.mu.Unlock()

	sub, err := cli.Subscribe(ctx, client.SubscribeOptions{Events: true, Buffer: 256})
	if err != nil {
		b.fail(state.HostUnreachable, err)
		return err
	}

	b.mu.Lock()
	b.snap = sub.Snapshot
	b.haveSub = true
	b.status.Status = state.HostConnected
	b.status.StatusReason = nil
	b.status.LatencyMs = latency.Milliseconds()
	b.adoptRemoteHostRow(sub.Snapshot)
	b.mu.Unlock()
	b.logger.Info("remote host connected",
		"host", b.cfg.Name, "target", b.cfg.Target,
		"daemon_version", hello.DaemonVersion, "latency_ms", latency.Milliseconds())
	b.onChange()

	return b.follow(ctx, cli, sub)
}

// follow applies deltas until the bridge drops. Every applied delta publishes,
// which is what puts a remote port on a local client's screen.
func (b *bridge) follow(ctx context.Context, cli *client.Client, sub *client.Subscription) error {
	ping := time.NewTicker(PingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case d, ok := <-sub.Deltas:
			if !ok {
				b.drop(cli.Err())
				return cli.Err()
			}
			b.mu.Lock()
			gap := b.snap.Seq != 0 && d.Seq > b.snap.Seq+1
			b.snap = state.Apply(b.snap, d)
			b.status.LastSeen = b.now().Format(time.RFC3339)
			b.adoptRemoteHostRow(b.snap)
			b.mu.Unlock()
			if gap || sub.Dropped() {
				// A seq gap is the documented resync trigger (contract §15),
				// and a dropped notification is a gap we caused ourselves.
				// Re-establishing the session is the cheapest correct resync.
				b.logger.Debug("remote host resyncing", "host", b.cfg.Name, "seq", d.Seq)
				b.drop(errors.New("delta sequence gap"))
				return nil
			}
			b.onChange()

		case ev, ok := <-sub.Events:
			if !ok {
				b.drop(cli.Err())
				return cli.Err()
			}
			ev.Host = b.cfg.Name
			if ev.Port != nil {
				p := *ev.Port
				p.Host = b.cfg.Name
				ev.Port = &p
			}
			b.onEvent(ev)

		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(ctx, PingTimeout)
			started := b.now()
			err := cli.Call(pingCtx, "daemon.status", rpc.Empty{}, nil)
			cancel()
			if err != nil {
				b.drop(err)
				return err
			}
			b.mu.Lock()
			b.status.LatencyMs = b.now().Sub(started).Milliseconds()
			b.status.LastSeen = b.now().Format(time.RFC3339)
			b.mu.Unlock()

		case <-cli.Done():
			b.drop(cli.Err())
			return cli.Err()
		}
	}
}

// adoptRemoteHostRow copies the remote's own localhost row — its cpu, load,
// memory, disk, os and uptime — onto this host's status row, keeping the
// fields that describe the *connection* rather than the machine. Caller holds
// the write lock.
func (b *bridge) adoptRemoteHostRow(snap state.Snapshot) {
	for _, h := range snap.Hosts {
		if !state.IsLocalhost(h.Name) {
			continue
		}
		status, reason, latency := b.status.Status, b.status.StatusReason, b.status.LatencyMs
		daemonVersion, protocolVersion := b.status.DaemonVersion, b.status.ProtocolVersion
		lastSeen := b.status.LastSeen

		b.status = h
		b.status.Name, b.status.Address = b.cfg.Name, b.cfg.Target
		b.status.Status, b.status.StatusReason, b.status.LatencyMs = status, reason, latency
		if daemonVersion != "" {
			b.status.DaemonVersion = daemonVersion
		}
		if protocolVersion != "" {
			b.status.ProtocolVersion = protocolVersion
		}
		if lastSeen != "" {
			b.status.LastSeen = lastSeen
		}
		return
	}
}

// drop marks the bridge unreachable and forgets the rows it was publishing.
func (b *bridge) drop(err error) {
	b.mu.Lock()
	b.haveSub = false
	b.snap = state.Snapshot{}
	b.cli = nil
	b.status = state.Host{
		Name:            b.cfg.Name,
		Address:         b.cfg.Target,
		Status:          state.HostUnreachable,
		StatusReason:    reasonOf(err),
		DaemonVersion:   b.status.DaemonVersion,
		ProtocolVersion: b.status.ProtocolVersion,
		LastSeen:        b.status.LastSeen,
	}
	b.mu.Unlock()
	b.onChange()
}

// fail records a connection that never came up.
func (b *bridge) fail(status string, err error) {
	b.mu.Lock()
	b.haveSub = false
	b.snap = state.Snapshot{}
	b.cli = nil
	b.status.Name, b.status.Address = b.cfg.Name, b.cfg.Target
	b.status.Status, b.status.StatusReason = status, reasonOf(err)
	b.status.Ports, b.status.Groups = 0, 0
	b.mu.Unlock()
	b.onChange()
}

// setStatus updates the status word without touching the load fields.
func (b *bridge) setStatus(status, reason string) {
	b.mu.Lock()
	changed := b.status.Status != status
	b.status.Status = status
	if reason != "" {
		b.status.StatusReason = &reason
	}
	b.mu.Unlock()
	if changed {
		b.onChange()
	}
}

func (b *bridge) closeClient() {
	b.mu.Lock()
	cli := b.cli
	b.cli = nil
	b.mu.Unlock()
	if cli != nil {
		cli.Close()
	}
}

// localVersion is the version this daemon reports to the remote in
// daemon.hello.
func (b *bridge) localVersion() string { return b.version }

func reasonOf(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	return &s
}
