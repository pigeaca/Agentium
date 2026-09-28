-- Coding tasks. Commits live in the project's bare repository. With a solution commit, its test-file changes are the
-- hidden tests and its other changes the reference solution; list columns are JSON arrays of paths or commands.
CREATE TABLE tasks (
    id              INTEGER PRIMARY KEY,
    project_id      INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    instruction     TEXT    NOT NULL,
    source          TEXT    NOT NULL,            -- "manual", "commit <sha>" or "pr #<n>"
    base_commit     TEXT    NOT NULL,
    solution_commit TEXT    NOT NULL DEFAULT '', -- empty: no hidden tests or reference
    hidden_tests    TEXT    NOT NULL DEFAULT '[]',
    reference_files TEXT    NOT NULL DEFAULT '[]',
    setup           TEXT    NOT NULL DEFAULT '[]', -- commands a fresh checkout needs first (e.g. build embedded assets)
    verify          TEXT    NOT NULL,
    needs_review    INTEGER NOT NULL DEFAULT 0,  -- 1: the instruction came from history and may leak the solution
    validation      TEXT    NOT NULL DEFAULT '', -- JSON of the last `task validate`, empty until then
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    UNIQUE (project_id, name)
);
