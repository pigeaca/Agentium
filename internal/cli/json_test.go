package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsonResult is a --json run: the one document on stdout, decoded.
type jsonResult struct {
	cliResult
	doc map[string]any
}

// getPath reads doc["a"]["b"]... ; nil when a step is missing.
func (r jsonResult) get(path ...string) any {
	var cur any = r.doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// jsonRun runs args with --json and checks the contract every document must meet: exactly one JSON value on stdout, the
// schema and command fields, no escape code, no absolute path of the repository, data folder or home.
func jsonRun(t *testing.T, f runFixture, wantCode int, args ...string) jsonResult {
	t.Helper()
	res := f.run(context.Background(), append(append([]string{}, args...), "--json")...)
	return checkJSON(t, f, res, wantCode, args)
}

func checkJSON(t *testing.T, f runFixture, res cliResult, wantCode int, args []string) jsonResult {
	t.Helper()
	if res.code != wantCode {
		t.Fatalf("%v: exit %d, want %d\nstdout %s\nstderr %s", args, res.code, wantCode, res.stdout, res.stderr)
	}
	dec := json.NewDecoder(strings.NewReader(res.stdout))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("%v: stdout is not JSON: %v\n%s", args, err, res.stdout)
	}
	if dec.More() {
		t.Errorf("%v: more than one document on stdout:\n%s", args, res.stdout)
	}
	if doc["schema"] != float64(JSONSchema) {
		t.Errorf("%v: schema = %v", args, doc["schema"])
	}
	if want := strings.Join(args[:min(len(args), 2)], " "); args[0] == "init" || args[0] == "start" {
		if doc["command"] != args[0] {
			t.Errorf("%v: command = %v", args, doc["command"])
		}
	} else if doc["command"] != want {
		t.Errorf("%v: command = %v, want %q", args, doc["command"], want)
	}
	if strings.Contains(res.stdout, "\x1b") {
		t.Errorf("%v: an escape code in the document:\n%s", args, res.stdout)
	}
	for _, p := range []string{f.repo, resolved(f.repo), f.data, resolved(f.data), f.home, resolved(f.home)} {
		if strings.Contains(res.stdout, p) {
			t.Errorf("%v: the document holds the path %s:\n%s", args, p, res.stdout)
		}
	}
	if wantCode != ExitOK {
		if _, ok := doc["error"]; !ok && args[0] != "task" && args[0] != "start" && !(len(args) > 1 && args[0] == "experiment" && args[1] == "run" && doc["run"] != nil) { // results, not errors
			t.Errorf("%v: failed without an error object:\n%s", args, res.stdout)
		}
	} else if _, ok := doc["error"]; ok {
		t.Errorf("%v: succeeded with an error object", args)
	}
	return jsonResult{res, doc}
}

func TestJSONDocumentsOfTheListedCommands(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	// Terminal output and color settings must not matter.
	*f.terminal = true
	f.vars["FORCE_COLOR"] = "1"

	init := jsonRun(t, f, ExitOK, "init")
	if init.get("project", "name") == nil || init.get("claude_code", "found") != false || init.get("sign_in") != "login" || init.get("head") == nil {
		t.Errorf("init: %s", init.stdout)
	}
	if _, ok := init.get("test_commands").([]any); !ok {
		t.Errorf("init: test_commands is not a list: %s", init.stdout)
	}

	show := jsonRun(t, f, ExitOK, "context", "show")
	if show.get("where") != "working tree" || len(show.get("entries").([]any)) == 0 || show.get("context", "startup_files") == nil {
		t.Errorf("context show: %s", show.stdout)
	}
	empty := jsonRun(t, f, ExitOK, "context", "list")
	if snaps, ok := empty.get("snapshots").([]any); !ok || len(snaps) != 0 {
		t.Errorf("context list of none must be an empty list: %s", empty.stdout)
	}
	snap := jsonRun(t, f, ExitOK, "context", "snapshot", "one", "--ref", "HEAD")
	if snap.get("name") != "one" || snap.get("source") != "HEAD" || snap.get("files") != float64(1) {
		t.Errorf("context snapshot: %s", snap.stdout)
	}
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nBe brief.\n")
	jsonRun(t, f, ExitOK, "context", "snapshot", "two", "--working-tree")
	if l := jsonRun(t, f, ExitOK, "context", "list"); len(l.get("snapshots").([]any)) != 2 {
		t.Errorf("context list: %s", l.stdout)
	}
	diff := jsonRun(t, f, ExitOK, "context", "diff", "one", "two", "--patch")
	if diff.get("changed") != true || !strings.Contains(diff.get("patch").(string), "+Be brief.") {
		t.Errorf("context diff: %s", diff.stdout)
	}
	same := jsonRun(t, f, ExitOK, "context", "diff", "one", "one")
	if same.get("changed") != false || same.get("delta_tokens_estimated") != float64(0) {
		t.Errorf("context diff of one version: %s", same.stdout)
	}
	lint := jsonRun(t, f, ExitOK, "context", "lint")
	if lint.get("snapshot", "name") != "two" || lint.get("problems") == nil || lint.get("warnings") == nil {
		t.Errorf("context lint: %s", lint.stdout)
	}

	tasks := jsonRun(t, f, ExitOK, "task", "list")
	rows := tasks.get("tasks").([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["name"] != "value" || rows[0].(map[string]any)["status"] != "unvalidated" ||
		rows[0].(map[string]any)["needs_review"] != true {
		t.Errorf("task list: %s", tasks.stdout)
	}
	shown := jsonRun(t, f, ExitOK, "task", "show", "value")
	if shown.get("instruction") == nil || shown.get("review") != "history" || shown.get("hidden_tests") == nil || shown.get("verify").([]any)[0] != "sh run_tests.sh" {
		t.Errorf("task show: %s", shown.stdout)
	}
	imported := jsonRun(t, f, ExitOK, "task", "import", "--commit", "HEAD", "--name", "again", "--verify", "sh run_tests.sh")
	if imported.get("task", "name") != "again" || imported.get("next_command") != "agentium task validate again" {
		t.Errorf("task import: %s", imported.stdout)
	}
	edited := jsonRun(t, f, ExitOK, "task", "edit", "again", "--reviewed")
	if edited.get("updated") != true || edited.get("task", "needs_review") != false {
		t.Errorf("task edit: %s", edited.stdout)
	}
	validated := jsonRun(t, f, ExitOK, "task", "validate", "value")
	if validated.get("status") != "valid" || validated.get("arms").([]any)[0] != "base" || validated.get("harness_changed") == nil {
		t.Errorf("task validate: %s", validated.stdout)
	}
	all := jsonRun(t, f, ExitOK, "task", "validate", "--all", "--status", "unvalidated")
	if all.get("total") != float64(1) || all.get("valid") != float64(1) || all.get("interrupted") != false {
		t.Errorf("task validate --all: %s", all.stdout)
	}
	none := jsonRun(t, f, ExitOK, "task", "validate", "--all", "--status", "flaky")
	if rows, ok := none.get("tasks").([]any); !ok || len(rows) != 0 {
		t.Errorf("task validate --all with nothing to do: %s", none.stdout)
	}
	removed := jsonRun(t, f, ExitOK, "task", "rm", "again")
	if removed.get("removed") != "again" {
		t.Errorf("task rm: %s", removed.stdout)
	}

	if runs := jsonRun(t, f, ExitOK, "run", "list"); len(runs.get("runs").([]any)) != 0 {
		t.Errorf("run list of none: %s", runs.stdout)
	}
}

func TestJSONRunOnceListAndShow(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt", false)
	once := jsonRun(t, f, ExitOK, "run", "once", "value")
	id, _ := once.get("run", "id").(string)
	if id == "" || once.get("run", "outcome") != "ok" || once.get("run", "passed") != true || once.get("run", "cost_usd") != 0.4 ||
		once.get("run", "behavior", "files_changed") != float64(1) {
		t.Errorf("run once: %s", once.stdout)
	}
	list := jsonRun(t, f, ExitOK, "run", "list")
	runs := list.get("runs").([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["id"] != id || runs[0].(map[string]any)["kind"] != "task" {
		t.Errorf("run list: %s", list.stdout)
	}
	shown := jsonRun(t, f, ExitOK, "run", "show", id, "--diff", "--log")
	if !strings.Contains(shown.get("diff").(string), "+new") || shown.get("verify_log") == nil || shown.get("setup_log") != nil ||
		shown.get("experiment") != nil || len(shown.get("files").([]any)) == 0 {
		t.Errorf("run show: %s", shown.stdout)
	}
	jsonRun(t, f, ExitError, "run", "show", "nope")
}

// pool update --json: the dry run lists the best --limit candidates with their scores and the commits set aside per
// reason; a pass imports and validates; the next takes the one left; a pass with nothing to do is a success. task
// mine is gone: its --json is a usage error that names pool update.
func TestJSONPoolUpdateMines(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 3)
	jsonRun(t, f, ExitOK, "init")
	mined := jsonRun(t, f, ExitOK, "pool", "update", "--dry-run", "--limit", "2")
	cands := mined.get("candidates").([]any)
	if mined.get("dry_run") != true || len(cands) != 2 || mined.get("candidates_found") != float64(3) || mined.get("set_aside", "no parent commit") != float64(1) {
		t.Errorf("pool update --dry-run: %s", mined.stdout)
	}
	first := cands[0].(map[string]any)
	if len(first["commit"].(string)) != 40 || first["score_parts"] == nil || first["subject"] == nil {
		t.Errorf("a candidate: %v", first)
	}
	imported := jsonRun(t, f, ExitOK, "pool", "update", "--limit", "2")
	rows := imported.get("tasks").([]any)
	if imported.get("dry_run") != false || len(imported.get("imported").([]any)) != 2 || imported.get("health", "valid") != float64(2) || len(rows) != 2 ||
		rows[0].(map[string]any)["task"].(map[string]any)["status"] != "valid" {
		t.Errorf("pool update: %s", imported.stdout)
	}
	again := jsonRun(t, f, ExitOK, "pool", "update", "--limit", "5")
	if len(again.get("imported").([]any)) != 1 {
		t.Errorf("a second pass takes the one left: %s", again.stdout)
	}
	nothing := jsonRun(t, f, ExitOK, "pool", "update")
	if nothing.get("candidates_found") != float64(0) || len(nothing.get("imported").([]any)) != 0 {
		t.Errorf("a pass with nothing to do is a success: %s", nothing.stdout)
	}
	removed := jsonRun(t, f, ExitUsage, "task", "mine", "--dry-run")
	if msg, _ := removed.get("error", "message").(string); !strings.Contains(msg, "pool update") {
		t.Errorf("task mine --json: %s", removed.stdout)
	}
}

func TestJSONTaskValidateOfAnInvalidTaskExitsOneWithItsResult(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	jsonRun(t, f, ExitOK, "task", "edit", "value", "--verify", "false")
	bad := jsonRun(t, f, ExitError, "task", "validate", "value")
	if bad.get("status") != "invalid" || bad.get("error") != nil {
		t.Errorf("an invalid task is a result, not an error object: %s", bad.stdout)
	}
	all := jsonRun(t, f, ExitError, "task", "validate", "--all")
	if all.get("valid") != float64(0) || all.get("total") != float64(1) {
		t.Errorf("task validate --all: %s", all.stdout)
	}
}

func TestJSONStartPreviewNeverAsksOrRuns(t *testing.T) {
	t.Parallel()
	f, ctrl := startFixture(t, 9)
	got := jsonRun(t, f, ExitOK, "start", "--accept-mined")
	if got.get("status") != "preview" && got.get("status") != "not_ready" {
		t.Errorf("start status: %s", got.stdout)
	}
	assertKeys(t, got.doc, startKeys)
	if got.get("tasks_ready") != float64(9) || got.get("tasks_awaiting_review") != float64(0) {
		t.Errorf("task counts: %s", got.stdout)
	}
	assertKeys(t, got.doc["experiment"], startExperimentKeys)
	assertKeys(t, got.doc["project"], "id,name")
	assertKeys(t, got.doc["north_star"], "decisive,experiment,metric,seconds,spent_usd,verdict")
	assertKeys(t, got.doc["readiness"].([]any)[0], "status,text")
	if got.get("nothing_was_run") != true || got.get("experiment", "name") != "quick-aa-baseline" || got.get("experiment", "runs") != float64(18) || // 9 tasks, all taken (seq-v1 start aims for 16, accepts 8 or more)
		got.get("project", "name") == nil || got.get("run_command") != "agentium experiment run quick-aa-baseline" {
		t.Errorf("start: %s", got.stdout)
	}
	if log, _ := got.get("log").([]any); len(log) == 0 || !strings.Contains(got.stdout, "Mining: ") {
		t.Errorf("the stages' text belongs in log: %s", got.stdout)
	}
	if stored, started := paidRuns(t, f, ctrl); stored != 0 || started != 0 {
		t.Errorf("start --json ran the agent: %d stored, %d started", stored, started)
	}
	again := jsonRun(t, f, ExitOK, "start", "--accept-mined")
	if again.get("experiment", "name") != "quick-aa-baseline" || again.get("tasks_ready") != nil || again.get("tasks_awaiting_review") != nil {
		t.Errorf("a second start resumes, without counting tasks: %s", again.stdout)
	}
}

func TestJSONStartWithTooFewTasksExitsOneWithItsDocument(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 2)
	got := jsonRun(t, f, ExitError, "start", "--accept-mined")
	if got.get("status") != "too_few_tasks" || got.get("experiment") != nil || got.get("nothing_was_run") != true {
		t.Errorf("start: %s", got.stdout)
	}
}

// blockingReader never returns: a read of it would hang the command.
type blockingReader struct{ stop chan struct{} }

func (b blockingReader) Read([]byte) (int, error) { <-b.stop; return 0, io.EOF }

// A command that would ask must not read a stdin that is not a terminal; with --json it never asks, even at a terminal.
func TestStartNeverBlocksOnStdin(t *testing.T) {
	t.Parallel()
	f, ctrl := readyFixture(t)
	for name, terminal := range map[string]bool{"stdin is a pipe": false, "--json at a terminal": true} {
		stop := make(chan struct{})
		args := []string{"start"}
		if terminal {
			args = append(args, "--json")
		}
		done := make(chan cliResult, 1)
		go func() {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), Env{DefaultGrader: "host", Args: args, Stdin: blockingReader{stop}, StdinTerminal: terminal, Terminal: terminal, Stdout: &stdout,
				Stderr: &stderr, Dir: f.repo, Getenv: func(k string) string { return f.vars[k] },
				Environ:  func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home} },
				LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now})
			done <- cliResult{code, stdout.String(), stderr.String()}
		}()
		select {
		case res := <-done:
			if res.code != ExitOK || strings.Contains(res.stdout, "[y/N]") {
				t.Errorf("%s: exit %d, or it asked:\n%s%s", name, res.code, res.stdout, res.stderr)
			}
			if terminal && !strings.Contains(res.stdout, `"status": "preview"`) {
				t.Errorf("%s: no preview document:\n%s", name, res.stdout)
			}
			if !terminal && !strings.Contains(res.stdout, "Nothing was run") {
				t.Errorf("%s: no preview text:\n%s", name, res.stdout)
			}
		case <-time.After(3 * time.Minute):
			t.Fatalf("%s: start blocked on stdin", name)
		}
		close(stop)
	}
	if stored, started := paidRuns(t, f, ctrl); started != 0 {
		t.Errorf("start without consent ran %d run(s) (%d stored)", started, stored)
	}
}

func TestJSONErrorsCarryTheExitCode(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	cases := []struct {
		args    []string
		code    int
		message string
	}{
		{[]string{"task", "show"}, ExitUsage, "invalid arguments"},
		{[]string{"task", "show", "a", "b"}, ExitUsage, "invalid arguments"},
		{[]string{"task", "edit", "value"}, ExitUsage, "give NAME and at least one of"},
		{[]string{"context", "snapshot", "Bad Name"}, ExitUsage, "must be lowercase"},
		{[]string{"context", "diff", "x", "y"}, ExitError, "x"},
		{[]string{"context", "lint", "--hook"}, ExitUsage, "--hook"},
		{[]string{"task", "show", "nope"}, ExitError, "nope"},
		{[]string{"run", "once", "nope"}, ExitError, "nope"},
		{[]string{"task", "validate", "--all", "value"}, ExitUsage, "not both"},
		{[]string{"task", "list", "--bogus"}, ExitUsage, "bogus"},
		{[]string{"context", "show", "--ref", "no-such-ref"}, ExitError, "no-such-ref"},
	}
	for _, c := range cases {
		got := jsonRun(t, f, c.code, c.args...)
		body, _ := got.get("error").(map[string]any)
		if body == nil || body["code"] != float64(c.code) || !strings.Contains(body["message"].(string), c.message) {
			t.Errorf("%v: error = %v, want code %d and %q", c.args, got.get("error"), c.code, c.message)
		}
		if strings.Contains(body["message"].(string), "Usage:") {
			t.Errorf("%v: the usage text is not an error message: %v", c.args, body["message"])
		}
		if len(got.stderr) != 0 {
			t.Errorf("%v: stderr in JSON mode:\n%s", c.args, got.stderr)
		}
	}
}

func TestJSONNotRegisteredShowsNoPath(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	f := runFixtureAt(repo, filepath.Join(t.TempDir(), "data"), t.TempDir())
	got := jsonRun(t, f, ExitError, "task", "list")
	if msg := got.get("error", "message").(string); !strings.Contains(msg, "is not registered: run `agentium init` first") || !strings.HasPrefix(msg, "<repo>") {
		t.Errorf("message = %q", msg)
	}
}

func TestJSONIgnoresColorSettingsAndHelpStaysText(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	plain := jsonRun(t, f, ExitOK, "task", "list").stdout
	*f.terminal = true
	for _, env := range []map[string]string{{"NO_COLOR": "1"}, {"FORCE_COLOR": "1"}, {"FORCE_COLOR": "1", "TERM": "xterm-256color"}} {
		for k, v := range env {
			f.vars[k] = v
		}
		if got := jsonRun(t, f, ExitOK, "task", "list").stdout; got != plain {
			t.Errorf("%v changed the document:\n%s\nwas\n%s", env, got, plain)
		}
	}
	help := f.run(context.Background(), "task", "list", "--json", "-h")
	if help.code != ExitOK || !strings.HasPrefix(help.stdout, "Usage:") {
		t.Errorf("--json with -h: exit %d\n%s", help.code, help.stdout)
	}
}

// Commands outside the --json contract keep their behavior: the flag is not theirs.
func TestJSONFlagIsUntouchedOnOtherCommands(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	got := f.run(context.Background(), "run", "calibrate", "--json")
	if got.code != ExitUsage || !strings.Contains(got.stderr, "json") {
		t.Errorf("run calibrate --json: exit %d\n%s", got.code, got.stderr)
	}
	if got := f.run(context.Background(), "version", "--json"); got.code != ExitOK {
		t.Errorf("version: %d", got.code)
	}
}

func TestSplitJSONFlag(t *testing.T) {
	cases := []struct {
		command string
		args    []string
		rest    []string
		want    bool
	}{
		{"init", []string{"--json"}, []string{}, true},
		{"init", []string{"x", "--json"}, []string{"x"}, true},
		{"init", []string{"--", "--json"}, []string{"--", "--json"}, false},
		{"task", []string{"list", "--json"}, []string{"list"}, true},
		{"task", []string{"--json", "show", "a"}, []string{"--json", "show", "a"}, false}, // the subcommand comes first
		{"task", []string{"list", "--json", "-h"}, []string{"list", "-h"}, false},
		{"experiment", []string{"report", "--json"}, []string{"report", "--json"}, false},
		{"experiment", []string{"run", "x", "--yes", "--json"}, []string{"run", "x", "--yes"}, true},
		{"experiment", []string{"list", "--json"}, []string{"list"}, true},
		{"run", []string{"calibrate", "--json"}, []string{"calibrate", "--json"}, false},
		{"context", []string{"lint", "-json"}, []string{"lint"}, true},
		{"context", []string{"snapshot", "help", "--json"}, []string{"snapshot", "help"}, true}, // "help" is a name here
		{"task", []string{"show", "help", "--json"}, []string{"show", "help"}, true},
		{"task", []string{"add", "n", "--include", "help", "--json"}, []string{"add", "n", "--include", "help"}, true},
		{"task", []string{"show", "x", "--json", "--help"}, []string{"show", "x", "--help"}, false},
		{"task", []string{"show", "x", "--json", "-help"}, []string{"show", "x", "-help"}, false},
		{"task", []string{"add", "n", "--instruction", "--", "--json"}, []string{"add", "n", "--instruction", "--", "--json"}, false},
	}
	for _, c := range cases {
		rest, want := splitJSONFlag(c.command, c.args)
		if want != c.want || strings.Join(rest, "\x00") != strings.Join(c.rest, "\x00") {
			t.Errorf("%s %v: rest %q, want %v; got %v", c.command, c.args, rest, c.want, want)
		}
	}
}
