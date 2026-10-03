package cli

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/pool"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// poolFixture is a start repository with features task candidates, registered, whose Claude Code is a canary: any
// execution of it leaves a mark (the pool must never start an agent or a judge, which run the same binary). Its run
// takes a clock.
type poolFixture struct {
	runFixture
	canary string // the mark the fake Claude Code leaves when it runs
	clock  time.Time
}

func newPoolFixture(t *testing.T, features, named int) *poolFixture {
	t.Helper()
	f := runFixtureAt(startRepoNaming(t, features, named), filepath.Join(t.TempDir(), "data"), t.TempDir())
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "", false)
	expect(t, f.run(context.Background(), "init"), ExitOK) // init asks Claude Code for its version: before the canary
	p := &poolFixture{runFixture: f, canary: filepath.Join(t.TempDir(), "claude-ran"), clock: time.Now().UTC()}
	cli := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho \"$@\" >> '"+p.canary+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.vars["AGENTIUM_CLAUDE"] = cli
	return p
}

// at runs agentium with the clock at p.clock plus offset.
func (p *poolFixture) at(offset time.Duration, args ...string) cliResult {
	now := p.clock.Add(offset)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), Env{DefaultGrader: "host", Args: args, Stdout: &stdout, Stderr: &stderr, Dir: p.repo,
		Getenv:   func(key string) string { return p.vars[key] },
		Environ:  func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + p.home} },
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: func() time.Time { return now }})
	return cliResult{code, stdout.String(), stderr.String()}
}

// noAgent fails when the canary ran.
func (p *poolFixture) noAgent(t *testing.T) {
	t.Helper()
	if data, err := os.ReadFile(p.canary); err == nil {
		t.Errorf("the pool started Claude Code: %s", data)
	}
}

// tasks reads the project's tasks as stored.
func (p *poolFixture) tasks(t *testing.T) []store.Task {
	t.Helper()
	db, project := p.db(t)
	tasks, err := db.Tasks(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	return tasks
}

func (p *poolFixture) db(t *testing.T) (*store.Store, int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(p.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	return db, projects[0].ID
}

// name is the full name of the task whose name starts with prefix (mined names end with a short commit hash).
func (p *poolFixture) name(t *testing.T, prefix string) string {
	t.Helper()
	for _, tk := range p.tasks(t) {
		if strings.HasPrefix(tk.Name, prefix) {
			return tk.Name
		}
	}
	t.Fatalf("no task named %s...", prefix)
	return ""
}

// health checks the counts of the health table in out: label, then the count.
func health(t *testing.T, r cliResult, counts ...any) {
	t.Helper()
	for i := 0; i+1 < len(counts); i += 2 {
		label, want := counts[i].(string), counts[i+1].(int)
		found := false
		for _, line := range strings.Split(r.stdout, "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), label+" "); ok {
				found = true
				if got := strings.Fields(rest); len(got) == 0 || got[0] != fmt.Sprint(want) {
					t.Errorf("%s: %q, want %d\n%s", label, rest, want, r.stdout)
				}
			}
		}
		if !found {
			t.Errorf("no %q line:\n%s", label, r.stdout)
		}
	}
}

// validatedAt maps each task to when its validation was made.
func validatedAt(tasks []store.Task) map[string]time.Time {
	at := map[string]time.Time{}
	for _, t := range tasks {
		at[t.Name] = task.ValidationOf(t).At
	}
	return at
}

// A full pass: the new commits become validated tasks awaiting review, and the pool's health says so; a second pass
// has nothing new; a new commit after it is the next pass's only import. No agent starts at any point.
func TestPoolUpdateMinesValidatesAndReports(t *testing.T) {
	t.Parallel()
	p := newPoolFixture(t, 3, 0)
	first := p.at(0, "pool", "update")
	expect(t, first, ExitOK, "runs no agent and costs nothing", "commit(s) read since the last pass, 3 candidate(s)", "Imported 3 of 3 candidate(s) tried (verify: make test)",
		"Validating 3 mined task(s)", "Review each mined instruction for solution leaks", "Task pool: 3 task(s)")
	health(t, first, "valid", 3, "awaiting review", 3, "retired", 0)
	tasks := p.tasks(t)
	if len(tasks) != 3 {
		t.Fatalf("%d tasks", len(tasks))
	}
	for _, tk := range tasks {
		if !tk.NeedsReview || task.ValidationOf(tk).Status != task.StatusValid {
			t.Errorf("%s: review %v, status %q", tk.Name, tk.NeedsReview, task.ValidationOf(tk).Status)
		}
	}
	st, err := pool.Load(pool.StateFile(filepath.Join(p.data, "projects", fmt.Sprint(tasks[0].ProjectID), "repo.git")))
	if err != nil || len(st.Mined) != 3 || len(st.Watermark) != 1 || !st.LastPass.Equal(p.clock) {
		t.Errorf("state %+v, %v", st, err)
	}

	again := p.at(time.Hour, "pool", "update")
	expect(t, again, ExitOK, "0 commit(s) read since the last pass, 0 candidate(s)", "Nothing new to import.")
	if strings.Contains(again.stdout, "Validating") || strings.Contains(again.stdout, "Re-validating") {
		t.Errorf("a pass with nothing new validated:\n%s", again.stdout)
	}

	lib, err := os.ReadFile(filepath.Join(p.repo, "lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.repo, "lib.sh", string(lib)+"f9() { echo v9; }\n")
	writeFile(t, p.repo, "tests/f9_test.sh", ". ./lib.sh\n[ \"$(f9)\" = v9 ]\n")
	gitIn(t, p.repo, "add", "-A")
	gitIn(t, p.repo, "commit", "-q", "-m", "Add f9 to the library\n\nThe f9 function prints v9 for the status bar.")
	expect(t, p.at(2*time.Hour, "pool", "update"), ExitOK, "1 commit(s) read since the last pass, 1 candidate(s)", "Imported 1 of 1")
	status := p.at(3*time.Hour, "pool", "status")
	expect(t, status, ExitOK, "Task pool: 4 task(s)", "last pass "+p.clock.Add(2*time.Hour).Format("2006-01-02 15:04 UTC"))
	health(t, status, "valid", 4, "awaiting review", 4)
	p.noAgent(t)

	// The canary works: a command that does start Claude Code leaves its mark.
	p.at(4*time.Hour, "run", "once", p.name(t, "add-f9-"))
	if _, err := os.Stat(p.canary); err != nil {
		t.Errorf("run once left no canary mark: %v", err)
	}
}

// --accept-mined accepts only this pass's valid imports that the automatic checks pass; one whose message names a
// reference file is held back, and a task imported by hand is never accepted.
func TestPoolUpdateAcceptMined(t *testing.T) {
	t.Parallel()
	p := newPoolFixture(t, 3, 1)
	expect(t, p.at(0, "task", "import", "--commit", "HEAD", "--name", "by-hand"), ExitOK)
	got := p.at(0, "pool", "update", "--accept-mined")
	expect(t, got, ExitOK, "Imported 2 of 2", "Accepted 1 mined instruction(s) without your review (--accept-mined)",
		"held back from --accept-mined: ", "names reference file lib.sh")
	review := map[string]bool{}
	for _, tk := range p.tasks(t) {
		review[tk.Name] = tk.NeedsReview
	}
	if len(review) != 3 || !review["by-hand"] {
		t.Errorf("review flags %v: the hand-imported task must still need its review", review)
	}
	accepted := 0
	for _, r := range review {
		if !r {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("review flags %v, want one accepted", review)
	}
	if p.at(time.Hour, "pool", "update", "--dry-run", "--accept-mined").code != ExitUsage {
		t.Error("--dry-run --accept-mined is a usage error")
	}
	p.noAgent(t)
}

// tree lists the data folder's folders, and its files with their sizes and modification times (SQLite's write-ahead log
// and shared-memory index aside, which opening the database makes).
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, "-shm") || strings.HasSuffix(path, "-wal") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() { // a folder's time changes with SQLite's own files in it
			fmt.Fprintln(&b, path)
			return nil
		}
		fmt.Fprintf(&b, "%s %d %v\n", path, info.Size(), info.ModTime())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// --dry-run lists what a pass would import, re-validate and retire, and writes nothing: no task, no state file, no
// validation, no retirement.
func TestPoolUpdateDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	p := newPoolFixture(t, 3, 0)
	before := tree(t, p.data)
	dry := p.at(0, "pool", "update", "--dry-run", "--limit", "2")
	expect(t, dry, ExitOK, "Dry run: nothing is imported", "3 candidate(s)", "1 more candidate(s)", "Task pool: 0 task(s)")
	if after := tree(t, p.data); after != before {
		t.Errorf("the dry run wrote to the data folder:\nbefore\n%s\nafter\n%s", before, after)
	}
	if len(p.tasks(t)) != 0 {
		t.Error("the dry run imported tasks")
	}

	expect(t, p.at(0, "pool", "update"), ExitOK)
	gitIn(t, p.repo, "rm", "-q", "tests/f1_test.sh")
	gitIn(t, p.repo, "commit", "-q", "-m", "Drop the f1 test")
	before, tasks := tree(t, p.data), p.tasks(t)
	dry = p.at(31*pool.Day, "pool", "update", "--dry-run")
	expect(t, dry, ExitOK, "Would retire "+p.name(t, "add-f1-")+": tests/f1_test.sh is gone from the default branch",
		"Would re-validate "+p.name(t, "add-f2-")+": validated on", "more than 30 days ago")
	if after := tree(t, p.data); after != before || !slices.EqualFunc(p.tasks(t), tasks, func(a, b store.Task) bool {
		return a.RetiredAt.Equal(b.RetiredAt) && bytes.Equal(a.Validation, b.Validation)
	}) {
		t.Errorf("the dry run changed the data folder or the tasks")
	}
	p.noAgent(t)
}

// The stale and retire rules, end to end with a fake clock: a validation 29 days old stays, one 31 days old is made
// again; a task whose hidden test is gone from the default branch retires, as do tasks whose base is 271 days old (not
// 269), each with its reason; a retired task is left alone afterwards.
func TestPoolUpdateStaleAndRetireRules(t *testing.T) {
	t.Parallel()
	p := newPoolFixture(t, 3, 0)
	expect(t, p.at(0, "pool", "update"), ExitOK)
	first := validatedAt(p.tasks(t))

	day29 := p.at(29*pool.Day, "pool", "update")
	expect(t, day29, ExitOK)
	if strings.Contains(day29.stdout, "Re-validating") || !sameTimes(validatedAt(p.tasks(t)), first) {
		t.Errorf("29 days: re-validated:\n%s", day29.stdout)
	}
	gitIn(t, p.repo, "rm", "-q", "tests/f1_test.sh")
	gitIn(t, p.repo, "commit", "-q", "-m", "Drop the f1 test")
	day31 := p.at(31*pool.Day, "pool", "update")
	f1, f2 := p.name(t, "add-f1-"), p.name(t, "add-f2-")
	expect(t, day31, ExitOK, "Re-validating 2 stale task(s)", "more than 30 days ago",
		"Retired "+f1+": tests/f1_test.sh is gone from the default branch")
	health(t, day31, "valid", 2, "retired", 1)
	for _, tk := range p.tasks(t) {
		switch at := task.ValidationOf(tk).At; {
		case tk.Name == f1:
			if !tk.Retired() || tk.RetiredReason != "tests/f1_test.sh is gone from the default branch" || !at.Equal(first[tk.Name]) {
				t.Errorf("f1: retired %v (%q), validated %v", tk.Retired(), tk.RetiredReason, at)
			}
		case !at.Equal(p.clock.Add(31 * pool.Day)):
			t.Errorf("%s: validated at %v, want the 31st day", tk.Name, at)
		}
	}

	health(t, p.at(269*pool.Day, "pool", "update"), "retired", 1)
	day271 := p.at(271*pool.Day, "pool", "update")
	expect(t, day271, ExitOK, "Retired "+f2+": its base commit is 271 days old (270 or more retire)")
	health(t, day271, "valid", 0, "retired", 3)
	if strings.Contains(day271.stdout, "Retired "+f1) {
		t.Errorf("a retired task retired again:\n%s", day271.stdout)
	}
	p.noAgent(t)
}

func sameTimes(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !b[k].Equal(v) {
			return false
		}
	}
	return true
}

// A stale task that a locked, unfinished experiment uses is kept as it is and reported; the others are re-validated.
func TestPoolUpdateKeepsTasksInUse(t *testing.T) {
	t.Parallel()
	p := newPoolFixture(t, 2, 0)
	expect(t, p.at(0, "pool", "update"), ExitOK)
	first := validatedAt(p.tasks(t))
	db, project := p.db(t)
	ctx := context.Background()
	e, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: project, Name: "lean", Template: "context-ab", Design: []byte(`{}`), CreatedAt: p.clock})
	if err != nil {
		t.Fatal(err)
	}
	f1, f2 := p.name(t, "add-f1-"), p.name(t, "add-f2-")
	if err := db.LockExperiment(ctx, e.ID, []byte(`{"tasks":[{"name":"`+f1+`"}]}`)); err != nil {
		t.Fatal(err)
	}
	got := p.at(31*pool.Day, "pool", "update")
	expect(t, got, ExitOK, f1+": kept for experiment lean, which uses it", "Re-validating 1 stale task(s)")
	at := validatedAt(p.tasks(t))
	if !at[f1].Equal(first[f1]) || at[f2].Equal(first[f2]) {
		t.Errorf("validations %v, first %v", at, first)
	}
	p.noAgent(t)
}

// While an experiment's runs hold the run lock, a foreground pass warns and skips the re-validations (they would slow
// the runs); it still mines and retires.
func TestPoolUpdateSkipsRevalidationsWhileRunsAreBusy(t *testing.T) {
	t.Parallel()
	p := newPoolFixture(t, 2, 0)
	expect(t, p.at(0, "pool", "update"), ExitOK)
	first := validatedAt(p.tasks(t))
	release, err := home.Layout{Root: p.data}.LockRuns()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	lib, err := os.ReadFile(filepath.Join(p.repo, "lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.repo, "lib.sh", string(lib)+"f9() { echo v9; }\n")
	writeFile(t, p.repo, "tests/f9_test.sh", ". ./lib.sh\n[ \"$(f9)\" = v9 ]\n")
	gitIn(t, p.repo, "add", "-A")
	gitIn(t, p.repo, "commit", "-q", "-m", "Add f9 to the library\n\nThe f9 function prints v9 for the status bar.")
	got := p.at(31*pool.Day, "pool", "update")
	expect(t, got, ExitOK, "an experiment is running: 2 stale task(s) are not re-validated now",
		"an experiment is running: the new tasks are validated one at a time, not 2", "Validating 1 mined task(s), 1 at a time")
	now := validatedAt(p.tasks(t))
	for name, at := range first {
		if !now[name].Equal(at) {
			t.Errorf("%s re-validated while runs were busy", name)
		}
	}
	if strings.Contains(got.stdout, "Re-validating") {
		t.Errorf("re-validated while runs were busy:\n%s", got.stdout)
	}
	doc := checkJSON(t, p.runFixture, p.at(31*pool.Day, "pool", "update", "--json"), ExitOK, []string{"pool", "update"})
	if doc.get("revalidations_skipped") != true || len(doc.get("revalidated").([]any)) != 2 {
		t.Errorf("JSON: %s", doc.stdout)
	}
	p.noAgent(t)
}

// The documents' keys are a public contract (A1): fixed here.
func TestPoolJSONKeys(t *testing.T) {
	t.Parallel()
	p := newPoolFixture(t, 2, 0)
	const updateKeys = "accepted,candidates,candidates_found,command,commits_read,complete,dry_run,head,health,held_back,imported,interrupted,kept,outside_window,ref," +
		"retired,revalidated,revalidations_skipped,schema,set_aside,tasks,unknown_watermark,verify,warnings,watermark_moved"
	const healthKeys = "awaiting_review,flaky,invalid,last_pass,oldest_valid_base,retired,total,unchecked,unvalidated,valid,weak"
	run := func(code int, offset time.Duration, args ...string) jsonResult {
		t.Helper()
		return checkJSON(t, p.runFixture, p.at(offset, append(args, "--json")...), code, args)
	}
	dry := run(ExitOK, 0, "pool", "update", "--dry-run")
	assertKeys(t, dry.doc, updateKeys)
	assertKeys(t, dry.get("candidates").([]any)[0], "changed_lines,code_files,commit,score,score_parts,subject,test_files")
	assertKeys(t, dry.get("health"), healthKeys)
	if dry.get("health", "last_pass") != nil || dry.get("dry_run") != true {
		t.Errorf("dry run: %s", dry.stdout)
	}
	done := run(ExitOK, 0, "pool", "update")
	assertKeys(t, done.doc, updateKeys)
	assertKeys(t, done.get("tasks").([]any)[0], "commit,name,problem,task")
	if done.get("watermark_moved") != true || len(done.get("imported").([]any)) != 2 || done.get("health", "valid") != 2.0 {
		t.Errorf("update: %s", done.stdout)
	}
	gitIn(t, p.repo, "rm", "-q", "tests/f1_test.sh")
	gitIn(t, p.repo, "commit", "-q", "-m", "Drop the f1 test")
	later := run(ExitOK, 31*pool.Day, "pool", "update")
	assertKeys(t, later.get("revalidated").([]any)[0], "experiments,name,problem,reasons,status")
	assertKeys(t, later.get("retired").([]any)[0], "name,reason")
	if later.get("revalidated").([]any)[0].(map[string]any)["status"] != "valid" {
		t.Errorf("re-validated: %s", later.stdout)
	}
	status := run(ExitOK, 31*pool.Day, "pool", "status")
	assertKeys(t, status.doc, "command,health,schema")
	assertKeys(t, status.get("health"), healthKeys)
	if status.get("health", "retired") != 1.0 || status.get("health", "oldest_valid_base") == nil {
		t.Errorf("status: %s", status.stdout)
	}
	usage := run(ExitUsage, 0, "pool", "update", "--limit", "0")
	assertKeys(t, usage.doc, "command,error,schema")
	p.noAgent(t)
}

// task validate records the host's versions of the project's build tools (here Go, detected from go.mod), which the
// pool later compares to find validations made with other versions.
func TestTaskValidateRecordsTheToolchain(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	writeFile(t, f.repo, "go.mod", "module example.com/value\n\ngo 1.27\n")
	expect(t, f.run(context.Background(), "task", "validate", "value"), ExitOK)
	p := &poolFixture{runFixture: f}
	for _, tk := range p.tasks(t) {
		if v := task.ValidationOf(tk); !strings.HasPrefix(v.Toolchain["go"], "go1.") {
			t.Errorf("%s: toolchain %v", tk.Name, v.Toolchain)
		}
	}
}

// --accept-mined with an unreadable state file accepts nothing, and says so in text and JSON; the pass itself goes on.
func TestPoolUpdateAcceptMinedRefusesAnUnreadableState(t *testing.T) {
	t.Parallel()
	p := newPoolFixture(t, 2, 0)
	expect(t, p.at(0, "pool", "status"), ExitOK)
	_, project := p.db(t)
	file := pool.StateFile(filepath.Join(p.data, "projects", fmt.Sprint(project), "repo.git"))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := checkJSON(t, p.runFixture, p.at(0, "pool", "update", "--accept-mined", "--json"), ExitOK, []string{"pool", "update", "--accept-mined"})
	warnings := fmt.Sprint(doc.get("warnings"))
	if len(doc.get("accepted").([]any)) != 0 || len(doc.get("imported").([]any)) != 2 ||
		!strings.Contains(warnings, "--accept-mined accepted nothing: the pool's state file was unreadable") {
		t.Errorf("JSON: %s", doc.stdout)
	}
	for _, tk := range p.tasks(t) {
		if !tk.NeedsReview {
			t.Errorf("%s was accepted", tk.Name)
		}
	}
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.repo, "tests/f9_test.sh", "true\n")
	writeFile(t, p.repo, "lib9.sh", "f9() { :; }\n")
	gitIn(t, p.repo, "add", "-A")
	gitIn(t, p.repo, "commit", "-q", "-m", "Add f9, a no-op for the status bar")
	expect(t, p.at(time.Hour, "pool", "update", "--accept-mined"), ExitOK, "unreadable", "so --accept-mined accepts nothing this time")
	p.noAgent(t)
}

// Without a detected test command the pass mines nothing, says why, and still retires and re-validates.
func TestPoolUpdateWithoutTestCommandsStillMaintains(t *testing.T) {
	t.Parallel()
	p := newPoolFixture(t, 2, 0)
	expect(t, p.at(0, "pool", "update"), ExitOK)
	gitIn(t, p.repo, "rm", "-q", "Makefile", "tests/f1_test.sh")
	gitIn(t, p.repo, "commit", "-q", "-m", "Drop the Makefile and the f1 test")
	db, _ := p.db(t)
	projects, err := db.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveProject(context.Background(), projects[0].Root, projects[0].Name, []byte(`{}`), p.clock); err != nil { // no test commands found
		t.Fatal(err)
	}
	got := p.at(31*pool.Day, "pool", "update")
	expect(t, got, ExitOK, "no test commands were detected for this project, so the pass mines nothing", "Re-validating 1 stale task(s)",
		"Retired "+p.name(t, "add-f1-")+": tests/f1_test.sh is gone from the default branch")
	if strings.Contains(got.stdout, "Imported") {
		t.Errorf("mined without a test command:\n%s", got.stdout)
	}
	p.noAgent(t)
}
