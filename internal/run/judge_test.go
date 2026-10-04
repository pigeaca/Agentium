package run

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/task"
)

func TestHasReferenceCodeAndNeedsJudging(t *testing.T) {
	code := task.Spec{Solution: "s", Reference: []string{"README.md", "tests/a_test.sh", "value.txt"}}
	for name, c := range map[string]struct {
		spec task.Spec
		want bool
	}{
		"code":        {code, true},
		"no solution": {task.Spec{Reference: []string{"value.txt"}}, false},
		"docs only":   {task.Spec{Solution: "s", Reference: []string{"README.md", "docs/guide.md"}}, false},
		"tests only":  {task.Spec{Solution: "s", Reference: []string{"tests/a_test.sh"}}, false},
	} {
		if got := HasReferenceCode(c.spec); got != c.want {
			t.Errorf("%s: HasReferenceCode = %v", name, got)
		}
	}
	passed := true
	for name, c := range map[string]struct {
		rec  Record
		spec task.Spec
		want bool
	}{
		"not judged":           {Record{Passed: &passed}, code, true},
		"stopped at a limit":   {Record{Passed: &passed, Judge: &judge.Verdict{Stopped: judge.StoppedLimit, Fixed: judge.Yes}}, code, true},
		"stopped, no call":     {Record{Passed: &passed, Judge: &judge.Verdict{Stopped: judge.StoppedCall}}, code, true},
		"judged":               {Record{Passed: &passed, Judge: &judge.Verdict{Fixed: judge.No}}, code, false},
		"judged, no answer":    {Record{Passed: &passed, Judge: &judge.Verdict{Errors: []string{"timed out"}}}, code, false},
		"no code in candidate": {Record{Passed: &passed, Judge: &judge.Verdict{Empty: true}}, code, false},
		"not graded":           {Record{}, code, false},
		"no reference code":    {Record{Passed: &passed}, task.Spec{Solution: "s", Reference: []string{"README.md"}}, false},
	} {
		if got := NeedsJudging(c.rec, c.spec); got != c.want {
			t.Errorf("%s: NeedsJudging = %v", name, got)
		}
	}
}

func TestDescribe(t *testing.T) {
	for _, c := range []struct {
		v    judge.Verdict
		want string
	}{
		{judge.Verdict{Fixed: judge.Yes, Answers: []string{"yes", "yes", "partly"}, Requested: 3}, "fixed (2 of 3)"},
		{judge.Verdict{Fixed: judge.Partly, Answers: []string{"yes", "no"}, Requested: 3}, "partly (no majority of 2)"},
		{judge.Verdict{Fixed: judge.No, Answers: []string{"no"}, Requested: 1}, "no (1 of 1)"},
		{judge.Verdict{Empty: true}, "not asked (the run changed no code)"},
		{judge.Verdict{Stopped: judge.StoppedLimit, Requested: 3}, "no answer; stopped at a usage limit or sign-in failure"},
		{judge.Verdict{Stopped: judge.StoppedCall, Errors: []string{"fork failed"}}, "no answer; stopped: fork failed"},
	} {
		if got := Describe(c.v); got != c.want {
			t.Errorf("Describe(%+v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// judgeFixture is a repository with a base and a solution commit (value.txt changed, a hidden test added), a graded
// run's records holding its agent.diff, and an Env whose Claude Code is script.
func judgeFixture(t *testing.T, script string) (Env, Spec, Record) {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = gitx.Environ(os.Environ())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(p, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("value.txt", "old\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	write("value.txt", "new\n")
	write("tests/value_test.sh", "grep -q new value.txt\n")
	git("add", "-A")
	git("commit", "-q", "-m", "solution")
	solution := git("rev-parse", "HEAD")

	data := t.TempDir()
	records := filepath.Join(data, "records", "r1")
	if err := os.MkdirAll(records, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(records, "agent.diff"), []byte("diff --git a/value.txt b/value.txt\n--- a/value.txt\n+++ b/value.txt\n@@ -1 +1 @@\n-old\n+new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	passed := true
	env := Env{Bare: filepath.Join(repo, ".git"), CLI: cli, Home: t.TempDir(), SignIn: claude.SignInTokenFile, Secret: "tok-secret-1234567890",
		Environ: []string{"PATH=" + os.Getenv("PATH")}}
	spec := Spec{TaskName: "value", Instruction: "Make the value new.", Task: task.Spec{Base: base, Solution: solution,
		HiddenTests: []string{"tests/value_test.sh"}, Reference: []string{"tests/value_test.sh", "value.txt"}, Verify: []string{"true"}}}
	rec := Record{ID: "r1", Outcome: agent.OutcomeOK, Passed: &passed, RecordsDir: records, Metrics: agent.Metrics{CostUSD: 0.3}}
	return env, spec, rec
}

// The judge's verdict is stored beside the run, which stays as it was; texts are redacted and its folder removed.
func TestJudgeStoresAVerdictAndLeavesTheRunAlone(t *testing.T) {
	answer := `{"type":"result","is_error":false,"result":"","structured_output":{"fixed":"partly","reason":"Only for one input (token ` +
		`$CLAUDE_CODE_OAUTH_TOKEN)."},"total_cost_usd":0.05}`
	env, spec, rec := judgeFixture(t, "cat > /dev/null\necho \""+strings.ReplaceAll(answer, `"`, `\"`)+"\"\n")
	env.Judge(context.Background(), spec, judge.Settings{Repeats: 2}, &rec)
	v := rec.Judge
	if v == nil || v.Fixed != judge.Partly || len(v.Answers) != 2 || v.Model != judge.DefaultModel || v.CostUSD < 0.0999 || v.CostUSD > 0.1001 {
		t.Fatalf("verdict %+v", v)
	}
	if strings.Contains(v.Reason, "tok-secret") || !strings.Contains(v.Reason, "[REDACTED]") {
		t.Errorf("the reason is not redacted: %q", v.Reason)
	}
	if rec.Outcome != agent.OutcomeOK || rec.Passed == nil || !*rec.Passed || rec.Metrics.CostUSD != 0.3 || rec.JudgeCostUSD() != v.CostUSD {
		t.Errorf("the run changed: %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(rec.RecordsDir, "judge")); err == nil {
		t.Error("the judge's folder was left behind")
	}

	// Judged again (a stopped verdict on resume): the earlier spend stays in the cost.
	rec.Judge = &judge.Verdict{Stopped: judge.StoppedLimit, CostUSD: 0.01}
	env.Judge(context.Background(), spec, judge.Settings{Repeats: 1}, &rec)
	if v := rec.Judge; v == nil || v.Stopped != "" || v.CostUSD < 0.0599 || v.CostUSD > 0.0601 {
		t.Errorf("judged again: %+v, want $0.05 + the earlier $0.01", v)
	}
}

// A judge that cannot run, or a task it cannot judge, leaves the run untouched: a verdict without an answer, or a
// note.
func TestJudgeFailuresNeverChangeTheRun(t *testing.T) {
	env, spec, rec := judgeFixture(t, "exit 0\n")
	env.CLI = filepath.Join(t.TempDir(), "missing")
	env.Judge(context.Background(), spec, judge.Settings{Repeats: 3}, &rec)
	if v := rec.Judge; v == nil || v.Fixed != "" || v.Stopped != judge.StoppedCall || len(v.Errors) != 1 {
		t.Errorf("a judge that cannot start: %+v", v)
	}
	if rec.Outcome != agent.OutcomeOK || !*rec.Passed || rec.Metrics.CostUSD != 0.3 {
		t.Errorf("the run changed: %+v", rec)
	}

	env, spec, rec = judgeFixture(t, "exit 0\n")
	spec.Task.Solution = ""
	env.Judge(context.Background(), spec, judge.Settings{}, &rec)
	if rec.Judge != nil || len(rec.Notes) != 1 || !strings.Contains(rec.Notes[0], "not judged: the task has no reference solution in code") {
		t.Errorf("a task without a solution: verdict %+v, notes %v", rec.Judge, rec.Notes)
	}
	env, spec, rec = judgeFixture(t, "exit 0\n")
	spec.Task.Reference = []string{"tests/value_test.sh", "README.md"}
	env.Judge(context.Background(), spec, judge.Settings{}, &rec)
	if rec.Judge != nil || len(rec.Notes) != 1 {
		t.Errorf("a reference without code: verdict %+v, notes %v", rec.Judge, rec.Notes)
	}
	// What can never be judged gets a final verdict without cost, so resumes leave it: a missing diff, a reference diff
	// that fails or changes no code. An earlier "not judged:" note goes once a verdict is stored.
	for name, change := range map[string]func(*Spec, *Record){
		"no diff":         func(_ *Spec, r *Record) { os.Remove(filepath.Join(r.RecordsDir, "agent.diff")) },
		"bad reference":   func(s *Spec, _ *Record) { s.Task.Solution = strings.Repeat("0", 40) },
		"no code changed": func(s *Spec, _ *Record) { s.Task.Reference = []string{"tests/value_test.sh", "other.txt"} },
	} {
		env, spec, rec = judgeFixture(t, "exit 0\n")
		rec.Notes = []string{"not judged: an earlier attempt", "kept"}
		change(&spec, &rec)
		env.Judge(context.Background(), spec, judge.Settings{}, &rec)
		v := rec.Judge
		if v == nil || v.Fixed != "" || v.Stopped != "" || len(v.Errors) != 1 || v.CostUSD != 0 || NeedsJudging(rec, spec.Task) ||
			len(rec.Notes) != 1 || rec.Notes[0] != "kept" || !strings.HasPrefix(Describe(*v), "no answer: ") {
			t.Errorf("%s: verdict %+v, notes %v", name, v, rec.Notes)
		}
	}
	env, spec, rec = judgeFixture(t, "exit 0\n")
	rec.Passed = nil
	env.Judge(context.Background(), spec, judge.Settings{}, &rec)
	if rec.Judge != nil || len(rec.Notes) != 0 {
		t.Errorf("an ungraded run was judged: %+v %v", rec.Judge, rec.Notes)
	}
}

// An interrupted judgement keeps what it spent, as a stopped verdict that a resume judges again.
func TestJudgeInterrupted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "calling")
	env, spec, rec := judgeFixture(t, "cat > /dev/null\ntouch "+marker+"\nsleep 30\n")
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
	env.Judge(ctx, spec, judge.Settings{Repeats: 2}, &rec)
	if v := rec.Judge; v == nil || v.Stopped != judge.StoppedCall || !NeedsJudging(rec, spec.Task) || !strings.HasPrefix(v.Errors[len(v.Errors)-1], "interrupted") {
		t.Errorf("an interrupted judgement: %+v", v)
	}
	if rec.Outcome != agent.OutcomeOK || !*rec.Passed {
		t.Errorf("the run changed: %+v", rec)
	}
}

// Ctrl-C during a judge call: the call is interrupted, Claude Code reports what it spent, and that spend is kept, in
// the verdict and through the spend hook that stores it in the run's start file as the call ends.
func TestJudgeInterruptedKeepsTheCallsSpend(t *testing.T) {
	dir := t.TempDir()
	marker, result := filepath.Join(dir, "calling"), filepath.Join(dir, "result.json")
	must(t, os.WriteFile(result, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"interrupted","total_cost_usd":0.07}`), 0o600))
	env, spec, rec := judgeFixture(t, "trap 'cat "+result+"; exit 130' INT\ncat > /dev/null\ntouch "+marker+"\nwhile :; do sleep 0.05; done\n")
	var seen []float64
	env.judgeSpent = func(usd float64) { seen = append(seen, usd) }
	rec.Judge = &judge.Verdict{Stopped: judge.StoppedLimit, CostUSD: 0.01} // judged before: its spend stays
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
	env.Judge(ctx, spec, judge.Settings{Repeats: 2}, &rec)
	v := rec.Judge
	if v == nil || v.Stopped != judge.StoppedCall || !NeedsJudging(rec, spec.Task) {
		t.Fatalf("an interrupted judgement: %+v", v)
	}
	if math.Abs(v.CostUSD-0.08) > 1e-9 {
		t.Errorf("the verdict's cost $%.4f, want $0.08 (the earlier $0.01 and the interrupted call's $0.07)", v.CostUSD)
	}
	if len(seen) != 1 || math.Abs(seen[0]-0.08) > 1e-9 {
		t.Errorf("spend hook: %v, want [0.08]", seen)
	}
}

// Each call's cost reaches the spend hook as it lands, added to an earlier verdict's: what Once keeps in the start file.
func TestJudgeReportsItsSpendAsItGoes(t *testing.T) {
	env, spec, rec := judgeFixture(t, "cat > /dev/null\necho '{\"type\":\"result\",\"is_error\":false,\"structured_output\":{\"fixed\":\"yes\",\"reason\":\"r\"},\"total_cost_usd\":0.05}'\n")
	var seen []float64
	env.judgeSpent = func(usd float64) { seen = append(seen, usd) }
	rec.Judge = &judge.Verdict{Stopped: judge.StoppedLimit, CostUSD: 0.01}
	env.Judge(context.Background(), spec, judge.Settings{Repeats: 3}, &rec)
	if len(seen) != 3 || seen[0] < 0.0599 || seen[0] > 0.0601 || seen[2] < 0.1599 || seen[2] > 0.1601 {
		t.Errorf("spend as it went: %v, want $0.06, $0.11, $0.16", seen)
	}
}

// A run graded and marked finished, whose Agentium died while judging, is stored as finished: its records are redacted
// and the judge's folder (with its config and sign-in) removed.
func TestRecoverAJudgementCutShort(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Database: filepath.Join(data, "agentium.db"), Artifacts: filepath.Join(data, "artifacts"),
		Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records"), Cache: filepath.Join(data, "cache")}
	dir := filepath.Join(layout.Records, "r1")
	for p, body := range map[string]string{
		"stream.jsonl":              `{"type":"result","result":"token tok-secret-1234567890"}` + "\n",
		"agent.diff":                "+tok-secret-1234567890\n",
		"judge/config/.claude.json": `{"token":"tok-secret-1234567890"}`,
		"judge/call-1/placeholder":  "",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	passed := true
	rec := Record{ID: "r1", Task: "fix", Arm: "A", Outcome: agent.OutcomeOK, Passed: &passed, RecordsDir: dir,
		Judge: &judge.Verdict{Stopped: judge.StoppedCall, CostUSD: 0.05, Errors: []string{"Agentium stopped while judging"}}}
	if err := (Env{}).writeStart(start{Record: rec, Workspace: filepath.Join(layout.Workspaces, "r1"), AgentStarted: true, Finished: true}); err != nil {
		t.Fatal(err)
	}
	orphans, err := Recover(context.Background(), layout, func(string) (bool, error) { return false, nil }, "tok-secret-1234567890", time.Now())
	if err != nil || len(orphans) != 1 {
		t.Fatalf("Recover = %+v, %v", orphans, err)
	}
	got := orphans[0].Record
	if got.Recovered != RecoveredFinished || got.Judge == nil || got.Judge.CostUSD != 0.05 || !NeedsJudging(got, task.Spec{Solution: "s", Reference: []string{"a.go"}}) {
		t.Errorf("recovered %+v, verdict %+v: the judge's spend so far, judged again on resume", got, got.Judge)
	}
	if _, err := os.Stat(filepath.Join(dir, "judge")); err == nil {
		t.Error("the judge's folder was left behind")
	}
	for _, name := range []string{"stream.jsonl", "agent.diff"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || strings.Contains(string(data), "tok-secret") || !strings.Contains(string(data), "[REDACTED]") {
			t.Errorf("%s not redacted: %s %v", name, data, err)
		}
	}
}
