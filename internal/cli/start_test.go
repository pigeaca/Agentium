package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
)

// startRepo is a repository for start: `make test` runs the shell tests, and features commits each add a function with
// its test, so each is a task candidate.
func startRepo(t *testing.T, features int) string { return startRepoNaming(t, features, 0) }

// startRepoNaming is startRepo where the first named commit messages name lib.sh, a file of the reference solution.
func startRepoNaming(t *testing.T, features, named int) string {
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
		gitIn(t, repo, "commit", "-q", "-m", fmt.Sprintf("Add f%d to the library\n\nThe f%d function prints v%d for the welcome screen.%s", i, i, i, map[bool]string{true: " It lives in lib.sh.", false: ""}[i <= named]))
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
	got := f.run(ctx, "start", "--accept-mined")
	expect(t, got, ExitOK, "Registered ", "Context: saved snapshot baseline from HEAD", "Mining: ", "imported 8 of 8 tried (verify: make test)",
		"Validating 8 task(s) in 2 context(s)", "8 valid of 8", "Tasks: 8 ready (needs 8), in ", "Experiment quick-aa-baseline: created, 8 task(s) × 1 run per arm = 16 runs",
		"A/A calibration of baseline", "Before it runs:", "Looks (method seq-v1; runs count both arms):", "is not calibrated: calibrated when the experiment runs, about $",
		"Calibration: 1 context calibration(s)", "Nothing was run and nothing was spent. To run it (real Claude Code runs, first 1 calibration run(s) of about $",
		"agentium experiment run quick-aa-baseline", "First decisive verdict: none yet ($0.00 spent since init)")
	if stored, started := paidRuns(t, f, ctrl); stored != 0 || started != 0 {
		t.Errorf("start without --yes ran the agent: %d stored run(s), %d started", stored, started)
	}
	list := f.run(ctx, "experiment", "list")
	expect(t, list, ExitOK, "quick-aa-baseline", "aa", "baseline / baseline", "8 × 1")

	again := f.run(ctx, "start", "--accept-mined")
	expect(t, again, ExitOK, "Project ", "registered (skipped)", "Context: snapshot baseline from ", "Tasks: skipped (the experiment exists)",
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
	got := f.run(ctx, "start", "--b", "lean", "--accept-mined")
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
	expect(t, f.run(ctx, "start", "--accept-mined"), ExitOK)
	// No manual calibration: the experiment calibrates the context itself, after the consent --yes gives.
	ready := f.run(ctx, "start")
	expect(t, ready, ExitOK, "Nothing was run and nothing was spent. To run it (real Claude Code runs, first 1 calibration run(s) of about $", "agentium experiment run quick-aa-baseline")
	if strings.Contains(ready.stdout, "MISSING") {
		t.Errorf("an uncalibrated experiment is not ready:\n%s", ready.stdout)
	}
	if stored, started := paidRuns(t, f, ctrl); started != 0 {
		t.Errorf("start without --yes ran %d run(s) (%d stored)", started, stored)
	}
	got := f.run(ctx, "start", "--yes")
	expect(t, got, ExitOK, "Experiment quick-aa-baseline: exists (skipped)", "Before it runs:", "Calibrating 1 context(s) on claude-sonnet-5", "[16/16]", "settled")
	if logged, _ := os.ReadFile(filepath.Join(ctrl, "calibrations")); strings.Count(string(logged), "\n") != 1 {
		t.Errorf("start --yes calibrated %q, want one calibration (the A/A's one context)", logged)
	}
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
	got := f.run(ctx, "start", "--accept-mined", "--yes")
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

// Mined instructions wait for a person unless --accept-mined accepts them.
func TestStartWaitsForReviews(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 9)
	ctx := context.Background()
	got := f.run(ctx, "start")
	expect(t, got, ExitError, "8 valid of 8", "8 valid, 0 ready of the 8 an experiment needs: the others wait for your review", "Read each instruction for solution leaks",
		"agentium task show NAME", "agentium task edit NAME --reviewed", "agentium start --accept-mined", "without your review")
	expect(t, f.run(ctx, "start", "--accept-mined"), ExitOK, "Accepted 8 mined instruction(s) without your review (--accept-mined)", "a message that explains the fix is not detected",
		"Tasks: 8 ready", "Experiment quick-aa-baseline: created")
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
			if got := s.confirm(context.Background(), 12.5, ""); got != c.want {
				t.Errorf("confirm = %v, want %v", got, c.want)
			}
			if asked := strings.Contains(out.String(), "Run it now?"); asked != c.asked {
				t.Errorf("asked = %v, want %v (output %q)", asked, c.asked, out.String())
			}
		})
	}
	var out strings.Builder
	if (&starter{env: Env{Terminal: true, StdinTerminal: true, Stdout: &out}}).confirm(context.Background(), 1, "") || out.Len() != 0 {
		t.Error("a terminal without a stdin reader was asked")
	}
}

// readyFixture is startFixture with the experiment created and its baseline calibrated, so the next start is ready to
// ask. It leaves the experiment agent as Claude Code.
func readyFixture(t *testing.T) (runFixture, string) {
	t.Helper()
	f, ctrl := startFixture(t, 9)
	ctx := context.Background()
	expect(t, f.run(ctx, "start", "--accept-mined"), ExitOK)
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")
	expect(t, f.run(ctx, "run", "calibrate", "--snapshot", "baseline"), ExitOK)
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, ctrl)
	return f, ctrl
}

// terminalRun runs start as a person at a terminal would, answering the prompt with stdin.
func terminalRun(f runFixture, ctx context.Context, stdin io.Reader, args ...string) cliResult {
	var stdout, stderr bytes.Buffer
	code := Run(ctx, Env{Args: args, Stdin: stdin, StdinTerminal: true, Terminal: true, Stdout: &stdout, Stderr: &stderr, Dir: f.repo,
		Getenv: func(key string) string {
			if key == "NO_COLOR" {
				return "1"
			}
			return f.vars[key]
		},
		Environ:  func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home} },
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now,
		Backoff: func(int) time.Duration { return 10 * time.Millisecond }})
	return cliResult{code, stdout.String(), stderr.String()}
}

// The prompt inside start: asked on a terminal, "n" runs nothing, "y" runs the experiment; it quotes the budget the run
// would have, which --budget raises, and a lower --budget is refused before asking.
func TestStartPromptQuotesTheEffectiveBudget(t *testing.T) {
	t.Parallel()
	f, ctrl := readyFixture(t)
	ctx := context.Background()
	declined := terminalRun(f, ctx, strings.NewReader("n\n"), "start")
	expect(t, declined, ExitOK, "Run it now? It makes real Claude Code runs and spends up to $42.00. [y/N]", "Nothing was run and nothing was spent",
		"up to $42.00): agentium experiment run quick-aa-baseline\n")
	raised := terminalRun(f, ctx, strings.NewReader("n\n"), "start", "--budget", "100")
	expect(t, raised, ExitOK, "Budget for this run: $100.00 (the design's $42.00, raised by --budget)", "spends up to $100.00. [y/N]",
		"up to $100.00): agentium experiment run quick-aa-baseline --budget 100")
	lower := terminalRun(f, ctx, strings.NewReader("y\n"), "start", "--budget", "10")
	expect(t, lower, ExitUsage, "--budget $10.00 is below the experiment's $42.00")
	if strings.Contains(lower.stdout, "Run it now?") {
		t.Errorf("a lower budget was asked about:\n%s", lower.stdout)
	}
	if stored, started := paidRuns(t, f, ctrl); started != 0 {
		t.Errorf("declined prompts ran %d run(s) (%d stored)", started, stored)
	}
	yes := terminalRun(f, ctx, strings.NewReader("y\n"), "start", "--budget", "100")
	expect(t, yes, ExitOK, "spends up to $100.00. [y/N]", "[16/16]")
	if runs := experimentRuns(t, f, "quick-aa-baseline"); len(runs) < 16 {
		t.Errorf("answering y stored %d run(s), want 16", len(runs))
	}
}

// Ctrl-C while the prompt waits is a no.
func TestStartPromptStopsOnCancel(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	s := &starter{env: Env{Stdin: pr, StdinTerminal: true, Terminal: true, Stdout: &out}}
	done := make(chan bool)
	go func() { done <- s.confirm(ctx, 5, "") }()
	cancel()
	select {
	case got := <-done:
		if got {
			t.Error("a cancelled prompt said yes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt kept waiting after cancel")
	}
}

// Another snapshot saved later does not change which experiment start resumes (arm A stays baseline), and a committed
// context that moved on is noted.
func TestStartKeepsBaselineAsArmA(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 9)
	ctx := context.Background()
	expect(t, f.run(ctx, "start", "--accept-mined"), ExitOK, "Experiment quick-aa-baseline: created")
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nKeep it short.\n")
	expect(t, f.run(ctx, "context", "snapshot", "later", "--working-tree"), ExitOK)
	again := f.run(ctx, "start")
	expect(t, again, ExitOK, "Context: snapshot baseline from ", "Tasks: skipped (the experiment exists)", "Experiment quick-aa-baseline: exists (skipped)")
	if strings.Contains(again.stdout, "created") {
		t.Errorf("a new snapshot made a new experiment:\n%s", again.stdout)
	}
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "Shorten the rules")
	expect(t, f.run(ctx, "start"), ExitOK, "the context committed at HEAD differs from snapshot baseline", "agentium context snapshot NAME")
}

// --accept-mined accepts only what start mined itself: a task imported by hand keeps waiting for its review.
func TestStartAcceptMinedLeavesOtherTasksAlone(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 10)
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	head := strings.TrimSpace(gitIn(t, f.repo, "rev-parse", "HEAD"))
	expect(t, f.run(ctx, "task", "import", "--commit", head, "--name", "by-hand", "--verify", "make test"), ExitOK)
	got := f.run(ctx, "start", "--accept-mined")
	expect(t, got, ExitOK, "Accepted 8 mined instruction(s) without your review", "Experiment quick-aa-baseline: created")
	for _, line := range strings.Split(got.stdout, "\n") {
		if strings.HasPrefix(line, "Accepted ") && strings.Contains(line, "by-hand") {
			t.Errorf("the hand-imported task was accepted: %s", line)
		}
	}
	for _, task := range storedTasks(t, f.data) {
		if (task.Name == "by-hand") != task.NeedsReview {
			t.Errorf("task %s needs review = %v", task.Name, task.NeedsReview)
		}
	}
}

func TestNamesReferenceFile(t *testing.T) {
	t.Parallel()
	for instruction, want := range map[string]bool{"Add Reverse to strutil.go.": true, "Add Reverse to the library.": false, "Edit pkg/strutil.go": true} {
		got := namedReferenceFile(store.Task{Instruction: instruction, Reference: []string{"pkg/strutil.go"}}) != ""
		if got != want {
			t.Errorf("%q: names a reference file = %v, want %v", instruction, got, want)
		}
	}
}

// When every candidate fails validation, start stops after the first round instead of importing the whole history.
func TestStartStopsWhenNothingValidates(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "Makefile", "test:\n\tsh run_tests.sh\n")
	writeFile(t, repo, "run_tests.sh", "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n")
	writeFile(t, repo, "CLAUDE.md", "# Rules\n")
	writeFile(t, repo, "lib.sh", "base() { echo base; }\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "Initial commit")
	for i := 1; i <= 12; i++ { // each commit's test passes on the base already: the task is invalid
		writeFile(t, repo, "lib.sh", fmt.Sprintf("# change %d\nbase() { echo base; }\n", i))
		writeFile(t, repo, fmt.Sprintf("tests/b%d_test.sh", i), ". ./lib.sh\n[ \"$(base)\" = base ]\n")
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", fmt.Sprintf("Explain base again, %d\n\nA comment says what base prints.", i))
	}
	f := runFixtureAt(repo, filepath.Join(t.TempDir(), "data"), t.TempDir())
	got := f.run(context.Background(), "start", "--accept-mined")
	expect(t, got, ExitError, "imported 8 of 8 tried", "0 valid of 8", "set aside: ", "only 0 of the 8 an experiment needs are ready",
		"none of the 8 tasks just mined is valid")
	if n := strings.Count(got.stdout, "Mining:"); n != 1 {
		t.Errorf("start mined %d times, want once:\n%s", n, got.stdout)
	}
	if tasks := storedTasks(t, f.data); len(tasks) != 8 {
		t.Errorf("%d tasks imported, want 8", len(tasks))
	}
}

func projectFile(f runFixture, name string) string {
	return filepath.Join(f.data, "projects", "1", name)
}

// A broken task someone added by hand says nothing about mining: start still mines.
func TestStartMiningIgnoresABrokenHandAddedTask(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 10)
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	head := strings.TrimSpace(gitIn(t, f.repo, "rev-parse", "HEAD"))
	expect(t, f.run(ctx, "task", "import", "--commit", head, "--name", "broken", "--verify", "false"), ExitOK)
	got := f.run(ctx, "start", "--accept-mined")
	expect(t, got, ExitOK, "Mining: ", "Experiment quick-aa-baseline: created")
	if strings.Contains(got.stdout, "mining more would likely repeat") {
		t.Errorf("the broken task stopped mining:\n%s", got.stdout)
	}
}

// A task removed and imported by hand under the same name is not what start mined: --accept-mined leaves it, and the
// removed commit is never mined again.
func TestStartAcceptMinedIgnoresRemovedAndReimportedTasks(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 9)
	ctx := context.Background()
	expect(t, f.run(ctx, "start"), ExitError, "8 valid")
	first := storedTasks(t, f.data)[0]
	expect(t, f.run(ctx, "task", "rm", first.Name), ExitOK)
	expect(t, f.run(ctx, "task", "import", "--commit", first.SolutionCommit, "--name", first.Name, "--verify", "make test"), ExitOK)
	got := f.run(ctx, "start", "--accept-mined")
	expect(t, got, ExitOK, "Experiment quick-aa-baseline: created")
	byName := map[string]bool{}
	for _, task := range storedTasks(t, f.data) {
		byName[task.Name] = task.NeedsReview
	}
	if !byName[first.Name] {
		t.Errorf("the hand-imported task %s was accepted unread", first.Name)
	}
}

func TestStartNeverMinesARemovedTaskAgain(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 9)
	ctx := context.Background()
	expect(t, f.run(ctx, "start"), ExitError, "8 valid")
	first := storedTasks(t, f.data)[0]
	expect(t, f.run(ctx, "task", "rm", first.Name), ExitOK)
	got := f.run(ctx, "start", "--accept-mined")
	expect(t, got, ExitOK, "Experiment quick-aa-baseline: created")
	for _, task := range storedTasks(t, f.data) {
		if task.SolutionCommit == first.SolutionCommit {
			t.Errorf("start mined the removed commit again as %s", task.Name)
		}
	}
}

// Tasks the checks hold back do not count toward the 8, so mining goes on; what stays held back is listed with its
// reason instead of suggesting the flag again.
func TestStartHeldBackTasksDoNotCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enough := runFixtureAt(startRepoNaming(t, 10, 2), filepath.Join(t.TempDir(), "data"), t.TempDir())
	expect(t, enough.run(ctx, "start", "--accept-mined"), ExitOK, "Experiment quick-aa-baseline: created")
	short := runFixtureAt(startRepoNaming(t, 9, 2), filepath.Join(t.TempDir(), "data"), t.TempDir())
	got := short.run(ctx, "start", "--accept-mined")
	expect(t, got, ExitError, "held back from --accept-mined: ", "the instruction names reference file lib.sh")
	if strings.Contains(got.stdout, "(or agentium start --accept-mined") {
		t.Errorf("the flag was suggested again:\n%s", got.stdout)
	}
}

// A corrupt state file costs only the --accept-mined shortcut.
func TestStartWithACorruptStateFile(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 9)
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	if err := os.MkdirAll(filepath.Dir(projectFile(f, "x")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectFile(f, "start-mined.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "start", "--accept-mined"), ExitError, "start-mined.json is unreadable", "cannot tell which tasks start mined")
	plain := f.run(ctx, "start")
	expect(t, plain, ExitError, "is unreadable and ignored", "8 valid")
}

// tryTasks counts the project's tasks while another command runs; any failure counts as none.
func tryTasks(data string) int {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(data, "agentium.db"))
	if err != nil {
		return 0
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		return 0
	}
	tasks, _ := db.Tasks(ctx, projects[0].ID)
	return len(tasks)
}

// Ctrl-C while start imports keeps the records of what was imported, so a task removed afterwards is dismissed, not
// mined again and accepted unread.
func TestStartInterruptedMiningKeepsItsRecords(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 9)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if tryTasks(f.data) >= 2 {
				cancel()
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	got := f.run(ctx, "start", "--accept-mined")
	expect(t, got, ExitError, "Interrupted: ")
	tasks := storedTasks(t, f.data)
	if len(tasks) < 2 {
		t.Fatalf("%d task(s) imported before the interrupt", len(tasks))
	}
	var state minedState
	data, err := os.ReadFile(projectFile(f, "start-mined.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil || len(state.Mined) != len(tasks) {
		t.Fatalf("state %s (%v): want a record for each of the %d imported task(s)", data, err, len(tasks))
	}
	gone := tasks[0]
	expect(t, f.run(context.Background(), "task", "rm", gone.Name), ExitOK)
	expect(t, f.run(context.Background(), "start", "--accept-mined"), ExitOK, "Experiment quick-aa-baseline: created")
	for _, task := range storedTasks(t, f.data) {
		if task.SolutionCommit == gone.SolutionCommit {
			t.Errorf("the removed commit came back as %s", task.Name)
		}
	}
}

// Two starts at once keep each other's records.
func TestStartStateFileMergesConcurrentWriters(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 1)
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	w, err := openProject(ctx, Env{Dir: f.repo, Getenv: func(k string) string { return f.vars[k] }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	a := &starter{w: w, records: []minedRecord{{Name: "a", SolutionCommit: "1"}}, dismissed: map[string]bool{"x": true}}
	b := &starter{w: w, records: []minedRecord{{Name: "b", SolutionCommit: "2"}}, dismissed: map[string]bool{"y": true}}
	for _, s := range []*starter{a, b} {
		if err := s.saveMined(); err != nil {
			t.Fatal(err)
		}
	}
	state, err := a.readMined()
	if err != nil || len(state.Mined) != 2 || len(state.Dismissed) != 2 {
		t.Errorf("state %+v, %v: want both writers' records and dismissals", state, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(w.bare), "*.tmp")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// The budget line after an earlier raise, and what the "nothing was run" line says about earlier runs.
func TestStartQuotesAnEarlierRaiseAndEarlierRuns(t *testing.T) {
	t.Parallel()
	f, _ := readyFixture(t)
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored, err := db.ExperimentByName(ctx, 1, "quick-aa-baseline")
	if err != nil {
		t.Fatal(err)
	}
	var design experiment.Design
	if err := json.Unmarshal(stored.Design, &design); err != nil {
		t.Fatal(err)
	}
	design.BudgetUSD = 60 // raised by an earlier `experiment run --budget`
	lock, _ := json.Marshal(experiment.Lock{Design: design})
	if err := db.LockExperiment(ctx, stored.ID, lock); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRun(ctx, store.Run{ID: "r1", ProjectID: 1, TaskName: "x", Arm: "A", Outcome: "ok", ExperimentID: stored.ID, Slot: 0, Attempt: 1,
		Record: []byte("{}"), Started: time.Now(), Finished: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got := terminalRun(f, ctx, strings.NewReader("n\n"), "start")
	expect(t, got, ExitOK, "Budget for this run: $60.00 (the design's $42.00, raised earlier)", "spends up to $60.00. [y/N]",
		"Nothing was run in this command (the experiment has 1 run(s) from before)")
	if strings.Contains(got.stdout, "nothing was spent") {
		t.Errorf("claims nothing was spent:\n%s", got.stdout)
	}
}

// A project whose north star cannot be computed still previews and runs: the line is left out with a warning.
func TestStartLeavesOutANorthStarItCannotCompute(t *testing.T) {
	t.Parallel()
	f, _ := readyFixture(t)
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bad, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: 1, Name: "broken", Template: "aa", Design: []byte("{}"), CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.LockExperiment(ctx, bad.ID, []byte("{bad")); err != nil {
		t.Fatal(err)
	}
	if err := db.SetExperimentStatus(ctx, bad.ID, store.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	got := f.run(ctx, "start")
	expect(t, got, ExitOK, "Before it runs:", "the first-decisive-verdict line is left out", "Nothing was run and nothing was spent")
	if strings.Contains(got.stdout, "First decisive verdict") {
		t.Errorf("a line was shown:\n%s", got.stdout)
	}
}

// promptCanceller cancels the command when the prompt is shown, as Ctrl-C at it would.
type promptCanceller struct {
	bytes.Buffer
	cancel func()
}

func (p *promptCanceller) Write(b []byte) (int, error) {
	n, err := p.Buffer.Write(b)
	if strings.Contains(p.String(), "[y/N]") {
		p.cancel()
	}
	return n, err
}

// Ctrl-C at the prompt exits 1, as every other interrupt does, and runs nothing.
func TestStartCtrlCAtThePromptExitsOne(t *testing.T) {
	t.Parallel()
	f, ctrl := readyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	out, errOut := &promptCanceller{cancel: cancel}, &bytes.Buffer{}
	code := Run(ctx, Env{Args: []string{"start"}, Stdin: pr, StdinTerminal: true, Terminal: true, Stdout: out, Stderr: errOut, Dir: f.repo,
		Getenv: func(key string) string {
			if key == "NO_COLOR" {
				return "1"
			}
			return f.vars[key]
		},
		Environ:  func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home} },
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now})
	if code != ExitError || !strings.Contains(out.String(), "Interrupted: nothing was run.") {
		t.Errorf("exit %d, want 1\nstdout %s\nstderr %s", code, out.String(), errOut.String())
	}
	if stored, started := paidRuns(t, f, ctrl); started != 0 {
		t.Errorf("a run started (%d stored)", stored)
	}
}

// The drift note compares the context files and the documents the snapshot included, and says nothing when they match.
func TestStartDriftChecksIncludedDocuments(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 1)
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	writeFile(t, f.repo, "docs/guide.md", "Guide v1\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "Add a guide")
	expect(t, f.run(ctx, "context", "snapshot", "baseline", "--include", "docs/guide.md"), ExitOK)
	var out strings.Builder
	env := Env{Dir: f.repo, Stdout: &out, Stderr: &out, Getenv: func(k string) string { return f.vars[k] }}
	w, err := openProject(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	snap, err := w.db.SnapshotByName(ctx, w.project.ID, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	s := &starter{env: env, w: w}
	drifts := func() bool {
		out.Reset()
		if err := s.noteDrift(ctx, snap); err != nil {
			t.Fatal(err)
		}
		return strings.Contains(out.String(), "differs from snapshot baseline")
	}
	if drifts() {
		t.Errorf("false drift with an included document:\n%s", out.String())
	}
	writeFile(t, f.repo, "docs/guide.md", "Guide v2\n")
	gitIn(t, f.repo, "commit", "-q", "-am", "Change the guide")
	if !drifts() {
		t.Error("a changed included document is not drift")
	}
	gitIn(t, f.repo, "rm", "-q", "docs/guide.md")
	gitIn(t, f.repo, "commit", "-q", "-m", "Remove the guide")
	if !drifts() {
		t.Error("a missing included document is not drift")
	}
}
