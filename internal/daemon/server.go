package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/state"
)

// DefaultIdleTimeout is the config default for daemon.idle_timeout. Zero means
// "never idle out".
const DefaultIdleTimeout = 30 * time.Minute

// idleCheckInterval bounds how often the idle watchdog wakes.
const idleCheckInterval = 5 * time.Second

// Capabilities lists the method families this build serves. Clients read it
// from daemon.hello to tell whether, say, expose.* exists before calling it.
// Later steps append their own family as they land.
func Capabilities() []string {
	return append([]string{"state", "ports.read", "ports.kill", "store", "streams"},
		registeredCapabilities()...)
}

// Options configures a Server.
type Options struct {
	// Socket is the address to listen on. Empty means SocketPath().
	Socket string
	// Version is the daemon's own version string, reported by daemon.hello.
	Version string
	// BinaryPath is the executable clients should re-exec to autostart us.
	BinaryPath string
	// IdleTimeout stops the daemon after this long with no clients, no
	// subscribers and no keepalive. Zero disables it.
	IdleTimeout time.Duration
	// StatsInterval is the cadence of the scanner's stats-only tick
	// (`daemon.stats_interval`). Zero means scanner.StatsInterval. Ignored
	// when Scanner is set, since the caller built the loop itself.
	StatsInterval time.Duration
	// ScanInterval is the base cadence of the scanner's port scan
	// (`daemon.scan_interval`). Zero means scanner.BaseInterval. Ignored when
	// Scanner is set, for the same reason StatsInterval is.
	ScanInterval time.Duration
	// Logger receives the daemon's structured log. Required in production;
	// tests may leave it nil for a discard logger.
	Logger *slog.Logger
	// Lock is an already-acquired single-instance lock. When nil, Serve takes
	// one itself and releases it on exit.
	Lock *Lock
	// Scanner overrides the scan loop. Tests inject one; production leaves it
	// nil and gets the OS scanner.
	Scanner *scanner.Loop
	// DBPath is the SQLite database to open. Empty means store.Path(); tests
	// point it at a temp directory.
	DBPath string
}

// Server is the daemon: a listener, a set of connections, the subscription
// fan-out and the process lifecycle.
type Server struct {
	opts    Options
	logger  *slog.Logger
	socket  string
	ln      net.Listener
	runtime *Runtime
	loop    *scanner.Loop

	lock    *Lock
	ownLock bool

	nextConnID atomic.Uint64
	keepalives atomic.Int64
	lastActive atomic.Int64 // unix nanos

	subsMu sync.RWMutex
	conns  map[uint64]*Conn

	pendingMu sync.Mutex
	pending   []state.Event

	stopOnce sync.Once
	stopping chan struct{}
	done     chan struct{}
	graceful atomic.Bool
	wg       sync.WaitGroup
}

// New builds a Server. It does not listen; call Serve.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Socket == "" {
		opts.Socket = SocketPath()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	s := &Server{
		opts:     opts,
		logger:   opts.Logger,
		socket:   opts.Socket,
		conns:    map[uint64]*Conn{},
		stopping: make(chan struct{}),
		done:     make(chan struct{}),
	}
	s.touch()

	s.loop = opts.Scanner
	if s.loop == nil {
		s.loop = scanner.New(scanner.Options{
			DaemonVersion:   opts.Version,
			ProtocolVersion: rpc.ProtocolVersion,
			Logger:          opts.Logger,
			StatsInterval:   opts.StatsInterval,
			ScanInterval:    opts.ScanInterval,
		})
	}
	s.loop.SetDemand(s.demand)
	s.loop.SetPublisher(s.publish)

	binary := opts.BinaryPath
	if binary == "" {
		binary, _ = os.Executable()
	}
	s.runtime = &Runtime{
		Version:    opts.Version,
		Socket:     s.socket,
		BinaryPath: binary,
		PID:        os.Getpid(),
		StartedAt:  time.Now(),
		Logger:     opts.Logger,
		Scanner:    s.loop,
		srv:        s,
	}
	s.loop.SetRuns(s.runtime.RunRegistry)
	return s
}

// Runtime is the runtime handed to handlers and extension hooks.
func (s *Server) Runtime() *Runtime { return s.runtime }

// Socket is the address the server listens on.
func (s *Server) Socket() string { return s.socket }

// Serve takes the single-instance lock, listens, and accepts connections until
// ctx is cancelled or Shutdown is called. It returns ErrAlreadyRunning when
// another daemon holds the lock.
func (s *Server) Serve(ctx context.Context) error {
	defer close(s.done)

	if s.opts.Lock != nil {
		s.lock = s.opts.Lock
	} else {
		lock, err := AcquireLock(lockPathFor(s.socket))
		if err != nil {
			return err
		}
		s.lock, s.ownLock = lock, true
	}
	defer func() {
		if s.ownLock {
			_ = s.lock.Release()
		}
	}()

	// The lock is ours, so any socket file still on disk belongs to a daemon
	// that died without cleaning up.
	if err := removeStaleSocket(s.socket); err != nil {
		return fmt.Errorf("removing stale socket %s: %w", s.socket, err)
	}

	ln, err := listen(s.socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.socket, err)
	}
	s.ln = ln

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The store comes up before the scanner so the first tick already resolves
	// groups with the stored pins and publishes the stored names.
	s.openStore()
	defer s.closeStore()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop.Run(ctx)
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.watchIdle(ctx)
	}()

	runStartHooks(s.runtime)
	s.logger.Info("daemon listening",
		"socket", s.socket, "pid", s.runtime.PID, "version", s.opts.Version,
		"idle_timeout", s.opts.IdleTimeout.String())

	go func() {
		select {
		case <-ctx.Done():
			s.Shutdown()
		case <-s.stopping:
		}
	}()

	acceptErr := s.accept(ctx)

	cancel()
	s.closeAllConns("daemon stopping")
	s.wg.Wait()
	runShutdownHooks(s.graceful.Load())
	_ = removeStaleSocket(s.socket)
	s.logger.Info("daemon stopped", "graceful", s.graceful.Load())
	return acceptErr
}

// accept is the accept loop. A failure after Shutdown has been requested is
// the listener closing under us, not an error.
func (s *Server) accept(ctx context.Context) error {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stopping:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.startConn(ctx, nc)
	}
}

// startConn registers a connection and starts its read and write loops.
func (s *Server) startConn(ctx context.Context, nc net.Conn) {
	c := newConn(s.nextConnID.Add(1), s, nc)

	s.subsMu.Lock()
	s.conns[c.id] = c
	s.subsMu.Unlock()
	s.touch()
	s.logger.Debug("client connected", "conn", c.id)

	c.writerWG.Add(1)
	go c.writeLoop()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer c.Close()
		c.readLoop(ctx)
	}()
}

// removeConn drops a connection from the registry. Called once per connection,
// from Conn.closeWithReason.
func (s *Server) removeConn(c *Conn, reason string) {
	s.subsMu.Lock()
	delete(s.conns, c.id)
	s.subsMu.Unlock()
	s.recountKeepalive()
	s.touch()
	s.logger.Debug("client disconnected", "conn", c.id, "reason", reason)
	s.loop.Wake()
}

// closeAllConns disconnects every client.
func (s *Server) closeAllConns(reason string) {
	s.subsMu.RLock()
	conns := make([]*Conn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.subsMu.RUnlock()
	for _, c := range conns {
		c.closeWithReason(reason)
	}
}

// Shutdown stops the daemon. It broadcasts state.event{daemon_stopping} to
// every subscriber first (spec, daemon.shutdown), gives the writers a moment to
// flush, and then closes the listener.
func (s *Server) Shutdown() {
	s.stopOnce.Do(func() {
		s.graceful.Store(true)
		s.broadcastEvent(state.Event{
			Kind: "daemon_stopping",
			At:   time.Now().Format(time.RFC3339),
		})
		s.drainQueues(500 * time.Millisecond)
		close(s.stopping)
		if s.ln != nil {
			_ = s.ln.Close()
		}
	})
}

// Done is closed once Serve has returned.
func (s *Server) Done() <-chan struct{} { return s.done }

// drainQueues waits, up to timeout, for every connection's outbound queue to
// empty, so a client sees daemon_stopping (and the reply to daemon.shutdown)
// before the socket goes away.
func (s *Server) drainQueues(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.subsMu.RLock()
		pending := 0
		for _, c := range s.conns {
			pending += len(c.out)
		}
		s.subsMu.RUnlock()
		if pending == 0 {
			// One more tick so the writer flushes what it has already taken.
			time.Sleep(20 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ----------------------------------------------------------- subscriptions ---

// Subscribers counts the connections currently subscribed to state.
func (s *Server) Subscribers() int {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	n := 0
	for _, c := range s.conns {
		if c.subscribed {
			n++
		}
	}
	return n
}

// Clients counts open connections, subscribed or not.
func (s *Server) Clients() int {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	return len(s.conns)
}

// demand is the scanner's Demand callback: how many subscribers there are and
// the union of what they asked to have collected.
func (s *Server) demand() (int, scanner.Include) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	var inc scanner.Include
	n := 0
	for _, c := range s.conns {
		if !c.subscribed {
			continue
		}
		n++
		inc.Stats = inc.Stats || c.include.Stats
		inc.Health = inc.Health || c.include.Health
	}
	return n, inc
}

// subscribe registers a connection as a subscriber and hands it the cached
// snapshot as the reply to its own state.subscribe call. Both happen under the
// publish lock, so no delta can slip in between the two.
//
// The reply never waits for a scan: the snapshot is whatever the loop last
// published — with stats and health null when the loop has not been collecting
// them — and Wake makes the loop scan immediately, so the fields the subscriber
// asked for arrive in the first delta rather than delaying the reply by a scan.
// The delta's seq is the cached snapshot's seq plus one, so the §15 resync rule
// is unchanged.
func (s *Server) subscribe(c *Conn, id json.RawMessage, include scanner.Include, events bool, hosts state.HostFilter) {
	s.subsMu.Lock()
	snap := s.loop.CachedAll()
	c.subscribed, c.include, c.events, c.hosts = true, include, events, hosts
	filtered := filterSnapshot(snap, include, hosts)
	raw, err := json.Marshal(filtered)
	if err == nil {
		msg, mErr := json.Marshal(rpc.Response{JSONRPC: rpc.Version, ID: id, Result: raw})
		if mErr == nil {
			c.enqueue(msg)
		}
	}
	s.subsMu.Unlock()

	s.touch()
	s.loop.Wake()
	if err != nil {
		c.replyError(id, rpc.NewError(rpc.CodeInternal, "marshalling snapshot: "+err.Error(), ""))
	}
}

// unsubscribe drops a connection's subscription.
func (s *Server) unsubscribe(c *Conn) {
	s.subsMu.Lock()
	c.subscribed, c.include, c.events, c.hosts = false, scanner.Include{}, false, state.HostFilter{}
	s.subsMu.Unlock()
	s.touch()
	s.loop.Wake()
}

// publish fans a scanner transition out to every subscriber. The delta is
// marshalled once per distinct include set, not once per subscriber.
func (s *Server) publish(prev, next state.Snapshot, events []state.Event) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	// Events raised before anyone was listening ride out with the first delta
	// that has somewhere to go.
	if s.listeningForEvents() {
		events = append(s.takePending(), events...)
	}

	deltaCache := map[viewKey][]byte{}
	eventCache := map[viewKey][][]byte{}

	for _, c := range s.conns {
		if !c.subscribed {
			continue
		}
		view := viewKey{include: c.include, hosts: c.hosts.Key()}
		msg, ok := deltaCache[view]
		if !ok {
			msg = marshalDelta(prev, next, c.include, c.hosts)
			deltaCache[view] = msg
		}
		if msg != nil {
			c.enqueue(msg)
		}
		if !c.events || len(events) == 0 {
			continue
		}
		msgs, ok := eventCache[view]
		if !ok {
			msgs = marshalEvents(events, c.include, c.hosts)
			eventCache[view] = msgs
		}
		for _, m := range msgs {
			c.enqueue(m)
		}
	}
}

// viewKey identifies one subscriber's view of the stream: what it opted into
// collecting and which hosts it asked to see. The delta is marshalled once per
// distinct view rather than once per subscriber.
type viewKey struct {
	include scanner.Include
	hosts   string
}

// listeningForEvents reports whether any subscriber asked for events. Caller
// holds subsMu.
func (s *Server) listeningForEvents() bool {
	for _, c := range s.conns {
		if c.subscribed && c.events {
			return true
		}
	}
	return false
}

// BroadcastEvent sends one event to every subscriber that asked for events and
// can see the host it happened on. The remote-host bridge uses it to forward a
// remote daemon's events into the local stream.
func (s *Server) BroadcastEvent(ev state.Event) { s.broadcastEvent(ev) }

// broadcastEvent sends one event to every subscriber that asked for events.
func (s *Server) broadcastEvent(ev state.Event) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	cache := map[viewKey][][]byte{}
	for _, c := range s.conns {
		if !c.subscribed || !c.events {
			continue
		}
		view := viewKey{include: c.include, hosts: c.hosts.Key()}
		msgs, ok := cache[view]
		if !ok {
			msgs = marshalEvents([]state.Event{ev}, c.include, c.hosts)
			cache[view] = msgs
		}
		for _, m := range msgs {
			c.enqueue(m)
		}
	}
}

// marshalDelta builds one state.delta notification for a given include set.
// It returns nil when the delta is empty for this subscriber, so a client that
// asked for neither stats nor health is not woken by a stats-only tick.
func marshalDelta(prev, next state.Snapshot, include scanner.Include, hosts state.HostFilter) []byte {
	prev = state.FilterSnapshot(prev, hosts)
	next = state.FilterSnapshot(next, hosts)
	var d state.Delta
	if include.Stats {
		d = state.DiffWithStats(prev, next)
	} else {
		d = state.Diff(prev, next)
	}
	if emptyDelta(d) {
		return nil
	}
	d.Ports.Added = filterPorts(d.Ports.Added, include)
	d.Ports.Updated = filterPorts(d.Ports.Updated, include)

	raw, err := json.Marshal(d)
	if err != nil {
		return nil
	}
	msg, err := json.Marshal(rpc.Notification{
		JSONRPC: rpc.Version, Method: rpc.MethodStateDelta, Params: raw,
	})
	if err != nil {
		return nil
	}
	return msg
}

// marshalEvents builds one state.event notification per event, skipping the
// ones that happened on a host this subscriber did not ask to see.
func marshalEvents(events []state.Event, include scanner.Include, hosts state.HostFilter) [][]byte {
	out := make([][]byte, 0, len(events))
	for _, ev := range events {
		if !hosts.Allows(ev.Host) {
			continue
		}
		if ev.Port != nil {
			p := filterPort(*ev.Port, include)
			ev.Port = &p
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		msg, err := json.Marshal(rpc.Notification{
			JSONRPC: rpc.Version, Method: rpc.MethodStateEvent, Params: raw,
		})
		if err != nil {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func emptyDelta(d state.Delta) bool {
	return len(d.Ports.Added) == 0 && len(d.Ports.Updated) == 0 && len(d.Ports.Removed) == 0 &&
		len(d.Groups.Added) == 0 && len(d.Groups.Updated) == 0 && len(d.Groups.Removed) == 0 &&
		len(d.Tunnels.Added) == 0 && len(d.Tunnels.Updated) == 0 && len(d.Tunnels.Removed) == 0 &&
		len(d.Proxies.Added) == 0 && len(d.Proxies.Updated) == 0 && len(d.Proxies.Removed) == 0 &&
		len(d.Sessions.Added) == 0 && len(d.Sessions.Updated) == 0 && len(d.Sessions.Removed) == 0 &&
		len(d.Hosts.Added) == 0 && len(d.Hosts.Updated) == 0 && len(d.Hosts.Removed) == 0
}

// filterSnapshot strips the enrichments this subscriber did not ask for, so
// `include` is honest even when another subscriber made the scanner collect
// them.
func filterSnapshot(snap state.Snapshot, include scanner.Include, hosts state.HostFilter) state.Snapshot {
	snap = state.FilterSnapshot(snap, hosts)
	snap.Ports = filterPorts(snap.Ports, include)
	if snap.Groups == nil {
		snap.Groups = []state.Group{}
	}
	if snap.Tunnels == nil {
		snap.Tunnels = []state.Tunnel{}
	}
	if snap.Proxies == nil {
		snap.Proxies = []state.Proxy{}
	}
	if snap.Sessions == nil {
		snap.Sessions = []state.SessionRecord{}
	}
	// `hosts` is never filtered by `include`. A machine's load is state the
	// daemon collects for itself on every tick, not an enrichment a
	// subscriber opts into, so it reaches every subscriber whatever their
	// `include` — the rule contract §22 set for configured health. The
	// `hosts` *filter* above is a different question: it decides which
	// machines this subscriber asked about at all.
	if snap.Hosts == nil {
		snap.Hosts = []state.Host{}
	}
	return snap
}

func filterPorts(pp []state.Port, include scanner.Include) []state.Port {
	if include.Stats && include.Health {
		return pp
	}
	out := make([]state.Port, len(pp))
	for i := range pp {
		out[i] = filterPort(pp[i], include)
	}
	return out
}

// filterPort strips what this subscriber did not ask for. Health a
// `sonar.yaml` asked the daemon to poll is the exception: the config declared
// it, so it is state every subscriber sees whether or not it asked for
// `include: ["health"]` (step 1A.7).
func filterPort(p state.Port, include scanner.Include) state.Port {
	if !include.Stats {
		p.Stats = nil
	}
	if !include.Health && p.Health != nil && !p.Health.Configured {
		p.Health = nil
	}
	return p
}

// ------------------------------------------------------------------- idle ---

// touch records client activity, resetting the idle countdown.
func (s *Server) touch() { s.lastActive.Store(time.Now().UnixNano()) }

// recountKeepalive recomputes how many connected clients asked the daemon to
// stay up (daemon.hello{keepalive:true}).
func (s *Server) recountKeepalive() {
	s.subsMu.RLock()
	n := int64(0)
	for _, c := range s.conns {
		if c.Keepalive() {
			n++
		}
	}
	s.subsMu.RUnlock()
	s.keepalives.Store(n)
}

// watchIdle stops the daemon once it has been idle for IdleTimeout. A
// keepalive client or any subscriber counts as activity, so the desktop app
// keeps the daemon alive simply by staying connected.
func (s *Server) watchIdle(ctx context.Context) {
	if s.opts.IdleTimeout <= 0 {
		return
	}
	tick := idleCheckInterval
	if s.opts.IdleTimeout/4 < tick {
		tick = s.opts.IdleTimeout / 4
	}
	if tick <= 0 {
		tick = time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopping:
			return
		case <-t.C:
			if s.keepalives.Load() > 0 || s.Subscribers() > 0 {
				s.touch()
				continue
			}
			idle := time.Since(time.Unix(0, s.lastActive.Load()))
			if idle >= s.opts.IdleTimeout {
				s.logger.Info("idle timeout reached, stopping",
					"idle", idle.Round(time.Second).String())
				s.Shutdown()
				return
			}
		}
	}
}
