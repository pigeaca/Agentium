package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/report/reporttest"
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
// both judges, too few pairs for the pair judge, and seq-v1 experiments that stopped early (sure enough) and for
// futility.
func reportScenes(t *testing.T) map[string]reporttest.Experiment {
	t.Helper()
	decisive := reporttest.OneRun(experiment.MethodV2, experiment.TemplateContextAB)

	// Not sure: the A/A's runs, with the second arm's context named: no true difference, ten tasks too few to tell.
	unsure := reporttest.OneRun(experiment.MethodV2, experiment.TemplateAA)
	unsure.Name = "lean-ab-unsure"
	l := &unsure.Lock
	l.Design.Template = experiment.TemplateContextAB
	l.Design.Arms[1] = experiment.Arm{Name: "B", Context: "lean", Snapshot: "b808beb3ddbaea19f7643ae0c20ec8167641da46"}
	l.Arms[1].Arm = l.Design.Arms[1]

	stopped, err := reporttest.Seq(0.5, 16, true, experiment.StatusDone)
	if err != nil {
		t.Fatal(err)
	}
	futility, err := reporttest.Seq(1.0, 16, true, experiment.StatusDone)
	if err != nil {
		t.Fatal(err)
	}
	futility.Name = "lean-seq-futility"
	return map[string]reporttest.Experiment{"decisive": decisive, "unsure": unsure, "aa": reporttest.OneRun(experiment.MethodV2, experiment.TemplateAA),
		"judged": reporttest.JudgedPairs(), "few-pairs": reporttest.FewPairs(), "seq-stopped": stopped, "seq-futility": futility}
}

// TestReportViewPreview writes the report view of each scene, as a terminal of 80 columns at 256 colors shows it,
// into the folder AGENTIUM_REPORT_DEMO names, for scripts/readme_images/ansi2svg.py. Skipped unless asked.
func TestReportViewPreview(t *testing.T) {
	dir := os.Getenv("AGENTIUM_REPORT_DEMO")
	if dir == "" {
		t.Skip("set AGENTIUM_REPORT_DEMO to a folder to write the report view's previews")
	}
	sh := term.Shapes{Style: term.Colored().WithDepth(term.Color256)}
	for name, e := range reportScenes(t) {
		rep := buildReport(t, e)
		lines := reportView(rep, sh, 80)
		text := "\x1b[36m$\x1b[39m agentium experiment report " + e.Name + "\n" + strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "report-"+name+".ans"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// reportWidths are the widths the goldens pin: the narrowest designed terminal (the versions' boxes stacked), the
// common ones, and a wide one (the content stops at term.MaxContentWidth).
var reportWidths = []int{term.MinWidth, 80, 100, 120}

// The report view of each scene at each width, in plain Unicode; the decisive one also in 256 colors and in ASCII. No
// line is wider than the terminal.
func TestReportViewGoldens(t *testing.T) {
	scenes := reportScenes(t)
	for _, name := range []string{"decisive", "unsure", "aa", "judged", "few-pairs", "seq-stopped", "seq-futility"} {
		rep := buildReport(t, scenes[name])
		var b strings.Builder
		for _, width := range reportWidths {
			fmt.Fprintf(&b, "=== %d columns\n", width)
			for _, line := range reportView(rep, plainUnicode, width) {
				if w := term.Width(line); w > width {
					t.Errorf("%s at %d columns: a line of %d cells: %q", name, width, w, line)
				}
				b.WriteString(line + "\n")
			}
		}
		checkGolden(t, "report-"+name+".golden", b.String())
	}
	rep := buildReport(t, scenes["decisive"])
	checkGolden(t, "report-decisive-color.golden", strings.Join(reportView(rep, color256, 80), "\n")+"\n")
	checkGolden(t, "report-decisive-ascii.golden", strings.Join(reportView(rep, plainASCII, 80), "\n")+"\n")
}

// The answer is the dashboard's: the view's headline and status are answerWords' for the report's answer, in every
// scene, so the two screens never word a verdict apart.
func TestReportViewReusesTheAnswerWords(t *testing.T) {
	want := map[string][2]string{
		"decisive":     {"lean is cheaper: 22% less", "after all 10 tasks · sure enough"},
		"unsure":       {"no clear difference in cost", "after all 10 tasks · not sure yet · about 57 tasks would settle it"},
		"aa":           {"no clear difference in cost", "after all 10 tasks · not sure"},
		"seq-stopped":  {"lean is cheaper: 50% less", "stopped early: sure enough"},
		"seq-futility": {"no clear difference in cost", "stopped early · more tasks are unlikely to settle it"},
	}
	scenes := reportScenes(t)
	for name, words := range want {
		rep := buildReport(t, scenes[name])
		f := factsOf(rep.Lock, 0)
		a, _ := reportAnswerState(rep, f)
		headline, status := answerWords(a, f.labels, f.aa)
		if headline != words[0] || status != words[1] {
			t.Errorf("%s: answerWords %q · %q, want %q · %q", name, headline, status, words[0], words[1])
		}
		view := strings.Join(reportView(rep, plainUnicode, 100), "\n")
		if !strings.Contains(view, headline) || !strings.Contains(view, status) {
			t.Errorf("%s: the view does not show the answer's words %q and %q:\n%s", name, headline, status, view)
		}
	}
}

// The judges in plain words: the per-run judge per version, the pair judge's preference by task with "(unvalidated"
// and whether it could be chance, and "too few to say" below the floor.
func TestReportViewJudgeLines(t *testing.T) {
	scenes := reportScenes(t)
	judged := strings.Join(reportView(buildReport(t, scenes["judged"]), plainUnicode, 100), "\n")
	for _, want := range []string{"the judge's opinion  an AI reading the fixes, not a test", "baseline: the judge thinks ", " passing fixes are right",
		"lean: the judge thinks ", "the judge preferred lean's fix in 6 of 10 tasks (unvalidated, may be chance)"} {
		if !strings.Contains(judged, want) {
			t.Errorf("the judged view lacks %q:\n%s", want, judged)
		}
	}
	few := strings.Join(reportView(buildReport(t, scenes["few-pairs"]), plainUnicode, 100), "\n")
	if !strings.Contains(few, "which fix is better: too few to say (3 tasks with a preference; it takes 5)") || strings.Contains(few, "the judge preferred") {
		t.Errorf("below the floor the view must say too few:\n%s", few)
	}
	if plain := strings.Join(reportView(buildReport(t, scenes["decisive"]), plainUnicode, 100), "\n"); strings.Contains(plain, "judge") {
		t.Errorf("a view without judges names one:\n%s", plain)
	}
}

// The grid lists only the tasks where the versions ended differently and counts the rest in one line; tasks a seq-v1
// experiment never ran are left out.
func TestReportViewCollapsedGrid(t *testing.T) {
	scenes := reportScenes(t)
	rep := buildReport(t, scenes["decisive"])
	differ := 0
	for _, task := range rep.Tasks {
		if a, b := task.Arms["A"], task.Arms["B"]; a.Successes != b.Successes || a.Counted != b.Counted {
			differ++
		}
	}
	view := reportView(rep, plainUnicode, 80)
	rows := 0
	for _, line := range view {
		if strings.HasPrefix(strings.TrimSpace(line), "task-") {
			rows++
		}
	}
	text := strings.Join(view, "\n")
	if rows != differ || !strings.Contains(text, fmt.Sprintf("+ %d tasks where both ended the same: 3 passed", len(rep.Tasks)-differ)) {
		t.Errorf("%d rows for %d differing tasks:\n%s", rows, differ, text)
	}
	seq := strings.Join(reportView(buildReport(t, scenes["seq-stopped"]), plainUnicode, 80), "\n")
	if !strings.Contains(seq, "every task ended the same in both: 8 passed\n") || !strings.Contains(seq, "8 of 16 tasks") {
		t.Errorf("a stopped seq-v1 experiment's unrun tasks must stay out of the grid:\n%s", seq)
	}
}

// Text from outside Agentium (task and context names) is sanitized: no escape reaches the terminal but the view's own.
func TestReportViewSanitizes(t *testing.T) {
	e := reporttest.OneRun(experiment.MethodV2, experiment.TemplateContextAB)
	evil := "\x1b]0;owned\x07lean\x1b[2J"
	e.Lock.Design.Arms[1].Context, e.Lock.Arms[1].Context = evil, evil
	for i := range e.Lock.Tasks {
		if e.Lock.Tasks[i].Name == "task-1" {
			e.Lock.Tasks[i].Name = "task-1\x1b[31m"
		}
	}
	for i := range e.Runs {
		if e.Runs[i].Record.Task == "task-1" {
			e.Runs[i].Record.Task = "task-1\x1b[31m"
		}
	}
	for i, s := range e.Lock.Schedule {
		if s.Task == "task-1" {
			e.Lock.Schedule[i].Task = "task-1\x1b[31m"
		}
	}
	view := strings.Join(reportView(buildReport(t, e), plainUnicode, 80), "\n")
	if strings.ContainsAny(view, "\x1b\x07") || !strings.Contains(view, "lean") || !strings.Contains(view, "task-1") {
		t.Errorf("an escape from outside reached the view:\n%q", view)
	}
}

// A wrapped note keeps its grey on its second line.
func TestReportViewWrappedLinesKeepTheirColor(t *testing.T) {
	lines := wrapped(color256.Style, "  ", "3 runs not counted (1 setup changed, 1 infrastructure, 1 stopped); their cost is in the total", term.Muted, 60)
	muted := color256.Style.Paint(term.Muted, "x")
	open := muted[:strings.Index(muted, "x")]
	if len(lines) < 2 {
		t.Fatalf("not wrapped: %q", lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(strings.TrimLeft(l, " "), open) {
			t.Errorf("a wrapped line lost its color: %q", l)
		}
	}
	judgeLine := wrapped(color256.Style, "  ", "the judge preferred "+color256.Style.Paint(term.ArmB, "lean")+"'s fix in 6 of 10 tasks"+
		color256.Style.Paint(term.Muted, " (unvalidated, may be chance)"), term.Default, 60)
	if len(judgeLine) != 2 || !strings.HasPrefix(strings.TrimLeft(judgeLine[1], " "), open) {
		t.Errorf("the judge's wrapped line lost its grey: %q", judgeLine)
	}
}
