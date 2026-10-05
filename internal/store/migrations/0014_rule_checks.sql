-- Rule checks (`agentium check add`): what a project's owner wants agents to have done, such as running the tests or
-- leaving a folder alone. A report counts, per version, the runs that met each check, read again from each run's stored
-- transcript and change when the report is made, so nothing is recorded at run time. Like the other project settings,
-- checks live in the data folder, never in the repository.
CREATE TABLE rule_checks (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('ran', 'changed', 'not-changed')),
    pattern    TEXT    NOT NULL CHECK (pattern <> ''),
    created_at TEXT    NOT NULL,
    UNIQUE (project_id, name)
);
