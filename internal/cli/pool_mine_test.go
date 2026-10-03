package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/mine"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// mineRepo builds a repository with shell code and tests: three commits that make tasks (one of which is invalid:
// its test passes on the base already) and four to set aside. It returns the root and the three candidates' hashes,
// newest first.
func mineRepo(t *testing.T) (string, []string) {
	t.Helper()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	commit := func(message string, files map[string]string) string {
		for p, body := range files {
			writeFile(t, repo, p, body)
		}
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", message)
		return strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	}
	commit("Initial commit", map[string]string{
		"run_tests.sh": "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n",
		"lib.sh":       "base() { echo base; }\n", "README.md": "# lib\n",
	})
	greet := commit("Add greet to the library\n\nThe greet function prints hello, for the welcome screen.\n\nCo-Authored-By: Someone <s@example.com>",
		map[string]string{"lib.sh": "base() { echo base; }\ngreet() { echo hello; }\n",
			"tests/greet_test.sh": ". ./lib.sh\n[ \"$(greet)\" = hello ]\n"})
	commit("Document greet in the README", map[string]string{"README.md": "# lib\n\ngreet prints hello.\n"})
	commit("Cover base in the tests", map[string]string{"tests/base_test.sh": ". ./lib.sh\n[ \"$(base)\" = base ]\n"})
	// Invalid as a task: its test of base passes before the change too. It ranks second (an issue reference).
	invalid := commit("Explain base in a comment and check it again\n\nA comment says what base prints, and a second test pins it. Refs #5.",
		map[string]string{"lib.sh": "# base prints base.\nbase() { echo base; }\ngreet() { echo hello; }\n",
			"tests/base2_test.sh": ". ./lib.sh\n[ \"$(base)\" = base ]\n"})
	commit("Speed up base without tests", map[string]string{"lib.sh": "# base prints base.\nbase() { printf 'base\\n'; }\ngreet() { echo hello; }\n"})
	farewell := commit("Add farewell to the library\n\nThe farewell function prints bye when the session ends.",
		map[string]string{"lib.sh": "# base prints base.\nbase() { printf 'base\\n'; }\ngreet() { echo hello; }\nfarewell() { echo bye; }\n",
			"tests/farewell_test.sh": ". ./lib.sh\n[ \"$(farewell)\" = bye ]\n"})
	return repo, []string{farewell, invalid, greet}
}

// storedTasks reads every task in the data folder's only project.
func storedTasks(t *testing.T, data string) []store.Task {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(data, "agentium.db"))
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
	return tasks
}

// cliNow is cliIn with the real clock: the pool's window is counted back from now, and the fixtures' commits are
// made now.
func cliNow(t *testing.T, dir, data string) func(args ...string) cliResult {
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": t.TempDir(), "AGENTIUM_CLAUDE": filepath.Join(t.TempDir(), "no-claude")}
	return func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{DefaultGrader: "host", Args: args, Stdout: &stdout, Stderr: &stderr, Dir: dir, Getenv: func(key string) string { return vars[key] },
			LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now})
		return cliResult{code, stdout.String(), stderr.String()}
	}
}

// Mining is pool update's (task mine is gone, with a pointer): its dry run lists the candidates with their scores and
// why the other commits were set aside; a pass imports the best --limit with the project's verify setting and
// validates them; the next pass takes the rest; a re-scan (--since) counts the commits that are tasks now. task import
// --commit makes the same tasks.
func TestPoolUpdateMinesHistory(t *testing.T) {
	t.Parallel()
	repo, hashes := mineRepo(t)
	data := filepath.Join(t.TempDir(), "data")
	run := cliNow(t, repo, data)
	expect(t, run("init"), ExitOK)
	before := repoState(t, repo)

	for _, args := range [][]string{{"task", "mine"}, {"task", "mine", "--dry-run", "--since", "2026-01-01"}} {
		got := run(args...)
		expect(t, got, ExitUsage, "agentium task mine: removed: agentium pool update mines", "agentium pool update --dry-run lists the candidates")
		if got.stdout != "" {
			t.Errorf("%q printed on stdout: %s", args, got.stdout)
		}
	}
	expect(t, run("pool", "update", "--limit", "0"), ExitUsage, "--limit must be at least 1")
	expect(t, run("pool", "update", "--since", "yesterday", "--dry-run"), ExitUsage, `--since "yesterday" is not a date`)
	expect(t, run("pool", "update", "--jobs", "0"), ExitUsage, "--jobs must be at least 1")
	expect(t, run("pool", "update", "--max-files", "0"), ExitUsage, "--max-files and --max-lines must be at least 1")
	expect(t, run("pool", "update", "--verify-timeout", "0s"), ExitUsage, "--verify-timeout must be at least 1ms")
	expect(t, run("pool", "update", "extra"), ExitUsage, "takes no arguments")
	// No test command is detected here: without one the pass mines nothing; --verify (this call) or the setting gives one.
	expect(t, run("pool", "update", "--dry-run"), ExitOK, "so the pass mines nothing", "agentium init --verify CMD", "0 candidate(s)")
	expect(t, run("pool", "update", "--dry-run", "--verify", "sh run_tests.sh"), ExitOK, "3 candidate(s)",
		"mined tasks will verify with: sh run_tests.sh")
	expect(t, run("init", "--verify", "sh run_tests.sh"), ExitOK, "verify          sh run_tests.sh")

	dry := run("pool", "update", "--dry-run")
	expect(t, dry, ExitOK, "Would mine main at ", "7 commit(s) since the last pass, 3 candidate(s)", "SCORE", "Add farewell to the library",
		"+3 the message has a subject and a body", "Set aside: 4 commit(s)", "no parent commit", "documentation only", "no test changes",
		"tests only", "note: mined tasks will verify with: sh run_tests.sh (agentium init --verify to change)", "agentium pool update mines, validates and retires")
	for _, h := range hashes {
		if !strings.Contains(dry.stdout, experiment.ShortCommit(h)) {
			t.Errorf("dry run lacks candidate %s:\n%s", h, dry.stdout)
		}
	}
	// The reasons come in the order the scan checks them.
	if order := []string{"no parent commit", "documentation only", "no test changes", "tests only"}; !inOrder(dry.stdout, order) {
		t.Errorf("rejections not in scan order %v:\n%s", order, dry.stdout)
	}
	expect(t, run("task", "list"), ExitOK, "No tasks yet: agentium pool update")
	limited := run("pool", "update", "--dry-run", "--limit", "1")
	expect(t, limited, ExitOK, "2 more candidate(s); --limit N lists more")
	if strings.Contains(limited.stdout, "  2  ") {
		t.Errorf("--limit 1 listed a second candidate:\n%s", limited.stdout)
	}
	expect(t, run("pool", "update", "--dry-run", "--since", "2099-01-01"), ExitOK, "0 commit(s) since 2099-01-01, 0 candidate(s)", "No candidates.")

	first := run("pool", "update", "--limit", "2", "--jobs", "2")
	expect(t, first, ExitOK, "Imported 2 of 2 candidate(s) tried (verify: sh run_tests.sh)", "Validating 2 mined task(s), 2 at a time",
		"Review each mined instruction for solution leaks")
	tasks := storedTasks(t, data)
	if len(tasks) != 2 {
		t.Fatalf("--limit 2 stored %d task(s)", len(tasks))
	}
	mined := map[string]store.Task{}
	for _, task := range tasks {
		mined[task.SolutionCommit] = task
		if !task.NeedsReview || task.Validation == nil || !slices.Equal(task.Verify, []string{"sh run_tests.sh"}) {
			t.Errorf("mined task %+v: wants review, a validation and the project's verify setting", task)
		}
	}
	greet, ok := mined[hashes[2]]
	if !ok {
		greet = mined[hashes[0]]
	}
	if greet.Instruction == "" || strings.Contains(greet.Instruction, "Co-Authored-By") {
		t.Errorf("instruction %q keeps its trailers", greet.Instruction)
	}

	expect(t, run("pool", "update", "--dry-run"), ExitOK, "1 candidate(s)", "Next: agentium pool update imports 1 and validates them")
	second := run("pool", "update")
	expect(t, second, ExitOK, "1 candidate(s)", "Imported 1 of 1 candidate(s) tried")
	tasks = storedTasks(t, data)
	if len(tasks) != 3 {
		t.Fatalf("mining again stored %d task(s), want 3", len(tasks))
	}
	byCommit := map[string]store.Task{}
	for _, task := range tasks {
		if _, dup := byCommit[task.SolutionCommit]; dup {
			t.Fatalf("commit %s imported twice", task.SolutionCommit)
		}
		byCommit[task.SolutionCommit] = task
	}
	statuses := map[string]string{}
	for _, h := range hashes {
		statuses[h] = statusOf(byCommit[h])
	}
	if statuses[hashes[0]] != "valid" || statuses[hashes[1]] != "invalid" || statuses[hashes[2]] != "valid" {
		t.Errorf("statuses %v: want farewell and greet valid, the comment invalid", statuses)
	}
	expect(t, run("pool", "update", "--dry-run"), ExitOK, "0 commit(s) since the last pass, 0 candidate(s)", "No candidates.")
	// A re-scan reads every commit again and counts the tasks among the commits it set aside; it imports nothing new.
	again := run("pool", "update", "--dry-run", "--since", "2000-01-01")
	expect(t, again, ExitOK, "a re-scan: it reads every commit from", "--since 2000-01-01 is earlier", "7 commit(s) since 2000-01-01, 0 candidate(s)")
	if !slices.ContainsFunc(strings.Split(again.stdout, "\n"), func(line string) bool {
		return strings.Join(strings.Fields(line), " ") == "already a task 3"
	}) {
		t.Errorf("a re-scan does not count the 3 tasks:\n%s", again.stdout)
	}
	expect(t, run("pool", "update", "--since", "2000-01-01"), ExitOK, "Nothing new to import")
	expect(t, run("task", "list"), ExitOK, "invalid: base/hidden-tests wanted fail")

	// task import --commit makes the same tasks, through the same path, with the same settings.
	otherData := filepath.Join(t.TempDir(), "data")
	other := cliNow(t, repo, otherData)
	expect(t, other("init", "--verify", "sh run_tests.sh"), ExitOK)
	for _, h := range hashes {
		expect(t, other("task", "import", "--commit", h), ExitOK, "verify: sh run_tests.sh")
	}
	for _, imported := range storedTasks(t, otherData) {
		m := byCommit[imported.SolutionCommit]
		if m.Name != imported.Name || m.Instruction != imported.Instruction || m.Source != imported.Source || m.BaseCommit != imported.BaseCommit ||
			!slices.Equal(m.HiddenTests, imported.HiddenTests) || !slices.Equal(m.Reference, imported.Reference) ||
			!slices.Equal(m.Verify, imported.Verify) || !slices.Equal(m.Setup, imported.Setup) || m.NeedsReview != imported.NeedsReview ||
			m.Grading != imported.Grading {
			t.Errorf("imported %+v\nmined    %+v", imported, m)
		}
	}
	if after := repoState(t, repo); after != before {
		t.Errorf("mining modified the repository:\nbefore %s\nafter  %s", before, after)
	}
}

// inOrder reports whether every text appears in out, each after the one before it.
func inOrder(out string, texts []string) bool {
	at := 0
	for _, text := range texts {
		i := strings.Index(out[at:], text)
		if i < 0 {
			return false
		}
		at += i + len(text)
	}
	return true
}

// validateRepo is a repository with one commit, registered in its own data folder.
func validateRepo(t *testing.T) (repo, data string, run func(args ...string) cliResult) {
	t.Helper()
	repo = t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "value.txt", "1\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	data = filepath.Join(t.TempDir(), "data")
	run = cliIn(t, repo, data)
	expect(t, run("init"), ExitOK)
	return repo, data, run
}

// maxCount is the largest number in the file's lines (0 when it has none).
func maxCount(t *testing.T, file string) int {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	most := 0
	for _, field := range strings.Fields(string(raw)) {
		n, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("%s: %q", file, raw)
		}
		most = max(most, n)
	}
	return most
}

func TestTaskValidateAll(t *testing.T) {
	t.Parallel()
	_, _, run := validateRepo(t)
	// The probe counts the validations running at once: each holds a folder for a second.
	probeDir := t.TempDir()
	probe := filepath.Join(probeDir, "probe.sh")
	writeFile(t, probeDir, "probe.sh", "d="+probeDir+"\nmkdir \"$d/run.$$\"\nls -d \"$d\"/run.* | wc -l >> \"$d/counts\"\nsleep 1\nrmdir \"$d/run.$$\"\n")
	for i := 1; i <= 4; i++ {
		expect(t, run("task", "add", "t"+strconv.Itoa(i), "--base", "HEAD", "--instruction", "Anything.", "--verify", "sh "+probe), ExitOK)
	}

	expect(t, run("task", "validate", "t1", "--all"), ExitUsage, "give a task NAME or --all, not both")
	expect(t, run("task", "validate", "--all", "--jobs", "0"), ExitUsage, "--jobs must be at least 1")
	expect(t, run("task", "validate", "t1", "--jobs", "2"), ExitUsage, "--status and --jobs need --all")
	expect(t, run("task", "validate", "t1", "--status", "valid"), ExitUsage, "--status and --jobs need --all")
	expect(t, run("task", "validate", "--all", "--status", "done"), ExitUsage, "--status must be one of unvalidated, valid, invalid, flaky, unchecked")
	expect(t, run("task", "validate", "--all", "--weak-tests"), ExitUsage, "--weak-tests checks one task at a time")
	expect(t, run("task", "validate", "--all", "--snapshot", "missing"), ExitError, "missing")
	expect(t, run("task", "validate", "--all", "--status", "valid"), ExitOK, "No valid tasks to validate.")

	all := run("task", "validate", "--all", "--jobs", "2")
	expect(t, all, ExitOK, "Validating 4 task(s) in 1 arm(s), 2 at a time", "NAME", "STATUS", "t1", "t4", "unchecked")
	if n := maxCount(t, filepath.Join(probeDir, "counts")); n != 2 {
		t.Errorf("--jobs 2 ran %d validation(s) at once at most", n)
	}
	if err := os.Remove(filepath.Join(probeDir, "counts")); err != nil {
		t.Fatal(err)
	}
	expect(t, run("task", "validate", "--all", "--jobs", "1", "--status", "unchecked", "--repeat", "2"), ExitOK, "Validating 4 task(s)")
	if n := maxCount(t, filepath.Join(probeDir, "counts")); n != 1 {
		t.Errorf("--jobs 1 ran %d validation(s) at once at most", n)
	}

	expect(t, run("task", "validate", "--all", "--status", "unvalidated"), ExitOK, "No unvalidated tasks to validate.")
	expect(t, run("task", "add", "bad", "--base", "HEAD", "--instruction", "Anything.", "--verify", "exit 1"), ExitOK)
	bad := run("task", "validate", "--all", "--status", "unvalidated")
	expect(t, bad, ExitError, "Validating 1 task(s)", "bad", "invalid: base/base wanted pass")
	if strings.Contains(bad.stdout, "t1") {
		t.Errorf("--status unvalidated validated t1:\n%s", bad.stdout)
	}
	expect(t, run("task", "list"), ExitOK, "invalid: base/base wanted pass")
}

// Ctrl-C during a batch keeps the validations that finished; the one it stopped and those not started keep none.
func TestTaskValidateAllInterrupted(t *testing.T) {
	t.Parallel()
	repo, data, run := validateRepo(t)
	marks := t.TempDir()
	for _, task := range [][2]string{{"a-fast", "touch " + marks + "/a"}, {"b-fast", "touch " + marks + "/b"},
		{"c-slow", "touch " + marks + "/slow; sleep 30"}, {"d-later", "true"}} { // validated in this order
		expect(t, run("task", "add", task[0], "--base", "HEAD", "--instruction", "Anything.", "--verify", task[1]), ExitOK)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": t.TempDir()}
	go func() {
		done <- Run(ctx, Env{DefaultGrader: "host", Args: []string{"task", "validate", "--all", "--jobs", "1"}, Stdout: &stdout, Stderr: &stderr, Dir: repo,
			Getenv: func(key string) string { return vars[key] }, LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Now: func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }})
	}()
	waitFor(t, "the slow validation", func() bool {
		_, err := os.Stat(filepath.Join(marks, "slow"))
		return err == nil
	})
	start := time.Now()
	cancel()
	var code int
	select {
	case code = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the batch did not stop on cancel")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("stopping took %s", took)
	}
	r := cliResult{code, stdout.String(), stderr.String()}
	expect(t, r, ExitError, "Interrupted: 2 of 4 task(s) validated and kept", "agentium task validate --all --status unvalidated",
		"not started (interrupted)")
	if !strings.Contains(strings.ReplaceAll(r.stdout, "not started (interrupted)", ""), "interrupted") {
		t.Errorf("c-slow is not marked interrupted:\n%s", r.stdout)
	}
	byName := map[string]string{}
	for _, task := range storedTasks(t, data) {
		byName[task.Name] = statusOf(task)
	}
	if want := map[string]string{"a-fast": "unchecked", "b-fast": "unchecked", "c-slow": "unvalidated", "d-later": "unvalidated"}; !mapsEqual(byName, want) {
		t.Errorf("statuses %v, want %v", byName, want)
	}
	// The resumed batch validates only what is left.
	expect(t, run("task", "edit", "c-slow", "--verify", "true"), ExitOK)
	expect(t, run("task", "validate", "--all", "--status", "unvalidated"), ExitOK, "Validating 2 task(s)")
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Mined tasks verify with the build tools' test commands, not every command init found; task import keeps those all.
// The project's verify and setup settings replace both defaults.
func TestPoolUpdateVerifiesWithBuildTools(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	commit := func(message string, files map[string]string) string {
		for p, body := range files {
			writeFile(t, repo, p, body)
		}
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", message)
		return strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	}
	commit("Initial commit", map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "m.go": "package m\n",
		"Makefile": "test:\n\tfalse\n", "lint.sh": "exit 1\n"})
	add := commit("Add Double to the package\n\nDouble returns twice its argument, for the totals.",
		map[string]string{"m.go": "package m\n\nfunc Double(n int) int { return 2 * n }\n",
			"m_test.go": "package m\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal(Double(2))\n\t}\n}\n"})
	data := filepath.Join(t.TempDir(), "data")
	run := cliNow(t, repo, data)
	expect(t, run("init"), ExitOK, "go test ./...; make test", "not set: mined tasks verify with go test ./..., imported ones with go test ./...; make test")

	expect(t, run("pool", "update", "--dry-run"), ExitOK, "note: mined tasks will verify with: go test ./... (agentium init --verify to change)",
		"note: only commits with go tests count, as go test ./... runs them")
	mined := run("pool", "update")
	expect(t, mined, ExitOK, "Imported 1 of 1 candidate(s) tried (verify: go test ./...)")
	tasks := storedTasks(t, data)
	if len(tasks) != 1 || !slices.Equal(tasks[0].Verify, []string{"go test ./..."}) || len(tasks[0].Setup) != 0 || statusOf(tasks[0]) != "valid" {
		t.Fatalf("mined %+v", tasks)
	}
	expect(t, run("task", "rm", tasks[0].Name), ExitOK)
	expect(t, run("task", "import", "--commit", add), ExitOK, "verify: go test ./...; make test")
	imported := storedTasks(t, data)[0]
	expect(t, run("task", "rm", imported.Name), ExitOK)

	// The settings replace both (in another data folder: the pool never mines a removed task's commit again), and task
	// import takes them too; task import's own --verify and --setup (hidden) win for that call.
	data = filepath.Join(t.TempDir(), "data")
	run = cliNow(t, repo, data)
	expect(t, run("init", "--verify", "go vet ./...", "--verify", "go test ./...", "--setup", "true"), ExitOK,
		"verify          go vet ./...; go test ./...", "setup           true")
	expect(t, run("pool", "update"), ExitOK, "Imported 1 of 1 candidate(s) tried (verify: go vet ./...; go test ./...)")
	tasks = storedTasks(t, data)
	if len(tasks) != 1 || !slices.Equal(tasks[0].Verify, []string{"go vet ./...", "go test ./..."}) || !slices.Equal(tasks[0].Setup, []string{"true"}) ||
		statusOf(tasks[0]) != "valid" {
		t.Fatalf("mined with the settings %+v", tasks)
	}
	expect(t, run("task", "rm", tasks[0].Name), ExitOK)
	expect(t, run("task", "import", "--commit", add), ExitOK, "setup:  true", "verify: go vet ./...; go test ./...")
	expect(t, run("task", "rm", storedTasks(t, data)[0].Name), ExitOK)
	expect(t, run("task", "import", "--commit", add, "--verify", "go test ./...", "--setup", ""), ExitOK, "verify: go test ./...")
	if got := storedTasks(t, data)[0]; !slices.Equal(got.Verify, []string{"go test ./..."}) || len(got.Setup) != 0 {
		t.Errorf("task import's own flags: %+v", got)
	}
	// An empty --verify and --setup return init's settings to the defaults.
	expect(t, run("init", "--verify", "", "--setup", ""), ExitOK, "not set: mined tasks verify with go test ./...", "setup           none")
}

// A batch stores each validation without undoing edits made while it ran, and drops one whose commands changed.
func TestTaskValidateAllKeepsEditsMadeMeanwhile(t *testing.T) {
	t.Parallel()
	repo, data, run := validateRepo(t)
	gate := t.TempDir()
	wait := "touch " + gate + "/$TASK; while [ ! -e " + gate + "/go ]; do sleep 0.05; done"
	for _, name := range []string{"edited", "changed"} {
		expect(t, run("task", "add", name, "--base", "HEAD", "--instruction", "Anything.", "--verify", strings.ReplaceAll(wait, "$TASK", name)), ExitOK)
	}
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": t.TempDir()}
	go func() {
		done <- Run(context.Background(), Env{DefaultGrader: "host", Args: []string{"task", "validate", "--all", "--jobs", "2"}, Stdout: &stdout, Stderr: &stderr, Dir: repo,
			Getenv: func(key string) string { return vars[key] }, LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Now: func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }})
	}()
	waitFor(t, "both validations", func() bool {
		_, a := os.Stat(filepath.Join(gate, "edited"))
		_, b := os.Stat(filepath.Join(gate, "changed"))
		return a == nil && b == nil
	})
	expect(t, run("task", "edit", "edited", "--instruction", "Edited while it ran."), ExitOK)
	expect(t, run("task", "edit", "changed", "--verify", "true"), ExitOK)
	writeFile(t, gate, "go", "")
	r := cliResult{<-done, stdout.String(), stderr.String()}
	expect(t, r, ExitError, "not stored: the task changed during validation")
	byName := map[string]store.Task{}
	for _, task := range storedTasks(t, data) {
		byName[task.Name] = task
	}
	if e := byName["edited"]; e.Instruction != "Edited while it ran." || statusOf(e) != "unchecked" {
		t.Errorf("edited: instruction %q, status %s", e.Instruction, statusOf(e))
	}
	if c := byName["changed"]; c.Validation != nil || !slices.Equal(c.Verify, []string{"true"}) {
		t.Errorf("changed: verify %v, validation %s", c.Verify, c.Validation)
	}

	// Single validation stores the same way.
	expect(t, run("task", "add", "single", "--base", "HEAD", "--instruction", "Anything.",
		"--verify", "touch "+gate+"/single; while [ ! -e "+gate+"/go2 ]; do sleep 0.05; done"), ExitOK)
	var out, errOut bytes.Buffer
	go func() {
		done <- Run(context.Background(), Env{DefaultGrader: "host", Args: []string{"task", "validate", "single"}, Stdout: &out, Stderr: &errOut, Dir: repo,
			Getenv: func(key string) string { return vars[key] }, LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Now: func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }})
	}()
	waitFor(t, "the single validation", func() bool {
		_, err := os.Stat(filepath.Join(gate, "single"))
		return err == nil
	})
	expect(t, run("task", "edit", "single", "--verify", "true"), ExitOK)
	writeFile(t, gate, "go2", "")
	expect(t, cliResult{<-done, out.String(), errOut.String()}, ExitError, "not stored: the task changed during validation")
}

// When no candidate can be imported, pool update says why per commit, without a review reminder.
func TestPoolUpdateNothingImported(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("read-only folders do not stop root")
	}
	repo, _ := mineRepo(t)
	data := filepath.Join(t.TempDir(), "data")
	run := cliNow(t, repo, data)
	expect(t, run("init", "--verify", "sh run_tests.sh"), ExitOK)
	expect(t, run("task", "list"), ExitOK) // opens Agentium's repository of the project
	// Agentium's repository refuses new objects, so no commit can be kept there.
	objects := filepath.Join(data, "projects", "1", "repo.git", "objects")
	var dirs []string
	if err := filepath.WalkDir(objects, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, d := range dirs {
			os.Chmod(d, 0o700)
		}
	})
	r := run("pool", "update")
	expect(t, r, ExitOK, "Imported 0 of 3 candidate(s) tried", "not imported: ")
	if strings.Contains(r.stdout, "Review each mined instruction") || len(storedTasks(t, data)) != 0 {
		t.Errorf("nothing imported, yet:\n%s", r.stdout)
	}
}

// A commit another process made a task of while it was being imported is skipped, not imported twice.
func TestMineTaskSkipsACommitImportedMeanwhile(t *testing.T) {
	t.Parallel()
	repo, _ := mineRepo(t)
	data := filepath.Join(t.TempDir(), "data")
	run := cliIn(t, repo, data)
	expect(t, run("init"), ExitOK)
	ctx := context.Background()
	vars := map[string]string{"AGENTIUM_HOME": data}
	w, err := openProject(ctx, Env{Dir: repo, Getenv: func(key string) string { return vars[key] }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	res, err := mine.Scan(ctx, repo, mine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, run("task", "import", "--commit", res.Candidates[0].Hash, "--verify", "true"), ExitOK) // the other process
	names := map[string]bool{}                                                                       // as read before it
	_, err = w.mineTask(ctx, res.Candidates[0], store.Task{ProjectID: w.project.ID, Verify: []string{"true"}}, names)
	if !errors.Is(err, errAlreadyTask) || len(storedTasks(t, data)) != 1 {
		t.Fatalf("mineTask of %s: %v, %d task(s)", res.Candidates[0].Hash, err, len(storedTasks(t, data)))
	}
	// Another commit whose name is taken gets the next free one.
	second := res.Candidates[1]
	expect(t, run("task", "add", taskName(second.Subject, second.Hash), "--base", "HEAD", "--instruction", "Other.", "--verify", "true"), ExitOK)
	saved, err := w.mineTask(ctx, second, store.Task{ProjectID: w.project.ID, Verify: []string{"true"}}, names)
	if err != nil || saved.Name != taskName(second.Subject, second.Hash)+"-2" {
		t.Fatalf("mineTask = %q, %v", saved.Name, err)
	}
}

// A batch says when an experiment holds the run lock: validations slow its runs.
func TestTaskValidateAllNotesARunningExperiment(t *testing.T) {
	t.Parallel()
	_, data, run := validateRepo(t)
	expect(t, run("task", "add", "one", "--base", "HEAD", "--instruction", "Anything.", "--verify", "true"), ExitOK)
	expect(t, run("task", "validate", "--all"), ExitOK)
	release, err := home.Layout{Root: data}.LockRuns()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	expect(t, run("task", "validate", "--all"), ExitOK, "note: an experiment is running")
}

// After a parallel batch, the advice names a command for each status that may come from running side by side.
func TestBatchAdviceNamesEachStatusFound(t *testing.T) {
	t.Parallel()
	result := func(status string) task.BatchResult {
		return task.BatchResult{Validated: true, Task: store.Task{Validation: []byte(`{"status":"` + status + `"}`)}}
	}
	for _, c := range []struct {
		statuses []string
		jobs     int
		want     []string
	}{
		{[]string{"valid", "flaky"}, 2, []string{"--status flaky --jobs 1"}},
		{[]string{"invalid", "flaky", "invalid"}, 2, []string{"--status invalid --jobs 1 and agentium task validate --all --status flaky --jobs 1"}},
		{[]string{"valid"}, 2, nil},
		{[]string{"invalid"}, 1, nil},
	} {
		var stdout strings.Builder
		var results []task.BatchResult
		for _, s := range c.statuses {
			results = append(results, result(s))
		}
		batchAdvice(Env{Stdout: &stdout, Getenv: func(string) string { return "" }}, results, c.jobs)
		if len(c.want) == 0 && stdout.Len() > 0 {
			t.Errorf("%v with --jobs %d gave advice: %s", c.statuses, c.jobs, stdout.String())
		}
		for _, w := range c.want {
			if !strings.Contains(stdout.String(), w) {
				t.Errorf("%v with --jobs %d: advice %q lacks %q", c.statuses, c.jobs, stdout.String(), w)
			}
		}
	}
}

// statusOf is a task's status for --status (task.StatusOf).
func statusOf(t store.Task) string { return task.StatusOf(t) }

// errAlreadyTask means a candidate became a task (in another process) while it was being imported.
var errAlreadyTask = mine.ErrAlreadyTask

// mineTask imports candidate c as the task t describes (mine.Importer.One), with a name not in names.
func (w *workspace) mineTask(ctx context.Context, c mine.Candidate, t store.Task, names map[string]bool) (store.Task, error) {
	return w.importer(names).One(ctx, c, t)
}

// A partial clone (--filter) is refused by pool update and by start's mining step before any history is read, with the
// remedy; the same repository without the setting still mines.
func TestMineRefusesPartialClone(t *testing.T) {
	t.Parallel()
	repo, _ := mineRepo(t)
	data := filepath.Join(t.TempDir(), "data")
	run := cliNow(t, repo, data)
	expect(t, run("init", "--verify", "sh run_tests.sh"), ExitOK)
	expect(t, run("pool", "update", "--dry-run"), ExitOK, "3 candidate(s)")

	gitIn(t, repo, "config", "extensions.partialClone", "origin")
	gitIn(t, repo, "config", "remote.origin.promisor", "true")
	want := "agentium: this is a partial clone (--filter): mining needs the full history offline; run `git fetch --refetch` or clone without --filter"
	got := run("pool", "update", "--dry-run")
	expect(t, got, ExitError, want)
	if got.stdout != "" {
		t.Errorf("a refused mine printed on stdout: %s", got.stdout)
	}
	expect(t, run("pool", "update"), ExitError, want)
	expect(t, run("start", "--accept-mined"), ExitError, "this is a partial clone (--filter)")

	gitIn(t, repo, "config", "--unset", "extensions.partialClone")
	gitIn(t, repo, "config", "--unset", "remote.origin.promisor")
	expect(t, run("pool", "update", "--dry-run"), ExitOK, "3 candidate(s)")
}

// The scan's note names what runs the tests mining picks commits by: the tool's own command (Go's), else the verify
// commands (Python's profile proposes none), else nothing, never an empty name; without languages, no note.
func TestMineScanNoteNamesTheRunner(t *testing.T) {
	for _, c := range []struct {
		opts   mine.Options
		verify []string
		want   string
	}{
		{mine.Options{Languages: []string{"go"}, TestCommand: "go test ./..."}, []string{"go test ./..."}, "only commits with go tests count, as go test ./... runs them"},
		{mine.Options{Languages: []string{"python"}}, []string{"uv run pytest"}, "only commits with python tests count, as uv run pytest runs them"},
		{mine.Options{Languages: []string{"python"}}, nil, "only commits with python tests count"},
		{mine.Options{}, []string{"make test"}, ""},
	} {
		if got := scanNote(c.opts, c.verify); got != c.want {
			t.Errorf("%+v: %q, want %q", c.opts, got, c.want)
		}
	}
}

// A Python commit whose base pins no dependencies is mined by default (the user's decision 3); the require-lock setting
// sets it aside with the reason, a command's own --require-lock[=false] (hidden) overrides it for that call, and start
// follows the setting and names it when it emptied the history.
func TestPoolUpdateUnlockedPython(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	commit := func(message string, files map[string]string) string {
		for p, body := range files {
			writeFile(t, repo, p, body)
		}
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", message)
		return strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	}
	commit("Start the package", map[string]string{"pyproject.toml": "[project]\nname = \"pkg\"\n", "pkg/__init__.py": "\n",
		"pkg/core.py": "def base():\n    return 1\n", "tests/test_core.py": "from pkg.core import base\n", "Makefile": "test:\n\ttrue\n"})
	hash := commit("Add double to pkg.core\n\nThe double function returns twice its argument, for the reports.",
		map[string]string{"pkg/core.py": "def base():\n    return 1\n\n\ndef double(x):\n    return 2 * x\n",
			"tests/test_core.py": "from pkg.core import base, double\n\n\ndef test_double():\n    assert double(2) == 4\n"})
	run := cliNow(t, repo, filepath.Join(t.TempDir(), "data"))
	expect(t, run("init"), ExitOK, "require lock    off (default)")
	dry := run("pool", "update", "--dry-run")
	expect(t, dry, ExitOK, "2 commit(s) since the last pass, 1 candidate(s)", "Add double to pkg.core")
	if !strings.Contains(dry.stdout, experiment.ShortCommit(hash)) || strings.Contains(dry.stdout, "no lock file") {
		t.Errorf("the default:\n%s", dry.stdout)
	}
	expect(t, run("pool", "update", "--dry-run", "--require-lock"), ExitOK, "0 candidate(s)", "No candidates.", "no lock file")

	expect(t, run("init", "--require-lock"), ExitOK, "require lock    on")
	expect(t, run("pool", "update", "--dry-run"), ExitOK, "0 candidate(s)", "no lock file")
	expect(t, run("pool", "update", "--dry-run", "--require-lock=false"), ExitOK, "1 candidate(s)", "Add double to pkg.core")
	// start follows the setting: its mining finds nothing, and says the setting is why. Its own --require-lock still parses.
	started := run("start", "--accept-mined")
	expect(t, started, ExitError, "Mining: no more candidates in 2 commit(s) read; the require-lock setting set aside 1 Python commit(s)",
		"agentium init --require-lock=false mines them")
	expect(t, run("start", "--require-lock", "extra"), ExitUsage)
	expect(t, run("init", "--require-lock=false"), ExitOK, "require lock    off")
	expect(t, run("start", "--require-lock", "--accept-mined"), ExitError, "the require-lock setting set aside 1 Python commit(s)")
}
