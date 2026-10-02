package experiment

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// PairRun is a pair's settled run in one arm.
type PairRun struct {
	ID   string
	Slot int
	Rec  run.Record
}

// PairRuns is one pair of the schedule (Slot.Pair): a task's run in each arm with the same repeat index, the slots the
// paired statistics already pair. A and B are each arm's settled run (Settles), nil while the arm's slot has none; a
// slot settles once, so it has at most one.
type PairRuns struct {
	Pair   int
	Task   string
	Repeat int
	A, B   *PairRun
}

// PairsOf pairs an experiment's stored runs by its lock's schedule, in the schedule's order of pairs. Runs of no slot of
// the schedule, and runs that did not settle (infrastructure failures, cancelled runs), are left out.
func PairsOf(lock Lock, runs []store.Run) ([]PairRuns, error) {
	if len(lock.Design.Arms) != 2 {
		return nil, fmt.Errorf("an experiment has two arms, not %d", len(lock.Design.Arms))
	}
	armA, armB := lock.Design.Arms[0].Name, lock.Design.Arms[1].Name
	var out []PairRuns
	index := map[int]int{} // pair → its place in out
	for _, s := range lock.Schedule {
		if _, seen := index[s.Pair]; !seen {
			index[s.Pair] = len(out)
			out = append(out, PairRuns{Pair: s.Pair, Task: s.Task, Repeat: s.Repeat})
		}
	}
	for _, r := range runs {
		if r.Slot < 0 || r.Slot >= len(lock.Schedule) || !Settles(r.Outcome) {
			continue
		}
		slot := lock.Schedule[r.Slot]
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return nil, fmt.Errorf("run %s: %w", r.ID, err)
		}
		p := &out[index[slot.Pair]]
		settled := &PairRun{ID: r.ID, Slot: r.Slot, Rec: rec}
		switch slot.Arm {
		case armA:
			p.A = settled
		case armB:
			p.B = settled
		}
	}
	return out, nil
}

// Comparable reports whether the pair judge compares the pair: both runs count as passing (Success: fair, passing the
// hidden tests, without test-runner configuration changed beyond the reference's), and the task has a reference
// solution in code to compare them against. Quality is never compared across failing runs.
func (p PairRuns) Comparable(lock Lock) bool {
	if p.A == nil || p.B == nil {
		return false
	}
	t, ok := lock.Task(p.Task)
	if !ok || !run.HasReferenceCode(t.Spec()) {
		return false
	}
	for _, r := range []*PairRun{p.A, p.B} {
		if !Success(r.Rec.Outcome, r.Rec.Passed, r.Rec.Behavior.ConfigChanged) {
			return false
		}
	}
	return true
}

// NeedsComparing reports whether the pair is still to be compared, as a resume decides it: an experiment with the pair
// judge, a Comparable pair, and no comparison on its arm-B run that ran its course (run.NeedsPairJudging).
func (p PairRuns) NeedsComparing(lock Lock) bool {
	return lock.Design.JudgePairs != nil && p.Comparable(lock) && run.NeedsPairJudging(p.B.Rec)
}

// uncompared counts the pairs still to be compared.
func uncompared(lock Lock, runs []store.Run) (int, error) {
	if lock.Design.JudgePairs == nil {
		return 0, nil
	}
	pairs, err := PairsOf(lock, runs)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pairs {
		if p.NeedsComparing(lock) {
			n++
		}
	}
	return n, nil
}

// PairPreference summarises an experiment's pair comparisons, for the report. Its figures are unvalidated and
// exploratory: they never make a verdict.
//   - Pairs counts the comparisons one by one: complete, incomplete, empty, ties and flips (the flip rate is a pair's
//     property: its two orders disagreed).
//   - Tasks is the preference, clustered by task. With more than one run per arm, a task's pairs share its difficulty,
//     its reference and often the same fix, so they are not independent: counted apart, they would make the interval
//     too narrow and the test too ready to see a preference. Each task with a complete comparison gets one vote: the
//     arm its comparisons preferred more often, or a tie when they preferred neither more (ties and flips prefer
//     neither). The share, its Wilson interval, the exact binomial test and the floor of judge.MinPreferences count
//     these votes. With one run per arm (seq-v1) a task has one pair, so Tasks and Pairs agree on the preference.
type PairPreference struct {
	Pairs judge.PreferenceSummary
	Tasks judge.PreferenceSummary
}

// PairPreferenceOf summarises the comparisons stored on pairs (those without one are left out).
func PairPreferenceOf(pairs []PairRuns) PairPreference {
	var verdicts []judge.PairVerdict
	byTask := map[string][]judge.PairVerdict{}
	var tasks []string
	for _, p := range pairs {
		if p.B == nil || p.B.Rec.PairJudge == nil {
			continue
		}
		v := p.B.Rec.PairJudge.Verdict
		verdicts = append(verdicts, v)
		if _, seen := byTask[p.Task]; !seen {
			tasks = append(tasks, p.Task)
		}
		byTask[p.Task] = append(byTask[p.Task], v)
	}
	var votes []judge.PairVerdict
	for _, t := range slices.Sorted(slices.Values(tasks)) {
		votes = append(votes, taskVote(byTask[t]))
	}
	return PairPreference{Pairs: judge.Preference(verdicts), Tasks: judge.Preference(votes)}
}

// taskVote is a task's comparisons as one: the arm they preferred more often, a tie when neither, incomplete when none
// is complete, and Empty when every one is.
func taskVote(verdicts []judge.PairVerdict) judge.PairVerdict {
	a, b, complete, empty := 0, 0, 0, 0
	for _, v := range verdicts {
		switch {
		case v.Empty:
			empty++
		case v.Complete():
			complete++
			switch v.Prefer {
			case judge.PreferA:
				a++
			case judge.PreferB:
				b++
			}
		}
	}
	switch {
	case empty == len(verdicts):
		return judge.PairVerdict{Empty: true}
	case complete == 0:
		return judge.PairVerdict{}
	case a > b:
		return judge.PairVerdict{Prefer: judge.PreferA}
	case b > a:
		return judge.PairVerdict{Prefer: judge.PreferB}
	}
	return judge.PairVerdict{Prefer: judge.PreferTie}
}
