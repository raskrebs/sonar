package sessions_test

// The sessions table and its migration. These live here rather than in
// internal/store because migration 005 is registered from internal/sessions:
// the store holds the version open (contract §8) and the owning package fills
// it in, so only a package that links this one has the table.

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/sessions"
	"github.com/raskrebs/sonar/internal/store"

	_ "modernc.org/sqlite"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "sonar.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func row(id string, seen time.Time) store.SessionRow {
	return store.SessionRow{
		ID: id, Tool: sessions.ToolClaudeCode, Label: "build the thing",
		Worktree: "feature-x", Branch: "feature/x", Detected: true,
		FirstSeen: seen, LastSeen: seen,
	}
}

// applied reports whether the database recorded one migration version. Later
// migrations (007, group aliases) sit above 005, so the highest version alone
// no longer says whether 005 ran.
func applied(t *testing.T, s *store.Store, version int) bool {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM schema_version WHERE version = ?`, version).Scan(&n); err != nil {
		t.Fatalf("reading schema_version: %v", err)
	}
	return n == 1
}

func TestMigration005IsTheReservedVersion(t *testing.T) {
	s := openStore(t)
	v, err := s.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != store.LatestVersion() || !applied(t, s, store.VersionSessions) {
		t.Errorf("version = %d, want %d with the reserved sessions version %d applied",
			v, store.LatestVersion(), store.VersionSessions)
	}
	var n int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE name IN ('sessions','idx_sessions_last_seen')`,
	).Scan(&n); err != nil {
		t.Fatalf("querying sqlite_master: %v", err)
	}
	if n != 2 {
		t.Errorf("found %d of the two objects migration 005 creates", n)
	}
}

// TestMigrateFromV2 replays a database written by a sonar that had only
// migrations 001 and 002 and checks that 005 lands on top without losing rows.
func TestMigrateFromV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonar.db")

	fixture, err := os.ReadFile(filepath.Join("testdata", "v2_schema.sql"))
	if err != nil {
		t.Fatalf("reading the v2 fixture: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("creating the v2 database: %v", err)
	}
	if _, err := raw.Exec(string(fixture)); err != nil {
		t.Fatalf("applying the v2 fixture: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing the v2 database: %v", err)
	}

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open on a v2 database: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.ResetHappened() {
		t.Fatal("a valid v2 database was treated as corrupt")
	}
	if v, err := s.Version(); err != nil || v != store.LatestVersion() || !applied(t, s, store.VersionSessions) {
		t.Fatalf("version after upgrade = %d (%v), want %d with %d applied",
			v, err, store.LatestVersion(), store.VersionSessions)
	}
	if name, ok, err := s.GetRename("cwd:/home/me/code/shop:3000"); err != nil || !ok || name != "storefront" {
		t.Errorf("the v2 rename did not survive: %q, %v, %v", name, ok, err)
	}
	roots, err := s.Roots()
	if err != nil || len(roots) != 1 {
		t.Errorf("the v2 known_roots row did not survive: %v, %v", roots, err)
	}
	if err := s.Sessions().Upsert(row("s1", time.Now())); err != nil {
		t.Errorf("writing to the upgraded sessions table: %v", err)
	}
}

func TestSessionsUpsertTouchAndList(t *testing.T) {
	s := openStore(t)
	table := s.Sessions()

	first := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := table.Upsert(row("s1", first)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// A second upsert refreshes the mutable half and moves last_seen, but
	// first_seen is when this session first started something.
	later := first.Add(2 * time.Hour)
	updated := row("s1", later)
	updated.Branch = "main"
	updated.Detected = false
	if err := table.Upsert(updated); err != nil {
		t.Fatalf("Upsert again: %v", err)
	}

	rows, err := table.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	if !got.FirstSeen.Equal(first) {
		t.Errorf("first_seen = %v, want the original %v", got.FirstSeen, first)
	}
	if !got.LastSeen.Equal(later) {
		t.Errorf("last_seen = %v, want %v", got.LastSeen, later)
	}
	if got.Branch != "main" || got.Detected {
		t.Errorf("the second upsert did not refresh the row: %+v", got)
	}
	if got.Tool != sessions.ToolClaudeCode || got.Worktree != "feature-x" || got.Label == "" {
		t.Errorf("round trip lost fields: %+v", got)
	}

	touched := later.Add(time.Hour)
	if err := table.Touch("s1", touched); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	rows, _ = table.List()
	if !rows[0].LastSeen.Equal(touched) {
		t.Errorf("Touch left last_seen at %v", rows[0].LastSeen)
	}

	// Touching a session nobody recorded creates nothing.
	if err := table.Touch("ghost", touched); err != nil {
		t.Fatalf("Touch on an unknown session: %v", err)
	}
	if rows, _ := table.List(); len(rows) != 1 {
		t.Errorf("Touch created a row: %+v", rows)
	}

	if err := table.Upsert(store.SessionRow{}); err == nil {
		t.Error("an id-less session was accepted")
	}
}

func TestSessionsListIsNewestFirstAndGetFindsOne(t *testing.T) {
	s := openStore(t)
	table := s.Sessions()
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	for i, id := range []string{"old", "new"} {
		if err := table.Upsert(row(id, base.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := table.List()
	if err != nil || len(rows) != 2 {
		t.Fatalf("List = %v, %v", rows, err)
	}
	if rows[0].ID != "new" {
		t.Errorf("List order = %q, %q; want the newest first", rows[0].ID, rows[1].ID)
	}

	got, ok, err := table.Get("old")
	if err != nil || !ok || got.ID != "old" {
		t.Errorf("Get(old) = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := table.Get("missing"); ok {
		t.Error("Get found a session that was never recorded")
	}
}

func TestSessionsPruneDropsOnlyTheOldOnes(t *testing.T) {
	s := openStore(t)
	table := s.Sessions()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	if err := table.Upsert(row("stale", now.Add(-8*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := table.Upsert(row("recent", now.Add(-6*24*time.Hour))); err != nil {
		t.Fatal(err)
	}

	n, err := table.Prune(now.Add(-sessions.Retention))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("Prune removed %d rows, want 1", n)
	}
	rows, _ := table.List()
	if len(rows) != 1 || rows[0].ID != "recent" {
		t.Errorf("after prune: %+v", rows)
	}

	// Retention is the seven days spec 2 §3 promises.
	if sessions.Retention != 7*24*time.Hour {
		t.Errorf("Retention = %v, want 7 days", sessions.Retention)
	}
}

// A database that reached version 006 under a build with claims but without
// sessions still gets migration 005 the first time a build that owns sessions
// opens it. This is why the store applies every registered version that has no
// schema_version row rather than everything above the highest one.
func TestMigration005LandsOnADatabaseAlreadyAtV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonar.db")

	fixture, err := os.ReadFile(filepath.Join("testdata", "v2_schema.sql"))
	if err != nil {
		t.Fatalf("reading the v2 fixture: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("creating the database: %v", err)
	}
	if _, err := raw.Exec(string(fixture)); err != nil {
		t.Fatalf("applying the v2 fixture: %v", err)
	}
	// A sibling package's migration 006, applied by a build that never had
	// 005 linked in.
	if _, err := raw.Exec(
		`CREATE TABLE claims (port INTEGER PRIMARY KEY, key TEXT NOT NULL);
		 INSERT INTO schema_version(version, name, applied_at)
		 VALUES (6, 'claims', '2026-09-05T00:00:00.000000000Z');`); err != nil {
		t.Fatalf("faking migration 006: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open on a v6 database: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.ResetHappened() {
		t.Fatal("a valid database was treated as corrupt")
	}
	if err := s.Sessions().Upsert(row("s1", time.Now())); err != nil {
		t.Fatalf("migration 005 did not run on a v6 database: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM schema_version WHERE version = ?`, store.VersionSessions).Scan(&n); err != nil {
		t.Fatalf("counting the 005 row: %v", err)
	}
	if n != 1 {
		t.Errorf("schema_version has %d rows for version %d, want 1", n, store.VersionSessions)
	}
	// The claims table the other build created is untouched.
	if _, err := s.DB().Exec(`INSERT INTO claims(port, key) VALUES(3000, 'shop/main')`); err != nil {
		t.Errorf("the sibling package's table did not survive: %v", err)
	}
}
