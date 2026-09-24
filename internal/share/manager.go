// Package share is the daemon's half of `sonar share`: the `share.*` RPC
// methods, the calls to the relay's share control plane, and the tunnel that
// each live public share holds open.
//
// # The daemon does the work
//
// A client asks for a share and gets a URL back. Everything between those two
// things happens here and stays here: the relay call that reserves or reclaims
// the slug, the account session that authenticates it, the outbound WebSocket
// the share is served over, and the watcher that ends the share when the shared
// service goes away for good. Nothing about a share lives in the process that
// asked for it, which is why `sonar share 3000 --public` can print a URL and
// exit, and why closing the terminal — or the SSH session it was on — does not
// take the preview down with it.
//
// # Reach is always written down
//
// There is no default reach and there will not be one. "Everyone on this wifi"
// and "the entire internet" are far enough apart that nobody should arrive at
// the second by forgetting a flag (sonar-relay/docs/SHARE.md). `share.create`
// refuses an empty reach rather than choosing.
//
// `lan` is accepted by the protocol and not built: it is a listener the daemon
// opens on 0.0.0.0, which is a different mechanism with a firewall prompt and a
// port-in-use case of its own, and it returns a plain "not built yet".
package share

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/raskrebs/sonar/internal/tunnel"
)

// The two reaches. Only one of them is built.
const (
	ReachPublic = "public"
	ReachLAN    = "lan"
)

// connectTimeout is how long `share.create` waits for the tunnel to come up
// before answering. Long enough for a handshake over a bad link; short enough
// that a person is not left looking at a prompt. Timing out is not a failure:
// the share is published and the connection is still being made, so the answer
// is a `connecting` share rather than an error.
const connectTimeout = 20 * time.Second

// logLines is how many forwarded requests a share remembers for `share.logs`.
// It is a debugging aid on the machine doing the forwarding, not an audit log —
// the takedown log is the relay's, and it is the relay that can answer for it.
const logLines = 200

// Manager owns every live share on this daemon.
type Manager struct {
	session caller
	log     *slog.Logger
	now     func() time.Time
	// dial runs one tunnel until it ends. A seam, so the tests drive create,
	// stop, extend and the liveness rules without a relay to dial.
	dial func(ctx context.Context, cfg tunnel.Config) error
	// probeFn asks a local address what it is. A seam for the same reason as
	// dial: a test that had to stand up a Postgres to prove the refusal would
	// not be run.
	probeFn func(ctx context.Context, addr string) (probeResult, error)
	// connectTimeout is how long Create waits for `ready`.
	connectTimeout time.Duration
	// installID is this machine's id for the fallback key.
	installID string
	// tunnelURL is where the control connection is dialled. Empty means the
	// relay itself, which is what every hosted install uses.
	tunnelURL string

	mu     sync.Mutex
	shares map[string]*live
}

// Options builds a Manager. Every field has a working default.
type Options struct {
	// Session is where the account session comes from. Nil means
	// internal/session, which is what production wants; a test that has no
	// credentials store passes its own.
	Session        caller
	Logger         *slog.Logger
	Now            func() time.Time
	Dial           func(ctx context.Context, cfg tunnel.Config) error
	ConnectTimeout time.Duration
	InstallID      string
	// TunnelURL is where a share's control connection goes when that is not
	// the relay's own origin (config `share.tunnel`). Empty means the relay.
	TunnelURL string
}

// live is one share this daemon is holding.
type live struct {
	mu     sync.Mutex
	share  state.Share
	slug   string
	target target
	ttl    string

	cancel context.CancelFunc
	done   chan struct{}
	// stopping is set when a person ended the share, so the goroutine that
	// notices the tunnel finishing does not tell the relay a second time.
	stopping bool

	logs []string
}

// New builds a Manager.
func New(opts Options) *Manager {
	m := &Manager{
		session:        opts.Session,
		log:            opts.Logger,
		now:            opts.Now,
		dial:           opts.Dial,
		connectTimeout: opts.ConnectTimeout,
		installID:      opts.InstallID,
		tunnelURL:      opts.TunnelURL,
		shares:         map[string]*live{},
	}
	if m.session == nil {
		m.session = sessionAdapter{}
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.probeFn == nil {
		m.probeFn = probe
	}
	if m.dial == nil {
		m.dial = tunnel.Run
	}
	if m.connectTimeout <= 0 {
		m.connectTimeout = connectTimeout
	}
	if m.installID == "" {
		m.installID = InstallID()
	}
	return m
}

// Create is `share.create`.
func (m *Manager) Create(ctx context.Context, snap state.Snapshot, p rpc.ShareCreateParams) (state.Share, []string, error) {
	reach := strings.TrimSpace(strings.ToLower(p.Reach))
	switch reach {
	case "":
		// Deliberately not a default. See the package comment.
		return state.Share{}, nil, rpc.NewError(rpc.CodeInvalidParams,
			`a reach is required: "lan" for this network, "public" for the internet`,
			"there is no default, because the two are too far apart to pick by accident")
	case ReachLAN:
		return state.Share{}, nil, rpc.NewError(rpc.CodeUnsupported,
			"sharing on the LAN is not built yet",
			"`--public` works today; a LAN share is a listener on this machine and is still to come")
	case ReachPublic:
	default:
		return state.Share{}, nil, rpc.Errorf(rpc.CodeInvalidParams,
			"%q is not a reach; use \"lan\" or \"public\"", p.Reach)
	}

	ttl := TTLWhileItRuns
	if p.TTL != nil {
		normalized, ok := NormalizeTTL(*p.TTL)
		if !ok {
			return state.Share{}, nil, rpc.Errorf(rpc.CodeInvalidParams,
				"%q is not a ttl; use %q, %q or %q", *p.TTL, TTLWhileItRuns, TTLOneHour, TTLOneDay)
		}
		ttl = normalized
	}

	t, err := resolveTarget(snap, p.Target)
	if err != nil {
		return state.Share{}, nil, err
	}

	// Publishing the same thing twice gets the same URL, and when this daemon
	// is already holding it there is nothing to do: a second tunnel for one
	// slug would have the relay hand the share to the new connection and tell
	// the old one it was replaced, for no gain.

	// Sharing the whole project rather than the one service.
	//
	// Before the check below, not after it: a project share has a reservation
	// of its own, so resolving it is what decides which reservation "already
	// sharing this" is asking about. Before anything is published, too, so a
	// project that cannot be shared costs no slug.
	var proj *project
	if p.Project {
		resolved, perr := m.resolveProject(ctx, snap, t)
		if perr != nil {
			return state.Share{}, nil, perr
		}
		t = resolved.key()
		proj = &resolved
	}

	if existing, ok := m.liveFor(t); ok {
		return existing.snapshot(), nil, nil
	}

	// Ask the port what it is before reserving anything. A share pointed at a
	// database is a URL that will never work, and finding that out from a
	// blank page costs a slug and whatever the link was pasted into.
	notes, err := m.checkTarget(ctx, t)
	if err != nil {
		return state.Share{}, nil, err
	}
	if proj != nil {
		// Where each service sits, and what was left behind, under the URL.
		notes = append(notes, proj.describe()...)
	}

	req, err := m.request(t, ttl, p.Replace)
	if err != nil {
		return state.Share{}, nil, err
	}
	view, err := m.publish(ctx, req, t)
	if err != nil {
		return state.Share{}, nil, err
	}

	token, err := m.session.Token()
	if err != nil {
		return state.Share{}, nil, err
	}

	row := m.viewToShare(view, ReachPublic)
	row.Status = "connecting"
	if t.Group != "" {
		group := t.Group
		row.TargetGroup = &group
	}
	if row.TargetPort == 0 {
		row.TargetPort = t.Port
	}
	if row.CreatedAt == "" {
		row.CreatedAt = m.now().UTC().Format(time.RFC3339)
	}

	l := &live{share: row, slug: view.Slug, target: t, ttl: ttl, done: make(chan struct{})}
	m.mu.Lock()
	m.shares[row.ID] = l
	m.mu.Unlock()

	ready := make(chan struct{})
	var readyOnce sync.Once
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l.cancel = cancel

	cfg := tunnel.Config{
		RelayURL: m.controlURL(),
		Key:      token,
		Share:    view.Slug,
		// The entry service. Liveness watches this one: the share ends when
		// the thing at the root goes away, while a secondary service dying
		// only fails its own paths.
		LocalPort: t.Port,
		Client:    "sonar-daemon",
		Logger:    m.log.With("share", view.Slug),
		OnStatus: func(s tunnel.Status) {
			l.onStatus(s)
			if s.State == tunnel.StateConnected {
				readyOnce.Do(func() { close(ready) })
			}
		},
		OnRequest: func(entry tunnel.RequestLog) { l.record(entry) },
	}
	if proj != nil {
		cfg.Route = proj.Routes.router()
	}

	go func() {
		defer close(l.done)
		err := m.dial(runCtx, cfg)
		readyOnce.Do(func() { close(ready) })
		m.finished(l, err)
	}()

	// Wait for the URL to be worth printing. A timeout is not a failure — the
	// slug is reserved and the connection is still being made — so the caller
	// gets a `connecting` share and the same URL.
	select {
	case <-ready:
	case <-time.After(m.connectTimeout):
	case <-ctx.Done():
	}

	out := l.snapshot()
	// Last, because the sentence carries the URL and the URL is only settled
	// here. An application that checks which page is asking — a CORS list, a
	// sign-in redirect allowlist — will refuse this share while it names only
	// localhost, and the error it produces says "CORS" rather than saying
	// which variable to edit.
	if g, ok := groupNamed(snap, t.Group); ok {
		if note := originNote(g, out.URL); note != "" {
			notes = append(notes, note)
		}
	}
	m.log.Info("share published", "slug", view.Slug, "url", out.URL,
		"port", t.Port, "status", out.Status, "ttl", ttl)
	return out, notes, nil
}

// controlURL is where the tunnel dials. The relay's own origin unless this
// install points the share edge somewhere else.
func (m *Manager) controlURL() string {
	if u := strings.TrimSpace(m.tunnelURL); u != "" {
		return u
	}
	return m.session.Relay()
}

// request builds the body of `POST /v1/shares` from a resolved target.
func (m *Manager) request(t target, ttl string, replace bool) (publishRequest, error) {
	req := publishRequest{TTL: ttl, Replace: replace}
	if t.committed() {
		req.Repo, req.Worktree, req.ServiceName = t.Repo, t.Worktree, t.Service
		return req, nil
	}
	req.InstallID, req.ProjectRoot, req.Port = m.installID, t.ProjectRoot, t.Port
	if req.InstallID == "" || req.ProjectRoot == "" || req.Port <= 0 {
		return publishRequest{}, rpc.NewError(rpc.CodeInvalidParams,
			"this port has no project behind it, so there is nothing to key a URL on: "+
				"sonar can see neither a sonar.yaml service nor the directory the process is running in",
			"run `sonar init` in the project and commit the sonar.yaml — "+
				"then the URL follows the project rather than this machine")
	}
	return req, nil
}

// Stop is `share.stop`. It cancels the tunnel and tells the relay, in that
// order: the connection closing is what makes the URL stop working now, and the
// row is what makes it stay stopped.
func (m *Manager) Stop(ctx context.Context, p rpc.ShareStopParams) ([]string, error) {
	chosen, err := m.choose(p)
	if err != nil {
		return nil, err
	}
	stopped := make([]string, 0, len(chosen))
	var failures []error
	for _, l := range chosen {
		l.mu.Lock()
		l.stopping = true
		slug := l.slug
		l.mu.Unlock()

		if l.cancel != nil {
			l.cancel()
		}
		if err := m.stopRemote(ctx, slug); err != nil {
			failures = append(failures, err)
			continue
		}
		l.setStatus("stopped", "stopped here")
		m.forget(l.snapshot().ID)
		stopped = append(stopped, l.snapshot().ID)
	}
	if len(stopped) == 0 && len(failures) > 0 {
		return nil, failures[0]
	}
	return stopped, nil
}

// choose resolves `share.stop`'s three ways of naming shares.
func (m *Manager) choose(p rpc.ShareStopParams) ([]*live, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case p.All:
		out := make([]*live, 0, len(m.shares))
		for _, l := range m.shares {
			out = append(out, l)
		}
		return out, nil
	case p.ID != nil && *p.ID != "":
		l, ok := m.shares[*p.ID]
		if !ok {
			// The slug is what a person has in front of them, so accept it too.
			for _, candidate := range m.shares {
				if candidate.slug == *p.ID {
					return []*live{candidate}, nil
				}
			}
			return nil, rpc.Errorf(rpc.CodeNotFound, "no share here has the id %q", *p.ID)
		}
		return []*live{l}, nil
	case p.Target != nil && p.Target.Port != nil:
		var out []*live
		for _, l := range m.shares {
			if l.target.Port == *p.Target.Port {
				out = append(out, l)
			}
		}
		if len(out) == 0 {
			return nil, rpc.Errorf(rpc.CodeNotFound, "nothing is shared from port %d", *p.Target.Port)
		}
		return out, nil
	}
	return nil, rpc.NewError(rpc.CodeInvalidParams,
		"name a share by id, by target, or pass all", `send {"all": true} to stop every share`)
}

// List is `share.list`: the shares this daemon is holding.
//
// Not the relay's list. A reservation on the relay is a slug waiting to be
// re-used and has no target, no port and no process behind it on this machine;
// what a client wants from a daemon is what is live here. `GET /v1/shares` is
// where the reservations are, for whoever wants them.
func (m *Manager) List() []state.Share {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]state.Share, 0, len(m.shares))
	for _, l := range m.shares {
		out = append(out, l.snapshot())
	}
	return out
}

// Extend is `share.extend`: a new expiry, and the same URL.
func (m *Manager) Extend(ctx context.Context, p rpc.ShareExtendParams) (state.Share, error) {
	ttl, ok := NormalizeTTL(p.TTL)
	if !ok {
		return state.Share{}, rpc.Errorf(rpc.CodeInvalidParams,
			"%q is not a ttl; use %q, %q or %q", p.TTL, TTLWhileItRuns, TTLOneHour, TTLOneDay)
	}
	l, err := m.byID(p.ID)
	if err != nil {
		return state.Share{}, err
	}
	view, err := m.extendRemote(ctx, l.slug, ttl)
	if err != nil {
		return state.Share{}, err
	}
	l.mu.Lock()
	l.ttl = ttl
	if view.ExpiresAt != "" {
		at := view.ExpiresAt
		l.share.ExpiresAt = &at
	}
	l.mu.Unlock()
	return l.snapshot(), nil
}

// Logs is `share.logs`.
func (m *Manager) Logs(p rpc.ShareLogsParams) ([]string, error) {
	l, err := m.byID(p.ID)
	if err != nil {
		return nil, err
	}
	return l.tail(p.Tail), nil
}

func (m *Manager) byID(id string) (*live, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.shares[id]; ok {
		return l, nil
	}
	for _, l := range m.shares {
		if l.slug == id {
			return l, nil
		}
	}
	return nil, rpc.Errorf(rpc.CodeNotFound, "no share here has the id %q", id)
}

func (m *Manager) liveFor(t target) (*live, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.shares {
		if sameKey(l.target, t) && isLive(l.snapshot().Status) {
			return l, true
		}
	}
	return nil, false
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.shares, id)
	m.mu.Unlock()
}

// finished is what the tunnel ending means for the share.
//
// ErrServiceGone is the one that has to tell the relay: the shared service
// stayed dead for the whole grace window, the share is over, and the row must
// say so rather than sit there as a live share nothing is connected to. It does
// not resume however quickly the port comes back — a port is not an identity,
// and the next thing to bind it may be a different project.
func (m *Manager) finished(l *live, err error) {
	l.mu.Lock()
	stopping := l.stopping
	slug := l.slug
	id := l.share.ID
	l.mu.Unlock()
	if stopping {
		return
	}

	reason := "the tunnel ended"
	switch {
	case err == nil:
		reason = "the daemon stopped the share"
	case errors.Is(err, tunnel.ErrServiceGone):
		reason = "the shared service stopped"
	case errors.Is(err, tunnel.ErrUnauthorized):
		reason = "the relay refused this session"
	case errors.Is(err, tunnel.ErrReplaced):
		reason = "another connection took this share"
	default:
		reason = err.Error()
	}
	l.setStatus("stopped", reason)
	l.append(m.now(), "share stopped: "+reason)
	m.log.Info("share ended", "slug", slug, "reason", reason)

	// Telling the relay is best effort and deliberately not conditional on
	// anything: an unreachable relay already ends the share when the control
	// connection drops, and the row's own expiry is the backstop.
	if !errors.Is(err, tunnel.ErrReplaced) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if stopErr := m.stopRemote(ctx, slug); stopErr != nil {
			m.log.Debug("could not tell the relay the share ended", "slug", slug, "err", stopErr)
		}
		cancel()
	}
	m.forget(id)
}

// StopAll ends every share. The daemon calls it on shutdown: a share whose
// daemon is gone cannot serve anything, and leaving the row live would have the
// relay answer 502 for it until its own clock ran out.
func (m *Manager) StopAll() {
	m.mu.Lock()
	all := make([]*live, 0, len(m.shares))
	for _, l := range m.shares {
		all = append(all, l)
	}
	m.shares = map[string]*live{}
	m.mu.Unlock()

	for _, l := range all {
		l.mu.Lock()
		l.stopping = true
		slug := l.slug
		l.mu.Unlock()
		if l.cancel != nil {
			l.cancel()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = m.stopRemote(ctx, slug)
		cancel()
	}
}

func sameKey(a, b target) bool {
	if a.committed() != b.committed() {
		return false
	}
	if a.committed() {
		return a.Repo == b.Repo && a.Worktree == b.Worktree && a.Service == b.Service
	}
	return a.ProjectRoot == b.ProjectRoot && a.Port == b.Port
}

// isLive is the relay's own definition: the three statuses that count against
// an account's limit. A reservation is not one of them.
func isLive(status string) bool {
	switch status {
	case "ready", "degraded", "connecting":
		return true
	}
	return false
}

// ---------------------------------------------------------------- live ---

func (l *live) snapshot() state.Share {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.share
}

func (l *live) setStatus(status, reason string) {
	l.mu.Lock()
	l.share.Status = status
	l.share.StatusReason = reason
	l.mu.Unlock()
}

// onStatus maps the tunnel's vocabulary onto the share statuses SHARE.md fixes.
//
// A dropped control connection is `connecting`, never `stopped`: the share is
// still a share and the laptop is just briefly unreachable.
func (l *live) onStatus(s tunnel.Status) {
	switch s.State {
	case tunnel.StateConnected:
		l.setStatus("ready", "")
		l.append(time.Now(), "connected: "+l.urlOrSlug())
	case tunnel.StateConnecting:
		if s.Attempt > 0 {
			l.setStatus("connecting", fmt.Sprintf("reconnecting (attempt %d)", s.Attempt+1))
			return
		}
		l.setStatus("connecting", "")
	case tunnel.StateDegraded:
		// The service stopped listening. The relay answers 503 for up to
		// ninety seconds, which is what a dev-server restart costs.
		l.setStatus("degraded", "the shared service is not answering")
		l.append(time.Now(), "degraded: the shared service is not answering")
	case tunnel.StateStopped:
		// finished() writes the real reason; this only fills in when the run
		// ends without one.
		l.mu.Lock()
		if isLive(l.share.Status) {
			l.share.Status = "stopped"
		}
		l.mu.Unlock()
	}
}

func (l *live) urlOrSlug() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.share.URL != "" {
		return l.share.URL
	}
	return l.slug
}

// record is one forwarded request, for `share.logs`.
func (l *live) record(entry tunnel.RequestLog) {
	line := fmt.Sprintf("%s %s %s", entry.At.UTC().Format(time.RFC3339), entry.Method, entry.Path)
	switch {
	case entry.Err != nil:
		line += " — the app did not answer: " + entry.Err.Error()
	case entry.Upgrade:
		line += fmt.Sprintf(" (upgraded, %d bytes, %s)", entry.BytesOut, entry.Duration.Round(time.Millisecond))
	default:
		line += fmt.Sprintf(" (%d bytes, %s)", entry.BytesOut, entry.Duration.Round(time.Millisecond))
	}
	l.mu.Lock()
	l.share.Requests++
	l.share.BytesOut += entry.BytesOut
	at := entry.At.UTC().Format(time.RFC3339)
	l.share.LastActiveAt = &at
	l.mu.Unlock()
	l.appendLine(line)
}

func (l *live) append(at time.Time, text string) {
	l.appendLine(at.UTC().Format(time.RFC3339) + " " + text)
}

func (l *live) appendLine(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logs = append(l.logs, line)
	if len(l.logs) > logLines {
		l.logs = l.logs[len(l.logs)-logLines:]
	}
}

func (l *live) tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.logs) {
		n = len(l.logs)
	}
	out := make([]string, n)
	copy(out, l.logs[len(l.logs)-n:])
	return out
}
