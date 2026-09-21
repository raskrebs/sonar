package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// State is what a share's connection is doing. A dropped control connection is
// "connecting", never "stopped": the share is still a share, the laptop is
// just briefly unreachable.
type State string

const (
	StateConnecting State = "connecting"
	StateConnected  State = "connected"
	// StateDegraded is the tunnel up and the shared app not listening. It is a
	// waiting room, not a failure: restarting a dev server passes through it.
	StateDegraded State = "degraded"
	StateStopped  State = "stopped"
)

// Status is one state change, for whatever is showing the share to a person.
type Status struct {
	State State
	// URL is where the share is reachable, once connected.
	URL string
	// Attempt counts consecutive failed connections; zero once connected.
	Attempt int
	// Err is why, on a failed attempt or a stop.
	Err error
}

// The errors that end a run rather than being retried.
var (
	// ErrUnauthorized: the key is wrong. Retrying cannot fix a wrong key.
	ErrUnauthorized = errors.New("tunnel: the relay refused this key")
	// ErrReplaced: another daemon took this share. Reconnecting would take it
	// back, and the two would do that to each other forever.
	ErrReplaced = errors.New("tunnel: another daemon took this share")
	// ErrUnsupportedVersion: the relay speaks a protocol this build does not.
	ErrUnsupportedVersion = errors.New("tunnel: the relay speaks a different tunnel protocol")
	// ErrServiceGone: the shared port stopped listening and stayed that way
	// for the whole grace window. The share is over, and it does not resume on
	// its own however quickly the port comes back — see watchService.
	ErrServiceGone = errors.New("tunnel: the shared service stopped")
)

// Target is where one request goes: the local address that answers it, the
// path to ask for, and the prefix that was taken off on the way.
//
// Prefix is empty unless something was stripped, which makes it both the
// value for X-Forwarded-Prefix and the record of whether the path changed.
type Target struct {
	Addr   string
	Path   string
	Prefix string
}

// Config is one share: where the relay is, what to prove, and what to serve.
type Config struct {
	// RelayURL is the relay's base URL ("https://relay.trysonar.dev") or the
	// control route itself ("wss://relay.trysonar.dev/v1/tunnel").
	RelayURL string
	// Key authorises the control route. It comes from the environment or the
	// keychain, never from a command line, where ps would read it. It is the
	// account session for a share with a slug, and the operator's tunnel key
	// for the single-hostname connection that predates slugs.
	Key string
	// Share is the slug this connection attaches to, as `?share=<slug>` on the
	// control route. Empty is the legacy single-hostname connection, which
	// only a tunnel key may hold.
	//
	// It is a query parameter rather than a field on the hello frame because
	// the control frames are pinned byte for byte against a fixture checked in
	// to this repository and to sonar-relay; the relay made the same choice
	// for the same reason (sonar-relay/docs/SHARE.md, "The wire surface").
	Share string
	// LocalPort is the port on this machine being shared.
	LocalPort int
	// Route places one request inside a shared project: which service answers
	// it, and what to ask that service for. Nil is a share of one service,
	// where every request goes to LocalPort.
	//
	// A function rather than a table, on purpose. The relay is never told the
	// shape of a project — that is the property which stops it steering this
	// daemon anywhere — and there is no reason for the tunnel to learn it
	// either. It asks where a path goes and dials there; internal/share owns
	// the answer.
	Route func(path string) Target

	// LocalHost is what to dial it on. Empty means "localhost", which covers
	// the dev server that bound ::1 and the one that bound 127.0.0.1.
	LocalHost string
	// Client is what to call this daemon in hello. Logged by the relay.
	Client string
	// OnStatus, when set, is called on every state change, from the run
	// goroutine: it must not block.
	OnStatus func(Status)
	Logger   *slog.Logger
	// MinBackoff, MaxBackoff bound the reconnect wait. Zero means 500ms and
	// 30s.
	MinBackoff, MaxBackoff time.Duration
	// HandshakeTimeout bounds the dial and the hello exchange. Zero means 15s.
	HandshakeTimeout time.Duration
	// DialTimeout bounds connecting to the local app. Zero means 5s.
	DialTimeout time.Duration
	// Linger is how long a connection that said going_away keeps serving what
	// it already has. Zero means 30s.
	Linger time.Duration
	// WatchInterval is how often the shared port is checked. Zero means 2s.
	WatchInterval time.Duration
	// ServiceGrace is how long the shared port may stay dead before the share
	// ends for good. Zero means 90s.
	ServiceGrace time.Duration
	// HTTPClient performs the WebSocket handshake. Zero means the default.
	HTTPClient *http.Client
	// OnRequest, when set, is called once per forwarded exchange, from that
	// exchange's goroutine: it must not block. It is what `share.logs` tails.
	// Nothing about a request's content is offered — no headers, no bodies —
	// because the daemon has no business keeping those and a log that held
	// them would be the wrong thing to have on the machine being shared.
	OnRequest func(RequestLog)
}

// RequestLog is one forwarded exchange, as the daemon saw it.
type RequestLog struct {
	At       time.Time
	Method   string
	Path     string
	Upgrade  bool
	BytesOut int64
	Duration time.Duration
	// Err is why the exchange did not reach the app, when it did not. An
	// exchange that reached the app has no error here even if the app answered
	// a 500: the daemon splices bytes and never parses the response, so the
	// status is the relay's to log and not this side's.
	Err error
}

const (
	defaultMinBackoff       = 500 * time.Millisecond
	defaultMaxBackoff       = 30 * time.Second
	defaultHandshakeTimeout = 15 * time.Second
	defaultDialTimeout      = 5 * time.Second
	defaultLinger           = 30 * time.Second
	defaultWatchInterval    = 2 * time.Second
	// defaultServiceGrace is how long a restart may take before the share is
	// over: about 45 checks at the default interval.
	defaultServiceGrace = 90 * time.Second
	// stableAfter is how long a connection must last before the next failure
	// starts counting from zero again. Without it, a relay that accepts and
	// immediately drops would be retried as fast as it can accept.
	stableAfter = 10 * time.Second
)

// Run holds one share open until ctx is cancelled or something terminal
// happens: a refused key, a protocol mismatch, or another daemon taking the
// share. Everything else — a dropped connection, a relay restart, a laptop
// that closed its lid — is reconnected with backoff and jitter.
//
// It returns nil when ctx ends it.
func Run(ctx context.Context, cfg Config) error {
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	return c.run(ctx)
}

type client struct {
	cfg   Config
	url   string
	local string
	log   *slog.Logger
	// wg covers the per-request goroutines, so Run does not return while a
	// request is still being forwarded.
	wg sync.WaitGroup
}

func newClient(cfg Config) (*client, error) {
	if strings.TrimSpace(cfg.Key) == "" {
		return nil, errors.New("tunnel: a key is required")
	}
	if cfg.LocalPort < 1 || cfg.LocalPort > 65535 {
		return nil, fmt.Errorf("tunnel: %d is not a port", cfg.LocalPort)
	}
	u, err := ControlURL(cfg.RelayURL)
	if err != nil {
		return nil, err
	}
	if u, err = withShare(u, cfg.Share); err != nil {
		return nil, err
	}
	if cfg.LocalHost == "" {
		cfg.LocalHost = "localhost"
	}
	if cfg.Client == "" {
		cfg.Client = "sonar"
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = defaultHandshakeTimeout
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.Linger <= 0 {
		cfg.Linger = defaultLinger
	}
	if cfg.WatchInterval <= 0 {
		cfg.WatchInterval = defaultWatchInterval
	}
	if cfg.ServiceGrace <= 0 {
		cfg.ServiceGrace = defaultServiceGrace
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &client{
		cfg:   cfg,
		url:   u,
		local: net.JoinHostPort(cfg.LocalHost, fmt.Sprint(cfg.LocalPort)),
		log:   cfg.Logger,
	}, nil
}

// ControlURL turns whatever the caller has — a relay base URL or the control
// route itself, http or ws — into the WebSocket URL to dial.
func ControlURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("tunnel: a relay URL is required")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("tunnel: %q is not a URL: %w", raw, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("tunnel: %q is not an http or ws URL", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("tunnel: %q has no host", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = Path
	}
	return u.String(), nil
}

// withShare names the slug on a control URL. A relay URL that already carries
// other parameters keeps them.
func withShare(raw, slug string) (string, error) {
	if strings.TrimSpace(slug) == "" {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("tunnel: %q is not a URL: %w", raw, err)
	}
	q := u.Query()
	q.Set("share", strings.TrimSpace(slug))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *client) run(ctx context.Context) error {
	defer c.wg.Wait()
	attempt := 0
	for {
		c.status(Status{State: StateConnecting, Attempt: attempt})
		cn, err := c.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				c.status(Status{State: StateStopped})
				return nil
			}
			if terminal(err) {
				c.status(Status{State: StateStopped, Err: err})
				return err
			}
			c.log.Warn("the tunnel could not connect", "err", err, "attempt", attempt)
			c.status(Status{State: StateConnecting, Attempt: attempt, Err: err})
			if !sleep(ctx, jitter(c.backoff(attempt))) {
				c.status(Status{State: StateStopped})
				return nil
			}
			attempt++
			continue
		}

		c.log.Info("the share is live", "url", cn.url)
		c.status(Status{State: StateConnected, URL: cn.url})
		started := time.Now()
		reason, retryAfter, err := c.serve(ctx, cn)
		if ctx.Err() != nil {
			c.status(Status{State: StateStopped})
			return nil
		}
		if errors.Is(err, ErrServiceGone) {
			// Terminal on purpose. A port is not an identity: whatever binds it
			// next may be a different project, and resuming would hand that
			// project a URL someone already shared.
			c.log.Info("the shared service stopped; the share has ended",
				"addr", c.local, "grace", c.cfg.ServiceGrace)
			c.status(Status{State: StateStopped, Err: err})
			return err
		}

		var wait time.Duration
		switch {
		case reason == GoingAwayServiceGone:
			// The relay's own clock ran out on a share whose service never
			// came back. It means what our own window means, and it is
			// terminal for the same reason: nothing resumes without a person.
			cn.close()
			c.log.Info("the relay ended the share; its service never came back",
				"addr", c.local)
			c.status(Status{State: StateStopped, Err: ErrServiceGone})
			return ErrServiceGone
		case reason == GoingAwayReplaced:
			// Nothing to linger for: this daemon is stopping, and the relay is
			// already serving the share from the connection that replaced it.
			cn.close()
			c.status(Status{State: StateStopped, Err: ErrReplaced})
			return ErrReplaced
		case reason != "":
			// A relay restart is not a failure: come back when it said to.
			c.log.Info("the relay is going away", "reason", reason, "retry_after", retryAfter)
			attempt = 0
			if retryAfter <= 0 {
				retryAfter = c.cfg.MinBackoff
			}
			wait = jitter(retryAfter)
		default:
			c.log.Warn("the tunnel dropped", "err", err, "up", time.Since(started).Round(time.Millisecond))
			if time.Since(started) >= stableAfter {
				attempt = 0
			}
			wait = jitter(c.backoff(attempt))
			attempt++
		}
		if !sleep(ctx, wait) {
			c.status(Status{State: StateStopped})
			return nil
		}
	}
}

// serve runs one connection, forwarding requests until it ends. It returns the
// going_away reason when there was one.
func (c *client) serve(ctx context.Context, cn *conn) (string, time.Duration, error) {
	// The service watcher lives exactly as long as this connection does.
	wctx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	gone := make(chan struct{}, 1)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.watchService(wctx, cn, gone)
	}()

	go func() {
		for {
			stream, err := cn.sess.AcceptStream()
			if err != nil {
				return
			}
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				c.forward(ctx, stream)
			}()
		}
	}()

	frames := make(chan Frame, 4)
	go func() {
		defer close(frames)
		for {
			var f Frame
			if err := cn.dec.Decode(&f); err != nil {
				return
			}
			frames <- f
		}
	}()

	for {
		select {
		case <-ctx.Done():
			cn.close()
			return "", 0, ctx.Err()
		case <-gone:
			// The shared service was gone for the whole window. This share is
			// over: close the control connection and do not come back.
			cn.close()
			return "", 0, ErrServiceGone
		case <-cn.sess.CloseChan():
			cn.close()
			return "", 0, errors.New("the control connection dropped")
		case f, ok := <-frames:
			if !ok {
				// The control stream ended; the session close follows.
				frames = nil
				continue
			}
			if f.Type != FrameGoingAway {
				continue
			}
			// Do not close: requests already in flight keep being served
			// until the relay closes the connection itself.
			c.linger(ctx, cn)
			return f.Reason, time.Duration(f.RetryAfterMS) * time.Millisecond, nil
		}
	}
}

// linger keeps a going-away connection alive for what it already has, and
// closes it if the relay never does.
func (c *client) linger(ctx context.Context, cn *conn) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		t := time.NewTimer(c.cfg.Linger)
		defer t.Stop()
		select {
		case <-cn.sess.CloseChan():
		case <-ctx.Done():
		case <-t.C:
		}
		cn.close()
	}()
}

// conn is one control connection and the session on it.
type conn struct {
	sess   *yamux.Session
	dec    *json.Decoder
	enc    *json.Encoder
	sendMu sync.Mutex
	url    string
	cancel context.CancelFunc
	once   sync.Once
}

// send writes one frame to the relay. Frames are small and rare; one lock
// keeps the handshake and the service watcher from interleaving lines.
func (cn *conn) send(f Frame) error {
	cn.sendMu.Lock()
	defer cn.sendMu.Unlock()
	return cn.enc.Encode(f)
}

func (cn *conn) close() {
	cn.once.Do(func() {
		_ = cn.sess.Close()
		cn.cancel()
	})
}

func (c *client) connect(ctx context.Context) (*conn, error) {
	dctx, cancel := context.WithTimeout(ctx, c.cfg.HandshakeTimeout)
	defer cancel()

	ws, resp, err := websocket.Dial(dctx, c.url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.cfg.Key}},
		HTTPClient: c.cfg.HTTPClient,
	})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("%w (%s)", ErrUnauthorized, resp.Status)
		}
		return nil, err
	}

	// The session's life is its own, not the handshake's.
	sctx, scancel := context.WithCancel(context.Background())
	nc := websocket.NetConn(sctx, ws, websocket.MessageBinary)
	sess, err := yamux.Client(nc, yamuxConfig())
	if err != nil {
		scancel()
		_ = ws.CloseNow()
		return nil, err
	}
	cn := &conn{sess: sess, cancel: scancel}

	ctrl, err := sess.OpenStream()
	if err != nil {
		cn.close()
		return nil, fmt.Errorf("opening the control stream: %w", err)
	}
	_ = ctrl.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	cn.enc = json.NewEncoder(ctrl)
	if err := cn.send(Frame{
		Type: FrameHello, Version: ProtocolVersion, Client: c.cfg.Client}); err != nil {
		cn.close()
		return nil, fmt.Errorf("sending hello: %w", err)
	}
	dec := json.NewDecoder(ctrl)
	var f Frame
	if err := dec.Decode(&f); err != nil {
		cn.close()
		return nil, fmt.Errorf("reading the relay's answer: %w", err)
	}
	_ = ctrl.SetDeadline(time.Time{})

	switch f.Type {
	case FrameWelcome:
	case FrameError:
		cn.close()
		if f.Reason == ErrReasonUnsupportedVersion {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedVersion, f.Message)
		}
		return nil, fmt.Errorf("the relay refused the connection: %s (%s)", f.Message, f.Reason)
	default:
		cn.close()
		return nil, fmt.Errorf("the relay answered a hello with %q", f.Type)
	}
	cn.dec = dec
	cn.url = f.URL
	return cn, nil
}

func (c *client) status(s Status) {
	if c.cfg.OnStatus != nil {
		c.cfg.OnStatus(s)
	}
}

func terminal(err error) bool {
	return errors.Is(err, ErrUnauthorized) ||
		errors.Is(err, ErrUnsupportedVersion) ||
		errors.Is(err, ErrReplaced) ||
		errors.Is(err, ErrServiceGone)
}

// backoff doubles from MinBackoff to MaxBackoff.
func (c *client) backoff(attempt int) time.Duration {
	d := c.cfg.MinBackoff
	for i := 0; i < attempt && d < c.cfg.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, c.cfg.MaxBackoff)
}

// jitter spreads a wait over [d/2, d], so a relay coming back up is not met by
// every daemon it dropped at the same millisecond.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// sleep waits, and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// yamuxConfig matches the relay's: keepalives often enough to sit under every
// idle timeout between here and it, and a stream the relay never finishes
// closing is given up on after a minute.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.StreamCloseTimeout = time.Minute
	cfg.LogOutput = io.Discard
	return cfg
}
