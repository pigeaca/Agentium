package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

func validationOf(status string) []byte { return []byte(fmt.Sprintf(`{"status":%q}`, status)) }

// Each row of the table of what a task tells, and the edges between the rows.
func TestTellOf(t *testing.T) {
	t.Parallel()
	valid := store.Task{Validation: validationOf("valid")}
	with := func(f func(*store.Task)) store.Task { c := valid; f(&c); return c }
	for _, tc := range []struct {
		name    string
		task    store.Task
		gaps    bool
		n, k    int
		want    string
		words   string
		blocked bool
	}{
		{"retired beats everything", with(func(t *store.Task) {
			t.RetiredAt = time.Now()
			t.Validation = validationOf("invalid")
			t.NeedsReview = true
		}), true, 5, 2, "retired", "retired", true},
		{"invalid", with(func(t *store.Task) { t.Validation = validationOf("invalid"); t.NeedsReview = true }), false, 4, 2, "invalid", "invalid", true},
		{"unreadable validation is invalid", with(func(t *store.Task) { t.Validation = []byte(`not json`) }), false, 0, 0, "invalid", "invalid", true},
		{"flaky", with(func(t *store.Task) { t.Validation = validationOf("flaky") }), false, 0, 0, "flaky", "flaky", true},
		{"unchecked", with(func(t *store.Task) { t.Validation = validationOf("unchecked") }), false, 0, 0, "unchecked", "no solution to check with", true},
		{"never validated", with(func(t *store.Task) { t.Validation = nil; t.NeedsReview = true }), true, 3, 1, "not-validated", "not validated", true},
		{"unstated requirements", with(func(t *store.Task) { t.NeedsReview = true }), true, 3, 1, "unstated-requirements", "unstated requirements", true},
		{"a gap check that fails is no gaps", with(func(t *store.Task) { t.NeedsReview = true }), false, 3, 1, "not-reviewed", "not reviewed", true},
		{"a reviewed task's gaps are accepted", valid, true, 0, 0, "not-run", "not run yet", false},
		{"not run", valid, false, 0, 0, "not-run", "not run yet", false},
		{"0 of 1", valid, false, 1, 0, "few-runs", "passed 0 of 1", false},
		{"0 of 2", valid, false, 2, 0, "never-passed", "never passed: check the task text", false},
		{"0 of 7", valid, false, 7, 0, "never-passed", "never passed: check the task text", false},
		{"1 of 1", valid, false, 1, 1, "few-runs", "passed 1 of 1", false},
		{"1 of 2", valid, false, 2, 1, "few-runs", "passed 1 of 2", false},
		{"3 of 3", valid, false, 3, 3, "few-runs", "passed 3 of 3", false},
		{"4 of 4", valid, false, 4, 4, "too-easy", "always passes: too easy", false},
		{"3 of 4", valid, false, 4, 3, "separates", "separates", false},
		{"0 of 4 never passed", valid, false, 4, 0, "never-passed", "never passed: check the task text", false},
		{"12 of 12", valid, false, 12, 12, "too-easy", "always passes: too easy", false},
	} {
		got := tellOf(tc.task, tc.gaps, tc.n, tc.k)
		if got != tc.want || tellWords(got, tc.n, tc.k) != tc.words || blocked(got) != tc.blocked {
			t.Errorf("%s: %q, %q, blocked %v; want %q, %q, %v", tc.name, got, tellWords(got, tc.n, tc.k), blocked(got), tc.want, tc.words, tc.blocked)
		}
	}
}

func marksOf(s string) []bool {
	var out []bool
	for _, r := range s {
		out = append(out, r == 'p')
	}
	return out
}

// listScene holds every tag, a task with more than 8 runs, a module, a long name and a name with an escape.
func listScene() []taskTell {
	valid, review := validationOf("valid"), true
	mk := func(name, module string, v []byte, f func(*store.Task), runs string) taskTell {
		tk := store.Task{Name: name, Module: module, Validation: v}
		if f != nil {
			f(&tk)
		}
		marks := marksOf(runs)
		k := 0
		for _, p := range marks {
			if p {
				k++
			}
		}
		return taskTell{Task: tk, N: len(marks), K: k, Marks: marks, Tell: tellOf(tk, strings.HasPrefix(name, "gaps"), len(marks), k)}
	}
	return []taskTell{
		mk("retired-old-parser-task", "", valid, func(t *store.Task) { t.RetiredAt = time.Now() }, "pf"),
		mk("feature-intersect-by-653-43ae3", "", validationOf("invalid"), nil, ""),
		mk("flaky-clock-test", "", validationOf("flaky"), nil, ""),
		mk("no-solution-here", "", validationOf("unchecked"), nil, ""),
		mk("never-checked", "", nil, nil, ""),
		mk("gaps-in-the-text", "", valid, func(t *store.Task) { t.NeedsReview = review }, ""),
		mk("fix-correct-dropbyindex-handling-of-negative-values", "", valid, func(t *store.Task) { t.NeedsReview = review }, "pf"),
		mk("fix-iter-tuples-support-break-for-early-exit", "", valid, nil, ""),
		mk("fix-nth-reject-indexes-that-do-not-exist", "", valid, nil, "pp"),
		mk("feat-support-for-buffer-iterator-over-streams", "", valid, nil, "ff"),
		mk("feat-add-unionby-and-unionbyerror-helpers", "lib/core", valid, nil, "ppppppppppp"),
		mk("fix-it-mode-align-behavior-with-the-docs", "", valid, nil, "pfpp"),
		mk("feat-add-nthor-and-nthorempty-for-lists", "", valid, nil, "fppfpfppfp"),
		mk("evil\x1b[31m-name", "", valid, nil, "pfpfp"),
	}
}

func TestTaskListView(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for _, width := range []int{term.MinWidth, 80, 120} {
		fmt.Fprintf(&b, "=== %d columns\n", width)
		for _, line := range taskListView(listScene(), plainUnicode, width, "agentium task show NAME") {
			if w := term.Width(line); w > width {
				t.Errorf("a line of %d cells at %d columns: %q", w, width, line)
			}
			b.WriteString(line + "\n")
		}
	}
	checkGolden(t, "tasks-scene.golden", b.String())
	checkGolden(t, "tasks-scene-color.golden", strings.Join(taskListView(listScene(), color256, 80, color256.Style.Command("agentium task show NAME")), "\n")+"\n")
	checkGolden(t, "tasks-scene-ascii.golden", strings.Join(taskListView(listScene(), plainASCII, 80, "agentium task show NAME"), "\n")+"\n")

	v := strings.Join(taskListView(listScene(), plainUnicode, 100, "agentium task show NAME"), "\n")
	for _, want := range []string{"tasks · 14", "what it tells you", "+3 ✓✓✓✓✓✓✓✓", "module", "lib/core", "never passed: check the task text",
		"3 separate · 1 too easy · 1 to check · 1 with few runs · 1 not run yet · 6 not ready · 1 retired", "agentium task show NAME: a task's text"} {
		if !strings.Contains(v, want) {
			t.Errorf("want %q in:\n%s", want, v)
		}
	}
	if strings.Contains(v, "\x1b") {
		t.Errorf("an escape in the plain list:\n%s", v)
	}
	for _, jargon := range []string{"arm", "interval", "eligible"} {
		if strings.Contains(v, jargon) {
			t.Errorf("the list says %q:\n%s", jargon, v)
		}
	}
	// The rows come ordered by tag, with the retired one last; a task's marks stay whole at the narrowest width.
	if i, j := strings.Index(v, "separates"), strings.Index(v, "retired"); i < 0 || j < i {
		t.Errorf("the order:\n%s", v)
	}
	if !strings.Contains(strings.Join(taskListView(listScene(), plainUnicode, term.MinWidth, "x"), "\n"), "+3 ✓✓✓✓✓✓✓✓") {
		t.Errorf("a mark was cut at the narrowest width")
	}
}

func TestTellCountsLine(t *testing.T) {
	t.Parallel()
	c := countTells(listScene())
	if got, want := strings.ReplaceAll(c.line(unicodeMarks), nbsp, " "), "3 separate · 1 too easy · 1 to check · 1 with few runs · 1 not run yet · 6 not ready · 1 retired"; got != want {
		t.Errorf("counts line %q, want %q", got, want)
	}
	if got := (tellCounts{}).line(unicodeMarks); got != "" {
		t.Errorf("no tasks: %q", got)
	}
}

// taskListFixture is the experiment fixture's project with tasks and runs for each case, stored directly.
func taskListFixture(t *testing.T) runFixture {
	t.Helper()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	for _, name := range []string{"sep", "easy", "never", "few", "fresh", "retired"} {
		expect(t, f.run(ctx, "task", "add", name, "--base", "HEAD", "--instruction", "Do "+name+".", "--verify", "true"), ExitOK)
		expect(t, f.run(ctx, "task", "edit", name, "--reviewed"), ExitOK)
	}
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	tasks, err := db.Tasks(ctx, projects[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tk := range tasks {
		switch tk.Name {
		case "retired":
			if _, err := db.RetireTask(ctx, tk.ID, "test", now); err != nil {
				t.Fatal(err)
			}
		case "value":
		default:
			if _, err := db.SetTaskValidation(ctx, tk.ID, tk.Verify, tk.Setup, validationOf("valid"), now); err != nil {
				t.Fatal(err)
			}
		}
	}
	ids := taskIDs(t, f)
	yes, no := true, false
	run := func(name, outcome string, passed *bool) store.Run {
		return store.Run{TaskID: ids[name], TaskName: name, Outcome: outcome, Passed: passed}
	}
	var runs []store.Run
	for i := 0; i < 3; i++ { // sep: 3 of 5 fair and graded, plus runs that do not count
		runs = append(runs, run("sep", "ok", &yes))
	}
	runs = append(runs, run("sep", "ok", &no), run("sep", "timeout", &no), run("sep", "infra", &yes), run("sep", "ok", nil))
	for i := 0; i < 4; i++ {
		runs = append(runs, run("easy", "ok", &yes))
	}
	runs = append(runs, run("never", "ok", &no), run("never", "capped", &no), run("few", "ok", &yes))
	saveRuns(t, f, runs...)
	return f
}

func TestTaskListOnATerminal(t *testing.T) {
	t.Parallel()
	f := taskListFixture(t)
	ctx := context.Background()
	piped := f.run(ctx, "task", "list")
	expect(t, piped, ExitOK, "GRADED BY", "STATUS")
	for _, designedOnly := range []string{"what it tells you", "separates"} {
		if strings.Contains(piped.stdout, designedOnly) {
			t.Errorf("the plain table has %q:\n%s", designedOnly, piped.stdout)
		}
	}
	*f.terminal = true
	designed := f.run(ctx, "task", "list")
	plain := term.Plain(designed.stdout)
	expect(t, cliResult{designed.code, plain, designed.stderr}, ExitOK, "what it tells you", "separates", "always passes: too easy",
		"never passed: check the task text", "passed 1 of 1", "not run yet", "retired",
		"1 separate", "1 too easy", "1 to check", "1 with few runs", "2 not run yet", "1 retired", "agentium task show NAME")
	if !strings.Contains(plain, "✓✓✓✗✗") && !strings.Contains(plain, "+++xx") {
		t.Errorf("the runs' marks are missing:\n%s", plain)
	}
	if strings.Contains(plain, "GRADED BY") {
		t.Errorf("the designed list has the table:\n%s", plain)
	}
	details := f.run(ctx, "task", "list", "--details")
	if details.code != ExitOK || term.Plain(details.stdout) != piped.stdout {
		t.Errorf("--details on a terminal differs from the plain table:\n%s", details.stdout)
	}
	f.vars["NO_COLOR"] = "1"
	if got := f.run(ctx, "task", "list"); got.stdout != piped.stdout {
		t.Errorf("NO_COLOR differs from the plain table:\n%s", got.stdout)
	}
	delete(f.vars, "NO_COLOR")
	f.vars["COLUMNS"] = "40"
	if got := f.run(ctx, "task", "list"); term.Plain(got.stdout) != piped.stdout {
		t.Errorf("a narrow terminal differs from the plain table:\n%s", got.stdout)
	}
	delete(f.vars, "COLUMNS")
	expect(t, f.run(ctx, "task", "list", "extra"), ExitUsage)
	expect(t, f.run(ctx, "task", "list", "--nope"), ExitUsage)
	*f.terminal = false
	expect(t, f.run(ctx, "task", "list", "--details"), ExitOK, "GRADED BY")

	pool := f.run(ctx, "pool", "status")
	if strings.Contains(pool.stdout, "separate") {
		t.Errorf("plain pool status has the count line:\n%s", pool.stdout)
	}
	*f.terminal = true
	expect(t, cliResult{0, term.Plain(f.run(ctx, "pool", "status").stdout), ""}, ExitOK, "task pool", "1 separate", "1 too easy")
}

func TestTaskListJSONTells(t *testing.T) {
	t.Parallel()
	f := taskListFixture(t)
	ctx := context.Background()
	res := f.run(ctx, "task", "list", "--json")
	expect(t, res, ExitOK)
	var doc struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]any{ // graded, passed, tells
		"sep": {5.0, 3.0, "separates"}, "easy": {4.0, 4.0, "too-easy"}, "never": {2.0, 0.0, "never-passed"},
		"few": {1.0, 1.0, "few-runs"}, "fresh": {0.0, 0.0, "not-run"}, "retired": {0.0, 0.0, "retired"}, "value": {0.0, 0.0, "not-run"},
	}
	seen := 0
	for _, tk := range doc.Tasks {
		name := tk["name"].(string)
		w, ok := want[name]
		if !ok {
			continue
		}
		seen++
		if tk["graded_runs"] != w[0] || tk["passed_runs"] != w[1] || tk["tells"] != w[2] {
			t.Errorf("%s: graded %v passed %v tells %v, want %v", name, tk["graded_runs"], tk["passed_runs"], tk["tells"], w)
		}
	}
	if seen != len(want) {
		t.Errorf("saw %d of %d tasks in %s", seen, len(want), res.stdout)
	}
	// The other task documents keep their keys.
	shown := f.run(ctx, "task", "show", "sep", "--json")
	if strings.Contains(shown.stdout, "graded_runs") || strings.Contains(shown.stdout, `"tells"`) {
		t.Errorf("task show has the list's keys:\n%s", shown.stdout)
	}
}
