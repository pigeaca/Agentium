-- Agent runs. The full record (metrics, behavior, drift, verification) is JSON; the columns are what listings and
-- experiments filter on. Transcripts, diffs and logs live in the data folder's records/<id>.
CREATE TABLE runs (
    id          TEXT    PRIMARY KEY,                                  -- time-ordered, also the records folder name
    project_id  INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    task_id     INTEGER REFERENCES tasks (id) ON DELETE SET NULL,
    task_name   TEXT    NOT NULL,
    arm         TEXT    NOT NULL,
    outcome     TEXT    NOT NULL,                                     -- ok, capped, timeout, infra, unfair
    passed      INTEGER,                                              -- NULL when the verification did not run
    cost_usd    REAL    NOT NULL DEFAULT 0,
    record      TEXT    NOT NULL,
    started_at  TEXT    NOT NULL,
    finished_at TEXT    NOT NULL
);
CREATE INDEX runs_by_project ON runs (project_id, started_at);
