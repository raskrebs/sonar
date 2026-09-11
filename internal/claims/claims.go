package claims

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/raskrebs/sonar/internal/store"
)

// DefaultTTL is how long a claim lives when the caller does not say: one day,
// the `claim_port` tool's default in spec 2.
const DefaultTTL = 24 * time.Hour

// MaxCount caps how many ports one key may hold, so a typo in `count` cannot
// reserve a whole range. It is the same cap `sonar.yaml` puts on
// worktree_ports, so a block a config asks for is always one Acquire grants.
const MaxCount = groups.MaxWorktreePorts

// acquireAttempts is how often Acquire re-derives its ports when the table
// changed underneath it. Inside one daemon the mutex makes a second attempt
// unnecessary; a second writer on the same database (two daemons on one HOME)
// is what this is for.
const acquireAttempts = 3

// ErrExhausted is returned when the range holds no free port left.
var ErrExhausted = errors.New("no free port left in the claim range")

// ErrNoProject is returned when neither a key nor a project was given.
var ErrNoProject = errors.New("a claim needs a project or a key")

// Table is the persistence Manager needs; *store.Store's Claims view is the
// production implementation.
type Table interface {
	Put(rows ...store.ClaimRow) error
	Get(key string) ([]store.ClaimRow, error)
	List() ([]store.ClaimRow, error)
	Delete(key string) (int, error)
	Expire(now time.Time) (int, error)
}

// Options tunes a Manager. The zero value uses the spec's range, the wall
// clock and no listener probe.
type Options struct {
	// Range is the window ports are derived from. Zero means DefaultRange.
	Range Range
	// Now is the clock, replaced in tests.
	Now func() time.Time
	// Listening reports the ports something is bound to right now — the
	// daemon's scan snapshot. A claim never hands out a port that is already
	// serving, except one this same key already holds (that listener is very
	// likely the caller's own process, and taking the port away from it would
	// break the idempotency the whole feature exists for).
	Listening func() (map[int]bool, error)
	// DefaultTTL overrides DefaultTTL for callers that pass no ttl.
	DefaultTTL time.Duration
	// DefaultCount is how many ports a request that names no count takes for
	// a project: the daemon answers it from the project's `sonar.yaml`
	// worktree_ports. A result of zero, or a nil func, means one port.
	DefaultCount func(project string) int
}

// Manager is the claims book. One lives in the daemon, per database.
type Manager struct {
	table        Table
	rng          Range
	now          func() time.Time
	ttl          time.Duration
	live         func() (map[int]bool, error)
	defaultCount func(project string) int

	mu sync.Mutex
}

// New builds a manager over a claims table.
func New(t Table, o Options) *Manager {
	m := &Manager{
		table: t, rng: o.Range, now: o.Now, ttl: o.DefaultTTL, live: o.Listening,
		defaultCount: o.DefaultCount,
	}
	if m.rng.Valid() != nil {
		m.rng = DefaultRange
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.ttl <= 0 {
		m.ttl = DefaultTTL
	}
	return m
}

// Range reports the window this manager derives from.
func (m *Manager) Range() Range { return m.rng }

// Request is one acquire. Key wins when set; otherwise it is derived from
// Project and Worktree.
type Request struct {
	Key      string
	Project  string
	Worktree string
	Count    int
	TTL      time.Duration
}

// Result is what an acquire hands back: the same shape as claims.acquire.
type Result struct {
	Key       string
	Project   string
	Worktree  string
	Ports     []int
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Acquire reserves Count ports for a key and returns them.
//
// The same key gets the same ports every time: the ports it already holds are
// taken first, in port order, and only the shortfall is probed for. Probing
// starts at the port derived from the key and walks upward, skipping ports
// that are listening and ports another key holds. The TTL is refreshed on
// every call, so a live agent keeps its block and an abandoned one lets go.
func (m *Manager) Acquire(req Request) (Result, error) {
	key, project, worktree, err := resolve(req)
	if err != nil {
		return Result{}, err
	}
	// An explicit count always wins; only an omitted one asks the project.
	count := req.Count
	if count == 0 && m.defaultCount != nil {
		count = m.defaultCount(project)
	}
	if count == 0 {
		count = 1
	}
	if count < 1 || count > MaxCount {
		return Result{}, fmt.Errorf("claims: count must be between 1 and %d", MaxCount)
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = m.ttl
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < acquireAttempts; attempt++ {
		res, err := m.acquireOnce(key, project, worktree, count, ttl)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, store.ErrClaimed) {
			return Result{}, err
		}
		lastErr = err
	}
	return Result{}, lastErr
}

func (m *Manager) acquireOnce(key, project, worktree string, count int, ttl time.Duration) (Result, error) {
	now := m.now()
	if _, err := m.table.Expire(now); err != nil {
		return Result{}, err
	}
	rows, err := m.table.List()
	if err != nil {
		return Result{}, err
	}
	listening, err := m.listening()
	if err != nil {
		return Result{}, err
	}

	heldBy := make(map[int]string, len(rows))
	own := make([]store.ClaimRow, 0, count)
	for _, r := range rows {
		heldBy[r.Port] = r.Key
		if r.Key == key {
			own = append(own, r)
		}
	}
	sort.Slice(own, func(i, j int) bool { return own[i].Port < own[j].Port })

	created := make(map[int]time.Time, len(own))
	chosen := make([]int, 0, count)
	taken := make(map[int]bool, count)
	for _, r := range own {
		if len(chosen) == count {
			break
		}
		chosen = append(chosen, r.Port)
		taken[r.Port] = true
		created[r.Port] = r.CreatedAt
	}

	for i := 0; len(chosen) < count && i < m.rng.Span(); i++ {
		port := m.rng.At(key, i)
		if taken[port] || listening[port] {
			continue
		}
		if holder, ok := heldBy[port]; ok && holder != key {
			continue
		}
		chosen = append(chosen, port)
		taken[port] = true
	}
	if len(chosen) < count {
		return Result{}, fmt.Errorf("%w (%d-%d): wanted %d, found %d",
			ErrExhausted, m.rng.Start, m.rng.End, count, len(chosen))
	}
	sort.Ints(chosen)

	// A shrinking count leaves ports behind; drop the whole key first so what
	// claims.list reports is exactly what the caller was just told it holds.
	if len(own) > len(chosen) {
		if _, err := m.table.Delete(key); err != nil {
			return Result{}, err
		}
	}

	expires := now.Add(ttl)
	write := make([]store.ClaimRow, 0, len(chosen))
	for _, port := range chosen {
		at, ok := created[port]
		if !ok {
			at = now
		}
		write = append(write, store.ClaimRow{
			Port: port, Key: key, Project: project, Worktree: worktree,
			CreatedAt: at, ExpiresAt: expires,
		})
	}
	if err := m.table.Put(write...); err != nil {
		return Result{}, err
	}

	first := now
	for _, port := range chosen {
		if at, ok := created[port]; ok && at.Before(first) {
			first = at
		}
	}
	return Result{
		Key: key, Project: project, Worktree: worktree,
		Ports: chosen, CreatedAt: first, ExpiresAt: expires,
	}, nil
}

// Release drops every port held under key and reports how many went. A key
// nobody holds releases nothing, without erroring.
func (m *Manager) Release(key string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.table.Expire(m.now()); err != nil {
		return 0, err
	}
	return m.table.Delete(key)
}

// List returns the live claims, one row per key, ports ascending.
func (m *Manager) List() ([]state.Claim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.table.Expire(m.now()); err != nil {
		return nil, err
	}
	rows, err := m.table.List()
	if err != nil {
		return nil, err
	}
	return group(rows), nil
}

// Held maps every claimed port to the key holding it, after sweeping expired
// rows. `ports.next` uses it to skip foreign claims.
func (m *Manager) Held() (map[int]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.table.Expire(m.now()); err != nil {
		return nil, err
	}
	rows, err := m.table.List()
	if err != nil {
		return nil, err
	}
	out := make(map[int]string, len(rows))
	for _, r := range rows {
		out[r.Port] = r.Key
	}
	return out, nil
}

// group folds the per-port rows into one Claim per key.
func group(rows []store.ClaimRow) []state.Claim {
	byKey := map[string]*state.Claim{}
	order := []string{}
	for _, r := range rows {
		c, ok := byKey[r.Key]
		if !ok {
			c = &state.Claim{
				Key: r.Key, Project: r.Project, Worktree: r.Worktree,
				CreatedAt: stamp(r.CreatedAt),
				ExpiresAt: stamp(r.ExpiresAt),
			}
			byKey[r.Key] = c
			order = append(order, r.Key)
		}
		c.Ports = append(c.Ports, r.Port)
		if stamp(r.CreatedAt) < c.CreatedAt {
			c.CreatedAt = stamp(r.CreatedAt)
		}
		if stamp(r.ExpiresAt) > c.ExpiresAt {
			c.ExpiresAt = stamp(r.ExpiresAt)
		}
	}
	sort.Strings(order)
	out := make([]state.Claim, 0, len(order))
	for _, k := range order {
		c := byKey[k]
		sort.Ints(c.Ports)
		out = append(out, *c)
	}
	return out
}

// stamp is the wire form of a claim time: UTC RFC 3339, so the strings sort
// chronologically and every client reads one zone.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (m *Manager) listening() (map[int]bool, error) {
	if m.live == nil {
		return map[int]bool{}, nil
	}
	live, err := m.live()
	if err != nil {
		return nil, err
	}
	if live == nil {
		return map[int]bool{}, nil
	}
	return live, nil
}

// resolve turns a request into the key and the project/worktree stored with
// it. An explicit key wins; project and worktree are then recovered from it
// unless the caller sent them too.
func resolve(req Request) (key, project, worktree string, err error) {
	project, worktree = req.Project, req.Worktree
	key = req.Key
	if key == "" {
		if project == "" {
			return "", "", "", ErrNoProject
		}
		key = Key(project, worktree)
	}
	if project == "" {
		project, worktree = SplitKey(key)
	} else if worktree == "" {
		worktree = DefaultWorktree
	}
	return key, project, worktree, nil
}
