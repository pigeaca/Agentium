-- Registered repositories. `discovery` is the JSON from the last `agentium init` (never credential values).
CREATE TABLE projects (
    id         INTEGER PRIMARY KEY,
    root       TEXT    NOT NULL UNIQUE, -- absolute path with symlinks resolved
    name       TEXT    NOT NULL,
    discovery  TEXT    NOT NULL,
    created_at TEXT    NOT NULL,        -- RFC 3339, UTC
    updated_at TEXT    NOT NULL
);
