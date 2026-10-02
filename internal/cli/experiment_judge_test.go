package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/experiment"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// judgeFiles lists the fake judge's kept files of one kind ("prompt" or "dir") and their contents.
func judgeFiles(t *testing.T, ctrl, kind string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(ctrl, "judge-"+kind+"-*"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(data))
	}
	return out
}

func records(t *testing.T, runs []store.Run) []run.Record {
	t.Helper()
	var out []run.Record
	for _, r := range runs {
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func TestExperimentNewJudgeFlags(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--judge-model", "claude-opus-5-5"}, "set the judge: add --judge"},
		{[]string{"--judge-repeats", "2"}, "set the judge: add --judge"},
		{[]string{"--judge", "--judge-repeats", "-1"}, "--judge-repeats must be positive"},
		{[]string{"--judge", "--judge-repeats", "10"}, "the judge's repeats must be 1 to 9"},
	} {
		expect(t, f.run(ctx, append([]string{"experiment", "new", "x", "--b", "lean", "--task", "value", "--budget", "50"}, c.args...)...), ExitUsage, c.want)
	}
	// Defaults: the pilot's model, effort and 3 calls; a budget below a pair with their judgements is refused.
	expect(t, f.run(ctx, "experiment", "new", "x", "--b", "lean", "--task", "value", "--judge", "--budget", "8"), ExitUsage,
		"below one pair of runs at their caps ($12.60)")
	expect(t, f.run(ctx, "experiment", "new", "x", "--b", "lean", "--task", "value", "--judge", "--budget", "50"), ExitOK,
		"The judge: claude-opus-5-5 at effort high, 3 call(s) per run, its verdicts a second opinion beside the tests.")
	d := storedDesign(t, f, "x")
	if d.Judge == nil || *d.Judge != (llmjudge.Settings{Model: "claude-opus-5-5", Effort: "high", Repeats: 3}) {
		t.Errorf("stored judge settings %+v", d.Judge)
	}
	expect(t, f.run(ctx, "experiment", "new", "plain", "--b", "lean", "--task", "value", "--budget", "50"), ExitOK)
	if d := storedDesign(t, f, "plain"); d.Judge != nil {
		t.Errorf("an experiment without --judge has judge settings %+v", d.Judge)
	}
	expect(t, f.run(ctx, "experiment", "--help"), ExitOK, "[--judge [--judge-model MODEL] [--judge-effort LEVEL] [--judge-repeats N]]",
		"--judge asks an LLM judge about every graded run", "it decides nothing")
}

// storedDesign reads an experiment's stored design, as the database keeps it.
func storedDesign(t *testing.T, f runFixture, name string) experiment.Design {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	e, err := db.ExperimentByName(ctx, projects[0].ID, name)
	if err != nil {
		t.Fatal(err)
	}
	var d experiment.Design
	if err := json.Unmarshal(e.Design, &d); err != nil {
		t.Fatal(err)
	}
	if e.Lock != nil {
		var l experiment.Lock
		if err := json.Unmarshal(e.Lock, &l); err != nil || (l.Design.Judge == nil) != (d.Judge == nil) {
			t.Errorf("the lock's judge %+v, the design's %+v (%v)", l.Design.Judge, d.Judge, err)
		}
	}
	return d
}

// With --judge, the preview states the judge's estimate apart from the agent's, every graded run gets a verdict, the
// budget counts the judge's spend, and the agent's cost (the run's cost column and its record's metrics) stays its own.
func TestExperimentJudgesEveryGradedRun(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "judged", "--b", "lean", "--task", "value", "--repeats", "1", "--judge", "--judge-repeats", "2",
		"--seed", "5"), ExitOK, "1 task(s) × 1 run(s) per arm = 2 runs, budget $21.00", "The judge: claude-opus-5-5 at effort high, 2 call(s) per run")
	plan := f.run(ctx, "experiment", "plan", "judged")
	expect(t, plan, ExitOK, "judge: claude-opus-5-5 at effort high, 2 call(s) per run; each run's judgement up to $2.00",
		"The judge: about $0.26 for this experiment's 2 runs × 2 call(s) at $0.065 a call (EST. COST includes it; the agent's\nruns are $3.22)",
		"the judge pilot's mean call on claude-opus-5-5 at effort high, not a measure of this project",
		"Worst case: every run reaches its $3.00 cap, and its judgement $2.00")
	if !strings.Contains(plan.stdout, "$3.48      $10.60") { // this experiment: 2 × $1.612 + 2 × 2 × $0.065; worst 2 × ($3 + $0.30 + $2)
		t.Errorf("this experiment's row, want $3.48 and the worst case $10.60:\n%s", plan.stdout)
	}

	first := f.run(ctx, "experiment", "run", "judged")
	expect(t, first, ExitOK, "The judge: claude-opus-5-5 at effort high, 2 call(s) per run.", "each run up to $3.00 and its judgement up to $2.00",
		"ok, $0.30; judge: fixed (2 of 2), $0.10", "spent $0.80 of $21.00 (the judge $0.20 of it, not in the arms' costs)", "Experiment judged: done")
	for _, arm := range []string{"A", "B"} {
		if row := armRow(first.stdout, arm); len(row) < 9 || row[8] != "$0.30" {
			t.Errorf("arm %s = %v: its cost is the agent's alone", arm, row)
		}
	}
	runs := experimentRuns(t, f, "judged")
	for i, rec := range records(t, runs) {
		v := rec.Judge
		if runs[i].CostUSD != 0.30 || rec.Metrics.CostUSD != 0.30 || rec.Outcome != "ok" || rec.Passed == nil || !*rec.Passed {
			t.Errorf("run %d: cost column $%v, metrics $%v, %s: the agent's own", i, runs[i].CostUSD, rec.Metrics.CostUSD, rec.Outcome)
		}
		if v == nil || v.Fixed != llmjudge.Yes || len(v.Answers) != 2 || v.Requested != 2 || v.Reason != "Sets the value as the reference does." ||
			v.Model != "claude-opus-5-5" || v.Effort != "high" || v.CostUSD < 0.0999 || v.CostUSD > 0.1001 || v.Stopped != "" {
			t.Errorf("run %d's verdict %+v", i, v)
		}
		if _, err := os.Stat(filepath.Join(rec.RecordsDir, "judge")); err == nil {
			t.Errorf("run %d's judge folder was left behind", i)
		}
	}
	// The judge read both changes' code with pinned headers, never the hidden tests, and worked in the run's records.
	prompts := judgeFiles(t, ctrl, "prompt")
	if len(prompts) != 4 {
		t.Fatalf("%d judge calls, want 2 runs × 2", len(prompts))
	}
	for _, p := range prompts {
		if strings.Count(p, "diff --git a/value.txt b/value.txt") != 2 || strings.Contains(p, "value_test") || !strings.Contains(p, "+new") {
			t.Errorf("the judge's prompt:\n%s", p)
		}
	}
	for _, dir := range judgeFiles(t, ctrl, "dir") {
		if !strings.Contains(dir, filepath.Join("records", "")) || !strings.Contains(dir, filepath.Join("judge", "call-")) {
			t.Errorf("a judge call ran in %s, not in its run's records", dir)
		}
	}
	emptyWorkspaces(t, f)
	expect(t, f.run(ctx, "experiment", "show", "judged"), ExitOK, "Judge: claude-opus-5-5 at effort high, 2 call(s) per run")
	// The report's spend is the budget's, the judge's included; the arms' costs stay the agent's.
	expect(t, f.run(ctx, "experiment", "report", "judged"), ExitOK, "2 of 2 runs settled (done); spent $0.80", "$0.300 → $0.300",
		"## Judge", "| A | passed | 1 | 1 (100%; 21–100%) |", "Judge cost: A $0.10, B $0.10; $0.20 in total", "The judge called every passing run it judged fixed.")
	if d := storedDesign(t, f, "judged"); d.Judge == nil || d.Judge.Repeats != 2 {
		t.Errorf("design judge %+v", d.Judge)
	}

	// A judge that never answers leaves the run as it was: graded, counted, and its verdict final (judged, no answer).
	expect(t, f.run(ctx, "experiment", "new", "broken", "--b", "lean", "--task", "value", "--repeats", "1", "--judge", "--judge-repeats", "2"), ExitOK)
	writeFile(t, ctrl, "judge-broken", "")
	expect(t, f.run(ctx, "experiment", "run", "broken"), ExitOK, "ok, $0.30; judge: no answer: exit 1, not JSON: not json, $0.00", "Experiment broken: done")
	for _, rec := range records(t, experimentRuns(t, f, "broken")) {
		if rec.Outcome != "ok" || rec.Passed == nil || !*rec.Passed || rec.Metrics.CostUSD != 0.30 || rec.Judge == nil || rec.Judge.Fixed != "" ||
			rec.Judge.Stopped != "" || len(rec.Judge.Errors) != 2 {
			t.Errorf("a run whose judge failed: %s, passed %v, verdict %+v", rec.Outcome, rec.Passed, rec.Judge)
		}
	}
	os.Remove(filepath.Join(ctrl, "judge-broken"))
	if again := f.run(ctx, "experiment", "run", "broken"); strings.Contains(again.stdout, "Judged run") {
		t.Errorf("a judgement that ran its course was judged again:\n%s", again.stdout)
	}
}

// A judge at its usage limit pauses the experiment; the resume judges the runs left without a verdict (stopped, or
// never judged), keeps what the stopped judgement spent, then runs the rest.
func TestExperimentJudgePausesAndResumes(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "limit", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2", "--concurrency", "1", "--judge",
		"--judge-repeats", "2", "--budget", "40"), ExitOK)
	writeFile(t, ctrl, "judge-limit", "")
	paused := f.run(ctx, "experiment", "run", "limit")
	expect(t, paused, ExitOK, "[1/4] value", "judge: no answer; stopped at a usage limit or sign-in failure, $0.01",
		"Experiment limit: paused at the usage limit: the judge hit a usage limit or a sign-in failure",
		"Paused: the judge hit a usage limit or a sign-in failure. To continue, once it resets: agentium experiment run limit")
	if strings.Contains(paused.stdout, "[2/4]") {
		t.Errorf("a run started after the judge hit its limit:\n%s", paused.stdout)
	}
	runs := experimentRuns(t, f, "limit")
	recs := records(t, runs)
	if len(runs) != 1 || recs[0].Outcome != "ok" || recs[0].Passed == nil || !*recs[0].Passed || runs[0].CostUSD != 0.30 ||
		recs[0].Judge == nil || recs[0].Judge.Stopped != llmjudge.StoppedLimit || len(judgeFiles(t, ctrl, "prompt")) != 1 {
		t.Fatalf("the paused run: %+v, verdict %+v", runs, recs[0].Judge)
	}
	os.Remove(filepath.Join(ctrl, "judge-limit"))
	resumed := f.run(ctx, "experiment", "run", "limit")
	expect(t, resumed, ExitOK, "Judged run "+runs[0].ID+" (task value, arm ", "fixed (2 of 2), $0.10 (spent $0.41 of $40.00)",
		"Experiment limit: done", "4 of 4 runs settled; spent $1.61 of $40.00 (the judge $0.41 of it")
	recs = records(t, experimentRuns(t, f, "limit"))
	if v := recs[0].Judge; v == nil || v.Fixed != llmjudge.Yes || v.Stopped != "" || v.CostUSD < 0.1099 || v.CostUSD > 0.1101 {
		t.Errorf("the run judged again: %+v (its cost keeps the stopped judgement's $0.01)", v)
	}

	// A graded run stored without a verdict (its Agentium stopped while judging) is judged on resume.
	recs[2].Judge = nil
	encoded, err := json.Marshal(recs[2])
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	stored := experimentRuns(t, f, "limit")
	if err := db.SetRunRecord(ctx, stored[2].ID, encoded); err != nil {
		t.Fatal(err)
	}
	db.Close()
	again := f.run(ctx, "experiment", "run", "limit")
	expect(t, again, ExitOK, "Judged run "+stored[2].ID, "fixed (2 of 2), $0.10")
	if n := strings.Count(again.stdout, "Judged run"); n != 1 {
		t.Errorf("%d runs judged again, want the one without a verdict:\n%s", n, again.stdout)
	}
}

// grading's agent.diff has the same form whatever the user's git settings: a/ and b/ prefixes, and no renames.
func TestAgentDiffFormIsPinned(t *testing.T) {
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	gitHome := t.TempDir()
	writeFile(t, gitHome, ".gitconfig", "[diff]\n\tnoprefix = true\n\tmnemonicPrefix = true\n\trenames = copies\n")
	t.Setenv("HOME", gitHome) // git's own: the user's global settings
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(gitHome, "xdg"))
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt; mv CLAUDE.md RULES.md", false)
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitOK, "outcome      ok; verification passed")
	diffs, err := filepath.Glob(filepath.Join(f.data, "records", "*", "agent.diff"))
	if err != nil || len(diffs) != 1 {
		t.Fatalf("agent.diff: %v, %v", diffs, err)
	}
	data, err := os.ReadFile(diffs[0])
	if err != nil {
		t.Fatal(err)
	}
	diff := string(data)
	for _, want := range []string{"diff --git a/CLAUDE.md b/CLAUDE.md\ndeleted file", "diff --git a/RULES.md b/RULES.md\nnew file",
		"diff --git a/value.txt b/value.txt", "--- a/value.txt\n+++ b/value.txt"} {
		if !strings.Contains(diff, want) {
			t.Errorf("agent.diff lacks %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "rename from") || strings.Contains(diff, "copy from") {
		t.Errorf("agent.diff has renames:\n%s", diff)
	}
}

// The judge hitting its limit on the last run leaves the experiment paused, not done. A resume judges it when the budget
// leaves room for a judgement, and says so when it does not.
func TestExperimentJudgeLimitOnTheLastRun(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	// Runs at $5, each with up to 2 × 2 × $0.50 of judgement: the $11 budget fits one pair ($10).
	writeFile(t, ctrl, "cost", "5")
	writeFile(t, ctrl, "judge-limit-after", "2") // the second run's first call
	expect(t, f.run(ctx, "experiment", "new", "last", "--b", "lean", "--task", "value", "--repeats", "1", "--concurrency", "1", "--judge",
		"--judge-repeats", "2", "--budget", "11"), ExitOK)
	paused := f.run(ctx, "experiment", "run", "last")
	expect(t, paused, ExitOK, "[2/2] value", "judge: no answer; stopped at a usage limit or sign-in failure",
		"Experiment last: paused at the usage limit: the judge hit a usage limit", "2 of 2 runs settled",
		"1 graded run(s) still need the judge: agentium experiment run last", "Paused: the judge hit a usage limit or a sign-in failure.",
		"--wait does not wait for the judge")
	if strings.Contains(paused.stdout, "Every run is done") {
		t.Errorf("done while a run still needs the judge:\n%s", paused.stdout)
	}
	expect(t, f.run(ctx, "experiment", "show", "last"), ExitOK, "1 graded run(s) still need the judge")

	// $10.11 spent: a judgement's $2 does not fit the $11 budget.
	os.Remove(filepath.Join(ctrl, "judge-limit-after"))
	os.Remove(filepath.Join(ctrl, "judge-limit"))
	unfunded := f.run(ctx, "experiment", "run", "last")
	// Every slot settled, and only the judge is waiting for a budget: a budget stop (exit 0), not a failure.
	expect(t, unfunded, ExitOK, "1 run(s) still need the judge, but the budget leaves no room for a judgement ($2.00): raise it with --budget",
		"1 graded run(s) still need the judge: agentium experiment run last --budget USD",
		"Stopped at the budget. To continue: agentium experiment run last --budget USD")
	if strings.Contains(unfunded.stdout, "Judged run") || strings.Contains(unfunded.stdout, "Stopped. To continue") {
		t.Errorf("a judgement past the budget, or a stop:\n%s", unfunded.stdout)
	}
	expect(t, f.run(ctx, "experiment", "show", "last"), ExitOK, "1 run(s) still need the judge, but the budget leaves no room for a judgement")
	// The report says the success and cost verdicts are complete, not that the experiment is unfinished.
	report := f.run(ctx, "experiment", "report", "last")
	expect(t, report, ExitOK, "2 of 2 runs settled (budget)", "## Judge",
		"Every run settled, so the success and cost verdicts are complete; 1 run(s) lack a judge verdict, which the budget leaves no room for: "+
			"agentium experiment run last --budget USD judges them.", "1 stopped early",
		"1 run(s) still need the judge: agentium experiment run last --budget USD judges them.")
	if strings.Contains(report.stdout, "not finished") {
		t.Errorf("the report calls the experiment unfinished:\n%s", report.stdout)
	}
	expect(t, f.run(ctx, "experiment", "run", "last", "--budget", "20"), ExitOK, "Judged run ", "fixed (2 of 2), $0.10 (spent $10.21 of $20.00)",
		"Experiment last: done", "Every run is done.")
}
