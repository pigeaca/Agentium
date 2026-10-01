package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/store"
)

// startRepo is a repository for start: `make test` runs the shell tests, and features commits each add a function with
// its test, so each is a task candidate.
func startRepo(t *testing.T, features int) string {
	t.Helper()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "Makefile", "test:\n\tsh run_tests.sh\n")
	writeFile(t, repo, "run_tests.sh", "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n")
	writeFile(t, repo, "CLAUDE.md", "# Rules\n")
	writeFile(t, repo, "lib.sh", "base() { echo base; }\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "Initial commit")
	for i := 1; i <= features; i++ {
		lib, err := os.ReadFile(filepath.Join(repo, "lib.sh"))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, repo, "lib.sh", string(lib)+fmt.Sprintf("f%d() { echo v%d; }\n", i, i))
		writeFile(t, repo, fmt.Sprintf("tests/f%d_test.sh", i), fmt.Sprintf(". ./lib.sh\n[ \"$(f%d)\" = v%d ]\n", i, i))
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", fmt.Sprintf("Add f%d to the library\n\nThe f%d function prints v%d for the welcome screen.", i, i, i))
	}
	return repo
}

// startFixture is startRepo with a data folder and the fake Claude Code; nothing is registered yet.
func startFixture(t *testing.T, features int) (runFixture, string) {
	t.Helper()
	f := runFixtureAt(startRepo(t, features), filepath.Join(t.TempDir(), "data"), t.TempDir())
	ctrl := t.TempDir()
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, ctrl)
	return f, ctrl
}

// paidRuns is how many runs the data folder holds, and the fake agent's start marks in ctrl.
func paidRuns(t *testing.T, f runFixture, ctrl string) (stored, started int) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	runs, err := db.Runs(ctx, projects[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	marks, _ := filepath.Glob(filepath.Join(ctrl, "pid-*"))
	return len(runs), len(marks)
}

func TestStartReachesAPreviewWithoutPromptsOrPaidRuns(t *testing.T) {
	t.Parallel()
	f, ctrl := startFixture(t, 9)
	ctx := context.Background()
	got := f.run(ctx, "start", "--reviewed")
	expect(t, got, ExitOK, "Registered ", "Context: saved snapshot baseline from HEAD", "Mining: ", "imported 8 of 8 tried (verify: make test)",
		"Validating 8 task(s) in 2 context(s)", "8 valid of 8", "Tasks: 8 ready (needs 8), in ", "Experiment quick-aa-baseline: created, 8 task(s) × 1 run per arm = 16 runs",
		"A/A calibration of baseline", "Before it runs:", "Sizes (runs count both arms):", "is not calibrated", "Nothing was run: fix what is missing",
		"agentium experiment run quick-aa-baseline", "First decisive verdict: none yet ($0.00 spent since init)")
	if stored, started := paidRuns(t, f, ctrl); stored != 0 || started != 0 {
		t.Errorf("start without --yes ran the agent: %d stored run(s), %d started", stored, started)
	}
	list := f.run(ctx, "experiment", "list")
	expect(t, list, ExitOK, "quick-aa-baseline", "aa", "baseline / baseline", "8 × 1")

	again := f.run(ctx, "start", "--reviewed")
	expect(t, again, ExitOK, "Project ", "registered (skipped)", "Context: snapshot baseline (skipped)", "Tasks: 8 ready (needs 8) (skipped)",
		"Experiment quick-aa-baseline: exists (skipped)", "Nothing was run")
	for _, not := range []string{"Mining:", "Validating", "Registered ", "created"} {
		if strings.Contains(again.stdout, not) {
			t.Errorf("a second start repeats %q:\n%s", not, again.stdout)
		}
	}
}

func TestStartWithBComparesContexts(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 9)
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nKeep it short.\n")
	expect(t, f.run(ctx, "context", "snapshot", "lean", "--working-tree"), ExitOK)
	gitIn(t, f.repo, "checkout", "--", "CLAUDE.md")

	expect(t, f.run(ctx, "start", "--b", "nope"), ExitError, "snapshot", "nope")
	got := f.run(ctx, "start", "--b", "lean", "--reviewed")
	expect(t, got, ExitOK, "Context: saved snapshot baseline from HEAD", "Validating 8 task(s) in 3 context(s)",
		"Experiment quick-baseline-vs-lean: created, 8 task(s) × 1 run per arm = 16 runs", "baseline", "lean")
	if strings.Contains(got.stdout, "no second context was given") {
		t.Errorf("--b made an A/A:\n%s", got.stdout)
	}
	expect(t, f.run(ctx, "experiment", "list"), ExitOK, "quick-baseline-vs-lean", "context-ab", "baseline / lean")
}

func TestStartYesRunsTheExperiment(t *testing.T) {
	t.Parallel()
	f, ctrl := startFixture(t, 9)
	ctx := context.Background()
	expect(t, f.run(ctx, "start", "--reviewed"), ExitOK)
	// Calibration inside experiment run is a later step: calibrate the baseline here, as the readiness check asks.
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")
	expect(t, f.run(ctx, "run", "calibrate", "--snapshot", "baseline"), ExitOK)
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, ctrl)

	ready := f.run(ctx, "start")
	expect(t, ready, ExitOK, "Nothing was run and nothing was spent. To run it (real Claude Code runs, up to $", "agentium experiment run quick-aa-baseline")
	if strings.Contains(ready.stdout, "MISSING") {
		t.Errorf("a calibrated experiment is not ready:\n%s", ready.stdout)
	}
	if stored, started := paidRuns(t, f, ctrl); started != 0 {
		t.Errorf("start without --yes ran %d run(s) (%d stored)", started, stored)
	}
	got := f.run(ctx, "start", "--yes")
	expect(t, got, ExitOK, "Experiment quick-aa-baseline: exists (skipped)", "Before it runs:", "[16/16]", "settled")
	if strings.Contains(got.stdout, "Nothing was run") {
		t.Errorf("--yes stopped at the preview:\n%s", got.stdout)
	}
	if runs := experimentRuns(t, f, "quick-aa-baseline"); len(runs) < 16 {
		t.Errorf("--yes stored %d run(s), want 16", len(runs))
	}
	// The project's spend now shows, and an A/A never counts as a decisive verdict.
	expect(t, f.run(ctx, "start"), ExitOK, "First decisive verdict: none yet ($")
	report := f.run(ctx, "experiment", "report", "quick-aa-baseline", "--markdown")
	expect(t, report, ExitOK, "First decisive verdict: none yet ($")
}

func TestStartWithTooFewTasks(t *testing.T) {
	t.Parallel()
	f, ctrl := startFixture(t, 5)
	ctx := context.Background()
	got := f.run(ctx, "start", "--reviewed", "--yes")
	expect(t, got, ExitError, "Mining: ", "5 valid of 5", "only 5 of the 8 an experiment needs are ready", "the history has no more candidates",
		"agentium task mine --dry-run", "agentium task add")
	if strings.Contains(got.stdout, "Experiment ") || strings.Contains(got.stdout, "Before it runs") {
		t.Errorf("an experiment was made from too few tasks:\n%s", got.stdout)
	}
	if stored, started := paidRuns(t, f, ctrl); stored != 0 || started != 0 {
		t.Errorf("runs happened: %d, %d", stored, started)
	}
	if got := f.run(ctx, "experiment", "list"); !strings.Contains(got.stdout, "No experiments yet") {
		t.Errorf("experiments exist:\n%s", got.stdout)
	}
}

// Mined instructions wait for a person unless --reviewed accepts them.
func TestStartWaitsForReviews(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 9)
	ctx := context.Background()
	got := f.run(ctx, "start")
	expect(t, got, ExitError, "8 valid of 8", "8 valid, 0 ready of the 8 an experiment needs: the others wait for your review", "Read each instruction for solution leaks",
		"agentium task show NAME", "agentium task edit NAME --reviewed", "agentium start --reviewed")
	expect(t, f.run(ctx, "start", "--reviewed"), ExitOK, "Tasks: 8 ready", "Experiment quick-aa-baseline: created")
}

func TestStartUsage(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 1)
	ctx := context.Background()
	expect(t, f.run(ctx, "start", "extra"), ExitUsage, "Usage: agentium start")
	expect(t, f.run(ctx, "start", "--budget", "-1"), ExitUsage, "Usage: agentium start")
	expect(t, f.run(ctx, "start", "--b", "Bad Name"), ExitUsage, "Usage: agentium start")
	expect(t, f.run(ctx, "start", "-h"), ExitOK, "Usage: agentium start", "--yes", "--budget", "--b SNAPSHOT")
	expect(t, f.run(ctx, "help"), ExitOK, "start ")
}

// The prompt is offered only where a person can answer: stdin and stdout both terminals.
func TestStartPromptOnlyOnATerminal(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                string
		stdinTTY, stdoutTTY bool
		input               string
		want, asked         bool
	}{
		{"terminal, yes", true, true, "y\n", true, true},
		{"terminal, word yes", true, true, "Yes\n", true, true},
		{"terminal, no", true, true, "n\n", false, true},
		{"terminal, enter", true, true, "\n", false, true},
		{"terminal, end of input", true, true, "", false, true},
		{"piped stdin", false, true, "y\n", false, false},
		{"stdout not a terminal", true, false, "y\n", false, false},
		{"neither", false, false, "y\n", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			s := &starter{env: Env{Stdin: strings.NewReader(c.input), StdinTerminal: c.stdinTTY, Terminal: c.stdoutTTY, Stdout: &out}}
			if got := s.confirm(12.5); got != c.want {
				t.Errorf("confirm = %v, want %v", got, c.want)
			}
			if asked := strings.Contains(out.String(), "Run it now?"); asked != c.asked {
				t.Errorf("asked = %v, want %v (output %q)", asked, c.asked, out.String())
			}
		})
	}
	var out strings.Builder
	if (&starter{env: Env{Terminal: true, StdinTerminal: true, Stdout: &out}}).confirm(1) || out.Len() != 0 {
		t.Error("a terminal without a stdin reader was asked")
	}
}
