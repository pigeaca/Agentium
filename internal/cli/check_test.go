package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/term"
)

// The check commands keep a project's rule checks: their text and JSON, a duplicate and a missing name (failures), and
// every mistake in how they are called (usage errors).
func TestCheckCommands(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, t.TempDir()+"/data")
	ctx := context.Background()

	expect(t, f.run(ctx, "check", "list"), ExitOK, "No rule checks yet", "agentium check add NAME --ran TEXT")
	expect(t, f.run(ctx, "check", "add", "ran-tests", "--ran", "go test"), ExitOK, "Added check ran-tests", `ran a command containing "go test"`)
	expect(t, f.run(ctx, "check", "add", "touched-tests", "--changed", "**/*_test.go"), ExitOK, "changed a file matching **/*_test.go")
	expect(t, f.run(ctx, "check", "add", "--not-changed", ".agents/**", "kept-docs"), ExitOK, "changed no file matching .agents/**") // the flag first
	list := f.run(ctx, "check", "list")
	expect(t, list, ExitOK, "NAME", "KIND", "PATTERN", "ran-tests", "not-changed", ".agents/**")
	if strings.Index(list.stdout, "ran-tests") > strings.Index(list.stdout, "kept-docs") {
		t.Errorf("checks are listed oldest first:\n%s", list.stdout)
	}

	// A name that exists, and one rm does not find, are failures.
	expect(t, f.run(ctx, "check", "add", "ran-tests", "--ran", "make"), ExitError, "already exists")
	expect(t, f.run(ctx, "check", "rm", "nope"), ExitError, "not found")

	for name, args := range map[string][]string{
		"no subcommand":       {"check"},
		"unknown":             {"check", "show"},
		"no name":             {"check", "add", "--ran", "x"},
		"two names":           {"check", "add", "a", "b", "--ran", "x"},
		"a bad name":          {"check", "add", "Not Valid", "--ran", "x"},
		"no flag":             {"check", "add", "a"},
		"two flags":           {"check", "add", "a", "--ran", "x", "--changed", "y"},
		"the same flag twice": {"check", "add", "a", "--ran", "x", "--ran", "y"},
		"an empty value":      {"check", "add", "a", "--ran", ""},
		"an empty glob":       {"check", "add", "a", "--not-changed", ""},
		"an unknown flag":     {"check", "add", "a", "--where", "x"},
		"list with a name":    {"check", "list", "x"},
		"rm without a name":   {"check", "rm"},
	} {
		if got := f.run(ctx, args...); got.code != ExitUsage {
			t.Errorf("%s (%v): exit %d, want 2:\n%s%s", name, args, got.code, got.stdout, got.stderr)
		}
	}
	if got := f.run(ctx, "check", "add", "-h"); got.code != ExitOK || !strings.Contains(got.stdout, "agentium check add NAME") {
		t.Errorf("check add -h: %d %s", got.code, got.stdout)
	}
	if got := f.run(ctx, "help"); !strings.Contains(got.stdout, "check ") || !strings.Contains(got.stdout, "rule of yours") {
		t.Errorf("help does not name check:\n%s", got.stdout)
	}

	expect(t, f.run(ctx, "check", "rm", "ran-tests"), ExitOK, "Removed check ran-tests")
	if got := f.run(ctx, "check", "list"); strings.Contains(got.stdout, "ran-tests") || !strings.Contains(got.stdout, "touched-tests") {
		t.Errorf("after rm:\n%s", got.stdout)
	}
}

// The same commands with --json: one document each, exit codes unchanged.
func TestCheckCommandsJSON(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, t.TempDir()+"/data")
	empty := jsonRun(t, f, ExitOK, "check", "list")
	assertKeys(t, empty.doc, "checks,command,schema")
	if got := empty.get("checks").([]any); len(got) != 0 {
		t.Errorf("no checks: %s", empty.stdout)
	}
	added := jsonRun(t, f, ExitOK, "check", "add", "ran-tests", "--ran", "go test")
	assertKeys(t, added.doc, "check,command,schema")
	assertKeys(t, added.get("check"), "kind,name,pattern")
	if added.get("check", "kind") != "ran" || added.get("check", "pattern") != "go test" {
		t.Errorf("check add: %s", added.stdout)
	}
	jsonRun(t, f, ExitOK, "check", "add", "quiet", "--not-changed", "docs/**")
	list := jsonRun(t, f, ExitOK, "check", "list")
	if got := list.get("checks").([]any); len(got) != 2 || got[1].(map[string]any)["kind"] != "not-changed" {
		t.Errorf("check list: %s", list.stdout)
	}
	removed := jsonRun(t, f, ExitOK, "check", "rm", "quiet")
	assertKeys(t, removed.doc, "command,removed,schema")
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"check", "add", "ran-tests", "--ran", "x"}, ExitError},
		{[]string{"check", "rm", "quiet"}, ExitError},
		{[]string{"check", "add", "a"}, ExitUsage},
		{[]string{"check", "add", "a", "--ran", ""}, ExitUsage},
	} {
		got := jsonRun(t, f, c.code, c.args...)
		assertKeys(t, got.get("error"), "code,message")
		if got.get("error", "code") != float64(c.code) {
			t.Errorf("%v: %s", c.args, got.stdout)
		}
	}
}

// A report counts the checks in all its formats, from the stored runs of experiments that ran before the checks existed;
// a project without checks gets none of it (its golden tests are the other reports'): JSON says [] and Markdown has no
// section.
func TestExperimentReportRuleChecks(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--seed", "5"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "lean-ab", "--budget", "30"), ExitOK)

	before := f.run(ctx, "experiment", "report", "lean-ab")
	if strings.Contains(before.stdout, "Rule checks") {
		t.Errorf("a project without checks:\n%s", before.stdout)
	}
	var doc struct {
		Checks []any `json:"checks"`
	}
	if got := f.run(ctx, "experiment", "report", "lean-ab", "--json"); json.Unmarshal([]byte(got.stdout), &doc) != nil || doc.Checks == nil || len(doc.Checks) != 0 {
		t.Errorf("the JSON of a project without checks must hold checks: []:\n%.300s", got.stdout)
	}

	expect(t, f.run(ctx, "check", "add", "changed-value", "--changed", "value.txt"), ExitOK)
	expect(t, f.run(ctx, "check", "add", "kept-docs", "--not-changed", "docs/**"), ExitOK)
	expect(t, f.run(ctx, "check", "add", "never-ran", "--ran", "no such command anywhere"), ExitOK)
	md := f.run(ctx, "experiment", "report", "lean-ab")
	expect(t, md, ExitOK, "## Rule checks", "not a verdict", "| `changed-value` | changed a file matching value.txt | 3 of 3 | 3 of 3 |",
		"| `kept-docs` | changed no file matching docs/** | 3 of 3 | 3 of 3 |")
	if !strings.Contains(md.stdout, "| `never-ran` | ran a command containing \"no such command anywhere\" |") {
		t.Errorf("the ran check is missing:\n%s", md.stdout)
	}
	if strings.Replace(md.stdout, md.stdout[strings.Index(md.stdout, "\n## Rule checks"):strings.Index(md.stdout, "\n## Per task")], "", 1) != before.stdout {
		t.Error("the rest of the Markdown report changed with checks")
	}

	js := f.run(ctx, "experiment", "report", "lean-ab", "--json")
	var full struct {
		Checks []struct {
			Name, Kind, Pattern string
			Arms                map[string]struct{ Met, Counted, Unread int }
		}
	}
	if err := json.Unmarshal([]byte(js.stdout), &full); err != nil || len(full.Checks) != 3 {
		t.Fatalf("the JSON checks: %v\n%.400s", err, js.stdout)
	}
	if c := full.Checks[0]; c.Name != "changed-value" || c.Arms["A"].Met != 3 || c.Arms["B"].Counted != 3 || c.Arms["A"].Unread != 0 {
		t.Errorf("changed-value: %+v", c)
	}

	*f.terminal = true
	designed := term.Plain(f.run(ctx, "experiment", "report", "lean-ab").stdout)
	if !strings.Contains(designed, "what the agents did") || !strings.Contains(designed, "changed-value") || !strings.Contains(designed, "3 of 3") ||
		!strings.Contains(designed, "not a verdict") {
		t.Errorf("the designed report lacks the block:\n%s", designed)
	}
	details := term.Plain(f.run(ctx, "experiment", "report", "lean-ab", "--details").stdout)
	if !strings.Contains(details, "Rule checks") || !strings.Contains(details, "kept-docs") {
		t.Errorf("--details lacks the checks:\n%s", details)
	}
	// Removing the checks takes them out of the next report.
	*f.terminal = false
	for _, n := range []string{"changed-value", "kept-docs", "never-ran"} {
		expect(t, f.run(ctx, "check", "rm", n), ExitOK)
	}
	if after := f.run(ctx, "experiment", "report", "lean-ab"); after.stdout != before.stdout {
		t.Error("a report after the checks are removed is not the report before them")
	}
}
