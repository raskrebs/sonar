-- 007_group_aliases.sql — project names set with groups.rename (step 5A.6).
--
-- A project with a .sonar.yaml is renamed by writing name: into that file.
-- One without has nowhere to keep a new name, so it is kept here, keyed by the
-- root of the main checkout every one of its worktrees points back to.

CREATE TABLE IF NOT EXISTS group_aliases (
    root       TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL
);
