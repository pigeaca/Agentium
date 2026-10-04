package experiment

import (
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/task"
)

// TestRunDataOfMarksPendingGrades: looks, the report and the north star read a pending grade only through RunDataOf
// (slotsDone waits for it; the analysis leaves the run out until it is graded), so the mapping must carry
// run.NeedsGrading. Without it a seq-v1 look would settle a slot whose grade can still change.
func TestRunDataOfMarksPendingGrades(t *testing.T) {
	yes := true
	cases := []struct {
		name string
		rec  run.Record
		want bool
	}{
		{"judge-graded, not graded yet", run.Record{Outcome: claude.OutcomeOK, GradedBy: task.GradingJudge}, true},
		{"judge-graded, graded", run.Record{Outcome: claude.OutcomeOK, GradedBy: task.GradingJudge, Passed: &yes}, false},
		{"judge-graded, left ungraded for good", run.Record{Outcome: claude.OutcomeOK, GradedBy: task.GradingJudge, Ungraded: "tie"}, false},
		{"test-graded, verification did not run", run.Record{Outcome: claude.OutcomeOK}, false},
	}
	for _, c := range cases {
		if got := RunDataOf(3, c.rec); got.Pending != c.want {
			t.Errorf("%s: Pending = %v, want %v", c.name, got.Pending, c.want)
		}
	}
}
