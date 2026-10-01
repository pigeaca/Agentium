package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
)

const codeDiff = `diff --git a/report/scrub.go b/report/scrub.go
--- a/report/scrub.go
+++ b/report/scrub.go
@@ -1 +1 @@
-old
+new
`

const mixedDiff = "noise before the first file\n" + codeDiff + `diff --git a/report/scrub_test.go b/report/scrub_test.go
--- a/report/scrub_test.go
+++ b/report/scrub_test.go
@@ -0,0 +1 @@
+hidden test
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-x
+y
diff --git a/tests/fixture.txt b/tests/fixture.txt
--- a/tests/fixture.txt
+++ b/tests/fixture.txt
@@ -0,0 +1 @@
+fixture
`

// The judge's words are the pilot's, so the pilot's noise and cost figures apply: they are read from the pilot's script.
func TestPromptsAreThePilots(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "docs", "research", "judge-pilot", "judge_pilot.py"))
	if err != nil {
		t.Fatal(err)
	}
	py := string(src)
	system := regexp.MustCompile(`(?s)SYSTEM_PROMPT = \((.*?)\n\)`).FindStringSubmatch(py)
	if system == nil {
		t.Fatal("SYSTEM_PROMPT not found in the pilot")
	}
	var joined strings.Builder
	for _, m := range regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(system[1], -1) {
		joined.WriteString(m[1])
	}
	if joined.String() != SystemPrompt {
		t.Errorf("system prompt differs from the pilot's:\n%q\n%q", SystemPrompt, joined.String())
	}
	single := regexp.MustCompile(`(?s)SINGLE_PROMPT = """(.*?)"""`).FindStringSubmatch(py)
	if single == nil {
		t.Fatal("SINGLE_PROMPT not found in the pilot")
	}
	want := strings.NewReplacer("{instruction}", "%s", "{reference}", "%s", "{candidate}", "%s").Replace(single[1])
	if want != promptTemplate {
		t.Errorf("prompt differs from the pilot's:\n%s\n---\n%s", promptTemplate, want)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(Schema), &schema); err != nil {
		t.Fatal(err)
	}
	wantSchema := map[string]any{"type": "object", "properties": map[string]any{"fixed": map[string]any{"type": "string",
		"enum": []any{"yes", "partly", "no"}}, "reason": map[string]any{"type": "string"}}, "required": []any{"fixed", "reason"},
		"additionalProperties": false}
	if !reflect.DeepEqual(schema, wantSchema) {
		t.Errorf("schema = %v", schema)
	}
}

func TestCodeOnlyDropsTestsAndDocuments(t *testing.T) {
	if got := CodeOnly(mixedDiff); got != codeDiff {
		t.Errorf("CodeOnly =\n%s", got)
	}
	if got := CodeOnly("diff --git a/a_test.go b/a_test.go\n+x\n"); got != "" {
		t.Errorf("tests only = %q", got)
	}
}

func TestPromptCutsLongDiffsByCharacter(t *testing.T) {
	long := "diff --git a/a.go b/a.go\n+" + strings.Repeat("é", MaxDiffChars) + "\n"
	text, cut := Prompt(Input{Instruction: "do it", Reference: codeDiff, Candidate: long})
	if !cut || !strings.Contains(text, fmt.Sprintf("[... diff cut at %d characters ...]", MaxDiffChars)) {
		t.Fatalf("not cut: %v", cut)
	}
	if !strings.Contains(text, "<instruction>\ndo it\n</instruction>") || !strings.Contains(text, "<reference>\n"+codeDiff+"\n</reference>") {
		t.Errorf("prompt:\n%s", text)
	}
	if _, cut := Prompt(Input{Reference: codeDiff, Candidate: codeDiff}); cut {
		t.Error("a short diff was cut")
	}
}

func TestMajority(t *testing.T) {
	for _, c := range []struct {
		answers []string
		want    string
	}{
		{[]string{"yes", "partly", "yes"}, "yes"},
		{[]string{"yes", "partly", "no"}, "partly"}, // no majority
		{[]string{"no", "yes"}, "partly"},           // a tie between two answers is no majority either
		{[]string{"no"}, "no"},
		{nil, ""},
	} {
		if got := Majority(c.answers); got != c.want {
			t.Errorf("Majority(%v) = %q, want %q", c.answers, got, c.want)
		}
	}
}

func result(fixed, reason string, cost float64) Reply {
	out, _ := json.Marshal(map[string]any{"structured_output": map[string]string{"fixed": fixed, "reason": reason}, "total_cost_usd": cost, "result": ""})
	return Reply{Stdout: out}
}

func TestParse(t *testing.T) {
	cases := []struct {
		name  string
		reply Reply
		want  answer
	}{
		{"structured", result("partly", "misses /private/var", 0.04), answer{fixed: "partly", reason: "misses /private/var", cost: 0.04}},
		{"JSON in the text", Reply{Stdout: []byte(`{"result": "Here: {\"fixed\": \"no\", \"reason\": \"r\"}", "total_cost_usd": 0.03}`)},
			answer{fixed: "no", reason: "r", cost: 0.03}},
		{"error result", Reply{Stdout: []byte(`{"is_error": true, "result": "You've hit your session limit", "total_cost_usd": 0}`), ExitCode: 1},
			answer{err: "error result: You've hit your session limit", kind: kindInfra}},
		{"not JSON", Reply{Stdout: []byte("API Error: 400"), ExitCode: 1}, answer{err: "exit 1, not JSON: API Error: 400", kind: kindInfra}},
		{"nothing on stdout", Reply{Stderr: "claude: not signed in", ExitCode: 1}, answer{err: "exit 1, not JSON: claude: not signed in", kind: kindInfra}},
		{"no verdict", Reply{Stdout: []byte(`{"result": "I cannot say", "total_cost_usd": 0.02}`)},
			answer{cost: 0.02, err: "no valid verdict in: I cannot say", kind: kindMalformed}},
		{"a verdict outside the enum", result("maybe", "", 0.02), answer{cost: 0.02, err: "no valid verdict in: ", kind: kindMalformed}},
		{"verdict but a failed exit", Reply{Stdout: result("yes", "r", 0.05).Stdout, ExitCode: 2, Stderr: "boom"},
			answer{cost: 0.05, err: "exit 2: boom", kind: kindInfra}},
		{"timeout", Reply{TimedOut: true}, answer{err: "timed out after 10m0s (its cost is unknown)", kind: kindInfra}},
	}
	for _, c := range cases {
		if got := parse(c.reply); got != c.want {
			t.Errorf("%s: parse = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// fake returns replies in order and counts the calls.
func fake(replies ...Reply) (Caller, *int) {
	calls := 0
	return func(ctx context.Context, prompt string) (Reply, error) {
		if !strings.Contains(prompt, "<candidate>") {
			return Reply{}, errors.New("not the judge's prompt")
		}
		calls++
		if calls > len(replies) {
			return Reply{}, errors.New("too many calls")
		}
		return replies[calls-1], nil
	}, &calls
}

var (
	malformed = Reply{Stdout: []byte(`{"result": "hmm", "total_cost_usd": 0.01}`)}
	limit     = Reply{Stdout: []byte(`{"is_error": true, "result": "You've hit your session limit", "total_cost_usd": 0}`), ExitCode: 1}
)

func TestJudge(t *testing.T) {
	in := Input{Instruction: "do it", Reference: codeDiff, Candidate: mixedDiff}
	s := Settings{Model: DefaultModel, Effort: DefaultEffort, Repeats: 3}
	cases := []struct {
		name    string
		replies []Reply
		calls   int
		fixed   string
		answers []string
		reason  string
		errors  int
		cost    float64
	}{
		{"majority", []Reply{result("partly", "first", 0.04), result("yes", "second", 0.05), result("yes", "third", 0.06)},
			3, "yes", []string{"partly", "yes", "yes"}, "second", 0, 0.15},
		{"a malformed reply is asked once more", []Reply{malformed, result("no", "r1", 0.04), result("no", "r2", 0.04), result("yes", "r3", 0.04)},
			4, "no", []string{"no", "no", "yes"}, "r1", 1, 0.13},
		{"malformed twice leaves that repeat out", []Reply{malformed, malformed, result("yes", "r", 0.04), result("yes", "r", 0.04)},
			4, "yes", []string{"yes", "yes"}, "r", 2, 0.10},
		{"no answer ends the judgement", []Reply{result("partly", "only", 0.04), limit},
			2, "partly", []string{"partly"}, "only", 1, 0.04},
		{"no repeat answered", []Reply{limit}, 1, "", []string{}, "", 1, 0},
		{"no majority is partly, with its partly answer's reason", []Reply{result("yes", "a", 0), result("no", "b", 0), result("partly", "c", 0)},
			3, "partly", []string{"yes", "no", "partly"}, "c", 0, 0},
	}
	for _, c := range cases {
		call, calls := fake(c.replies...)
		v, err := Judge(context.Background(), in, s, call)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if *calls != c.calls || v.Fixed != c.fixed || !reflect.DeepEqual(v.Answers, c.answers) || v.Reason != c.reason ||
			len(v.Errors) != c.errors || fmt.Sprintf("%.2f", v.CostUSD) != fmt.Sprintf("%.2f", c.cost) {
			t.Errorf("%s: %d call(s), verdict %+v", c.name, *calls, v)
		}
		if v.Model != DefaultModel || v.Effort != DefaultEffort || v.Empty || v.Truncated {
			t.Errorf("%s: settings %+v", c.name, v)
		}
	}
}

func TestJudgeSkipsChangesWithoutCode(t *testing.T) {
	call, calls := fake()
	v, err := Judge(context.Background(), Input{Reference: codeDiff, Candidate: "diff --git a/README.md b/README.md\n+x\n"}, Settings{}, call)
	if err != nil || !v.Empty || *calls != 0 || v.Fixed != "" {
		t.Errorf("verdict %+v, %d call(s), %v", v, *calls, err)
	}
}

func TestJudgeStopsWhenACallCannotRunOrIsCancelled(t *testing.T) {
	in := Input{Reference: codeDiff, Candidate: codeDiff}
	v, err := Judge(context.Background(), in, Settings{Repeats: 3}, func(context.Context, string) (Reply, error) {
		return Reply{}, errors.New("exec: claude: not found")
	})
	if err != nil || v.Fixed != "" || !reflect.DeepEqual(v.Errors, []string{"exec: claude: not found"}) {
		t.Errorf("verdict %+v, %v", v, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = Judge(ctx, in, Settings{Repeats: 3}, func(context.Context, string) (Reply, error) {
		cancel()
		return Reply{}, context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// TestClaudeCaller runs a fake Claude Code: the prompt arrives on stdin, in an empty folder of its own (removed
// afterwards), with no tools, the pilot's system prompt and schema, a budget cap and no credentials.
func TestClaudeCaller(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	base := filepath.Join(root, "judge")
	for _, d := range []string{home, base} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(root, "claude")
	script := `#!/bin/sh
pwd > "$HOME/pwd"
ls -A > "$HOME/ls"
env > "$HOME/env"
cat > "$HOME/stdin"
for a in "$@"; do printf '%s\n' "[$a]"; done > "$HOME/args"
echo '{"structured_output": {"fixed": "partly", "reason": "misses a case"}, "total_cost_usd": 0.05, "result": ""}'
`
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	environ := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GITHUB_TOKEN=secret", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1"}
	call := ClaudeCaller(claude.Judgement{CLI: cli, Dir: base, Model: DefaultModel, Effort: DefaultEffort, SignIn: claude.SignInLogin, Home: home}, environ)
	v, err := Judge(context.Background(), Input{Instruction: "Fix the scrub.", Reference: codeDiff, Candidate: codeDiff}, Settings{Model: DefaultModel, Repeats: 1}, call)
	if err != nil || v.Fixed != Partly || v.Reason != "misses a case" || v.CostUSD != 0.05 {
		t.Fatalf("verdict %+v, %v", v, err)
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if dir := strings.TrimSpace(read("pwd")); mustEval(t, filepath.Dir(dir)) != mustEval(t, base) || !strings.HasPrefix(filepath.Base(dir), "call-") {
		t.Errorf("ran in %s", dir)
	}
	if ls := read("ls"); ls != "" {
		t.Errorf("its folder held %q", ls)
	}
	if left, _ := os.ReadDir(base); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if stdin := read("stdin"); !strings.Contains(stdin, "<instruction>\nFix the scrub.\n</instruction>") {
		t.Errorf("stdin = %q", stdin)
	}
	args := read("args")
	for _, want := range []string{"[--tools]\n[]\n", "[--system-prompt]\n[" + SystemPrompt + "]\n", "[--json-schema]\n[" + Schema + "]\n",
		"[--output-format]\n[json]\n", "[--max-budget-usd]\n[1]\n", "[--strict-mcp-config]", "[--model]\n[claude-opus-5-5]\n", "[--effort]\n[high]\n"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	env := read("env")
	if strings.Contains(env, "GITHUB_TOKEN") || strings.Contains(env, "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB") || !strings.Contains(env, "ENABLE_CLAUDEAI_MCP_SERVERS=false") {
		t.Errorf("env:\n%s", env)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestReferenceDiffIsCodeOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("a.go", "package a\n")
	write("README.md", "old\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	write("a.go", "package a\n\nfunc A() {}\n")
	write("a_test.go", "package a\n")
	write("README.md", "new\n")
	write("my file.go", "package a\n")
	git("add", "-A")
	git("commit", "-q", "-m", "solution")
	solution := git("rev-parse", "HEAD")
	diff, err := ReferenceDiff(context.Background(), filepath.Join(repo, ".git"), base, solution, []string{"a.go", "a_test.go", "README.md", "my file.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "diff --git a/a.go b/a.go") || !strings.Contains(diff, "b/my file.go") || strings.Contains(diff, "a_test.go") || strings.Contains(diff, "README") {
		t.Errorf("diff:\n%s", diff)
	}
	if CodeOnly(diff) != diff {
		t.Error("CodeOnly changed a code-only diff")
	}
	if _, err := ReferenceDiff(context.Background(), filepath.Join(repo, ".git"), base, solution, []string{"README.md"}); err == nil {
		t.Error("a documents-only reference was accepted")
	}
}
