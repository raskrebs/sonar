package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/raskrebs/sonar/internal/state"
)

// ErrInvalidName is returned when a rename or group name is empty or holds a
// separator. Same rule as sonar.yaml service names in the daemon spec.
var ErrInvalidName = errors.New("name must be non-empty and free of whitespace, / and \\")

func validName(name string) error {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, " \t\n\r/\\") {
		return fmt.Errorf("%q: %w", name, ErrInvalidName)
	}
	return nil
}

// GetRename returns the rename stored under key.
func (s *Store) GetRename(key string) (string, bool, error) {
	return s.getKeyed("renames", "name", key)
}

// SetRename stores name under key, replacing any earlier rename for it.
func (s *Store) SetRename(key, name string) error {
	if key == "" {
		return errors.New("store: rename needs a match key")
	}
	if err := validName(name); err != nil {
		return err
	}
	return s.exec(
		`INSERT INTO renames(key, name, created_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET name = excluded.name, created_at = excluded.created_at`,
		key, name, nowString(),
	)
}

// ClearRename removes the rename for key. Clearing an absent key is not an
// error — `sonar rename 3000 --clear` is idempotent.
func (s *Store) ClearRename(key string) error {
	return s.exec(`DELETE FROM renames WHERE key = ?`, key)
}

// Renames returns every stored rename as key→name.
func (s *Store) Renames() (map[string]string, error) {
	return s.allKeyed(`SELECT key, name FROM renames`)
}

// ResolveRename finds the rename that applies to p, trying every match key in
// order of specificity.
func (s *Store) ResolveRename(p state.Port) (string, bool, error) {
	return s.resolve("renames", "name", p)
}

// getKeyed reads one column of one row from a key-addressed table.
func (s *Store) getKeyed(table, column, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(
		fmt.Sprintf(`SELECT %s FROM %s WHERE key = ?`, column, table), key,
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", table, err)
	}
	return v, true, nil
}

// resolve looks p up under each of its match keys, most specific first.
func (s *Store) resolve(table, column string, p state.Port) (string, bool, error) {
	keys := MatchKeys(p)
	args := make([]any, len(keys))
	placeholders := make([]string, len(keys))
	for i, k := range keys {
		args[i] = k
		placeholders[i] = "?"
	}
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT key, %s FROM %s WHERE key IN (%s)`,
		column, table, strings.Join(placeholders, ","),
	), args...)
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	found := make(map[string]string, len(keys))
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return "", false, fmt.Errorf("reading %s: %w", table, err)
		}
		found[k] = v
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("reading %s: %w", table, err)
	}
	for _, k := range keys {
		if v, ok := found[k]; ok {
			return v, true, nil
		}
	}
	return "", false, nil
}

func (s *Store) allKeyed(query string) (map[string]string, error) {
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
