-- Experiments: the design (arms, tasks, repeats, model, caps, margins, seed) is fixed before the first run. Runs and the
-- lock written before them come with execution.
CREATE TABLE experiments (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    template   TEXT    NOT NULL, -- context-ab or aa
    design     TEXT    NOT NULL, -- JSON
    created_at TEXT    NOT NULL,
    UNIQUE (project_id, name)
);
