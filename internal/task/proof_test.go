package task

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Canned `go test -json` output, as go 1.27 writes it (times left out: the reader ignores them).
const (
	eventsPassSkipFail = `{"Action":"start","Package":"example.com/m/a"}
{"Action":"run","Package":"example.com/m/a","Test":"TestPass"}
{"Action":"output","Package":"example.com/m/a","Test":"TestPass","Output":"=== RUN   TestPass\n"}
{"Action":"run","Package":"example.com/m/a","Test":"TestPass/sub"}
{"Action":"pass","Package":"example.com/m/a","Test":"TestPass/sub","Elapsed":0}
{"Action":"output","Package":"example.com/m/a","Test":"TestPass","Output":"--- PASS: TestPass (0.00s)\n"}
{"Action":"pass","Package":"example.com/m/a","Test":"TestPass","Elapsed":0}
{"Action":"run","Package":"example.com/m/a","Test":"TestSkip"}
{"Action":"output","Package":"example.com/m/a","Test":"TestSkip","Output":"--- SKIP: TestSkip (0.00s)\n"}
{"Action":"skip","Package":"example.com/m/a","Test":"TestSkip","Elapsed":0}
{"Action":"run","Package":"example.com/m/a","Test":"TestFail"}
{"Action":"output","Package":"example.com/m/a","Test":"TestFail","Output":"--- FAIL: TestFail (0.00s)\n"}
{"Action":"fail","Package":"example.com/m/a","Test":"TestFail","Elapsed":0}
{"Action":"run","Package":"example.com/m/a","Test":"TestSubFails"}
{"Action":"run","Package":"example.com/m/a","Test":"TestSubFails/case"}
{"Action":"fail","Package":"example.com/m/a","Test":"TestSubFails/case","Elapsed":0}
{"Action":"fail","Package":"example.com/m/a","Test":"TestSubFails","Elapsed":0}
{"Action":"output","Package":"example.com/m/a","Output":"FAIL\n"}
{"Action":"fail","Package":"example.com/m/a","Elapsed":0.948}
`
	// A TestMain or an init that exits 0 before any test: the package passes, no test has an event.
	eventsExitedEarly = `{"Action":"start","Package":"example.com/m"}
{"Action":"output","Package":"example.com/m","Output":"ok  \texample.com/m\t0.2s\n"}
{"Action":"pass","Package":"example.com/m","Elapsed":0.2}
`
	eventsBuildFail = `{"ImportPath":"example.com/m/b [example.com/m/b.test]","Action":"build-output","Output":"b/b.go:3:15: declared and not used: x\n"}
{"ImportPath":"example.com/m/b [example.com/m/b.test]","Action":"build-fail"}
{"Action":"start","Package":"example.com/m/b"}
{"Action":"output","Package":"example.com/m/b","Output":"FAIL\texample.com/m/b [build failed]\n"}
{"Action":"fail","Package":"example.com/m/b","Elapsed":0,"FailedBuild":"example.com/m/b [example.com/m/b.test]"}
`
	// A cached result replays the events of the run it saved (the proof's -count=1 never takes one).
	eventsCached = `{"Action":"start","Package":"example.com/m/a"}
{"Action":"run","Package":"example.com/m/a","Test":"TestPass"}
{"Action":"pass","Package":"example.com/m/a","Test":"TestPass","Elapsed":0}
{"Action":"output","Package":"example.com/m/a","Output":"ok  \texample.com/m/a\t(cached)\n"}
{"Action":"pass","Package":"example.com/m/a","Elapsed":0}
`
	// Two packages with a test of the same name: it passes only when it passes in both.
	eventsTwoPackages = `{"Action":"run","Package":"example.com/m/a","Test":"TestSame"}
{"Action":"pass","Package":"example.com/m/a","Test":"TestSame","Elapsed":0}
{"Action":"run","Package":"example.com/m/b","Test":"TestSame"}
{"Action":"fail","Package":"example.com/m/b","Test":"TestSame","Elapsed":0}
{"Action":"run","Package":"example.com/m/a","Test":"TestBoth"}
{"Action":"pass","Package":"example.com/m/a","Test":"TestBoth","Elapsed":0}
{"Action":"run","Package":"example.com/m/b","Test":"TestBoth"}
{"Action":"pass","Package":"example.com/m/b","Test":"TestBoth","Elapsed":0}
`
)

func TestTestResultsReadsEachTestsOutcome(t *testing.T) {
	for name, c := range map[string]struct {
		events string
		want   []string
		saw    map[string]string
	}{
		"pass, skip, fail, subtests": {eventsPassSkipFail, []string{"TestPass", "TestSkip", "TestFail", "TestSubFails", "TestAbsent"},
			map[string]string{"TestPass": "pass", "TestSkip": SawSkip, "TestFail": SawFail, "TestSubFails": SawFail}},
		"no events for a test":            {eventsExitedEarly, []string{"TestHidden"}, map[string]string{}},
		"a compile error":                 {eventsBuildFail, []string{"TestB"}, map[string]string{}},
		"a cached replay":                 {eventsCached, []string{"TestPass"}, map[string]string{"TestPass": "pass"}},
		"two packages with the same name": {eventsTwoPackages, []string{"TestSame", "TestBoth"}, map[string]string{"TestSame": SawFail, "TestBoth": "pass"}},
		"nothing at all":                  {"", []string{"TestX"}, map[string]string{}},
		"not events, no final newline": {"go: downloading x\nnot json\n" + `{"Action":"pass","Test":"TestX"}`, []string{"TestX"},
			map[string]string{"TestX": "pass"}},
	} {
		got, err := testResults(strings.NewReader(c.events), c.want)
		if err != nil || !reflect.DeepEqual(got, c.saw) {
			t.Errorf("%s: %v, %v; want %v", name, got, err, c.saw)
		}
	}
}

// A line longer than the reader keeps (a test's huge output) is skipped whole, and the lines after it still count.
func TestTestResultsSkipsOverlongLines(t *testing.T) {
	long := `{"Action":"pass","Test":"TestLong","Output":"` + strings.Repeat("x", maxEventLine) + `"}`
	events := long + "\n" + `{"Action":"pass","Test":"TestAfter"}` + "\n"
	got, err := testResults(strings.NewReader(events), []string{"TestLong", "TestAfter"})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"TestAfter": "pass"}) {
		t.Errorf("%v, %v", got, err)
	}
}

func TestPlanGoProof(t *testing.T) {
	base := snapSource{
		"a/a_test.go":     "package a\n\nimport \"testing\"\n\nvar cases = 1\n\nfunc TestOld(t *testing.T) {}\n\nfunc TestTable(t *testing.T) { _ = cases }\n",
		"b/b_test.go":     "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n",
		"table/x_test.go": "package table\n\nimport \"testing\"\n\nvar want = 1\n\nfunc TestTable(t *testing.T) { _ = want }\n",
	}
	solution := snapSource{
		// TestNew is new, TestTable changed, TestOld unchanged, TestMain never counts.
		"a/a_test.go": "package a\n\nimport \"testing\"\n\nvar cases = 1\n\nfunc TestMain(m *testing.M) { m.Run() }\n\nfunc TestOld(t *testing.T) {}\n\n" +
			"func TestTable(t *testing.T) { _ = cases; _ = 2 }\n\nfunc TestNew(t *testing.T) {}\n",
		"b/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n\nfunc ExampleB() {\n\t// Output:\n}\n",
		// Only a table changed: no test function is new or changed.
		"table/x_test.go":            "package table\n\nimport \"testing\"\n\nvar want = 2\n\nfunc TestTable(t *testing.T) { _ = want }\n",
		"helpers_test.go":            "package root\n\nvar _ = 1\n",
		"a/testdata/fixture_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestFixture(t *testing.T) {}\n",
		"_skip/x_test.go":            "package skip\n\nimport \"testing\"\n\nfunc TestIgnored(t *testing.T) {}\n",
	}
	hidden := []string{"a/a_test.go", "b/b_test.go", "a/testdata/fixture_test.go", "_skip/x_test.go", "README.md"}
	proof, goFiles := PlanGoProof(Spec{HiddenTests: hidden, Verify: []string{"go test ./..."}}, base, solution)
	want := GoProof{Packages: []ProofPackage{{Dir: "a", Tests: []string{"TestNew", "TestTable"}}, {Dir: "b", Tests: []string{"ExampleB"}}}}
	if !goFiles || !reflect.DeepEqual(proof, want) || proof.Tests() != 3 {
		t.Errorf("own tests: %+v, %v", proof, goFiles)
	}
	// A folder the wildcards skip is proven when a verify command names it, from the module's folder.
	proof, _ = PlanGoProof(Spec{HiddenTests: hidden, Verify: []string{"go test ./... && go test -count=1 -run Fix ./testdata", "go test ../_skip"}, Module: "a"},
		base, solution)
	want = GoProof{Packages: []ProofPackage{{Dir: "_skip", Tests: []string{"TestIgnored"}}, {Dir: "a", Tests: []string{"TestNew", "TestTable"}},
		{Dir: "a/testdata", Tests: []string{"TestFixture"}}, {Dir: "b", Tests: []string{"ExampleB"}}}}
	if !reflect.DeepEqual(proof, want) {
		t.Errorf("named folders: %+v", proof)
	}
	// None is new or changed: all of the hidden files' tests.
	proof, _ = PlanGoProof(Spec{HiddenTests: []string{"table/x_test.go", "b/b_test.go"}}, base, snapSource{"table/x_test.go": solution["table/x_test.go"], "b/b_test.go": base["b/b_test.go"]})
	if want := (GoProof{Packages: []ProofPackage{{Dir: "b", Tests: []string{"TestB"}}, {Dir: "table", Tests: []string{"TestTable"}}}}); !reflect.DeepEqual(proof, want) {
		t.Errorf("fallback: %+v", proof)
	}
	// Go test files without a test function: nothing to prove. No Go test files: no proof at all.
	if proof, goFiles := PlanGoProof(Spec{HiddenTests: []string{"helpers_test.go"}}, base, solution); !proof.Empty() || !goFiles {
		t.Errorf("nothing to prove: %+v, %v", proof, goFiles)
	}
	if proof, goFiles := PlanGoProof(Spec{HiddenTests: []string{"tests/value_test.sh"}}, base, snapSource{"tests/value_test.sh": "true\n"}); !proof.Empty() || goFiles {
		t.Errorf("not Go: %+v, %v", proof, goFiles)
	}
}

// An example runs only with an output comment that ends it (Go's own rule): a line that looks like one inside a raw
// string does not make a documentation example runnable, so the proof never waits for an event Go does not emit.
func TestGoTestFuncsTakesGosRuleForExamples(t *testing.T) {
	src := "package a\n\nimport \"fmt\"\n\nfunc ExampleDoc() {\n\tfmt.Println(`\n// Output: shown in the docs\n`)\n}\n\n" +
		"func ExampleRuns() {\n\tfmt.Println(1)\n\t// Output: 1\n}\n\nfunc ExampleEmpty() {\n\t// Output:\n}\n\n" +
		"func ExampleUnordered() {\n\t// Unordered output:\n\t// 1\n}\n"
	got, ok := goTestFuncs([]byte(src))
	if !ok || len(got) != 3 || got["ExampleDoc"] != "" || got["ExampleRuns"] == "" || got["ExampleEmpty"] == "" || got["ExampleUnordered"] == "" {
		t.Errorf("%v, %v", slices.Sorted(maps.Keys(got)), ok)
	}
}

func TestChangesTestMain(t *testing.T) {
	main := func(body string) []byte {
		return []byte("package a\n\nimport \"testing\"\n\nfunc TestMain(m *testing.M) { " + body + " }\n")
	}
	plain := []byte("package a\n\nfunc helper() {}\n")
	for name, c := range map[string]struct {
		before, after []byte
		want          bool
	}{
		"added":          {nil, main("m.Run()"), true},
		"added to file":  {plain, main("m.Run()"), true},
		"changed":        {main("m.Run()"), main("_ = m"), true},
		"unchanged":      {main("m.Run()"), main("m.Run()"), false},
		"removed":        {main("m.Run()"), plain, false},
		"none":           {nil, plain, false},
		"does not parse": {nil, []byte("package a\nfunc TestMain("), false},
	} {
		if got := ChangesTestMain(c.before, c.after); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestProofCommandFromTheModulesFolder(t *testing.T) {
	pkg := ProofPackage{Dir: "svc/pkg", Tests: []string{"ExampleX", "TestA"}}
	for module, want := range map[string]string{
		"":         "go test -json -count=1 -run '^(ExampleX|TestA)$' ./svc/pkg",
		"svc":      "go test -json -count=1 -run '^(ExampleX|TestA)$' ./pkg",
		"svc/pkg":  "go test -json -count=1 -run '^(ExampleX|TestA)$' .",
		"other":    "go test -json -count=1 -run '^(ExampleX|TestA)$' ../svc/pkg",
		"svc/pkg/": "go test -json -count=1 -run '^(ExampleX|TestA)$' .",
	} {
		if got := pkg.Command(module); got != want {
			t.Errorf("module %q: %s", module, got)
		}
	}
	if got := (ProofPackage{Dir: "it's dir", Tests: []string{"TestA"}}).Command(""); got != `go test -json -count=1 -run '^(TestA)$' './it'\''s dir'` {
		t.Errorf("quoted: %s", got)
	}
}

// Run goes package by package, stops at the first not proven (the rest are reported as not run), and writes each
// command and one line of what it saw to the log.
func TestProvingRunStopsAtTheFirstFolderNotProven(t *testing.T) {
	events := map[string]string{
		"./a": `{"Action":"pass","Package":"m/a","Test":"TestA"}` + "\n",
		"./b": eventsExitedEarly,
	}
	var ran []string
	p := &Proving{Proof: GoProof{Packages: []ProofPackage{{Dir: "a", Tests: []string{"TestA"}}, {Dir: "b", Tests: []string{"TestB"}},
		{Dir: "c", Tests: []string{"TestC"}}}}, Events: filepath.Join(t.TempDir(), ProofEvents)}
	var log strings.Builder
	ok, err := p.Run(context.Background(), &log, func(_ context.Context, command string, f *os.File) (Command, error) {
		target := command[strings.LastIndex(command, " ")+1:]
		ran = append(ran, target)
		_, err := f.WriteString(events[target])
		return Command{Command: command}, err
	})
	if err != nil || ok || !slices.Equal(ran, []string{"./a", "./b"}) {
		t.Fatalf("ok %v, err %v, ran %v", ok, err, ran)
	}
	want := []MissingTest{{Package: "./b", Test: "TestB", Saw: SawNone}, {Package: "./c", Test: "TestC", Saw: SawNone}}
	if !reflect.DeepEqual(p.Result.Missing, want) || p.Result.Tests != 3 || len(p.Result.Commands) != 2 || p.Result.Proven() {
		t.Errorf("result %+v", p.Result)
	}
	if !strings.Contains(log.String(), "$ go test -json -count=1 -run '^(TestB)$' ./b\n") ||
		!strings.Contains(log.String(), "[agentium] the hidden tests did not run: go test -json showed no pass for TestB in ./b (not run), TestC in ./c (not run) (its events: proof.jsonl)\n") {
		t.Errorf("log:\n%s", log.String())
	}
	// All proven.
	p = &Proving{Proof: GoProof{Packages: []ProofPackage{{Dir: "a", Tests: []string{"TestA"}}}}, Events: filepath.Join(t.TempDir(), ProofEvents)}
	log.Reset()
	ok, err = p.Run(context.Background(), &log, func(_ context.Context, command string, f *os.File) (Command, error) {
		_, err := f.WriteString(events["./a"])
		return Command{Command: command}, err
	})
	if err != nil || !ok || !p.Result.Proven() || !strings.Contains(log.String(), "the hidden tests ran: go test -json showed each of the task's 1 hidden test(s) pass") {
		t.Errorf("proven: %v, %v, %+v\n%s", ok, err, p.Result, log.String())
	}
	// A proof that ran out of time says so.
	p = &Proving{Proof: GoProof{Packages: []ProofPackage{{Dir: "a", Tests: []string{"TestA"}}}}, Events: filepath.Join(t.TempDir(), ProofEvents)}
	log.Reset()
	ok, err = p.Run(context.Background(), &log, func(_ context.Context, command string, _ *os.File) (Command, error) {
		return Command{Command: command, ExitCode: -1, TimedOut: true}, nil
	})
	if err != nil || ok || !p.Result.TimedOut() || p.Result.Words() != "the proof that the hidden tests ran timed out" ||
		!strings.Contains(log.String(), "[agentium] the proof timed out\n[agentium] the proof that the hidden tests ran timed out: go test -json showed no pass for TestA in ./a (not run)") {
		t.Errorf("timed out: %v, %v, %+v\n%s", ok, err, p.Result, log.String())
	}
	if (*TestProof)(nil).Proven() || (&TestProof{Tests: 1}).Proven() {
		t.Error("a proof that never ran proves nothing")
	}
}

// goModule is a Go module whose Value returns 0 at the base; each solution sets the files it names. The hidden test
// value_test.go wants Value() == 1.
func goModule(t *testing.T, base map[string]string, solutions ...map[string]string) (bare, baseCommit string, ids []string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	files := map[string]string{"go.mod": "module example.com/fixture\n\ngo 1.22\n", "value.go": "package fixture\n\nfunc Value() int { return 0 }\n"}
	for p, c := range base {
		files[p] = c
	}
	return history(t, files, solutions...)
}

const hiddenGoTest = "package fixture\n\nimport \"testing\"\n\nfunc TestHidden(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatal(Value())\n\t}\n}\n"

// Validation decides a pass by the proof: a reference that passes the verification only because an init exits 0
// (the hidden test never runs) is invalid, on the host and in the sandbox; a reference that passes the test stays
// valid; and the validation records the rule.
func TestValidateNeedsTheProofThatTheHiddenTestsRan(t *testing.T) {
	ctx := context.Background()
	bare, base, ids := goModule(t, nil,
		map[string]string{"value_test.go": hiddenGoTest, "value.go": "package fixture\n\nfunc Value() int { return 1 }\n"},
		map[string]string{"value_test.go": hiddenGoTest, "value.go": "package fixture\n\nimport \"os\"\n\nfunc Value() int { return 0 }\n\nfunc init() { os.Exit(0) }\n"})
	for _, mode := range []string{GraderHost, GraderSandbox} {
		v, progress := validator(t, bare)
		v.Grader = mode
		if mode == GraderSandbox {
			fake := &fakeSandbox{}
			v.Checkout = func(context.Context, string, string, []string, string) (CheckoutCommands, error) {
				return CheckoutCommands{Isolated: fake.run}, nil
			}
		}
		spec := Spec{Base: base, Solution: ids[0], HiddenTests: []string{"value_test.go"}, Reference: []string{"value.go"}, Verify: []string{"go test -count=1 ./..."}}
		good, err := v.Validate(ctx, spec, []Arm{{Name: "base"}})
		if err != nil || good.Status != StatusValid || good.PassRule != PassGoTests {
			t.Fatalf("%s: a reference that passes the test: %v, %v, %s\n%s", mode, good.Summary(), err, good.PassRule, progress)
		}
		ref := good.Stages[1]
		if ref.Stage != StageReference || !ref.Proof.Proven() || ref.Proof.Tests != 1 || good.Stages[0].Proof != nil {
			t.Errorf("%s: stages %+v", mode, good.Stages)
		}
		if events, err := os.ReadFile(filepath.Join(v.LogDir, "base-reference.proof.jsonl")); err != nil || !strings.Contains(string(events), `"Test":"TestHidden"`) {
			t.Errorf("%s: the proof's events: %v\n%s", mode, err, events)
		}

		v, progress = validator(t, bare)
		v.Grader = mode
		if mode == GraderSandbox {
			fake := &fakeSandbox{}
			v.Checkout = func(context.Context, string, string, []string, string) (CheckoutCommands, error) {
				return CheckoutCommands{Isolated: fake.run}, nil
			}
		}
		spec.Solution = ids[1]
		exits, err := v.Validate(ctx, spec, []Arm{{Name: "base"}})
		if err != nil || exits.Status != StatusInvalid || !strings.Contains(exits.Summary(), "base/reference: the hidden tests did not run") {
			t.Fatalf("%s: a reference whose init exits 0: %v, %v\n%s", mode, exits.Summary(), err, progress)
		}
		if !strings.Contains(progress.String(), "NOT OK (the hidden tests did not run)") {
			t.Errorf("%s: progress:\n%s", mode, progress)
		}
		if p := exits.Stages[1].Proof; p == nil || p.Commands[0].ExitCode != 0 ||
			!reflect.DeepEqual(p.Missing, []MissingTest{{Package: ".", Test: "TestHidden", Saw: SawNone}}) {
			t.Errorf("%s: the proof: %+v", mode, p)
		}
		log, _ := os.ReadFile(exits.Stages[1].Log)
		if !strings.Contains(string(log), "$ go test -json -count=1 -run '^(TestHidden)$' .\n") || !strings.Contains(string(log), "[agentium] the hidden tests did not run") {
			t.Errorf("%s: the stage's log:\n%s", mode, log)
		}
	}
}

// Hidden Go test files without a test function cannot be proven: the exit codes grade the task, and validation warns.
func TestValidateWarnsWhenTheHiddenGoTestsHoldNothingToProve(t *testing.T) {
	bare, base, ids := goModule(t, nil, map[string]string{"helpers_test.go": "package fixture\n\nvar _ = Two()\n",
		"two.go": "package fixture\n\nfunc Two() int { return 2 }\n"})
	v, progress := validator(t, bare)
	got, err := v.Validate(context.Background(), Spec{Base: base, Solution: ids[0], HiddenTests: []string{"helpers_test.go"}, Reference: []string{"two.go"},
		Verify: []string{"go vet ./... && go test -count=1 ./..."}}, []Arm{{Name: "base"}})
	if err != nil || got.Status != StatusValid || got.Stages[1].Proof != nil || len(got.Warnings) != 1 ||
		!strings.Contains(got.Warnings[0], "Agentium cannot prove they ran") || !strings.Contains(progress.String(), "cannot prove they ran") {
		t.Errorf("%v, %v, %v\n%s", got.Summary(), got.Warnings, err, progress)
	}
}

// The weak-tests check counts a try as passing only with the proof: without the second hunk the init exits 0 before
// the hidden test runs, which the exit codes alone would call a pass (an untested hunk).
func TestWeakTestsNeedTheProof(t *testing.T) {
	gate := func(value, exitAt int) string {
		return "package fixture\n\nimport \"os\"\n\nfunc Value() int { return " + string(rune('0'+value)) + " }\n\n// apart\n\nfunc init() {\n\tif Value() == " +
			string(rune('0'+exitAt)) + " {\n\t\tos.Exit(0)\n\t}\n}\n"
	}
	bare, base, ids := goModule(t, map[string]string{"value.go": gate(0, 1)}, map[string]string{"value_test.go": hiddenGoTest, "value.go": gate(1, 2)})
	v, progress := validator(t, bare)
	v.WeakTests = true
	got, err := v.Validate(context.Background(), Spec{Base: base, Solution: ids[0], HiddenTests: []string{"value_test.go"}, Reference: []string{"value.go"},
		Verify: []string{"go test -count=1 ./..."}}, []Arm{{Name: "base"}})
	if err != nil || got.Status != StatusValid || got.WeakTests == nil || got.WeakTests.Checked != 2 || len(got.WeakTests.Untested) != 0 {
		t.Fatalf("%v, %+v, %v\n%s", got.Summary(), got.WeakTests, err, progress)
	}
	if log, _ := os.ReadFile(filepath.Join(v.LogDir, "base-weak-2.log")); !strings.Contains(string(log), "[agentium] the hidden tests did not run") {
		t.Errorf("the second hunk's try:\n%s", log)
	}
}

func TestPassRules(t *testing.T) {
	if PassRuleOf("") != PassExitCode || !KnownPassRule("") || !KnownPassRule(PassGoTests) || KnownPassRule("go-tests-v9") {
		t.Error("rules")
	}
	if !HasGoTestFiles([]string{"a.txt", "x/y_test.go"}) || HasGoTestFiles([]string{"tests/value_test.sh"}) {
		t.Error("Go test files")
	}
}

// A folder the go tool's wildcards skip still runs when a verify command names it, so its hidden tests are proven: an
// init that exits 0 there is caught, and a correct reference passes.
func TestValidateProvesAFolderAVerifyCommandNames(t *testing.T) {
	pTest := "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatal(Value())\n\t}\n}\n"
	bare, base, ids := goModule(t, map[string]string{"testdata/p/p.go": "package p\n\nfunc Value() int { return 0 }\n"},
		map[string]string{"testdata/p/p_test.go": pTest, "testdata/p/p.go": "package p\n\nfunc Value() int { return 1 }\n"},
		map[string]string{"testdata/p/p_test.go": pTest, "testdata/p/p.go": "package p\n\nimport \"os\"\n\nfunc Value() int { return 0 }\n\nfunc init() { os.Exit(0) }\n"})
	// Named as a package, as -C's folder (no package argument), and after a debug flag that takes a value.
	for _, verify := range []string{"go test -count=1 ./testdata/p", "go test -C ./testdata/p -count=1", "go test -debug-trace=trace.json -count=1 ./testdata/p"} {
		spec := Spec{Base: base, HiddenTests: []string{"testdata/p/p_test.go"}, Reference: []string{"testdata/p/p.go"}, Verify: []string{verify}}
		spec.Solution = ids[0]
		v, progress := validator(t, bare)
		good, err := v.Validate(context.Background(), spec, []Arm{{Name: "base"}})
		if err != nil || good.Status != StatusValid || !good.Stages[1].Proof.Proven() {
			t.Fatalf("%s: a correct reference: %v, %v\n%s", verify, good.Summary(), err, progress)
		}
		spec.Solution = ids[1]
		v, progress = validator(t, bare)
		exits, err := v.Validate(context.Background(), spec, []Arm{{Name: "base"}})
		if err != nil || exits.Status != StatusInvalid || !strings.Contains(exits.Summary(), NoteHiddenTestsNotRun) {
			t.Errorf("%s: an init that exits 0 in testdata/p: %v, %v\n%s", verify, exits.Summary(), err, progress)
		}
	}
}

// A documentation example whose text holds a line like an output comment is not run by Go, and not looked for.
func TestValidateTakesADocumentationExampleAsGoDoes(t *testing.T) {
	test := hiddenGoTest + "\nfunc ExampleValue() {\n\t_ = `\n// Output: 1\n`\n}\n"
	bare, base, ids := goModule(t, nil, map[string]string{"value_test.go": test, "value.go": "package fixture\n\nfunc Value() int { return 1 }\n"})
	v, progress := validator(t, bare)
	got, err := v.Validate(context.Background(), Spec{Base: base, Solution: ids[0], HiddenTests: []string{"value_test.go"}, Reference: []string{"value.go"},
		Verify: []string{"go test -count=1 ./..."}}, []Arm{{Name: "base"}})
	if err != nil || got.Status != StatusValid || got.Stages[1].Proof.Tests != 1 {
		t.Errorf("%v, %+v, %v\n%s", got.Summary(), got.Stages, err, progress)
	}
}

// go test's arguments are read as go reads them: a flag's value is never a package, and what follows the package list
// once a flag, -args or -- came after it is the test binary's; after a flag go test does not know, later words count.
func TestGoTestTargets(t *testing.T) {
	for command, want := range map[string][]goTarget{
		"go test -outputdir ./testdata/p ./...":                    {{pkg: "./..."}},
		"go test -outputdir=./testdata/p ./testdata/q":             {{pkg: "./testdata/q"}},
		"go test --coverprofile ./testdata/c.out -count=1 ./a ./b": {{pkg: "./a"}, {pkg: "./b"}},
		"go test -C sub -test.run X ./testdata/p -v ./other":       {{dir: "sub", pkg: "./testdata/p"}},
		"go test ./a -args ./b":                                    {{pkg: "./a"}},
		"go test ./a -- ./b":                                       {{pkg: "./a"}},
		// A flag go test does not know: what follows it may be a package, so it is listed (the doubt shows).
		"go test -myflag ./testdata/p":       {{pkg: "./testdata/p"}},
		"go test ./a -myflag x ./testdata/p": {{pkg: "./a"}, {pkg: "x"}, {pkg: "./testdata/p"}},
		// No package: the folder go test runs in, -C's when given.
		"go test -C ./testdata/p": {{dir: "./testdata/p", pkg: "."}},
		"go test -v -count=1":     {{pkg: "."}},
		// cmd/go's debug flags take a value.
		"go test -debug-actiongraph=graph.json ./testdata/p":           {{pkg: "./testdata/p"}},
		"go test -debug-runtime-trace rt.out ./testdata/p":             {{pkg: "./testdata/p"}},
		"go test -debug-trace trace.json ./testdata/p":                 {{pkg: "./testdata/p"}},
		"GOPROXY=off go test -race ./a && go test -tags it ./testdata": {{pkg: "./a"}, {pkg: "./testdata"}},
	} {
		if got := goTestTargets(command); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, want %+v", command, got, want)
		}
	}
}

// A folder named only as a flag's value is not run, so its hidden tests are not proven; a file whose own name starts
// with "_" or "." is never built, even in a package a verify command names, so it is never proven either.
func TestPlanGoProofLeavesOutWhatGoNeverRuns(t *testing.T) {
	test := func(pkg, name string) string {
		return "package " + pkg + "\n\nimport \"testing\"\n\nfunc " + name + "(t *testing.T) {}\n"
	}
	solution := snapSource{"testdata/p/p_test.go": test("p", "TestP"), "_fixture_test.go": test("root", "TestUnderscore"),
		".fixture_test.go": test("root", "TestDot"), "a/_x_test.go": test("a", "TestA"), "value_test.go": test("root", "TestValue")}
	hidden := []string{"testdata/p/p_test.go", "_fixture_test.go", ".fixture_test.go", "a/_x_test.go", "value_test.go"}
	for _, verify := range []string{"go test -outputdir ./testdata/p ./...", "go test . ./a", "go test -C a . && go test ."} {
		proof, _ := PlanGoProof(Spec{HiddenTests: hidden, Verify: []string{verify}}, snapSource{}, solution)
		if want := (GoProof{Packages: []ProofPackage{{Dir: ".", Tests: []string{"TestValue"}}}}); !reflect.DeepEqual(proof, want) {
			t.Errorf("%s: %+v", verify, proof)
		}
	}
	if !namedGoPackage([]string{"go test -C sub ./testdata/p"}, "svc", "svc/sub/testdata/p") || namedGoPackage([]string{"go test -C sub ./testdata/p"}, "svc", "svc/testdata/p") {
		t.Error("-C names folders from its own")
	}
}
