package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// monorepo is a repository with no build file at its root and three modules: Go, Python (pytest) and Maven, plus a
// nested Go module inside the Maven one (folded into it) and folders that are never modules.
func monorepo(t *testing.T) (repo, data string, run func(args ...string) cliResult) {
	t.Helper()
	repo = t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	for name, body := range map[string]string{
		"README.md":                          "a monorepo\n",
		"services/billing/go.mod":            "module example.com/billing\n\ngo 1.22\n",
		"services/billing/main.go":           "package main\n\nfunc main() {}\n",
		"tools/report/pyproject.toml":        "[project]\nname = \"report\"\n[tool.pytest.ini_options]\n",
		"tools/report/test_report.py":        "def test_ok():\n    pass\n",
		"backend/pom.xml":                    "<project/>\n",
		"backend/inner/go.mod":               "module example.com/inner\n",
		"node_modules/dep/package.json":      "{}\n",
		"docs/guide.md":                      "text\n",
		"a/b/c/d/deep/go.mod":                "module example.com/toodeep\n",
		"a/b/c/shallow/Cargo.toml":           "[package]\nname = \"s\"\n",
		"services/billing/internal/x/go.mod": "module example.com/nested\n",
	} {
		writeFile(t, repo, name, body)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	data = filepath.Join(t.TempDir(), "data")
	return repo, data, cliIn(t, repo, data)
}

// With no build file at the root, init lists the folders that could be modules: up to four levels deep, a nested one
// folded into its parent, hidden and dependency folders skipped.
func TestInitListsTheModulesOfAMonorepo(t *testing.T) {
	t.Parallel()
	repo, data, run := monorepo(t)
	first := run("init")
	expect(t, first, ExitOK, "Modules (the root has no build file; agentium init --module PATH measures one)",
		"a/b/c/shallow     cargo", "backend           maven", "services/billing  go", "tools/report      python")
	for _, absent := range []string{"inner", "toodeep", "node_modules", "internal/x", "(chosen)", "module  "} {
		if strings.Contains(first.stdout, absent) {
			t.Errorf("init lists or shows %q:\n%s", absent, first.stdout)
		}
	}
	f := asFixture(t, repo, data, run)
	doc := jsonRun(t, f, ExitOK, "init")
	modules, _ := doc.get("modules").([]any)
	if len(modules) != 4 || doc.get("modules_total") != 4.0 {
		t.Fatalf("init --json modules: %s", doc.stdout)
	}
	if first, _ := modules[1].(map[string]any); first["path"] != "backend" || !equalAny(first["tools"], "maven") {
		t.Errorf("the second module = %v", modules[1])
	}
	if _, ok := doc.get("settings").(map[string]any)["module"]; ok {
		t.Errorf("a project with no module names one: %s", doc.stdout)
	}
}

// A repository whose root has a build file lists no modules, and shows no module setting.
func TestInitWithABuildFileAtTheRootListsNoModules(t *testing.T) {
	t.Parallel()
	repo, _, run := monorepo(t)
	writeFile(t, repo, "go.mod", "module example.com/root\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "root module")
	out := run("init")
	expect(t, out, ExitOK)
	if strings.Contains(out.stdout, "Modules (") || strings.Contains(out.stdout, "  module  ") {
		t.Errorf("a root build file, yet init talks of modules:\n%s", out.stdout)
	}
}

// init --module stores the module (with the other settings, which it keeps), prints it, describes the module's folder
// (its test command, not the root's), refuses what is not a module, and --module ” returns to the whole repository.
func TestInitModuleSettingStoresAndRefuses(t *testing.T) {
	t.Parallel()
	repo, data, run := monorepo(t)
	f := asFixture(t, repo, data, run)
	expect(t, run("init", "--jobs", "3"), ExitOK)
	set := run("init", "--module", "tools/report")
	expect(t, set, ExitOK, "module          tools/report", "tools/report      python  (chosen)", "python3 -m pytest",
		"tasks added or imported verify with python3 -m pytest (mining inside a module comes in a later version)")
	if got := storedSettings(t, data); got.Module != "tools/report" || got.Jobs != 3 {
		t.Errorf("stored %+v: the module is stored with the other settings", got)
	}
	doc := jsonRun(t, f, ExitOK, "init", "--module", "./services//billing/")
	if doc.get("settings", "module") != "services/billing" || !equalAny(doc.get("test_commands"), "go test ./...") ||
		!equalAny(doc.get("settings", "mined_verify")) { // nothing is mined inside a module yet
		t.Errorf("init --json with a module: %s", doc.stdout)
	}
	expect(t, run("init"), ExitOK, "module          services/billing") // init again keeps it
	if got := storedSettings(t, data); got.Module != "services/billing" {
		t.Errorf("init without the flag changed the module: %+v", got)
	}

	if err := os.Symlink(filepath.Join(repo, "backend"), filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ module, want string }{
		{"nowhere", "there is no such folder"}, {"docs", "no build file Agentium knows"}, {"README.md", "is not a folder"},
		{"..", "inside the repository"}, {"../outside", "inside the repository"}, {"/etc", "inside the repository"},
		{"services/../..", "inside the repository"}, {".", "the repository's root"}, {"linked", "is a link"},
	} {
		expect(t, run("init", "--module", c.module), ExitUsage, "agentium init: invalid module", c.want)
		if got := storedSettings(t, data); got.Module != "services/billing" {
			t.Errorf("--module %q was refused, yet the setting changed to %q", c.module, got.Module)
		}
	}

	expect(t, run("init", "--module", ""), ExitOK)
	if got := storedSettings(t, data); got.Module != "" || got.Jobs != 3 {
		t.Errorf("after --module '': %+v", got)
	}
	cleared := jsonRun(t, f, ExitOK, "init")
	if _, ok := cleared.get("settings").(map[string]any)["module"]; ok || len(cleared.get("test_commands").([]any)) != 0 {
		t.Errorf("the root again: %s", cleared.stdout)
	}
}

// A task records its module when it is made (the project's setting, or its own hidden --module), and runs there for good:
// task validate uses the task's module, so changing the project's setting moves no task. task show and task list name it.
func TestTasksKeepTheirModule(t *testing.T) {
	t.Parallel()
	repo, data, run := monorepo(t)
	f := asFixture(t, repo, data, run)
	expect(t, run("init"), ExitOK)
	expect(t, run("task", "add", "root", "--base", "HEAD", "--instruction", "Anything.", "--verify", "test -f go.mod"), ExitOK)
	expect(t, run("task", "validate", "root"), ExitError, "want pass got fail") // the root has no go.mod

	expect(t, run("init", "--module", "services/billing"), ExitOK)
	expect(t, run("task", "add", "mod", "--base", "HEAD", "--instruction", "Anything.", "--verify", "test -f go.mod && test -f main.go"), ExitOK)
	expect(t, run("task", "add", "other", "--base", "HEAD", "--instruction", "Anything.", "--module", "tools/report", "--verify", "test -f pyproject.toml"), ExitOK)
	expect(t, run("task", "add", "atroot", "--base", "HEAD", "--instruction", "Anything.", "--module", "", "--verify", "test -f README.md"), ExitOK)
	expect(t, run("task", "validate", "mod"), ExitOK, "got pass")
	expect(t, run("task", "validate", "other"), ExitOK, "got pass")
	expect(t, run("task", "validate", "atroot"), ExitOK, "got pass")
	expect(t, run("task", "validate", "root"), ExitError, "want pass got fail") // made before the setting: still a root task

	// The setting is only the default for new tasks.
	expect(t, run("init", "--module", ""), ExitOK)
	expect(t, run("task", "validate", "mod"), ExitOK, "got pass")
	expect(t, run("task", "add", "later", "--base", "HEAD", "--instruction", "Anything.", "--verify", "test -f README.md"), ExitOK)
	expect(t, run("task", "validate", "later"), ExitOK, "got pass") // the root again

	expect(t, run("task", "show", "mod"), ExitOK, "module     services/billing")
	if out := run("task", "show", "root"); strings.Contains(out.stdout, "module ") {
		t.Errorf("a root task shows a module:\n%s", out.stdout)
	}
	expect(t, run("task", "list"), ExitOK, "MODULE", "services/billing", "tools/report")
	shown := jsonRun(t, f, ExitOK, "task", "show", "mod")
	if shown.get("module") != "services/billing" {
		t.Errorf("task show --json: %s", shown.stdout)
	}
	if rootShown := jsonRun(t, f, ExitOK, "task", "show", "root"); rootShown.get("module") != nil {
		t.Errorf("a root task's json names a module: %s", rootShown.stdout)
	}
	expect(t, run("task", "add", "bad", "--base", "HEAD", "--instruction", "x", "--module", "docs"), ExitUsage, "invalid module")
}

// While the project's module is set, pool update and start mine nothing and say why; init does not offer a mined
// verify command. Clearing the module brings mining back.
func TestMiningWaitsWhileAModuleIsSet(t *testing.T) {
	t.Parallel()
	_, _, run := monorepo(t)
	expect(t, run("init", "--module", "services/billing"), ExitOK)
	expect(t, run("pool", "update"), ExitOK, "module set: mining inside a module comes in a later version; use task import --commit or task add")
	if out := run("init"); strings.Contains(out.stdout, "mined tasks verify") {
		t.Errorf("init offers a mined verify command in a module:\n%s", out.stdout)
	}
}

// start with a module set mines nothing and says so in its own words: the history was not read, so it does not claim
// the history has no more candidates, and it says how to go on (tasks in the module, or the whole repository).
func TestStartWithAModuleSetDoesNotMine(t *testing.T) {
	t.Parallel()
	repo, data, _ := monorepo(t)
	f := runFixtureAt(repo, data, t.TempDir())
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, t.TempDir())
	ctx := context.Background()
	expect(t, f.run(ctx, "init", "--module", "services/billing"), ExitOK)
	got := f.run(ctx, "start", "--yes")
	expect(t, got, ExitError, "Mining: module set: mining inside a module comes in a later version",
		"only 0 of the 8 an experiment needs are ready",
		"start does not mine while the module services/billing is set: add tasks in the module with agentium task add or agentium task import --commit REF, then run agentium start again",
		`agentium init --module "" measures the whole repository`)
	if n := strings.Count(got.stdout, "Mining: "); n != 1 || strings.Contains(got.stdout, "no more candidates") {
		t.Errorf("mined %d time(s), or blamed the history:\n%s", n, got.stdout)
	}
}

// Validation prepares the build tools of the task's module, not the root's: a Python module's checks get Python's
// command environment (its import root, in the module), which a root with no build file would not give them.
func TestValidationPreparesTheTasksModule(t *testing.T) {
	t.Parallel()
	_, _, run := monorepo(t)
	expect(t, run("init"), ExitOK) // the setting stays the root's: the task's own module decides
	expect(t, run("task", "add", "report", "--base", "HEAD", "--instruction", "Anything.", "--module", "tools/report",
		"--verify", `case "$PYTHONPATH" in */tools/report*) exit 0;; esac; exit 1`), ExitOK)
	expect(t, run("task", "validate", "report"), ExitOK, "want pass got pass")
}
