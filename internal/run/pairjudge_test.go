package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/judge"
)

// pairFixture is judgeFixture's task with two graded runs, a (arm A) and b (arm B), each with its agent.diff, and
// Claude Code answering the pair schema with script.
func pairFixture(t *testing.T, script string) (Env, Spec, Record, Record) {
	t.Helper()
	env, spec, a := judgeFixture(t, script)
	b := a
	b.ID, b.Arm, b.RecordsDir = "r2", "B", filepath.Join(filepath.Dir(a.RecordsDir), "r2")
	if err := os.MkdirAll(b.RecordsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	diff := "diff --git a/value.txt b/value.txt\n--- a/value.txt\n+++ b/value.txt\n@@ -1 +1 @@\n-old\n+newer\n"
	if err := os.WriteFile(filepath.Join(b.RecordsDir, "agent.diff"), []byte(diff), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Arm = "A"
	return env, spec, a, b
}

// answers is a pair judge that keeps each prompt in dir and answers "second" when change 2 sets "newer" (arm B's), else
// "first": it prefers B in both orders.
func answers(dir string) string {
	return `prompt=$(cat); n=$(ls "` + dir + `" | wc -l | tr -d ' '); printf '%s' "$prompt" > "` + dir + `/p$n"; pwd > "` + dir + `/d$n"
answer=first; printf '%s' "$prompt" | sed -n '/<second>/,/<\/second>/p' | grep -q '+newer' && answer=second
echo '{"type":"result","is_error":false,"structured_output":{"prefer":"'$answer'","reason":"Sets newer (token tok-secret-1234567890)."},"total_cost_usd":0.09}'
`
}

// A comparison asks both orders with both runs' code and the reference, never the hidden tests, maps them back to the
// arms, keeps every call's cost as it lands, redacts its texts and removes its folder; the runs stay as they were.
func TestJudgePairComparesBothOrders(t *testing.T) {
	calls := t.TempDir()
	env, spec, a, b := pairFixture(t, answers(calls))
	var seen []PairJudgement
	c := env.JudgePair(context.Background(), spec, judge.Settings{}, a, b, func(p PairJudgement) { seen = append(seen, p) })
	v := c.Verdict
	if c.RunA != "r1" || v.Prefer != judge.PreferB || v.Flip || v.AB.Answer != "second" || v.BA.Answer != "first" || v.Stopped != "" ||
		v.Model != judge.DefaultModel || v.Effort != judge.DefaultEffort || v.Version != judge.PairVersion || v.CostUSD < 0.1799 || v.CostUSD > 0.1801 {
		t.Fatalf("comparison %+v", c)
	}
	if strings.Contains(v.AB.Reason, "tok-secret") || !strings.Contains(v.AB.Reason, "[REDACTED]") || strings.Contains(v.BA.Reason, "tok-secret") {
		t.Errorf("reasons not redacted: %q, %q", v.AB.Reason, v.BA.Reason)
	}
	if len(seen) != 2 || seen[0].Verdict.Stopped != judge.StoppedCall || seen[0].Verdict.CostUSD < 0.0899 || seen[0].Verdict.CostUSD > 0.0901 ||
		seen[1].Verdict.CostUSD < 0.1799 || seen[1].RunA != "r1" || !NeedsPairJudging(Record{PairJudge: &seen[1]}) {
		t.Errorf("spend as it went: %+v, want two stopped comparisons at $0.09 and $0.18", seen)
	}
	for i := range 2 {
		prompt, err := os.ReadFile(filepath.Join(calls, "p"+string(rune('0'+2*i))))
		if err != nil {
			t.Fatal(err)
		}
		text := string(prompt)
		if !strings.Contains(text, "+new\n") || !strings.Contains(text, "+newer\n") || strings.Contains(text, "value_test") ||
			!strings.Contains(text, "Make the value new.") {
			t.Errorf("prompt %d:\n%s", i, text)
		}
		dir, _ := os.ReadFile(filepath.Join(calls, "d"+string(rune('0'+2*i))))
		if !strings.Contains(string(dir), filepath.Join("r2", "pair-judge", "call-")) {
			t.Errorf("call %d ran in %s, not in arm B's records", i, dir)
		}
	}
	if _, err := os.Stat(PairJudgeDir(b)); err == nil {
		t.Error("the pair judge's folder was left behind")
	}
	if NeedsPairJudging(Record{PairJudge: &c}) || !NeedsPairJudging(Record{}) {
		t.Error("a comparison that ran its course is compared again, or a missing one is not")
	}

	// Compared again (a stopped comparison on resume): the earlier spend stays in the cost, and a folder left by the
	// comparison Agentium died in goes.
	if err := os.MkdirAll(filepath.Join(PairJudgeDir(b), "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	b.PairJudge = &PairJudgement{RunA: "r1", Verdict: judge.PairVerdict{Stopped: judge.StoppedLimit, CostUSD: 0.01}}
	again := env.JudgePair(context.Background(), spec, judge.Settings{}, a, b, nil)
	if v := again.Verdict; v.Stopped != "" || v.Prefer != judge.PreferB || v.CostUSD < 0.1899 || v.CostUSD > 0.1901 {
		t.Errorf("compared again: %+v, want $0.18 + the earlier $0.01", v)
	}
	if _, err := os.Stat(PairJudgeDir(b)); err == nil {
		t.Error("the pair judge's folder was left behind")
	}
}

// What can never be compared gets a final comparison without cost; what may pass is stopped, and compared again.
func TestJudgePairFailures(t *testing.T) {
	for name, change := range map[string]func(*Spec, *Record, *Record){
		"no diff in A":    func(_ *Spec, a, _ *Record) { os.Remove(filepath.Join(a.RecordsDir, "agent.diff")) },
		"bad reference":   func(s *Spec, _, _ *Record) { s.Task.Solution = strings.Repeat("0", 40) },
		"no code changed": func(s *Spec, _, _ *Record) { s.Task.Reference = []string{"tests/value_test.sh", "other.txt"} },
	} {
		env, spec, a, b := pairFixture(t, "exit 0\n")
		change(&spec, &a, &b)
		c := env.JudgePair(context.Background(), spec, judge.Settings{}, a, b, nil)
		if v := c.Verdict; v.Prefer != "" || v.Stopped != "" || len(v.Errors) != 1 || v.CostUSD != 0 || NeedsPairJudging(Record{PairJudge: &c}) ||
			!strings.HasPrefix(DescribePair(v), "no answer: ") {
			t.Errorf("%s: %+v", name, c)
		}
	}
	env, spec, a, b := pairFixture(t, "exit 0\n")
	env.CLI = filepath.Join(t.TempDir(), "missing")
	if c := env.JudgePair(context.Background(), spec, judge.Settings{}, a, b, nil); c.Verdict.Stopped != judge.StoppedCall || !NeedsPairJudging(Record{PairJudge: &c}) {
		t.Errorf("a judge that cannot start: %+v", c)
	}
	// An empty change is not asked about, and that is final.
	env, spec, a, b = pairFixture(t, "exit 1\n")
	if err := os.WriteFile(filepath.Join(b.RecordsDir, "agent.diff"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if c := env.JudgePair(context.Background(), spec, judge.Settings{}, a, b, nil); !c.Verdict.Empty || NeedsPairJudging(Record{PairJudge: &c}) {
		t.Errorf("an empty change: %+v", c)
	}
	// An interrupted comparison keeps what it spent, stopped, and is compared again.
	marker := filepath.Join(t.TempDir(), "calling")
	env, spec, a, b = pairFixture(t, "cat > /dev/null\ntouch "+marker+"\nsleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	b.PairJudge = &PairJudgement{Verdict: judge.PairVerdict{Stopped: judge.StoppedCall, CostUSD: 0.04}}
	c := env.JudgePair(ctx, spec, judge.Settings{}, a, b, nil)
	if v := c.Verdict; v.Stopped != judge.StoppedCall || v.CostUSD != 0.04 || !strings.HasPrefix(v.Errors[len(v.Errors)-1], "interrupted") {
		t.Errorf("an interrupted comparison: %+v", c)
	}
	if a.Outcome != claude.OutcomeOK || b.Metrics.CostUSD != 0.3 || b.Spend().AgentUSD != 0.3 {
		t.Errorf("the runs changed: %+v, %+v", a, b)
	}
}

func TestDescribePair(t *testing.T) {
	for _, c := range []struct {
		v    judge.PairVerdict
		want string
	}{
		{judge.PairVerdict{Prefer: judge.PreferB}, "prefers B"},
		{judge.PairVerdict{Prefer: judge.PreferTie}, "tie"},
		{judge.PairVerdict{Prefer: judge.PreferTie, Flip: true}, "tie (the two orders disagreed)"},
		{judge.PairVerdict{Empty: true}, "not asked (a run changed no code)"},
		{judge.PairVerdict{Stopped: judge.StoppedLimit}, "no answer; stopped at a usage limit or sign-in failure"},
		{judge.PairVerdict{Stopped: judge.StoppedCall, Errors: []string{"fork failed"}}, "no answer; stopped: fork failed"},
		{judge.PairVerdict{Errors: []string{"timed out"}}, "no answer: timed out"},
	} {
		if got := DescribePair(c.v); got != c.want {
			t.Errorf("DescribePair(%+v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// A comparison, a judgement or a grading attempt (Regrade) Agentium died in leaves its folder (with a config folder that
// may hold the sign-in) in a stored run's records: any command that takes the run lock removes it, and keeps the rest
// of the records.
func TestRecoverRemovesALeftoverPairJudgeFolder(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Records: filepath.Join(data, "records"), Workspaces: filepath.Join(data, "workspaces")}
	dir := filepath.Join(layout.Records, "r2")
	for p, body := range map[string]string{"agent.diff": "+x\n", "pair-judge/config/.claude.json": `{"token":"tok-secret-1234567890"}`,
		"judge/config/.claude.json": `{"token":"tok-secret-1234567890"}`} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orphans, err := Recover(context.Background(), layout, func(id string) (bool, error) { return id == "r2", nil }, "", time.Now())
	if err != nil || len(orphans) != 0 {
		t.Fatalf("Recover = %+v, %v", orphans, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pair-judge")); err == nil {
		t.Error("the pair judge's folder outlived the crash")
	}
	if _, err := os.Stat(filepath.Join(dir, "judge")); err == nil {
		t.Error("the judge's folder outlived the crash")
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.diff")); err != nil {
		t.Errorf("the stored run's records went: %v", err)
	}
}
