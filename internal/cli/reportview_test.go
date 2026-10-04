package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/report/reporttest"
	"github.com/pigeaca/agentium/internal/stats"
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
		"judged": reporttest.JudgedPairs(), "few-pairs": reporttest.FewPairs(), "seq-stopped": stopped, "seq-futility": futility,
		"judge-graded": reporttest.JudgeGraded()}
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
	for _, name := range []string{"decisive", "unsure", "aa", "judged", "few-pairs", "seq-stopped", "seq-futility", "judge-graded"} {
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
		"unsure":       {"no clear difference in cost", "after all 10 tasks · not sure yet · about 57 tasks in all could settle it"},
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
	e.Name = "lean\x1b[2J-ab\x07"
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
	if strings.ContainsAny(view, "\x1b\x07") || !strings.Contains(view, "lean") || !strings.Contains(view, "task-1") ||
		!strings.Contains(view, "agentium experiment report lean-ab --details") {
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

// The answer is never an unqualified yes: the other side of the question shows in the box, in words from the guard's
// verdict when it has one, else as too few to tell.
func TestReportViewGuardWords(t *testing.T) {
	scenes := reportScenes(t)
	rep := buildReport(t, scenes["decisive"])
	f := factsOf(rep.Lock, 0)
	if words, role := guardWords(rep, f); words != "whether lean passes as many tasks: too few to tell" || role != term.Muted {
		t.Errorf("without a verdict: %q (%v)", words, role)
	}
	for i, r := range rep.Analysis.Results {
		if r.Role == experiment.RoleGuard {
			rep.Analysis.Results[i].Verdict = stats.NoLoss
		}
	}
	words, role := guardWords(rep, f)
	if words != "lean passes no fewer tasks, within 15 points (lean 90%, baseline 40%)" || role != term.VerdictNoLoss {
		t.Errorf("with a verdict: %q (%v)", words, role)
	}
	if view := strings.Join(reportView(rep, plainUnicode, 100), "\n"); !strings.Contains(view, words) {
		t.Errorf("the box lacks the guard:\n%s", view)
	}
	seq := buildReport(t, scenes["seq-stopped"]) // seq-v1: success is exploratory, still said
	if words, _ := guardWords(seq, factsOf(seq.Lock, 0)); words != "whether lean passes as many tasks: too few to tell" {
		t.Errorf("seq-v1: %q", words)
	}
	aa := buildReport(t, scenes["aa"])
	if words, _ := guardWords(aa, factsOf(aa.Lock, 0)); words != "" {
		t.Errorf("an A/A: %q", words)
	}
}

// An early seq-v1 stop says that the saving is likely smaller than its estimate, as the Markdown does; a futility
// stop and a fixed design do not.
func TestReportViewEarlyStopNote(t *testing.T) {
	scenes := reportScenes(t)
	for name, want := range map[string]bool{"seq-stopped": true, "seq-futility": false, "decisive": false} {
		view := strings.Join(reportView(buildReport(t, scenes[name]), plainUnicode, 100), "\n")
		if got := strings.Contains(view, "it stopped early: the true saving is likely smaller than 50%"); got != want && name == "seq-stopped" ||
			name != "seq-stopped" && strings.Contains(view, "it stopped early:") {
			t.Errorf("%s: early-stop note shown %v, want %v:\n%s", name, got, want, view)
		}
	}
}

// A seq-v1 experiment whose last look counted nothing new keeps the last analysed look's answer, as the report does.
func TestReportViewKeepsTheLastAnalysedLook(t *testing.T) {
	e, err := reporttest.Seq(0.8, 26, false, experiment.StatusStopped)
	if err != nil {
		t.Fatal(err)
	}
	var runs []reporttest.Run
	for _, r := range e.Runs {
		if r.Slot >= 16 && r.Slot < 24 { // stage 2: three infrastructure failures a slot, so look 2 counts nothing new
			for attempt := 1; attempt <= 3; attempt++ {
				failed := r
				failed.Attempt, failed.Record.Outcome, failed.Record.Passed = attempt, claude.OutcomeInfra, nil
				runs = append(runs, failed)
			}
			continue
		}
		runs = append(runs, r)
	}
	e.Runs = runs
	rep := buildReport(t, e)
	s := rep.Analysis.Sequential
	if len(s.Looks) != 2 || s.Looks[1].Analysed || !s.Looks[0].Analysed {
		t.Fatalf("looks %+v: want look 1 analysed and look 2 not", s.Looks)
	}
	f := factsOf(rep.Lock, 0)
	a, _ := reportAnswerState(rep, f)
	if a.Verdict != s.Looks[0].Verdict || !a.HasEstimate {
		t.Errorf("answer %+v: want look 1's verdict %q", a, s.Looks[0].Verdict)
	}
	headline, _ := answerWords(a, f.labels, f.aa)
	view := strings.Join(reportView(rep, plainUnicode, 100), "\n")
	if !strings.Contains(view, headline) || strings.Contains(view, "too few tasks finished") || strings.Contains(view, "too early to tell") {
		t.Errorf("the view lost look 1's answer %q:\n%s", headline, view)
	}
}

// A lopsided preference that a binomial test tells from an even split still says it is unvalidated, and no more that
// it may be chance.
func TestReportViewLopsidedPairs(t *testing.T) {
	e := reporttest.JudgedPairs()
	for i := range e.Runs {
		if c := e.Runs[i].Record.PairJudge; c != nil {
			c.Verdict.Prefer, c.Verdict.Flip = judge.PreferB, false
			c.Verdict.AB.Answered, c.Verdict.BA.Answered = true, true
		}
	}
	rep := buildReport(t, e)
	if p := rep.PairJudge.Tasks; p.A != 0 || p.B < 6 || p.P >= 0.05 {
		t.Fatalf("pairs %+v: want a lopsided preference", p)
	}
	view := strings.Join(reportView(rep, plainUnicode, 100), "\n")
	want := fmt.Sprintf("the judge preferred lean's fix in %d of %d tasks (unvalidated)", rep.PairJudge.Tasks.B, rep.PairJudge.Tasks.Complete)
	if !strings.Contains(view, want) || strings.Contains(view, "may be chance") {
		t.Errorf("want %q:\n%s", want, view)
	}
}

// A cost a run cut short makes a lower bound: "≥", or ">=" where the locale is not UTF-8.
func TestReportViewAtLeastMark(t *testing.T) {
	e := reporttest.OneRun(experiment.MethodV2, experiment.TemplateContextAB)
	for i := range e.Runs {
		if r := &e.Runs[i].Record; r.Task == "task-1" && r.Arm == "B" {
			r.Outcome = claude.OutcomeCapped
		}
	}
	rep := buildReport(t, e)
	unicode, ascii := strings.Join(reportView(rep, plainUnicode, 80), "\n"), strings.Join(reportView(rep, plainASCII, 80), "\n")
	if !strings.Contains(unicode, "≥$0.27") || !strings.Contains(ascii, ">=$0.27") || strings.Contains(ascii, "≥") {
		t.Errorf("the lower bound's mark:\n%s\n%s", unicode, ascii)
	}
	if !strings.Contains(unicode, "! 1 run cut short at the cap") {
		t.Errorf("the arm's box lacks the run cut short:\n%s", unicode)
	}
}

// The last note's command stays whole on a narrow terminal.
func TestReportViewKeepsTheCommandWhole(t *testing.T) {
	scenes := reportScenes(t)
	view := reportView(buildReport(t, scenes["seq-futility"]), plainUnicode, term.MinWidth)
	if !strings.Contains(strings.Join(view, "\n"), "  agentium experiment report lean-seq-futility --details") {
		t.Errorf("the command was split:\n%s", strings.Join(view, "\n"))
	}
}

// fitParts keeps the status's height: it drops its least important parts, never cuts one that fits alone.
func TestFitParts(t *testing.T) {
	status := "after all 16 tasks · not sure yet · about 57 tasks in all could settle it"
	for width, want := range map[int]string{
		80: status,
		60: "not sure yet · about 57 tasks in all could settle it",
		40: "not sure yet",
		8:  "not sur…",
	} {
		if got := fitParts(status, width, unicodeMarks); got != want {
			t.Errorf("width %d: %q, want %q", width, got, want)
		}
	}
	if got := fitParts("stopped early · more tasks are unlikely to settle it", 30, unicodeMarks); got != "more tasks are unlikely to se…" {
		t.Errorf("futility at 30: %q", got)
	}
	lines := answerBox(plainUnicode, unicodeMarks, answerState{Metric: experiment.MetricCost, Verdict: stats.Inconclusive, Decision: experiment.LookFinal,
		Seq: true, All: 16, Settle: 57}, factsOf(screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), 85), 44)
	if len(lines) != 5 || strings.Contains(strings.Join(lines, "\n"), "…") || !strings.Contains(lines[3], "not sure yet") {
		t.Errorf("a narrow answer box:\n%s", strings.Join(lines, "\n"))
	}
}
