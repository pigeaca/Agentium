package experiment

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// Pending grades. A judge-graded run whose grading ended in an error (a call that could not be made, an outage, the
// usage limit, an interrupt, or Agentium dying while the judge graded it) keeps the agent's run: it settles its slot,
// so the scheduler never runs the agent again, and its grade stays pending (run.NeedsGrading). The execution grades it
// again from its stored change, never by a new run:
//   - before the runs of an experiment that is not seq-v1, as it judges what a stopped execution left unjudged (resume
//     and crash recovery);
//   - before each of a seq-v1 experiment's stages and looks, whose looks wait for every grade of their stages
//     (slotsDone), the first of them on a resume;
//   - after the runs of any other experiment, up to run.MaxGradeErrors passes.
// A pass that follows one which left grades pending for an error waits the retry backoff first.
// run.MaxGradeErrors bounds the attempts that end in errors, so an outage cannot loop; a usage limit pauses the
// experiment instead, without counting. A refusal, a malformed reply or a tie is final: the run is left ungraded, which
// the analysis counts per arm (UngradedCheck).

// pendingGrades counts the stored runs whose grade is pending.
func pendingGrades(lock Lock, runs []store.Run) (int, error) {
	if !lock.JudgeGraded() {
		return 0, nil
	}
	n := 0
	for _, s := range runs {
		var rec run.Record
		if err := json.Unmarshal(s.Record, &rec); err != nil {
			return 0, fmt.Errorf("run %s: %w", s.ID, err)
		}
		if _, ok := lock.Task(s.TaskName); ok && run.NeedsGrading(rec) {
			n++
		}
	}
	return n, nil
}

// regradeResult is what a pass over the pending grades did.
type regradeResult struct {
	note      string // a pause note: a grade stopped at a usage limit
	unfunded  int    // grades left pending because the budget leaves no room for an attempt
	attempted int    // attempts made
	pending   int    // grades still pending after the pass
	spentUSD  float64
}

// gradePending grades again, one at a time, the stored runs whose grade is pending (Env.Regrade), each from its stored
// records, and stores each with what its attempt left (its record and passed column; runs[i] too), so the spend that
// follows counts it. An attempt starts only when the spend so far (calibrationUSD, the runs' and what the pair judge
// stored and holds outside them) and the grading's cap (Design.GradingCapUSD) fit the budget; one that does not is left
// for a resume with a higher budget. A grade stopped at a usage limit ends the pass with a pause note. The error is
// only a record that cannot be read or stored. A cancelled ctx ends the pass quietly: what follows sees it.
func (x *execution) gradePending(ctx context.Context, runs []store.Run, calibrationUSD float64) (regradeResult, error) {
	var out regradeResult
	lock, r := x.lock, x.r
	if !lock.JudgeGraded() {
		return out, nil
	}
	design := lock.Design
	spent := calibrationUSD
	for _, s := range runs {
		spent += storedSpend(s).TotalUSD()
	}
	for i, s := range runs {
		t, ok := lock.Task(s.TaskName)
		if !ok {
			continue
		}
		var rec run.Record
		if err := json.Unmarshal(s.Record, &rec); err != nil {
			return out, fmt.Errorf("run %s: %w", s.ID, err)
		}
		if !run.NeedsGrading(rec) {
			continue
		}
		outside := 0.0
		if x.pairs != nil {
			unread, held := x.pairs.outside()
			outside = unread + held
		}
		if ctx.Err() != nil || out.note != "" {
			out.pending++
			continue
		}
		if spent+outside+design.GradingCapUSD() > design.BudgetUSD+1e-9 {
			out.unfunded++
			out.pending++
			continue
		}
		before := rec.Spend().JudgeUSD
		save := func(rec run.Record) error {
			encoded, err := json.Marshal(rec)
			if err != nil {
				return fmt.Errorf("encode run %s: %w", s.ID, err)
			}
			if err := r.Project.DB.SetRunGrade(context.WithoutCancel(ctx), s.ID, encoded, rec.Passed); err != nil {
				return err
			}
			runs[i].Record, runs[i].Passed = encoded, rec.Passed
			return nil
		}
		x.runEnv.Regrade(ctx, run.Spec{TaskName: t.Name, Instruction: t.Instruction, Task: t.Spec(), JudgeGrading: design.JudgeGrading}, &rec, save)
		out.attempted++
		spent += rec.Spend().JudgeUSD - before
		out.spentUSD += rec.Spend().JudgeUSD - before
		if err := save(rec); err != nil {
			return out, err
		}
		words := run.GradeWords(rec)
		fmt.Fprintf(r.Out, "Graded run %s again (task %s, arm %s; the agent did not run again): %s, $%.2f (spent $%.2f of $%.2f)\n",
			s.ID, s.TaskName, s.Arm, words, rec.Spend().JudgeUSD-before, spent, design.BudgetUSD)
		if run.NeedsGrading(rec) {
			out.pending++
			if v := rec.Judge; v != nil && v.Stopped == llmjudge.StoppedLimit {
				out.note = judgeLimitNote
			}
		}
	}
	if out.unfunded > 0 {
		fmt.Fprintln(r.Out, r.Style.Warn(fmt.Sprintf("%d run(s) still wait for the judge's grade, but the budget leaves no room for one ($%.2f): raise it with --budget",
			out.unfunded, design.GradingCapUSD())))
	}
	return out, nil
}

// regradeAfterRuns grades the pending grades again after an execution's runs, up to run.MaxGradeErrors passes while a
// pass makes attempts and leaves grades pending: a grade that errs on every pass is then ungraded for good (Regrade).
// Passes after the first wait the retry backoff (x.regradeWait), so a brief outage does not use up the attempts. It
// stops at a usage limit (pausing the execution), when the budget leaves no room, or when ctx ends, and returns what
// it spent.
func (x *execution) regradeAfterRuns(ctx context.Context, calibrationUSD float64) (spentUSD float64, err error) {
	for pass := range run.MaxGradeErrors {
		if pass > 0 && !x.regradeWait(ctx, pass) {
			return spentUSD, nil
		}
		runs, err := x.storedRuns(ctx)
		if err != nil {
			return spentUSD, err
		}
		if n, err := pendingGrades(x.lock, runs); err != nil || n == 0 {
			return spentUSD, err
		}
		res, err := x.gradePending(ctx, runs, calibrationUSD)
		spentUSD += res.spentUSD
		if err != nil {
			return spentUSD, err
		}
		if res.note != "" {
			x.judgePaused.Store(true)
			return spentUSD, nil
		}
		if res.pending == 0 || res.attempted == 0 {
			return spentUSD, nil
		}
	}
	return spentUSD, nil
}

// regradeWait waits before the pass-th pass (from 1) that grades pending grades again after one that left some for an
// error: the scheduler's retry backoff (Runner.Backoff; 30 seconds, then 2 minutes). It reports false when ctx ended.
func (x *execution) regradeWait(ctx context.Context, pass int) bool {
	backoff := x.r.Backoff
	if backoff == nil {
		backoff = retryBackoff
	}
	t := time.NewTimer(backoff(pass))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
