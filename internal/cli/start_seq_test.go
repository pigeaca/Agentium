package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// start aims for the cost experiment's 16 tasks and takes 8 or more when the candidates run out (the user's decision,
// 2026-10-02):
//   - 16 or more candidates: 16 tasks, looks after 8, 12 and 16;
//   - 11 candidates: 11 tasks. The design's planned maximum is 11, so it has one look, after all 11, at the
//     sequential level (3.5%): the 8–11 rule of planned looks. The lost-task rule does not apply: no planned task is
//     lost, the plan is smaller;
//   - 7 candidates: too few, as before.
func TestStartAimsForSixteenTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct {
		candidates int
		want       []string
	}{
		{18, []string{"imported 16 of 16 tried", "Tasks: 16 ready (aims for 16), in ", "created, 16 task(s) × 1 run per arm = 32 runs at most",
			"looks after 8, 12 and 16 tasks", "Looks (method seq-v1; runs count both arms):", "3 of 3"}},
		{11, []string{"imported 11 of 11 tried", "Tasks: 11 ready (aims for 16; the history has no more candidates), in ",
			"the experiment takes them all, one look, after all 11 tasks (a fixed design at the sequential level)",
			"created, 11 task(s) × 1 run per arm = 22 runs at most", "1 of 1", "96.50%"}},
	} {
		f, _ := startFixture(t, c.candidates)
		got := f.run(ctx, "start", "--accept-mined")
		expect(t, got, ExitOK, c.want...)
	}
	f, _ := startFixture(t, 7)
	got := f.run(ctx, "start", "--accept-mined")
	expect(t, got, ExitError, "7 valid of 7", "only 7 of the 8 an experiment needs are ready", "the history has no more candidates")
	if strings.Contains(got.stdout, "Experiment ") {
		t.Errorf("an experiment was made from 7 tasks:\n%s", got.stdout)
	}
}

// A first start mines 11 tasks and stops for their review, listing every one; the next start finds them, mines
// nothing new (the history has no other candidates) and takes all 11.
func TestStartResumedDoesNotMineAgain(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 11)
	ctx := context.Background()
	first := f.run(ctx, "start")
	expect(t, first, ExitError, "imported 11 of 11 tried", "11 valid, 0 ready: the others wait for your review", "agentium task rm NAME")
	tasks := storedTasks(t, f.data)
	if len(tasks) != 11 {
		t.Fatalf("%d tasks after the first start", len(tasks))
	}
	for _, task := range tasks {
		if !strings.Contains(first.stdout, task.Name) {
			t.Errorf("the review list leaves out %s:\n%s", task.Name, first.stdout)
		}
	}
	again := f.run(ctx, "start", "--accept-mined")
	expect(t, again, ExitOK, "Accepted 11 mined instruction(s)", "Tasks: 11 ready", "created, 11 task(s)")
	if strings.Contains(again.stdout, "imported") || strings.Contains(again.stdout, "Validating") || len(storedTasks(t, f.data)) != 11 {
		t.Errorf("the resumed start mined or validated again:\n%s", again.stdout)
	}
}

// start --json tells a start waiting for reviews from one with too few tasks: 10 ready and 6 of its mined tasks
// waiting is awaiting_review, with both counts, not too_few_tasks.
func TestJSONStartAwaitingReview(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 16)
	first := jsonRun(t, f, ExitError, "start")
	if first.get("status") != "awaiting_review" || first.get("tasks_ready") != float64(0) || first.get("tasks_awaiting_review") != float64(16) {
		t.Errorf("all 16 waiting: %s", first.stdout)
	}
	for _, task := range storedTasks(t, f.data)[:10] {
		expect(t, f.run(context.Background(), "task", "edit", task.Name, "--reviewed"), ExitOK)
	}
	got := jsonRun(t, f, ExitError, "start")
	if got.get("status") != "awaiting_review" || got.get("tasks_ready") != float64(10) || got.get("tasks_awaiting_review") != float64(6) ||
		got.get("experiment") != nil {
		t.Errorf("10 ready, 6 waiting: %s", got.stdout)
	}
	short := jsonRun(t, startFixtureOnly(t, 2), ExitError, "start", "--accept-mined")
	if short.get("status") != "too_few_tasks" || short.get("tasks_ready") != float64(2) || short.get("tasks_awaiting_review") != float64(0) {
		t.Errorf("2 tasks: %s", short.stdout)
	}
}

// startFixtureOnly is startFixture's project alone.
func startFixtureOnly(t *testing.T, features int) runFixture {
	t.Helper()
	f, _ := startFixture(t, features)
	return f
}

// A task the user imported and left waiting for review is theirs: it does not hold start back once 8 or more are
// ready and none of start's own mined tasks waits.
func TestStartIsNotHeldBackByTasksItDidNotMine(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 9)
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	head := strings.TrimSpace(gitIn(t, f.repo, "rev-parse", "HEAD"))
	expect(t, f.run(ctx, "task", "import", "--commit", head, "--name", "by-hand", "--verify", "make test"), ExitOK)
	expect(t, f.run(ctx, "start"), ExitError, "wait for your review")
	for _, task := range storedTasks(t, f.data) {
		if task.Name != "by-hand" {
			expect(t, f.run(ctx, "task", "edit", task.Name, "--reviewed"), ExitOK)
		}
	}
	got := f.run(ctx, "start")
	expect(t, got, ExitOK, "Tasks: 8 ready (aims for 16;", "Experiment quick-aa-baseline: created, 8 task(s)")
	for _, task := range storedTasks(t, f.data) {
		if task.Name == "by-hand" && !task.NeedsReview {
			t.Error("the hand-imported task was marked reviewed")
		}
	}
}

// Small mining rounds add up for the stop rule: with 14 tasks ready and every candidate left invalid, start mines two
// rounds of 2, stops after those 4 invalid tasks (the rule's sample is 3), and takes the 14, instead of mining round
// after round up to its cap.
func TestStartStopsMiningAfterInvalidRoundsAddUp(t *testing.T) {
	t.Parallel()
	f := runFixtureAt(startRepo(t, 14), filepath.Join(t.TempDir(), "data"), t.TempDir())
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, t.TempDir())
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	expect(t, f.run(ctx, "task", "mine", "--limit", "14"), ExitOK, "14 of 14 imported task(s) are valid")
	for _, task := range storedTasks(t, f.data) {
		expect(t, f.run(ctx, "task", "edit", task.Name, "--reviewed"), ExitOK)
	}
	for i := 1; i <= 20; i++ { // each such commit's test already passes on its base: the task is invalid
		lib, err := os.ReadFile(filepath.Join(f.repo, "lib.sh"))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, f.repo, "lib.sh", "# note "+strconv.Itoa(i)+"\n"+string(lib))
		writeFile(t, f.repo, "tests/b"+strconv.Itoa(i)+"_test.sh", ". ./lib.sh\n[ \"$(base)\" = base ]\n")
		gitIn(t, f.repo, "add", "-A")
		gitIn(t, f.repo, "commit", "-q", "-m", "Explain base again, "+strconv.Itoa(i)+"\n\nA comment says what base prints.")
	}
	got := f.run(ctx, "start", "--accept-mined")
	expect(t, got, ExitOK, "imported 2 of 2 tried", "0 valid of 2",
		"Tasks: 14 ready (aims for 16; the last 4 tasks mined are all invalid, so mining more would likely repeat that)",
		"Experiment quick-aa-baseline: created, 14 task(s)")
	if n := strings.Count(got.stdout, "Mining:"); n != 2 {
		t.Errorf("start mined %d rounds, want 2:\n%s", n, got.stdout)
	}
	if n := len(storedTasks(t, f.data)); n != 18 {
		t.Errorf("%d tasks, want the 14 and 4 invalid ones", n)
	}
}
