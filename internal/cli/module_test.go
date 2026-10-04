package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
		"not set: mined tasks verify with python3 -m pytest")
	if got := storedSettings(t, data); got.Module != "tools/report" || got.Jobs != 3 {
		t.Errorf("stored %+v: the module is stored with the other settings", got)
	}
	doc := jsonRun(t, f, ExitOK, "init", "--module", "./services//billing/")
	if doc.get("settings", "module") != "services/billing" || !equalAny(doc.get("test_commands"), "go test ./...") ||
		!equalAny(doc.get("settings", "mined_verify"), "go test ./...") { // the module's own test command
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

// monorepoHistory adds three commits to the monorepo fixture: one in services/billing (code and its test), one in
// tools/report, and the hostile one: billing's code with a test outside the module. It returns their hashes.
func monorepoHistory(t *testing.T, repo string) (billing, report, hostile string) {
	t.Helper()
	commit := func(message string, files map[string]string) string {
		for name, body := range files {
			writeFile(t, repo, name, body)
		}
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", message)
		return strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	}
	billing = commit("Add the invoice total to billing\n\nThe total sums the invoice lines in cents.", map[string]string{
		"services/billing/total.go":      "package main\n\nfunc total(lines []int) int {\n\tsum := 0\n\tfor _, l := range lines {\n\t\tsum += l\n\t}\n\treturn sum\n}\n",
		"services/billing/total_test.go": "package main\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) {\n\tif total([]int{1, 2}) != 3 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n"})
	report = commit("Sort the report by date\n\nThe report lists the newest entries first.", map[string]string{
		"tools/report/report.py": "def entries():\n    return []\n", "tools/report/test_report.py": "def test_ok():\n    assert True\n"})
	hostile = commit("Charge a late fee in billing\n\nA late payment pays a fixed fee of one euro.", map[string]string{
		"services/billing/fee.go": "package main\n\nfunc fee() int { return 100 }\n", "tools/report/test_fee.py": "def test_fee():\n    assert True\n"})
	return billing, report, hostile
}

// With a module set, pool update mines only the module's commits: another module's commit is outside it, and a commit
// whose tests sit outside the module is set aside (its hidden tests would never run there). The mined task records the
// module, and its hidden tests are the module's. Clearing the module mines the whole repository again.
func TestPoolMinesOnlyTheModule(t *testing.T) {
	t.Parallel()
	repo, data, run := monorepo(t)
	billing, report, hostile := monorepoHistory(t, repo)
	f := asFixture(t, repo, data, run)
	expect(t, run("init", "--module", "services/billing", "--verify", "test -f total.go"), ExitOK)
	dry := run("pool", "update", "--dry-run")
	expect(t, dry, ExitOK, "Would mine main at "+hostile[:12]+" in services/billing: 4 commit(s) since the last pass, 1 candidate(s)", "Add the invoice total to billing",
		"outside the module", "changes outside the module")
	for _, absent := range []string{"Sort the report by date", "Charge a late fee", "later version"} {
		if strings.Contains(dry.stdout, absent) {
			t.Errorf("the dry run offers or says %q:\n%s", absent, dry.stdout)
		}
	}
	doc := jsonRun(t, f, ExitOK, "pool", "update", "--dry-run")
	candidates, _ := doc.get("candidates").([]any)
	if doc.get("module") != "services/billing" || len(candidates) != 1 || doc.get("set_aside", "changes outside the module") != 1.0 ||
		doc.get("set_aside", "outside the module") == nil {
		t.Errorf("pool update --dry-run --json in a module: %s", doc.stdout)
	}
	if first, _ := candidates[0].(map[string]any); !strings.HasPrefix(billing, fmt.Sprint(first["commit"])) {
		t.Errorf("the candidate is %v, want %s", first["commit"], billing)
	}

	expect(t, run("pool", "update"), ExitOK, "Mined main at "+hostile[:12]+" in services/billing", "Imported 1 of 1 candidate(s) tried")
	tasks := jsonRun(t, f, ExitOK, "task", "list").get("tasks").([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks: %v", tasks)
	}
	name := tasks[0].(map[string]any)["name"].(string)
	if status := tasks[0].(map[string]any)["status"]; status != "valid" {
		t.Errorf("the mined task's validation in the module: %v", status)
	}
	shown := jsonRun(t, f, ExitOK, "task", "show", name)
	if shown.get("module") != "services/billing" || !equalAny(shown.get("hidden_tests"), "services/billing/total_test.go") ||
		!equalAny(shown.get("reference"), "services/billing/total.go") {
		t.Errorf("the mined task: %s", shown.stdout)
	}
	expect(t, run("task", "validate", name), ExitOK, "want pass got pass") // in the module's folder: total.go is there

	// The whole repository again: the other commits are candidates, and the module's pass did not move the root's watermark.
	expect(t, run("init", "--module", ""), ExitOK)
	root := jsonRun(t, f, ExitOK, "pool", "update", "--dry-run")
	var commits []string
	for _, c := range root.get("candidates").([]any) {
		commits = append(commits, fmt.Sprint(c.(map[string]any)["commit"]))
	}
	if root.get("module") != nil || !slices.ContainsFunc(commits, func(c string) bool { return strings.HasPrefix(report, c) }) ||
		!slices.ContainsFunc(commits, func(c string) bool { return strings.HasPrefix(hostile, c) }) {
		t.Errorf("at the root again: %s", root.stdout)
	}
}

// task import into a module refuses a commit whose tests are outside it: they would be hidden tests the module's
// commands never run. At the root, or into the module the tests are in, the same commit's tests are not the problem.
func TestTaskImportRefusesTestsOutsideTheModule(t *testing.T) {
	t.Parallel()
	repo, _, run := monorepo(t)
	billing, _, hostile := monorepoHistory(t, repo)
	expect(t, run("init", "--module", "services/billing", "--verify", "true"), ExitOK)
	expect(t, run("task", "import", "--commit", hostile), ExitError, "changes test files outside the module services/billing (tools/report/test_fee.py)")
	expect(t, run("task", "import", "--commit", billing), ExitOK, "1 hidden test file(s)")
	expect(t, run("task", "import", "--commit", hostile, "--module", ""), ExitOK, "1 hidden test file(s)")
}

// start with a module set mines that module's history, and says where it mined. A baseline snapshot taken at the root
// is named as such: its arm loads none of the module's own .claude files.
func TestStartMinesInTheModule(t *testing.T) {
	t.Parallel()
	repo, data, _ := monorepo(t)
	monorepoHistory(t, repo)
	f := runFixtureAt(repo, data, t.TempDir())
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, t.TempDir())
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	expect(t, f.run(ctx, "context", "snapshot", "baseline"), ExitOK)
	expect(t, f.run(ctx, "init", "--module", "services/billing", "--verify", "test -f total.go"), ExitOK)
	got := f.run(ctx, "start", "--yes")
	expect(t, got, ExitError, "Mining: 1 candidate(s) in 4 commit(s) read in services/billing; imported 1 of 1 tried", "1 valid of 1",
		"snapshot baseline was taken for the repository's root, not for the module services/billing")
	if strings.Contains(got.stdout, "later version") || strings.Contains(got.stdout, "does not mine") {
		t.Errorf("start says it does not mine:\n%s", got.stdout)
	}
}

// With a module set, the context is what a session started in the module loads (where the module's tasks' agents
// start): the root's CLAUDE.md and the module's, both at start, and the module's skill; a snapshot holds them all and
// names the module. At the root the module's CLAUDE.md loads on demand and its skill is no context.
func TestContextOfAModule(t *testing.T) {
	t.Parallel()
	repo, data, run := monorepo(t)
	writeFile(t, repo, "CLAUDE.md", "Root rules.\n")
	writeFile(t, repo, "services/billing/CLAUDE.md", "Billing rules.\n")
	writeFile(t, repo, "services/billing/.claude/skills/pay/SKILL.md", "---\nname: pay\ndescription: Pay an invoice\n---\nbody\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "context")
	f := asFixture(t, repo, data, run)
	expect(t, run("init", "--module", "services/billing"), ExitOK)
	expect(t, run("context", "show"), ExitOK, "started in the module services/billing")
	shown := jsonRun(t, f, ExitOK, "context", "show")
	kinds := map[string]string{}
	for _, e := range shown.get("entries").([]any) {
		entry := e.(map[string]any)
		kinds[entry["path"].(string)] = entry["kind"].(string)
	}
	want := map[string]string{"CLAUDE.md": "instructions", "services/billing/CLAUDE.md": "instructions", "services/billing/.claude/skills/pay/SKILL.md": "skill"}
	if !maps.Equal(kinds, want) || shown.get("module") != "services/billing" {
		t.Errorf("context show in the module: %v (%s)", kinds, shown.stdout)
	}
	expect(t, run("context", "snapshot", "billing"), ExitOK, "Saved snapshot billing from HEAD", "3 file(s)")
	files := strings.TrimSpace(gitIn(t, filepath.Join(data, "projects", "1", "repo.git"), "ls-tree", "-r", "--name-only", "refs/agentium/snapshots/billing"))
	if files != "CLAUDE.md\nservices/billing/.claude/skills/pay/SKILL.md\nservices/billing/CLAUDE.md" {
		t.Errorf("the snapshot holds:\n%s", files)
	}

	expect(t, run("init", "--module", ""), ExitOK)
	root := jsonRun(t, f, ExitOK, "context", "show")
	kinds = map[string]string{}
	for _, e := range root.get("entries").([]any) {
		entry := e.(map[string]any)
		kinds[entry["path"].(string)] = entry["kind"].(string)
	}
	if !maps.Equal(kinds, map[string]string{"CLAUDE.md": "instructions", "services/billing/CLAUDE.md": "nested"}) || root.get("module") != nil {
		t.Errorf("context show at the root: %v", kinds)
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
