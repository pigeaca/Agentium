package report

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// starProject is a registered project, 2 hours before the fixtures' runs, in a fresh database.
func starProject(t *testing.T) (experiment.Project, time.Time) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	registered := fixture().Lock.LockedAt.Add(-2 * time.Hour)
	saved, err := db.SaveProject(ctx, "/work/repo", "repo", []byte("{}"), registered)
	if err != nil {
		t.Fatal(err)
	}
	return experiment.Project{DB: db, ID: saved.ID}, registered
}

// storeExperiment saves an experiment of in as stored data: its lock and every run.
func storeExperiment(t *testing.T, p experiment.Project, in Input) {
	t.Helper()
	storeExperimentAs(t, p, in, store.StatusDone)
}

// storeExperimentAs is storeExperiment with the status the experiment ended in.
func storeExperimentAs(t *testing.T, p experiment.Project, in Input, status string) {
	t.Helper()
	ctx := context.Background()
	design, _ := json.Marshal(in.Lock.Design)
	e, err := p.DB.SaveExperiment(ctx, store.Experiment{ProjectID: p.ID, Name: in.Name, Template: in.Lock.Design.Template, Design: design, CreatedAt: in.Lock.LockedAt})
	if err != nil {
		t.Fatal(err)
	}
	lock, _ := json.Marshal(in.Lock)
	if err := p.DB.LockExperiment(ctx, e.ID, lock); err != nil {
		t.Fatal(err)
	}
	if err := p.DB.SetExperimentStatus(ctx, e.ID, status, ""); err != nil {
		t.Fatal(err)
	}
	for _, r := range in.Runs {
		rec, _ := json.Marshal(r.Record)
		err := p.DB.SaveRun(ctx, store.Run{ID: in.Name + "-" + r.ID, ProjectID: p.ID, TaskName: r.Record.Task, Arm: r.Record.Arm, Outcome: r.Record.Outcome,
			Passed: r.Record.Passed, CostUSD: r.Record.Metrics.CostUSD, Record: rec, Started: r.Record.Started, Finished: r.Record.Finished,
			ExperimentID: e.ID, Slot: r.Slot, Attempt: r.Attempt})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// total is what in's runs spent, agent and judge.
func total(in Input) float64 {
	sum := 0.0
	for _, r := range in.Runs {
		sum += r.Record.Spend().TotalUSD()
	}
	return sum
}

// verdictOf is the verdict of in's metric with the given role in its own analysis.
func verdictOf(t *testing.T, in Input, role string) string {
	t.Helper()
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Analysis.Results {
		if r.Role == role {
			return r.Verdict
		}
	}
	t.Fatalf("no %s result", role)
	return ""
}

// noisyTie is a 10 × 1 experiment whose arms differ by noise only: its verdicts are inconclusive.
func noisyTie(t *testing.T) Input {
	t.Helper()
	in := oneRun(experiment.MethodV2, experiment.TemplateContextAB)
	r := rand.New(rand.NewPCG(9, 9))
	for i := range in.Runs {
		in.Runs[i].Record.Metrics.CostUSD = 0.30 * math.Exp(0.45*r.NormFloat64())
		in.Runs[i].Record.Passed = &[]bool{r.Float64() < 0.5}[0]
	}
	in.Name = "noisy-tie"
	return in
}

func TestNorthStarNoneYet(t *testing.T) {
	t.Parallel()
	p, _ := starProject(t)
	ctx := context.Background()
	got, err := LoadNorthStar(ctx, p)
	if err != nil || got.Decisive || got.Line() != "First decisive verdict: none yet ($0.00 spent since init)" {
		t.Fatalf("no experiments: %+v, %q, %v", got, got.Line(), err)
	}

	// Inconclusive and exploratory verdicts do not count, and an A/A never does: only the spend shows.
	tie := noisyTie(t)
	if v := verdictOf(t, tie, experiment.RolePrimary); v != stats.Inconclusive {
		t.Fatalf("premise: the noisy tie's cost verdict is %q, want inconclusive", v)
	}
	small := oneRun(experiment.MethodV2, experiment.TemplateContextAB)
	small.Name = "small"
	small.Lock.Design.Tasks = small.Lock.Design.Tasks[:4]
	small.Lock.Schedule = experiment.Schedule(small.Lock.Design)
	small.Runs = small.Runs[:0]
	for _, r := range oneRun(experiment.MethodV2, experiment.TemplateContextAB).Runs {
		if strings.Contains(" task-0 task-1 task-2 task-3 ", " "+r.Record.Task+" ") {
			small.Runs = append(small.Runs, r)
		}
	}
	if v := verdictOf(t, small, experiment.RolePrimary); v != stats.Exploratory {
		t.Fatalf("premise: the 4-task cost verdict is %q, want exploratory", v)
	}
	aa := oneRun(experiment.MethodV2, experiment.TemplateAA)
	aa.Name = "aa"
	for _, in := range []Input{tie, small, aa} {
		storeExperiment(t, p, in)
	}
	got, err = LoadNorthStar(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	want := total(tie) + total(small) + total(aa)
	if got.Decisive || math.Abs(got.SpentUSD-want) > 1e-6 || !strings.HasPrefix(got.Line(), "First decisive verdict: none yet ($") {
		t.Errorf("only inconclusive, exploratory and A/A experiments: %+v, %q (spent want %.4f)", got, got.Line(), want)
	}
}

func TestNorthStarDecisive(t *testing.T) {
	t.Parallel()
	p, registered := starProject(t)
	ctx := context.Background()
	tie := noisyTie(t)
	tie.Lock.LockedAt = tie.Lock.LockedAt.Add(-time.Hour) // before the decisive one
	for i := range tie.Runs {
		tie.Runs[i].Record.Started, tie.Runs[i].Record.Finished = tie.Runs[i].Record.Started.Add(-time.Hour), tie.Runs[i].Record.Finished.Add(-time.Hour)
	}
	decisive := oneRun(experiment.MethodV2, experiment.TemplateContextAB)
	decisive.Name = "lean-ab"
	if v := verdictOf(t, decisive, experiment.RolePrimary); !Decisive(v) {
		t.Fatalf("premise: the 20%% cheaper arm's cost verdict is %q, want decisive", v)
	}
	// A later experiment's spend is not counted, and a judged run counts its judge's spend.
	later := oneRun(experiment.MethodV2, experiment.TemplateContextAB)
	later.Name = "later"
	for i := range later.Runs {
		later.Runs[i].Record.Started, later.Runs[i].Record.Finished = later.Runs[i].Record.Started.Add(5*time.Hour), later.Runs[i].Record.Finished.Add(5*time.Hour)
	}
	decisive.Runs[0].Record.Judge = &judge.Verdict{CostUSD: 0.07}
	// A calibration before the experiment is project spend too (kind "calibration", outside every experiment).
	calibration := run.Record{ID: "cal", Metrics: agent.Metrics{CostUSD: 0.42}}
	calJSON, _ := json.Marshal(calibration)
	if err := p.DB.SaveRun(ctx, store.Run{ID: "cal", ProjectID: p.ID, Kind: "calibration", Outcome: agent.OutcomeOK, CostUSD: 0.42, Record: calJSON,
		Started: decisive.Lock.LockedAt.Add(-90 * time.Minute), Finished: decisive.Lock.LockedAt.Add(-89 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	for _, in := range []Input{tie, decisive, later} {
		storeExperiment(t, p, in)
	}
	got, err := LoadNorthStar(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	last := decisive.Runs[len(decisive.Runs)-1].Record
	if !got.Decisive || got.Experiment != "lean-ab" || got.Metric != experiment.MetricCost || !Decisive(got.Verdict) {
		t.Errorf("first decisive verdict %+v, want lean-ab on cost", got)
	}
	if want := last.Finished.Sub(registered).Seconds(); math.Abs(got.Seconds-want) > 1 {
		t.Errorf("seconds %.0f, want %.0f from registration to the last run's end", got.Seconds, want)
	}
	if want := total(tie) + total(decisive) + 0.42; math.Abs(got.SpentUSD-want) > 1e-6 {
		t.Errorf("spent $%.4f, want $%.4f: every run up to the decisive experiment's last, its judge included", got.SpentUSD, want)
	}
	line := got.Line()
	for _, s := range []string{"First decisive verdict: ", " on cost in lean-ab, 2h ", " after init, $"} {
		if !strings.Contains(line, s) {
			t.Errorf("line %q lacks %q", line, s)
		}
	}
}

// The report shows the line in the terminal and Markdown renderings and in JSON, only when Load set it.
func TestReportShowsTheNorthStar(t *testing.T) {
	t.Parallel()
	rep, err := Build(oneRun(experiment.MethodV2, experiment.TemplateContextAB))
	if err != nil {
		t.Fatal(err)
	}
	var plain strings.Builder
	if err := rep.Markdown(&plain); err != nil || strings.Contains(plain.String(), "First decisive verdict") {
		t.Fatalf("a report built without a north star shows one (%v)", err)
	}
	rep.NorthStar = &NorthStar{SpentUSD: 12.5}
	var md, txt, js strings.Builder
	if err := rep.Markdown(&md); err != nil {
		t.Fatal(err)
	}
	if err := rep.Terminal(&txt, term.Style{}); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&js); err != nil {
		t.Fatal(err)
	}
	const line = "First decisive verdict: none yet ($12.50 spent since init)."
	for name, out := range map[string]string{"markdown": md.String(), "terminal": txt.String()} {
		if !strings.Contains(out, line) {
			t.Errorf("%s lacks %q", name, line)
		}
	}
	if !strings.Contains(js.String(), `"north_star": {`) || !strings.Contains(js.String(), `"spent_usd": 12.5`) {
		t.Errorf("JSON lacks the north star:\n%.300s", js.String())
	}
}

func TestSpanFormat(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{40 * time.Second: "40s", 7 * time.Minute: "7m", 3*time.Hour + 12*time.Minute: "3h 12m", 51 * time.Hour: "2d 3h"} {
		if got := formatSpan(d); got != want {
			t.Errorf("formatSpan(%v) = %q, want %q", d, got, want)
		}
	}
}

// Only an experiment that ran to its end counts: one still running, or stopped for its budget, may change its verdict.
func TestNorthStarIgnoresUnfinishedExperiments(t *testing.T) {
	t.Parallel()
	p, _ := starProject(t)
	decisive := oneRun(experiment.MethodV2, experiment.TemplateContextAB)
	decisive.Name = "lean-ab"
	storeExperimentAs(t, p, decisive, store.StatusRunning)
	got, err := LoadNorthStar(context.Background(), p)
	if err != nil || got.Decisive || math.Abs(got.SpentUSD-total(decisive)) > 1e-6 {
		t.Fatalf("a running experiment: %+v, %v", got, err)
	}
	if err := p.DB.SetExperimentStatus(context.Background(), 1, store.StatusBudget, "budget"); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadNorthStar(context.Background(), p); got.Decisive {
		t.Errorf("an experiment stopped for its budget counts: %+v", got)
	}
	for _, status := range []string{store.StatusUsage, store.StatusStopped} {
		if err := p.DB.SetExperimentStatus(context.Background(), 1, status, ""); err != nil {
			t.Fatal(err)
		}
		if got, _ := LoadNorthStar(context.Background(), p); got.Decisive {
			t.Errorf("an experiment with status %s counts: %+v", status, got)
		}
	}
	if err := p.DB.SetExperimentStatus(context.Background(), 1, store.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadNorthStar(context.Background(), p); !got.Decisive {
		t.Errorf("the finished experiment does not count: %+v", got)
	}
}

// guardOnly is a 20 × 3 experiment whose arms differ by cost noise only (the primary metric is inconclusive) and whose
// every run passes (the success guard is decisive at the floor).
func guardOnly(t *testing.T) Input {
	t.Helper()
	in := fixture()
	d := &in.Lock.Design
	d.Tasks = nil
	for i := range 20 {
		d.Tasks = append(d.Tasks, fmt.Sprintf("task-%d", i))
	}
	in.Lock.Schedule = experiment.Schedule(*d)
	r := rand.New(rand.NewPCG(3, 4))
	yes := true
	in.Runs = nil
	for _, s := range in.Lock.Schedule {
		rec := run.Record{ID: fmt.Sprintf("r%03d", s.Position), Task: s.Task, Arm: s.Arm, Outcome: agent.OutcomeOK, Passed: &yes,
			Started:  in.Lock.LockedAt.Add(time.Duration(s.Position) * time.Minute),
			Finished: in.Lock.LockedAt.Add(time.Duration(s.Position)*time.Minute + 50*time.Second),
			Metrics:  agent.Metrics{CostUSD: 0.30 * math.Exp(0.6*r.NormFloat64()), DurationMS: 40000, OutputTokens: 3000, SawResult: true}}
		in.Runs = append(in.Runs, Run{ID: rec.ID, Slot: s.Position, Attempt: 1, Record: rec})
	}
	in.Name = "guard-only"
	return in
}

func TestNorthStarCountsADecisiveGuard(t *testing.T) {
	t.Parallel()
	in := guardOnly(t)
	if v := verdictOf(t, in, experiment.RolePrimary); v != stats.Inconclusive {
		t.Fatalf("premise: the primary verdict is %q, want inconclusive", v)
	}
	if v := verdictOf(t, in, experiment.RoleGuard); !Decisive(v) {
		t.Fatalf("premise: the guard verdict is %q, want decisive", v)
	}
	p, _ := starProject(t)
	storeExperiment(t, p, in)
	got, err := LoadNorthStar(context.Background(), p)
	if err != nil || !got.Decisive || got.Metric != experiment.MetricSuccess {
		t.Errorf("north star %+v, %v: want decisive on success (the guard)", got, err)
	}
}

// An experiment with judge-graded tasks never counts toward the north star, whatever its verdict, until the judge's
// paid real check validates it: its cost verdict has no tests' guard over those tasks. The line says what it left out
// and why; a project without such experiments reads as before.
func TestNorthStarLeavesOutJudgeGradedExperiments(t *testing.T) {
	t.Parallel()
	in := oneRun(experiment.MethodV2, experiment.TemplateContextAB)
	in.Name = "tickets"
	l := &in.Lock
	grading := judge.GradingSettings()
	l.Design.JudgeGraded, l.Design.JudgeGrading = []string{"task-9"}, &grading
	l.Design.Version = l.Design.WantVersion()
	for i, lt := range l.Tasks {
		if lt.Name == "task-9" {
			l.Tasks[i] = experiment.NewLockedTask(lt.Name, lt.Instruction, task.Spec{Base: "base-commit", Solution: "solution-commit",
				Reference: []string{"value.go"}, Verify: []string{"make test"}, Grading: task.GradingJudge})
		}
	}
	for i := range in.Runs {
		if in.Runs[i].Record.Task == "task-9" {
			in.Runs[i].Record.GradedBy = task.GradingJudge
		}
	}
	if v := verdictOf(t, in, experiment.RolePrimary); !Decisive(v) {
		t.Fatalf("premise: the cost verdict is %q, want decisive", v)
	}
	p, _ := starProject(t)
	storeExperiment(t, p, in)
	got, err := LoadNorthStar(context.Background(), p)
	if err != nil || got.Decisive || len(got.LeftOut) != 1 || got.LeftOut[0].Experiment != "tickets" || got.LeftOut[0].Why != whyJudgeGraded {
		t.Fatalf("north star %+v, %v: want none, tickets left out", got, err)
	}
	if line := got.Line(); !strings.HasSuffix(line, "; left out: tickets (it has judge-graded tasks, whose grades are unvalidated until the judge's paid real check)") ||
		!strings.HasPrefix(line, "First decisive verdict: none yet") {
		t.Errorf("line %q", line)
	}
	if js, _ := json.Marshal(got); !strings.Contains(string(js), `"left_out":[{"experiment":"tickets","why":"it has judge-graded tasks`) {
		t.Errorf("JSON %s", js)
	}
	// A test-graded experiment beside it still counts; nothing is said to be left out without such experiments.
	plain := oneRun(experiment.MethodV2, experiment.TemplateContextAB)
	plain.Name = "lean-ab"
	storeExperiment(t, p, plain)
	if got, err := LoadNorthStar(context.Background(), p); err != nil || !got.Decisive || got.Experiment != "lean-ab" || len(got.LeftOut) != 1 {
		t.Errorf("beside a test-graded experiment: %+v, %v", got, err)
	}
	if js, _ := json.Marshal(NorthStar{SpentUSD: 1}); strings.Contains(string(js), "left_out") || strings.Contains(NorthStar{SpentUSD: 1}.Line(), "left out") {
		t.Errorf("a north star without left-out experiments mentions them: %s", js)
	}
}
