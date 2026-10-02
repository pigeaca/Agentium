package cli

import (
	"context"
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
