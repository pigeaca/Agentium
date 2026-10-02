package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// assertKeys fails when the object's keys are not exactly the comma-separated list: a renamed, dropped or added field
// of a public document changes this list on purpose, never by accident.
func assertKeys(t *testing.T, v any, want string) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Errorf("not an object: %v", v)
		return
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != want {
		t.Errorf("keys\n got  %s\n want %s", got, want)
	}
}

const (
	taskInfoKeys = "base_commit,graded_by,hidden_test_files,name,needs_review,reference_files,solution_commit,source,status,status_summary,unstated_requirements,untested_hunks"
	behaviorKeys = "bash_commands,checks_changed,commits,config_changed,denials,files_changed,lines_added,lines_removed,outside_reads,ran_checks,ran_tests,tests_changed,tests_removed"
	runKeys      = "arm,behavior,cli_version,cost_estimated,cost_usd,drift,duration_ms,effort,finished,first_request_tokens,id,judge_cost_usd,model,notes,outcome,passed,permission_mode,sign_in,skills,started,task,tools,turns"
)

func TestJSONFieldNamesAreFixed(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	keys := func(r jsonResult, want string) { t.Helper(); assertKeys(t, r.doc, want) }
	row := func(r jsonResult, list, want string) { t.Helper(); assertKeys(t, r.get(list).([]any)[0], want) }

	keys(jsonRun(t, f, ExitOK, "init"), "claude_code,command,context,head,local_binding,project,schema,sign_in,test_commands,warnings")
	show := jsonRun(t, f, ExitOK, "context", "show")
	keys(show, "above_repository_files,command,context,entries,linked,project,schema,warnings,where")
	row(show, "entries", "bytes,kind,path,startup_bytes,via")
	assertKeys(t, show.get("context"), "on_demand_files,startup_files,startup_tokens_estimated")
	keys(jsonRun(t, f, ExitOK, "context", "snapshot", "one"), "command,commit,files,name,not_included_linked,schema,source,startup_tokens_estimated,uncaptured_changes,warnings")
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nBe brief.\n")
	jsonRun(t, f, ExitOK, "context", "snapshot", "two", "--working-tree")
	list := jsonRun(t, f, ExitOK, "context", "list")
	keys(list, "command,schema,snapshots")
	row(list, "snapshots", "commit,files,name,source,startup_tokens_estimated,warnings")
	keys(jsonRun(t, f, ExitOK, "context", "diff", "one", "two"), "changed,command,delta_tokens_estimated,from,from_tokens_estimated,patch,schema,stat,to,to_tokens_estimated")
	lint := jsonRun(t, f, ExitOK, "context", "lint")
	keys(lint, "command,no_comparison,problems,project,schema,snapshot,startup_tokens_estimated,warnings,where")
	assertKeys(t, lint.get("snapshot"), "delta_tokens_estimated,name,startup_tokens_estimated")

	tasks := jsonRun(t, f, ExitOK, "task", "list")
	keys(tasks, "command,schema,tasks")
	row(tasks, "tasks", taskInfoKeys)
	shown := jsonRun(t, f, ExitOK, "task", "show", "value")
	keys(shown, "base_commit,command,graded_by,hidden_test_files,hidden_tests,instruction,instruction_names_reference_files,name,needs_review,reference,reference_files,review,"+
		"schema,setup,solution_commit,source,status,status_summary,unstated_requirement_details,unstated_requirements,untested_hunks,verify,weak_tests")
	imported := jsonRun(t, f, ExitOK, "task", "import", "--commit", "HEAD", "--name", "again", "--verify", "sh run_tests.sh")
	keys(imported, "command,next_command,schema,setup,solution_leak_sections,task,verify")
	assertKeys(t, imported.get("task"), taskInfoKeys)
	edited := jsonRun(t, f, ExitOK, "task", "edit", "again", "--reviewed")
	keys(edited, "command,schema,task,updated")
	validated := jsonRun(t, f, ExitOK, "task", "validate", "value", "--weak-tests")
	keys(validated, "arms,command,harness_changed,judge,needs_review,repeats,schema,status,summary,task,unstated_requirement_details,weak_tests")
	assertKeys(t, validated.get("weak_tests"), "checked,reason,skipped,timed_out,untested")
	all := jsonRun(t, f, ExitOK, "task", "validate", "--all")
	keys(all, "command,interrupted,schema,tasks,total,valid")
	row(all, "tasks", "commit,name,problem,task")
	keys(jsonRun(t, f, ExitOK, "task", "rm", "again"), "command,removed,schema")

	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt", false)
	once := jsonRun(t, f, ExitOK, "run", "once", "value")
	keys(once, "command,run,schema")
	assertKeys(t, once.get("run"), runKeys)
	assertKeys(t, once.get("run", "behavior"), behaviorKeys)
	runs := jsonRun(t, f, ExitOK, "run", "list")
	keys(runs, "command,runs,schema")
	row(runs, "runs", "arm,cost_usd,id,kind,outcome,passed,started,task")
	id := once.get("run", "id").(string)
	plain := jsonRun(t, f, ExitOK, "run", "show", id)
	keys(plain, "command,diff,experiment,files,kind,run,schema,setup_log,verify_log") // present, and null when not asked for
	for _, k := range []string{"diff", "setup_log", "verify_log", "experiment"} {
		if v, present := plain.doc[k]; !present || v != nil {
			t.Errorf("run show: %s = %v (present %v), want null", k, v, present)
		}
	}
	asked := jsonRun(t, f, ExitOK, "run", "show", id, "--diff", "--log")
	if asked.get("diff") == nil || asked.get("verify_log") == nil || asked.get("setup_log") != nil {
		t.Errorf("run show --diff --log: %s", asked.stdout)
	}

	failed := jsonRun(t, f, ExitError, "task", "show", "nope")
	keys(failed, "command,error,schema")
	assertKeys(t, failed.get("error"), "code,message")
}

func TestJSONMineKeys(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 3)
	jsonRun(t, f, ExitOK, "init")
	keys := "candidates,candidates_found,command,commits_read,dry_run,head,imported,interrupted,ref,schema,set_aside,shallow,tasks,tried,valid,verify"
	dry := jsonRun(t, f, ExitOK, "task", "mine", "--dry-run", "--limit", "1")
	assertKeys(t, dry.doc, keys)
	assertKeys(t, dry.get("candidates").([]any)[0], "changed_lines,code_files,commit,score,score_parts,subject,test_files")
	imp := jsonRun(t, f, ExitOK, "task", "mine", "--limit", "1")
	assertKeys(t, imp.doc, keys)
	assertKeys(t, imp.get("tasks").([]any)[0], "commit,name,problem,task")
}

// A judge-graded task has no hidden tests; validating it runs nothing, so no paid call is possible. Its document must
// have the same lists as any other, never null.
func TestJSONJudgeGradedValidateHasNoNulls(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	writeFile(t, f.repo, "value.txt", "newer\n") // code only: no test file changes
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "Make the value newer")
	jsonRun(t, f, ExitOK, "task", "add", "judged", "--base", "HEAD~1", "--solution", "HEAD", "--judge-graded", "--instruction", "Make the value newer.", "--verify", "true")
	got := jsonRun(t, f, ExitOK, "task", "validate", "judged")
	assertKeys(t, got.doc, "arms,command,harness_changed,judge,needs_review,repeats,schema,status,summary,task,unstated_requirement_details,weak_tests")
	for _, k := range []string{"arms", "unstated_requirement_details", "harness_changed"} {
		if got.doc[k] == nil {
			t.Errorf("%s is null:\n%s", k, got.stdout)
		}
	}
	assertKeys(t, got.get("judge"), "changed_lines,code_files,instruction_words,reference_diff_truncated")
	if files, ok := got.get("judge", "code_files").([]any); !ok || len(files) != 1 {
		t.Errorf("judge: %s", got.stdout)
	}
}

// Redaction touches free text at whole path names only, and never the user's own content.
func TestRedactMatchesWholePathNames(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ home, dir, in, want string }{
		{"/root", "", ".claude/rules/root.md and x/root.go", ".claude/rules/root.md and x/root.go"},
		{"/root", "", "COPY . /root/app", "COPY . ~/app"}, // free text that quotes the path
		{"/root", "", "cannot read /root: denied", "cannot read ~: denied"},
		{"/Users/al", "", "/Users/alice/notes and /Users/alice", "/Users/alice/notes and /Users/alice"},
		{"/Users/al", "", "see /Users/al/notes", "see ~/notes"},
		{"/Users/al", "/Users/al/repo", "/Users/al/repo-old and /Users/al/repo/src", "~/repo-old and <repo>/src"},
		{"/Users/al", "/Users/al/repo", `path="/Users/al/repo" and (/Users/al/repo)`, `path="<repo>" and (<repo>)`},
		{"/Users/al", "", "x/Users/al/y", "x/Users/al/y"},
		{"/Users/al", "", "run `chmod 700 /nowhere/data` to fix", "run `chmod 700 <data>` to fix"}, // outside HOME, before a backtick
		{"/Users/al", "/srv/repo", "see /srv/repo.", "see <repo>."},                                // a sentence's full stop
		{"/Users/al", "/srv/repo", "see /srv/repo.git and /srv/repo.", "see /srv/repo.git and <repo>."},
		{"/Users/al", "/srv/repo", "[/srv/repo] {/srv/repo} (/srv/repo)", "[<repo>] {<repo>} (<repo>)"},
		{"/Users/al", "", "file:///Users/al/x and file:///Users/al", "file://~/x and file://~"},
		{"/Users/al", "", "/Users/al-2 and /Users/al_x and /Users/al9", "/Users/al-2 and /Users/al_x and /Users/al9"},
	} {
		env := Env{Dir: c.dir, Getenv: func(k string) string { return map[string]string{"HOME": c.home, "AGENTIUM_HOME": "/nowhere/data"}[k] }}
		if got := env.redact(c.in); got != c.want {
			t.Errorf("home %s dir %s: redact(%q) = %q, want %q", c.home, c.dir, c.in, got, c.want)
		}
	}
}

func TestJSONKeepsUserContentAndNamesThatLookLikeThePath(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["HOME"] = "/root" // a home folder whose name is a common word
	writeFile(t, f.repo, ".claude/rules/root.md", "Rules about root.\n")
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nCOPY . /root/app\n")
	jsonRun(t, f, ExitOK, "context", "snapshot", "one", "--ref", "HEAD")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "Add rules")
	jsonRun(t, f, ExitOK, "context", "snapshot", "two", "--ref", "HEAD")
	show := jsonRun(t, f, ExitOK, "context", "show")
	if !strings.Contains(show.stdout, `".claude/rules/root.md"`) {
		t.Errorf("a context file named root.md was rewritten:\n%s", show.stdout)
	}
	diff := jsonRun(t, f, ExitOK, "context", "diff", "one", "two", "--patch")
	if !strings.Contains(diff.get("patch").(string), "COPY . /root/app") {
		t.Errorf("a diff was redacted:\n%s", diff.stdout)
	}
	writeFile(t, f.repo, "x/root.go", "package x\n")
	writeFile(t, f.repo, "tests/root_test.sh", "true\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "Add root")
	jsonRun(t, f, ExitOK, "task", "import", "--commit", "HEAD", "--name", "rooty", "--verify", "sh run_tests.sh")
	shown := jsonRun(t, f, ExitOK, "task", "show", "rooty")
	if ref := fmt.Sprint(shown.get("reference")); !strings.Contains(ref, "x/root.go") {
		t.Errorf("a reference file named root.go was rewritten: %s", ref)
	}
}

// start ran from a folder inside a repository that is not under the home folder must not name that repository.
func TestJSONStartFromASubfolderHidesTheRepository(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 2)
	sub := filepath.Join(f.repo, "docs", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	g := runFixtureAt(sub, f.data, f.home)
	g.vars["AGENTIUM_CLAUDE"] = f.vars["AGENTIUM_CLAUDE"]
	res := g.run(context.Background(), "start", "--accept-mined", "--json")
	got := checkJSON(t, f, res, ExitError, []string{"start", "--accept-mined"}) // two tasks: too few, with the log
	if log := fmt.Sprint(got.get("log")); !strings.Contains(log, "<repo>") {
		t.Errorf("the log should name the repository as <repo>: %s", log)
	}
	// And in a failure message: an unregistered repository, run from a subfolder.
	h := runFixtureAt(sub, filepath.Join(t.TempDir(), "other"), f.home)
	failed := checkJSON(t, f, h.run(context.Background(), "task", "list", "--json"), ExitError, []string{"task", "list"})
	if msg := failed.get("error", "message").(string); !strings.Contains(msg, "<repo> is not registered") {
		t.Errorf("message = %q", msg)
	}
}

func TestErrorMessageIsTheErrorNotAnEarlierWarning(t *testing.T) {
	t.Parallel()
	env := Env{Getenv: func(string) string { return "" }, json: &jsonState{}}
	if got := errorMessage(env, "warning: a note\nagentium: the real failure\n", ExitError); got != "the real failure" {
		t.Errorf("message = %q", got)
	}
	if got := errorMessage(env, "warning: a note\nagentium task edit: give NAME\n\nUsage:\n  agentium task list\n", ExitUsage); got != "agentium task edit: give NAME" {
		t.Errorf("usage message = %q", got)
	}
	env.json.err = "recorded error"
	if got := errorMessage(env, "warning: unrelated\n", ExitError); got != "recorded error" {
		t.Errorf("message = %q", got)
	}
	if got := errorMessage(Env{Getenv: func(string) string { return "" }}, "Usage:\n  agentium x\n", ExitUsage); !strings.Contains(got, "invalid arguments") {
		t.Errorf("message = %q", got)
	}
}

// A warning printed before a failure does not become the failure's message.
func TestJSONErrorAfterWarning(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	// task edit --reviewed on a task with gaps prints them (plain, on stderr) and then fails; here the failure is simply a
	// missing task after a context warning source: any recorded error wins over stderr's first lines.
	got := jsonRun(t, f, ExitError, "context", "diff", "nope", "nope2")
	if msg := got.get("error", "message").(string); !strings.Contains(msg, "nope") {
		t.Errorf("message = %q", msg)
	}
}

func TestReadinessStatusIsAFixedSet(t *testing.T) {
	for label, want := range map[string]string{"ok": "ok", "MISSING": "missing", "WARNING": "warning", "NOTE": "warning", "": "warning"} {
		if got := readinessStatus(label); got != want {
			t.Errorf("readinessStatus(%q) = %q, want %q", label, got, want)
		}
	}
}

// The logs of run show are Agentium's own output and are redacted; the diff is the agent's work and is not.
func TestJSONRunShowRedactsLogsNotTheDiff(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt", false)
	id := jsonRun(t, f, ExitOK, "run", "once", "value").get("run", "id").(string)
	dirs, _ := filepath.Glob(filepath.Join(f.data, "records", "*"))
	if len(dirs) != 1 {
		t.Fatalf("records: %v", dirs)
	}
	writeFile(t, dirs[0], "verify.log", "cd "+filepath.Join(f.data, "workspaces", id)+"\nok\n")
	writeFile(t, dirs[0], "agent.diff", "+COPY . "+f.data+"/app\n")
	res := f.run(context.Background(), "run", "show", id, "--diff", "--log", "--json") // not jsonRun: the diff holds the path on purpose
	var got jsonResult
	got.cliResult = res
	if err := json.Unmarshal([]byte(res.stdout), &got.doc); err != nil || res.code != ExitOK {
		t.Fatalf("exit %d: %v\n%s", res.code, err, res.stdout)
	}
	if log := got.get("verify_log").(string); !strings.Contains(log, "cd <data>/workspaces/") {
		t.Errorf("verify_log = %q", log)
	}
	if diff := got.get("diff").(string); !strings.Contains(diff, f.data+"/app") {
		t.Errorf("the diff was redacted: %q", diff)
	}
}

func TestSmallDocumentKeys(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]struct {
		doc  any
		keys string
	}{
		"gap":        {gapDoc{}, "file,kind,text"},
		"experiment": {runExperimentDoc{}, "attempt,name,slot"},
		"north star": {northStarDoc{}, "decisive,experiment,metric,seconds,spent_usd,verdict"},
	} {
		raw, err := json.Marshal(v.doc)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) { assertKeys(t, m, v.keys) })
	}
}
