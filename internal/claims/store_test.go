package claims_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/store"
)

// This file covers migration 006 and the claims table it creates. It lives
// here rather than in internal/store because the migration is registered by
// this package: a store test binary that does not link claims must not grow
// the table (cross-spec contract §8).

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sonar.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestMigrationTakesTheStoreToVersionSix is the v2 -> v6 step: on this branch
// 003, 004 and 005 are not registered, so the version sequence has to tolerate
// the gap. It does — store.migrate applies every registered migration above
// the applied version — and the gap is recorded in the migration's own doc
// comment: a database that reaches 6 without 005 in the binary would not pick
// 005 up later, which only a single-branch development build can produce.
func TestMigrationTakesTheStoreToVersionSix(t *testing.T) {
	st := openStore(t)

	v, err := st.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	// Later migrations (007, group aliases) may sit above claims' 006.
	if v != store.LatestVersion() {
		t.Fatalf("version = %d, want the latest registered %d", v, store.LatestVersion())
	}

	var applied []int
	rows, err := st.DB().Query(`SELECT version FROM schema_version ORDER BY version`)
	if err != nil {
		t.Fatalf("reading schema_version: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		applied = append(applied, n)
	}
	hasClaims := false
	for _, n := range applied {
		hasClaims = hasClaims || n == store.VersionClaims
	}
	if len(applied) < 3 || !hasClaims {
		t.Fatalf("applied versions = %v, want them to include %d", applied, store.VersionClaims)
	}

	var name string
	if err := st.DB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'claims'`).Scan(&name); err != nil {
		t.Fatalf("the claims table is missing: %v", err)
	}
}

func TestClaimsTableRoundTrip(t *testing.T) {
	c := openStore(t).Claims()
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)

	if err := c.Put(
		store.ClaimRow{Port: 10001, Key: "a/main", Project: "a", Worktree: "main", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		store.ClaimRow{Port: 10002, Key: "a/main", Project: "a", Worktree: "main", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		store.ClaimRow{Port: 20001, Key: "b/main", Project: "b", Worktree: "main", CreatedAt: now, ExpiresAt: now.Add(2 * time.Hour)},
	); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := c.Get("a/main")
	if err != nil || len(got) != 2 || got[0].Port != 10001 || got[1].Port != 10002 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if !got[0].CreatedAt.Equal(now) || !got[0].ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("times did not round trip: %+v", got[0])
	}

	all, err := c.List()
	if err != nil || len(all) != 3 {
		t.Fatalf("List = %+v, %v", all, err)
	}

	n, err := c.Delete("a/main")
	if err != nil || n != 2 {
		t.Fatalf("Delete = %d, %v; want 2", n, err)
	}
	if n, err := c.Delete("nobody/main"); err != nil || n != 0 {
		t.Fatalf("Delete of an unheld key = %d, %v; want 0, nil", n, err)
	}
}

func TestPutRefusesAPortAnotherKeyHoldsAndWritesNothing(t *testing.T) {
	c := openStore(t).Claims()
	now := time.Now()
	if err := c.Put(store.ClaimRow{
		Port: 10001, Key: "a/main", Project: "a", Worktree: "main", ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	err := c.Put(
		store.ClaimRow{Port: 10005, Key: "b/main", Project: "b", Worktree: "main", ExpiresAt: now.Add(time.Hour)},
		store.ClaimRow{Port: 10001, Key: "b/main", Project: "b", Worktree: "main", ExpiresAt: now.Add(time.Hour)},
	)
	if !errors.Is(err, store.ErrClaimed) {
		t.Fatalf("Put over a foreign claim = %v, want ErrClaimed", err)
	}
	rows, err := c.Get("b/main")
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows = %+v, %v; want the whole Put rolled back", rows, err)
	}
}

func TestPutKeepsTheOriginalCreatedAtOnRefresh(t *testing.T) {
	c := openStore(t).Claims()
	first := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)

	row := store.ClaimRow{Port: 10001, Key: "a/main", Project: "a", Worktree: "main", CreatedAt: first, ExpiresAt: first.Add(time.Hour)}
	if err := c.Put(row); err != nil {
		t.Fatalf("Put: %v", err)
	}
	row.CreatedAt, row.ExpiresAt = later, later.Add(time.Hour)
	if err := c.Put(row); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got, err := c.Get("a/main")
	if err != nil || len(got) != 1 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if !got[0].CreatedAt.Equal(first) {
		t.Errorf("created_at = %v, want the original %v", got[0].CreatedAt, first)
	}
	if !got[0].ExpiresAt.Equal(later.Add(time.Hour)) {
		t.Errorf("expires_at = %v, want the refreshed one", got[0].ExpiresAt)
	}
}

func TestExpireDropsOnlyTheClaimsThatRanOut(t *testing.T) {
	c := openStore(t).Claims()
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	if err := c.Put(
		store.ClaimRow{Port: 10001, Key: "gone/main", Project: "gone", Worktree: "main", CreatedAt: now, ExpiresAt: now.Add(time.Minute)},
		store.ClaimRow{Port: 10002, Key: "live/main", Project: "live", Worktree: "main", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	); err != nil {
		t.Fatalf("Put: %v", err)
	}

	n, err := c.Expire(now.Add(2 * time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("Expire = %d, %v; want 1", n, err)
	}
	rows, err := c.List()
	if err != nil || len(rows) != 1 || rows[0].Key != "live/main" {
		t.Fatalf("rows = %+v, %v; want only the live claim", rows, err)
	}
}

func TestPutRejectsNonsense(t *testing.T) {
	c := openStore(t).Claims()
	if err := c.Put(store.ClaimRow{Port: 0, Key: "a/main"}); err == nil {
		t.Error("port 0 was accepted")
	}
	if err := c.Put(store.ClaimRow{Port: 10001, Key: "  "}); err == nil {
		t.Error("an empty key was accepted")
	}
	if err := c.Put(); err != nil {
		t.Errorf("Put with no rows = %v, want nil", err)
	}
}
