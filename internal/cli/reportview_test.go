package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/report/reporttest"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/term"
)

// buildReport is a test experiment's report.
func buildReport(t *testing.T, e reporttest.Experiment) report.Report {
	t.Helper()
	in := report.Input{Name: e.Name, Lock: e.Lock, Status: e.Status, StatusNote: e.StatusNote, DataDir: e.DataDir, Home: e.Home}
	for _, r := range e.Runs {
		in.Runs = append(in.Runs, report.Run(r))
	}
	rep, err := report.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// reportScenes are the report view's test experiments, by name: a decisive cost answer, one that is not sure, an A/A,
// and one with both judges.
func reportScenes() map[string]reporttest.Experiment {
	decisive := reporttest.OneRun(experiment.MethodV2, experiment.TemplateContextAB)

	// Not sure: the A/A's runs, with the second arm's context named: no true difference, ten tasks too few to tell.
	unsure := reporttest.OneRun(experiment.MethodV2, experiment.TemplateAA)
	unsure.Name = "lean-ab-unsure"
	l := &unsure.Lock
	l.Design.Template = experiment.TemplateContextAB
	l.Design.Arms[1] = experiment.Arm{Name: "B", Context: "lean", Snapshot: "b808beb3ddbaea19f7643ae0c20ec8167641da46"}
	l.Arms[1].Arm = l.Design.Arms[1]

	aa := reporttest.OneRun(experiment.MethodV2, experiment.TemplateAA)
	aa.Lock.Design.Arms[1] = aa.Lock.Arms[1].Arm // the fixture's design keeps the second context's name

	// Both judges, on three runs per task and arm: the per-run judge's verdicts are reporttest.Judged's, and the pair
	// judge prefers lean's fix in most pairs that both passed.
	judged := reporttest.Judged()
	judged.Lock.Design.JudgePairs = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 1}
	passed := map[[2]any]bool{} // task and repeat
	for _, r := range judged.Runs {
		if r.Record.Arm == "A" && r.Record.Passed != nil && *r.Record.Passed {
			passed[[2]any{r.Record.Task, judged.Lock.Schedule[r.Slot].Repeat}] = true
		}
	}
	n := 0
	for i := range judged.Runs {
		r := &judged.Runs[i]
		rec := &r.Record
		if rec.Arm != "B" || rec.Passed == nil || !*rec.Passed || !passed[[2]any{rec.Task, judged.Lock.Schedule[r.Slot].Repeat}] {
			continue
		}
		prefer := judge.PreferB
		switch n % 5 {
		case 1:
			prefer = judge.PreferA
		case 3:
			prefer = judge.PreferTie
		}
		n++
		rec.PairJudge = &run.PairJudgement{RunA: "a", Verdict: judge.PairVerdict{Version: judge.PairVersion, Prefer: prefer, Model: judge.DefaultModel, CostUSD: 0.18}}
	}
	return map[string]reporttest.Experiment{"decisive": decisive, "unsure": unsure, "aa": aa, "judged": judged}
}

// TestReportViewPreview writes the report view of each scene, as a terminal of 80 columns at 256 colors shows it,
// into the folder AGENTIUM_REPORT_DEMO names, for scripts/readme_images/ansi2svg.py. Skipped unless asked.
func TestReportViewPreview(t *testing.T) {
	dir := os.Getenv("AGENTIUM_REPORT_DEMO")
	if dir == "" {
		t.Skip("set AGENTIUM_REPORT_DEMO to a folder to write the report view's previews")
	}
	sh := term.Shapes{Style: term.Colored().WithDepth(term.Color256)}
	for name, e := range reportScenes() {
		rep := buildReport(t, e)
		lines := reportView(rep, sh, 80)
		text := "\x1b[36m$\x1b[39m agentium experiment report " + e.Name + "\n" + strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "report-"+name+".ans"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
