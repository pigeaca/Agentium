package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
)

// seqFixture is a project with 16 tasks (startRepo's f1 to f16, mined), each reviewed and valid in its own context and
// in snapshot "lean" (whose CLAUDE.md says "Keep it short"), both contexts calibrated, and experimentAgent as Claude
// Code: enough for every look of a seq-v1 experiment. Built once per test run, and copied.
func seqFixture(t *testing.T) (runFixture, string) {
	t.Helper()
	dirs := templates["seq"].build(t, "seq", []string{"repo", "data", "home"}, func(d []string) {
		if err := copyTree(startRepo(t, 16), d[0]); err != nil {
			t.Fatal(err)
		}
		f := runFixtureAt(d[0], d[1], d[2])
		ctx := context.Background()
		f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")
		expect(t, f.run(ctx, "init"), ExitOK)
		writeFile(t, f.repo, "CLAUDE.md", "# Rules\nKeep it short.\n")
		expect(t, f.run(ctx, "context", "snapshot", "lean", "--working-tree"), ExitOK)
		gitIn(t, f.repo, "checkout", "--", "CLAUDE.md")
		expect(t, f.run(ctx, "task", "mine", "--limit", "16", "--jobs", "4"), ExitOK, "16 of 16 imported task(s) are valid")
		db, id := openFixtureDB(t, f)
		tasks, err := db.Tasks(ctx, id)
		if err != nil || len(tasks) != 16 {
			t.Fatalf("%d tasks, %v", len(tasks), err)
		}
		for _, task := range tasks {
			expect(t, f.run(ctx, "task", "edit", task.Name, "--reviewed"), ExitOK)
		}
		expect(t, f.run(ctx, "task", "validate", "--all", "--snapshot", "lean", "--jobs", "4"), ExitOK)
		expect(t, f.run(ctx, "run", "calibrate"), ExitOK)
		expect(t, f.run(ctx, "run", "calibrate", "--snapshot", "lean"), ExitOK)
	})
	f := runFixtureAt(t.TempDir(), filepath.Join(t.TempDir(), "data"), t.TempDir())
	cloneDirs(t, dirs, []string{f.repo, f.data, f.home})
	ctrl := t.TempDir()
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, ctrl)
	return f, ctrl
}

// control writes the fake agent's control files.
func control(t *testing.T, ctrl string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(ctrl, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// seqLockOf reads an experiment's lock.
func seqLockOf(t *testing.T, f runFixture, name string) experiment.Lock {
	t.Helper()
	var l experiment.Lock
	if err := json.Unmarshal(storedLock(t, f, name), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// checkBarrier fails when a run of a stage started before every run of the stages before it had finished.
func checkBarrier(t *testing.T, l experiment.Lock, runs []store.Run) {
	t.Helper()
	for k := 1; k < len(l.Sequential.Looks); k++ {
		end := l.Sequential.StageEnd(k)
		var lastBefore, firstAfter time.Time
		for _, r := range runs {
			switch {
			case r.Slot < end && r.Finished.After(lastBefore):
				lastBefore = r.Finished
			case r.Slot >= end && (firstAfter.IsZero() || r.Started.Before(firstAfter)):
				firstAfter = r.Started
			}
		}
		if !firstAfter.IsZero() && firstAfter.Before(lastBefore) {
			t.Errorf("a run of stage %d started at %s, before stage %d's last run finished at %s", k+1, firstAfter.Format(time.StampMilli), k, lastBefore.Format(time.StampMilli))
		}
	}
}

// A clear cut ends a seq-v1 experiment at its first look: the preview states the maximum spend and the expected spend,
// the lock records the sequential design, look 1 counts exactly stage 1's 8 tasks, no run of stage 2 starts, and the
// report says where it stopped, shows the look's interval and warns that an early stop overstates the effect.
func TestSeqExperimentStopsAtItsFirstLook(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	ctx := context.Background()
	control(t, ctrl, map[string]string{"cost-lean": "0.15", "cost-jitter": ""})
	expect(t, f.run(ctx, "experiment", "new", "lean-seq", "--b", "lean", "--seed", "5"), ExitOK,
		"Created experiment lean-seq: context A/B, A = base, B = lean, 16 task(s) × 1 run(s) per arm = 32 runs",
		"Method seq-v1: looks after 8, 12 and 16 tasks; it stops at the first look with a cost verdict, or for futility.")
	plan := f.run(ctx, "experiment", "plan", "lean-seq")
	expect(t, plan, ExitOK, "Looks (method seq-v1; runs count both arms):", "1 of 3", "2 of 3", "3 of 3", "99.84%", "98.84%", "96.88%",
		"Spend: at most $", "if every look runs (all 16 tasks; $105.60 if every run reaches its cap, overshoot included); expected\nabout $",
		"if nothing changed (10.3 tasks on average)", "at a 20% cut (13.2 tasks)", "The budget is sized for the\nmaximum")
	t.Log("experiment plan, a seq-v1 design:\n" + plan.stdout)

	run := f.run(ctx, "experiment", "run", "lean-seq")
	expect(t, run, ExitOK, "Locked: Claude Code 2.1.281", "32 runs in a seeded order", "[16/32]",
		"Look 1 of 3 (8 of 8 tasks counted): cost improved at 99.84%: stop",
		"Experiment lean-seq: done: stopped at look 1 of 3 (8 tasks): cost improved", "16 of 32 runs settled",
		"Done: stopped at look 1 of 3 (8 tasks): cost improved. The report: agentium experiment report lean-seq",
		"Looks (method seq-v1): stopped at look 1 of 3 (8 tasks): cost improved")
	t.Log("experiment run, stopped at look 1:\n" + run.stdout)
	if strings.Contains(run.stdout, "[17/32]") {
		t.Errorf("a run of stage 2 started after the stop:\n%s", run.stdout)
	}
	l := seqLockOf(t, f, "lean-seq")
	if l.Method != experiment.MethodSeq || l.Sequential == nil || !slices.Equal(l.Sequential.Looks, []int{8, 12, 16}) || l.Sequential.Futility != 0.10 {
		t.Errorf("the lock: method %s, sequential %+v", l.Method, l.Sequential)
	}
	runs := experimentRuns(t, f, "lean-seq")
	if len(runs) != 16 || slices.ContainsFunc(runs, func(r store.Run) bool { return r.Slot >= 16 }) {
		t.Errorf("%d runs; want stage 1's 16 only", len(runs))
	}

	report := f.run(ctx, "experiment", "report", "lean-seq")
	expect(t, report, ExitOK, "Method seq-v1: stopped at look 1 of 3 (8 tasks): cost improved.", "**Cost -50%** (99.84%: ", "): improved",
		"16 of 32 runs settled (done)", "## Looks", "| 1 of 3 | 8 of 8 | -50% |", "| 99.84% | improved | - | stop |",
		"An early stop overstates the effect's size on average", "The results are look 1's", "| Metric | Role | A | B | B vs A | Bootstrap | t | Verdict |",
		"Intervals are at 95%, cost's at 99.84%, its look's level.", "First decisive verdict: improved on cost in lean-seq")
	*f.terminal = true
	f.vars["NO_COLOR"] = "1"
	t.Log("experiment report, stopped early (terminal, NO_COLOR):\n" + f.run(ctx, "experiment", "report", "lean-seq").stdout)
	*f.terminal = false
	delete(f.vars, "NO_COLOR")

	// Done is done: a resume runs nothing, and the looks are the same.
	again := f.run(ctx, "experiment", "run", "lean-seq")
	expect(t, again, ExitOK, "Resuming experiment lean-seq", "Look 1 of 3 (8 of 8 tasks counted): cost improved at 99.84%: stop",
		"Done: stopped at look 1 of 3")
	if strings.Contains(again.stdout, "started") || len(experimentRuns(t, f, "lean-seq")) != 16 {
		t.Errorf("a finished experiment ran again:\n%s", again.stdout)
	}
	var js struct {
		Analysis experiment.Analysis `json:"analysis"`
	}
	out := filepath.Join(t.TempDir(), "report.json")
	expect(t, f.run(ctx, "experiment", "report", "lean-seq", "--json", "--out", out), ExitOK)
	if data, err := os.ReadFile(out); err != nil || json.Unmarshal(data, &js) != nil || js.Analysis.Sequential == nil || js.Analysis.Sequential.Looks[0].Counted != 8 {
		t.Errorf("the JSON report's looks: %v, %+v", err, js.Analysis.Sequential)
	}
}

// A runner killed outright in the middle of stage 2 loses nothing and repeats no look: the resume recovers the run in
// progress, finishes stage 2 before look 2, and look 1 is the one the first execution made (the same line, the same
// tasks). With no effect and futility off, every stage runs, each after the look before it.
func TestSeqExperimentSurvivesAKillMidStage(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	ctx := context.Background()
	control(t, ctrl, map[string]string{"cost-jitter": "", "hang": "s18-t1\n"})
	expect(t, f.run(ctx, "experiment", "new", "flat", "--b", "lean", "--no-futility", "--seed", "3"), ExitOK,
		"Method seq-v1: looks after 8, 12 and 16 tasks; it stops at the first look with a cost verdict (futility stops off).")
	var environ []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ANTHROPIC_") && !strings.HasPrefix(kv, "AGENTIUM_") && !strings.HasPrefix(kv, "CLAUDE_") && !strings.HasPrefix(kv, "HOME=") {
			environ = append(environ, kv)
		}
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestExperimentHelperProcess$", "-test.count=1")
	helper.Dir = f.repo
	helper.Env = append(environ, "AGENTIUM_TEST_HELPER=1", "AGENTIUM_TEST_ARGS=experiment run flat", "AGENTIUM_HOME="+f.data,
		"HOME="+f.home, "AGENTIUM_CLAUDE="+f.vars["AGENTIUM_CLAUDE"])
	var first bytes.Buffer
	helper.Stdout, helper.Stderr = &first, &first
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pids, _ := filepath.Glob(filepath.Join(ctrl, "pid-*"))
		for _, p := range pids {
			if data, err := os.ReadFile(p); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
					syscall.Kill(-pid, syscall.SIGKILL)
				}
			}
		}
	})
	waitFor(t, "slot 18's agent", func() bool {
		_, err := os.Stat(filepath.Join(ctrl, "hanging-e1-s18-t1"))
		return err == nil
	})
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	helper.Wait()
	data, err := os.ReadFile(filepath.Join(ctrl, "pid-e1-s18-t1"))
	if err != nil {
		t.Fatal(err)
	}
	pgid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	syscall.Kill(-pgid, syscall.SIGKILL)
	waitFor(t, "the agent's process group to end", func() bool { return syscall.Kill(-pgid, 0) != nil })
	os.Remove(filepath.Join(ctrl, "hang"))

	look1 := "Look 1 of 3 (8 of 8 tasks counted): cost inconclusive at 99.84%: continue"
	if !strings.Contains(first.String(), look1) || strings.Contains(first.String(), "Look 2 of 3") {
		t.Fatalf("before the kill: look 1 continued, and no look 2:\n%s", first.String())
	}
	before := experimentRuns(t, f, "flat")
	if slices.ContainsFunc(before, func(r store.Run) bool { return r.Slot >= 24 }) {
		t.Errorf("a run of stage 3 was stored before look 2")
	}

	// The other run in flight at the kill may still be grading: a resume refuses until its process group is gone.
	var resumed cliResult
	recovered := ""
	waitFor(t, "the runs in flight at the kill to end", func() bool {
		resumed = f.run(ctx, "experiment", "run", "flat")
		recovered += resumed.stdout
		return !strings.Contains(resumed.stderr, "may still be running")
	})
	if !strings.Contains(recovered, "Recovered run ") {
		t.Errorf("the run in progress at the kill was not recovered:\n%s", recovered)
	}
	expect(t, resumed, ExitOK, "Resuming experiment flat", look1,
		"Look 2 of 3 (12 of 12 tasks counted): cost inconclusive at 98.84%: continue",
		"Look 3 of 3 (16 of 16 tasks counted): cost ", ": final", "Experiment flat: done", "32 of 32 runs settled", "Every run is done.")
	for _, line := range []string{look1, "Look 2 of 3", "Look 3 of 3"} {
		if n := strings.Count(resumed.stdout, line); n != 1 {
			t.Errorf("%q appears %d times in the resume:\n%s", line, n, resumed.stdout)
		}
	}
	// Look 2 comes after stage 2's last run, before stage 3's first.
	if i, j, k := strings.Index(resumed.stdout, "Look 2 of 3"), strings.Index(resumed.stdout, "[24/32]"), strings.Index(resumed.stdout, "[25/32]"); i < j || k < i {
		t.Errorf("look 2 is not between stages 2 and 3:\n%s", resumed.stdout)
	}
	l := seqLockOf(t, f, "flat")
	runs := experimentRuns(t, f, "flat")
	checkBarrier(t, l, runs)
	// The runs in flight at the kill (slot 18, and perhaps its pair's other run) were recovered as cancelled.
	settled, cancelled := 0, 0
	for _, r := range runs {
		switch {
		case r.Outcome == "cancelled" && r.Slot >= 16 && r.Slot < 24:
			cancelled++
		case r.Outcome == "ok":
			settled++
		}
	}
	if settled != 32 || cancelled < 1 || cancelled+settled != len(runs) || !slices.ContainsFunc(runs, func(r store.Run) bool { return r.Slot == 18 && r.Outcome == "cancelled" }) {
		t.Errorf("%d runs, %d settled, %d recovered in stage 2: want 32 settled and slot 18's recovered", len(runs), settled, cancelled)
	}
	var js struct {
		Analysis experiment.Analysis `json:"analysis"`
	}
	out := filepath.Join(t.TempDir(), "report.json")
	expect(t, f.run(ctx, "experiment", "report", "flat", "--json", "--out", out), ExitOK)
	raw, _ := os.ReadFile(out)
	if err := json.Unmarshal(raw, &js); err != nil || js.Analysis.Sequential == nil {
		t.Fatalf("the JSON report: %v", err)
	}
	var counted []int
	for _, look := range js.Analysis.Sequential.Looks {
		counted = append(counted, look.Counted)
	}
	if !slices.Equal(counted, []int{8, 12, 16}) || js.Analysis.Sequential.Ended != experiment.LookFinal {
		t.Errorf("looks counted %v, ended %q: want one look per stage, each on its stages' tasks", counted, js.Analysis.Sequential.Ended)
	}
}

// A budget stop between looks keeps the last look's verdict: the report's results are look 1's, the stage-2 runs that
// ran are left out of them, and a resume with a higher budget finishes stage 2 before look 2. The budget's caps and
// reserve are the design's on every stage: the stop comes where the whole-experiment rule puts it.
func TestSeqExperimentBudgetStopKeepsTheLastLook(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	ctx := context.Background()
	control(t, ctrl, map[string]string{"cost-jitter": ""})
	// 16 runs of stage 1 at about $0.33 spend about $5.3; with $1 caps and 2 at a time, a pair starts only while the
	// spend, the caps in flight and its own two caps fit $8, so two pairs of stage 2 run, and the third does not.
	expect(t, f.run(ctx, "experiment", "new", "short", "--b", "lean", "--no-futility", "--run-budget", "1", "--budget", "8", "--seed", "3"), ExitOK)
	stopped := f.run(ctx, "experiment", "run", "short")
	expect(t, stopped, ExitOK, "Look 1 of 3 (8 of 8 tasks counted): cost inconclusive at 99.84%: continue",
		"Experiment short: budget: the next run would not fit the $8.00 budget", "Looks (method seq-v1): look 1 of 3 made (continue); look 2 comes once the first 12 tasks are settled")
	runs := experimentRuns(t, f, "short")
	if n := len(runs); n <= 16 || n >= 24 {
		t.Fatalf("%d runs: want stage 1 and part of stage 2", n)
	}
	report := f.run(ctx, "experiment", "report", "short")
	expect(t, report, ExitOK, "Method seq-v1: look 1 of 3 made (continue)", "**Cost", "(99.84%: ", "): inconclusive",
		"The experiment is not finished (budget: ", "and the results are look 1's.", "The results are look 1's",
		"run(s) of stages after look 1 are not in its results")
	if strings.Contains(report.stdout, "early stop overstates") {
		t.Errorf("a budget stop is not an early stop:\n%s", report.stdout)
	}

	resumed := f.run(ctx, "experiment", "run", "short", "--budget", "30")
	expect(t, resumed, ExitOK, "Budget raised to $30.00", "Look 2 of 3 (12 of 12 tasks counted)", "Look 3 of 3 (16 of 16 tasks counted)",
		"Experiment short: done", "32 of 32 runs settled")
	checkBarrier(t, seqLockOf(t, f, "short"), experimentRuns(t, f, "short"))
}

// A usage pause between looks keeps the last look's verdict too: the pause comes in stage 2, and the report's results
// stay look 1's until stage 2 is settled.
func TestSeqExperimentUsagePauseKeepsTheLastLook(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	ctx := context.Background()
	resets := time.Now().Add(time.Hour).Truncate(time.Second)
	// Each run moves the five-hour window 4.5% further (one at a time, so none overlap): stage 1's 16 runs reach 72%, and
	// the 85% limit stops stage 2 after a pair.
	control(t, ctrl, map[string]string{"cost-jitter": "", "usage": "0.00 0.045 " + strconv.FormatInt(resets.Unix(), 10) + "\n"})
	expect(t, f.run(ctx, "experiment", "new", "window", "--b", "lean", "--no-futility", "--concurrency", "1", "--seed", "3"), ExitOK)
	paused := f.run(ctx, "experiment", "run", "window")
	expect(t, paused, ExitOK, "Look 1 of 3 (8 of 8 tasks counted): cost inconclusive at 99.84%: continue", "Paused before the usage limit",
		"Experiment window: paused at the usage limit")
	runs := experimentRuns(t, f, "window")
	if len(runs) < 16 || len(runs) >= 24 {
		t.Fatalf("%d runs: want stage 1's 16 and at most part of stage 2", len(runs))
	}
	expect(t, f.run(ctx, "experiment", "report", "window"), ExitOK, "Method seq-v1: look 1 of 3 made (continue)", "): inconclusive",
		"The experiment is not finished (usage: ", "The results are look 1's")
	os.Remove(filepath.Join(ctrl, "usage"))
	expect(t, f.run(ctx, "experiment", "run", "window", "--usage-limit", "100"), ExitOK, "Look 2 of 3 (12 of 12 tasks counted)", "Experiment window: done")
	checkBarrier(t, seqLockOf(t, f, "window"), experimentRuns(t, f, "window"))
}

// A cost experiment stored before seq-v1 (its design names no method) still locks, runs, resumes and reports under
// phase1-v2, as it always did: three runs per task and arm, one fixed analysis at 95%, no looks.
func TestLegacyCostExperimentStaysPhase1(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "legacy", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--run-budget", "1", "--budget", "3"), ExitOK)
	db, err := sql.Open("sqlite3", filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var design map[string]any
	var raw string
	if err := db.QueryRow(`SELECT design FROM experiments WHERE name = 'legacy'`).Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &design) != nil {
		t.Fatalf("design %q: %v", raw, err)
	}
	design["goal"] = experiment.GoalCheaper // as experiment new made cost experiments before seq-v1
	delete(design, "method")
	legacy, _ := json.Marshal(design)
	if _, err := db.Exec(`UPDATE experiments SET design = ? WHERE name = 'legacy'`, string(legacy)); err != nil {
		t.Fatal(err)
	}

	expect(t, f.run(ctx, "experiment", "plan", "legacy"), ExitOK, "Sizes (runs count both arms):", "Quick", "Floors (method phase1-v2)",
		"note: at this size the no-loss guard certifies only about")
	expect(t, f.run(ctx, "experiment", "run", "legacy"), ExitOK, "Locked: ", "6 runs in a seeded order", "Experiment legacy: budget:", "4 of 6 runs settled")
	if l := seqLockOf(t, f, "legacy"); l.Method != experiment.MethodV2 || l.Sequential != nil || l.Schedule[0].Stage != 0 {
		t.Errorf("a legacy cost design locked as %s, sequential %+v", l.Method, l.Sequential)
	}
	resumed := f.run(ctx, "experiment", "run", "legacy", "--budget", "10")
	expect(t, resumed, ExitOK, "Resuming experiment legacy", "Experiment legacy: done", "6 of 6 runs settled", "Every run is done.")
	if strings.Contains(resumed.stdout, "Look ") {
		t.Errorf("a phase1-v2 experiment made looks:\n%s", resumed.stdout)
	}
	report := f.run(ctx, "experiment", "report", "legacy")
	expect(t, report, ExitOK, "**Cost**: no result (fewer than two tasks", "(method phase1-v2)", "| 95% bootstrap | 95% t |",
		"the intervals in the summary are the wider of the two, at 95%")
	for _, seq := range []string{"Method seq-v1", "## Looks", "look's level"} {
		if strings.Contains(report.stdout, seq) {
			t.Errorf("a phase1-v2 report says %q:\n%s", seq, report.stdout)
		}
	}
}

// Lost tasks through the executor: a slot of stage 1 that fails three times for infrastructure leaves look 1 with 7
// tasks, below the floor, so it gives no verdict and stage 2 runs; look 2 counts 11 tasks, at the level the spending
// function gives 11 of the planned 16, and its verdict stops the experiment.
func TestSeqExperimentLostTasks(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	ctx := context.Background()
	control(t, ctrl, map[string]string{"cost-lean": "0.15", "cost-jitter": "", "infra": "s0-t1\ns0-t2\ns0-t3\n"})
	expect(t, f.run(ctx, "experiment", "new", "lossy", "--b", "lean", "--seed", "5"), ExitOK)
	looks, err := stats.SequentialLooks([]int{11}, 16, false, stats.SeqAlpha, stats.SeqEquivalenceAlpha)
	if err != nil {
		t.Fatal(err)
	}
	got := f.run(ctx, "experiment", "run", "lossy")
	expect(t, got, ExitOK, "(attempt 3 of 3): started",
		"Look 1 of 3 (7 of 8 tasks counted): no verdict (7 task(s) have cost in both arms, below the floor of 8): continue",
		"Look 2 of 3 (11 of 12 tasks counted): cost improved at "+strconv.FormatFloat(100*looks[0].EffLevel, 'f', 2, 64)+"%: stop",
		"Experiment lossy: done: stopped at look 2 of 3 (11 tasks): cost improved")
	checkBarrier(t, seqLockOf(t, f, "lossy"), experimentRuns(t, f, "lossy"))
}

// A model A/B is a cost experiment too: seq-v1, stored as design version 3, each arm with its own run cap through the
// stages. A clear cut stops it at look 1; a tight budget stops it at the per-arm caps' rule, never past the budget.
func TestSeqModelAB(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	ctx := context.Background()
	control(t, ctrl, map[string]string{"cost-claude-opus-5-5": "0.60", "cost-jitter": ""})
	expect(t, f.run(ctx, "experiment", "new", "models", "--a", "claude-opus-5-5", "--b", "claude-sonnet-5", "--seed", "5"), ExitOK,
		"16 task(s) × 1 run(s) per arm = 32 runs", "Method seq-v1: looks after 8, 12 and 16 tasks")
	// Each arm's own run cap, as --run-budget-a and --run-budget-b set them before they were removed.
	perArmCaps := func(d *experiment.Design) { d.Arms[0].RunBudgetUSD, d.Arms[1].RunBudgetUSD = 2, 1 }
	storeAsBefore(t, f, "models", perArmCaps)
	if d := loadDesign(t, f, "models"); d.Version != experiment.DesignVersionSeq || d.Method != experiment.MethodSeq || d.Template != experiment.TemplateModelAB {
		t.Errorf("the stored design: version %d, method %s, template %s", d.Version, d.Method, d.Template)
	}
	expect(t, f.run(ctx, "experiment", "plan", "models"), ExitOK, "Looks (method seq-v1; runs count both arms):", "$27.60", "$55.20",
		"if every run reaches its cap")
	got := f.run(ctx, "experiment", "run", "models")
	expect(t, got, ExitOK, "each run up to $2.00 (arm A) or $1.00 (arm B)", "Look 1 of 3 (8 of 8 tasks counted): cost improved at 99.84%: stop",
		"Done: stopped at look 1 of 3 (8 tasks): cost improved")
	if runs := experimentRuns(t, f, "models"); len(runs) != 16 {
		t.Errorf("%d runs; want stage 1's 16", len(runs))
	}
	expect(t, f.run(ctx, "experiment", "report", "models"), ExitOK, "B (claude-sonnet-5) costs", "Method seq-v1: stopped at look 1 of 3")

	// $2 and $1 caps ($2.30 and $1.15 with their overshoot: Opus 5.5's floor is $0.30), 2 at a time: a pair starts only
	// while the spend, the caps in flight and its own $3.45 fit $6.
	expect(t, f.run(ctx, "experiment", "new", "tight", "--a", "claude-opus-5-5", "--b", "claude-sonnet-5", "--budget", "7", "--seed", "5"), ExitOK)
	storeAsBefore(t, f, "tight", func(d *experiment.Design) { perArmCaps(d); d.BudgetUSD = 6 })
	tight := f.run(ctx, "experiment", "run", "tight")
	expect(t, tight, ExitOK, "Experiment tight: budget: the next run would not fit the $6.00 budget", "$2.30 (arm A) or $1.15 (arm B) per run at most")
	spent := 0.0
	runs := experimentRuns(t, f, "tight")
	for _, r := range runs {
		spent += r.CostUSD
	}
	if spent > 6 || len(runs) >= 16 || strings.Contains(tight.stdout, "Look 1") {
		t.Errorf("$%.2f spent on %d runs; want under $6 and stage 1 unfinished, so no look", spent, len(runs))
	}
}
