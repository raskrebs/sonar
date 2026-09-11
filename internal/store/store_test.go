package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// openTemp opens a store in a fresh temp directory and closes it on cleanup.
func openTemp(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sonar.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "sonar.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.DBPath() != path {
		t.Errorf("DBPath = %q, want %q", s.DBPath(), path)
	}
	if s.ResetHappened() {
		t.Error("fresh database reported a reset")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("database mode = %o, want 600", perm)
		}
	}

	var mode string
	if err := s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	var busy int
	if err := s.DB().QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busy <= 0 {
		t.Errorf("busy_timeout = %d, want a positive timeout", busy)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonar.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.SetRename("port:3000", "api"); err != nil {
		t.Fatalf("SetRename: %v", err)
	}
	v1, err := first.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()

	v2, err := second.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v1 != v2 {
		t.Errorf("version changed on reopen: %d -> %d", v1, v2)
	}
	if name, ok, err := second.GetRename("port:3000"); err != nil || !ok || name != "api" {
		t.Errorf("GetRename after reopen = %q, %v, %v; want api, true, nil", name, ok, err)
	}

	// Each migration is recorded exactly once, never re-applied. Versions
	// have gaps (003 and 004 are reserved), so count against the registered
	// set rather than the highest number.
	var rows int
	if err := second.DB().QueryRow(`SELECT count(*) FROM schema_version`).Scan(&rows); err != nil {
		t.Fatalf("counting schema_version: %v", err)
	}
	if want := len(registeredMigrations()); rows != want {
		t.Errorf("schema_version has %d rows, want one per registered migration (%d)", rows, want)
	}
}

func TestPathHonoursEnvOverride(t *testing.T) {
	t.Setenv("SONAR_DB", "/tmp/custom/sonar.db")
	if got := Path(); got != "/tmp/custom/sonar.db" {
		t.Errorf("Path() = %q, want the SONAR_DB override", got)
	}

	t.Setenv("SONAR_DB", "")
	got := Path()
	if filepath.Base(got) != "sonar.db" {
		t.Errorf("Path() = %q, want it to end in sonar.db", got)
	}
	if want := filepath.Join("sonar", "sonar.db"); !strings.HasSuffix(got, want) {
		t.Errorf("Path() = %q, want it inside the sonar config dir", got)
	}
}

func TestCorruptDatabaseIsMovedAsideAndReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sonar.db")

	junk := []byte(strings.Repeat("this is definitely not a sqlite database\n", 200))
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatalf("seeding a corrupt file: %v", err)
	}

	var seen []ResetInfo
	s, err := OpenWith(path, Options{OnReset: func(i ResetInfo) { seen = append(seen, i) }})
	if err != nil {
		t.Fatalf("OpenWith on a corrupt file: %v", err)
	}
	defer func() { _ = s.Close() }()

	if !s.ResetHappened() {
		t.Fatal("ResetHappened = false after opening a corrupt file")
	}
	if len(seen) != 1 {
		t.Fatalf("OnReset called %d times, want 1", len(seen))
	}
	info := s.Reset()
	if info == nil || info.MovedTo == "" {
		t.Fatal("Reset() carries no move-aside path")
	}
	if seen[0].MovedTo != info.MovedTo {
		t.Errorf("OnReset path %q != Reset() path %q", seen[0].MovedTo, info.MovedTo)
	}
	if !strings.HasPrefix(filepath.Base(info.MovedTo), "sonar.db.corrupt-") {
		t.Errorf("moved-aside name %q, want sonar.db.corrupt-<timestamp>", filepath.Base(info.MovedTo))
	}
	if info.Reason == "" {
		t.Error("Reset().Reason is empty")
	}

	parked, err := os.ReadFile(info.MovedTo)
	if err != nil {
		t.Fatalf("reading the parked file: %v", err)
	}
	if string(parked) != string(junk) {
		t.Error("the parked file is not the original bytes")
	}

	// The recreated database is fully usable.
	if err := s.SetRename("port:3000", "api"); err != nil {
		t.Fatalf("SetRename on the recreated database: %v", err)
	}
	if v, err := s.Version(); err != nil || v != LatestVersion() {
		t.Errorf("version after reset = %d (%v), want %d", v, err, LatestVersion())
	}
}

func TestOpenPropagatesNonCorruptionErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not gate file creation on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := Open(filepath.Join(dir, "sonar.db")); err == nil {
		t.Fatal("Open into an unwritable directory succeeded, want an error")
	}
}

func TestIsCorruptionIgnoresOrdinaryErrors(t *testing.T) {
	if isCorruption(nil) {
		t.Error("nil counted as corruption")
	}
	if isCorruption(os.ErrPermission) {
		t.Error("a permission error counted as corruption")
	}
	if isCorruption(sql.ErrNoRows) {
		t.Error("sql.ErrNoRows counted as corruption")
	}
}

// TestCloseLeavesNoWALSidecars: a closed database has to be one file. The WAL
// and shared-memory sidecars are rewritten by whichever pooled connection
// closes last, so a caller that closes the store and then removes the
// directory — a stopping daemon, a test's t.TempDir cleanup — otherwise races
// a `-wal` recreated behind the delete and fails with "directory not empty".
func TestCloseLeavesNoWALSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sonar.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Write something, so there is a WAL to leave behind.
	if err := s.SetRename("port:8123", "storefront"); err != nil {
		t.Fatalf("SetRename: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-wal") || strings.HasSuffix(e.Name(), "-shm") {
			t.Errorf("%s survived Close; the database should be a single file", e.Name())
		}
	}

	// The data is still there: leaving WAL must checkpoint, not discard.
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = again.Close() }()
	if name, _, err := again.GetRename("port:8123"); err != nil || name != "storefront" {
		t.Errorf("rename after reopen = %q (%v), want storefront", name, err)
	}
}
