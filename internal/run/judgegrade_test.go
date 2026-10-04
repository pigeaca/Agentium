package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/task"
)

// judgeGradeFixture is judgeFixture as a judge-graded task's run, graded by grade but not yet by the judge, whose Claude Code
// answers each call with the next line of answers: "yes", "partly" or "no" at $0.05, "limit" (a usage limit, $0.01) or
// "broken" (no JSON).
func judgeGradeFixture(t *testing.T, answers ...string) (Env, Spec, Record) {
	t.Helper()
	ctrl := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctrl, "answers"), []byte(strings.Join(answers, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, spec, rec := judgeFixture(t, `cat > /dev/null
CTRL='`+ctrl+`'
n=$(cat "$CTRL/n" 2>/dev/null || echo 0); echo $((n+1)) > "$CTRL/n"
a=$(sed -n "$((n+1))p" "$CTRL/answers")
case "$a" in
limit) echo '{"type":"result","subtype":"error","is_error":true,"result":"Claude AI usage limit reached","total_cost_usd":0.01}'; exit 1;;
broken) echo 'not json'; exit 1;;
*) echo '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"fixed":"'"$a"'","reason":"Answered '"$a"'."},"total_cost_usd":0.05}';;
esac
`)
	env.Now = time.Now
	spec.Task.Grading, spec.Task.HiddenTests, spec.Task.Reference = task.GradingJudge, nil, []string{"value.txt"}
	rec.Passed, rec.GradedBy = nil, task.GradingJudge
	return env, spec, rec
}

// persisted records what gradeByJudge persists, and stands in for Once's unfinished: the run cancelled.
type persisted struct{ records []Record }

func (p *persisted) persist(r Record) error { p.records = append(p.records, r); return nil }

func unfinishedStub(rec *Record) func(error) (Record, error) {
	return func(err error) (Record, error) {
		rec.Outcome, rec.Passed = claude.OutcomeCancelled, nil
		return *rec, err
	}
}

// The judge's majority of 5 grades a judge-graded run: "yes" passes, anything else fails; the verdict, its spend and
// its votes are kept, and the run is persisted as not graded before the first call and as each call's cost lands.
func TestGradeByJudge(t *testing.T) {
	for name, c := range map[string]struct {
		answers []string
		passed  bool
		cost    float64
		words   string
	}{
		"fixed":            {[]string{"yes", "yes", "yes", "partly", "yes"}, true, 0.25, "fixed (4 of 5)"},
		"not fixed":        {[]string{"no", "partly", "no", "yes", "no"}, false, 0.25, "not fixed (4 of 5 said partly or no)"},
		"decided, limited": {[]string{"yes", "yes", "yes", "limit"}, true, 0.16, "fixed (3 of 5)"},
		"a broken reply":   {[]string{"broken", "yes", "yes", "yes", "no"}, true, 0.20, "fixed (3 of 5)"},
	} {
		env, spec, rec := judgeGradeFixture(t, c.answers...)
		var p persisted
		got, err := env.gradeByJudge(context.Background(), spec, &rec, p.persist, unfinishedStub(&rec))
		v := got.Judge
		if err != nil || got.Outcome != claude.OutcomeOK || got.Passed == nil || *got.Passed != c.passed || v == nil || v.Requested != judge.GradeRepeats ||
			v.CostUSD < c.cost-1e-9 || v.CostUSD > c.cost+1e-9 || got.Spend().JudgeUSD != v.CostUSD || got.Metrics.CostUSD != 0.3 {
			t.Errorf("%s: %v, outcome %s, passed %v, verdict %+v", name, err, got.Outcome, got.Passed, v)
			continue
		}
		if words := GradeWords(got); words != c.words {
			t.Errorf("%s: GradeWords = %q, want %q", name, words, c.words)
		}
		first := p.records[0]
		if len(p.records) < 2 || first.Outcome != claude.OutcomeInfra || first.Passed != nil || first.Judge == nil || first.Judge.Stopped != judge.StoppedCall ||
			!strings.Contains(strings.Join(first.Notes, "; "), "Agentium stopped while the judge graded it") {
			t.Errorf("%s: persisted %+v", name, p.records)
		}
		if last := p.records[len(p.records)-1]; last.Judge.CostUSD <= 0 || last.Outcome != claude.OutcomeInfra {
			t.Errorf("%s: the spend as it landed: %+v", name, last.Judge)
		}
		if _, err := os.Stat(filepath.Join(rec.RecordsDir, "judge")); err == nil {
			t.Errorf("%s: the judge's folder was left behind", name)
		}
		if NeedsJudging(got, spec.Task) {
			t.Errorf("%s: a judge-graded run needs the second-opinion judge", name)
		}
	}
}

// A judge error, a refusal or a usage limit leaves a judge-graded run without a grade: infrastructure, never failed.
func TestGradeByJudgeErrorsNeverFailARun(t *testing.T) {
	for name, c := range map[string]struct {
		answers []string
		stopped string
		why     string
	}{
		"a usage limit":     {[]string{"limit"}, judge.StoppedLimit, "it stopped at a usage limit"},
		"limit after a tie": {[]string{"yes", "no", "limit"}, judge.StoppedLimit, "answered 2 of 5 times (1 fixed, 1 not)"},
		"every call broken": {[]string{"broken", "broken", "broken", "broken", "broken", "broken", "broken", "broken", "broken", "broken"}, "", "answered 0 of 5 times"},
	} {
		env, spec, rec := judgeGradeFixture(t, c.answers...)
		var p persisted
		got, err := env.gradeByJudge(context.Background(), spec, &rec, p.persist, unfinishedStub(&rec))
		notes := strings.Join(got.Notes, "; ")
		if err != nil || got.Outcome != claude.OutcomeInfra || got.Passed != nil || got.Judge == nil || got.Judge.Stopped != c.stopped ||
			!strings.Contains(notes, "not graded: "+"the judge") || !strings.Contains(notes, c.why) || !strings.Contains(notes, "infrastructure") {
			t.Errorf("%s: %v, outcome %s, passed %v, verdict %+v, notes %s", name, err, got.Outcome, got.Passed, got.Judge, notes)
		}
		if words := GradeWords(got); !strings.HasPrefix(words, "not graded (") || GradeVotes(got) != "" {
			t.Errorf("%s: GradeWords = %q", name, words)
		}
	}
	// A task without a reference in code cannot be graded: infrastructure, nothing spent.
	env, spec, rec := judgeGradeFixture(t, "yes")
	spec.Task.Reference = []string{"README.md"}
	var p persisted
	if got, err := env.gradeByJudge(context.Background(), spec, &rec, p.persist, unfinishedStub(&rec)); err != nil || got.Outcome != claude.OutcomeInfra ||
		got.Passed != nil || got.Spend().JudgeUSD != 0 {
		t.Errorf("no reference in code: %v, %+v", err, got)
	}
	// Interrupted before a majority: the run is cancelled (Once's unfinished), with what the judge spent.
	env, spec, rec = judgeGradeFixture(t, "yes")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := env.gradeByJudge(ctx, spec, &rec, p.persist, unfinishedStub(&rec))
	if !errors.Is(err, context.Canceled) || got.Outcome != claude.OutcomeCancelled || got.Passed != nil {
		t.Errorf("interrupted: %v, %+v", err, got)
	}
}

// A judge-graded run whose Agentium died while the judge graded it is recovered as infrastructure, with what the judge
// spent so far: an experiment tries it again, and counts the spend.
func TestRecoverAJudgeGradeCutShort(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Database: filepath.Join(data, "agentium.db"), Artifacts: filepath.Join(data, "artifacts"),
		Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records"), Cache: filepath.Join(data, "cache")}
	dir := filepath.Join(layout.Records, "r1")
	if err := os.MkdirAll(filepath.Join(dir, "judge", "call-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(`{"type":"result","result":"done"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, spec, rec := judgeGradeFixture(t)
	rec.ID, rec.RecordsDir = "r1", dir
	var last Record
	env.judgeSpent = nil
	persist := func(r Record) error {
		last = r
		return env.writeStart(start{Record: r, Workspace: filepath.Join(layout.Workspaces, "r1"), AgentStarted: true, Finished: true})
	}
	// The judge's calls fail to start: the grade stops, as Agentium dying would leave it after its first write.
	env.CLI = filepath.Join(t.TempDir(), "missing")
	if _, err := env.gradeByJudge(context.Background(), spec, &rec, persist, unfinishedStub(&rec)); err != nil {
		t.Fatal(err)
	}
	if err := persist(last); err != nil { // the start file as the first write left it
		t.Fatal(err)
	}
	orphans, err := Recover(context.Background(), layout, func(string) (bool, error) { return false, nil }, "", time.Now())
	if err != nil || len(orphans) != 1 {
		t.Fatalf("Recover = %+v, %v", orphans, err)
	}
	got := orphans[0].Record
	if got.Recovered != RecoveredFinished || got.Outcome != claude.OutcomeInfra || got.Passed != nil || got.GradedBy != task.GradingJudge || got.Judge == nil {
		t.Errorf("recovered %+v", got)
	}
}
