package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
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
	rec := Record{ID: "r1", Outcome: claude.OutcomeOK, Passed: &passed, RecordsDir: records, Metrics: claude.Metrics{CostUSD: 0.3}}
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
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || !*rec.Passed || rec.Metrics.CostUSD != 0.3 || rec.JudgeCostUSD() != v.CostUSD {
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
	if rec.Outcome != claude.OutcomeOK || !*rec.Passed || rec.Metrics.CostUSD != 0.3 {
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
	env, spec, rec = judgeFixture(t, "exit 0\n")
	os.Remove(filepath.Join(rec.RecordsDir, "agent.diff"))
	env.Judge(context.Background(), spec, judge.Settings{}, &rec)
	if rec.Judge != nil || len(rec.Notes) != 1 || !strings.Contains(rec.Notes[0], "the run's diff") {
		t.Errorf("a run without its diff: verdict %+v, notes %v", rec.Judge, rec.Notes)
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
	if rec.Outcome != claude.OutcomeOK || !*rec.Passed {
		t.Errorf("the run changed: %+v", rec)
	}
}
