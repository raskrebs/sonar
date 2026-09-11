// Package store is the daemon's SQLite database: per-machine renames, group
// pins, the port-event history ring and the set of known sonar.yaml roots.
//
// The database lives next to config.yaml (~/.config/sonar/sonar.db) and is
// created on first use. It is driven by modernc.org/sqlite, a pure-Go driver,
// so CGO_ENABLED=0 release builds keep working.
//
// Only the daemon opens the store. The CLI's no-daemon path never touches it.
//
// Migrations are tracked per version, not by a high-water mark: schema_version
// holds one row per applied migration and Open applies every registered
// version that has no row. Versions 003-006 are reserved for sibling packages
// (contract §8), so which of them a binary knows about depends on what it
// links; a database that reached 006 without 005 registered still receives 005
// the first time a build that owns it opens the file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/config"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Time is stored as fixed-width UTC RFC 3339 text: nanoseconds are always
// nine digits so string comparison is chronological and `since` filters can be
// pushed into SQL.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func nowString() string { return timeToString(time.Now()) }

func timeToString(t time.Time) string { return t.UTC().Format(timeLayout) }

func timeFromString(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// ResetInfo describes a database that had to be thrown away because the file
// on disk was not a usable SQLite database. The daemon turns this into a
// state.event{kind:"db_reset"}.
type ResetInfo struct {
	// Path is the database that was reset.
	Path string
	// MovedTo is where the unusable file was parked.
	MovedTo string
	// Reason is the driver error that triggered the reset.
	Reason string
}

// Options tunes Open. The zero value is what Open uses.
type Options struct {
	// OnReset, when set, is called after a corrupt database has been moved
	// aside and recreated, before Open returns.
	OnReset func(ResetInfo)
	// BusyTimeout is how long a statement waits for a competing writer.
	// Zero means five seconds.
	BusyTimeout time.Duration
}

// Store is an open sonar database. It is safe for concurrent use: reads run
// on the connection pool (WAL lets them proceed while a write is in flight)
// and writes are serialised behind a mutex so SQLITE_BUSY cannot surface as a
// user-visible error.
type Store struct {
	db   *sql.DB
	path string

	wmu sync.Mutex

	reset *ResetInfo
}

// Path is the default database location: sonar.db next to config.yaml, i.e.
// ~/.config/sonar/sonar.db. SONAR_DB overrides it, which is what the tests and
// `sonar serve` with a temp HOME use.
func Path() string {
	if p := strings.TrimSpace(os.Getenv("SONAR_DB")); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(config.Path()), "sonar.db")
}

// Open opens (creating if needed) the database at path and migrates it to the
// newest registered schema version. An empty path means Path().
//
// A file that is not a usable SQLite database is moved aside to
// <path>.corrupt-<timestamp> and recreated from scratch; Open succeeds and
// ResetHappened reports it.
func Open(path string) (*Store, error) { return OpenWith(path, Options{}) }

// OpenWith is Open with options.
func OpenWith(path string, opts Options) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		path = Path()
	}
	if err := prepareFile(path); err != nil {
		return nil, err
	}

	s, err := openAt(path, opts)
	if err == nil {
		return s, nil
	}
	if !isCorruption(err) {
		return nil, err
	}

	movedTo, mvErr := moveAside(path)
	if mvErr != nil {
		return nil, fmt.Errorf("%s is unusable (%v) and could not be moved aside: %w", path, err, mvErr)
	}
	if err := prepareFile(path); err != nil {
		return nil, err
	}
	fresh, freshErr := openAt(path, opts)
	if freshErr != nil {
		return nil, fmt.Errorf("recreating %s after moving the unusable file to %s: %w", path, movedTo, freshErr)
	}
	fresh.reset = &ResetInfo{Path: path, MovedTo: movedTo, Reason: err.Error()}
	if opts.OnReset != nil {
		opts.OnReset(*fresh.reset)
	}
	return fresh, nil
}

// openAt does the actual open + migrate, with no corruption recovery.
func openAt(path string, opts Options) (*Store, error) {
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = 5 * time.Second
	}
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)",
		path, busy.Milliseconds(),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// A handful of pooled connections is plenty: writes are serialised in
	// Go and readers are short.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := &Store{db: db, path: path}

	// Touch the schema so a file that is not a database fails here, where
	// the caller can still recover, rather than on the first query.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		_ = db.Close()
		return nil, fmt.Errorf("securing %s: %w", path, err)
	}
	return s, nil
}

// prepareFile makes sure the parent directory exists (0700) and that the
// database file, when it has to be created, is created 0600 rather than with
// whatever the process umask would give it.
func prepareFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil // already there; SQLite takes it from here
		}
		return fmt.Errorf("creating %s: %w", path, err)
	}
	// A zero-length file is a valid empty SQLite database.
	return f.Close()
}

// moveAside parks an unusable database (and its WAL sidecars) next to itself
// and returns the new path of the main file.
func moveAside(path string) (string, error) {
	stamp := time.Now().UTC().Format("20060102-150405")
	target := fmt.Sprintf("%s.corrupt-%s", path, stamp)
	for i := 1; ; i++ {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			break
		}
		target = fmt.Sprintf("%s.corrupt-%s-%d", path, stamp, i)
	}
	if err := os.Rename(path, target); err != nil {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Rename(path+suffix, target+suffix)
	}
	return target, nil
}

// isCorruption reports whether err means "this file is not a database sonar
// can use" — the only failure Open recovers from by starting over. Disk
// permissions, a full disk or a locked file are all propagated instead.
func isCorruption(err error) bool {
	if err == nil {
		return false
	}
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() & 0xff {
		case sqlite3.SQLITE_NOTADB, sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_FORMAT:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "file is not a database") ||
		strings.Contains(msg, "file is encrypted or is not a database") ||
		strings.Contains(msg, "database disk image is malformed") ||
		strings.Contains(msg, "malformed database schema")
}

// Close releases the database, leaving one file behind rather than three.
//
// WAL keeps a `-wal` and a `-shm` beside the database, and they are written
// again by whichever pooled connection closes last — after Close has returned,
// as far as the caller can tell. A daemon that has stopped, or a test whose
// temp directory is being removed, has no way to wait for that: the removal
// fails with "directory not empty" against a `-wal` recreated behind it.
// Checkpointing and leaving WAL mode deletes both sidecars while we still hold
// the connection that owns them. A busy database refuses the switch; that is
// the state we were already in, so the error is not worth reporting.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	// Drop the idle pool: journal_mode can only change when one connection is
	// left to change it.
	s.db.SetMaxIdleConns(0)
	s.db.SetMaxOpenConns(1)
	if conn, err := s.db.Conn(context.Background()); err == nil {
		_, _ = conn.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`)
		_, _ = conn.ExecContext(context.Background(), `PRAGMA journal_mode(DELETE)`)
		_ = conn.Close()
	}
	return s.db.Close()
}

// DBPath is the file this store is backed by.
func (s *Store) DBPath() string { return s.path }

// DB exposes the raw handle. The cross-spec contract hands this to daemon
// extensions through Runtime; nothing inside sonar should reach for it.
func (s *Store) DB() *sql.DB { return s.db }

// ResetHappened reports whether Open had to throw the previous database away.
// The daemon calls it right after Open and emits state.event{kind:"db_reset"}.
func (s *Store) ResetHappened() bool { return s != nil && s.reset != nil }

// Reset returns the details of that reset, or nil if none happened.
func (s *Store) Reset() *ResetInfo {
	if s == nil || s.reset == nil {
		return nil
	}
	cp := *s.reset
	return &cp
}

// exec runs one write statement with the writer lock held.
func (s *Store) exec(query string, args ...any) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(query, args...)
	return err
}
