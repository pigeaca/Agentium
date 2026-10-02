package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, file string) *Store {
	t.Helper()
	s, err := Open(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationsApplyOnceAndSurviveReopen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "space in path", "agentium.db")
	if err := mkdirAll(filepath.Dir(file)); err != nil {
		t.Fatal(err)
	}
	first := open(t, file)
	ctx := context.Background()
	if _, err := first.SaveProject(ctx, "/repo", "repo", []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second := open(t, file) // reopening must not re-run migrations or lose data
	var applied int
	if err := second.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if applied != len(files) {
		t.Errorf("recorded migrations = %d, want %d", applied, len(files))
	}
	if _, err := second.ProjectByRoot(ctx, "/repo"); err != nil {
		t.Errorf("project lost after reopen: %v", err)
	}
}

// A database from before the grading column keeps its tasks, all graded by tests.
func TestGradingMigrationKeepsOldTasks(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agentium.db")
	ctx := context.Background()
	old := open(t, file)
	app, err := old.SaveProject(ctx, "/work/app", "app", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Turn the fresh database back into one from before migration 8, with a task stored the old way.
	for _, stmt := range []string{`ALTER TABLE tasks DROP COLUMN grading`, `DELETE FROM schema_migrations WHERE version = 8`,
		`INSERT INTO tasks (project_id, name, instruction, source, base_commit, verify, created_at, updated_at)
		 VALUES (?, 'old', 'Fix it.', 'manual', 'base', '["make test"]', '2026-09-28T10:00:00Z', '2026-09-28T10:00:00Z')`} {
		var args []any
		if strings.Contains(stmt, "?") {
			args = append(args, app.ID)
		}
		if _, err := old.db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s := open(t, file)
	got, err := s.TaskByName(ctx, app.ID, "old")
	if err != nil || got.Grading != "tests" || got.Instruction != "Fix it." {
		t.Errorf("after the migration: %+v, %v", got, err)
	}
}

func TestSaveProjectUpsertsByRoot(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	created := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	first, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{"v":1}`), created)
	if err != nil {
		t.Fatal(err)
	}
	later := created.Add(time.Hour)
	second, err := s.SaveProject(ctx, "/work/app", "app2", []byte(`{"v":2}`), later)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || !second.CreatedAt.Equal(created) {
		t.Errorf("re-registering changed identity: first %+v, second %+v", first, second)
	}
	got, err := s.ProjectByRoot(ctx, "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "app2" || string(got.Discovery) != `{"v":2}` || !got.UpdatedAt.Equal(later) || !got.CreatedAt.Equal(created) {
		t.Errorf("stored project = %+v", got)
	}
	if _, err := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), later); err != nil {
		t.Fatal(err)
	}
	all, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Name != "app2" || all[1].Name != "other" {
		t.Errorf("projects = %+v", all)
	}
}

func TestProjectByRootNotFound(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	if _, err := s.ProjectByRoot(context.Background(), "/nowhere"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestOpenFailsClearly(t *testing.T) {
	file := filepath.Join(t.TempDir(), "missing-dir", "agentium.db")
	if _, err := Open(context.Background(), file); err == nil || !strings.Contains(err.Error(), "open database "+file) {
		t.Errorf("err = %v, want one naming %s", err, file)
	}
}

func TestOpenRefusesANewerSchemaAndAcceptsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s := open(t, "agentium.db") // relative: must not become a URI authority
	ctx := context.Background()
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, %v; want 1", foreignKeys, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (9999, 'later')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, filepath.Join(dir, "agentium.db")); err == nil || !strings.Contains(err.Error(), "schema version 9999") {
		t.Errorf("newer schema: err = %v", err)
	}
}

func mkdirAll(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func TestSnapshotsAreUniquePerProjectAndCascade(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	snap := Snapshot{ProjectID: app.ID, Name: "baseline", Source: "HEAD", SourceCommit: "abc", CommitID: "def",
		Manifest: []byte(`{"files":[]}`), CreatedAt: now}
	saved, err := s.SaveSnapshot(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSnapshot(ctx, snap); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate name: err = %v, want ErrExists", err)
	}
	snap.ProjectID = other.ID // names are per project
	if _, err := s.SaveSnapshot(ctx, snap); err != nil {
		t.Errorf("same name in another project: %v", err)
	}
	later := Snapshot{ProjectID: app.ID, Name: "minimal", Source: "working tree", SourceCommit: "abc", CommitID: "fed",
		Manifest: []byte(`{}`), CreatedAt: now.Add(time.Minute)}
	if _, err := s.SaveSnapshot(ctx, later); err != nil {
		t.Fatal(err)
	}
	got, err := s.SnapshotByName(ctx, app.ID, "baseline")
	if err != nil || got.ID != saved.ID || got.CommitID != "def" || string(got.Manifest) != `{"files":[]}` || !got.CreatedAt.Equal(now) {
		t.Errorf("SnapshotByName = %+v, %v", got, err)
	}
	if _, err := s.SnapshotByName(ctx, app.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing snapshot: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSnapshot(ctx, other.ID, "baseline"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := s.DeleteSnapshot(ctx, other.ID, "baseline"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete again: err = %v, want ErrNotFound", err)
	}
	list, err := s.Snapshots(ctx, app.ID)
	if err != nil || len(list) != 2 || list[0].Name != "baseline" || list[1].Name != "minimal" {
		t.Errorf("Snapshots = %+v, %v", list, err)
	}
	// Foreign keys are on: deleting a project deletes its snapshots, and a snapshot needs a project.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, app.ID); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Snapshots(ctx, app.ID); err != nil || len(list) != 0 {
		t.Errorf("snapshots after project delete = %+v, %v", list, err)
	}
	if _, err := s.SaveSnapshot(ctx, Snapshot{ProjectID: 999, Name: "orphan", Manifest: []byte(`{}`), CreatedAt: now}); err == nil {
		t.Error("a snapshot without a project must be rejected")
	}
}

// Separate *sql.DB handles behave like separate processes: each has its own connections and locks.
func TestConcurrentOpensOfAFreshDatabase(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agentium.db")
	const openers = 8
	errs := make(chan error, openers)
	for range openers {
		go func() {
			s, err := Open(context.Background(), file)
			if err == nil {
				_, err = s.SaveProject(context.Background(), "/work/app", "app", []byte(`{}`), time.Now())
				s.Close()
			}
			errs <- err
		}()
	}
	for range openers {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestTasksRoundTripUpdateAndCascade(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ProjectID: app.ID, Name: "fix-parser", Instruction: "Fix the parser.", Source: "commit abc",
		BaseCommit: "base", SolutionCommit: "sol", HiddenTests: []string{"p/parser_test.go"}, Reference: []string{"p/parser.go"},
		Verify: []string{"go test ./..."}, Setup: []string{"make assets"}, NeedsReview: true, CreatedAt: now}
	if _, err := s.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTask(ctx, task); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate: err = %v, want ErrExists", err)
	}
	manual := Task{ProjectID: app.ID, Name: "manual", Instruction: "Do it.", Source: "manual", BaseCommit: "base",
		Verify: []string{"make test"}, CreatedAt: now.Add(time.Minute)}
	if _, err := s.SaveTask(ctx, manual); err != nil {
		t.Fatal(err)
	}
	got, err := s.TaskByName(ctx, app.ID, "fix-parser")
	if err != nil || got.SolutionCommit != "sol" || got.HiddenTests[0] != "p/parser_test.go" || got.Reference[0] != "p/parser.go" || got.Setup[0] != "make assets" ||
		!got.NeedsReview || got.Validation != nil || !got.CreatedAt.Equal(now) {
		t.Errorf("TaskByName = %+v, %v", got, err)
	}
	got.Instruction, got.NeedsReview, got.Validation = "Make the parser accept empty input.", false, []byte(`{"status":"valid"}`)
	got.Setup = nil
	if err := s.UpdateTask(ctx, got, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.TaskByName(ctx, app.ID, "fix-parser"); again.NeedsReview || string(again.Validation) != `{"status":"valid"}` || len(again.Setup) != 0 ||
		!again.UpdatedAt.Equal(now.Add(time.Hour)) || again.Instruction != "Make the parser accept empty input." {
		t.Errorf("after update: %+v", again)
	}
	// A validation is stored only while the commands it ran are the task's, and writes nothing else.
	got.Instruction, got.NeedsReview = "Edited meanwhile.", true
	if err := s.UpdateTask(ctx, got, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetTaskValidation(ctx, got.ID, []string{"go test ./..."}, nil, []byte(`{"status":"invalid"}`), now.Add(3*time.Hour)); err != nil || !ok {
		t.Fatalf("SetTaskValidation = %v, %v", ok, err)
	}
	if again, _ := s.TaskByName(ctx, app.ID, "fix-parser"); string(again.Validation) != `{"status":"invalid"}` || again.Instruction != "Edited meanwhile." ||
		!again.NeedsReview || !again.UpdatedAt.Equal(now.Add(3*time.Hour)) {
		t.Errorf("after SetTaskValidation: %+v", again)
	}
	for _, commands := range [][2][]string{{{"go test ./pkg"}, nil}, {{"go test ./..."}, {"make assets"}}} {
		if ok, err := s.SetTaskValidation(ctx, got.ID, commands[0], commands[1], []byte(`{"status":"valid"}`), now); err != nil || ok {
			t.Errorf("SetTaskValidation with other commands %v = %v, %v; want false", commands, ok, err)
		}
	}
	if again, _ := s.TaskByName(ctx, app.ID, "fix-parser"); string(again.Validation) != `{"status":"invalid"}` {
		t.Errorf("a validation of other commands was stored: %s", again.Validation)
	}
	list, err := s.Tasks(ctx, app.ID)
	if err != nil || len(list) != 2 || list[1].Name != "manual" || list[1].HiddenTests == nil || len(list[1].HiddenTests) != 0 ||
		list[0].Grading != "tests" || list[1].Grading != "tests" {
		t.Errorf("Tasks = %+v, %v", list, err)
	}
	judged := Task{ProjectID: app.ID, Name: "judged", Instruction: "Do it.", Source: "ticket ABC-1", BaseCommit: "base", SolutionCommit: "sol",
		Reference: []string{"p/parser.go"}, Verify: []string{"make test"}, Grading: "judge", CreatedAt: now.Add(2 * time.Minute)}
	if _, err := s.SaveTask(ctx, judged); err != nil {
		t.Fatal(err)
	}
	if got, err := s.TaskByName(ctx, app.ID, "judged"); err != nil || got.Grading != "judge" || got.Source != "ticket ABC-1" {
		t.Errorf("judged = %+v, %v", got, err)
	}
	judged.Name, judged.Grading = "unknown", "vibes"
	if _, err := s.SaveTask(ctx, judged); err == nil {
		t.Error("an unknown grading mode must be refused")
	}
	if err := s.DeleteTask(ctx, app.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, Task{ProjectID: app.ID, Name: "manual"}, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of a deleted task: err = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, app.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Tasks(ctx, app.ID); len(list) != 0 {
		t.Errorf("tasks survive their project: %+v", list)
	}
}

func TestRunsRoundTripAndTaskRemoval(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveTask(ctx, Task{ProjectID: app.ID, Name: "fix", Instruction: "Fix.", Source: "manual", BaseCommit: "b", Verify: []string{"true"}, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	passed := true
	for i, run := range []Run{
		{ID: "20260928T100000Z-aaaaaa", ProjectID: app.ID, TaskID: saved.ID, TaskName: "fix", Arm: "base", Outcome: "ok", Passed: &passed, CostUSD: 0.31, Record: []byte(`{"id":"a"}`), Started: now, Finished: now.Add(time.Minute)},
		{ID: "20260928T100500Z-bbbbbb", ProjectID: app.ID, TaskID: saved.ID, TaskName: "fix", Arm: "trimmed", Outcome: "infra", Record: []byte(`{}`), Started: now.Add(5 * time.Minute), Finished: now.Add(6 * time.Minute)},
	} {
		if err := s.SaveRun(ctx, run); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	got, err := s.RunByID(ctx, app.ID, "20260928T100000Z-aaaaaa")
	if err != nil || got.Passed == nil || !*got.Passed || got.CostUSD != 0.31 || got.TaskID != saved.ID || string(got.Record) != `{"id":"a"}` || got.Kind != "task" {
		t.Errorf("RunByID = %+v, %v", got, err)
	}
	if err := s.DeleteTask(ctx, app.ID, "fix"); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx, app.ID)
	if err != nil || len(runs) != 2 || runs[0].TaskID != 0 || runs[1].Passed != nil || runs[1].Arm != "trimmed" {
		t.Errorf("runs after the task was removed = %+v, %v (runs outlive their task)", runs, err)
	}
	if _, err := s.RunByID(ctx, app.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing run: %v", err)
	}
	// A record replaced (a verdict added on resume) leaves the columns as they were.
	if err := s.SetRunRecord(ctx, "20260928T100000Z-aaaaaa", []byte(`{"id":"a","judge":{}}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RunByID(ctx, app.ID, "20260928T100000Z-aaaaaa"); err != nil || string(got.Record) != `{"id":"a","judge":{}}` || got.CostUSD != 0.31 ||
		got.Outcome != "ok" || got.Passed == nil || !*got.Passed {
		t.Errorf("after SetRunRecord = %+v, %v", got, err)
	}
	if err := s.SetRunRecord(ctx, "nope", []byte(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetRunRecord of a missing run: %v", err)
	}
}

func TestLatestCalibrationPerArmAndSnapshot(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range []Calibration{
		{ProjectID: app.ID, Arm: "base", RunID: "r1", Result: []byte(`{"n":1}`), CreatedAt: now},
		{ProjectID: app.ID, Arm: "base", RunID: "r2", Result: []byte(`{"n":2}`), CreatedAt: now.Add(time.Hour)},
		{ProjectID: app.ID, Arm: "trim", Snapshot: "abc", RunID: "r3", Result: []byte(`{"n":3}`), CreatedAt: now},
	} {
		if err := s.SaveCalibration(ctx, c); err != nil {
			t.Fatalf("calibration %d: %v", i, err)
		}
	}
	if c, err := s.LatestCalibration(ctx, app.ID, "base", ""); err != nil || c.RunID != "r2" || string(c.Result) != `{"n":2}` {
		t.Errorf("latest base = %+v, %v", c, err)
	}
	// A snapshot redefined under the same name is another context: its old calibration does not apply.
	if _, err := s.LatestCalibration(ctx, app.ID, "trim", "def"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another snapshot commit: %v", err)
	}
}

func TestExperimentsRoundTrip(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveExperiment(ctx, Experiment{ProjectID: app.ID, Name: "lean", Template: "context-ab", Design: []byte(`{"repeats":3}`), CreatedAt: now})
	if err != nil || saved.ID == 0 {
		t.Fatalf("SaveExperiment = %+v, %v", saved, err)
	}
	if _, err := s.SaveExperiment(ctx, Experiment{ProjectID: app.ID, Name: "lean", Template: "aa", Design: []byte(`{}`), CreatedAt: now}); !errors.Is(err, ErrExists) {
		t.Errorf("a second experiment named lean: %v", err)
	}
	if _, err := s.SaveExperiment(ctx, Experiment{ProjectID: app.ID, Name: "noise", Template: "aa", Design: []byte(`{}`), CreatedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ExperimentByName(ctx, app.ID, "lean")
	if err != nil || got.Template != "context-ab" || string(got.Design) != `{"repeats":3}` || !got.CreatedAt.Equal(now) {
		t.Errorf("ExperimentByName = %+v, %v", got, err)
	}
	if all, err := s.Experiments(ctx, app.ID); err != nil || len(all) != 2 || all[0].Name != "lean" || all[1].Name != "noise" {
		t.Errorf("Experiments = %+v, %v", all, err)
	}
	if err := s.DeleteExperiment(ctx, app.ID, "lean"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExperimentByName(ctx, app.ID, "lean"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted experiment: %v", err)
	}
	if err := s.DeleteExperiment(ctx, app.ID, "lean"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestExperimentLockStatusAndRuns(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.SaveExperiment(ctx, Experiment{ProjectID: app.ID, Name: "lean", Template: "context-ab", Design: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ExperimentByName(ctx, app.ID, "lean"); got.Status != StatusDraft || got.Lock != nil {
		t.Errorf("a new experiment = %+v, want a draft without a lock", got)
	}
	if err := s.AmendLock(ctx, e.ID, []byte(`{"v":0}`)); !errors.Is(err, ErrNotFound) {
		t.Errorf("amending before locking: %v", err)
	}
	if err := s.LockExperiment(ctx, e.ID, []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.LockExperiment(ctx, e.ID, []byte(`{"v":2}`)); !errors.Is(err, ErrLocked) {
		t.Errorf("locking twice: %v", err)
	}
	if err := s.AmendLock(ctx, e.ID, []byte(`{"v":3}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExperimentStatus(ctx, e.ID, StatusBudget, "the next pair would pass $20.00"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ExperimentByName(ctx, app.ID, "lean")
	if err != nil || string(got.Lock) != `{"v":3}` || got.Status != StatusBudget || got.StatusNote != "the next pair would pass $20.00" {
		t.Errorf("locked experiment = %+v, %v", got, err)
	}

	for i, run := range []Run{
		{ID: "r1", ProjectID: app.ID, TaskName: "fix", Arm: "A", Outcome: "infra", Record: []byte(`{}`), Started: now, Finished: now, ExperimentID: e.ID, Slot: 3, Attempt: 1},
		{ID: "r2", ProjectID: app.ID, TaskName: "fix", Arm: "A", Outcome: "ok", Record: []byte(`{}`), Started: now.Add(time.Minute), Finished: now, ExperimentID: e.ID, Slot: 3, Attempt: 2},
		{ID: "r3", ProjectID: app.ID, TaskName: "fix", Arm: "base", Outcome: "ok", Record: []byte(`{}`), Started: now, Finished: now},
		{ID: "r4", ProjectID: app.ID, TaskName: "calibration", Kind: "calibration", Arm: "base", Outcome: "ok", Record: []byte(`{}`), Started: now, Finished: now, ExperimentID: e.ID},
	} {
		if err := s.SaveRun(ctx, run); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	runs, err := s.ExperimentRuns(ctx, e.ID)
	if err != nil || len(runs) != 2 || runs[0].ID != "r1" || runs[1].Slot != 3 || runs[1].Attempt != 2 || runs[1].ExperimentID != e.ID {
		t.Errorf("ExperimentRuns = %+v, %v", runs, err)
	}
	if cal, err := s.ExperimentCalibrationRuns(ctx, e.ID); err != nil || len(cal) != 1 || cal[0].ID != "r4" {
		t.Errorf("ExperimentCalibrationRuns = %+v, %v: a calibration run is the experiment's but not one of its slots", cal, err)
	}
	if other, _ := s.RunByID(ctx, app.ID, "r3"); other.ExperimentID != 0 || other.Slot != 0 {
		t.Errorf("a run outside experiments = %+v", other)
	}
	for id, want := range map[string]bool{"r1": true, "r3": true, "nope": false} {
		if got, err := s.HasRun(ctx, id); err != nil || got != want {
			t.Errorf("HasRun(%s) = %v, %v", id, got, err)
		}
	}
	// Runs outlive their experiment.
	if err := s.DeleteExperiment(ctx, app.ID, "lean"); err != nil {
		t.Fatal(err)
	}
	if run, err := s.RunByID(ctx, app.ID, "r2"); err != nil || run.ExperimentID != 0 {
		t.Errorf("run after its experiment was removed = %+v, %v", run, err)
	}
}

// The local-binding opt-in is off for a new project, stored per project, kept when the project is registered again, and
// read back with the project.
func TestProjectLocalBindingOptIn(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx, now := context.Background(), time.Now()
	p, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil || p.AllowLocalBinding {
		t.Fatalf("a new project: %+v, %v", p, err)
	}
	other, _ := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), now)
	if err := s.SetLocalBinding(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	again, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now.Add(time.Hour))
	if err != nil || !again.AllowLocalBinding {
		t.Fatalf("registering again must keep the choice: %+v, %v", again, err)
	}
	if got, _ := s.ProjectByRoot(ctx, "/work/other"); got.ID != other.ID || got.AllowLocalBinding {
		t.Errorf("another project: %+v", got)
	}
	if err := s.SetLocalBinding(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ProjectByRoot(ctx, "/work/app"); got.AllowLocalBinding {
		t.Error("turning it off did not stick")
	}
}

func TestOpenReadOnlyReadsWithoutWritingOrMigrating(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "agentium.db")
	s, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveProject(ctx, "/repo", "repo", []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(file + side); err == nil {
			t.Fatalf("precondition: %s left behind by the last close", side)
		}
	}

	ro, err := OpenReadOnly(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.ProjectByRoot(ctx, "/repo"); err != nil {
		t.Errorf("read: %v", err)
	}
	if _, err := ro.SaveProject(ctx, "/other", "other", []byte(`{}`), time.Now()); err == nil {
		t.Error("a read-only store accepted a write")
	}
	ro.Close()
	after, err := os.ReadFile(file)
	if err != nil || string(after) != string(before) {
		t.Errorf("the database file changed (%v)", err)
	}
	if _, err := os.Stat(file + "-wal"); err == nil {
		t.Error("a -wal file was created")
	}
}

func TestOpenReadOnlySchemaAndMissingFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := OpenReadOnly(ctx, filepath.Join(dir, "none.db")); err == nil {
		t.Error("a missing database must not be created or opened")
	}
	if _, err := os.Stat(filepath.Join(dir, "none.db")); err == nil {
		t.Error("OpenReadOnly created the file")
	}
	file := filepath.Join(dir, "agentium.db")
	s, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (9999, 'now')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := OpenReadOnly(ctx, file); !errors.Is(err, ErrSchema) || !errors.Is(err, ErrSchemaNewer) {
		t.Errorf("a newer schema: %v, want ErrSchema and ErrSchemaNewer", err)
	}
	notDB := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(notDB, []byte(strings.Repeat("not a database ", 500)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(ctx, notDB); err == nil || errors.Is(err, ErrSchema) {
		t.Errorf("a corrupt file is unreadable, not another schema: %v", err)
	}
}

// retirementVersion is the version of the migration that adds task retirement, found by its name so that the number
// lives in the file name alone.
func retirementVersion(t *testing.T) int {
	t.Helper()
	names, err := fs.Glob(migrations, "migrations/*_task_retirement.sql")
	if err != nil || len(names) != 1 {
		t.Fatalf("the retirement migration: %v, %v", names, err)
	}
	version, err := strconv.Atoi(strings.SplitN(path.Base(names[0]), "_", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	return version
}

// A database populated before the retirement migration (projects, tasks with validations, a locked experiment and its
// runs) keeps everything when the migration applies, and every task stays active.
func TestRetirementMigrationOnAPopulatedDatabase(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agentium.db")
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	old, err := openOnce(ctx, file, retirementVersion(t))
	if err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := old.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name LIKE 'retired%'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("the older schema has %d retirement columns (%v)", columns, err)
	}
	app, err := old.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-migration binary's own INSERT, which names no retirement column.
	for i, name := range []string{"fix-parser", "add-flag"} {
		if _, err := old.db.ExecContext(ctx, `
			INSERT INTO tasks (project_id, name, instruction, source, base_commit, solution_commit, hidden_tests, reference_files,
			                   verify, setup, needs_review, grading, validation, created_at, updated_at)
			VALUES (?, ?, 'Do it.', 'commit abc', 'base', 'sol', '["a_test.go"]', '["a.go"]', '["go test ./..."]', '[]', 0, 'tests',
			        '{"status":"valid"}', ?, ?)`, app.ID, name, formatTime(now.Add(time.Duration(i)*time.Minute)), formatTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	e, err := old.SaveExperiment(ctx, Experiment{ProjectID: app.ID, Name: "lean", Template: "context-ab", Design: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := old.LockExperiment(ctx, e.ID, []byte(`{"tasks":[{"name":"fix-parser"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := old.SaveRun(ctx, Run{ID: "r1", ProjectID: app.ID, TaskID: 1, TaskName: "fix-parser", Arm: "A", Outcome: "ok", Record: []byte(`{}`),
		Started: now, Finished: now, ExperimentID: e.ID, Slot: 0, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s := open(t, file)
	tasks, err := s.Tasks(ctx, app.ID)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks after the migration = %+v, %v", tasks, err)
	}
	for _, task := range tasks {
		if task.Retired() || task.RetiredReason != "" || string(task.Validation) != `{"status":"valid"}` || task.HiddenTests[0] != "a_test.go" {
			t.Errorf("task after the migration = %+v", task)
		}
	}
	if runs, err := s.ExperimentRuns(ctx, e.ID); err != nil || len(runs) != 1 || runs[0].TaskID != tasks[0].ID {
		t.Errorf("the experiment's runs after the migration = %+v, %v", runs, err)
	}
	if got, err := s.ExperimentByName(ctx, app.ID, "lean"); err != nil || got.Status != StatusRunning || got.Lock == nil {
		t.Errorf("the experiment after the migration = %+v, %v", got, err)
	}
	if ok, err := s.RetireTask(ctx, tasks[1].ID, "its base is 300 days old", now); err != nil || !ok {
		t.Errorf("retiring a migrated task = %v, %v", ok, err)
	}
}

// Retiring is a flag with a reason: the row stays listed and editable, a second retirement keeps the first reason, and
// RestoreTask reverses it. SaveTask and UpdateTask leave the flag alone.
func TestRetireAndRestoreTask(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveTask(ctx, Task{ProjectID: app.ID, Name: "fix", Instruction: "Fix it.", Source: "manual", BaseCommit: "base",
		Verify: []string{"go test ./..."}, CreatedAt: now, RetiredAt: now, RetiredReason: "ignored by SaveTask"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TaskByName(ctx, app.ID, "fix"); got.Retired() || got.RetiredReason != "" {
		t.Errorf("SaveTask wrote the retirement: %+v", got)
	}
	if _, err := s.RetireTask(ctx, saved.ID, "  ", now); err == nil {
		t.Error("a retirement without a reason must be refused")
	}
	if ok, err := s.RetireTask(ctx, saved.ID, "a/b.go is gone from the default branch", now.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("RetireTask = %v, %v", ok, err)
	}
	if ok, err := s.RetireTask(ctx, saved.ID, "a second reason", now.Add(2*time.Hour)); err != nil || ok {
		t.Errorf("retiring twice = %v, %v; want false", ok, err)
	}
	got, err := s.TaskByName(ctx, app.ID, "fix")
	if err != nil || !got.Retired() || !got.RetiredAt.Equal(now.Add(time.Hour)) || got.RetiredReason != "a/b.go is gone from the default branch" ||
		!got.UpdatedAt.Equal(now) {
		t.Errorf("retired task = %+v, %v", got, err)
	}
	got.Instruction = "Fix it now."
	if err := s.UpdateTask(ctx, got, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Tasks(ctx, app.ID); len(list) != 1 || !list[0].Retired() || list[0].Instruction != "Fix it now." {
		t.Errorf("Tasks lists = %+v; want the retired task, edited and still retired", list)
	}
	if ok, err := s.RestoreTask(ctx, app.ID, "fix"); err != nil || !ok {
		t.Fatalf("RestoreTask = %v, %v", ok, err)
	}
	if ok, err := s.RestoreTask(ctx, app.ID, "fix"); err != nil || ok {
		t.Errorf("restoring an active task = %v, %v; want false", ok, err)
	}
	if got, _ := s.TaskByName(ctx, app.ID, "fix"); got.Retired() || got.RetiredReason != "" {
		t.Errorf("restored task = %+v", got)
	}
	if _, err := s.RestoreTask(ctx, app.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("restoring a missing task: %v", err)
	}
	if ok, err := s.RetireTask(ctx, 9999, "gone", now); err != nil || ok {
		t.Errorf("retiring a missing task = %v, %v", ok, err)
	}
}

// The pool's re-validation stores nothing for a task that a locked experiment able to run still uses, by name, in the
// same project; drafts, finished experiments, unreadable locks and other projects do not count.
func TestSetTaskValidationIdleSkipsTasksOfLockedExperiments(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, name := range []string{"running", "stopped", "draft", "done", "free", "corrupt"} {
		saved, err := s.SaveTask(ctx, Task{ProjectID: app.ID, Name: name, Instruction: "Do it.", Source: "manual", BaseCommit: "base",
			Verify: []string{"go test ./..."}, Validation: []byte(`{"status":"valid"}`), CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = saved.ID
	}
	experiment := func(projectID int64, name, lock, status string) {
		t.Helper()
		e, err := s.SaveExperiment(ctx, Experiment{ProjectID: projectID, Name: name, Template: "context-ab", Design: []byte(`{"tasks":["draft"]}`), CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if lock == "" {
			return
		}
		if err := s.LockExperiment(ctx, e.ID, []byte(lock)); err != nil {
			t.Fatal(err)
		}
		if err := s.SetExperimentStatus(ctx, e.ID, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	experiment(app.ID, "b-run", `{"tasks":[{"name":"running"},{"name":"stopped"}]}`, StatusRunning)
	experiment(app.ID, "a-stop", `{"tasks":[{"name":"stopped"},"not an object",{"digest":"no name"}]}`, StatusStopped)
	experiment(app.ID, "drafted", "", "")
	experiment(app.ID, "finished", `{"tasks":[{"name":"done"}]}`, StatusDone)
	experiment(app.ID, "broken", `{"tasks":[{"name":"corrupt"}`, StatusRunning)
	experiment(other.ID, "elsewhere", `{"tasks":[{"name":"free"}]}`, StatusRunning)

	users, err := s.TasksInUse(ctx, app.ID)
	want := map[string][]string{"running": {"b-run"}, "stopped": {"a-stop", "b-run"}}
	if err != nil || fmt.Sprint(users) != fmt.Sprint(want) {
		t.Errorf("TasksInUse = %v, %v; want %v", users, err, want)
	}
	verify := []string{"go test ./..."}
	for _, name := range []string{"running", "stopped"} {
		ok, err := s.SetTaskValidationIdle(ctx, ids[name], verify, nil, []byte(`{"status":"invalid"}`), now.Add(time.Hour))
		if ok || !errors.Is(err, ErrTaskInUse) || !strings.Contains(err.Error(), "b-run") {
			t.Errorf("%s: SetTaskValidationIdle = %v, %v; want ErrTaskInUse naming b-run", name, ok, err)
		}
		if got, _ := s.TaskByName(ctx, app.ID, name); string(got.Validation) != `{"status":"valid"}` || !got.UpdatedAt.Equal(now) {
			t.Errorf("%s: a task in use was changed: %+v", name, got)
		}
	}
	for _, name := range []string{"draft", "done", "free", "corrupt"} {
		if ok, err := s.SetTaskValidationIdle(ctx, ids[name], verify, nil, []byte(`{"status":"invalid"}`), now.Add(time.Hour)); err != nil || !ok {
			t.Errorf("%s: SetTaskValidationIdle = %v, %v; want stored", name, ok, err)
		}
	}
	if ok, err := s.SetTaskValidationIdle(ctx, ids["free"], []string{"make test"}, nil, []byte(`{}`), now); err != nil || ok {
		t.Errorf("other commands: SetTaskValidationIdle = %v, %v; want not stored", ok, err)
	}
	if ok, err := s.SetTaskValidationIdle(ctx, 9999, verify, nil, []byte(`{}`), now); err != nil || ok {
		t.Errorf("a missing task: SetTaskValidationIdle = %v, %v; want not stored", ok, err)
	}
}
