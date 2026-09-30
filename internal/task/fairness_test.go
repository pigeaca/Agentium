package task

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/source"
)

// fairnessRepo commits base then solution (files overlaid on it) and returns views of both.
func fairnessRepo(t *testing.T, base, solution map[string]string) (source.Source, source.Source) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	b := commit(t, repo, base, "base")
	s := commit(t, repo, solution, "solution")
	ctx := context.Background()
	bs, err := source.Commit(ctx, b, "-C", repo)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := source.Commit(ctx, s, "-C", repo)
	if err != nil {
		t.Fatal(err)
	}
	return bs, ss
}

func gapTexts(gaps []Gap) []string {
	var out []string
	for _, g := range gaps {
		out = append(out, g.Kind+":"+g.Text)
	}
	return out
}

func wantGaps(t *testing.T, gaps []Gap, want ...string) {
	t.Helper()
	got := gapTexts(gaps)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("gaps = %q, want %q", got, want)
	}
}

const errText = "only documents (Markdown, reStructuredText, AsciiDoc; not test data) can be included"

// The three unfair cases found in a real experiment: an exact error message, a new struct field and a note's text.
var (
	fairBase = map[string]string{
		"go.mod":          "module example.com/m\n\ngo 1.22\n",
		"ctx/ctx.go":      "package ctx\n\ntype Expect struct{ Files []string }\n\nfunc Note() string { return \"graded with the base version\" }\n\nfunc Include(p string) error { return nil }\n",
		"ctx/ctx_test.go": "package ctx\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) { _ = Expect{Files: nil} }\n",
	}
	fairSolution = map[string]string{
		"ctx/ctx.go": "package ctx\n\nimport \"errors\"\n\ntype Expect struct {\n\tFiles         []string\n\tSlashCommands []string\n}\n\n" +
			"func Note() string { return \"graded with the starting version\" }\n\n" +
			"func Include(p string) error { return errors.New(\"" + errText + "\") }\n",
		"ctx/ctx_test.go": "package ctx\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\n" +
			"func TestOld(t *testing.T) { _ = Expect{Files: nil} }\n\n" +
			"func TestErr(t *testing.T) {\n\tif err := Include(\"a.go\"); err == nil || !strings.Contains(err.Error(), \"" + errText + "\") {\n\t\tt.Fatal(err)\n\t}\n}\n\n" +
			"func TestField(t *testing.T) { _ = Expect{SlashCommands: []string{\"x\"}} }\n\n" +
			"func TestNote(t *testing.T) {\n\tif !strings.Contains(Note(), \"graded with the starting version\") {\n\t\tt.Fatal(Note())\n\t}\n}\n",
	}
)

func TestFairnessFlagsTheThreeUnfairCases(t *testing.T) {
	base, solution := fairnessRepo(t, fairBase, fairSolution)
	gaps, err := Fairness(base, solution, "Make Include reject non-documents and update the note.", []string{"ctx/ctx_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	wantGaps(t, gaps, "identifier:SlashCommands", "literal:graded with the starting version", "literal:"+errText)
	if gaps[0].File != "ctx/ctx_test.go" || !strings.Contains(gaps[0].String(), "SlashCommands") {
		t.Errorf("gap = %+v (%s)", gaps[0], gaps[0])
	}
}

func TestFairnessNothingWhenTheInstructionStatesThem(t *testing.T) {
	base, solution := fairnessRepo(t, fairBase, fairSolution)
	instruction := "Include must fail with the error text\n  \"" + errText + "\".\nNote returns \"graded with the starting version\".\n" +
		"Add a SlashCommands field to Expect."
	gaps, err := Fairness(base, solution, instruction, []string{"ctx/ctx_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	wantGaps(t, gaps)
}

func TestFairnessIgnoresBaseTextsTrivialLiteralsAndStdlibNames(t *testing.T) {
	base := map[string]string{
		"p/p.go": "package p\n\nfunc Greeting() string { return \"hello there, friend\" }\n",
	}
	solution := map[string]string{
		"p/p.go": "package p\n\nfunc Greeting() string { return \"hello there, friend\" }\n\nfunc Extra() {}\n",
		"p/p_test.go": "package p\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestG(t *testing.T) {\n" +
			"\tif !strings.Contains(Greeting(), \"hello there\") || Greeting() == \"12345678\" || Greeting() == \"ok\" {\n\t\tt.Fatal(\"x\", t.Name())\n\t}\n}\n",
	}
	b, s := fairnessRepo(t, base, solution)
	gaps, err := Fairness(b, s, "", []string{"p/p_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	// "hello there" occurs in the base; "12345678" has no letter; "ok" and "x" are short; Extra is declared but unused.
	wantGaps(t, gaps)
}

func TestFairnessOtherLanguagesCheckLiteralsOnly(t *testing.T) {
	base := map[string]string{
		"app.py":            "def note():\n    return 'graded with the base version'\n",
		"tests/test_app.py": "from app import note\n\ndef test_old():\n    assert note()\n",
	}
	solution := map[string]string{
		"app.py": "def note():\n    return 'graded with the starting version'\n\ndef brand_new_helper():\n    pass\n",
		"tests/test_app.py": "from app import note, brand_new_helper\n\ndef test_old():\n    assert note()\n\n" +
			"def test_note():\n    assert note() == 'graded with the starting version'\n    brand_new_helper()\n    assert \"a\" != \"b\"\n",
		"tests/data.json": "{\"message\": \"a long message nobody states\"}\n",
	}
	b, s := fairnessRepo(t, base, solution)
	gaps, err := Fairness(b, s, "", []string{"tests/test_app.py", "tests/data.json"})
	if err != nil {
		t.Fatal(err)
	}
	wantGaps(t, gaps, "literal:graded with the starting version") // no identifiers; data files are not scanned
}

func TestFairnessNoHiddenTestsNoGaps(t *testing.T) {
	b, s := fairnessRepo(t, fairBase, fairSolution)
	gaps, err := Fairness(b, s, "", nil)
	if err != nil || len(gaps) != 0 {
		t.Errorf("gaps = %v, %v", gaps, err)
	}
	if _, err := Fairness(b, s, "", []string{filepath.Join("ctx", "missing_test.go")}); err == nil {
		t.Error("a hidden test file absent from the solution should be an error")
	}
}
