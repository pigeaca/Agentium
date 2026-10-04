package report

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/report/reporttest"
	"github.com/pigeaca/agentium/internal/term"
)

// judged is reporttest.Judged as a report's input.
func judged() Input { return inputOf(reporttest.Judged()) }

func renderAll(t *testing.T, rep Report) (md, js, txt string) {
	t.Helper()
	var m, j, x bytes.Buffer
	if err := rep.Markdown(&m); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&j); err != nil {
		t.Fatal(err)
	}
	if err := rep.Terminal(&x, term.Style{}); err != nil {
		t.Fatal(err)
	}
	return m.String(), j.String(), x.String()
}

func TestReportJudgeGolden(t *testing.T) {
	rep, err := Build(judged())
	if err != nil {
		t.Fatal(err)
	}
	md, js, txt := renderAll(t, rep)
	golden(t, "lean-ab-judged.md", []byte(md))
	goldenJSON(t, "lean-ab-judged.json", []byte(js))
	golden(t, "lean-ab-judged.txt", []byte(txt))
	assertTerminalColors(t, rep, txt)
	for _, want := range []string{"## Judge", "which decides nothing", "claude-opus-5-5 at effort high with 3 repeats per run", "Its accuracy is unmeasured",
		"| Arm | Tests | Judged | Fixed | Partly | No |", "Not judged: A ", "changed no code", "got no answer", "have no reference in code",
		"Repeat agreement: every answer was the same in ", "Judge cost: A $", "in the spend and not in the arms' costs",
		"Passing runs the judge did not call fixed (", "and 7 more (the JSON report lists them all)", "The repeats had no majority",
		"partly (no majority of 3)", "no (2 of 3)"} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown lacks %q", want)
		}
	}
	if !strings.Contains(txt, "and 7 more (agentium experiment report lean-ab-judged --json lists them all)") {
		t.Error("the terminal caps the list and points at the JSON")
	}
	for _, private := range []string{"/home/someone", "sk-ant-api03", "but\n"} {
		if strings.Contains(md, private) || strings.Contains(js, private) || strings.Contains(txt, private) {
			t.Errorf("the report shows %q", private)
		}
	}
	if !strings.Contains(md, "Handles the empty case but not a missing file; see <agentium data>/records/x/agent.diff with [REDACTED]") {
		t.Error("a reason is scrubbed and on one line")
	}
	// The verdicts of the tests stay as they were without the judge.
	plain, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	encode := func(v any) string {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if encode(plain.Analysis) != encode(rep.Analysis) || encode(plain.Arms) != encode(rep.Arms) || encode(plain.Tasks) != encode(rep.Tasks) {
		t.Error("the judge changed the analysis or the arms")
	}
	if rep.SpentUSD <= plain.SpentUSD || rep.SpentUSD-plain.SpentUSD-rep.Judge.CostUSD > 1e-9 {
		t.Errorf("spent %v, without the judge %v, judge %v: the spend counts the judge", rep.SpentUSD, plain.SpentUSD, rep.Judge.CostUSD)
	}
}

// Counts, shares, Wilson intervals and the flagged list, from the data.
func TestReportJudgeCounts(t *testing.T) {
	in := judged()
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	j := rep.Judge
	if j == nil || j.Model != judge.DefaultModel || j.Effort != judge.DefaultEffort || j.Repeats != 3 || len(j.Arms) != 2 {
		t.Fatalf("judge %+v", j)
	}
	type want struct{ fixed, partly, no int }
	got := map[string][2]want{}
	flagged, cost, agreeing, multi := 0, 0.0, 0, 0
	notJudged := map[string]NotJudged{}
	for _, r := range in.Runs {
		rec := r.Record
		cost += rec.JudgeCostUSD()
		if !experiment.Fair(rec.Outcome) {
			continue
		}
		nj := notJudged[rec.Arm]
		v := rec.Judge
		switch {
		case rec.Passed == nil:
			nj.NotGraded++
		case v == nil:
			nj.NoReference++
		case v.Empty:
			nj.Empty++
		case v.Fixed == "":
			nj.NoAnswer++
		default:
			success := experiment.Success(rec.Outcome, rec.Passed, rec.Behavior.ConfigChanged)
			side := 1
			if success {
				side = 0
			}
			c := got[rec.Arm]
			w := &c[side]
			switch v.Fixed {
			case judge.Yes:
				w.fixed++
			case judge.Partly:
				w.partly++
			default:
				w.no++
			}
			got[rec.Arm] = c
			if success && v.Fixed != judge.Yes {
				flagged++
			}
			multi++
			if allSame(v.Answers) {
				agreeing++
			}
		}
		notJudged[rec.Arm] = nj
	}
	for _, a := range j.Arms {
		for side, c := range []JudgeCounts{a.Passing, a.Failing} {
			w := got[a.Name][side]
			if c.Fixed.Count != w.fixed || c.Partly.Count != w.partly || c.No.Count != w.no || c.Judged != w.fixed+w.partly+w.no {
				t.Errorf("arm %s side %d: %+v, want %+v", a.Name, side, c, w)
			}
			for _, p := range []Proportion{c.Fixed, c.Partly, c.No} {
				if p.Of != c.Judged || (p.Of > 0 && (p.Share == nil || *p.Share < p.Low || *p.Share > p.High || p.Low < 0 || p.High > 1)) {
					t.Errorf("arm %s: proportion %+v", a.Name, p)
				}
			}
		}
		if a.NotJudged != notJudged[a.Name] {
			t.Errorf("arm %s not judged %+v, want %+v", a.Name, a.NotJudged, notJudged[a.Name])
		}
	}
	if len(j.Flagged) != flagged || flagged <= MaxFlagged || j.Pending != 0 {
		t.Errorf("flagged %d, want %d (more than %d); pending %d", len(j.Flagged), flagged, MaxFlagged, j.Pending)
	}
	if d := j.CostUSD - cost; d > 1e-9 || d < -1e-9 || j.Agreement.Count != agreeing || j.Agreement.Of != multi {
		t.Errorf("cost %v (want %v), agreement %+v (want %d of %d)", j.CostUSD, cost, j.Agreement, agreeing, multi)
	}
	// 7 of 10 has the textbook Wilson interval.
	if p := proportion(7, 10); p.Share == nil || *p.Share != 0.7 || p.Low < 0.396 || p.Low > 0.398 || p.High < 0.891 || p.High > 0.893 {
		t.Errorf("proportion(7, 10) = %+v", p)
	}
	if p := proportion(0, 0); p.Share != nil || p.Low != 0 || p.High != 1 {
		t.Errorf("proportion(0, 0) = %+v", p)
	}
}

// Without the judge the report has no Judge section and no judge fields: byte for byte what it was (the golden files).
func TestReportWithoutJudge(t *testing.T) {
	rep, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	md, js, txt := renderAll(t, rep)
	if rep.Judge != nil || strings.Contains(md, "Judge") || strings.Contains(txt, "Judge") || strings.Contains(js, `"judge"`) {
		t.Error("a report without the judge shows it")
	}
}

// When every slot settled and only judge verdicts are missing, the report says the verdicts are complete, not that
// the experiment is unfinished, and how to judge the rest.
func TestReportOnlyJudgeVerdictsMissing(t *testing.T) {
	for _, c := range []struct {
		status, note, want, command string
	}{
		{experiment.StatusStopped, "2 run(s) still need the judge", "lack a judge verdict: agentium experiment run lean-ab-judged judges them.",
			"agentium experiment run lean-ab-judged judges them."},
		{experiment.StatusBudget, "2 run(s) still need the judge, but the budget leaves no room for a judgement ($6.00)",
			"lack a judge verdict, which the budget leaves no room for: agentium experiment run lean-ab-judged --budget USD judges them.",
			"agentium experiment run lean-ab-judged --budget USD judges them."},
		{experiment.StatusUsage, "the judge hit a usage limit or a sign-in failure, which the next calls would hit too",
			"lack a judge verdict (the judge was paused at a usage limit): agentium experiment run lean-ab-judged judges them once the usage limit resets.",
			"agentium experiment run lean-ab-judged judges them once the usage limit resets."},
		// Another status note is kept, scrubbed.
		{experiment.StatusStopped, "Agentium could not store a judgement: open /home/someone/.agentium/agentium.db: locked",
			"lack a judge verdict (stopped: Agentium could not store a judgement: open <agentium data>/agentium.db: locked): agentium experiment run lean-ab-judged judges them.",
			"agentium experiment run lean-ab-judged judges them."},
	} {
		in := judged()
		in.Status, in.StatusNote = c.status, c.note
		stopped, never := false, false
		for i := range in.Runs {
			rec := &in.Runs[i].Record
			if rec.Judge == nil || rec.Judge.Fixed == "" {
				continue
			}
			switch {
			case !stopped:
				rec.Judge.Stopped, stopped = judge.StoppedLimit, true
			case !never:
				rec.Judge, never = nil, true
			}
		}
		rep, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		md, _, txt := renderAll(t, rep)
		if rep.Judge.Pending != 2 {
			t.Errorf("pending %d, want 2", rep.Judge.Pending)
		}
		for _, text := range []string{md, txt} {
			if strings.Contains(text, "not finished") || !strings.Contains(text, "Every run settled, so the success and cost verdicts are complete; 2 run(s) "+c.want) ||
				!strings.Contains(text, "1 stopped early") || !strings.Contains(text, "1 not judged yet") || !strings.Contains(text, "2 run(s) still need the judge: "+c.command) || strings.Contains(text, "/home/someone") ||
				strings.Count(text, "agentium experiment run lean-ab-judged") != 2 || strings.Count(text, c.command) != 2 {
				t.Errorf("%s:\n%s", c.status, text)
			}
		}
	}
	// Slots not settled: the experiment is unfinished, as before.
	in := judged()
	in.Status = experiment.StatusStopped
	in.Runs = in.Runs[:20]
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if note := rep.Notes[0]; !strings.HasPrefix(note, "The experiment is not finished (stopped)") {
		t.Errorf("first note %q", note)
	}
}

// The run rows carry the verdict, scrubbed.
func TestReportJudgeRunRows(t *testing.T) {
	rep, err := Build(judged())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rep.Runs {
		v := r.Judge
		if v == nil {
			continue
		}
		data, _ := json.Marshal(v)
		if strings.Contains(string(data), "/home/someone") || strings.Contains(string(data), "sk-ant") {
			t.Errorf("run %s's verdict is not scrubbed: %s", r.ID, data)
		}
		found = found || strings.Contains(v.Reason, "<agentium data>")
	}
	if !found {
		t.Error("no run row carries the scrubbed reason")
	}
	if rep.Runs[0].Outcome != claude.OutcomeOK {
		t.Errorf("run outcome %s", rep.Runs[0].Outcome)
	}
}

// Every free-text string of judge.Verdict reaches the shared report scrubbed: a new string field that shareVerdict
// leaves alone fails here. Enumerations (the answers, model, effort and why it stopped) are not free text.
func TestShareVerdictScrubsEveryText(t *testing.T) {
	enums := map[string]bool{"Fixed": true, "Answers": true, "Model": true, "Effort": true, "Stopped": true}
	const private = "/home/someone/.agentium/x \x1b[31mred"
	var v judge.Verdict
	rv := reflect.ValueOf(&v).Elem()
	for i := range rv.NumField() {
		f, field := rv.Field(i), rv.Type().Field(i)
		switch {
		case enums[field.Name]:
		case f.Kind() == reflect.String:
			f.SetString(private)
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
			f.Set(reflect.ValueOf([]string{private}))
		case f.Kind() == reflect.Bool || f.Kind() == reflect.Int || f.Kind() == reflect.Float64:
		default:
			t.Fatalf("Verdict.%s is a %s: decide how a report shares it", field.Name, f.Kind())
		}
	}
	in := Input{DataDir: "/home/someone/.agentium", Home: "/home/someone"}
	data, err := json.Marshal(in.shareVerdict(&v))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/home/someone") || strings.Contains(string(data), `\u001b`) {
		t.Errorf("a verdict's text is shared as it was: %s", data)
	}
	if !strings.Contains(string(data), `"answers":[]`) {
		t.Errorf("nil answers should be an empty list: %s", data)
	}
}

// Control characters in a model's reason never reach the terminal or Markdown, and a flagged run's detail is scrubbed.
func TestReportJudgeStripsControlCharacters(t *testing.T) {
	in := judged()
	for i := range in.Runs {
		v := in.Runs[i].Record.Judge
		if v != nil && v.Fixed == judge.No && in.Runs[i].Record.Passed != nil && *in.Runs[i].Record.Passed {
			v.Reason = "Skips \x1b[2Jthe\x07 check\r\nentirely.\x00"
		}
	}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	md, js, txt := renderAll(t, rep)
	for _, text := range []string{md, txt} {
		if strings.ContainsAny(text, "\x1b\x07\x00\r") || !strings.Contains(text, "Skips [2Jthe check entirely.") {
			t.Errorf("control characters reached the rendering:\n%q", text)
		}
	}
	if strings.Contains(js, `\u001b`) || strings.Contains(js, `\u0007`) {
		t.Error("control characters reached the JSON")
	}
	for _, f := range rep.Judge.Flagged {
		if strings.ContainsAny(f.Detail+f.Reason, "\x1b\x07\x00\r\n") {
			t.Errorf("flagged %+v", f)
		}
	}
}

// With no passing run judged, the report does not claim the judge called them all fixed.
func TestReportJudgeNoPassingRunJudged(t *testing.T) {
	in := judged()
	for i := range in.Runs {
		if rec := &in.Runs[i].Record; rec.Passed != nil && *rec.Passed {
			rec.Judge = nil
		}
	}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	md, _, _ := renderAll(t, rep)
	if !strings.Contains(md, "The judge has judged no passing run.") || strings.Contains(md, "called every passing run") {
		t.Errorf("no passing run judged:\n%s", md)
	}
}
