package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// setRecord replaces a stored run's record, as a crash at some step would have left it.
func setRecord(t *testing.T, f runFixture, id string, rec run.Record) {
	t.Helper()
	ctx := context.Background()
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetRunRecord(ctx, id, encoded); err != nil {
		t.Fatal(err)
	}
}

// pairedRuns are an experiment's arm-A and arm-B runs by repeat, and every run's record by ID.
func pairedRuns(t *testing.T, f runFixture, name string) (a, b map[int]store.Run, recs map[string]run.Record) {
	t.Helper()
	l := seqLockOf(t, f, name)
	a, b, recs = map[int]store.Run{}, map[int]store.Run{}, map[string]run.Record{}
	runs := experimentRuns(t, f, name)
	for i, rec := range records(t, runs) {
		r := runs[i]
		recs[r.ID] = rec
		slot := l.Schedule[r.Slot]
		if slot.Arm == "A" {
			a[slot.Repeat] = r
		} else {
			b[slot.Repeat] = r
		}
	}
	return a, b, recs
}

// --judge-pairs is validated with the other judge flags and locked with the design; the preview, the worst case and
// the budget's minimum include each pair's comparison.
func TestExperimentNewJudgePairs(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	newArgs := []string{"experiment", "new", "x", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2", "--seed", "5"}
	// A pair's two runs at $3.30 and its comparison's 4 calls at $0.50.
	expect(t, f.run(ctx, append(newArgs, "--judge-pairs", "--budget", "8.5")...), ExitUsage, "below one pair of runs at their caps ($8.60)")
	expect(t, f.run(ctx, append(newArgs, "--judge-pairs=claude-opus-5-5:max")...), ExitOK,
		"The pair judge: claude-opus-5-5 at effort max, both orders of each pair of passing runs; unvalidated, its preferences are exploratory and decide nothing.")
	if d := storedDesign(t, f, "x"); d.Judge != nil || d.JudgePairs == nil || *d.JudgePairs != (llmjudge.Settings{Model: "claude-opus-5-5", Effort: "max", Repeats: 1}) {
		t.Errorf("stored judges %+v, %+v", d.Judge, d.JudgePairs)
	}
	newArgs[2] = "pairs"
	expect(t, f.run(ctx, append(newArgs, "--judge-pairs")...), ExitOK)
	plan := f.run(ctx, "experiment", "plan", "pairs")
	expect(t, plan, ExitOK, "judge pairs: claude-opus-5-5 at effort high, both orders of each pair of passing runs; each comparison up to $2.00; unvalidated",
		"The pair judge (unvalidated): about $0.35 if it compares all 2 pairs at $0.176 a pair, both orders (EST. COST includes it;",
		"Its preferences are exploratory: never a verdict.",
		"Worst case: every run reaches its $3.00 cap. Each pair's comparison may reach $2.00 (4 calls at $0.50: both orders, each asked twice at most).")
	// This experiment: 4 runs at the default profile's $1.612 and 2 comparisons at $0.176; worst 2 × ($3.30 + $3.30 + $2).
	if !strings.Contains(plan.stdout, "$6.80      $17.20") {
		t.Errorf("this experiment's row, want $6.80 and the worst case $17.20:\n%s", plan.stdout)
	}
	t.Log("experiment plan with --judge-pairs:\n" + plan.stdout)
	doc := jsonRun(t, f, ExitOK, "experiment", "plan", "pairs")
	if worst, _ := doc.get("spend", "worst_case_usd").(float64); doc.get("experiment", "judge_pairs") != true || doc.get("experiment", "judge") != false || !near(worst, 17.2) {
		t.Errorf("experiment plan --json: %s", doc.stdout)
	}
	// The default budget holds the comparisons' estimate and their reserve: 1.25 × ($6.448 + $0.352) + 3 × ($3.30 + $2).
	expect(t, f.run(ctx, "experiment", "new", "sized", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2", "--judge-pairs"), ExitOK,
		"= 4 runs, budget $25.00")
}

// With --judge-pairs at concurrency 2, each pair of passing runs is compared in both orders beside the runs, its
// comparison stored on the arm-B run with its cost counted in the budget; a resume compares no pair twice and skips
// none: a comparison lost before it started, or stopped by a crash, is made again, keeping what the stopped one spent.
func TestExperimentJudgesPairs(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	control(t, ctrl, map[string]string{"pair-answer": "alternate"})
	expect(t, f.run(ctx, "experiment", "new", "pairs", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2", "--judge-pairs",
		"--seed", "5", "--budget", "30"), ExitOK)
	first := f.run(ctx, "experiment", "run", "pairs")
	expect(t, first, ExitOK, "The pair judge (unvalidated): claude-opus-5-5 at effort high", "Running up to 2 at a time; each run up to $3.00 and 20m0s; each pair's comparison up to $2.00; budget $30.00.", "Compared pair value, repeat 1 (pair judge, unvalidated): prefers B, $0.18",
		"Compared pair value, repeat 2 (pair judge, unvalidated): prefers B, $0.18", "Experiment pairs: done",
		"4 of 4 runs settled; spent $1.56 of $30.00 (the judge $0.36 of it, not in the arms' costs)", "Every run is done.")
	t.Log("experiment run with --judge-pairs:\n" + first.stdout)
	for _, arm := range []string{"A", "B"} {
		if row := armRow(first.stdout, arm); len(row) < 9 || row[8] != "$0.60" {
			t.Errorf("arm %s = %v: its cost is the agent's alone", arm, row)
		}
	}
	a, b, recs := pairedRuns(t, f, "pairs")
	for repeat := 1; repeat <= 2; repeat++ {
		ra, rb := recs[a[repeat].ID], recs[b[repeat].ID]
		c := rb.PairJudge
		if ra.PairJudge != nil || c == nil || c.RunA != a[repeat].ID {
			t.Fatalf("repeat %d: arm A holds %+v, arm B %+v (want B's, naming A's run %s)", repeat, ra.PairJudge, c, a[repeat].ID)
		}
		v := c.Verdict
		if v.Prefer != llmjudge.PreferB || v.Flip || !v.AB.Answered || v.AB.Answer != "second" || !v.BA.Answered || v.BA.Answer != "first" ||
			v.CostUSD < 0.1799 || v.CostUSD > 0.1801 || v.Stopped != "" || v.Model != "claude-opus-5-5" || v.Version != llmjudge.PairVersion {
			t.Errorf("repeat %d's comparison %+v", repeat, v)
		}
		if b[repeat].CostUSD != 0.30 || rb.Metrics.CostUSD != 0.30 || rb.Outcome != "ok" {
			t.Errorf("repeat %d's arm-B run: cost column $%v, metrics $%v, %s: the agent's own", repeat, b[repeat].CostUSD, rb.Metrics.CostUSD, rb.Outcome)
		}
		if _, err := os.Stat(filepath.Join(rb.RecordsDir, "pair-judge")); err == nil {
			t.Errorf("repeat %d's pair judge folder was left behind", repeat)
		}
	}
	// Both orders read both changes' code, never the hidden tests, in the arm-B run's records.
	prompts := judgeFiles(t, ctrl, "prompt")
	pairPrompts, _ := filepath.Glob(filepath.Join(ctrl, "pair-prompt-*"))
	if len(prompts) != 0 || len(pairPrompts) != 4 {
		t.Fatalf("%d judge calls and %d pair calls, want none and 2 pairs × 2 orders", len(prompts), len(pairPrompts))
	}
	for _, p := range pairPrompts {
		data, _ := os.ReadFile(p)
		if text := string(data); strings.Count(text, "diff --git a/value.txt b/value.txt") != 3 || strings.Contains(text, "value_test") ||
			!strings.Contains(text, "<first>") || !strings.Contains(text, "<second>") {
			t.Errorf("a pair prompt:\n%s", text)
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(ctrl, "pair-dir-*"))
	for _, p := range dirs {
		data, _ := os.ReadFile(p)
		if dir := string(data); !strings.Contains(dir, filepath.Join("records", "")) || !strings.Contains(dir, filepath.Join("pair-judge", "call-")) {
			t.Errorf("a pair call ran in %s, not in its arm-B run's records", dir)
		}
	}
	show := jsonRun(t, f, ExitOK, "experiment", "show", "pairs")
	pairUSD, _ := show.get("progress", "pair_judge_usd").(float64)
	judgeUSD, _ := show.get("progress", "judge_usd").(float64)
	if !near(pairUSD, 0.36) || !near(judgeUSD, 0.36) || show.get("progress", "uncompared_pairs") != 0.0 || show.get("experiment", "judge_pairs") != true {
		t.Errorf("experiment show --json: %s", show.stdout)
	}
	runShow := jsonRun(t, f, ExitOK, "run", "show", b[1].ID)
	if pairCost, _ := runShow.get("run", "pair_judge_cost_usd").(float64); !near(pairCost, 0.18) || runShow.get("run", "cost_usd") != 0.3 {
		t.Errorf("run show --json of an arm-B run: %s", runShow.stdout)
	}
	expect(t, f.run(ctx, "experiment", "show", "pairs"), ExitOK, "Pair judge: claude-opus-5-5 at effort high, both orders of each pair of passing runs (unvalidated, exploratory)")

	// The report: one vote for the one task, below the floor, in all three formats and the terminal view.
	expect(t, f.run(ctx, "experiment", "report", "pairs"), ExitOK, "## Judge pairs", "Judge prefers: too few to say (1 task with a preference, 5 needed).",
		"| value | B | 2 |")
	rep := jsonResult{cliResult: f.run(ctx, "experiment", "report", "pairs", "--json")}
	if err := json.Unmarshal([]byte(rep.stdout), &rep.doc); err != nil {
		t.Fatalf("experiment report --json: %v\n%s", err, rep.stdout)
	}
	if rep.get("pair_judge", "tasks", "b") != 1.0 || rep.get("pair_judge", "pairs", "complete") != 2.0 || rep.get("pair_judge", "uncompared") != 0.0 {
		t.Errorf("experiment report --json: %s", rep.stdout)
	}
	*f.terminal = true
	expect(t, f.run(ctx, "experiment", "report", "pairs"), ExitOK, "which fix is better: too few to say (1 task with a preference; it takes 5)")
	expect(t, f.run(ctx, "experiment", "report", "pairs", "--details"), ExitOK, "Judge pairs", "Judge prefers: too few to say")
	*f.terminal = false

	// Done: a resume compares nothing again.
	if again := f.run(ctx, "experiment", "run", "pairs"); strings.Contains(again.stdout, "Compared pair") {
		t.Errorf("a pair was compared twice:\n%s", again.stdout)
	}

	// Agentium died after storing repeat 1's arm-B run, before its comparison started; and while comparing repeat 2,
	// after a call that cost $0.05.
	lost := recs[b[1].ID]
	lost.PairJudge = nil
	setRecord(t, f, b[1].ID, lost)
	cut := recs[b[2].ID]
	cut.PairJudge = &run.PairJudgement{RunA: a[2].ID, Verdict: llmjudge.PairVerdict{Version: llmjudge.PairVersion, Model: "claude-opus-5-5",
		Effort: "high", CostUSD: 0.05, Stopped: llmjudge.StoppedCall, Errors: []string{"Agentium stopped while judging"}}}
	setRecord(t, f, b[2].ID, cut)
	expect(t, f.run(ctx, "experiment", "show", "pairs"), ExitOK, "2 pair(s) of passing runs still need the pair judge: agentium experiment run pairs")
	expect(t, f.run(ctx, "experiment", "report", "pairs"), ExitOK, "2 pair(s) still to compare: agentium experiment run pairs compares them.")
	resumed := f.run(ctx, "experiment", "run", "pairs")
	expect(t, resumed, ExitOK, "Compared pair value, repeat 1 (pair judge, unvalidated): prefers B, $0.18",
		"Compared pair value, repeat 2 (pair judge, unvalidated): prefers B, $0.18", "Experiment pairs: done",
		"spent $1.61 of $30.00 (the judge $0.41 of it") // 4 × $0.30 + 2 × $0.18 + the stopped comparison's $0.05
	if n := strings.Count(resumed.stdout, "Compared pair"); n != 2 || strings.Contains(resumed.stdout, "] value, arm") {
		t.Errorf("%d comparisons on resume, want 2 and no run:\n%s", n, resumed.stdout)
	}
	_, _, recs = pairedRuns(t, f, "pairs")
	if c := recs[b[2].ID].PairJudge; c == nil || c.Verdict.Stopped != "" || c.Verdict.CostUSD < 0.2299 || c.Verdict.CostUSD > 0.2301 {
		t.Errorf("repeat 2 compared again: %+v (its cost keeps the stopped comparison's $0.05)", c)
	}
	if again := f.run(ctx, "experiment", "run", "pairs"); strings.Contains(again.stdout, "Compared pair") {
		t.Errorf("a pair was compared twice:\n%s", again.stdout)
	}
}

// Pairs whose runs do not both pass are never compared; a comparison at a usage limit pauses the experiment, and the
// resume compares the pair when the budget leaves room for its cap, and says so when it does not.
func TestExperimentPairJudgeLimitAndBudget(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	// Runs at $5, past their caps: the $11 budget fits one pair with its comparison ($8.60) before any spend.
	control(t, ctrl, map[string]string{"cost": "5", "pair-limit": ""})
	expect(t, f.run(ctx, "experiment", "new", "last", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "1", "--judge-pairs",
		"--budget", "11"), ExitOK)
	paused := f.run(ctx, "experiment", "run", "last")
	expect(t, paused, ExitOK, "Compared pair value, repeat 1 (pair judge, unvalidated): no answer; stopped at a usage limit or sign-in failure, $0.01",
		"Experiment last: paused at the usage limit: the judge hit a usage limit", "2 of 2 runs settled",
		"1 pair(s) of passing runs still need the pair judge: agentium experiment run last", "Paused: the judge hit a usage limit or a sign-in failure.")
	// $10.01 spent: a comparison's $2 cap fits $11 no longer.
	os.Remove(filepath.Join(ctrl, "pair-limit"))
	unfunded := f.run(ctx, "experiment", "run", "last")
	expect(t, unfunded, ExitOK, "Experiment last: budget: 1 pair(s) still need comparing, but the budget leaves no room for a comparison ($2.00)",
		"1 pair(s) of passing runs still need the pair judge: agentium experiment run last --budget USD",
		"Stopped at the budget. To continue: agentium experiment run last --budget USD")
	if strings.Contains(unfunded.stdout, "Compared pair") {
		t.Errorf("a comparison past the budget:\n%s", unfunded.stdout)
	}
	expect(t, f.run(ctx, "experiment", "run", "last", "--budget", "20"), ExitOK, "Compared pair value, repeat 1 (pair judge, unvalidated): tie, $0.18",
		"Experiment last: done", "spent $10.19 of $20.00")
	_, b, recs := pairedRuns(t, f, "last")
	if c := recs[b[1].ID].PairJudge; c == nil || c.Verdict.Prefer != llmjudge.PreferTie || c.Verdict.CostUSD < 0.1899 || c.Verdict.CostUSD > 0.1901 {
		t.Errorf("the comparison %+v", c)
	}

	// A failing run leaves its pair uncompared, and nothing waits for it.
	control(t, ctrl, map[string]string{"cost": "0.30"})
	expect(t, f.run(ctx, "experiment", "new", "failing", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "1", "--judge-pairs"), ExitOK)
	control(t, ctrl, map[string]string{"no-change": ""})
	failing := f.run(ctx, "experiment", "run", "failing")
	expect(t, failing, ExitOK, "Experiment failing: done", "Every run is done.")
	if strings.Contains(failing.stdout, "Compared pair") || strings.Contains(failing.stdout, "still need the pair judge") {
		t.Errorf("a pair of failing runs was compared, or waits:\n%s", failing.stdout)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// A seq-v1 experiment with --judge-pairs compares each pair once both of its runs pass, beside the runs: its look does
// not wait for the comparisons, and an experiment that ends at the look leaves its unrun slots unpaired.
func TestSeqExperimentComparesPairsBesideItsLooks(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	ctx := context.Background()
	// Every run passes; every comparison waits for pair-block, which goes 3 s after stage 1's last run is stored.
	control(t, ctrl, map[string]string{"cost-lean": "0.15", "cost-jitter": "", "pair-block": "", "fix-lib": ""})
	expect(t, f.run(ctx, "experiment", "new", "lean-seq", "--b", "lean", "--seed", "5", "--judge-pairs"), ExitOK)
	go func() {
		if pollUntil(t, "16 stored runs", func() bool { return storedRunCount(f, "lean-seq") >= 16 }) {
			time.Sleep(3 * time.Second)
		}
		os.Remove(filepath.Join(ctrl, "pair-block"))
	}()
	out := f.run(ctx, "experiment", "run", "lean-seq")
	expect(t, out, ExitOK, "Look 1 of 3 (8 of 8 tasks counted): cost improved at 99.84%: stop",
		"Experiment lean-seq: done: stopped at look 1 of 3 (8 tasks): cost improved", "16 of 32 runs settled")
	t.Log("a seq-v1 run with --judge-pairs:\n" + out.stdout)
	if n := strings.Count(out.stdout, "Compared pair "); n != 8 {
		t.Errorf("%d comparisons, want stage 1's 8 pairs:\n%s", n, out.stdout)
	}
	// The comparisons were all waiting when the stage ended: the look did not wait for them.
	if look, first := strings.Index(out.stdout, "Look 1 of 3"), strings.Index(out.stdout, "Compared pair "); look < 0 || first < look {
		t.Errorf("the look waited for the comparisons:\n%s", out.stdout)
	}
	l := seqLockOf(t, f, "lean-seq")
	runs := experimentRuns(t, f, "lean-seq")
	compared := map[int]bool{}
	for i, rec := range records(t, runs) {
		slot := l.Schedule[runs[i].Slot]
		if slot.Stage != 1 {
			t.Errorf("slot %d of stage %d ran after the experiment ended at look 1", slot.Position, slot.Stage)
		}
		if rec.PairJudge != nil {
			if slot.Arm != "B" || compared[slot.Pair] || rec.PairJudge.Verdict.Stopped != "" || !rec.PairJudge.Verdict.Complete() {
				t.Errorf("slot %d: comparison %+v", slot.Position, rec.PairJudge)
			}
			compared[slot.Pair] = true
		}
	}
	if len(compared) != 8 {
		t.Errorf("%d pairs compared, want 8: the unrun slots have none", len(compared))
	}
	checkBarrier(t, l, runs)
	show := jsonRun(t, f, ExitOK, "experiment", "show", "lean-seq")
	if show.get("status") != "done" || show.get("progress", "uncompared_pairs") != 0.0 || show.get("progress", "ended_by") != "stop" {
		t.Errorf("experiment show --json: %s", show.stdout)
	}
	if again := f.run(ctx, "experiment", "run", "lean-seq"); strings.Contains(again.stdout, "Compared pair") || strings.Contains(again.stdout, "started") {
		t.Errorf("a resume of an ended experiment ran or compared again:\n%s", again.stdout)
	}
}

// pairPrompts counts the pair judge's calls so far.
func pairPrompts(t *testing.T, ctrl string) int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(ctrl, "pair-prompt-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(paths)
}

// storedRunCount counts an experiment's stored runs from another goroutine than the test's (0 on any error).
func storedRunCount(f runFixture, name string) int {
	ctx := context.Background()
	db, err := store.OpenReadOnly(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		return 0
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		return 0
	}
	e, err := db.ExperimentByName(ctx, projects[0].ID, name)
	if err != nil {
		return 0
	}
	runs, _ := db.ExperimentRuns(ctx, e.ID)
	return len(runs)
}

// A comparison's spend, stored while the runs go on, counts in the budget: here it keeps the third pair from starting
// ($1.20 of runs, $2 compared, $2 held for the second comparison and $8.60 for a pair pass the $13 budget; without the
// first comparison's $2 they would fit).
func TestExperimentComparisonSpendKeepsALaterPairBack(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	control(t, ctrl, map[string]string{"pair-cost": "1.00"}) // $2 a comparison, its cap
	expect(t, f.run(ctx, "experiment", "new", "tight", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--judge-pairs",
		"--seed", "5", "--budget", "13"), ExitOK)
	out := f.run(ctx, "experiment", "run", "tight")
	expect(t, out, ExitOK, "Compared pair value, repeat 1 (pair judge, unvalidated): tie, $2.00", "Compared pair value, repeat 2 (pair judge, unvalidated): tie, $2.00",
		"Experiment tight: budget: the next run would not fit the $13.00 budget", "4 of 6 runs settled; spent $5.20 of $13.00",
		"Stopped at the budget.")
	if runs := experimentRuns(t, f, "tight"); len(runs) != 4 {
		t.Errorf("%d runs, want 4: the third pair does not fit beside the comparisons\n%s", len(runs), out.stdout)
	}
}

// A pair judge at a usage limit pauses the runs after it at once (Plan.Paused), not only when a later run's result says
// so; the resume compares the pair again, then runs and compares the rest.
func TestExperimentPairJudgeLimitPausesTheRuns(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	control(t, ctrl, map[string]string{"pair-limit": ""})
	expect(t, f.run(ctx, "experiment", "new", "limit", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--concurrency", "1",
		"--judge-pairs", "--seed", "5"), ExitOK)
	paused := f.run(ctx, "experiment", "run", "limit")
	expect(t, paused, ExitOK, "Compared pair value, repeat 1 (pair judge, unvalidated): no answer; stopped at a usage limit or sign-in failure",
		"Experiment limit: paused at the usage limit: the judge hit a usage limit", "Paused: the judge hit a usage limit or a sign-in failure.")
	l := seqLockOf(t, f, "limit")
	runs := experimentRuns(t, f, "limit")
	for _, r := range runs {
		if l.Schedule[r.Slot].Repeat == 3 {
			t.Errorf("a run of the third pair started after the pair judge's limit:\n%s", paused.stdout)
		}
	}
	if len(runs) > 4 {
		t.Errorf("%d runs after the pair judge's limit", len(runs))
	}
	os.Remove(filepath.Join(ctrl, "pair-limit"))
	resumed := f.run(ctx, "experiment", "run", "limit")
	expect(t, resumed, ExitOK, "Compared pair value, repeat 1 (pair judge, unvalidated): tie", "Compared pair value, repeat 3 (pair judge, unvalidated): tie",
		"Experiment limit: done", "6 of 6 runs settled")
	if n := strings.Count(resumed.stdout, "Compared pair"); n != 3 {
		t.Errorf("%d comparisons on resume, want each of the 3 pairs once:\n%s", n, resumed.stdout)
	}
}

// A pause at the usage limit drops the queued comparisons (the running one finishes): none spends more of the window
// until the resume, which compares each exactly once.
func TestExperimentUsagePauseDropsQueuedComparisons(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	resets := time.Now().Add(time.Hour).Truncate(time.Second)
	// Two pairs fit before 85% (58% → 82%, 6% a run); the first comparison waits for pair-block meanwhile.
	control(t, ctrl, map[string]string{"usage": fmt.Sprintf("0.58 0.06 %d\n", resets.Unix()), "pair-block": ""})
	expect(t, f.run(ctx, "experiment", "new", "window", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--concurrency", "1",
		"--judge-pairs", "--seed", "5"), ExitOK)
	go func() { // once the second pair is stored and the execution has paused, let the running comparison go
		if pollUntil(t, "4 stored runs", func() bool { return storedRunCount(f, "window") >= 4 }) {
			time.Sleep(3 * time.Second)
		}
		os.Remove(filepath.Join(ctrl, "pair-block"))
	}()
	paused := f.run(ctx, "experiment", "run", "window")
	expect(t, paused, ExitOK, "Compared pair value, repeat 1 (pair judge, unvalidated): tie", "Paused before the usage limit",
		"1 pair(s) of passing runs still need the pair judge")
	if strings.Contains(paused.stdout, "repeat 2 (pair judge") || pairPrompts(t, ctrl) != 2 {
		t.Errorf("%d pair calls, want the running comparison's 2 only:\n%s", pairPrompts(t, ctrl), paused.stdout)
	}
	os.Remove(filepath.Join(ctrl, "usage"))
	resumed := f.run(ctx, "experiment", "run", "window", "--usage-limit", "100")
	expect(t, resumed, ExitOK, "Compared pair value, repeat 2 (pair judge, unvalidated): tie", "Compared pair value, repeat 3 (pair judge, unvalidated): tie",
		"Experiment window: done")
	if n := strings.Count(resumed.stdout, "Compared pair"); n != 2 || pairPrompts(t, ctrl) != 6 {
		t.Errorf("%d comparisons on resume and %d pair calls in all, want 2 and 3 pairs × 2:\n%s", n, pairPrompts(t, ctrl), resumed.stdout)
	}
}

// While --wait waits for the usage window to reset, no queued comparison starts (the running one finishes); the next
// run in the new window lets them go.
func TestExperimentUsageWaitHoldsComparisons(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	resets := time.Now().Add(time.Hour).Truncate(time.Second)
	control(t, ctrl, map[string]string{"usage": fmt.Sprintf("0.58 0.06 %d\n", resets.Unix()), "pair-block": ""})
	expect(t, f.run(ctx, "experiment", "new", "wait", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--concurrency", "1",
		"--judge-pairs", "--seed", "5"), ExitOK)
	during := -1
	*f.sleep = func(ctx context.Context, d time.Duration) error {
		os.Remove(filepath.Join(ctrl, "pair-block")) // the running comparison finishes its two calls
		pollUntil(t, "the running comparison's two calls", func() bool { return pairPrompts(t, ctrl) >= 2 })
		time.Sleep(time.Second) // time enough for a queued comparison to start, were it let
		during = pairPrompts(t, ctrl)
		control(t, ctrl, map[string]string{"usage": fmt.Sprintf("0 0.06 %d\n", resets.Add(5*time.Hour).Unix())})
		return nil
	}
	out := f.run(ctx, "experiment", "run", "wait", "--wait")
	expect(t, out, ExitOK, "waiting for it to reset", "Every run is done")
	if during != 2 {
		t.Errorf("%d pair calls by the end of the wait, want the running comparison's 2", during)
	}
	if n := strings.Count(out.stdout, "Compared pair"); n != 3 || pairPrompts(t, ctrl) != 6 {
		t.Errorf("%d comparisons and %d calls, want each of the 3 pairs once:\n%s", n, pairPrompts(t, ctrl), out.stdout)
	}
}

// pollUntil checks cond every 20 ms until it holds, and reports whether it did. After two minutes it fails the test
// instead of letting it hang until go test's own timeout; it is safe in the helper goroutines (it never calls FailNow).
func pollUntil(t *testing.T, what string, cond func() bool) bool {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Errorf("timed out waiting for %s", what)
			return false
		}
	}
	return true
}
