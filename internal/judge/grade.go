package judge

import (
	"fmt"

	"github.com/pigeaca/agentium/internal/claude"
)

// The judge as grader: a judge-graded task (task.GradingJudge) has no hidden tests, so its runs are graded by the
// judge's majority of GradeRepeats answers instead. Unlike the second opinion beside the tests, this verdict decides the
// run's pass or fail; it is unvalidated (the judge pilot's grading without tests was inconclusive), so experiments keep
// these grades apart from the tests' and give them no verdict (.agents/plans/2026-10-01-ticket-tasks.md).

// GradeRepeats is how many times the judge is asked about a judge-graded run: a pass or a fail needs a majority of them.
const GradeRepeats = 5

// GradingSettings are the judge that grades judge-graded runs: the pilot's model and effort, GradeRepeats calls.
func GradingSettings() Settings {
	return Settings{Model: DefaultModel, Effort: DefaultEffort, Repeats: GradeRepeats}
}

// CallCapFor is what one judge call on s may spend at most: CallCapUSD, with the overshoot allowance of a run on its
// model for any judge but the default one (DefaultModel at DefaultEffort), whose calls were measured well below the cap.
// Claude Code checks --max-budget-usd after a turn, so a call can pass its cap a little.
func CallCapFor(s Settings) float64 {
	s = s.WithDefaults()
	perCall := CallCapUSD
	if s.Model != DefaultModel || s.Effort != DefaultEffort {
		perCall += claude.CapOvershootUSD(CallCapUSD, s.Model)
	}
	return perCall
}

// CapUSD is what one judgement on s may spend at most: each repeat's call at its cap (CallCapFor), twice, since a
// malformed reply is asked again.
func CapUSD(s Settings) float64 {
	s = s.WithDefaults()
	return float64(s.Repeats) * 2 * CallCapFor(s)
}

// Grade reads a judge-graded run's grade from its verdict: passed when a majority of the requested repeats answered
// "yes", failed when a majority answered otherwise ("partly" or "no"), and a candidate that changed no code (Empty)
// fails, since a task with a reference in code cannot be done without changing code. Anything else is no grade: errors,
// a refusal, a usage limit or an interrupt left too few answers for a majority either way (ok false, with why in
// words). A grade is never a failure for want of answers: that is infrastructure, not the agent's result.
//
// Answers the judgement did not get cannot change a majority already reached, so a verdict stopped early still grades
// the run when its answers decide it (3 "yes" of 5 requested, say).
func Grade(v Verdict) (passed, ok bool, why string) {
	if v.Empty {
		return false, true, ""
	}
	requested := max(v.Requested, len(v.Answers))
	yes := 0
	for _, a := range v.Answers {
		if a == Yes {
			yes++
		}
	}
	other := len(v.Answers) - yes
	switch {
	case requested == 0:
		return false, false, "the judge was not asked"
	case yes*2 > requested:
		return true, true, ""
	case other*2 > requested:
		return false, true, ""
	case len(v.Answers) == requested:
		// Unreachable with an odd number of repeats; an even one can tie.
		return false, false, fmt.Sprintf("the judge's %d answers tie (%d fixed, %d not)", requested, yes, other)
	}
	why = fmt.Sprintf("the judge answered %d of %d times (%d fixed, %d not), too few for a majority either way", len(v.Answers), requested, yes, other)
	switch v.Stopped {
	case StoppedLimit:
		why += ": it stopped at a usage limit or a sign-in failure"
	case StoppedCall:
		why += ": it stopped early"
	}
	return false, false, why
}
