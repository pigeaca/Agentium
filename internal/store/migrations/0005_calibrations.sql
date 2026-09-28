-- Calibration runs: what a clean run of each arm's context looks like (CLI version, model, tools, skills, slash
-- commands) and how its first request compares with Agentium's estimate. Runs check against the latest one per arm.
CREATE TABLE calibrations (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    arm        TEXT    NOT NULL,
    snapshot   TEXT    NOT NULL DEFAULT '', -- the snapshot commit; empty for the base's own context
    run_id     TEXT    NOT NULL,
    result     TEXT    NOT NULL,            -- JSON
    created_at TEXT    NOT NULL
);
CREATE INDEX calibrations_by_arm ON calibrations (project_id, arm, created_at);

-- Calibration runs are stored as runs, so their spend shows; kind keeps them out of task results.
ALTER TABLE runs ADD COLUMN kind TEXT NOT NULL DEFAULT 'task'; -- task or calibration
