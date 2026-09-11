package store

import (
	"database/sql"
	"embed"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one forward-only schema step. Every migration runs inside a
// transaction together with the schema_version row that records it, so a
// failed migration leaves the database on the previous version.
//
// schema_version records *every* applied version, one row each, and migrate
// applies every registered version that has no row — not everything above the
// highest one. The distinction matters because the reserved versions 003–006
// are registered by sibling packages (contract §8), so which migrations a
// given binary knows about depends on what it links: a database that reached
// 006 under a build with claims but without sessions must still receive 005
// the first time a build with sessions opens it. Comparing against
// MAX(version) would have skipped it forever.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Reserved version numbers. 001 and 002 ship with this package; 003–006 are
// held for sibling specs (cross-spec contract §8) and are registered by those
// packages from their own init(), not here.
const (
	VersionCore     = 1 // renames, group_pins, port_events + ring trigger
	VersionIndexes  = 2 // history indexes, known_roots
	VersionTunnels  = 3 // reserved: spec 3
	VersionProxies  = 4 // reserved: spec 3
	VersionClaims   = 6 // reserved: spec 2
	VersionSessions = 5 // reserved: spec 2

	VersionGroupAliases = 7 // group_aliases: project names from groups.rename
)

// ReservedVersions maps the version numbers held for other packages to the
// feature that owns them. Registering one of these from outside this package
// is expected; registering anything already taken panics.
var ReservedVersions = map[int]string{
	VersionTunnels:  "tunnels",
	VersionProxies:  "proxies",
	VersionSessions: "sessions",
	VersionClaims:   "claims",
}

var (
	migMu      sync.Mutex
	migrations = map[int]Migration{}
)

// RegisterMigration adds a migration to the global set. It is meant to be
// called from a package init() — including from packages outside this one,
// which is how the reserved 003–006 versions get filled in — so it panics
// rather than returning an error: a duplicate version or an empty statement
// is a programming mistake that must not reach a user's database.
func RegisterMigration(version int, name string, stmts string) {
	if version <= 0 {
		panic(fmt.Sprintf("store: migration version must be positive, got %d", version))
	}
	if strings.TrimSpace(name) == "" {
		panic(fmt.Sprintf("store: migration %03d needs a name", version))
	}
	if strings.TrimSpace(stmts) == "" {
		panic(fmt.Sprintf("store: migration %03d (%s) is empty", version, name))
	}
	migMu.Lock()
	defer migMu.Unlock()
	if prev, ok := migrations[version]; ok {
		panic(fmt.Sprintf("store: migration %03d already registered as %q, refusing %q",
			version, prev.Name, name))
	}
	migrations[version] = Migration{Version: version, Name: name, SQL: stmts}
}

// registeredMigrations returns every registered migration in version order.
func registeredMigrations() []Migration {
	migMu.Lock()
	defer migMu.Unlock()
	out := make([]Migration, 0, len(migrations))
	for _, m := range migrations {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// LatestVersion is the highest registered migration version.
func LatestVersion() int {
	ms := registeredMigrations()
	if len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].Version
}

func init() {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		panic("store: embedded migrations unreadable: " + err.Error())
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			panic("store: " + err.Error())
		}
		body, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			panic("store: reading " + e.Name() + ": " + err.Error())
		}
		RegisterMigration(version, name, string(body))
	}
}

// parseMigrationName splits "002_indexes_roots.sql" into 2 and
// "indexes_roots".
func parseMigrationName(file string) (int, string, error) {
	base := strings.TrimSuffix(file, ".sql")
	num, rest, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("migration %q must be named <version>_<name>.sql", file)
	}
	v, err := strconv.Atoi(num)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q has a non-numeric version: %w", file, err)
	}
	return v, rest, nil
}

const schemaVersionDDL = `
CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TEXT NOT NULL
);`

// migrate brings db up to the newest registered version. It is forward-only:
// a database whose version is higher than anything registered (an older binary
// opening a newer file) is left untouched and reported.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(schemaVersionDDL); err != nil {
		return fmt.Errorf("creating schema_version: %w", err)
	}
	applied, err := appliedVersions(db)
	if err != nil {
		return err
	}
	// registeredMigrations is already in ascending version order, so a gap is
	// filled in the order its author wrote it in.
	for _, m := range registeredMigrations() {
		if applied[m.Version] {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return err
		}
	}
	return nil
}

// appliedVersions is the set of versions this database has already run. A
// version recorded here is never re-applied; one that is registered but
// missing is applied on the next open, wherever it sits in the order.
func appliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_version`)
	if err != nil {
		return nil, fmt.Errorf("reading schema_version: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("reading schema_version: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func applyMigration(db *sql.DB, m Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migration %03d (%s): %w", m.Version, m.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(m.SQL); err != nil {
		return fmt.Errorf("migration %03d (%s): %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_version(version, name, applied_at) VALUES(?, ?, ?)`,
		m.Version, m.Name, nowString(),
	); err != nil {
		return fmt.Errorf("migration %03d (%s): recording version: %w", m.Version, m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %03d (%s): commit: %w", m.Version, m.Name, err)
	}
	return nil
}

// schemaVersion reads the highest applied version, 0 for a fresh database. It
// is what Version() reports; migrate works from the whole applied set, not
// from this number.
func schemaVersion(db *sql.DB) (int, error) {
	var v sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("reading schema_version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// Version reports the schema version of the open database.
func (s *Store) Version() (int, error) { return schemaVersion(s.db) }
