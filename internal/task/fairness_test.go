package task

import (
	"context"
	"slices"
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

// Each language has one literal the reference produces and nothing states (flagged), a stated one, and a literal the
// base already had (neither is flagged).
func TestFairnessJavaKotlinAndRustLiterals(t *testing.T) {
	const instruction = "Check returns the message \"stated message here\" for an empty value."
	cases := map[string]struct {
		base, solution map[string]string
		want           string
	}{
		"java": {
			map[string]string{"src/main/java/Check.java": "class Check { static String old() { return \"the base message\"; } }\n"},
			map[string]string{
				"src/main/java/Check.java": "class Check {\n  static String old() { return \"the base message\"; }\n  static String a() { return \"unstated java message\"; }\n  static String b() { return \"stated message here\"; }\n}\n",
				"src/CheckTest.java":       "class CheckTest {\n  void t() {\n    assertEquals(\"unstated java message\", Check.a());\n    assertEquals(\"stated message here\", Check.b());\n    assertEquals(\"the base message\", Check.old());\n    char q = '\"'; // \"in a comment only\"\n  }\n}\n"},
			"unstated java message",
		},
		"java text block": {
			map[string]string{"src/main/java/Check.java": "class Check {}\n"},
			map[string]string{
				"src/main/java/Check.java": "class Check {\n  static String a() { return \"first line of the block\\nsecond unstated line\"; }\n}\n",
				"src/CheckTest.java":       "class CheckTest {\n  void t() {\n    String want = \"\"\"\n        first line of the block\n        second unstated line\n        \"\"\";\n  }\n}\n"},
			"first line of the block\n        second unstated line",
		},
		"kotlin": {
			map[string]string{"src/main/kotlin/Check.kt": "fun old() = \"the base message\"\n"},
			map[string]string{
				"src/main/kotlin/Check.kt": "fun old() = \"the base message\"\nfun a() = \"unstated kotlin message\"\nfun b() = \"stated message here\"\n",
				"src/CheckTest.kt":         "class CheckTest {\n  fun t() {\n    assertEquals(\"unstated kotlin message\", a())\n    assertEquals(\"stated message here\", b())\n    assertEquals(\"\"\"the base message\"\"\", old())\n    val n = 3; println(\"count $n and ${n + 1} items\")\n  }\n}\n"},
			"unstated kotlin message",
		},
		"rust": {
			map[string]string{"src/lib.rs": "pub fn old() -> &'static str { \"the base message\" }\n"},
			map[string]string{
				"src/lib.rs": "pub fn old() -> &'static str { \"the base message\" }\npub fn a() -> &'static str { \"unstated rust message\" }\npub fn b() -> &'static str { \"stated message here\" }\n",
				"tests/t.rs": "#[test]\nfn t() {\n    assert_eq!(\"unstated rust message\", m::a());\n    assert_eq!(r#\"stated message here\"#, m::b());\n    assert_eq!(b\"the base message\", m::old().as_bytes());\n    let _c = '\"'; let _l: &'static str = \"x\"; // \"in a comment only\"\n}\n"},
			"unstated rust message",
		},
		"rust raw string": {
			map[string]string{"src/lib.rs": "pub fn old() {}\n"},
			map[string]string{
				"src/lib.rs": "pub fn a() -> &'static str { r#\"raw \"quoted\" unstated text\"# }\n",
				"tests/t.rs": "#[test]\nfn t() {\n    assert_eq!(r#\"raw \"quoted\" unstated text\"#, m::a());\n}\n"},
			"raw \"quoted\" unstated text",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gaps := fairnessGaps(t, tc.base, tc.solution, instruction)
			wantGaps(t, gaps, "literal:"+tc.want)
		})
	}
}

func TestStringLiteralsLexer(t *testing.T) {
	for name, tc := range map[string]struct {
		l    lang
		src  string
		want []string
	}{
		"java escapes and char":   {langJava, `a("x\"y\n", '"', '\'', "z"); // "no"` + "\n/* \"no\" */", []string{"x\"y\n", "z"}},
		"java stray quote":        {langJava, "a(\"open\nb(\"next\")", []string{"open", "next"}},
		"kotlin raw and template": {langKotlin, `"a $b c" """r ${d + 1} \n""" "\$x"`, []string{"a \n c", "r \n \\n", "$x"}},
		"rust byte and raw":       {langRust, `b"by" r"a\b" r##"x"#y"## br#"z"# c"cs" r#type "end"`, []string{"by", `a\b`, `x"#y`, "z", "cs", "end"}},
		"rust lifetimes":          {langRust, `fn f<'a>(x: &'a str) -> &'static str { "ok" } 'x' '\u{1F600}'`, []string{"ok"}},
		"rust continuation":       {langRust, "\"one \\\n    two\"", []string{"one two"}},
		"rust nested comment":     {langRust, `/* a /* "no" */ "no" */ "yes"`, []string{"yes"}},
		"rust identifier ends r":  {langRust, `bar"x" for"y"`, []string{"x", "y"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := stringLiterals(tc.src, tc.l); !slices.Equal(got, tc.want) {
				t.Errorf("literals = %q, want %q", got, tc.want)
			}
		})
	}
}

// The Java, Kotlin and Rust identifier check: a name the hidden test uses, the reference declares, and neither the
// instruction nor the base has.
func TestFairnessJVMAndRustIdentifiers(t *testing.T) {
	const instruction = "Add the feature, exposed through Stated.stated_value()."
	cases := map[string]struct {
		base, solution map[string]string
		want           []string
	}{
		"java static import constant": {
			map[string]string{"src/main/java/Ext.java": "package p;\npublic class Ext { public static final String OLD_PARAMETER = \"old\"; }\n"},
			map[string]string{
				"src/main/java/Ext.java": "package p;\npublic class Ext {\n  public static final String OLD_PARAMETER = \"old\";\n  public static final String EXECUTION_DATE_PARAMETER = \"date\";\n}\n",
				"src/test/java/ExtTests.java": "package p;\nimport static p.Ext.EXECUTION_DATE_PARAMETER;\nimport static p.Ext.OLD_PARAMETER;\nimport java.util.Objects;\nimport org.junit.jupiter.api.Test;\n" +
					"class ExtTests {\n  // MISSING_IN_COMMENT\n  @Test void t() { Objects.requireNonNull(EXECUTION_DATE_PARAMETER); Objects.requireNonNull(OLD_PARAMETER); }\n}\n"},
			[]string{"EXECUTION_DATE_PARAMETER"},
		},
		"java: base, JDK, test-own and stated names are fine": {
			map[string]string{"src/main/java/Ext.java": "package p;\npublic class Ext { public static String existing() { return null; } }\n"},
			map[string]string{
				"src/main/java/Ext.java":      "package p;\npublic class Ext {\n  public static String existing() { return null; }\n  public static String stated_value() { return null; }\n  public String toList() { return null; }\n  public static void helperName() {}\n}\n",
				"src/test/java/ExtTests.java": "package p;\nimport java.util.List;\nclass ExtTests {\n  static void helperName() {}\n  void t() { Ext.existing(); Ext.stated_value(); List.of(); helperName(); }\n}\n"},
			nil,
		},
		"kotlin": {
			map[string]string{"src/main/kotlin/Check.kt": "fun oldCheck() = 1\n"},
			map[string]string{
				"src/main/kotlin/Check.kt":     "fun oldCheck() = 1\nconst val NEW_LIMIT = 5\nfun newChecker() = 2\n",
				"src/test/kotlin/CheckTest.kt": "import kotlin.test.assertEquals\nclass CheckTest {\n  @Test fun t() { assertEquals(5, NEW_LIMIT); assertEquals(1, oldCheck()); listOf(1).map { it } }\n}\n"},
			[]string{"NEW_LIMIT"},
		},
		"rust": {
			map[string]string{"src/lib.rs": "pub fn old_item() {}\n", "tests/t.rs": "use m::old_item;\n#[test]\nfn t() { old_item(); }\n"},
			map[string]string{
				"src/lib.rs": "pub fn old_item() {}\npub mod fresh_module { pub const FRESH_LIMIT: u32 = 3; }\npub struct Config { pub fresh_field: u32 }\n",
				"tests/t.rs": "use m::old_item;\nuse m::fresh_module::FRESH_LIMIT;\nuse serde::Serialize;\nuse std::collections::HashMap;\n#[test]\nfn t() { old_item(); let _ = FRESH_LIMIT; let _m: HashMap<u8, u8> = HashMap::new(); }\n"},
			[]string{"FRESH_LIMIT", "fresh_module"},
		},
		"rust: a dependency's items and a base item are fine": {
			map[string]string{"src/lib.rs": "pub fn shared_item() {}\n"},
			map[string]string{
				"src/lib.rs": "pub fn shared_item() {}\n",
				"tests/t.rs": "use serde_json::json;\nuse m::shared_item;\n#[test]\nfn t() { shared_item(); let _ = json!({}); }\n"},
			nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gaps := fairnessGaps(t, tc.base, tc.solution, instruction)
			var want []string
			for _, w := range tc.want {
				want = append(want, "identifier:"+w)
			}
			wantGaps(t, gaps, want...)
		})
	}
}

// Locals and parameters of a test never count, and overrides, switch defaults and comments or strings are not
// declarations; annotation elements and modifier-less interface methods are.
func TestFairnessJVMAndRustIdentifierEdgeCases(t *testing.T) {
	cases := map[string]struct {
		base, solution map[string]string
		want           []string
	}{
		"java local colliding with a reference field": {
			map[string]string{"src/main/java/R.java": "class R { void withDelay(long d) {} }\n"},
			map[string]string{
				"src/main/java/R.java":      "class R {\n  private long retryDelay;\n  void withDelay(long d) {}\n}\n",
				"src/test/java/RTests.java": "class RTests {\n  void t() {\n    long retryDelay = 5;\n    new R().withDelay(retryDelay);\n  }\n}\n"},
			nil,
		},
		"java parameter colliding with a reference field": {
			map[string]string{"src/main/java/R.java": "class R {}\n"},
			map[string]string{
				"src/main/java/R.java":      "class R {\n  private int retryDelay;\n}\n",
				"src/test/java/RTests.java": "class RTests {\n  void check(int retryDelay) { System.out.println(retryDelay); }\n}\n"},
			nil,
		},
		"kotlin parameter and lambda colliding with reference names": {
			map[string]string{"src/main/kotlin/R.kt": "fun old() = 1\n"},
			map[string]string{
				"src/main/kotlin/R.kt":     "fun old() = 1\nprivate val indentWidth = 2\nprivate val lineCount = 3\n",
				"src/test/kotlin/RTest.kt": "class RTest {\n  fun check(indentWidth: Int) { listOf(1).map { lineCount -> lineCount + indentWidth } }\n}\n"},
			nil,
		},
		"rust let and parameter colliding with reference names": {
			map[string]string{"src/lib.rs": "pub fn old() {}\n"},
			map[string]string{
				"src/lib.rs": "pub fn old() {}\nfn retry_delay() {}\nfn line_count() {}\n",
				"tests/t.rs": "fn check(line_count: u32) { let mut retry_delay = 1; retry_delay += line_count; }\n#[test]\nfn t() { check(1); }\n"},
			nil,
		},
		"java annotation element and interface method without a modifier": {
			map[string]string{"src/main/java/A.java": "@interface A { int old() default 1; }\n"},
			map[string]string{
				"src/main/java/A.java":      "@interface A {\n  int old() default 1;\n  int jitterSeed() default 0;\n}\ninterface Shape {\n  double computeArea(int scale);\n  default String describeShape() { return \"\"; }\n}\n",
				"src/test/java/ATests.java": "class ATests {\n  @A(jitterSeed = 3) void t(Shape s) { s.computeArea(1); s.describeShape(); }\n}\n"},
			[]string{"Shape", "computeArea", "describeShape", "jitterSeed"},
		},
		"java override, switch default and statements are not declarations": {
			map[string]string{"src/main/java/C.java": "class C {}\n"},
			map[string]string{
				"src/main/java/C.java":      "class C implements Comparable<C> {\n  @Override\n  public int compareTo(C other) {\n    switch (1) { default: break; }\n    return Math.max(other.hashCode(), 0);\n  }\n  public int zero() { return 0; }\n}\n",
				"src/test/java/CTests.java": "class CTests {\n  void t() { new C().compareTo(new C()); }\n}\n"},
			nil,
		},
		"kotlin override and comments and strings": {
			map[string]string{"src/main/kotlin/C.kt": "class C\n"},
			map[string]string{
				"src/main/kotlin/C.kt":     "class C : Comparable<C> {\n  override fun compareTo(other: C) = 0\n  // fun commentedOut() {}\n  val s = \"fun inString() {}\"\n  /* nested /* fun inBlock() {} */ */\n}\n",
				"src/test/kotlin/CTest.kt": "class CTest { fun t() { C().compareTo(C()); commentedOut(); inString(); inBlock() } }\n"},
			nil,
		},
		"rust trait impl methods and comments and strings": {
			map[string]string{"src/lib.rs": "pub struct S;\n"},
			map[string]string{
				"src/lib.rs": "pub struct S;\nimpl IntoIterator for S {\n    fn into_iter(self) -> Vec<u8> { vec![] }\n}\n// fn commented_out() {}\nconst T: &str = \"fn in_string() {}\";\n",
				"tests/t.rs": "#[test]\nfn t() { let _ = S.into_iter(); commented_out(); in_string(); }\n"},
			nil,
		},
		"a hidden test-support file is not flagged": {
			map[string]string{"src/main/java/R.java": "class R {}\n"},
			map[string]string{
				"src/main/java/R.java":        "class R {}\n",
				"src/test/java/Fixtures.java": "class Fixtures {\n  static final String FIXTURE_NAME = \"x\";\n}\n",
				"src/test/java/RTests.java":   "class RTests {\n  void t() { System.out.println(Fixtures.FIXTURE_NAME); }\n}\n"},
			nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gaps := fairnessGaps(t, tc.base, tc.solution, "")
			var want []string
			for _, w := range tc.want {
				want = append(want, "identifier:"+w)
			}
			wantGaps(t, gaps, want...)
		})
	}
}

// An annotation attribute written "name = value" after another attribute is not a local variable.
func TestFairnessJVMAnnotationAttributeIsNotALocal(t *testing.T) {
	gaps := fairnessGaps(t,
		map[string]string{"src/main/java/R.java": "@interface R { int maxAttempts() default 1; }\n"},
		map[string]string{
			"src/main/java/R.java":      "@interface R {\n  int maxAttempts() default 1;\n  int maxJitterMs() default 0;\n}\n",
			"src/test/java/RTests.java": "class RTests {\n  @R(maxAttempts = 3, maxJitterMs = -1) void t() {}\n}\n"}, "")
	wantGaps(t, gaps, "identifier:maxJitterMs")
}
