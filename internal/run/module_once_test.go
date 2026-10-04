package run

import (
	"context"
	"os"
	"path/filepath"
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
