package task

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestGoTestFiltersReadsShellCommands(t *testing.T) {
	t.Parallel()
	for command, want := range map[string][]goTestFilter{
		`go test ./... -skip 'TestSlow|TestNet'`:              {{skip: "TestSlow|TestNet"}},
		`go test -run=^TestA$ ./pkg`:                          {{run: "^TestA$"}},
		`go test -run "^TestA$" -skip TestB/sub ./...`:        {{run: "^TestA$", skip: "TestB/sub"}},
		`cd sub && /usr/local/go/bin/go test --run X; go vet`: {{run: "X"}},
		`go test -test.skip=TestX -run TestY -run TestZ`:      {{run: "TestZ", skip: "TestX"}}, // the last -run wins
		`go test ./... -args -run TestX`:                      {{}},                            // the test binary's
		`go test -run "$PATTERN" ./...`:                       {{}},                            // unknown here: as if it ran everything
		`go test -run "Test$(echo A)"`:                        {{}},
		`go test ./...`:                                       {{}},
		`go vet -run X`:                                       nil,
		`go test -run 'unterminated`:                          nil,
		`make test # go test -run X`:                          nil,
		"go test -skip TestA && go test -run TestB":           {{skip: "TestA"}, {run: "TestB"}},
	} {
		got := goTestFilters(command)
		for i := range got {
			got[i].command = ""
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: %+v, want %+v", command, got, want)
		}
	}
	if p := splitPattern("Test(A/B)/sub[/]x"); !slices.Equal(p, []string{"Test(A/B)", "sub[/]x"}) {
		t.Errorf("splitPattern = %q", p)
	}
}

// The smoke check's case: a verify command skips a pattern that one of the task's own new tests matches. Only tests the
// solution adds or changes count; a skip pattern with a subtest element skips subtests only; -run leaves out the
// tests it does not match.
func TestFilteredHiddenTests(t *testing.T) {
	t.Parallel()
	base := snapSource{"lo_test.go": "package lo\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n\nfunc TestSlowOld(t *testing.T) {}\n"}
	solution := snapSource{
		"lo_test.go": "package lo\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n\nfunc TestSlowOld(t *testing.T) {}\n\n" +
			"func TestUnionBy(t *testing.T) { t.Log(1) }\n\nfunc TestSlowUnionBy(t *testing.T) {}\n\nfunc Testing(t *testing.T) {}\n\nfunc helper() {}\n",
		"new_test.go": "package lo\n\nimport \"testing\"\n\nfunc FuzzUnion(f *testing.F) {}\n\nfunc ExampleUnionBy() {\n\tprintln(1)\n\t// Output: 1\n}\n\n" +
			"func ExampleNoOutput() {}\n\nfunc TestMain(m *testing.M) { m.Run() }\n", // TestMain and an example without output never run as tests
		"broken_test.go": "package lo\n\nfunc TestBroken(",
		"testdata/x.txt": "data",
	}
	hidden := []string{"broken_test.go", "lo_test.go", "new_test.go", "testdata/x.txt"}
	if got := ownGoTests(hidden, base, solution); !slices.Equal(got, []string{"ExampleUnionBy", "FuzzUnion", "TestSlowUnionBy", "TestUnionBy"}) { // no TestMain, no ExampleNoOutput
		t.Errorf("own tests = %v", got)
	}

	warnings := FilteredHiddenTests([]string{"go test ./... -skip 'Slow'"}, hidden, base, solution)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no `go test` in the verify commands runs hidden test(s) TestSlowUnionBy: `go test ./... -skip 'Slow'` (-skip \"Slow\") filter(s) them out") ||
		strings.Contains(warnings[0], "TestSlowOld") {
		t.Errorf("skip: %q", warnings)
	}
	warnings = FilteredHiddenTests([]string{"go build ./...", "go test -run 'Union' ./..."}, hidden, base, solution)
	if len(warnings) != 0 {
		t.Errorf("-run matching every own test: %q", warnings)
	}
	warnings = FilteredHiddenTests([]string{"go test -run '^TestUnionBy$' ./..."}, hidden, base, solution)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "runs hidden test(s) ExampleUnionBy, FuzzUnion, TestSlowUnionBy: `go test -run '^TestUnionBy$' ./...` (-run \"^TestUnionBy$\")") {
		t.Errorf("run: %q", warnings)
	}
	// A test one invocation filters out and another runs is graded: the warning is for tests no invocation runs.
	for _, verify := range [][]string{
		{"go test ./...", "go test -race -run TestConc ./pkg"},
		{"go test -skip 'Slow' ./...", "go test -run 'Slow' ./..."},
		{"go test -run \"$ONLY\" ./...", "go test -skip Slow"},
	} {
		if w := FilteredHiddenTests(verify, hidden, base, solution); len(w) != 0 {
			t.Errorf("%q: %q", verify, w)
		}
	}
	if w := FilteredHiddenTests([]string{"go test -skip 'Slow' ./a", "go test -skip 'SlowUnion|Fuzz' ./b"}, hidden, base, solution); len(w) != 1 ||
		!strings.Contains(w[0], "runs hidden test(s) TestSlowUnionBy: ") || !strings.Contains(w[0], "./a` (-skip \"Slow\") and `go test -skip") {
		t.Errorf("filtered out by both: %q", w)
	}
	for _, quiet := range []string{"go test ./...", "go test -skip 'TestUnionBy/empty' ./...", "go test -skip 'Nothing'", "go test -skip '('"} {
		if w := FilteredHiddenTests([]string{quiet}, hidden, base, solution); len(w) != 0 {
			t.Errorf("%s: %q", quiet, w)
		}
	}
	if w := FilteredHiddenTests([]string{"go test -skip Slow"}, hidden, nil, solution); len(w) != 1 || !strings.Contains(w[0], "TestSlowOld, TestSlowUnionBy") {
		t.Errorf("without a base every test is the solution's: %q", w)
	}
}

// Validation warns of a hidden test the verify command skips, stores the warning, and keeps the status: the task is
// still valid (its other new test fails on the base).
func TestValidateWarnsOfSkippedHiddenTests(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	ctx := context.Background()
	module := map[string]string{
		"go.mod":     "module example.com/lo\n\ngo 1.22\n",
		"lo.go":      "package lo\n\nfunc Value() string { return \"old\" }\n",
		"lo_test.go": "package lo\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n",
	}
	bare, base, ids := history(t, module, map[string]string{
		"lo.go": "package lo\n\nfunc Value() string { return \"new\" }\n",
		"lo_test.go": "package lo\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n\n" +
			"func TestValue(t *testing.T) {\n\tif Value() != \"new\" {\n\t\tt.Fatal(Value())\n\t}\n}\n\n" +
			"func TestSlowValue(t *testing.T) {\n\tif Value() != \"new\" {\n\t\tt.Fatal(Value())\n\t}\n}\n",
	})
	v, progress := validator(t, bare)
	v.Env = []string{"GOFLAGS=-mod=mod", "GOTOOLCHAIN=local"}
	result, err := v.Validate(ctx, Spec{Base: base, Solution: ids[0], HiddenTests: []string{"lo_test.go"}, Reference: []string{"lo.go"},
		Verify: []string{"go test ./... -skip 'TestSlow'"}}, []Arm{{Name: "base"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusValid || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "runs hidden test(s) TestSlowValue") {
		t.Fatalf("status %s, warnings %q, stages %v", result.Status, result.Warnings, stages(result))
	}
	if !strings.Contains(progress.String(), "  warning: no `go test` in the verify commands runs hidden test(s) TestSlowValue: `go test ./... -skip 'TestSlow'`") {
		t.Errorf("progress:\n%s", progress)
	}
}
