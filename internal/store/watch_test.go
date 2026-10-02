package store

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// watchVersion is the version of the migration that adds the watch, found by its name.
func watchVersion(t *testing.T) int {
	t.Helper()
	names, err := fs.Glob(migrations, "migrations/*_watch.sql")
	if err != nil || len(names) != 1 {
		t.Fatalf("the watch migration: %v, %v", names, err)
	}
	version, err := strconv.Atoi(strings.SplitN(path.Base(names[0]), "_", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func consent(confirmed bool, weekly float64, now time.Time) WatchConsent {
	return WatchConsent{Enabled: true, WeeklyUSD: weekly, RunCapUSD: 3, PassShare: 0.3, WeeklyShare: 0.15, StartFiveHour: 0.5,
		StartSevenDay: 0.6, SignIn: "token-file", SignInIdentity: "id-1", Confirmed: confirmed, GrantedBy: "ana", AgentiumVersion: "dev",
		GrantedAt: now}
}

// Crash and recovery: a database populated before the watch migration (a project, a task, a locked experiment, task
// and calibration runs) keeps everything when it applies. Old runs carry no pass or check, so the ledger counts none of
// them, no project has consent, and the new tables take rows that point at the old ones.
func TestWatchMigrationOnAPopulatedDatabase(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agentium.db")
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	old, err := openOnce(ctx, file, watchVersion(t))
	if err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := old.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name IN ('watch_pass_id', 'drift_check_id')`).
		Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("the older schema has %d watch columns (%v)", columns, err)
	}
	app, err := old.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	task, err := old.SaveTask(ctx, Task{ProjectID: app.ID, Name: "fix-parser", Instruction: "Fix it.", Source: "manual", BaseCommit: "base",
		Verify: []string{"go test ./..."}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	e, err := old.SaveExperiment(ctx, Experiment{ProjectID: app.ID, Name: "lean", Template: "context-ab", Design: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := old.LockExperiment(ctx, e.ID, []byte(`{"tasks":[{"name":"fix-parser"}]}`)); err != nil {
		t.Fatal(err)
	}
	// The pre-migration binary's own INSERT, which names no watch column.
	for i, kind := range []string{"task", "calibration"} {
		if _, err := old.db.ExecContext(ctx, `
			INSERT INTO runs (id, project_id, task_id, task_name, kind, arm, outcome, passed, cost_usd, record, started_at, finished_at,
			                  experiment_id, slot, attempt)
			VALUES (?, ?, ?, 'fix-parser', ?, 'A', 'ok', 1, 0.5, '{"sign_in":"login"}', ?, ?, ?, 0, 1)`,
			"r"+strconv.Itoa(i), app.ID, task.ID, kind, formatTime(now), formatTime(now.Add(time.Minute)), e.ID); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	s := open(t, file)
	runs, err := s.RunsFinishedSince(ctx, now.Add(-time.Hour))
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs after the migration = %+v, %v", runs, err)
	}
	for _, r := range runs {
		if r.WatchPassID != 0 || r.DriftCheckID != 0 || r.ExperimentID != e.ID || r.CostUSD != 0.5 {
			t.Errorf("run after the migration = %+v", r)
		}
	}
	if got, err := s.ExperimentRuns(ctx, e.ID); err != nil || len(got) != 1 {
		t.Errorf("the experiment's runs after the migration = %+v, %v", got, err)
	}
	if _, err := s.WatchConsentInForce(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("the consent after the migration: %v, want none", err)
	}
	if _, err := s.WatchLoopsOf(ctx, app.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a migrated project's loops: %v, want none", err)
	}
	if added, err := s.EnrolExperiment(ctx, app.ID, e.ID, now); err != nil || !added {
		t.Errorf("enrolling a migrated experiment = %v, %v", added, err)
	}
	pass, _, err := s.StartWatchPass(ctx, "schedule", "dev", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRun(ctx, Run{ID: "r2", ProjectID: app.ID, TaskID: task.ID, TaskName: "fix-parser", Arm: "A", Outcome: "ok", Record: []byte(`{}`),
		Started: now, Finished: now, WatchPassID: pass.ID}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PassRuns(ctx, pass.ID); err != nil || len(got) != 1 || got[0].ID != "r2" {
		t.Errorf("the pass's runs = %+v, %v", got, err)
	}
}

// Budget and consent: the history is append-only (no update, no delete), and an unconfirmed row cannot raise the
// consent in force, whatever writes it: the trigger refuses a first grant, a higher cap or threshold, and another
// sign-in mode or identity, and accepts a lowering and a revocation. After a revocation only a confirmed row restores
// it.
func TestWatchConsentOnlyTheTerminalRaises(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddWatchConsent(ctx, consent(false, 20, now)); !errors.Is(err, ErrConsentRaise) {
		t.Fatalf("an unconfirmed first grant: %v, want ErrConsentRaise", err)
	}
	granted, err := s.AddWatchConsent(ctx, consent(true, 20, now))
	if err != nil {
		t.Fatal(err)
	}
	raises := map[string]func(c *WatchConsent){
		"weekly":     func(c *WatchConsent) { c.WeeklyUSD = 21 },
		"run cap":    func(c *WatchConsent) { c.RunCapUSD = 4 },
		"pass share": func(c *WatchConsent) { c.PassShare = 0.31 },
		"week share": func(c *WatchConsent) { c.WeeklyShare = 0.2 },
		"five-hour":  func(c *WatchConsent) { c.StartFiveHour = 0.9 },
		"seven-day":  func(c *WatchConsent) { c.StartSevenDay = 0.7 },
		"sign-in":    func(c *WatchConsent) { c.SignIn = "api-key" },
		"identity":   func(c *WatchConsent) { c.SignInIdentity = "id-2" },
	}
	for name, raise := range raises {
		c := consent(false, 20, now)
		raise(&c)
		if _, err := s.AddWatchConsent(ctx, c); !errors.Is(err, ErrConsentRaise) {
			t.Errorf("an unconfirmed raise of the %s: %v, want ErrConsentRaise", name, err)
		}
	}
	if got, err := s.WatchConsentInForce(ctx); err != nil || got.ID != granted.ID {
		t.Fatalf("the consent in force after refused raises = %+v, %v", got, err)
	}
	if _, err := s.AddWatchConsent(ctx, consent(false, 10, now.Add(time.Hour))); err != nil {
		t.Fatalf("a lowering: %v", err)
	}
	if _, err := s.AddWatchConsent(ctx, consent(false, 15, now)); !errors.Is(err, ErrConsentRaise) {
		t.Errorf("raising back above the lowered budget: %v, want ErrConsentRaise", err)
	}
	revoked := WatchConsent{SignIn: "token-file", SignInIdentity: "id-1", GrantedBy: "ana", GrantedAt: now.Add(2 * time.Hour)}
	if _, err := s.AddWatchConsent(ctx, revoked); err != nil {
		t.Fatalf("a revocation: %v", err)
	}
	if _, err := s.AddWatchConsent(ctx, consent(false, 1, now)); !errors.Is(err, ErrConsentRaise) {
		t.Errorf("an unconfirmed grant after the revocation: %v, want ErrConsentRaise", err)
	}
	history, err := s.WatchConsents(ctx)
	if err != nil || len(history) != 3 || history[0].Enabled || history[1].WeeklyUSD != 10 || !history[2].Confirmed ||
		history[2].GrantedBy != "ana" || history[2].SignInIdentity != "id-1" {
		t.Fatalf("the consent history = %+v, %v", history, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE watch_consents SET weekly_usd = 1000`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("updating a consent row: %v, want refused", err)
	}
	if _, err := s.AddWatchConsent(ctx, consent(true, -1, now)); err == nil {
		t.Error("a negative budget was stored")
	}
}

// Budget and consent: deleting consent rows is refused, whether the newest (which would bring back an older, higher
// consent) or all of them; deleting a project leaves the data folder's consent in place.
func TestWatchConsentRowsCannotBeDeleted(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	app, _ := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if _, err := s.AddWatchConsent(ctx, consent(true, 50, now)); err != nil {
		t.Fatal(err)
	}
	lowered, err := s.AddWatchConsent(ctx, consent(false, 5, now))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`DELETE FROM watch_consents WHERE id = ` + strconv.FormatInt(lowered.ID, 10), `DELETE FROM watch_consents`} {
		if _, err := s.db.ExecContext(ctx, query); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v, want refused", query, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, app.ID); err != nil {
		t.Fatalf("deleting a project: %v", err)
	}
	if got, err := s.WatchConsentInForce(ctx); err != nil || got.ID != lowered.ID || got.WeeklyUSD != 5 {
		t.Errorf("the consent in force = %+v, %v; want the lowering", got, err)
	}
}

// Budget and consent: an unconfirmed write may turn a project's loops off but never on, each loop alone included, on
// insert and on update; a confirmed write may turn them on. A project's loops go with the project.
func TestWatchLoopsOnlyTheTerminalEnables(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	app, _ := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	enable := map[string]func(l *WatchLoops){
		"experiments": func(l *WatchLoops) { l.Experiments = true },
		"drift":       func(l *WatchLoops) { l.Drift = true },
		"screens":     func(l *WatchLoops) { l.Screens = true },
	}
	for name, on := range enable {
		l := WatchLoops{ProjectID: app.ID, SetBy: "ana", SetAt: now}
		on(&l)
		if err := s.SetWatchLoops(ctx, l); !errors.Is(err, ErrConsentRaise) {
			t.Errorf("an unconfirmed first %s loop: %v, want ErrConsentRaise", name, err)
		}
	}
	if err := s.SetWatchLoops(ctx, WatchLoops{ProjectID: app.ID, SetBy: "ana", SetAt: now}); err != nil {
		t.Fatalf("an unconfirmed row with every loop off: %v", err)
	}
	for name, on := range enable {
		l := WatchLoops{ProjectID: app.ID, SetBy: "ana", SetAt: now}
		on(&l)
		if err := s.SetWatchLoops(ctx, l); !errors.Is(err, ErrConsentRaise) {
			t.Errorf("an unconfirmed update turning on %s: %v, want ErrConsentRaise", name, err)
		}
	}
	if err := s.SetWatchLoops(ctx, WatchLoops{ProjectID: app.ID, Experiments: true, Drift: true, Screens: true, Confirmed: true, SetBy: "ana",
		SetAt: now}); err != nil {
		t.Fatalf("a confirmed update: %v", err)
	}
	if err := s.SetWatchLoops(ctx, WatchLoops{ProjectID: app.ID, Experiments: true, SetBy: "agentium.toml", SetAt: now}); err != nil {
		t.Fatalf("an unconfirmed lowering: %v", err)
	}
	if got, err := s.WatchLoopsOf(ctx, app.ID); err != nil || !got.Experiments || got.Drift || got.Screens || got.Confirmed || got.SetBy != "agentium.toml" {
		t.Errorf("the loops = %+v, %v", got, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WatchLoopsOf(ctx, app.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a removed project's loops: %v", err)
	}
}

// Crash and recovery: a pass whose process died stays open until the next pass closes it as interrupted; its runs,
// including one saved later from its start file, keep the pass, and a pass with runs cannot be deleted.
func TestWatchPassesCloseInterruptedOnes(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	dead, interrupted, err := s.StartWatchPass(ctx, "schedule", "dev", now)
	if err != nil || len(interrupted) != 0 {
		t.Fatalf("the first pass: %+v, %v, %v", dead, interrupted, err)
	}
	next, interrupted, err := s.StartWatchPass(ctx, "schedule", "dev", now.Add(24*time.Hour))
	if err != nil || len(interrupted) != 1 || interrupted[0] != dead.ID {
		t.Fatalf("the next pass: %+v, interrupted %v, %v", next, interrupted, err)
	}
	if err := s.SaveRun(ctx, Run{ID: "late", ProjectID: app.ID, TaskName: "t", Arm: "A", Outcome: "infra", Record: []byte(`{}`),
		Started: now, Finished: now.Add(time.Minute), WatchPassID: dead.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWatchPass(ctx, dead.ID, "done", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("finishing a closed pass: %v, want ErrNotFound", err)
	}
	if err := s.FinishWatchPass(ctx, next.ID, "the deadline", now.Add(28*time.Hour)); err != nil {
		t.Fatal(err)
	}
	passes, err := s.WatchPasses(ctx, now.Add(-time.Second))
	if err != nil || len(passes) != 2 || passes[0].StopReason != PassInterrupted || passes[1].StopReason != "the deadline" || passes[1].Finished.IsZero() {
		t.Fatalf("passes = %+v, %v", passes, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM watch_passes WHERE id = ?`, dead.ID); err == nil {
		t.Error("a pass with runs was deleted: the ledger would lose them")
	}
}

// Enrolment is once per experiment or check, and only within its project.
func TestWatchEnrolments(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	app, _ := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	other, _ := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), now)
	e, err := s.SaveExperiment(ctx, Experiment{ProjectID: app.ID, Name: "lean", Template: "context-ab", Design: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	check, created, err := s.SaveScreenCheck(ctx, ScreenCheck{ProjectID: app.ID, HeadCommit: "head", BaseCommit: "base", CreatedAt: now})
	if err != nil || !created || check.Status != ScreenQueued {
		t.Fatalf("a screen check = %+v, %v, %v", check, created, err)
	}
	if added, err := s.EnrolExperiment(ctx, app.ID, e.ID, now); err != nil || !added {
		t.Fatalf("enrol = %v, %v", added, err)
	}
	if added, err := s.EnrolExperiment(ctx, app.ID, e.ID, now); err != nil || added {
		t.Errorf("enrolling twice = %v, %v; want no second row", added, err)
	}
	if added, err := s.EnrolScreenCheck(ctx, app.ID, check.ID, now); err != nil || !added {
		t.Errorf("enrol a screen check = %v, %v", added, err)
	}
	if _, err := s.EnrolExperiment(ctx, other.ID, e.ID, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("enrolling another project's experiment: %v, want ErrNotFound", err)
	}
	if _, err := s.EnrolExperiment(ctx, app.ID, 999, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("enrolling a missing experiment: %v, want ErrNotFound", err)
	}
	list, err := s.WatchEnrolments(ctx, app.ID)
	if err != nil || len(list) != 2 || list[0].ExperimentID != e.ID || list[1].ScreenCheckID != check.ID {
		t.Fatalf("enrolments = %+v, %v", list, err)
	}
	if removed, err := s.UnenrolExperiment(ctx, app.ID, e.ID); err != nil || !removed {
		t.Errorf("unenrol = %v, %v", removed, err)
	}
	if removed, err := s.UnenrolExperiment(ctx, app.ID, e.ID); err != nil || removed {
		t.Errorf("unenrol twice = %v, %v", removed, err)
	}
	if err := s.DeleteExperiment(ctx, app.ID, "lean"); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.UnenrolScreenCheck(ctx, app.ID, check.ID); err != nil || !removed {
		t.Errorf("unenrol the screen check = %v, %v", removed, err)
	}
}

// A head commit has one screen check: saving it again returns the stored one.
func TestScreenCheckOncePerHead(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	app, _ := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	first, created, err := s.SaveScreenCheck(ctx, ScreenCheck{ProjectID: app.ID, HeadCommit: "head", BaseCommit: "base", CreatedAt: now})
	if err != nil || !created {
		t.Fatal(created, err)
	}
	e, err := s.SaveExperiment(ctx, Experiment{ProjectID: app.ID, Name: "screen-head", Template: "context-ab", Design: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetScreenCheck(ctx, first.ID, ScreenRunning, "", e.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScreenCheck(ctx, first.ID, ScreenDone, "inconclusive", 0, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	again, created, err := s.SaveScreenCheck(ctx, ScreenCheck{ProjectID: app.ID, HeadCommit: "head", BaseCommit: "other", CreatedAt: now})
	if err != nil || created || again.ID != first.ID || again.BaseCommit != "base" || again.Status != ScreenDone || again.ExperimentID != e.ID ||
		again.Note != "inconclusive" {
		t.Errorf("the second save = %+v, created %v, %v", again, created, err)
	}
	if err := s.SetScreenCheck(ctx, 999, ScreenDone, "", 0, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing check: %v", err)
	}
}

func driftSetup(t *testing.T, s *Store, now time.Time) (Project, DriftPanel) {
	t.Helper()
	ctx := context.Background()
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	panel, err := s.SaveDriftPanel(ctx, DriftPanel{ProjectID: app.ID, Model: "claude-sonnet-5", Effort: "", Snapshot: "c0ffee",
		SnapshotName: "main", Seed: 7, Tasks: []byte(`[]`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	return app, panel
}

// A check keeps one Claude Code version: resuming on the same version returns it, and a new version restarts it.
// A closed chart takes no new check and loses its open one; a project has one open chart per model and effort.
func TestDriftChecksAndPanels(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	app, panel := driftSetup(t, s, now)
	if _, err := s.SaveDriftPanel(ctx, DriftPanel{ProjectID: app.ID, Model: "claude-sonnet-5", Snapshot: "other", Tasks: []byte(`[]`),
		CreatedAt: now}); !errors.Is(err, ErrExists) {
		t.Errorf("a second open chart for the same model: %v, want ErrExists", err)
	}
	first, restarted, err := s.StartDriftCheck(ctx, panel.ID, "2.1.0", now)
	if err != nil || restarted != nil {
		t.Fatal(first, restarted, err)
	}
	resumed, restarted, err := s.StartDriftCheck(ctx, panel.ID, "2.1.0", now.Add(24*time.Hour))
	if err != nil || restarted != nil || resumed.ID != first.ID {
		t.Errorf("resuming on the same version = %+v, %+v, %v", resumed, restarted, err)
	}
	second, restarted, err := s.StartDriftCheck(ctx, panel.ID, "2.2.0", now.Add(24*time.Hour))
	if err != nil || restarted == nil || restarted.ID != first.ID || second.ID == first.ID || second.ClaudeVersion != "2.2.0" {
		t.Fatalf("a version change = %+v, restarted %+v, %v", second, restarted, err)
	}
	if got, _ := s.DriftCheckByID(ctx, first.ID); got.Status != DriftCheckRestarted || !strings.Contains(got.Note, "2.1.0 to 2.2.0") {
		t.Errorf("the restarted check = %+v", got)
	}
	if closed, err := s.CloseDriftPanel(ctx, panel.ID, "task fix-parser retired", now); err != nil || !closed {
		t.Fatal(closed, err)
	}
	if closed, err := s.CloseDriftPanel(ctx, panel.ID, "again", now); err != nil || closed {
		t.Errorf("closing twice = %v, %v", closed, err)
	}
	if got, _ := s.DriftCheckByID(ctx, second.ID); got.Status != DriftCheckLost || !strings.Contains(got.Note, "retired") {
		t.Errorf("the open check of a closed chart = %+v", got)
	}
	if _, _, err := s.StartDriftCheck(ctx, panel.ID, "2.2.0", now); !errors.Is(err, ErrClosed) {
		t.Errorf("a check on a closed chart: %v, want ErrClosed", err)
	}
	if _, err := s.SaveDriftPanel(ctx, DriftPanel{ProjectID: app.ID, Model: "claude-sonnet-5", Snapshot: "c0ffee", Tasks: []byte(`[]`),
		CreatedAt: now}); err != nil {
		t.Errorf("a new chart after the old one closed: %v", err)
	}
	if panels, err := s.DriftPanels(ctx, app.ID); err != nil || len(panels) != 2 || panels[0].ClosedAt.IsZero() || !panels[1].ClosedAt.IsZero() {
		t.Errorf("panels = %+v, %v", panels, err)
	}
}

// Crash and recovery: a point is stored once per check. Storing it again (a resumed pass) returns the stored point;
// other values are a conflict that changes nothing; a lost, restarted or other-version check takes no point.
func TestDriftPointStoredOncePerCheck(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	_, panel := driftSetup(t, s, now)
	check, _, err := s.StartDriftCheck(ctx, panel.ID, "2.1.0", now)
	if err != nil {
		t.Fatal(err)
	}
	point := DriftPoint{CheckID: check.ID, PanelID: panel.ID, Y: -0.42, ClaudeVersion: "2.1.0", Tasks: 8, CreatedAt: now}
	if _, _, err := s.SaveDriftPoint(ctx, DriftPoint{CheckID: check.ID, PanelID: panel.ID, Y: 1, ClaudeVersion: "2.2.0", Tasks: 8, CreatedAt: now}); err == nil {
		t.Error("a point on another version than its check's was stored")
	}
	stored, created, err := s.SaveDriftPoint(ctx, point)
	if err != nil || !created {
		t.Fatal(stored, created, err)
	}
	again, created, err := s.SaveDriftPoint(ctx, DriftPoint{CheckID: check.ID, PanelID: panel.ID, Y: -0.42, ClaudeVersion: "2.1.0", Tasks: 8,
		CreatedAt: now.Add(time.Hour)})
	if err != nil || created || !again.CreatedAt.Equal(now) {
		t.Errorf("storing the point again = %+v, %v, %v; want the stored one", again, created, err)
	}
	conflict := point
	conflict.Y = 0.1
	if _, _, err := s.SaveDriftPoint(ctx, conflict); !errors.Is(err, ErrPointConflict) {
		t.Errorf("another value for a stored point: %v, want ErrPointConflict", err)
	}
	if got, _ := s.DriftCheckByID(ctx, check.ID); got.Status != DriftCheckStored {
		t.Errorf("the check after its point = %+v", got)
	}
	lost, _, err := s.StartDriftCheck(ctx, panel.ID, "2.1.0", now.Add(7*24*time.Hour))
	if err != nil || lost.ID == check.ID {
		t.Fatal(lost, err)
	}
	if ok, err := s.LoseDriftCheck(ctx, lost.ID, "task fix-parser timed out", now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, _, err := s.SaveDriftPoint(ctx, DriftPoint{CheckID: lost.ID, PanelID: panel.ID, Y: 0, ClaudeVersion: "2.1.0", Tasks: 7, CreatedAt: now}); err == nil {
		t.Error("a lost check stored a point")
	}
	points, err := s.DriftPoints(ctx, panel.ID)
	if err != nil || len(points) != 1 || points[0].Y != -0.42 || points[0].Tasks != 8 {
		t.Errorf("points = %+v, %v", points, err)
	}
}

// Concurrency: writers in two processes (two handles on one file) storing the same check's point at once store it
// once; every writer gets the stored point, and exactly one reports creating it.
func TestDriftPointConcurrentWriters(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agentium.db")
	first := open(t, file)
	second := open(t, file)
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	_, panel := driftSetup(t, first, now)
	check, _, err := first.StartDriftCheck(ctx, panel.ID, "2.1.0", now)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 8
	var wg sync.WaitGroup
	created := make([]bool, writers)
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			s := first
			if i%2 == 1 {
				s = second
			}
			_, created[i], errs[i] = s.SaveDriftPoint(ctx, DriftPoint{CheckID: check.ID, PanelID: panel.ID, Y: 0.25, ClaudeVersion: "2.1.0",
				Tasks: 8, CreatedAt: now})
		})
	}
	wg.Wait()
	n := 0
	for i := range writers {
		if errs[i] != nil {
			t.Errorf("writer %d: %v", i, errs[i])
		}
		if created[i] {
			n++
		}
	}
	var rows int
	if err := first.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drift_points`).Scan(&rows); err != nil || rows != 1 || n != 1 {
		t.Errorf("%d rows, %d writers created one (%v); want 1 and 1", rows, n, err)
	}
}

// RunsFinishedSince compares times, not their text: stored times with and without fractions of a second on both sides
// of the bound come out right.
func TestRunsFinishedSinceAtSubSecondBounds(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	since := time.Date(2026, 9, 25, 9, 0, 0, 300_000_000, time.UTC)
	app, _ := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), since)
	for id, finished := range map[string]time.Time{
		"before-whole":    since.Truncate(time.Second),                  // 09:00:00, before since
		"before-fraction": since.Add(-100 * time.Millisecond),           // 09:00:00.2
		"at":              since,                                        // excluded: the span is (since, now]
		"after-fraction":  since.Add(200 * time.Millisecond),            // 09:00:00.5, text-sorts before 09:00:00Z
		"after-whole":     since.Truncate(time.Second).Add(time.Second), // 09:00:01
	} {
		if err := s.SaveRun(ctx, Run{ID: id, ProjectID: app.ID, TaskName: "t", Arm: "A", Outcome: "ok", Record: []byte(`{}`),
			Started: finished.Add(-time.Minute), Finished: finished}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.RunsFinishedSince(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "after-fraction,after-whole" {
		t.Errorf("runs after the bound = %v", ids)
	}
}
