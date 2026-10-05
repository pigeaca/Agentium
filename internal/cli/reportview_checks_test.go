package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/term"
)

// checksReport is the decisive scene's report with rule checks counted over its runs: the first version met most of
// them, the second fewer, one check has runs that could not be read, and one name is hostile.
func checksReport(t *testing.T, names ...string) report.Report {
	t.Helper()
	e := reportScenes(t)["decisive"]
	in := report.Input{Name: e.Name, Lock: e.Lock, Status: e.Status, StatusNote: e.StatusNote, DataDir: e.DataDir, Home: e.Home}
	for _, r := range e.Runs {
		in.Runs = append(in.Runs, report.Run(r))
	}
	in.Checks = []run.Check{{Name: "ran-go-test", Kind: run.CheckRan, Pattern: "go test"}, {Name: "touched-tests", Kind: run.CheckChanged, Pattern: "**/*_test.go"},
		{Name: "kept-docs", Kind: run.CheckNotChanged, Pattern: "docs/**"}, {Name: "read-badly", Kind: run.CheckRan, Pattern: "make"}}
	if len(names) > 0 {
		in.Checks = in.Checks[:0]
		for _, n := range names {
			in.Checks = append(in.Checks, run.Check{Name: n, Kind: run.CheckRan, Pattern: "x"})
		}
	}
	in.CheckResults = map[string][]run.CheckResult{}
	seen := map[string]int{}
	for _, r := range in.Runs {
		arm := r.Record.Arm
		k := seen[arm]
		seen[arm]++
		res := make([]run.CheckResult, len(in.Checks))
		for i := range res {
			res[i] = run.CheckMet
			switch {
			case len(names) > 0:
			case i == 0 && arm != "A" && k%3 == 2, i == 1 && k%2 == 1:
				res[i] = run.CheckUnmet
			case i == 3 && arm != "A" && k%4 == 0:
				res[i] = run.CheckUnread
			}
		}
		in.CheckResults[r.ID] = res
	}
	rep, err := report.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// The block "what the agents did": a line per check with a bar and "7 of 8" for each version; under 70 columns no bars;
// the footer says they are counts. It sits after the task block and before the notes, and is absent without checks.
func TestReportViewRuleChecks(t *testing.T) {
	rep := checksReport(t)
	var b strings.Builder
	for _, width := range []int{term.MinWidth, 80, 100, 120} {
		fmt.Fprintf(&b, "=== %d columns\n", width)
		for _, line := range reportView(rep, plainUnicode, width) {
			if w := term.Width(line); w > width {
				t.Errorf("at %d columns: a line of %d cells: %q", width, w, line)
			}
			b.WriteString(line + "\n")
		}
	}
	checkGolden(t, "report-checks.golden", b.String())
	checkGolden(t, "report-checks-color.golden", strings.Join(reportView(rep, color256, 80), "\n")+"\n")
	checkGolden(t, "report-checks-ascii.golden", strings.Join(reportView(rep, plainASCII, 80), "\n")+"\n")

	wide := strings.Join(reportView(rep, plainUnicode, 100), "\n")
	for _, want := range []string{"what the agents did", "ran-go-test", "touched-tests", " of ", "(3 unread)", "counts of your rule checks, not a verdict · agentium check list"} {
		if !strings.Contains(wide, want) {
			t.Errorf("the view lacks %q:\n%s", want, wide)
		}
	}
	if i, j := strings.Index(wide, "every task"), strings.Index(wide, "what the agents did"); i < 0 || j < i {
		t.Error("the block is not after the task block")
	}
	narrow := strings.Join(reportView(rep, plainUnicode, term.MinWidth), "\n")
	if strings.ContainsAny(narrow[strings.Index(narrow, "what the agents did"):], "█▏▎▍▌▋▊▉░") {
		t.Errorf("under 70 columns the bars are left out:\n%s", narrow)
	}
	// No color says good or bad: only the versions' own colors and the muted words.
	colored := strings.Join(reportView(rep, color256, 100), "\n")
	block := colored[strings.Index(colored, "what the agents did"):]
	for _, bad := range []string{"\x1b[32m", "\x1b[31m", "\x1b[33m"} {
		if strings.Contains(block, bad) {
			t.Errorf("the block has a color of good or bad: %q", bad)
		}
	}
	if without := strings.Join(reportView(buildReport(t, reportScenes(t)["decisive"]), plainUnicode, 100), "\n"); strings.Contains(without, "what the agents did") {
		t.Error("a report without checks has the block")
	}
}

// A name from the owner is sanitized before it is drawn, and cut to fit.
func TestReportViewRuleCheckNames(t *testing.T) {
	rep := checksReport(t, "ok\x1b[31m-name", strings.Repeat("long-", 12))
	view := strings.Join(reportView(rep, plainUnicode, 80), "\n")
	if strings.Contains(view, "\x1b") {
		t.Errorf("an escape code reached the screen: %q", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if term.Width(line) > 80 {
			t.Errorf("a line of %d cells: %q", term.Width(line), line)
		}
	}
}
