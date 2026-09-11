package store

import "fmt"

// Group aliases are project names set with `groups.rename` for a project that
// has no `sonar.yaml` to write the name into. They are keyed by the root of
// the project's main checkout, so the main checkout and every linked worktree
// of it pick the alias up together.

// SetGroupAlias names the project whose main checkout is at root, replacing any
// earlier alias for it.
func (s *Store) SetGroupAlias(root, name string) error {
	clean, err := cleanRoot(root)
	if err != nil {
		return err
	}
	if err := validName(name); err != nil {
		return err
	}
	return s.exec(
		`INSERT INTO group_aliases(root, name, created_at) VALUES(?, ?, ?)
		 ON CONFLICT(root) DO UPDATE SET name = excluded.name, created_at = excluded.created_at`,
		clean, name, nowString(),
	)
}

// ClearGroupAlias forgets the alias for root. Clearing an absent one is not an
// error.
func (s *Store) ClearGroupAlias(root string) error {
	clean, err := cleanRoot(root)
	if err != nil {
		return err
	}
	return s.exec(`DELETE FROM group_aliases WHERE root = ?`, clean)
}

// GroupAliases returns every alias as main checkout root → project name.
func (s *Store) GroupAliases() (map[string]string, error) {
	out, err := s.allKeyed(`SELECT root, name FROM group_aliases`)
	if err != nil {
		return nil, fmt.Errorf("reading group_aliases: %w", err)
	}
	return out, nil
}
