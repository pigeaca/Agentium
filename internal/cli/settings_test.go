package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/store"
)

// init prints the project's settings, every one at its default for a project that set none, and stores those it is
// given (only those: init again keeps the others); its JSON has them too. Values no command can use are refused.
func TestInitStoresAndPrintsSettings(t *testing.T) {
	t.Parallel()
	repo, data, run := validateRepo(t)
	f := asFixture(t, repo, data, run)
	first := run("init")
	expect(t, first, ExitOK, "Settings (in the data folder; agentium init --FLAG VALUE changes one, the others stay)",
		"verify          not set, and no test command was detected: agentium init --verify CMD sets one", "setup           none (default)", "require lock    off (default)",
		"jobs            2 (default)", "verify timeout  10m (default)")
	if strings.Contains(first.stdout, "local ports") {
		t.Errorf("a project without Gradle shows local ports:\n%s", first.stdout)
	}
	doc := jsonRun(t, f, ExitOK, "init")
	if got := doc.get("settings", "defaults"); !equalAny(got, "verify", "setup", "require_lock", "jobs", "verify_timeout") ||
		doc.get("settings", "jobs") != 2.0 || doc.get("settings", "verify_timeout_seconds") != 600.0 || doc.get("settings", "require_lock") != false {
		t.Errorf("init --json without settings: %s", doc.stdout)
	}

	set := run("init", "--verify", "make test", "--verify", "make lint", "--setup", "make assets", "--require-lock", "--jobs", "3", "--verify-timeout", "90s")
	expect(t, set, ExitOK, "verify          make test; make lint", "setup           make assets", "require lock    on", "jobs            3",
		"verify timeout  1m30s")
	if strings.Contains(set.stdout, "(default)") {
		t.Errorf("every setting is set, yet one shows its default:\n%s", set.stdout)
	}
	expect(t, run("init", "--jobs", "4"), ExitOK, "verify          make test; make lint", "jobs            4", "verify timeout  1m30s")
	want := store.Settings{Verify: []string{"make test", "make lint"}, Setup: []string{"make assets"}, RequireLock: true, Jobs: 4, VerifyTimeout: 90e9}
	if got := storedSettings(t, data); !settingsEqual(got, want) {
		t.Errorf("stored %+v, want %+v", got, want)
	}
	doc = jsonRun(t, f, ExitOK, "init")
	if !equalAny(doc.get("settings", "verify"), "make test", "make lint") || !equalAny(doc.get("settings", "mined_verify"), "make test", "make lint") ||
		!equalAny(doc.get("settings", "defaults")) || doc.get("settings", "jobs") != 4.0 || doc.get("settings", "verify_timeout_seconds") != 90.0 {
		t.Errorf("init --json with settings: %s", doc.stdout)
	}

	expect(t, run("init", "--jobs", "0"), ExitUsage, "agentium init: --jobs must be at least 1")
	expect(t, run("init", "--verify-timeout", "0s"), ExitUsage, "--verify-timeout must be more than 0")
	expect(t, run("init", "--verify-timeout", "soon"), ExitUsage, "invalid value")
	if got := storedSettings(t, data); !settingsEqual(got, want) {
		t.Errorf("a refused init changed the settings: %+v", got)
	}
	expect(t, run("init", "--verify", "", "--setup", "", "--require-lock=false"), ExitOK, "setup           none (default)", "require lock    off (default)",
		"jobs            4")
	if got := storedSettings(t, data); len(got.Verify) != 0 || len(got.Setup) != 0 || got.RequireLock || got.Jobs != 4 {
		t.Errorf("after clearing: %+v", got)
	}
}

// The jobs and verify-timeout settings drive task validate; its own --jobs and --timeout (hidden) win for that call
// only.
func TestTaskValidateUsesTheSettings(t *testing.T) {
	t.Parallel()
	_, data, run := validateRepo(t)
	probeDir := t.TempDir()
	probe := filepath.Join(probeDir, "probe.sh")
	writeFile(t, probeDir, "probe.sh", "d="+probeDir+"\nmkdir \"$d/run.$$\"\nls -d \"$d\"/run.* | wc -l >> \"$d/counts\"\nsleep 1\nrmdir \"$d/run.$$\"\n")
	for _, name := range []string{"t1", "t2", "t3"} {
		expect(t, run("task", "add", name, "--base", "HEAD", "--instruction", "Anything.", "--verify", "sh "+probe), ExitOK)
	}
	counts := func() int {
		t.Helper()
		n := maxCount(t, filepath.Join(probeDir, "counts"))
		if err := os.Remove(filepath.Join(probeDir, "counts")); err != nil {
			t.Fatal(err)
		}
		return n
	}
	expect(t, run("init", "--jobs", "1"), ExitOK)
	expect(t, run("task", "validate", "--all"), ExitOK, "Validating 3 task(s) in 1 arm(s), 1 at a time")
	if n := counts(); n != 1 {
		t.Errorf("the jobs setting 1 ran %d at once", n)
	}
	expect(t, run("task", "validate", "--all", "--jobs", "3"), ExitOK, "3 at a time")
	if n := counts(); n < 2 {
		t.Errorf("--jobs 3 ran %d at once at most", n)
	}
	expect(t, run("task", "validate", "--all"), ExitOK, "1 at a time") // the override was for that call only
	counts()

	expect(t, run("task", "add", "slow", "--base", "HEAD", "--instruction", "Anything.", "--verify", "sleep 3"), ExitOK)
	expect(t, run("init", "--verify-timeout", "1s"), ExitOK, "verify timeout  1s")
	expect(t, run("task", "validate", "slow"), ExitError, "timed out")
	expect(t, run("task", "validate", "slow", "--timeout", "1m"), ExitOK, "Result: unchecked")
	if got := storedSettings(t, data); got.Jobs != 1 || got.VerifyTimeout.Seconds() != 1 {
		t.Errorf("an override changed the settings: %+v", got)
	}
}

// Removed flags fail with a usage error that names the replacement, as text and in --json's error document;
// --instruction @FILE reads a file (relative to the working folder), and @@ starts a text with @.
func TestRemovedFlagsAndInstructionFiles(t *testing.T) {
	t.Parallel()
	repo, data, run := validateRepo(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"init", "--no-allow-local-binding"}, "agentium init: --no-allow-local-binding was removed: use --allow-local-binding=false"},
		{[]string{"task", "add", "x", "--base", "HEAD", "--instruction-file", "i.md"}, "agentium task add: --instruction-file was removed: use --instruction @FILE"},
		{[]string{"task", "add", "x", "--base", "HEAD", "--instruction-file=i.md"}, "--instruction-file was removed: use --instruction @FILE"},
		{[]string{"task", "edit", "x", "--instruction-file"}, "agentium task edit: --instruction-file was removed: use --instruction @FILE"},
		{[]string{"task", "mine", "--limit", "3"}, "agentium task mine: removed: agentium pool update mines"},
	} {
		got := run(c.args...)
		expect(t, got, ExitUsage, c.want)
		if strings.Contains(got.stderr, "Usage:") || strings.Contains(got.stderr, "not defined") {
			t.Errorf("%q: %s", c.args, got.stderr)
		}
	}
	removed := jsonRun(t, asFixture(t, repo, data, run), ExitUsage, "task", "add", "x", "--base", "HEAD", "--instruction-file", "i.md")
	if msg, _ := removed.get("error", "message").(string); !strings.Contains(msg, "use --instruction @FILE") {
		t.Errorf("--json: %s", removed.stdout)
	}

	writeFile(t, repo, "notes/instruction.md", "  Make the value 2.\n\nKeep the file.\n")
	expect(t, run("task", "add", "from-file", "--base", "HEAD", "--instruction", "@notes/instruction.md", "--verify", "true"), ExitOK)
	expect(t, run("task", "add", "literal", "--base", "HEAD", "--instruction", "@@mention the owner", "--verify", "true"), ExitOK)
	expect(t, run("task", "add", "missing", "--base", "HEAD", "--instruction", "@notes/none.md", "--verify", "true"), ExitUsage, "read instruction")
	expect(t, run("task", "add", "empty", "--base", "HEAD", "--instruction", "@", "--verify", "true"), ExitUsage, "--instruction @ needs a file name")
	writeFile(t, repo, "notes/edited.md", "Make the value 3.\n")
	expect(t, run("task", "edit", "literal", "--instruction", "@"+filepath.Join(repo, "notes/edited.md")), ExitOK)
	byName := map[string]string{}
	for _, task := range storedTasks(t, data) {
		byName[task.Name] = task.Instruction
	}
	if byName["from-file"] != "Make the value 2.\n\nKeep the file." || byName["literal"] != "Make the value 3." || len(byName) != 2 {
		t.Errorf("instructions %q", byName)
	}
	expect(t, run("task", "add", "at", "--base", "HEAD", "--instruction", "@@alice owns this", "--verify", "true"), ExitOK)
	if got := storedTasks(t, data); !slices.ContainsFunc(got, func(t store.Task) bool { return t.Name == "at" && t.Instruction == "@alice owns this" }) {
		t.Errorf("@@: %+v", got)
	}
}

// hiddenFlags are the expert flags and per-command overrides that still parse but are left out of each command's
// usage text, with the usage they must stay out of. Each is listed in the guide's "Advanced flags" table.
var hiddenFlags = map[string][]string{
	"pool":    {"--verify", "--setup", "--require-lock", "--jobs", "--verify-timeout", "--since", "--max-files", "--max-lines"},
	"start":   {"--require-lock"},
	"task":    {"--max-hunks", "--keep", "--jobs", "--timeout", "--require-lock", "--since", "--max-files", "--max-lines"},
	"context": {"--include-linked"},
}

// removedFlags are flags that no longer exist; no usage text names them.
var removedFlags = []string{"--no-allow-local-binding", "--instruction-file"}

// The usage texts list none of the hidden or removed flags (task add and edit keep --verify and --setup), and name no
// removed command; every hidden flag still parses; the guide's advanced-flags table lists each one.
func TestUsageListsNoHiddenFlags(t *testing.T) {
	t.Parallel()
	usages := map[string]string{"init": initUsage, "task": taskUsage, "pool": poolUsage, "start": startUsage, "context": contextUsage, "": usage}
	flagName := regexp.MustCompile(`(^|[^\w-])(--[a-z][a-z0-9-]*)`)
	for command, text := range usages {
		named := map[string]bool{}
		for _, m := range flagName.FindAllStringSubmatch(text, -1) {
			named[m[2]] = true
		}
		for _, f := range append(slices.Clone(hiddenFlags[command]), removedFlags...) {
			if named[f] {
				t.Errorf("the usage of %q lists %s", command, f)
			}
		}
		if strings.Contains(text, "task mine") {
			t.Errorf("the usage of %q names task mine", command)
		}
	}

	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(guide), "## Advanced flags")
	if !ok {
		t.Fatal(`the guide has no "## Advanced flags" section`)
	}
	table, _, _ = strings.Cut(table, "\n## ")
	for command, flags := range hiddenFlags {
		for _, f := range flags {
			if !strings.Contains(table, "`"+f) {
				t.Errorf("the guide's advanced flags lack %s (%s)", f, command)
			}
		}
	}

	// They still parse: a mistake later in the line is reported, never an unknown flag.
	repo, _, run := validateRepo(t)
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "second")
	expect(t, run("task", "add", "one", "--base", "HEAD", "--instruction", "Anything.", "--verify", "true"), ExitOK)
	for _, args := range [][]string{
		{"pool", "update", "--dry-run", "--verify", "true", "--setup", "true", "--require-lock", "--jobs", "1", "--verify-timeout", "1m",
			"--since", "2026-01-01", "--max-files", "3", "--max-lines", "10", "extra"},
		{"start", "--require-lock", "extra"},
		{"task", "validate", "one", "--weak-tests", "--max-hunks", "2", "--keep", "--timeout", "1m", "--repeat", "0"},
		{"task", "validate", "--all", "--jobs", "1", "--repeat", "0"},
		{"task", "import", "--verify", "true", "--setup", "true"},
		{"context", "snapshot", "s", "--include-linked", "--ref", "HEAD", "--working-tree"},
	} {
		got := run(args...)
		if got.code != ExitUsage || strings.Contains(got.stderr, "not defined") {
			t.Errorf("%q: exit %d, %s", args, got.code, got.stderr)
		}
	}
}

// asFixture is a runFixture over cliIn's run, for jsonRun.
func asFixture(t *testing.T, repo, data string, run func(args ...string) cliResult) runFixture {
	return runFixture{repo: repo, data: data, home: t.TempDir(), run: func(_ context.Context, args ...string) cliResult { return run(args...) }}
}

// storedSettings reads the data folder's only project's settings.
func storedSettings(t *testing.T, data string) store.Settings {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(context.Background())
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	return projects[0].Settings
}

func settingsEqual(a, b store.Settings) bool {
	return slices.Equal(a.Verify, b.Verify) && slices.Equal(a.Setup, b.Setup) && a.RequireLock == b.RequireLock && a.Jobs == b.Jobs &&
		a.VerifyTimeout == b.VerifyTimeout
}

// equalAny reports whether v, a decoded JSON list, holds exactly these strings.
func equalAny(v any, want ...string) bool {
	list, ok := v.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i, w := range want {
		if list[i] != w {
			return false
		}
	}
	return true
}
