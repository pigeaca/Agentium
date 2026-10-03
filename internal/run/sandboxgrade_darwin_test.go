package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/sandbox"
	"github.com/pigeaca/agentium/internal/task"
)

// needSandbox skips unless sandbox-exec can apply a profile here (macOS, not nested in another sandbox) and the
// unified log shows its denials (sandbox.Usable).
func needSandbox(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("starts sandboxed processes")
	}
	err := sandbox.Usable(context.Background())
	skipLogBlind(t, err)
	if err != nil {
		t.Skipf("the grading sandbox: %v", err)
	}
}

// logBlindSkip starts the message of every test skipped because this account's unified log did not show the
// sandbox's denials (in time): CI lists the skipped tests, and this names the reason.
const logBlindSkip = "LOG-BLIND: the unified log did not show the sandbox's denials here (in time)"

// skipLogBlind skips when err says the log did not show the sandbox's denials (sandbox.ErrDenialsUnread): on GitHub's
// macOS runners logd is slow and at times never shows a probe's denial, so a run's Usable check (or a grade's denial
// read) can fail after needSandbox passed.
func skipLogBlind(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, sandbox.ErrDenialsUnread) {
		t.Skipf("%s: %v", logBlindSkip, err)
	}
}

// needDenials skips when a grade's denials could not be read (task.SandboxGrade.Unread): what the test asserts of them
// is unknown then.
func needDenials(t *testing.T, g *task.SandboxGrade) {
	t.Helper()
	if g != nil && g.Unread != "" {
		t.Skipf("%s: %s", logBlindSkip, g.Unread)
	}
}

// onceFixture is a project (a repository whose run_tests.sh runs tests/*.sh), its bare repository holding a base and a
// solution that adds the hidden test tests/value_test.sh and makes value.txt "new", a data folder, and an Env for
// Once with a fake Claude Code that runs agent in its checkout.
type onceFixture struct {
	env  Env
	spec Spec
}

func newOnceFixture(t *testing.T, agent, hidden string) onceFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	user := filepath.Join(dir, "user")
	gitIn := func(args ...string) string {
		t.Helper()
		out, err := gitx.Run(ctx, append([]string{"-C", user, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	must(t, os.MkdirAll(user, 0o700))
	gitIn("init", "-q", "-b", "main")
	writeFile(t, filepath.Join(user, "run_tests.sh"), "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n")
	writeFile(t, filepath.Join(user, "value.txt"), "old\n")
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "base")
	base := gitIn("rev-parse", "HEAD")
	writeFile(t, filepath.Join(user, "tests", "value_test.sh"), hidden)
	writeFile(t, filepath.Join(user, "value.txt"), "new\n")
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "make the value new")
	solution := gitIn("rev-parse", "HEAD")

	layout, err := home.Resolve(func(key string) string { return map[string]string{"AGENTIUM_HOME": filepath.Join(dir, "data")}[key] })
	must(t, err)
	must(t, layout.Ensure())
	bare := filepath.Join(layout.Root, "projects", "1", "repo.git")
	must(t, gitx.InitBare(ctx, bare))
	for _, c := range []string{base, solution} {
		must(t, gitx.FetchCommit(ctx, user, c, gitx.SourceRef(c), "--git-dir", bare))
	}
	cli := filepath.Join(dir, "claude")
	writeFile(t, cli, "#!/bin/sh\n"+agent+"\n"+`cat <<'EOF'
{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5","permissionMode":"acceptEdits","tools":["Bash"],"skills":[],"slash_commands":[]}
{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.25,"num_turns":2,"duration_ms":1000,"modelUsage":{}}
EOF
`)
	must(t, os.Chmod(cli, 0o755))
	homeDir := filepath.Join(dir, "home")
	must(t, os.MkdirAll(homeDir, 0o700))
	id, err := NewID(time.Now())
	must(t, err)
	env := Env{ID: id, Layout: layout, Bare: bare, ProjectRoot: user, CLI: cli, Home: homeDir, Environ: []string{"PATH=/usr/bin:/bin", "HOME=" + homeDir},
		SignIn: claude.SignInLogin, VerifyTimeout: time.Minute, Grace: time.Second, Now: time.Now, Grader: task.GraderSandbox}
	spec := Spec{TaskName: "value", Instruction: "Make the value new.", Arm: task.Arm{Name: "base"}, Model: "claude-sonnet-5", BudgetUSD: 1,
		Timeout: time.Minute, Task: task.Spec{Base: base, Solution: solution, HiddenTests: []string{"tests/value_test.sh"}, Reference: []string{"value.txt"},
			Verify: []string{"sh run_tests.sh"}}}
	return onceFixture{env: env, spec: spec}
}

// gone fails when any of paths is still there.
func gone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s is still there", p)
		}
	}
}

// A run graded in the sandbox: the hidden test is added and the verification runs under the grading profile, in the
// run's grade folder, which goes afterwards with the copy; the record names the mode and what the sandbox reported. A
// solving agent's pass stays a pass, an idle agent's fail stays a fail (its denials are ones the agent's sandbox
// imposes too), and --keep keeps the copy where host mode keeps it.
func TestOnceGradesInTheSandbox(t *testing.T) {
	needSandbox(t)
	f := newOnceFixture(t, "printf 'new\\n' > value.txt", "grep -q new value.txt\n")
	var steps []string
	f.env.Step = func(s string) { steps = append(steps, s) }
	rec, err := Once(context.Background(), f.env, f.spec)
	skipLogBlind(t, err)
	if err != nil {
		t.Fatal(err)
	}
	// Each moment, in order, for a live display: the grade's cleanup, then the run's.
	if want := []string{StepPreparing, StepDependencies, StepAgent, StepGrading, StepSandbox, StepTests, StepCleanup, StepCleanup}; !slices.Equal(steps, want) {
		t.Errorf("steps %q, want %q", steps, want)
	}
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || !*rec.Passed || rec.Grader != task.GraderSandbox {
		t.Fatalf("solved: %s, passed %v, grader %q, notes %v", rec.Outcome, rec.Passed, rec.Grader, rec.Notes)
	}
	needDenials(t, rec.Sandbox)
	if g := rec.Sandbox; g == nil || g.Canary != task.CanaryPassed || len(g.Profile) != 64 || g.Unread != "" || g.FlaggedCount != 0 {
		t.Errorf("the sandbox's report: %+v", g)
	}
	gone(t, filepath.Join(rec.RecordsDir, gradingFolder), filepath.Join(rec.RecordsDir, "verify"))
	if log, _ := os.ReadFile(filepath.Join(rec.RecordsDir, "verify.log")); !strings.Contains(string(log), "$ sh run_tests.sh") {
		t.Errorf("verify.log:\n%s", log)
	}

	// An idle agent: the hidden test fails, and reads a denied file on the way (the agent's sandbox denies it too).
	f = newOnceFixture(t, "", "cat \"$HOME/.ssh/id_test\" 2>/dev/null; grep -q new value.txt\n")
	must(t, os.MkdirAll(filepath.Join(f.env.Home, ".ssh"), 0o700))
	writeFile(t, filepath.Join(f.env.Home, ".ssh", "id_test"), "not a key\n")
	rec, err = Once(context.Background(), f.env, f.spec)
	skipLogBlind(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || *rec.Passed {
		t.Fatalf("idle: %s, passed %v, notes %v", rec.Outcome, rec.Passed, rec.Notes)
	}
	needDenials(t, rec.Sandbox)
	if g := rec.Sandbox; g == nil || g.DenialCount == 0 || g.FlaggedCount != 0 {
		t.Errorf("the idle run's denials: %+v", g)
	}

	// --keep: the copy comes back from the grade's folder.
	f = newOnceFixture(t, "printf 'new\\n' > value.txt", "grep -q new value.txt\n")
	f.spec.Keep = true
	rec, err = Once(context.Background(), f.env, f.spec)
	skipLogBlind(t, err)
	if err != nil || rec.Passed == nil || !*rec.Passed {
		t.Fatalf("kept: %v, %v", rec.Passed, err)
	}
	if data, err := os.ReadFile(filepath.Join(rec.RecordsDir, "verify", "tests", "value_test.sh")); err != nil || !strings.Contains(string(data), "grep") {
		t.Errorf("the kept copy: %q, %v", data, err)
	}
	gone(t, filepath.Join(rec.RecordsDir, gradingFolder))
	removeTree(filepath.Join(f.env.Layout.Workspaces))
}

// Fail closed: when the canary shows the sandbox does not hold, nothing is graded, and the run is infrastructure
// (retried or left out): never a fail, never a host grade. A sandbox that cannot be used at all refuses the run
// before the agent starts.
func TestOnceCanaryFailureIsInfrastructure(t *testing.T) {
	needSandbox(t)
	marker := filepath.Join(t.TempDir(), "graded")
	f := newOnceFixture(t, "printf 'new\\n' > value.txt", "touch '"+marker+"'; grep -q new value.txt\n")
	f.env.canary = func(context.Context, string, string, sandbox.Profile) ([]int, error) {
		return nil, fmt.Errorf("%w: sandbox-exec exited 65 (nested)", sandbox.ErrUnavailable)
	}
	var steps []string
	f.env.Step = func(s string) { steps = append(steps, s) }
	rec, err := Once(context.Background(), f.env, f.spec)
	// The display hears that the sandbox did not start, and never that the tests ran.
	if want := []string{StepPreparing, StepDependencies, StepAgent, StepGrading, StepSandbox, StepCleanup, StepSandboxDown, StepCleanup}; err == nil && !slices.Equal(steps, want) {
		t.Errorf("steps %q, want %q", steps, want)
	}
	skipLogBlind(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != claude.OutcomeInfra || rec.Passed != nil || rec.Sandbox == nil || !strings.Contains(rec.Sandbox.Canary, "nested") ||
		!strings.Contains(strings.Join(rec.Notes, "\n"), "the grading sandbox is unavailable, so nothing was graded") {
		t.Errorf("canary failed: %s, passed %v, sandbox %+v, notes %v", rec.Outcome, rec.Passed, rec.Sandbox, rec.Notes)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the verification ran (on the host?) though the canary failed")
	}
	gone(t, filepath.Join(rec.RecordsDir, gradingFolder), filepath.Join(rec.RecordsDir, "verify"))

	f = newOnceFixture(t, "", "true\n")
	f.env.Grader = "sandbox-v0"
	if rec, err := Once(context.Background(), f.env, f.spec); err == nil || rec.Outcome != "" {
		t.Errorf("an unknown grader: %s, %v", rec.Outcome, err)
	}
}

// Decision 3: a failed grade with denials the agent's own sandbox does not impose (here the security server's lookup,
// which Claude Code's sandbox allows) is infrastructure; the same denial in a passing grade leaves the pass a pass.
func TestOnceFlaggedDenials(t *testing.T) {
	needSandbox(t)
	lookup := "/usr/bin/security find-generic-password -s agentium-made-up-service >/dev/null 2>&1; "
	f := newOnceFixture(t, "", lookup+"grep -q new value.txt\n")
	rec, err := Once(context.Background(), f.env, f.spec)
	skipLogBlind(t, err)
	if err != nil {
		t.Fatal(err)
	}
	needDenials(t, rec.Sandbox)
	if rec.Outcome != OutcomeSandboxFlagged || rec.Passed != nil || rec.Sandbox == nil || rec.Sandbox.FlaggedCount == 0 ||
		rec.Sandbox.FlaggedOperations() != "mach-lookup" || !strings.Contains(strings.Join(rec.Notes, "\n"), "not counted and not tried again") {
		t.Errorf("a flagged failure: %s, passed %v, sandbox %+v, notes %v", rec.Outcome, rec.Passed, rec.Sandbox, rec.Notes)
	}
	for _, n := range rec.Notes { // shared notes never carry what the grade chose
		if strings.Contains(n, "SecurityServer") {
			t.Errorf("a note names the denial's target: %s", n)
		}
	}

	f = newOnceFixture(t, "printf 'new\\n' > value.txt", lookup+"grep -q new value.txt\n")
	rec, err = Once(context.Background(), f.env, f.spec)
	skipLogBlind(t, err)
	if err != nil {
		t.Fatal(err)
	}
	needDenials(t, rec.Sandbox)
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || !*rec.Passed || rec.Sandbox == nil || rec.Sandbox.FlaggedCount == 0 {
		t.Errorf("a flagged pass: %s, passed %v, sandbox %+v", rec.Outcome, rec.Passed, rec.Sandbox)
	}

	// The same denial, logged by the task's reference while passing its validation, is harmless for the task: a failed
	// grade with it is the agent's fail.
	f = newOnceFixture(t, "", lookup+"grep -q new value.txt\n")
	f.spec.HarmlessDenials = []task.DenialKey{{Operation: "mach-lookup", Target: "com.apple.SecurityServer"}}
	rec, err = Once(context.Background(), f.env, f.spec)
	skipLogBlind(t, err)
	if err != nil {
		t.Fatal(err)
	}
	needDenials(t, rec.Sandbox)
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || *rec.Passed || rec.Sandbox == nil || rec.Sandbox.FlaggedCount != 0 || rec.Sandbox.Harmless == 0 {
		t.Errorf("a harmless denial: %s, passed %v, sandbox %+v", rec.Outcome, rec.Passed, rec.Sandbox)
	}
}

// What a run's grade left behind is a counted note; the process names (the grade's choice) stay in verify.log.
func TestOnceGradeWarningsStayInTheLog(t *testing.T) {
	needSandbox(t)
	if _, err := os.Stat("/usr/bin/perl"); err != nil {
		t.Skip("no perl to call setsid:", err)
	}
	leftover := `/usr/bin/perl -e 'use POSIX; exit 0 if fork; POSIX::setsid(); chdir "/"; open STDOUT, ">", "/dev/null"; open STDERR, ">", "/dev/null"; $0 = "hidden-SECRET"; sleep 300'`
	f := newOnceFixture(t, "printf 'new\\n' > value.txt", leftover+"\ngrep -q new value.txt\n")
	rec, err := Once(context.Background(), f.env, f.spec)
	skipLogBlind(t, err)
	if err != nil || rec.Passed == nil || !*rec.Passed {
		t.Fatalf("%v, %v, notes %v", rec.Passed, err, rec.Notes)
	}
	notes := strings.Join(rec.Notes, "\n")
	if !strings.Contains(notes, "grading: 1 warning(s)") || strings.Contains(notes, "perl") || strings.Contains(notes, "SECRET") {
		t.Errorf("notes %q", notes)
	}
	if log, _ := os.ReadFile(filepath.Join(rec.RecordsDir, "verify.log")); !strings.Contains(string(log), "it was stopped: ") {
		t.Errorf("verify.log:\n%s", log)
	}
}

// F7: a grade's processes die with it. A `setsid` child leaves the process group runner kills, and this one also moves
// to / and closes every file, so nothing of it lies in the grade's folders; it is found by its sandbox and stopped
// before the grade's folders go.
func TestSetsidChildDiesWithTheGrade(t *testing.T) {
	needSandbox(t)
	if _, err := os.Stat("/usr/bin/perl"); err != nil {
		t.Skip("no perl to call setsid:", err)
	}
	f := newGradeFixture(t)
	homeDir := filepath.Join(f.dir, "home")
	must(t, os.MkdirAll(homeDir, 0o700))
	f.env.Home, f.env.Environ = homeDir, []string{"PATH=/usr/bin:/bin", "HOME=" + homeDir}
	child := `/usr/bin/perl -e 'use POSIX; my $p = fork; if ($p) { print "child=$p\n"; exit 0 } POSIX::setsid(); chdir "/"; ` +
		`open STDIN, "<", "/dev/null"; open STDOUT, ">", "/dev/null"; open STDERR, ">", "/dev/null"; sleep 300'`
	var log strings.Builder
	var warnings []string
	in := sandboxGrade{Root: f.root, Copy: f.copy, Agent: claude.Invocation{Tools: []string{"go"}, Home: homeDir, Deps: f.deps, SignIn: claude.SignInLogin}, Base: commitA,
		Commands: []string{child}, Timeout: time.Minute, Log: &log, Running: func(int) {}, Warn: func(w string) { warnings = append(warnings, w) }}
	_, ok, report, err := f.env.gradeInSandbox(context.Background(), in)
	skipLogBlind(t, err)
	if err != nil || !ok || report == nil || report.Canary != task.CanaryPassed {
		t.Fatalf("the grade: ok %v, %+v, %v\n%s", ok, report, err, log.String())
	}
	m := regexp.MustCompile(`child=(\d+)`).FindStringSubmatch(log.String())
	if m == nil {
		t.Fatalf("no child started:\n%s", log.String())
	}
	pid, _ := strconv.Atoi(m[1])
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the setsid child %d outlived its grade (%v); warnings %v", pid, err, warnings)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), fmt.Sprintf("it was stopped: %d ", pid)) {
		t.Errorf("the stopped child is not reported: %v", warnings)
	}
	gone(t, f.root)

	// While it runs, the grade cannot rename or remove its temp root or its copy (its folder is locked), which the
	// sweep tells its processes by.
	in.Root, in.Copy = filepath.Join(filepath.Dir(f.root), "grading-2"), f.newCopy(f.root)
	in.Commands = []string{`! mv "$TMPDIR" "${TMPDIR}x" 2>/dev/null && ! rm -rf "$TMPDIR" 2>/dev/null && test -d "$TMPDIR" && ! mv "$PWD" "${PWD}x" 2>/dev/null`}
	if _, ok, _, err := f.env.gradeInSandbox(context.Background(), in); err != nil || !ok {
		skipLogBlind(t, err)
		t.Errorf("the grade could move its own folders: %v, %v", ok, err)
	}

	// The folder sweep alone does not see such a process: only its sandbox tells it apart.
	cmd := exec.Command("/usr/bin/perl", "-e", `use POSIX; POSIX::setsid(); chdir "/"; sleep 300`)
	must(t, cmd.Start())
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if found, err := processesUnder([]string{f.root}); err != nil || len(found) != 0 {
		t.Errorf("the folder sweep found %v, %v", found, err)
	}
}

// A run whose Agentium died while its grade ran leaves the grade's folder locked (0500) and maybe a process of the
// grade's sandbox; recovery stops it and removes the folder.
func TestRecoverALockedSandboxedGrade(t *testing.T) {
	needSandbox(t)
	f := newGradeFixture(t)
	dir := filepath.Join(f.env.Layout.Records, "r9")
	grade := filepath.Join(dir, gradingFolder)
	tmp := filepath.Join(grade, "tmp")
	must(t, os.MkdirAll(tmp, 0o700))
	profile := fmt.Sprintf(`(version 1)(deny default)(allow process-exec)(allow process-fork)(allow file-read*)(allow sysctl-read)(allow mach-lookup)(allow file-write* (subpath %q))`,
		sandbox.RealForm(tmp))
	cmd := exec.Command(sandbox.Exec, "-p", profile, "/bin/sleep", "300")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	must(t, cmd.Start())
	ended := make(chan struct{})
	go func() { cmd.Wait(); close(ended) }()
	t.Cleanup(func() { cmd.Process.Kill() })
	time.Sleep(200 * time.Millisecond)
	must(t, os.Chmod(grade, 0o500))
	must(t, (Env{}).writeStart(start{Record: Record{ID: "r9", RecordsDir: dir}, Workspace: filepath.Join(f.env.Layout.Workspaces, "r9"), AgentStarted: true}))
	if _, err := Recover(context.Background(), f.env.Layout, func(string) (bool, error) { return false, nil }, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	endsSoon(t, "a dead run's sandboxed grade process", ended)
	gone(t, grade)
}

// A log that does not show one grade's denials in time neither stops the run (nor so an experiment) nor changes its
// result: the denials are recorded as unread, with a note, and the tests' result stands.
func TestOnceWithALaggingLog(t *testing.T) {
	needSandbox(t)
	f := newOnceFixture(t, "printf 'new\\n' > value.txt", "grep -q new value.txt\n")
	f.env.readDenials = func(context.Context, string, sandbox.Profile, time.Time, time.Duration, []int) ([]sandbox.Denial, error) {
		return nil, fmt.Errorf("%w: the probe's denial did not reach the unified log within 10s", sandbox.ErrDenialsUnread)
	}
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatalf("a lagging log stopped the run: %v", err)
	}
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || !*rec.Passed || rec.Sandbox == nil || !strings.Contains(rec.Sandbox.Unread, "did not reach") ||
		!strings.Contains(strings.Join(rec.Notes, "\n"), "its denials could not be read") {
		t.Errorf("%s, passed %v, sandbox %+v, notes %v", rec.Outcome, rec.Passed, rec.Sandbox, rec.Notes)
	}
}
