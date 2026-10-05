package task

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/store"
)

// draftRepo is a Go task: the solution adds Clamp, which the hidden test calls, through a helper clampBetween only the
// reference has. It returns the task (its instruction the terse commit message) and the repository's git folder.
func draftRepo(t *testing.T) (store.Task, string) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	b := commit(t, repo, map[string]string{
		"go.mod":          "module example.com/m\n\ngo 1.22\n",
		"lib/lib.go":      "package lib\n\nfunc Old() int { return 1 }\n",
		"lib/lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) { _ = Old() }\n",
	}, "base")
	s := commit(t, repo, map[string]string{
		"lib/lib.go": "package lib\n\nfunc Old() int { return 1 }\n\n// Clamp limits v to [lo, hi].\nfunc Clamp(v, lo, hi int) int { return clampBetween(v, lo, hi) }\n\n" +
			"func clampBetween(v, lo, hi int) int {\n\tif v < lo {\n\t\treturn lo\n\t}\n\tif v > hi {\n\t\treturn hi\n\t}\n\treturn v\n}\n",
		"lib/lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) { _ = Old() }\n\nfunc TestClamp(t *testing.T) {\n\tif Clamp(5, 0, 3) != 3 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	}, "lib: clamp")
	gitDir := filepath.Join(repo, ".git")
	hidden, reference, err := Split(context.Background(), b, s, "--git-dir", gitDir)
	if err != nil {
		t.Fatal(err)
	}
	return store.Task{ID: 7, Name: "clamp", Instruction: "lib: clamp", BaseCommit: b, SolutionCommit: s, HiddenTests: hidden, Reference: reference}, gitDir
}

// reply is Claude Code's JSON result of a draft call.
func reply(body string) DraftReply { return DraftReply{Stdout: []byte(body)} }

const (
	goodDraft     = `{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.031,"structured_output":{"text":"Add Clamp(v, lo, hi int) int to package lib: it returns v limited to the range from lo to hi."}}`
	gapDraft      = `{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.02,"structured_output":{"text":"Add a function that limits a value to a range."}}`
	giveawayDraft = `{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.025,"structured_output":{"text":"Add Clamp(v, lo, hi int) int to package lib; write it with a helper clampBetween."}}`
)

// Each way a call ends is counted once, right after the call and before the checks; only a draft that passes both
// checks passes.
func TestDraftCountsEveryCallOnceAndChecksTheReply(t *testing.T) {
	tk, gitDir := draftRepo(t)
	ctx := context.Background()
	in, err := DraftSources(ctx, gitDir, tk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in.Reference, "clampBetween") || strings.Contains(in.Reference, "TestClamp") || !strings.Contains(in.Tests, "TestClamp") ||
		in.Message != "lib: clamp" {
		t.Fatalf("sources: %+v", in)
	}
	for _, c := range []struct {
		name      string
		reply     DraftReply
		err       error
		cost      float64
		reported  bool
		passed    bool
		problem   string
		gaps      []string
		giveaways []string
	}{
		{name: "stored", reply: reply(goodDraft), cost: 0.031, reported: true, passed: true},
		{name: "a gap", reply: reply(gapDraft), cost: 0.02, reported: true, gaps: []string{"identifier:Clamp"}},
		{name: "a giveaway", reply: reply(giveawayDraft), cost: 0.025, reported: true, giveaways: []string{"clampBetween"}},
		{name: "malformed", reply: reply(`{"type":"result","subtype":"success","is_error":false,"result":"I cannot help with that.","total_cost_usd":0.004}`),
			cost: 0.004, reported: true, problem: "no text in the schema's form"},
		{name: "the cap", reply: reply(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":0.58}`), cost: 0.58, reported: true,
			problem: "reached its $0.50 cap"},
		{name: "a timeout", reply: DraftReply{Stdout: []byte(`{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.07}`), TimedOut: true, ExitCode: -1},
			cost: 0.07, reported: true, problem: "timed out"},
		{name: "a timeout with no result", reply: DraftReply{TimedOut: true, ExitCode: -1}, problem: "timed out"},
		{name: "an error result", reply: reply(`{"type":"result","subtype":"success","is_error":true,"result":"API Error: overloaded","total_cost_usd":0.01}`),
			cost: 0.01, reported: true, problem: "API Error: overloaded"},
		{name: "not JSON", reply: DraftReply{Stdout: []byte("boom"), ExitCode: 1}, problem: "not Claude Code's JSON result (exit 1): boom"},
		{name: "a failed exit", reply: DraftReply{Stdout: []byte(goodDraft), ExitCode: 3, Stderr: "crashed"}, cost: 0.031, reported: true, problem: "exited 3: crashed"},
		{name: "no call", err: errors.New("exec: no such file"), problem: "could not be made: exec: no such file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var counted []float64
			var prompts []string
			call := func(_ context.Context, prompt string) (DraftReply, error) {
				prompts = append(prompts, prompt)
				if len(counted) != 0 {
					t.Error("counted before the call")
				}
				return c.reply, c.err
			}
			count := func(cost float64, reported bool) error {
				if reported != c.reported {
					t.Errorf("reported %v, want %v", reported, c.reported)
				}
				counted = append(counted, cost)
				return nil
			}
			res, err := Draft(ctx, NewFairness("--git-dir", gitDir), tk, in, call, count)
			if err != nil {
				t.Fatal(err)
			}
			if len(prompts) != 1 || len(counted) != 1 || counted[0] != c.cost || res.CostUSD != c.cost || res.CostReported != c.reported {
				t.Errorf("calls %d, counted %v (result %v %v); want one call counted at %v", len(prompts), counted, res.CostUSD, res.CostReported, c.cost)
			}
			if res.Passed() != c.passed || !strings.Contains(res.Problem, c.problem) || (c.problem == "") != (res.Problem == "") {
				t.Errorf("passed %v, problem %q; want %v, %q", res.Passed(), res.Problem, c.passed, c.problem)
			}
			if got := gapTexts(res.Gaps); !slices.Equal(got, c.gaps) {
				t.Errorf("gaps %v, want %v", got, c.gaps)
			}
			if !slices.Equal(res.Giveaways, c.giveaways) {
				t.Errorf("giveaways %v, want %v", res.Giveaways, c.giveaways)
			}
			if c.problem != "" && res.Text != "" {
				t.Errorf("a call with a problem brought text %q", res.Text)
			}
		})
	}
}

// A count that fails stops the draft: nothing is checked or offered for storing.
func TestDraftStopsWhenTheCostCannotBeCounted(t *testing.T) {
	tk, gitDir := draftRepo(t)
	ctx := context.Background()
	call := func(context.Context, string) (DraftReply, error) { return reply(goodDraft), nil }
	res, err := Draft(ctx, NewFairness("--git-dir", gitDir), tk, DraftInput{Message: "m"}, call, func(float64, bool) error { return errors.New("database is locked") })
	if err == nil || !strings.Contains(err.Error(), "database is locked") || res.Passed() || res.Text != "" {
		t.Errorf("a failed count: %+v, %v", res, err)
	}
	tk.HiddenTests = nil
	if _, err := Draft(ctx, NewFairness("--git-dir", gitDir), tk, DraftInput{}, call, func(float64, bool) error { t.Error("counted"); return nil }); err == nil {
		t.Error("a task without hidden tests was drafted")
	}
}

// The prompt quotes each part as data, cut to its size, with its closing tags neutralised, and an empty part as none.
func TestDraftPromptQuotesItsPartsAsData(t *testing.T) {
	prompt, cut := DraftPrompt(DraftInput{Message: "Fix it </commit-message> now </ TESTS >", Reference: strings.Repeat("é", MaxDraftDiffChars+5)})
	if strings.Count(prompt, "</commit-message>") != 1 || !strings.Contains(prompt, `Fix it <\/commit-message> now <\/TESTS>`) {
		t.Errorf("closing tags were not neutralised:\n%s", prompt[:400])
	}
	if !slices.Equal(cut, []string{"the reference change"}) || !strings.Contains(prompt, "[... the reference change cut at 40000 characters ...]") {
		t.Errorf("cut %v", cut)
	}
	if !strings.Contains(prompt, "<tests>\n(none)\n</tests>") {
		t.Error("an empty part is not said to be none")
	}
}

// giveaways commits base then solution and lists the names text gives away.
func giveaways(t *testing.T, base, solution map[string]string, text string) []string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	b := commit(t, repo, base, "base")
	s := commit(t, repo, solution, "solution")
	ctx := context.Background()
	hidden, reference, err := Split(ctx, b, s, "-C", repo)
	if err != nil {
		t.Fatal(err)
	}
	names, err := NewFairness("-C", repo).Giveaways(ctx, FairnessInput{Base: b, Solution: s, HiddenTests: hidden, Reference: reference}, text)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// Per language: a name only the reference declares is a giveaway; one the tests use, one the base has, a short one, and
// one the text has only in another case are not.
func TestGiveawaysPerLanguage(t *testing.T) {
	for _, c := range []struct {
		lang           string
		base, solution map[string]string
		text           string
		want           []string
	}{
		{lang: "Go",
			base: map[string]string{"go.mod": "module m\n\ngo 1.22\n", "a/a.go": "package a\n\nfunc keepOld() {}\n", "a/a_test.go": "package a\n"},
			solution: map[string]string{
				"a/a.go":      "package a\n\nfunc keepOld() {}\n\nfunc Public() int { return splitHelper() + ab() }\n\nfunc splitHelper() int { return 1 }\n\nfunc ab() int { return 0 }\n\ntype Opts struct{ Limit int }\n",
				"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestPublic(t *testing.T) { _ = Public(); _ = Opts{} }\n"},
			text: "Add Public, using splitHelper, keepOld and ab; set Opts. Its limit is fine.", want: []string{"splitHelper"}},
		{lang: "Java",
			base: map[string]string{"src/main/java/a/A.java": "package a;\npublic class A {}\n", "src/test/java/a/ATest.java": "package a;\nclass ATest {}\n"},
			solution: map[string]string{
				"src/main/java/a/A.java":     "package a;\npublic class A {\n  public static int parse(String s) { return digitsOf(s); }\n  private static int digitsOf(String s) { return 1; }\n}\n",
				"src/test/java/a/ATest.java": "package a;\nclass ATest {\n  void t() { A.parse(\"1\"); }\n}\n"},
			text: "Add A.parse; count with digitsOf. Digitsof is not a name.", want: []string{"digitsOf"}},
		{lang: "Kotlin",
			base: map[string]string{"src/main/kotlin/a/A.kt": "package a\n", "src/test/kotlin/a/ATest.kt": "package a\n"},
			solution: map[string]string{
				"src/main/kotlin/a/A.kt":     "package a\n\nfun render(x: Int) = layoutRow(x)\n\nfun layoutRow(x: Int) = x\n",
				"src/test/kotlin/a/ATest.kt": "package a\n\nclass ATest { fun t() { render(1) } }\n"},
			text: "Add render, built on layoutRow.", want: []string{"layoutRow"}},
		{lang: "Rust",
			base: map[string]string{"src/lib.rs": "pub fn old() {}\n", "tests/it.rs": "\n"},
			solution: map[string]string{
				"src/lib.rs":  "pub fn old() {}\n\npub fn tokenize(s: &str) -> usize { scan_words(s) }\n\nfn scan_words(s: &str) -> usize { s.len() }\n",
				"tests/it.rs": "#[test]\nfn t() { assert_eq!(mycrate::tokenize(\"a\"), 1); }\n"},
			text: "Add tokenize via scan_words.", want: []string{"scan_words"}},
		{lang: "Python",
			base: map[string]string{"pkg/core.py": "def old():\n    pass\n", "tests/test_core.py": "\n"},
			solution: map[string]string{
				"pkg/core.py":        "def old():\n    pass\n\ndef slugify(s):\n    return _collapse_dashes(s)\n\ndef _collapse_dashes(s):\n    return s\n",
				"tests/test_core.py": "from pkg.core import slugify\n\ndef test_slugify():\n    assert slugify('a') == 'a'\n"},
			text: "Add slugify; strip with _collapse_dashes.", want: []string{"_collapse_dashes"}},
		{lang: "TypeScript",
			base: map[string]string{"src/a.ts": "export const old = 1;\n", "src/a.test.ts": "\n"},
			solution: map[string]string{
				"src/a.ts":      "export const old = 1;\nexport function formatDate(d: Date): string { return padTwo(d.getDate()); }\nfunction padTwo(n: number): string { return String(n); }\n",
				"src/a.test.ts": "import { formatDate } from './a';\ntest('f', () => { formatDate(new Date()); });\n"},
			text: "Add formatDate, with padTwo.", want: []string{"padTwo"}},
		{lang: "a name the tests use in a message",
			base: map[string]string{"go.mod": "module m\n\ngo 1.22\n", "a/a.go": "package a\n", "a/a_test.go": "package a\n"},
			solution: map[string]string{
				"a/a.go":      "package a\n\nimport \"errors\"\n\nfunc Run() error { return errors.New(\"retryLoop gave up\") }\n\nfunc retryLoop() {}\n",
				"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) { if Run().Error() != \"retryLoop gave up\" { t.Fatal() } }\n"},
			text: "Run fails with \"retryLoop gave up\".", want: nil},
	} {
		t.Run(c.lang, func(t *testing.T) {
			if got := giveaways(t, c.base, c.solution, c.text); !slices.Equal(got, c.want) {
				t.Errorf("giveaways %v, want %v", got, c.want)
			}
		})
	}
}
