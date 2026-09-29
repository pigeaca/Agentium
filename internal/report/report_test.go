package report

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/task"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixture is a context A/B of 10 tasks × 3 runs per arm: arm B's context is smaller and cheaper, success equal but
// mixed; one run of each kind that is not counted; one pass with changed runner configuration; one recovered run.
func fixture() Input {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	d := experiment.Design{Version: experiment.DesignVersion, Template: experiment.TemplateContextAB,
		Arms:    []experiment.Arm{{Name: "A", Context: "base"}, {Name: "B", Context: "lean", Snapshot: "b808beb3ddbaea19f7643ae0c20ec8167641da46"}},
		Repeats: 3, Model: "claude-sonnet-5", Goal: experiment.GoalCheaper, CostMargin: 0.10, SuccessMargin: 0.15, RunBudgetUSD: 3, BudgetUSD: 60,
		Timeout: 20 * time.Minute, VerifyTimeout: 10 * time.Minute, Concurrency: 2, Seed: 42}
	for i := range 10 {
		d.Tasks = append(d.Tasks, fmt.Sprintf("task-%d", i))
	}
	l := experiment.Lock{Method: experiment.MethodVersion, Agentium: "test", LockedAt: at, ClaudeCode: "2.1.281", ClaudePath: "/usr/local/bin/claude",
		SignIn: claude.SignInLogin, Host: "darwin/arm64", PriceTable: "2026-09-29", Design: d, Schedule: experiment.Schedule(d), MaxAttempts: 3}
	for _, a := range d.Arms {
		l.Arms = append(l.Arms, experiment.LockedArm{Arm: a, Calibration: "cal-" + a.Name, Model: "claude-sonnet-5", Tools: []string{"Bash", "Edit", "Read"},
			Skills: []string{"personal-looking-skill"}, SlashCommands: []string{"compact", "review"}})
	}
	for _, name := range d.Tasks {
		l.Tasks = append(l.Tasks, experiment.NewLockedTask(name, "Fix "+name+".", task.Spec{Base: "base-commit", Verify: []string{"make test"}}))
	}
	var runs []Run
	yes, no := true, false
	for _, s := range l.Schedule {
		var ti int
		fmt.Sscanf(s.Task, "task-%d", &ti)
		cost := 0.30 * (1 + 0.25*float64(ti%3)) * (1 + 0.04*float64(s.Repeat))
		first := int64(30000)
		if s.Arm == "B" {
			cost *= 0.8
			first = 27000
		}
		passed := &yes
		if (ti+s.Repeat)%4 == 0 {
			passed = &no
		}
		rec := run.Record{ID: fmt.Sprintf("r%02d", s.Position), Task: s.Task, Arm: s.Arm, Model: d.Model, SignIn: claude.SignInLogin,
			Outcome: claude.OutcomeOK, Passed: passed, ContextHead: "ctx", RecordsDir: "/home/someone/.agentium/records/x",
			Started: at.Add(time.Duration(s.Position) * time.Minute), Finished: at.Add(time.Duration(s.Position)*time.Minute + 50*time.Second),
			Metrics: claude.Metrics{CLIVersion: "2.1.281", Model: "claude-sonnet-5", CostUSD: cost, DurationMS: int64(40000 + 1000*ti), InputTokens: 50,
				OutputTokens: int64(3000 + 100*ti), CacheReadTokens: 400000, CacheWriteTokens: 30000, FirstRequest: first, SawInit: true, SawResult: true},
			Behavior: run.Behavior{FilesChanged: 2, LinesAdded: 10, LinesRemoved: 3, TestsChanged: s.Arm == "A", RanTests: true, RanChecks: s.Arm == "A",
				BashCommands: 6}, Verify: []task.Command{{Command: "make test", ExitCode: 0, Seconds: 1.5}}}
		switch s.Position {
		case 3:
			rec.Outcome, rec.Passed, rec.Drift = claude.OutcomeUnfair, nil, []string{"tools differ (added Monitor; missing none)"}
		case 7:
			rec.Behavior.ConfigChanged = []string{"pytest.ini"}
		}
		runs = append(runs, Run{ID: rec.ID, Slot: s.Position, Attempt: 1, Record: rec})
		if s.Position == 10 { // an earlier attempt that failed for infrastructure, and one recovered after a stop
			infra := rec
			infra.ID, infra.Outcome, infra.Passed = "r10-infra", claude.OutcomeInfra, nil
			infra.Metrics.CostUSD = 0.05
			infra.Notes = []string{"Claude Code reported no cost: estimated from the transcript's requests at list prices"}
			cancelled := rec
			cancelled.ID, cancelled.Outcome, cancelled.Passed = "r10-cancelled", claude.OutcomeCancelled, nil
			cancelled.Notes = []string{"Agentium stopped during this run; recovered on 2026-09-29 12:30"}
			runs = append(runs[:len(runs)-1], Run{ID: infra.ID, Slot: 10, Attempt: 1, Record: infra}, Run{ID: cancelled.ID, Slot: 10, Attempt: 2, Record: cancelled},
				Run{ID: rec.ID, Slot: 10, Attempt: 2, Record: rec})
		}
	}
	return Input{Name: "lean-ab", Lock: l, Status: experiment.StatusDone, Runs: runs}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update after checking):\n%s", name, got)
	}
}

func TestReportGolden(t *testing.T) {
	rep, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	var md, js bytes.Buffer
	if err := rep.Markdown(&md); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&js); err != nil {
		t.Fatal(err)
	}
	golden(t, "lean-ab.md", md.Bytes())
	golden(t, "lean-ab.json", js.Bytes())
}

func TestReportContents(t *testing.T) {
	rep, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	var md, js bytes.Buffer
	rep.Markdown(&md)
	rep.JSON(&js)
	text := md.String()
	for _, want := range []string{"# Experiment lean-ab", "Context A/B: A = `base`, B = `lean`. Goal: cheaper, without losing success.",
		"**Cost -20%", "]: improved.", "**Success ", "exploratory", "60 of 60 runs settled (done)", "| B | `lean` |", "(-3000)",
		"Runs not counted: 1 unfair (the environment drifted), 1 infrastructure failure, 1 cancelled.",
		"Environment drift in unfair runs: tools differ (added Monitor; missing none).", "1 run(s) ended without Claude Code's cost",
		"1 run(s) were recovered", "Arm ", "passed with test-runner configuration changed",
		"Success is exploratory: 9 of 10 task(s) have 3 counted runs in both arms, below the floor of 20.",
		"Cold-cache cost prices every cached read", "| task-0 |"} {
		if !strings.Contains(text, want) {
			t.Errorf("Markdown lacks %q", want)
		}
	}
	for _, private := range []string{"personal-looking-skill", "/home/someone", "\"review\"", "/usr/local/bin"} {
		if strings.Contains(js.String(), private) || strings.Contains(text, private) {
			t.Errorf("the report shows %q", private)
		}
	}
	if !strings.Contains(js.String(), "(1 skill)") || !strings.Contains(js.String(), "(2 slash commands)") || !strings.Contains(js.String(), `"claude_path": "claude"`) {
		t.Error("the JSON lock should count skills and slash commands")
	}
	// Counted runs: 60 slots, one unfair; the infra and cancelled tries of slot 10 are extra runs, not counted.
	if rep.Arms[0].Counted+rep.Arms[1].Counted != 59 || len(rep.Runs) != 62 || rep.Settled != 60 {
		t.Errorf("counted %d+%d, runs %d, settled %d", rep.Arms[0].Counted, rep.Arms[1].Counted, len(rep.Runs), rep.Settled)
	}
}

func TestReportAA(t *testing.T) {
	in := fixture()
	in.Lock.Design.Template = experiment.TemplateAA
	in.Lock.Arms[1].Context, in.Lock.Arms[1].Snapshot = "base", ""
	in.Status, in.StatusNote = experiment.StatusBudget, "the next run would not fit the $60.00 budget"
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var md bytes.Buffer
	rep.Markdown(&md)
	for _, want := range []string{"A/A calibration of context `base`", "any difference is noise",
		"The experiment is not finished (budget: the next run would not fit the $60.00 budget)", "Measured noise, for planning later experiments"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("A/A report lacks %q", want)
		}
	}
}

// A guard loss the intervals can tell from none is "regressed", even within the margin (the study's order of rules);
// the headline says so.
func TestHeadlineGuardLossWithinTheMargin(t *testing.T) {
	a, b := 0.80, 0.77
	res := experiment.MetricResult{Metric: experiment.MetricSuccess, Role: experiment.RoleGuard, Tasks: 40, A: &a, B: &b,
		Boot95: stats.Interval{Estimate: -0.03, Low: -0.05, High: -0.01}, T95: stats.Interval{Estimate: -0.03, Low: -0.05, High: -0.01},
		Verdict: stats.Regressed}
	got := headline(res, experiment.Design{SuccessMargin: 0.15})
	if !strings.Contains(got, "regressed (a loss the intervals can tell from none, though within the 15 pp margin)") || !strings.Contains(got, "80% → 77%") {
		t.Errorf("headline %q", got)
	}
}
