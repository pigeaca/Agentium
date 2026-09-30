package task

import (
	"context"
	"strings"
	"testing"
)

// fairnessGaps commits base then solution (files overlaid on it) and returns the gaps for the instruction.
func fairnessGaps(t *testing.T, base, solution map[string]string, instruction string) []Gap {
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
	gaps, err := NewFairness("-C", repo).Gaps(ctx, FairnessInput{Base: b, Solution: s, Instruction: instruction, HiddenTests: hidden, Reference: reference})
	if err != nil {
		t.Fatal(err)
	}
	return gaps
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
	gaps := fairnessGaps(t, fairBase, fairSolution, "Make Include reject non-documents and update the note.")
	wantGaps(t, gaps, "identifier:SlashCommands", "literal:graded with the starting version", "literal:"+errText)
	if gaps[0].File != "ctx/ctx_test.go" || !strings.Contains(gaps[0].String(), "SlashCommands") {
		t.Errorf("gap = %+v (%s)", gaps[0], gaps[0])
	}
}

func TestFairnessNothingWhenTheInstructionStatesThem(t *testing.T) {
	instruction := "Include must fail with the error text\n  \"" + errText + "\".\nNote returns \"graded with the starting version\".\n" +
		"Add a SlashCommands field to Expect."
	wantGaps(t, fairnessGaps(t, fairBase, fairSolution, instruction))
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
	// "hello there" occurs in the base; "12345678" has no letter; "ok" and "x" are short; Extra is declared but unused.
	wantGaps(t, fairnessGaps(t, base, solution, ""))
}

// A realistic fair task: table-driven tests with format strings, subtest names and inputs that the implementation
// never produces are not requirements.
func TestFairnessRegressionTableDrivenTestsAreFair(t *testing.T) {
	base := map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "p/p.go": "package p\n\nfunc Upper(s string) string { return s }\n"}
	solution := map[string]string{
		"p/p.go": "package p\n\nimport \"strings\"\n\nfunc Upper(s string) string { return strings.ToUpper(s) }\n",
		"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestUpper(t *testing.T) {\n" +
			"\tfor _, tc := range []struct{ name, in, want string }{\n" +
			"\t\t{\"lower case word\", \"hello world\", \"HELLO WORLD\"},\n\t\t{\"already upper case\", \"ALREADY THERE\", \"ALREADY THERE\"},\n\t} {\n" +
			"\t\tt.Run(\"subtest \"+tc.name, func(t *testing.T) {\n" +
			"\t\t\tif got := Upper(tc.in); got != tc.want {\n\t\t\t\tt.Errorf(\"Upper(%q) returned %q, expected something else\", tc.in, got)\n\t\t\t}\n\t\t})\n\t}\n}\n",
	}
	wantGaps(t, fairnessGaps(t, base, solution, "Make Upper upper-case its input."))
}

func TestFairnessNamesFromAnotherDirectory(t *testing.T) {
	base := map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "a/a.go": "package a\n\ntype Opt struct{ Name string }\n"}
	solution := map[string]string{
		"a/a.go":      "package a\n\ntype Opt struct {\n\tName  string\n\tRetry int\n}\n\nfunc Apply(o Opt, ttl int) Opt { local := o; return local }\n",
		"b/b_test.go": "package b\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/a\"\n)\n\nfunc TestB(t *testing.T) { _ = a.Apply(a.Opt{Name: \"x\", Retry: 3}, 1) }\n",
	}
	// Retry (a struct member) and Apply (a function) are new; Name is old; ttl and local are not package-level.
	wantGaps(t, fairnessGaps(t, base, solution, ""), "identifier:Apply", "identifier:Retry")
}

func TestFairnessOtherLanguagesCheckLiteralsOnly(t *testing.T) {
	base := map[string]string{
		"app.py":            "def note():\n    return 'graded with the base version'\n",
		"tests/test_app.py": "from app import note\n\ndef test_old():\n    assert note()\n",
	}
	solution := map[string]string{
		"app.py": "def note():\n    return 'graded with the starting version'\n\ndef brand_new_helper():\n    pass\n",
		"tests/test_app.py": "from app import note, brand_new_helper\n\ndef test_old():\n    assert note()\n\n" +
			"def test_note():\n    assert note() == 'graded with the starting version'\n    brand_new_helper()\n    assert \"a\" != \"b\"\n" +
			"    assert 'a test-only message' != note()\n",
		"tests/data.json": "{\"message\": \"a long message nobody states\"}\n",
	}
	// No identifiers; data files are not scanned; a text the reference lacks is the test's own.
	wantGaps(t, fairnessGaps(t, base, solution, ""), "literal:graded with the starting version")
}

func TestFairnessFormatStringsAndTrailingPunctuation(t *testing.T) {
	base := map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "p/p.go": "package p\n\nfunc F() {}\n"}
	solution := map[string]string{
		"p/p.go": "package p\n\nimport \"fmt\"\n\nfunc F() error { return fmt.Errorf(\"cannot open %s for writing\", \"x\") }\n",
		"p/p_test.go": "package p\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestF(t *testing.T) {\n" +
			"\tif !strings.Contains(F().Error(), \"cannot open %s for writing.\\n\") {\n\t\tt.Fatal()\n\t}\n}\n",
	}
	// The reference builds the text with a verb and no final period: the pieces still match, and the instruction misses them.
	wantGaps(t, fairnessGaps(t, base, solution, ""), "literal:cannot open %s for writing.")
	wantGaps(t, fairnessGaps(t, base, solution, "The error says: cannot open the file for writing"))
}

func TestFairnessNoHiddenTestsNoGaps(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	b := commit(t, repo, fairBase, "base")
	s := commit(t, repo, fairSolution, "solution")
	ctx := context.Background()
	f := NewFairness("-C", repo)
	gaps, err := f.Gaps(ctx, FairnessInput{Base: b, Solution: s, Reference: []string{"ctx/ctx.go"}})
	if err != nil || len(gaps) != 0 {
		t.Errorf("gaps = %v, %v", gaps, err)
	}
	if _, err := f.Gaps(ctx, FairnessInput{Base: b, Solution: s, HiddenTests: []string{"ctx/missing_test.go"}}); err == nil {
		t.Error("a hidden test file absent from the solution should be an error")
	}
}

// Names are case-sensitive: a base that only has Wait(timeout) does not state a new Timeout field.
func TestFairnessNamesAreCaseSensitive(t *testing.T) {
	base := map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "p/p.go": "package p\n\ntype Config struct{}\n\nfunc Wait(timeout int) {}\n"}
	solution := map[string]string{
		"p/p.go":      "package p\n\ntype Config struct{ Timeout int }\n\nfunc Wait(timeout int) {}\n",
		"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) { _ = Config{Timeout: 5} }\n",
	}
	wantGaps(t, fairnessGaps(t, base, solution, ""), "identifier:Timeout")
}

func TestFairnessCancelledSearchIsAnErrorNotAGap(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	b := commit(t, repo, fairBase, "base")
	s := commit(t, repo, fairSolution, "solution")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewFairness("-C", repo).Gaps(ctx, FairnessInput{Base: b, Solution: s, HiddenTests: []string{"ctx/ctx_test.go"}, Reference: []string{"ctx/ctx.go"}})
	if err == nil {
		t.Error("a cancelled check should fail, not report gaps")
	}
}

// The real case that motivated this: the base already has Metrics.SlashCommands, the solution adds Expect.SlashCommands,
// and the reference builds the drift texts with fmt.Sprintf, so the literals a test compares to are in no reference file.
func TestFairnessSameFieldNameOnAnotherTypeAndFormattedTexts(t *testing.T) {
	base := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.22\n",
		"c/c.go": "package c\n\ntype Metrics struct{ SlashCommands []string }\n\ntype Expect struct{ Skills []string }\n\n" +
			"func Check(m Metrics, e Expect) []string { return nil }\n",
		"c/c_test.go": "package c\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) { _ = Check(Metrics{SlashCommands: nil}, Expect{Skills: nil}) }\n",
	}
	solution := map[string]string{
		"c/c.go": "package c\n\nimport \"fmt\"\n\ntype Metrics struct{ SlashCommands []string }\n\n" +
			"type Expect struct {\n\tSkills        []string\n\tSlashCommands []string\n}\n\n" +
			"func Check(m Metrics, e Expect) []string {\n\treturn []string{fmt.Sprintf(\"slash commands differ (%d added, %d missing)\", 1, 1),\n" +
			"\t\tfmt.Sprintf(\"%d personal skill(s) loaded\", 2)}\n}\n",
		"c/c_test.go": "package c\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) { _ = Check(Metrics{SlashCommands: nil}, Expect{Skills: nil}) }\n\n" +
			"func TestNew(t *testing.T) {\n\tdrift := Check(Metrics{SlashCommands: []string{\"a\"}}, Expect{SlashCommands: []string{\"b\"}})\n" +
			"\tif drift[0] != \"slash commands differ (1 added, 1 missing)\" || drift[1] != \"2 personal skill(s) loaded\" {\n\t\tt.Error(drift)\n\t}\n}\n",
	}
	instruction := "Only loaded skills should count; report a personal skill only when it is loaded."
	wantGaps(t, fairnessGaps(t, base, solution, instruction),
		"identifier:SlashCommands", "literal:2 personal skill(s) loaded", "literal:slash commands differ (1 added, 1 missing)")
	// Stating the field and the fixed texts clears them.
	stated := instruction + " Expect gets a SlashCommands field. Report \"slash commands differ (N added, M missing)\" and \"N personal skill(s) loaded\"."
	wantGaps(t, fairnessGaps(t, base, solution, stated))
}

// Typed keys must be resolved in the type's own package and against the base test's own uses.
func TestFairnessTypedKeysDoNotFlagOldOrForeignTypes(t *testing.T) {
	base := map[string]string{
		"go.mod":      "module example.com/m\n\ngo 1.22\n",
		"cli/cli.go":  "package cli\n\ntype Info struct{ Name string }\n",
		"run/run.go":  "package run\n\ntype Record struct{ Arm int }\n",
		"claude/c.go": "package claude\n\ntype Expect struct{ CLIVersion string }\n\ntype Local struct{ N int }\n",
		"claude/c_test.go": "package claude\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/run\"\n)\n\n" +
			"func TestOld(t *testing.T) { _ = run.Record{Arm: 1} }\n",
	}
	solution := map[string]string{
		// The references add fields named like keys the tests use: CLIVersion in another package, Arm on a local type.
		"cli/cli.go":  "package cli\n\ntype Info struct {\n\tName       string\n\tCLIVersion string\n}\n",
		"claude/c.go": "package claude\n\ntype Expect struct{ CLIVersion string }\n\ntype Local struct {\n\tN   int\n\tArm int\n}\n",
		"claude/c_test.go": "package claude\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/run\"\n)\n\n" +
			"func TestOld(t *testing.T) { _ = run.Record{Arm: 1} }\n\n" +
			"func TestNew(t *testing.T) { _ = Expect{CLIVersion: \"v\"}; _ = &run.Record{Arm: 2} }\n",
	}
	// run.Record{Arm:} is unchanged or another package's; Expect{CLIVersion:} names a field the base's Expect has.
	wantGaps(t, fairnessGaps(t, base, solution, ""))
	// A new field on a type the test names in its own directory is still found.
	solution["claude/c_test.go"] += "\nfunc TestLocal(t *testing.T) { _ = Local{Arm: 3} }\n"
	wantGaps(t, fairnessGaps(t, base, solution, ""), "identifier:Arm")
}

func TestFairnessGenericFormatDoesNotHideLiteralGaps(t *testing.T) {
	solution := map[string]string{}
	for k, v := range fairSolution {
		solution[k] = v
	}
	solution["ctx/extra.go"] = "package ctx\n\nimport \"fmt\"\n\nfunc Number(n int) string { return fmt.Sprintf(\"%d\", n) }\n\nfunc Pair(a, b any) string { return fmt.Sprintf(\"%s: %v\", a, b) }\n"
	gaps := fairnessGaps(t, fairBase, solution, "Make Include reject non-documents and update the note.")
	wantGaps(t, gaps, "identifier:SlashCommands", "literal:graded with the starting version", "literal:"+errText)
}

func TestFairnessControlCharactersSeparatePieces(t *testing.T) {
	if got := strings.Join(piecesOf("3\t1\ta.go\x00"), "|"); got != "" {
		t.Errorf("pieces = %q", got)
	}
	if got := strings.Join(piecesOf("first chunk of text\x00second chunk of text"), "|"); got != "first chunk of text|second chunk of text" {
		t.Errorf("pieces = %q", got)
	}
	base := map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "p/p.go": "package p\n\nfunc F() {}\n"}
	solution := map[string]string{
		"p/p.go":      "package p\n\nfunc F() string { return \"first chunk of text\" + \"\\x00\" + \"second chunk of text\" }\n",
		"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tif F() != \"first chunk of text\\x00second chunk of text\" {\n\t\tt.Fatal()\n\t}\n}\n",
	}
	wantGaps(t, fairnessGaps(t, base, solution, ""), "literal:first chunk of text\x00second chunk of text")
}

// A qualified or dot-imported type that cannot be resolved falls back to the word search instead of being dropped.
func TestFairnessUnresolvedTypedKeysFallBackToWordSearch(t *testing.T) {
	base := map[string]string{
		"go.mod":       "module \"example.com/m\"\n\ngo 1.22\n",
		"run/run.go":   "package run\n\ntype Record struct{ Arm int }\n",
		"foo-bar/f.go": "package foobar\n\ntype Item struct{ Arm int }\n",
		"t/t_test.go":  "package t\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n",
	}
	solution := map[string]string{
		"run/run.go":   "package run\n\ntype Record struct {\n\tArm   int\n\tExtra int\n}\n",
		"foo-bar/f.go": "package foobar\n\ntype Item struct {\n\tArm   int\n\tExtra2 int\n}\n",
	}
	dot := "package t\n\nimport (\n\t\"testing\"\n\n\t. \"example.com/m/run\"\n)\n\nfunc TestNew(t *testing.T) { _ = Record{Extra: 1} }\n"
	solution["t/t_test.go"] = dot
	wantGaps(t, fairnessGaps(t, base, solution, ""), "identifier:Extra")
	named := "package t\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/foo-bar\"\n)\n\nfunc TestNew(t *testing.T) { _ = foobar.Item{Extra2: 1} }\n"
	solution["t/t_test.go"] = named // package foobar lives in foo-bar/: the import's last element does not name it
	wantGaps(t, fairnessGaps(t, base, solution, ""), "identifier:Extra2")
}
