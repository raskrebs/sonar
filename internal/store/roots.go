package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Known roots are the directories where a sonar.yaml has been seen. The
// groups resolver rebuilds its index from them on daemon start instead of
// walking the filesystem, and the config watcher watches them.

// AddRoot records a directory as a known sonar.yaml root. Paths are cleaned
// and made absolute, and adding one twice is a no-op.
func (s *Store) AddRoot(path string) error {
	clean, err := cleanRoot(path)
	if err != nil {
		return err
	}
	return s.exec(
		`INSERT INTO known_roots(path, added_at) VALUES(?, ?)
		 ON CONFLICT(path) DO NOTHING`,
		clean, nowString(),
	)
}

// Roots returns every known root, sorted.
func (s *Store) Roots() ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM known_roots ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("reading known_roots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("reading known_roots: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RemoveRoot forgets a root. Removing an unknown root is not an error.
func (s *Store) RemoveRoot(path string) error {
	clean, err := cleanRoot(path)
	if err != nil {
		return err
	}
	return s.exec(`DELETE FROM known_roots WHERE path = ?`, clean)
}

func cleanRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("store: root path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("store: resolving root %q: %w", path, err)
	}
	return filepath.Clean(abs), nil
}
