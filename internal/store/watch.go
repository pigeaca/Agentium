package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RunKindDrift is the kind of a drift chart's runs (Run.Kind); they carry their check (Run.DriftCheckID).
const RunKindDrift = "drift"

// ErrConsentRaise is returned when a consent row that is not interactive would raise the consent in force (see
// migrations/0011_watch.sql): only `agentium watch enable` at a terminal raises it.
var ErrConsentRaise = errors.New("only agentium watch enable at a terminal raises the watch's consent")

// WatchConsent is one row of a project's consent history (see migrations/0011_watch.sql). Shares and thresholds are
// fractions.
type WatchConsent struct {
	ID              int64
	ProjectID       int64
	Enabled         bool
	WeeklyUSD       float64
	RunCapUSD       float64
	PassShare       float64
	WeeklyShare     float64
	StartFiveHour   float64
	StartSevenDay   float64
	LoopExperiments bool
	LoopDrift       bool
	LoopScreens     bool
	SignIn          string
	Interactive     bool
	GrantedBy       string
	AgentiumVersion string
	GrantedAt       time.Time
}

// AddWatchConsent appends a consent row, which becomes the consent in force. A row that is not interactive and would
// raise the consent in force (or grant one where none is) gives ErrConsentRaise; the check runs inside the insert,
// under the write lock, so it compares with the row actually in force.
func (s *Store) AddWatchConsent(ctx context.Context, c WatchConsent) (WatchConsent, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO watch_consents (project_id, enabled, weekly_usd, run_cap_usd, pass_share, weekly_share, start_five_hour,
		                            start_seven_day, loop_experiments, loop_drift, loop_screens, sign_in, interactive, granted_by,
		                            agentium_version, granted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.ProjectID, c.Enabled, c.WeeklyUSD, c.RunCapUSD, c.PassShare,
		c.WeeklyShare, c.StartFiveHour, c.StartSevenDay, c.LoopExperiments, c.LoopDrift, c.LoopScreens, c.SignIn, c.Interactive,
		c.GrantedBy, c.AgentiumVersion, formatTime(c.GrantedAt))
	if err != nil {
		if strings.Contains(err.Error(), "watch consent raise") {
			return WatchConsent{}, fmt.Errorf("save the watch's consent: %w", ErrConsentRaise)
		}
		return WatchConsent{}, fmt.Errorf("save the watch's consent: %w", err)
	}
	if c.ID, err = result.LastInsertId(); err != nil {
		return WatchConsent{}, fmt.Errorf("save the watch's consent: %w", err)
	}
	c.GrantedAt = c.GrantedAt.UTC()
	return c, nil
}

// WatchConsentOf returns the consent in force for a project (its newest row, which may be a revocation), or
// ErrNotFound when the project has none.
func (s *Store) WatchConsentOf(ctx context.Context, projectID int64) (WatchConsent, error) {
	found, err := s.queryConsents(ctx, `WHERE project_id = ? ORDER BY id DESC LIMIT 1`, projectID)
	if err != nil {
		return WatchConsent{}, err
	}
	if len(found) == 0 {
		return WatchConsent{}, fmt.Errorf("the watch's consent: %w", ErrNotFound)
	}
	return found[0], nil
}

// WatchConsents lists a project's consent history, newest first.
func (s *Store) WatchConsents(ctx context.Context, projectID int64) ([]WatchConsent, error) {
	return s.queryConsents(ctx, `WHERE project_id = ? ORDER BY id DESC`, projectID)
}

func (s *Store) queryConsents(ctx context.Context, clause string, args ...any) ([]WatchConsent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, enabled, weekly_usd, run_cap_usd, pass_share, weekly_share, start_five_hour, start_seven_day,
		       loop_experiments, loop_drift, loop_screens, sign_in, interactive, granted_by, agentium_version, granted_at
		FROM watch_consents `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query the watch's consent: %w", err)
	}
	defer rows.Close()
	var found []WatchConsent
	for rows.Next() {
		var c WatchConsent
		var granted string
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Enabled, &c.WeeklyUSD, &c.RunCapUSD, &c.PassShare, &c.WeeklyShare, &c.StartFiveHour,
			&c.StartSevenDay, &c.LoopExperiments, &c.LoopDrift, &c.LoopScreens, &c.SignIn, &c.Interactive, &c.GrantedBy,
			&c.AgentiumVersion, &granted); err != nil {
			return nil, fmt.Errorf("read the watch's consent: %w", err)
		}
		if c.GrantedAt, err = parseTime(granted); err != nil {
			return nil, err
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query the watch's consent: %w", err)
	}
	return found, nil
}

// WatchPass is one pass of the watch (see migrations/0011_watch.sql). Finished is zero while it runs, or after its
// process died until the next pass closes it.
type WatchPass struct {
	ID              int64
	StartedBy       string // "schedule" or "terminal"
	AgentiumVersion string
	Started         time.Time
	Finished        time.Time
	StopReason      string
}

// PassInterrupted is the stop reason StartWatchPass gives a pass left open by a process that died.
const PassInterrupted = "interrupted: its process ended before the pass finished"

// StartWatchPass records a new pass. The caller holds watch.lock, so any pass still open is one whose process died:
// it is closed first, in the same transaction, as PassInterrupted at now, and its id is returned in interrupted. Its
// runs keep their pass, so the ledger still counts them.
func (s *Store) StartWatchPass(ctx context.Context, startedBy, version string, now time.Time) (pass WatchPass, interrupted []int64, err error) {
	stamp := formatTime(now)
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `UPDATE watch_passes SET finished_at = ?, stop_reason = ? WHERE finished_at = '' RETURNING id`,
			stamp, PassInterrupted)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			interrupted = append(interrupted, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO watch_passes (started_by, agentium_version, started_at) VALUES (?, ?, ?)`,
			startedBy, version, stamp)
		if err != nil {
			return err
		}
		pass.ID, err = result.LastInsertId()
		return err
	})
	if err != nil {
		return WatchPass{}, nil, fmt.Errorf("start a watch pass: %w", err)
	}
	return WatchPass{ID: pass.ID, StartedBy: startedBy, AgentiumVersion: version, Started: now.UTC()}, interrupted, nil
}

// FinishWatchPass records why an open pass stopped; ErrNotFound when no open pass has this id.
func (s *Store) FinishWatchPass(ctx context.Context, id int64, reason string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE watch_passes SET finished_at = ?, stop_reason = ? WHERE id = ? AND finished_at = ''`,
		formatTime(now), reason, id)
	if err != nil {
		return fmt.Errorf("finish watch pass %d: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("open watch pass %d: %w", id, errors.Join(ErrNotFound, err))
	}
	return nil
}

// WatchPasses lists the passes started after since, oldest first.
func (s *Store) WatchPasses(ctx context.Context, since time.Time) ([]WatchPass, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, started_by, agentium_version, started_at, finished_at, stop_reason FROM watch_passes WHERE started_at >= ? ORDER BY id`,
		coarseBound(since))
	if err != nil {
		return nil, fmt.Errorf("query watch passes: %w", err)
	}
	defer rows.Close()
	var found []WatchPass
	for rows.Next() {
		var p WatchPass
		var started, finished string
		if err := rows.Scan(&p.ID, &p.StartedBy, &p.AgentiumVersion, &started, &finished, &p.StopReason); err != nil {
			return nil, fmt.Errorf("read watch pass: %w", err)
		}
		if p.Started, err = parseTime(started); err != nil {
			return nil, err
		}
		if p.Finished, err = parseOptionalTime(finished); err != nil {
			return nil, err
		}
		if p.Started.After(since) {
			found = append(found, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query watch passes: %w", err)
	}
	return found, nil
}

// RunsFinishedSince lists a project's runs (of every kind, the watch's or not) that finished after since, oldest
// first.
func (s *Store) RunsFinishedSince(ctx context.Context, projectID int64, since time.Time) ([]Run, error) {
	runs, err := s.queryRuns(ctx, `WHERE project_id = ? AND finished_at >= ? ORDER BY started_at, id`, projectID, coarseBound(since))
	if err != nil {
		return nil, err
	}
	kept := runs[:0]
	for _, r := range runs {
		if r.Finished.After(since) {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// PassRuns lists the runs a watch pass made, across projects, oldest first.
func (s *Store) PassRuns(ctx context.Context, passID int64) ([]Run, error) {
	return s.queryRuns(ctx, `WHERE watch_pass_id = ? ORDER BY started_at, id`, passID)
}

// DriftCheckRuns lists a drift check's runs, oldest first.
func (s *Store) DriftCheckRuns(ctx context.Context, checkID int64) ([]Run, error) {
	return s.queryRuns(ctx, `WHERE drift_check_id = ? ORDER BY started_at, id`, checkID)
}

// coarseBound is a lower bound for comparing stored times as text. RFC 3339 with nanoseconds drops trailing zeros, so
// text order is time order only between times in different seconds ("…:00.5Z" sorts before "…:00Z"); a bound a whole
// second earlier, without a fraction, keeps every later time, and callers filter the rest exactly.
func coarseBound(t time.Time) string {
	return t.UTC().Truncate(time.Second).Add(-time.Second).Format(time.RFC3339)
}

func parseOptionalTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return parseTime(value)
}

func optionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}

// WatchEnrolment is an experiment or a screen check the watch may continue; exactly one of the two ids is set.
type WatchEnrolment struct {
	ID            int64
	ProjectID     int64
	ExperimentID  int64
	ScreenCheckID int64
	AddedAt       time.Time
}

// EnrolExperiment lets the watch continue a project's experiment. It reports false when the experiment was enrolled
// already, and ErrNotFound when the project has no such experiment.
func (s *Store) EnrolExperiment(ctx context.Context, projectID, experimentID int64, now time.Time) (bool, error) {
	return s.enrol(ctx, projectID, "experiment_id", "experiments", experimentID, now)
}

// EnrolScreenCheck lets the watch continue a project's screen check, as EnrolExperiment.
func (s *Store) EnrolScreenCheck(ctx context.Context, projectID, checkID int64, now time.Time) (bool, error) {
	return s.enrol(ctx, projectID, "screen_check_id", "screen_checks", checkID, now)
}

// enrol inserts an enrolment once, after checking in the same transaction that the target belongs to the project.
// column and table are constants of this file.
func (s *Store) enrol(ctx context.Context, projectID int64, column, table string, id int64, now time.Time) (bool, error) {
	var added bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var owner int64
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM `+table+` WHERE id = ?`, id).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || err == nil && owner != projectID {
			return fmt.Errorf("%s %d of project %d: %w", strings.TrimSuffix(table, "s"), id, projectID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO watch_enrolments (project_id, `+column+`, added_at) VALUES (?, ?, ?)
			ON CONFLICT (`+column+`) DO NOTHING`, projectID, id, formatTime(now))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		added = n > 0
		return err
	})
	if err != nil {
		return false, fmt.Errorf("enrol in the watch: %w", err)
	}
	return added, nil
}

// UnenrolExperiment stops the watch continuing an experiment; false when it was not enrolled.
func (s *Store) UnenrolExperiment(ctx context.Context, projectID, experimentID int64) (bool, error) {
	return s.unenrol(ctx, projectID, "experiment_id", experimentID)
}

// UnenrolScreenCheck stops the watch continuing a screen check; false when it was not enrolled.
func (s *Store) UnenrolScreenCheck(ctx context.Context, projectID, checkID int64) (bool, error) {
	return s.unenrol(ctx, projectID, "screen_check_id", checkID)
}

func (s *Store) unenrol(ctx context.Context, projectID int64, column string, id int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM watch_enrolments WHERE project_id = ? AND `+column+` = ?`, projectID, id)
	if err != nil {
		return false, fmt.Errorf("remove from the watch: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("remove from the watch: %w", err)
	}
	return n > 0, nil
}

// WatchEnrolments lists a project's enrolments, oldest first.
func (s *Store) WatchEnrolments(ctx context.Context, projectID int64) ([]WatchEnrolment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, experiment_id, screen_check_id, added_at FROM watch_enrolments WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query watch enrolments: %w", err)
	}
	defer rows.Close()
	var found []WatchEnrolment
	for rows.Next() {
		var e WatchEnrolment
		var experimentID, checkID sql.NullInt64
		var added string
		if err := rows.Scan(&e.ID, &e.ProjectID, &experimentID, &checkID, &added); err != nil {
			return nil, fmt.Errorf("read watch enrolment: %w", err)
		}
		e.ExperimentID, e.ScreenCheckID = experimentID.Int64, checkID.Int64
		if e.AddedAt, err = parseTime(added); err != nil {
			return nil, err
		}
		found = append(found, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query watch enrolments: %w", err)
	}
	return found, nil
}

// ScreenCheck is a cost screen of one pushed head commit (see migrations/0011_watch.sql).
type ScreenCheck struct {
	ID           int64
	ProjectID    int64
	HeadCommit   string
	BaseCommit   string
	Status       string // Screen* below
	Note         string
	ExperimentID int64 // its seq-v1 cost experiment, zero until created
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Screen check statuses.
const (
	ScreenQueued  = "queued"  // waiting for a pass
	ScreenRunning = "running" // its experiment has runs; a pause resumes it on the next pass
	ScreenDone    = "done"    // its experiment ended
	ScreenRefused = "refused" // the range changes a harness file
	ScreenSkipped = "skipped" // nothing to screen (fewer valid tasks than a check needs)
	ScreenFailed  = "failed"  // the check broke; the note says why
)

// SaveScreenCheck records a check for a head commit once: a second save of the same head returns the stored check and
// false.
func (s *Store) SaveScreenCheck(ctx context.Context, c ScreenCheck) (ScreenCheck, bool, error) {
	if c.Status == "" {
		c.Status = ScreenQueued
	}
	var experimentID any
	if c.ExperimentID != 0 {
		experimentID = c.ExperimentID
	}
	stamp := formatTime(c.CreatedAt)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO screen_checks (project_id, head_commit, base_commit, status, note, experiment_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (project_id, head_commit) DO NOTHING`,
		c.ProjectID, c.HeadCommit, c.BaseCommit, c.Status, c.Note, experimentID, stamp, stamp)
	if err != nil {
		return ScreenCheck{}, false, fmt.Errorf("save the screen check of %s: %w", c.HeadCommit, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ScreenCheck{}, false, fmt.Errorf("save the screen check of %s: %w", c.HeadCommit, err)
	}
	stored, err := s.ScreenCheckByHead(ctx, c.ProjectID, c.HeadCommit)
	return stored, n > 0, err
}

// SetScreenCheck records where a check stands, and its experiment once created (zero keeps the stored one).
func (s *Store) SetScreenCheck(ctx context.Context, id int64, status, note string, experimentID int64, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE screen_checks SET status = ?, note = ?, experiment_id = COALESCE(?, experiment_id), updated_at = ? WHERE id = ?`,
		status, note, nullID(experimentID), formatTime(now), id)
	if err != nil {
		return fmt.Errorf("screen check %d: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("screen check %d: %w", id, errors.Join(ErrNotFound, err))
	}
	return nil
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ScreenCheckByHead returns a project's check of a head commit, or ErrNotFound.
func (s *Store) ScreenCheckByHead(ctx context.Context, projectID int64, head string) (ScreenCheck, error) {
	found, err := s.queryScreenChecks(ctx, `WHERE project_id = ? AND head_commit = ?`, projectID, head)
	if err != nil {
		return ScreenCheck{}, err
	}
	if len(found) == 0 {
		return ScreenCheck{}, fmt.Errorf("screen check of %s: %w", head, ErrNotFound)
	}
	return found[0], nil
}

// ScreenChecks lists a project's checks, oldest first.
func (s *Store) ScreenChecks(ctx context.Context, projectID int64) ([]ScreenCheck, error) {
	return s.queryScreenChecks(ctx, `WHERE project_id = ? ORDER BY id`, projectID)
}

func (s *Store) queryScreenChecks(ctx context.Context, clause string, args ...any) ([]ScreenCheck, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, head_commit, base_commit, status, note, experiment_id, created_at, updated_at FROM screen_checks `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query screen checks: %w", err)
	}
	defer rows.Close()
	var found []ScreenCheck
	for rows.Next() {
		var c ScreenCheck
		var experimentID sql.NullInt64
		var created, updated string
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.HeadCommit, &c.BaseCommit, &c.Status, &c.Note, &experimentID, &created, &updated); err != nil {
			return nil, fmt.Errorf("read screen check: %w", err)
		}
		c.ExperimentID = experimentID.Int64
		if c.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if c.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query screen checks: %w", err)
	}
	return found, nil
}

// DriftPanel is one drift chart: its key and fixed tasks (see migrations/0011_watch.sql).
type DriftPanel struct {
	ID           int64
	ProjectID    int64
	Model        string
	Effort       string
	Snapshot     string // the context snapshot's commit, pinned when the chart started
	SnapshotName string
	Seed         int64
	Tasks        []byte // JSON (watch.PanelTask)
	CreatedAt    time.Time
	ClosedAt     time.Time // zero while open
	CloseReason  string
}

// SaveDriftPanel starts a chart. A project has at most one open panel per model and effort: another gives ErrExists.
func (s *Store) SaveDriftPanel(ctx context.Context, p DriftPanel) (DriftPanel, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO drift_panels (project_id, model, effort, snapshot, snapshot_name, seed, tasks, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ProjectID, p.Model, p.Effort, p.Snapshot, p.SnapshotName, p.Seed, string(p.Tasks), formatTime(p.CreatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return DriftPanel{}, fmt.Errorf("an open drift chart for %s (effort %q): %w", p.Model, p.Effort, ErrExists)
		}
		return DriftPanel{}, fmt.Errorf("save drift panel: %w", err)
	}
	if p.ID, err = result.LastInsertId(); err != nil {
		return DriftPanel{}, fmt.Errorf("save drift panel: %w", err)
	}
	p.CreatedAt, p.ClosedAt, p.CloseReason = p.CreatedAt.UTC(), time.Time{}, ""
	return p, nil
}

// OpenDriftPanel returns a project's open panel for a model and effort, or ErrNotFound.
func (s *Store) OpenDriftPanel(ctx context.Context, projectID int64, model, effort string) (DriftPanel, error) {
	found, err := s.queryPanels(ctx, `WHERE project_id = ? AND model = ? AND effort = ? AND closed_at = ''`, projectID, model, effort)
	if err != nil {
		return DriftPanel{}, err
	}
	if len(found) == 0 {
		return DriftPanel{}, fmt.Errorf("open drift chart for %s: %w", model, ErrNotFound)
	}
	return found[0], nil
}

// DriftPanels lists a project's panels, open and closed, oldest first.
func (s *Store) DriftPanels(ctx context.Context, projectID int64) ([]DriftPanel, error) {
	return s.queryPanels(ctx, `WHERE project_id = ? ORDER BY id`, projectID)
}

func (s *Store) queryPanels(ctx context.Context, clause string, args ...any) ([]DriftPanel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, model, effort, snapshot, snapshot_name, seed, tasks, created_at, closed_at, close_reason FROM drift_panels `+clause,
		args...)
	if err != nil {
		return nil, fmt.Errorf("query drift panels: %w", err)
	}
	defer rows.Close()
	var found []DriftPanel
	for rows.Next() {
		var p DriftPanel
		var tasks, created, closed string
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Model, &p.Effort, &p.Snapshot, &p.SnapshotName, &p.Seed, &tasks, &created, &closed,
			&p.CloseReason); err != nil {
			return nil, fmt.Errorf("read drift panel: %w", err)
		}
		p.Tasks = []byte(tasks)
		if p.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if p.ClosedAt, err = parseOptionalTime(closed); err != nil {
			return nil, err
		}
		found = append(found, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query drift panels: %w", err)
	}
	return found, nil
}

// CloseDriftPanel closes an open chart and, in the same transaction, marks its open check lost: a closed chart gets
// no more points. It reports false when the panel was closed already (its first reason stays).
func (s *Store) CloseDriftPanel(ctx context.Context, id int64, reason string, now time.Time) (bool, error) {
	var closed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		stamp := formatTime(now)
		result, err := tx.ExecContext(ctx, `UPDATE drift_panels SET closed_at = ?, close_reason = ? WHERE id = ? AND closed_at = ''`,
			stamp, reason, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		closed = true
		_, err = tx.ExecContext(ctx, `UPDATE drift_checks SET status = ?, note = ?, ended_at = ? WHERE panel_id = ? AND status = ?`,
			DriftCheckLost, "the chart closed: "+reason, stamp, id, DriftCheckOpen)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("close drift panel %d: %w", id, err)
	}
	return closed, nil
}

// DriftCheck is one check of a chart (see migrations/0011_watch.sql).
type DriftCheck struct {
	ID            int64
	PanelID       int64
	ClaudeVersion string
	Status        string // DriftCheck* below
	Note          string
	Started       time.Time
	Ended         time.Time // zero while open
}

// Drift check statuses.
const (
	DriftCheckOpen      = "open"
	DriftCheckRestarted = "restarted" // Claude Code changed mid-check; a new check took over
	DriftCheckLost      = "lost"      // a task was lost, or the chart closed: no point
	DriftCheckStored    = "stored"    // its point is stored
)

// ErrClosed is returned when a drift chart is closed.
var ErrClosed = errors.New("closed")

// StartDriftCheck returns the panel's open check on version, starting one when there is none. An open check on
// another version is marked restarted in the same transaction and returned as restarted: all of a check's runs use
// one version. A closed panel gives ErrClosed.
func (s *Store) StartDriftCheck(ctx context.Context, panelID int64, version string, now time.Time) (check DriftCheck, restarted *DriftCheck, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var closed string
		if err := tx.QueryRowContext(ctx, `SELECT closed_at FROM drift_panels WHERE id = ?`, panelID).Scan(&closed); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("drift panel %d: %w", panelID, ErrNotFound)
			}
			return err
		}
		if closed != "" {
			return fmt.Errorf("drift panel %d: %w", panelID, ErrClosed)
		}
		open, err := queryChecks(ctx, tx, `WHERE panel_id = ? AND status = ?`, panelID, DriftCheckOpen)
		if err != nil {
			return err
		}
		if len(open) > 0 && open[0].ClaudeVersion == version {
			check = open[0]
			return nil
		}
		stamp := formatTime(now)
		if len(open) > 0 {
			old := open[0]
			old.Status, old.Note, old.Ended = DriftCheckRestarted, fmt.Sprintf("Claude Code changed from %s to %s mid-check", old.ClaudeVersion, version), now.UTC()
			if _, err := tx.ExecContext(ctx, `UPDATE drift_checks SET status = ?, note = ?, ended_at = ? WHERE id = ?`,
				old.Status, old.Note, stamp, old.ID); err != nil {
				return err
			}
			restarted = &old
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO drift_checks (panel_id, claude_version, started_at) VALUES (?, ?, ?)`,
			panelID, version, stamp)
		if err != nil {
			return err
		}
		check = DriftCheck{PanelID: panelID, ClaudeVersion: version, Status: DriftCheckOpen, Started: now.UTC()}
		check.ID, err = result.LastInsertId()
		return err
	})
	if err != nil {
		return DriftCheck{}, nil, fmt.Errorf("start a drift check: %w", err)
	}
	return check, restarted, nil
}

// LoseDriftCheck ends an open check without a point (a task was lost); false when it was not open.
func (s *Store) LoseDriftCheck(ctx context.Context, id int64, note string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE drift_checks SET status = ?, note = ?, ended_at = ? WHERE id = ? AND status = ?`,
		DriftCheckLost, note, formatTime(now), id, DriftCheckOpen)
	if err != nil {
		return false, fmt.Errorf("drift check %d: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("drift check %d: %w", id, err)
	}
	return n > 0, nil
}

// DriftCheckByID returns a check, or ErrNotFound.
func (s *Store) DriftCheckByID(ctx context.Context, id int64) (DriftCheck, error) {
	found, err := queryChecks(ctx, s.db, `WHERE id = ?`, id)
	if err != nil {
		return DriftCheck{}, err
	}
	if len(found) == 0 {
		return DriftCheck{}, fmt.Errorf("drift check %d: %w", id, ErrNotFound)
	}
	return found[0], nil
}

// DriftChecks lists a panel's checks, oldest first.
func (s *Store) DriftChecks(ctx context.Context, panelID int64) ([]DriftCheck, error) {
	return queryChecks(ctx, s.db, `WHERE panel_id = ? ORDER BY id`, panelID)
}

func queryChecks(ctx context.Context, q queryer, clause string, args ...any) ([]DriftCheck, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, panel_id, claude_version, status, note, started_at, ended_at FROM drift_checks `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query drift checks: %w", err)
	}
	defer rows.Close()
	var found []DriftCheck
	for rows.Next() {
		var c DriftCheck
		var started, ended string
		if err := rows.Scan(&c.ID, &c.PanelID, &c.ClaudeVersion, &c.Status, &c.Note, &started, &ended); err != nil {
			return nil, fmt.Errorf("read drift check: %w", err)
		}
		if c.Started, err = parseTime(started); err != nil {
			return nil, err
		}
		if c.Ended, err = parseOptionalTime(ended); err != nil {
			return nil, err
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query drift checks: %w", err)
	}
	return found, nil
}

// DriftPoint is a chart's point: one per check.
type DriftPoint struct {
	CheckID       int64
	PanelID       int64
	Y             float64 // the mean log isolated-run cost of the counted tasks
	ClaudeVersion string
	Tasks         int // the tasks counted
	CreatedAt     time.Time
}

// ErrPointConflict is returned when a check's point is stored already with other values.
var ErrPointConflict = errors.New("the check's point is stored with other values")

// SaveDriftPoint stores a check's point once and marks the check stored, in one transaction. Storing it again (a
// second writer, or a pass resumed after a crash) returns the stored point and false; with other values it gives
// ErrPointConflict and changes nothing. Only an open check of an open panel on the point's version takes a new point.
func (s *Store) SaveDriftPoint(ctx context.Context, p DriftPoint) (DriftPoint, bool, error) {
	var created bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		existing, err := queryPoints(ctx, tx, `WHERE check_id = ?`, p.CheckID)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			stored := existing[0]
			if stored.PanelID != p.PanelID || stored.Y != p.Y || stored.ClaudeVersion != p.ClaudeVersion || stored.Tasks != p.Tasks {
				return ErrPointConflict
			}
			p = stored
			return nil
		}
		checks, err := queryChecks(ctx, tx, `WHERE id = ?`, p.CheckID)
		if err != nil {
			return err
		}
		if len(checks) == 0 {
			return ErrNotFound
		}
		check := checks[0]
		var closed string
		if err := tx.QueryRowContext(ctx, `SELECT closed_at FROM drift_panels WHERE id = ?`, check.PanelID).Scan(&closed); err != nil {
			return err
		}
		switch {
		case check.PanelID != p.PanelID:
			return fmt.Errorf("the check belongs to panel %d, not %d", check.PanelID, p.PanelID)
		case closed != "":
			return fmt.Errorf("panel %d: %w", check.PanelID, ErrClosed)
		case check.Status != DriftCheckOpen:
			return fmt.Errorf("the check is %s, not open", check.Status)
		case check.ClaudeVersion != p.ClaudeVersion:
			return fmt.Errorf("the check ran on Claude Code %s, not %s", check.ClaudeVersion, p.ClaudeVersion)
		}
		stamp := formatTime(p.CreatedAt)
		if _, err := tx.ExecContext(ctx, `INSERT INTO drift_points (check_id, panel_id, y, claude_version, tasks, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			p.CheckID, p.PanelID, p.Y, p.ClaudeVersion, p.Tasks, stamp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE drift_checks SET status = ?, ended_at = ? WHERE id = ?`, DriftCheckStored, stamp, p.CheckID); err != nil {
			return err
		}
		p.CreatedAt, created = p.CreatedAt.UTC(), true
		return nil
	})
	if err != nil {
		return DriftPoint{}, false, fmt.Errorf("store the point of drift check %d: %w", p.CheckID, err)
	}
	return p, created, nil
}

// DriftPoints lists a panel's points in check order.
func (s *Store) DriftPoints(ctx context.Context, panelID int64) ([]DriftPoint, error) {
	return queryPoints(ctx, s.db, `WHERE panel_id = ? ORDER BY check_id`, panelID)
}

func queryPoints(ctx context.Context, q queryer, clause string, args ...any) ([]DriftPoint, error) {
	rows, err := q.QueryContext(ctx, `SELECT check_id, panel_id, y, claude_version, tasks, created_at FROM drift_points `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query drift points: %w", err)
	}
	defer rows.Close()
	var found []DriftPoint
	for rows.Next() {
		var p DriftPoint
		var created string
		if err := rows.Scan(&p.CheckID, &p.PanelID, &p.Y, &p.ClaudeVersion, &p.Tasks, &created); err != nil {
			return nil, fmt.Errorf("read drift point: %w", err)
		}
		if p.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		found = append(found, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query drift points: %w", err)
	}
	return found, nil
}
