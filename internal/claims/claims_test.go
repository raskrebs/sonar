package claims_test

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/claims"
	"github.com/raskrebs/sonar/internal/store"
)

// newManager builds a manager over a real temp database, so the SQL that
// enforces one holder per port is exercised together with the probe.
func newManager(t *testing.T, listening map[int]bool, now func() time.Time) (*claims.Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sonar.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return claims.New(st.Claims(), claims.Options{
		Now:       now,
		Listening: func() (map[int]bool, error) { return listening, nil },
	}), st
}

func acquire(t *testing.T, m *claims.Manager, req claims.Request) claims.Result {
	t.Helper()
	res, err := m.Acquire(req)
	if err != nil {
		t.Fatalf("Acquire(%+v): %v", req, err)
	}
	return res
}

func TestTheSameKeyAlwaysGetsTheSamePorts(t *testing.T) {
	m, _ := newManager(t, nil, nil)
	first := acquire(t, m, claims.Request{Project: "myapp", Worktree: "feature-x"})
	second := acquire(t, m, claims.Request{Project: "myapp", Worktree: "feature-x"})

	if len(first.Ports) != 1 || first.Ports[0] != claims.DefaultRange.Base("myapp/feature-x") {
		t.Fatalf("ports = %v, want the derived base port", first.Ports)
	}
	if second.Ports[0] != first.Ports[0] {
		t.Errorf("second acquire = %v, want the same port as %v", second.Ports, first.Ports)
	}
	if first.Key != "myapp/feature-x" {
		t.Errorf("key = %q", first.Key)
	}
}

func TestDifferentWorktreesGetDifferentPorts(t *testing.T) {
	m, _ := newManager(t, nil, nil)
	a := acquire(t, m, claims.Request{Project: "myapp", Worktree: "feature-x"})
	b := acquire(t, m, claims.Request{Project: "myapp", Worktree: "feature-y"})
	if a.Ports[0] == b.Ports[0] {
		t.Fatalf("both worktrees got port %d", a.Ports[0])
	}
}

func TestProbeSkipsListeningPorts(t *testing.T) {
	key := "myapp/main"
	base := claims.DefaultRange.Base(key)
	m, _ := newManager(t, map[int]bool{base: true, base + 1: true}, nil)

	res := acquire(t, m, claims.Request{Key: key})
	if res.Ports[0] != base+2 {
		t.Fatalf("ports = %v, want the first port past the two listeners (%d)", res.Ports, base+2)
	}
}

func TestProbeSkipsPortsAnotherKeyHolds(t *testing.T) {
	// Two keys derived to overlap: hold the whole of one key's block with a
	// foreign claim and the second key has to walk past it.
	key := "myapp/main"
	base := claims.DefaultRange.Base(key)
	m, st := newManager(t, nil, nil)

	expires := time.Now().Add(time.Hour)
	if err := st.Claims().Put(
		store.ClaimRow{Port: base, Key: "other/main", Project: "other", Worktree: "main", ExpiresAt: expires},
		store.ClaimRow{Port: base + 1, Key: "other/main", Project: "other", Worktree: "main", ExpiresAt: expires},
	); err != nil {
		t.Fatalf("seeding a foreign claim: %v", err)
	}

	res := acquire(t, m, claims.Request{Key: key, Count: 2})
	for _, p := range res.Ports {
		if p == base || p == base+1 {
			t.Fatalf("ports = %v, want none of the foreign claim's ports", res.Ports)
		}
	}
	if len(res.Ports) != 2 {
		t.Fatalf("ports = %v, want two", res.Ports)
	}
}

// TestAnOmittedCountTakesTheProjectDefault: a request with no count asks
// DefaultCount for its project (recovered from the key when only a key was
// sent); an explicit count never asks; a project with no default gets one.
func TestAnOmittedCountTakesTheProjectDefault(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "sonar.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var asked []string
	m := claims.New(st.Claims(), claims.Options{DefaultCount: func(project string) int {
		asked = append(asked, project)
		if project == "shop" {
			return 4
		}
		return 0
	}})

	if res := acquire(t, m, claims.Request{Project: "shop", Worktree: "feature-x"}); len(res.Ports) != 4 {
		t.Errorf("omitted count: ports = %v, want the project's block of 4", res.Ports)
	}
	if res := acquire(t, m, claims.Request{Key: "shop/feature-y"}); len(res.Ports) != 4 {
		t.Errorf("key only: ports = %v, want 4 from the project in the key", res.Ports)
	}
	if res := acquire(t, m, claims.Request{Project: "shop", Worktree: "feature-z", Count: 2}); len(res.Ports) != 2 {
		t.Errorf("explicit count: ports = %v, want the 2 asked for", res.Ports)
	}
	if res := acquire(t, m, claims.Request{Project: "other"}); len(res.Ports) != 1 {
		t.Errorf("no default: ports = %v, want one", res.Ports)
	}
	if got := strings.Join(asked, ","); got != "shop,shop,other" {
		t.Errorf("DefaultCount was asked for %q, want shop,shop,other (never for an explicit count)", got)
	}
}

// TestTheCountCapFitsTheLargestWorktreeBlock: `.sonar.yaml` allows
// worktree_ports up to 100, so the manager must grant 100 and refuse 101.
func TestTheCountCapFitsTheLargestWorktreeBlock(t *testing.T) {
	m, _ := newManager(t, nil, nil)
	if res := acquire(t, m, claims.Request{Key: "big/main", Count: 100}); len(res.Ports) != 100 {
		t.Fatalf("ports = %d, want 100", len(res.Ports))
	}
	if _, err := m.Acquire(claims.Request{Key: "bigger/main", Count: claims.MaxCount + 1}); err == nil {
		t.Fatal("a count above MaxCount should be refused")
	}
}

func TestReacquireRefreshesTheTTLAndKeepsCreatedAt(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := now
	m, st := newManager(t, nil, func() time.Time { return clock })

	first := acquire(t, m, claims.Request{Key: "myapp/main", TTL: time.Hour})
	if !first.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("expires_at = %v, want %v", first.ExpiresAt, now.Add(time.Hour))
	}

	clock = now.Add(30 * time.Minute)
	second := acquire(t, m, claims.Request{Key: "myapp/main", TTL: time.Hour})
	if second.Ports[0] != first.Ports[0] {
		t.Fatalf("refresh moved the port: %v -> %v", first.Ports, second.Ports)
	}
	if !second.ExpiresAt.Equal(clock.Add(time.Hour)) {
		t.Errorf("expires_at = %v, want the refreshed %v", second.ExpiresAt, clock.Add(time.Hour))
	}

	rows, err := st.Claims().Get("myapp/main")
	if err != nil || len(rows) != 1 {
		t.Fatalf("Get = %v, %v", rows, err)
	}
	if !rows[0].CreatedAt.Equal(now) {
		t.Errorf("created_at = %v, want the original %v", rows[0].CreatedAt, now)
	}
}

func TestExpiredClaimsAreSweptAndTheirPortsReused(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := now
	m, st := newManager(t, nil, func() time.Time { return clock })

	stale := acquire(t, m, claims.Request{Key: "gone/main", TTL: time.Minute})

	clock = now.Add(2 * time.Minute)
	live, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("List = %+v, want the expired claim swept", live)
	}
	rows, err := st.Claims().List()
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows = %v, %v; want the table swept", rows, err)
	}

	// The port is free again for a key that probes onto it.
	if err := st.Claims().Put(store.ClaimRow{
		Port: stale.Ports[0], Key: "next/main", Project: "next", Worktree: "main",
		ExpiresAt: clock.Add(time.Hour),
	}); err != nil {
		t.Fatalf("reclaiming the expired port: %v", err)
	}
}

func TestCountShrinksAndGrowsAroundTheSamePorts(t *testing.T) {
	m, _ := newManager(t, nil, nil)
	three := acquire(t, m, claims.Request{Key: "myapp/main", Count: 3})
	if len(three.Ports) != 3 {
		t.Fatalf("ports = %v, want three", three.Ports)
	}

	one := acquire(t, m, claims.Request{Key: "myapp/main", Count: 1})
	if len(one.Ports) != 1 || one.Ports[0] != three.Ports[0] {
		t.Fatalf("ports = %v, want just %d", one.Ports, three.Ports[0])
	}
	live, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 || len(live[0].Ports) != 1 {
		t.Fatalf("claims = %+v, want the surplus ports released", live)
	}

	grown := acquire(t, m, claims.Request{Key: "myapp/main", Count: 3})
	if grown.Ports[0] != three.Ports[0] || len(grown.Ports) != 3 {
		t.Fatalf("ports = %v, want to grow back around %v", grown.Ports, three.Ports)
	}
}

func TestConcurrentAcquiresNeverCollide(t *testing.T) {
	m, _ := newManager(t, nil, nil)

	const keys = 24
	var wg sync.WaitGroup
	results := make([][]int, keys)
	errs := make([]error, keys)
	start := make(chan struct{})
	for i := 0; i < keys; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := m.Acquire(claims.Request{
				Project: "myapp", Worktree: string(rune('a' + i)), Count: 2,
			})
			results[i], errs[i] = res.Ports, err
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[int]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if len(results[i]) != 2 {
			t.Fatalf("worker %d got %v, want two ports", i, results[i])
		}
		for _, p := range results[i] {
			if prev, dup := seen[p]; dup {
				t.Fatalf("workers %d and %d both got port %d", prev, i, p)
			}
			seen[p] = i
		}
	}
}

func TestReleaseFreesThePortsAndIsIdempotent(t *testing.T) {
	m, _ := newManager(t, nil, nil)
	res := acquire(t, m, claims.Request{Key: "myapp/main", Count: 2})

	n, err := m.Release("myapp/main")
	if err != nil || n != 2 {
		t.Fatalf("Release = %d, %v; want 2, nil", n, err)
	}
	n, err = m.Release("myapp/main")
	if err != nil || n != 0 {
		t.Fatalf("second Release = %d, %v; want 0, nil", n, err)
	}
	held, err := m.Held()
	if err != nil || len(held) != 0 {
		t.Fatalf("Held = %v, %v; want empty", held, err)
	}
	// And the ports are handed out again.
	again := acquire(t, m, claims.Request{Key: "myapp/main", Count: 2})
	if again.Ports[0] != res.Ports[0] {
		t.Errorf("after release ports = %v, want %v", again.Ports, res.Ports)
	}
}

func TestAcquireNeedsAProjectOrKey(t *testing.T) {
	m, _ := newManager(t, nil, nil)
	if _, err := m.Acquire(claims.Request{}); !errors.Is(err, claims.ErrNoProject) {
		t.Fatalf("err = %v, want ErrNoProject", err)
	}
}

func TestAFullRangeIsReportedNotSilentlyShort(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "sonar.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	small := claims.Range{Start: 20000, End: 20003}
	listening := map[int]bool{20000: true, 20001: true, 20002: true, 20003: true}
	m := claims.New(st.Claims(), claims.Options{
		Range:     small,
		Listening: func() (map[int]bool, error) { return listening, nil },
	})
	if _, err := m.Acquire(claims.Request{Key: "myapp/main"}); !errors.Is(err, claims.ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
}

func TestAPortTheSameKeyHoldsIsKeptEvenWhileItListens(t *testing.T) {
	key := "myapp/main"
	base := claims.DefaultRange.Base(key)
	listening := map[int]bool{}
	m, _ := newManager(t, listening, nil)

	first := acquire(t, m, claims.Request{Key: key})
	// The caller starts its server on the port it was given; asking again must
	// return the same port rather than stepping past its own listener.
	listening[first.Ports[0]] = true
	second := acquire(t, m, claims.Request{Key: key})
	if second.Ports[0] != base {
		t.Fatalf("ports = %v, want to keep %d", second.Ports, base)
	}
}
