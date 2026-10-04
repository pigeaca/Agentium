package run

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/task"
)

// moduleOnce is a monorepo whose module (a path of one or two folders) holds run_tests.sh (it runs tests/*.sh) and
// value.txt, plus a decoy module of the same shape whose run_tests.sh always passes: a grade that ran in the decoy, or at
// the root (which has no run_tests.sh), would show. The solution adds the hidden test module/tests/value_test.sh.
type moduleOnce struct {
	env  Env
	spec Spec
	// started and args are files the fake agent writes: the folder it started in (resolved), and its arguments, one a line.
	started, args string
}

func newModuleOnce(t *testing.T, module, decoy, agent string) moduleOnce {
	t.Helper()
	return newModuleOnceWith(t, module, decoy, agent, nil)
}

// newModuleOnceWith is newModuleOnce with more files in the base commit (path from the root: content); a content of
// "symlink:TARGET" makes the path a symbolic link to TARGET.
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
		if target, ok := strings.CutPrefix(content, "symlink:"); ok {
			must(t, os.MkdirAll(filepath.Dir(filepath.Join(user, filepath.FromSlash(name))), 0o755))
			must(t, os.Symlink(target, filepath.Join(user, filepath.FromSlash(name))))
			continue
		}
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
	// The fake agent records where it started (the module's folder, as the real one starts there) and its arguments, then
	// runs the test's commands from the checkout's root, where they name their files.
	started, args := filepath.Join(dir, "started"), filepath.Join(dir, "args")
	writeFile(t, cli, "#!/bin/sh\nstart=$(pwd -P)\nprintf '%s\\n' \"$start\" > "+started+"\nprintf '%s\\n' \"$@\" > "+args+
		"\ncd \"${start%/"+module+"}\"\n"+agent+"\n"+`cat <<'EOF'
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
		Timeout: time.Minute, Task: task.Spec{Base: base, Solution: solution, HiddenTests: []string{hidden}, Reference: []string{path.Join(module, "value.txt")},
			Verify: []string{"sh run_tests.sh"}, Module: module}}
	return moduleOnce{env: env, spec: spec, started: started, args: args}
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

// What the verification of a module depends on is read from the module's folder and listed from the root: scripts
// where the command names them (an absolute path is outside the repository, as at the root), and runner configuration
// in the module's folder and every folder above it, which the runners read too.
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
	// A subfolder's requirements are no ancestor's.
	if want := []string{"Makefile", "requirements.txt", "svc/Makefile", "svc/requirements-dev.txt"}; !slices.Equal(configs, want) {
		t.Errorf("configs %q, want %q", configs, want)
	}
	// The root is unchanged.
	scripts, configs = checkFiles([]string{"sh run_tests.sh", "make test"}, "", files)
	if !slices.Equal(scripts, []string{"run_tests.sh"}) || !slices.Equal(configs, []string{"Makefile"}) {
		t.Errorf("at the root: scripts %q, configs %q", scripts, configs)
	}

	// Each runner's configuration up the tree: Maven's parent pom.xml and .mvn, pytest's root pyproject.toml and
	// conftest.py, Gradle's root build files, Cargo's workspace and .cargo; for a module two folders deep, the folder
	// between too.
	tree := fakeSource{".cargo/config.toml", ".mvn/maven.config", "Cargo.toml", "a/b/conftest.py", "a/b/pom.xml", "a/conftest.py",
		"a/pom.xml", "build.gradle", "conftest.py", "gradle.properties", "gradlew", "pom.xml", "pyproject.toml", "settings.gradle",
		"svc/Cargo.toml", "svc/pom.xml"}
	slices.Sort(tree)
	for _, c := range []struct {
		module, verify string
		want           []string
	}{
		{"svc", "mvn test", []string{".mvn/maven.config", "pom.xml", "svc/pom.xml"}},
		{"svc", "cargo test", []string{".cargo/config.toml", "Cargo.toml", "svc/Cargo.toml"}},
		{"svc", "python -m pytest", []string{"conftest.py", "pyproject.toml"}},
		{"svc", "../gradlew test", []string{"build.gradle", "gradle.properties", "settings.gradle"}},
		{"a/b", "mvn test", []string{".mvn/maven.config", "a/b/pom.xml", "a/pom.xml", "pom.xml"}},
		{"a/b", "pytest", []string{"a/b/conftest.py", "a/conftest.py", "conftest.py", "pyproject.toml"}},
	} {
		_, configs := checkFiles([]string{c.verify}, c.module, tree)
		slices.Sort(configs)
		if !slices.Equal(configs, c.want) {
			t.Errorf("%s in %s: configs %q, want %q", c.verify, c.module, configs, c.want)
		}
	}
}

// An agent that edits a runner's configuration above the module (Maven's parent pom.xml, a root conftest.py that
// skips every test) is reported as one that edits the module's: ConfigChanged keeps the run from counting as a
// success (experiment.Success), however the tests came out.
func TestOnceReportsAnAncestorsRunnerConfig(t *testing.T) {
	f := newModuleOnceWith(t, "svc", "decoy", "printf 'new\\n' > svc/value.txt; printf '<!-- skip -->\\n' >> pom.xml; printf 'skip = 1\\n' >> conftest.py",
		map[string]string{"pom.xml": "<project/>\n", "svc/pom.xml": "<project/>\n", "conftest.py": "\n"})
	// ": mvn" and ": python -m pytest" name the runners (all checkFiles reads of them) without needing them installed.
	f.spec.Task.Verify = []string{"sh run_tests.sh", ": mvn", ": python -m pytest"}
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Passed == nil || !*rec.Passed {
		t.Fatalf("passed %v, notes %v", rec.Passed, rec.Notes)
	}
	got := slices.Sorted(slices.Values(rec.Behavior.ConfigChanged))
	if !slices.Equal(got, []string{"conftest.py", "pom.xml"}) {
		t.Errorf("config changed %q: want the root's conftest.py and pom.xml", rec.Behavior.ConfigChanged)
	}
}

// A script due for restore whose folder the agent turned into a link (or a file) cannot be restored: the grade fails
// with a note, never graded with the agent's script and never infrastructure (tried again at a cost). The note names
// paths from the copy's root, not where the copy lies.
func TestOnceFailsAScriptThatCannotBeRestored(t *testing.T) {
	for name, agent := range map[string]string{
		"linked": "mkdir elsewhere && printf 'exit 0\\n' > elsewhere/check.sh && rm -rf svc/scripts && ln -s ../elsewhere svc/scripts",
		"a file": "rm -rf svc/scripts && echo 'exit 0' > svc/scripts",
	} {
		t.Run(name, func(t *testing.T) {
			f := newModuleOnceWith(t, "svc", "decoy", agent, map[string]string{"svc/scripts/check.sh": "sh run_tests.sh\n"})
			f.spec.Task.Verify = []string{"sh scripts/check.sh"}
			rec, err := Once(context.Background(), f.env, f.spec)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || *rec.Passed {
				t.Fatalf("outcome %s, passed %v, notes %v: want a graded fail", rec.Outcome, rec.Passed, rec.Notes)
			}
			notes := strings.Join(rec.Notes, "\n")
			if !strings.Contains(notes, "could not be restored, so nothing was graded") || strings.Contains(notes, rec.RecordsDir) {
				t.Errorf("notes %v: want why, without the copy's location", rec.Notes)
			}
			if !slices.Contains(rec.Behavior.ChecksChanged, "svc/scripts/check.sh") || len(rec.Verify) != 0 {
				t.Errorf("checks changed %q, commands %+v", rec.Behavior.ChecksChanged, rec.Verify)
			}
		})
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

// TestInAncestorsEndsOnAnAbsoluteModule: path.Dir("/") is "/", never ".", so a walk that stopped only at "." would
// never end on a corrupted absolute module and hang the grade.
func TestInAncestorsEndsOnAnAbsoluteModule(t *testing.T) {
	done := make(chan []string, 1)
	go func() { done <- inAncestors("/svc", []string{"pom.xml"}) }()
	select {
	case got := <-done:
		if want := []string{"/svc/pom.xml", "pom.xml"}; !slices.Equal(got, want) {
			t.Fatalf("inAncestors(/svc) = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inAncestors did not end on an absolute module")
	}
}

// The agent of a task in a module starts in the module's folder, as a developer of the module would; the whole checkout
// stays its own (a working folder it may edit, the sandbox's allowWrite). The record names the module, and the context
// the run started with is what a session there loads: the root's CLAUDE.md and the module's. A root task starts at the
// checkout's root with no extra working folder, and its record has no module.
func TestOnceStartsTheAgentInTheTasksModule(t *testing.T) {
	f := newModuleOnceWith(t, "svc/api", "decoy", "printf 'new\\n' > svc/api/value.txt",
		map[string]string{"CLAUDE.md": "root rules\n", "svc/api/CLAUDE.md": "module rules\n", "decoy/CLAUDE.md": "decoy\n"})
	f.spec.Keep = true
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || !*rec.Passed || rec.Module != "svc/api" {
		t.Fatalf("outcome %s, passed %v, module %q, notes %v", rec.Outcome, rec.Passed, rec.Module, rec.Notes)
	}
	repo, err := filepath.EvalSymlinks(filepath.Join(f.env.Layout.Workspaces, f.env.workspaceName(), "repo"))
	must(t, err)
	if started := readTrimmed(t, f.started); started != filepath.Join(repo, "svc", "api") {
		t.Errorf("the agent started in %s, want the module's folder in %s", started, repo)
	}
	args := strings.Split(readTrimmed(t, f.args), "\n")
	i := slices.Index(args, "--add-dir")
	if i < 0 || i+1 >= len(args) || !sameFolder(args[i+1], repo) {
		t.Errorf("the whole checkout is no working folder of the agent: %v", args)
	}
	var settings struct {
		Sandbox struct {
			Filesystem struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	must(t, json.Unmarshal([]byte(args[slices.Index(args, "--settings")+1]), &settings))
	if !slices.ContainsFunc(settings.Sandbox.Filesystem.AllowWrite, func(p string) bool { return sameFolder(p, repo) }) {
		t.Errorf("the sandbox does not let the agent write its checkout: %v", settings.Sandbox.Filesystem.AllowWrite)
	}
	if rec.ContextUse == nil || !slices.Equal(rec.ContextUse.Start, []string{"CLAUDE.md", "svc/api/CLAUDE.md"}) {
		t.Errorf("the context at start: %+v", rec.ContextUse)
	}
	if data, err := json.Marshal(rec); err != nil || !strings.Contains(string(data), `"module":"svc/api"`) {
		t.Errorf("the record's JSON: %s, %v", data, err)
	}

	root := newModuleOnceWith(t, "", "decoy", "printf 'new\\n' > value.txt", map[string]string{"CLAUDE.md": "root rules\n", "decoy/CLAUDE.md": "decoy\n"})
	root.spec.Keep = true
	if rec, err = Once(context.Background(), root.env, root.spec); err != nil || rec.Passed == nil || !*rec.Passed {
		t.Fatalf("the root task: %v, %v, %v", rec.Passed, rec.Notes, err)
	}
	repo, err = filepath.EvalSymlinks(filepath.Join(root.env.Layout.Workspaces, root.env.workspaceName(), "repo"))
	must(t, err)
	if started := readTrimmed(t, root.started); started != repo {
		t.Errorf("a root task's agent started in %s, want %s", started, repo)
	}
	if args := readTrimmed(t, root.args); strings.Contains(args, "--add-dir") {
		t.Errorf("a root task's agent has another working folder: %s", args)
	}
	if data, _ := json.Marshal(rec); strings.Contains(string(data), `"module"`) || !slices.Equal(rec.ContextUse.Start, []string{"CLAUDE.md"}) {
		t.Errorf("a root task's record: %s", data)
	}
}

// A module that is no longer a real folder when the agent would start (the task's setup replaced it with a link) is
// Agentium's own failure, before the agent starts: it would start somewhere else.
func TestOnceRefusesToStartTheAgentInALinkedModule(t *testing.T) {
	f := newModuleOnce(t, "svc", "decoy", "")
	f.spec.Task.Setup = []string{"cd .. && mv svc svc-real && ln -s svc-real svc"}
	_, err := Once(context.Background(), f.env, f.spec)
	if err == nil || !strings.Contains(err.Error(), "the agent's folder") || !strings.Contains(err.Error(), "is a link") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(f.started); statErr == nil {
		t.Error("the agent started")
	}
}

func readTrimmed(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(file)
	must(t, err)
	return strings.TrimSpace(string(data))
}

// sameFolder reports whether a and b are one folder once links are resolved.
func sameFolder(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// An arm's snapshot is applied as a session in the task's module loads context: the module's own .claude files that
// the snapshot lacks are gone from the arm's checkout (at the root they would be no context, and stay), and its
// instruction files are the snapshot's.
func TestOnceAppliesAnArmInTheTasksModule(t *testing.T) {
	f := newModuleOnceWith(t, "svc", "decoy", "printf 'new\\n' > svc/value.txt", map[string]string{"CLAUDE.md": "root rules\n",
		"svc/CLAUDE.md": "module rules\n", "svc/.claude/skills/pay/SKILL.md": "---\nname: pay\ndescription: Pay\n---\n"})
	lean, manifest, err := snapshot.BuildIn(context.Background(), f.env.Bare, memSource{"CLAUDE.md": "root rules\n", "svc/CLAUDE.md": "lean\n"},
		"snapshot lean", nil, "svc")
	must(t, err)
	if manifest.Module != "svc" {
		t.Fatalf("manifest %+v", manifest)
	}
	f.spec.Arm, f.spec.Keep = task.Arm{Name: "lean", Snapshot: lean}, true
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil || rec.Passed == nil || !*rec.Passed {
		t.Fatalf("passed %v, notes %v, %v", rec.Passed, rec.Notes, err)
	}
	repo := filepath.Join(f.env.Layout.Workspaces, f.env.workspaceName(), "repo")
	if _, err := os.Stat(filepath.Join(repo, "svc", ".claude", "skills", "pay", "SKILL.md")); err == nil {
		t.Error("the module's skill, which the snapshot lacks, is still in the arm's checkout")
	}
	if got := readTrimmed(t, filepath.Join(repo, "svc", "CLAUDE.md")); got != "lean" {
		t.Errorf("the module's CLAUDE.md is %q", got)
	}
}
