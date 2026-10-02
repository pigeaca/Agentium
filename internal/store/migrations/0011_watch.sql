-- The watch (`agentium watch`) and the cost screen: the user's consent and each project's loops, enrolment, passes,
-- drift charts and screen checks. Nothing here counts spend: the ledger (internal/watch) is derived from the runs a
-- pass made (watch_pass_id) and their records, so a crash between a run and any bookkeeping cannot double-count it.

-- The user's consent to the watch's spend, for the whole data folder: one budget for all projects together, since the
-- subscription's windows belong to one login. Append-only (no UPDATE, no DELETE), so every grant, lowering and
-- revocation is kept with who made it. The newest row is the consent in force; no row, or a newest row with
-- enabled = 0, means the watch spends nothing. Shares and thresholds are fractions (0.3 is 30%): pass_share of one
-- five-hour window per pass, weekly_share of the seven-day window over 7 days, and start_five_hour and start_seven_day
-- the readings above which no pair starts. sign_in_identity is a fingerprint that never derives from a secret (for a
-- token file, its path and inode; empty where none is available). confirmed marks a row the user confirmed at a
-- terminal (watch.TerminalConfirmation).
CREATE TABLE watch_consents (
    id               INTEGER PRIMARY KEY,
    enabled          INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    weekly_usd       REAL    NOT NULL CHECK (weekly_usd >= 0),
    run_cap_usd      REAL    NOT NULL CHECK (run_cap_usd >= 0),
    pass_share       REAL    NOT NULL CHECK (pass_share >= 0 AND pass_share <= 1),
    weekly_share     REAL    NOT NULL CHECK (weekly_share >= 0 AND weekly_share <= 1),
    start_five_hour  REAL    NOT NULL CHECK (start_five_hour >= 0 AND start_five_hour <= 1),
    start_seven_day  REAL    NOT NULL CHECK (start_seven_day >= 0 AND start_seven_day <= 1),
    sign_in          TEXT    NOT NULL CHECK (sign_in IN ('login', 'token-file', 'api-key')),
    sign_in_identity TEXT    NOT NULL DEFAULT '',
    confirmed        INTEGER NOT NULL CHECK (confirmed IN (0, 1)),
    granted_by       TEXT    NOT NULL CHECK (granted_by <> ''), -- the OS user
    agentium_version TEXT    NOT NULL,
    granted_at       TEXT    NOT NULL
);
CREATE TRIGGER watch_consents_no_update BEFORE UPDATE ON watch_consents
BEGIN
    SELECT RAISE(ABORT, 'watch consents are append-only');
END;
CREATE TRIGGER watch_consents_no_delete BEFORE DELETE ON watch_consents
BEGIN
    SELECT RAISE(ABORT, 'watch consents are append-only');
END;
-- Only the terminal raises: an unconfirmed row may only revoke, or keep every cap and threshold at or below the
-- consent in force, with the same sign-in mode and identity. internal/watch refuses the same with a clearer message;
-- this holds for any other writer.
CREATE TRIGGER watch_consents_raise BEFORE INSERT ON watch_consents
WHEN NEW.confirmed = 0 AND NEW.enabled = 1 AND NOT EXISTS (
    SELECT 1 FROM watch_consents AS p
    WHERE p.id = (SELECT MAX(id) FROM watch_consents)
      AND p.enabled = 1 AND p.sign_in = NEW.sign_in AND p.sign_in_identity = NEW.sign_in_identity
      AND NEW.weekly_usd <= p.weekly_usd AND NEW.run_cap_usd <= p.run_cap_usd
      AND NEW.pass_share <= p.pass_share AND NEW.weekly_share <= p.weekly_share
      AND NEW.start_five_hour <= p.start_five_hour AND NEW.start_seven_day <= p.start_seven_day)
BEGIN
    SELECT RAISE(ABORT, 'watch consent raise: only agentium watch enable at a terminal raises it');
END;

-- What the watch may do in each project (its loops), within the one consent above. No row: nothing. Enabling a loop
-- needs a confirmed row, as raising a cap does; turning one off does not.
CREATE TABLE watch_loops (
    project_id  INTEGER PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    experiments INTEGER NOT NULL CHECK (experiments IN (0, 1)), -- continue enrolled experiments
    drift       INTEGER NOT NULL CHECK (drift IN (0, 1)),       -- drift checks
    screens     INTEGER NOT NULL CHECK (screens IN (0, 1)),     -- queued cost screens
    confirmed   INTEGER NOT NULL CHECK (confirmed IN (0, 1)),
    set_by      TEXT    NOT NULL CHECK (set_by <> ''),
    set_at      TEXT    NOT NULL
);
CREATE TRIGGER watch_loops_enable BEFORE INSERT ON watch_loops
WHEN NEW.confirmed = 0 AND NEW.experiments + NEW.drift + NEW.screens > 0
BEGIN
    SELECT RAISE(ABORT, 'watch consent raise: only agentium watch enable at a terminal enables a loop');
END;
CREATE TRIGGER watch_loops_raise BEFORE UPDATE ON watch_loops
WHEN NEW.confirmed = 0 AND (NEW.experiments > OLD.experiments OR NEW.drift > OLD.drift OR NEW.screens > OLD.screens)
BEGIN
    SELECT RAISE(ABORT, 'watch consent raise: only agentium watch enable at a terminal enables a loop');
END;

-- Watch passes: one at a time (watch.lock). started_by is "schedule" (launchd) or "terminal" (a screen run by hand,
-- whose spend counts in the weekly caps too). A pass whose process died keeps finished_at empty until the next pass
-- closes it.
CREATE TABLE watch_passes (
    id               INTEGER PRIMARY KEY,
    started_by       TEXT    NOT NULL,
    agentium_version TEXT    NOT NULL,
    started_at       TEXT    NOT NULL,
    finished_at      TEXT    NOT NULL DEFAULT '',
    stop_reason      TEXT    NOT NULL DEFAULT ''
);

-- Screen checks: one per pushed head commit. The check's seq-v1 cost experiment, once created, holds its runs.
CREATE TABLE screen_checks (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    head_commit   TEXT    NOT NULL,
    base_commit   TEXT    NOT NULL, -- the merge base with the default branch
    status        TEXT    NOT NULL DEFAULT 'queued',
    note          TEXT    NOT NULL DEFAULT '',
    experiment_id INTEGER REFERENCES experiments (id) ON DELETE SET NULL,
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL,
    UNIQUE (project_id, head_commit)
);

-- What the watch may continue: experiments (`agentium watch add`) and screen checks. Removing an enrolment deletes it.
CREATE TABLE watch_enrolments (
    id              INTEGER PRIMARY KEY,
    project_id      INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    experiment_id   INTEGER UNIQUE REFERENCES experiments (id) ON DELETE CASCADE,
    screen_check_id INTEGER UNIQUE REFERENCES screen_checks (id) ON DELETE CASCADE,
    added_at        TEXT    NOT NULL,
    CHECK ((experiment_id IS NULL) <> (screen_check_id IS NULL))
);
CREATE INDEX watch_enrolments_by_project ON watch_enrolments (project_id, id);

-- Drift charts. A panel is one chart: its key (project, model, effort, the context snapshot pinned when it started)
-- and its fixed tasks (JSON: id, name, fingerprint), drawn by seed. A panel whose task retires, turns invalid or is
-- edited closes, and a new panel starts a new chart; points are never spliced across panels. One open panel per
-- project, model and effort.
CREATE TABLE drift_panels (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    model         TEXT    NOT NULL,
    effort        TEXT    NOT NULL,
    snapshot      TEXT    NOT NULL, -- the snapshot's commit
    snapshot_name TEXT    NOT NULL,
    seed          INTEGER NOT NULL,
    tasks         TEXT    NOT NULL,
    created_at    TEXT    NOT NULL,
    closed_at     TEXT    NOT NULL DEFAULT '',
    close_reason  TEXT    NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX drift_panels_open ON drift_panels (project_id, model, effort) WHERE closed_at = '';
-- A check runs every panel task once on one Claude Code version; it may span passes. status: open, restarted (the
-- version changed mid-check), lost (a task was lost, or the panel closed) or stored (its point is in drift_points).
CREATE TABLE drift_checks (
    id             INTEGER PRIMARY KEY,
    panel_id       INTEGER NOT NULL REFERENCES drift_panels (id) ON DELETE CASCADE,
    claude_version TEXT    NOT NULL,
    status         TEXT    NOT NULL DEFAULT 'open',
    note           TEXT    NOT NULL DEFAULT '',
    started_at     TEXT    NOT NULL,
    ended_at       TEXT    NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX drift_checks_open ON drift_checks (panel_id) WHERE status = 'open';
-- A chart's points, at most one per check (the primary key): y is the mean log isolated-run cost of the counted tasks.
CREATE TABLE drift_points (
    check_id       INTEGER PRIMARY KEY REFERENCES drift_checks (id) ON DELETE CASCADE,
    panel_id       INTEGER NOT NULL REFERENCES drift_panels (id) ON DELETE CASCADE,
    y              REAL    NOT NULL,
    claude_version TEXT    NOT NULL,
    tasks          INTEGER NOT NULL CHECK (tasks > 0),
    created_at     TEXT    NOT NULL
);
CREATE INDEX drift_points_by_panel ON drift_points (panel_id, check_id);

-- Runs the watch made carry their pass, written with the run (and in its start file, so a recovered run keeps it):
-- the ledger counts exactly these. A pass with runs cannot be deleted. Drift runs (kind "drift") carry their check.
-- Every run before this migration has neither.
ALTER TABLE runs ADD COLUMN watch_pass_id INTEGER REFERENCES watch_passes (id);
ALTER TABLE runs ADD COLUMN drift_check_id INTEGER REFERENCES drift_checks (id) ON DELETE SET NULL;
CREATE INDEX runs_by_watch_pass ON runs (watch_pass_id);
CREATE INDEX runs_by_drift_check ON runs (drift_check_id);
CREATE INDEX runs_by_finish ON runs (finished_at); -- the ledger's week, across projects
