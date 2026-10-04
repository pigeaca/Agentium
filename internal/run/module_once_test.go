package run

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/task"
)

// moduleOnce is a monorepo whose module (a path of one or two folders) holds run_tests.sh (it runs tests/*.sh) and
// value.txt, plus a decoy module of the same shape whose run_tests.sh always passes: a grade that ran in the decoy, or at
// the root (which has no run_tests.sh), would show. The solution adds the hidden test module/tests/value_test.sh.
type moduleOnce struct {
	env  Env
	spec Spec
}

func newModuleOnce(t *testing.T, module, decoy, agent string) moduleOnce {
	t.Helper()
	return newModuleOnceWith(t, module, decoy, agent, nil)
}

// newModuleOnceWith is newModuleOnce with more files in the base commit (path from the root: content).
func newModuleOnceWith(t *testing.T, module, decoy, agent string, files map[string]string) moduleOnce {
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
	writeFile(t, filepath.Join(user, "README.md"), "a monorepo\n")
	writeFile(t, filepath.Join(user, module, "run_tests.sh"), "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n")
	writeFile(t, filepath.Join(user, module, "value.txt"), "old\n")
	writeFile(t, filepath.Join(user, decoy, "run_tests.sh"), "exit 0\n")
	writeFile(t, filepath.Join(user, decoy, "value.txt"), "old\n")
	for name, content := range files {
		writeFile(t, filepath.Join(user, filepath.FromSlash(name)), content)
	}
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "base")
	base := gitIn("rev-parse", "HEAD")
	hidden := filepath.ToSlash(filepath.Join(module, "tests", "value_test.sh"))
	writeFile(t, filepath.Join(user, hidden), "grep -q new value.txt\n")
	writeFile(t, filepath.Join(user, module, "value.txt"), "new\n")
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
	// The fake agent starts in the checkout's root (the module is not its folder yet), as the real one does.
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
		SignIn: claude.SignInLogin, VerifyTimeout: time.Minute, Grace: time.Second, Now: time.Now}
	spec := Spec{TaskName: "value", Instruction: "Make the value new.", Arm: task.Arm{Name: "base"}, Model: "claude-sonnet-5", BudgetUSD: 1,
		Timeout: time.Minute, Task: task.Spec{Base: base, Solution: solution, HiddenTests: []string{hidden}, Reference: []string{module + "/value.txt"},
			Verify: []string{"sh run_tests.sh"}, Module: module}}
	return moduleOnce{env: env, spec: spec}
}

// A run of a task in a module sets up, and grades, in the module's folder: run_tests.sh exists only there, and the
// setup's file lands there. The task's module is the run's, whatever else the Env says.
func TestOnceRunsSetupAndVerificationInTheTasksModule(t *testing.T) {
	f := newModuleOnce(t, "svc", "decoy", "printf 'new\\n' > svc/value.txt")
	f.spec.Task.Setup = []string{"touch setup-ran"}
	f.spec.Keep = true
	f.env.Module = "decoy" // the task wins over the Env
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || !*rec.Passed {
		t.Fatalf("solved: %s, passed %v, notes %v", rec.Outcome, rec.Passed, rec.Notes)
	}
	workspace := filepath.Join(f.env.Layout.Workspaces, f.env.workspaceName(), "repo")
	if _, err := os.Stat(filepath.Join(workspace, "svc", "setup-ran")); err != nil {
		t.Errorf("setup did not run in the module: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "setup-ran")); err == nil {
		t.Error("setup ran at the checkout's root")
	}
	if log, _ := os.ReadFile(filepath.Join(rec.RecordsDir, "verify.log")); !strings.Contains(string(log), "$ sh run_tests.sh") {
		t.Errorf("verify.log:\n%s", log)
	}
	// An idle agent fails the hidden test: the module's own run_tests.sh decided it.
	f = newModuleOnce(t, "svc", "decoy", "")
	if rec, err = Once(context.Background(), f.env, f.spec); err != nil || rec.Passed == nil || *rec.Passed {
		t.Errorf("idle: %v, %v", rec.Passed, err)
	}
}

// The module's folder in the agent's tree is not trusted: an agent that deletes it, replaces it with a link to another
// folder, or links one of its parents, gets a failed grade with a note (never a pass for another folder's tests, and
// never infrastructure, which would be retried and cost again).
func TestOnceFailsAModuleTheAgentRemovedOrLinked(t *testing.T) {
	for name, c := range map[string]struct{ module, decoy, agent string }{
		"deleted":         {"svc", "decoy", "rm -rf svc"},
		"linked":          {"svc", "decoy", "rm -rf svc && ln -s decoy svc"},
		"a file":          {"svc", "decoy", "rm -rf svc && echo x > svc"},
		"parent linked":   {"a/svc", "b/svc", "rm -rf a && ln -s b a"},
		"parent deleted":  {"a/svc", "b/svc", "rm -rf a"},
		"module replaced": {"a/svc", "b/svc", "rm -rf a/svc && ln -s ../b/svc a/svc"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newModuleOnce(t, c.module, c.decoy, c.agent)
			rec, err := Once(context.Background(), f.env, f.spec)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || *rec.Passed {
				t.Fatalf("outcome %s, passed %v, notes %v: want a graded fail", rec.Outcome, rec.Passed, rec.Notes)
			}
			if !strings.Contains(strings.Join(rec.Notes, "\n"), "the module's folder is not in the agent's tree") {
				t.Errorf("no note says why: %v", rec.Notes)
			}
		})
	}
}

// A module that is not in the checkout at setup is Agentium's own failure, found before any command runs.
func TestOnceRefusesASetupInAMissingModule(t *testing.T) {
	f := newModuleOnce(t, "svc", "decoy", "")
	f.spec.Task.Module = "nowhere"
	f.spec.Task.Setup = []string{"touch setup-ran"}
	if rec, err := Once(context.Background(), f.env, f.spec); err == nil || !strings.Contains(err.Error(), "setup") || rec.Passed != nil {
		t.Errorf("a missing module at setup: %v, passed %v", err, rec.Passed)
	}
}

// The module's own verification script is restored as the root's is: the commands run in the module's folder, so
// "sh run_tests.sh" names svc/run_tests.sh, and an agent that rewrites it to pass is graded with the starting version.
// A runner's configuration in the module (svc/Makefile, which make reads there) is reported when the agent changes it.
func TestOnceRestoresTheModulesChecks(t *testing.T) {
	f := newModuleOnce(t, "svc", "decoy", "printf 'exit 0\\n' > svc/run_tests.sh")
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || *rec.Passed {
		t.Fatalf("outcome %s, passed %v, notes %v: the rewritten script decided the grade", rec.Outcome, rec.Passed, rec.Notes)
	}
	if !slices.Contains(rec.Behavior.ChecksChanged, "svc/run_tests.sh") {
		t.Errorf("checks changed %q: want svc/run_tests.sh", rec.Behavior.ChecksChanged)
	}
	if !strings.Contains(strings.Join(rec.Notes, "\n"), "the agent changed the verification's own scripts; graded with the starting version: svc/run_tests.sh") {
		t.Errorf("no note says the script was restored: %v", rec.Notes)
	}

	// ": make" names the runner (all checkFiles reads of it) without needing make installed.
	f = newModuleOnceWith(t, "svc", "decoy", "printf 'new\\n' > svc/value.txt; printf 'all:\\n' >> svc/Makefile",
		map[string]string{"svc/Makefile": "test:\n\tsh run_tests.sh\n", "Makefile": "test:\n\texit 0\n"})
	f.spec.Task.Verify = []string{"sh run_tests.sh", ": make"}
	if rec, err = Once(context.Background(), f.env, f.spec); err != nil {
		t.Fatal(err)
	}
	if rec.Passed == nil || !*rec.Passed {
		t.Fatalf("passed %v, notes %v", rec.Passed, rec.Notes)
	}
	if !slices.Equal(rec.Behavior.ConfigChanged, []string{"svc/Makefile"}) || !slices.Contains(rec.Behavior.ChecksChanged, "svc/Makefile") {
		t.Errorf("config changed %q, checks changed %q: want svc/Makefile in both", rec.Behavior.ConfigChanged, rec.Behavior.ChecksChanged)
	}
}

// What the verification of a module depends on is read from the module's folder and listed from the root; the root's
// own files of the same names are not the module's, and an absolute path is outside the repository as at the root.
func TestCheckFilesInAModule(t *testing.T) {
	files := fakeSource{"Makefile", "run_tests.sh", "requirements.txt", "svc/Makefile", "svc/bin/sh", "svc/requirements-dev.txt",
		"svc/run_tests.sh", "svc/sub/requirements.txt", "tools/check.sh"}
	slices.Sort(files)
	scripts, configs := checkFiles([]string{"sh run_tests.sh && sh ../tools/check.sh", "make test", "/bin/sh -c pytest"}, "svc", files)
	slices.Sort(scripts)
	slices.Sort(configs)
	if want := []string{"svc/run_tests.sh", "tools/check.sh"}; !slices.Equal(scripts, want) {
		t.Errorf("scripts %q, want %q", scripts, want)
	}
	if want := []string{"svc/Makefile", "svc/requirements-dev.txt"}; !slices.Equal(configs, want) {
		t.Errorf("configs %q, want %q", configs, want)
	}
	// The root is unchanged.
	scripts, configs = checkFiles([]string{"sh run_tests.sh", "make test"}, "", files)
	if !slices.Equal(scripts, []string{"run_tests.sh"}) || !slices.Equal(configs, []string{"Makefile"}) {
		t.Errorf("at the root: scripts %q, configs %q", scripts, configs)
	}
}

// A grade's command finds the module's folder gone (a test of the agent's replaced it with a link between commands):
// the grade fails with a note, and is not an error (which would make the run infrastructure).
func TestGradeDirFailsAModuleGoneBetweenCommands(t *testing.T) {
	copy := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(copy, "decoy"), 0o700))
	must(t, os.MkdirAll(filepath.Join(copy, "svc"), 0o700))
	env := Env{Module: "svc"}
	var log strings.Builder
	var notes []string
	in := sandboxGrade{Log: &log, Note: func(n string) { notes = append(notes, n) }}
	if dir, ok := env.gradeDir(copy, in); !ok || dir != filepath.Join(copy, "svc") {
		t.Fatalf("the module's folder: %q, %v", dir, ok)
	}
	must(t, os.RemoveAll(filepath.Join(copy, "svc")))
	must(t, os.Symlink("decoy", filepath.Join(copy, "svc")))
	if dir, ok := env.gradeDir(copy, in); ok || dir != "" {
		t.Fatalf("a linked module: %q, %v", dir, ok)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "the module's folder left the agent's tree") || !strings.Contains(log.String(), notes[0]) {
		t.Errorf("notes %q, log %q", notes, log.String())
	}
}
