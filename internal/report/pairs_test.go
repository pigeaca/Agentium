package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/report/reporttest"
)

// The pair judge's section in all three formats: the preference by task with its interval and test, the comparisons'
// counts, the cost, and each task's vote with a reason, scrubbed and on one line.
func TestReportJudgePairsGolden(t *testing.T) {
	rep, err := Build(inputOf(reporttest.JudgedPairs()))
	if err != nil {
		t.Fatal(err)
	}
	md, js, txt := renderAll(t, rep)
	golden(t, "lean-ab-pairs.md", []byte(md))
	goldenJSON(t, "lean-ab-pairs.json", []byte(js))
	golden(t, "lean-ab-pairs.txt", []byte(txt))
	for _, want := range []string{"## Judge pairs", "Judge prefers: B in ", "unvalidated and exploratory", "flip", "| Task | Prefers | Compared | Why (Change 1 is A's) |",
		`keeps the parser's contract \| Change 2 widens it; see <agentium data>/records/x`, "<agentium data>/records/x"} {
		if !strings.Contains(md, want) {
			t.Errorf("the Markdown lacks %q", want)
		}
	}
	for _, text := range []string{md, js, txt} {
		if strings.Contains(text, "sk-ant-api03-zz") || strings.Contains(text, "/home/someone") {
			t.Errorf("a reason's key or path is shown:\n%s", text)
		}
		// The reason is on one line, without its escape codes: a Markdown row stays one row, a terminal stays clean.
		if strings.Contains(text, "\x1b") || strings.Contains(text, `\u001b`) || strings.Contains(text, "[31m") || strings.Contains(text, "widens it;\n") || strings.Contains(text, `widens it;\n`) {
			t.Errorf("a reason's line break or escape code is shown:\n%q", text)
		}
	}
	for _, line := range strings.Split(md[strings.Index(md, "| Task | Prefers"):strings.Index(md, "## Notes")], "\n") {
		if line != "" && !strings.HasPrefix(line, "| ") && !strings.HasPrefix(line, "|---") {
			t.Errorf("the pair table is broken by %q", line)
		}
	}
	if !strings.Contains(txt, "\nJudge pairs\n") || !strings.Contains(txt, "Judge prefers: B in ") {
		t.Errorf("the terminal report (--details) lacks the pair judge:\n%s", txt)
	}
	p := rep.PairJudge
	if p == nil || p.Model != judge.DefaultModel || p.CostUSD <= 0 || len(p.PerTask) == 0 {
		t.Fatalf("pair judge %+v", p)
	}
	if !strings.Contains(js, `"pair_judge": {`) {
		t.Error("the JSON lacks pair_judge")
	}
}

// The clustered statistics are PairPreferenceOf's: a task's comparisons make one vote; below the floor of 5 the
// summary is not enough, and every format says so.
func TestReportJudgePairsTooFew(t *testing.T) {
	rep, err := Build(inputOf(reporttest.FewPairs()))
	if err != nil {
		t.Fatal(err)
	}
	p := rep.PairJudge
	if p == nil || p.Tasks.Enough || p.Tasks.B == 0 || p.Tasks.B >= judge.MinPreferences {
		t.Fatalf("pair judge %+v: want fewer than %d preferences", p, judge.MinPreferences)
	}
	md, _, txt := renderAll(t, rep)
	for _, text := range []string{md, txt} {
		if !strings.Contains(text, "Judge prefers: too few to say (") {
			t.Errorf("no floor:\n%s", text)
		}
	}
}

// With repeats, a task's pairs are one vote: lean-ab-pairs has 30 pairs, but its preference counts tasks.
func TestReportJudgePairsVotePerTask(t *testing.T) {
	rep, err := Build(inputOf(reporttest.JudgedPairs()))
	if err != nil {
		t.Fatal(err)
	}
	p := rep.PairJudge
	tasks := p.Tasks.A + p.Tasks.B + p.Tasks.Ties
	if tasks != p.Tasks.Complete || tasks > len(rep.Lock.Tasks) || p.Pairs.Complete <= tasks {
		t.Errorf("tasks %+v, pairs %+v: one vote per task, more comparisons than votes", p.Tasks, p.Pairs)
	}
	votes := map[string]int{}
	for _, row := range p.PerTask {
		votes[row.Prefer]++
	}
	if votes[judge.PreferA] != p.Tasks.A || votes[judge.PreferB] != p.Tasks.B || votes[judge.PreferTie] != p.Tasks.Ties {
		t.Errorf("per task %v, summary %+v", votes, p.Tasks)
	}
}

// Without the pair judge the field is absent: the JSON and Markdown keep their form (the other goldens pin them).
func TestReportWithoutJudgePairs(t *testing.T) {
	rep, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	md, js, txt := renderAll(t, rep)
	if rep.PairJudge != nil || strings.Contains(js, "pair_judge") || strings.Contains(md, "Judge pairs") || strings.Contains(txt, "Judge pairs") {
		t.Error("a report without the pair judge shows it")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(js), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["pair_judge"]; ok {
		t.Error("pair_judge is in the JSON")
	}
}
