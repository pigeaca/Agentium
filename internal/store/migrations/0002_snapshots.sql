-- Named versions of a project's context. The files live in the project's bare repository (commit_id); the manifest
-- is the JSON summary (paths, kinds, sizes, SHA-256).
CREATE TABLE snapshots (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name          TEXT    NOT NULL,
    source        TEXT    NOT NULL, -- "working tree" or the ref it was read from
    source_commit TEXT    NOT NULL, -- the user's commit it was read from (HEAD for the working tree)
    commit_id     TEXT    NOT NULL,
    manifest      TEXT    NOT NULL,
    created_at    TEXT    NOT NULL,
    UNIQUE (project_id, name)
);
