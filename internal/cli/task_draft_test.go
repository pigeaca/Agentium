package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/store"
)

// The drafts' stand-in for Claude Code answers the drafter's schema ({"text": ...}) with whatever ctrl/reply.json
// holds, so a test changes the reply without a new script. It records each call's prompt and arguments in ctrl, and:
//   - with ctrl/hang, it waits to be interrupted and then replies, as Claude Code does on SIGINT;
//   - with ctrl/crash, it replies and then kills the Agentium process that called it (its parent) before exiting: the
//     reply is on disk, and nothing after the call ran.
const (
	draftStored   = `{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.031,"structured_output":{"text":"Add Clamp(v, lo, hi int) int to package lib: it returns v limited to the range from lo to hi."}}`
	draftGap      = `{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.02,"structured_output":{"text":"Add a function that limits a value to a range."}}`
	draftGiveaway = `{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.025,"structured_output":{"text":"Add Clamp(v, lo, hi int) int to package lib, through a helper clampBetween."}}`
	draftBadForm  = `{"type":"result","subtype":"success","is_error":false,"result":"I would rather not.","total_cost_usd":0.004}`
	draftCapped   = `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":0.58}`
	draftLate     = `{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.07}`
)

func draftAgent(t *testing.T, ctrl string) string {
	t.Helper()
	script := `#!/bin/sh
[ "$1" = --version ] && { echo '2.1.281 (Claude Code)'; exit 0; }
n=$(ls "CTRL" | grep -c '^prompt-')
cat > "CTRL/prompt-$n.txt"
printf '%s\n' "$@" > "CTRL/args-$n.txt"
if [ -e "CTRL/hang" ]; then
  trap 'cat "CTRL/reply.json"; exit 130' INT
  touch "CTRL/hanging"
  while :; do sleep 0.05; done
fi
cat "CTRL/reply.json"
if [ -e "CTRL/crash" ]; then kill -9 $PPID; fi
`
	cli := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(cli, []byte(strings.ReplaceAll(script, "CTRL", ctrl)), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli
}

// draftFixture is a Go project whose one commit adds Clamp, which its hidden test calls, through a helper clampBetween
// only the reference has; the task clamp is imported from it (its instruction the terse commit message "lib: clamp").
func draftFixture(t *testing.T) (runFixture, string) {
	t.Helper()
	f := runFixtureAt(t.TempDir(), filepath.Join(t.TempDir(), "data"), t.TempDir())
	ctrl := t.TempDir()
	f.vars["AGENTIUM_CLAUDE"] = draftAgent(t, ctrl)
	f.vars["NO_COLOR"] = "1"
	gitIn(t, f.repo, "init", "-q", "-b", "main")
	writeFile(t, f.repo, "go.mod", "module example.com/m\n\ngo 1.22\n")
	writeFile(t, f.repo, "lib/lib.go", "package lib\n\nfunc Old() int { return 1 }\n")
	writeFile(t, f.repo, "lib/lib_test.go", "package lib\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) { _ = Old() }\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "base")
	writeFile(t, f.repo, "lib/lib.go", "package lib\n\nfunc Old() int { return 1 }\n\n// Clamp limits v to [lo, hi].\nfunc Clamp(v, lo, hi int) int { return clampBetween(v, lo, hi) }\n\n"+
		"func clampBetween(v, lo, hi int) int {\n\tif v < lo {\n\t\treturn lo\n\t}\n\tif v > hi {\n\t\treturn hi\n\t}\n\treturn v\n}\n")
	writeFile(t, f.repo, "lib/lib_test.go", "package lib\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) { _ = Old() }\n\nfunc TestClamp(t *testing.T) {\n\tif Clamp(5, 0, 3) != 3 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "lib: clamp")
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	expect(t, f.run(ctx, "task", "import", "--commit", "HEAD", "--name", "clamp", "--verify", "go test ./..."), ExitOK)
	return f, ctrl
}

// setReply makes the stand-in answer body.
func setReply(t *testing.T, ctrl, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ctrl, "reply.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// draftCalls is how many calls the stand-in took.
func draftCalls(t *testing.T, ctrl string) int {
	t.Helper()
	prompts, _ := filepath.Glob(filepath.Join(ctrl, "prompt-*.txt"))
	return len(prompts)
}

// draftFolders lists the draft calls' folders left in the fixture's data folder.
func draftFolders(t *testing.T, f runFixture) []string {
	t.Helper()
	layout, err := home.Resolve(func(k string) string { return f.vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(layout.Drafts(), "*", "*"))
	return left
}

// onTerminal runs a command as a person types it: stdin and stdout are terminals (NO_COLOR keeps the text plain).
func onTerminal(f runFixture, args ...string) cliResult {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), Env{DefaultGrader: "host", Args: args, Stdout: &stdout, Stderr: &stderr, Dir: f.repo, Terminal: true, StdinTerminal: true,
		Getenv: func(key string) string { return f.vars[key] }, Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home} },
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now})
	return cliResult{code, stdout.String(), stderr.String()}
}

// Every way a draft call ends: the consent first (nothing runs or is spent without it), then one call whose cost is
// counted whatever it brought, a draft stored only when both checks pass, and the folder of each call gone.
func TestTaskDraftConsentChecksAndSpend(t *testing.T) {
	t.Parallel()
	f, ctrl := draftFixture(t)
	ctx := context.Background()
	setReply(t, ctrl, draftStored)

	// No consent, no call: --json without --yes, and a command off a terminal without it.
	refused := jsonRun(t, f, ExitError, "task", "draft", "clamp")
	if refused.get("outcome") != "refused" || refused.get("refused_by").([]any)[0] != "consent" || refused.get("cost_usd") != nil ||
		refused.get("next_command") != "agentium task draft clamp --yes" || refused.get("max_cost_usd") != 0.65 || refused.get("model") != "claude-sonnet-5-5" {
		t.Errorf("--json without --yes: %s", refused.stdout)
	}
	expect(t, f.run(ctx, "task", "draft", "clamp"), ExitError, "Drafting the text of task clamp: one call to claude-sonnet-5-5 without tools (sign-in login), at most $0.65",
		"nothing was run or spent: off a terminal a paid call needs --yes")
	if n := draftCalls(t, ctrl); n != 0 {
		t.Fatalf("%d call(s) without consent", n)
	}

	// A person's own command on a terminal is the consent: the draft is stored beside the instruction.
	stored := onTerminal(f, "task", "draft", "clamp")
	expect(t, stored, ExitOK, "at most $0.65 (its $0.50 cap, which a call can pass by up to $0.15)", "Stored the draft beside the instruction ($0.03; drafting this task has cost $0.03)",
		"  Add Clamp(v, lo, hi int) int to package lib", "agentium task edit clamp --accept-draft")
	got := storedTask(t, f, "clamp")
	if got.Draft != "Add Clamp(v, lo, hi int) int to package lib: it returns v limited to the range from lo to hi." || got.DraftModel != "claude-sonnet-5-5" ||
		got.Instruction != "lib: clamp" || !got.NeedsReview || !near(got.DraftSpendUSD, 0.031) {
		t.Fatalf("after a stored draft: %+v", got)
	}
	prompt, _ := os.ReadFile(filepath.Join(ctrl, "prompt-0.txt"))
	args, _ := os.ReadFile(filepath.Join(ctrl, "args-0.txt"))
	for _, want := range []string{"<commit-message>\nlib: clamp\n</commit-message>", "+func clampBetween", "+func TestClamp", "<tests>"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	for _, want := range []string{"--model\nclaude-sonnet-5-5\n", "--tools\n\n", "--max-budget-usd\n0.5\n", "--json-schema\n", "--no-session-persistence\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("the call's arguments lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "--effort") {
		t.Errorf("the drafter ran at an effort, not the CLI's default:\n%s", args)
	}

	// Refused by each check: nothing is stored, the earlier draft stays, the cost is counted.
	setReply(t, ctrl, draftGap)
	gap := jsonRun(t, f, ExitError, "task", "draft", "clamp", "--yes")
	if gap.get("outcome") != "refused" || len(gap.get("refused_by").([]any)) != 1 || gap.get("refused_by").([]any)[0] != "fairness" ||
		gap.get("draft") != "Add a function that limits a value to a range." || gap.get("cost_usd") != 0.02 || !near(gap.get("drafting_spend_usd").(float64), 0.051) ||
		len(gap.get("unstated_requirement_details").([]any)) != 1 || gap.get("unstated_requirement_details").([]any)[0].(map[string]any)["text"] != "Clamp" {
		t.Errorf("a draft with a gap: %s", gap.stdout)
	}
	setReply(t, ctrl, draftGiveaway)
	expect(t, f.run(ctx, "task", "draft", "clamp", "--yes"), ExitError, "Refused the draft ($0.03; drafting this task has cost $0.08): nothing was stored; the earlier draft stays.",
		"Giveaway: it names 1 name(s) only the reference solution has", "  clampBetween", "The refused text:", "  Add Clamp(v, lo, hi int) int to package lib, through a helper clampBetween.")
	if got := storedTask(t, f, "clamp"); !strings.HasPrefix(got.Draft, "Add Clamp(v, lo, hi int) int to package lib: it returns") || !near(got.DraftSpendUSD, 0.076) {
		t.Errorf("a refused draft changed the stored one or was not counted: %+v", got)
	}

	// No draft at all: each counted, exit 1 with the reason.
	setReply(t, ctrl, draftBadForm)
	expect(t, f.run(ctx, "task", "draft", "clamp", "--yes"), ExitError, "The call brought no draft ($0.00; drafting this task has cost $0.08)", "no text in the schema's form")
	setReply(t, ctrl, draftCapped)
	capped := jsonRun(t, f, ExitError, "task", "draft", "clamp", "--yes")
	if capped.get("outcome") != "failed" || capped.get("draft") != nil || capped.get("cost_usd") != 0.58 || !strings.Contains(capped.get("reason").(string), "reached its $0.50 cap") {
		t.Errorf("the cap reached: %s", capped.stdout)
	}
	setReply(t, ctrl, draftLate)
	if err := os.WriteFile(filepath.Join(ctrl, "hang"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "task", "draft", "clamp", "--yes", "--timeout", "300ms"), ExitError, "The call brought no draft ($0.07;", "timed out")
	os.Remove(filepath.Join(ctrl, "hang"))
	if got := storedTask(t, f, "clamp"); !near(got.DraftSpendUSD, 0.031+0.02+0.025+0.004+0.58+0.07) || got.Instruction != "lib: clamp" {
		t.Errorf("the spend after every call: %+v", got)
	}
	if n := draftCalls(t, ctrl); n != 6 {
		t.Errorf("%d calls, want 6", n)
	}
	if left := draftFolders(t, f); len(left) != 0 {
		t.Errorf("draft folders left: %v", left)
	}
}

// What cannot be drafted fails before any call: a task without hidden tests, a retired task, another process holding
// the run lock.
func TestTaskDraftRefusesBeforeSpending(t *testing.T) {
	t.Parallel()
	f, ctrl := draftFixture(t)
	ctx := context.Background()
	setReply(t, ctrl, draftStored)
	expect(t, f.run(ctx, "task", "add", "byhand", "--base", "HEAD", "--instruction", "Do it.", "--verify", "true"), ExitOK)
	expect(t, f.run(ctx, "task", "draft", "byhand", "--yes"), ExitError, "has no solution with hidden tests")
	expect(t, f.run(ctx, "task", "draft", "nope", "--yes"), ExitError, `task "nope": not found`)
	expect(t, f.run(ctx, "task", "draft", "--yes"), ExitUsage)

	layout, err := home.Resolve(func(k string) string { return f.vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	release, err := layout.LockRuns()
	if err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "task", "draft", "clamp", "--yes"), ExitError, "another Agentium process is running agents")
	release()

	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RetireTask(ctx, storedTask(t, f, "clamp").ID, "its base is too old", time.Now()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	expect(t, f.run(ctx, "task", "draft", "clamp", "--yes"), ExitError, "task clamp is retired")
	if n := draftCalls(t, ctrl); n != 0 {
		t.Errorf("%d call(s) were made", n)
	}
	if got := storedTask(t, f, "clamp"); got.DraftSpendUSD != 0 {
		t.Errorf("spent %v", got.DraftSpendUSD)
	}
}

// An Agentium killed between the call and the count loses no spend: the next task draft counts the call's cost from
// its folder, once, and says so. A folder whose call was counted already, one whose output holds no cost and one whose
// call may still run are each settled as they should be.
func TestTaskDraftCountsACrashedCallOnce(t *testing.T) {
	t.Parallel()
	f, ctrl := draftFixture(t)
	ctx := context.Background()
	setReply(t, ctrl, draftStored)
	if err := os.WriteFile(filepath.Join(ctrl, "crash"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var environ []string
	for _, kv := range os.Environ() { // the fixture's sign-in: no key or token from the developer's environment
		if !strings.HasPrefix(kv, "ANTHROPIC_") && !strings.HasPrefix(kv, "AGENTIUM_") && !strings.HasPrefix(kv, "CLAUDE_") && !strings.HasPrefix(kv, "HOME=") {
			environ = append(environ, kv)
		}
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestExperimentHelperProcess$", "-test.count=1")
	helper.Dir = f.repo
	helper.Env = append(environ, "AGENTIUM_TEST_HELPER=1", "AGENTIUM_TEST_ARGS=task draft clamp --yes", "AGENTIUM_HOME="+f.data, "HOME="+f.home,
		"AGENTIUM_CLAUDE="+f.vars["AGENTIUM_CLAUDE"])
	out, err := helper.CombinedOutput()
	if status, ok := err.(*exec.ExitError); !ok || status.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the helper was not killed: %v\n%s", err, out)
	}
	os.Remove(filepath.Join(ctrl, "crash"))
	left := draftFolders(t, f)
	if len(left) != 1 || storedTask(t, f, "clamp").DraftSpendUSD != 0 {
		t.Fatalf("after the crash: folders %v, spend %v\n%s", left, storedTask(t, f, "clamp").DraftSpendUSD, out)
	}
	callDir := left[0]
	pgid, _ := os.ReadFile(filepath.Join(callDir, "pgid"))
	if p, _ := strconv.Atoi(strings.TrimSpace(string(pgid))); p > 1 {
		waitFor(t, "the stand-in's process group to end", func() bool { return syscall.Kill(-p, 0) != nil })
	}

	// Three more folders, as a stopped Agentium could leave them.
	projectDir := filepath.Dir(callDir)
	leftover := func(name, meta, output, pgid string) {
		dir := filepath.Join(projectDir, name)
		if err := os.MkdirAll(filepath.Join(dir, "start"), 0o700); err != nil {
			t.Fatal(err)
		}
		for file, body := range map[string]string{"call.json": meta, "out.json": output, "pgid": pgid} {
			if body != "" {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	id := storedTask(t, f, "clamp").ID
	meta := `{"task_id":` + strconv.FormatInt(id, 10) + `,"task":"clamp","model":"claude-sonnet-5-5","started":"2026-10-05T10:00:00Z"}`
	leftover("20261005T100000Z-unknown", meta, "", "")
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CountDraftCall(ctx, "20261005T100001Z-counted", id, 0.40, time.Now()); err != nil { // counted, then the process died
		t.Fatal(err)
	}
	db.Close()
	leftover("20261005T100001Z-counted", meta, draftCapped, "")

	// One whose process group exists (this test's own) stops the next draft before anything is counted or called.
	leftover("20261005T100002Z-alive", meta, "", strconv.Itoa(syscall.Getpgrp()))
	expect(t, f.run(ctx, "task", "draft", "clamp", "--yes"), ExitError, "may still be running (process group "+strconv.Itoa(syscall.Getpgrp()))
	if n := draftCalls(t, ctrl); n != 1 {
		t.Fatalf("%d call(s) while a call may still run", n)
	}
	if left := draftFolders(t, f); len(left) != 4 || storedTask(t, f, "clamp").DraftSpendUSD != 0.40 {
		t.Fatalf("a refusal settled something: folders %v, spend %v", left, storedTask(t, f, "clamp").DraftSpendUSD)
	}
	os.RemoveAll(filepath.Join(projectDir, "20261005T100002Z-alive"))

	setReply(t, ctrl, draftGap)
	next := f.run(ctx, "task", "draft", "clamp", "--yes")
	expect(t, next, ExitError, "Counted $0.03 that a draft call of task clamp spent before Agentium stopped; its draft was not kept",
		"A draft call of task clamp, left by a stopped Agentium, reported no cost: its spend is unknown, at most $0.65", "Refused the draft ($0.02; drafting this task has cost $0.45)")
	if strings.Contains(next.stdout, "$0.58") || strings.Count(next.stdout, "Counted $") != 1 {
		t.Errorf("a call counted before its folder went was counted again:\n%s", next.stdout)
	}
	got := storedTask(t, f, "clamp")
	if !near(got.DraftSpendUSD, 0.40+0.031+0.02) || got.Draft != "" {
		t.Errorf("after the recovery: %+v", got)
	}
	again := f.run(ctx, "task", "draft", "clamp", "--yes")
	expect(t, again, ExitError, "Refused the draft ($0.02; drafting this task has cost $0.47)")
	if strings.Contains(again.stdout, "Counted $") || strings.Contains(again.stdout, "left by a stopped Agentium") {
		t.Errorf("the crashed call was settled twice:\n%s", again.stdout)
	}
	if left := draftFolders(t, f); len(left) != 0 {
		t.Errorf("draft folders left: %v", left)
	}
}

// task show prints both texts and the spend (text and JSON); task edit --accept-draft puts the draft in place and the
// task awaits review, unless --reviewed (with the usual gate for unstated requirements).
func TestTaskShowAndAcceptDraft(t *testing.T) {
	t.Parallel()
	f, ctrl := draftFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "task", "edit", "clamp", "--instruction", "Add Clamp(v, lo, hi int) int to package lib."), ExitOK)

	plain := f.run(ctx, "task", "show", "clamp")
	expect(t, plain, ExitOK, "Instruction:")
	if strings.Contains(plain.stdout, "Draft") || strings.Contains(plain.stdout, "Drafting") {
		t.Errorf("a task without a draft shows one:\n%s", plain.stdout)
	}
	none := jsonRun(t, f, ExitOK, "task", "show", "clamp")
	for _, key := range []string{"draft", "draft_written_at", "draft_model"} {
		if v, ok := none.doc[key]; !ok || v != nil {
			t.Errorf("task show --json without a draft: %s = %v (present %v)", key, v, ok)
		}
	}
	if none.get("drafting_spend_usd") != float64(0) {
		t.Errorf("drafting_spend_usd: %v", none.get("drafting_spend_usd"))
	}
	expect(t, f.run(ctx, "task", "edit", "clamp", "--accept-draft"), ExitError, "task clamp has no stored draft: write one with agentium task draft clamp")
	expect(t, f.run(ctx, "task", "edit", "clamp", "--accept-draft", "--instruction", "x"), ExitUsage, "give --instruction or --accept-draft, not both")

	setReply(t, ctrl, draftStored)
	expect(t, f.run(ctx, "task", "draft", "clamp", "--yes"), ExitOK)
	shown := f.run(ctx, "task", "show", "clamp")
	expect(t, shown, ExitOK, "Instruction:\n  Add Clamp(v, lo, hi int) int to package lib.\n", "Draft (by claude-sonnet-5-5, ", "; not in use):\n  Add Clamp(v, lo, hi int) int to package lib: it returns",
		"Put it in place of the instruction: agentium task edit clamp --accept-draft", "Drafting this task has cost $0.03.")
	doc := jsonRun(t, f, ExitOK, "task", "show", "clamp")
	if doc.get("draft") != storedTask(t, f, "clamp").Draft || doc.get("draft_model") != "claude-sonnet-5-5" || doc.get("drafting_spend_usd") != 0.031 ||
		doc.get("instruction") != "Add Clamp(v, lo, hi int) int to package lib." {
		t.Errorf("task show --json with a draft: %s", doc.stdout)
	}
	if at, _ := time.Parse(time.RFC3339, doc.get("draft_written_at").(string)); at.IsZero() {
		t.Errorf("draft_written_at: %v", doc.get("draft_written_at"))
	}

	// The task was reviewed; accepting the draft makes it await review again, keeps the spend and the validation.
	before := storedTask(t, f, "clamp")
	if before.NeedsReview {
		t.Fatal("the edited task is not reviewed")
	}
	expect(t, f.run(ctx, "task", "edit", "clamp", "--accept-draft"), ExitOK, "Updated task clamp: its draft is now its instruction",
		"Review it before experiments take the task: agentium task show clamp, then agentium task edit clamp --reviewed")
	accepted := storedTask(t, f, "clamp")
	if accepted.Instruction != before.Draft || !accepted.NeedsReview || accepted.Draft != "" || !near(accepted.DraftSpendUSD, 0.031) ||
		string(accepted.Validation) != string(before.Validation) || !accepted.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("after --accept-draft: %+v", accepted)
	}
	expect(t, f.run(ctx, "task", "edit", "clamp", "--accept-draft"), ExitError, "no stored draft")

	// With --reviewed, a draft that leaves a requirement unstated is held by the gate unless --accept-gaps.
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskDraft(ctx, accepted.ID, "Add a function that limits a value.", "claude-sonnet-5-5", time.Now()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	expect(t, f.run(ctx, "task", "edit", "clamp", "--accept-draft", "--reviewed"), ExitError, "identifier Clamp", "not marked reviewed")
	if got := storedTask(t, f, "clamp"); got.Instruction != accepted.Instruction || got.Draft == "" {
		t.Errorf("a gated accept wrote: %+v", got)
	}
	edited := jsonRun(t, f, ExitOK, "task", "edit", "clamp", "--accept-draft", "--reviewed", "--accept-gaps", "--verify", "go vet ./...")
	if edited.get("task", "needs_review") != false || edited.get("updated") != true {
		t.Errorf("--accept-draft --reviewed --accept-gaps: %s", edited.stdout)
	}
	if got := storedTask(t, f, "clamp"); got.Instruction != "Add a function that limits a value." || got.NeedsReview || got.Draft != "" || got.Validation != nil ||
		len(got.Verify) != 1 || got.Verify[0] != "go vet ./..." {
		t.Errorf("after --accept-draft --reviewed with --verify: %+v", got)
	}
}

// pool update --accept-mined never touches a draft: a stored draft survives a pass unchanged, and a task whose text came
// from a draft still awaits its review afterwards.
func TestPoolUpdateLeavesDrafts(t *testing.T) {
	t.Parallel()
	f, _ := startFixture(t, 3)
	ctx := context.Background()
	expect(t, f.run(ctx, "init"), ExitOK)
	expect(t, f.run(ctx, "pool", "update", "--limit", "2"), ExitOK, "Imported 2 of 2 candidate(s) tried")
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatal(projects, err)
	}
	tasks, err := db.Tasks(ctx, projects[0].ID)
	if err != nil || len(tasks) != 2 {
		t.Fatal(tasks, err)
	}
	kept, fromDraft := tasks[0], tasks[1]
	for _, tk := range tasks {
		if err := db.SetTaskDraft(ctx, tk.ID, "Draft of "+tk.Name+".", "claude-sonnet-5-5", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	expect(t, f.run(ctx, "task", "edit", fromDraft.Name, "--accept-draft"), ExitOK)

	expect(t, f.run(ctx, "pool", "update", "--accept-mined"), ExitOK, "Accepted 1 mined instruction(s) without your review (--accept-mined)")
	if got := storedTask(t, f, kept.Name); got.Draft != "Draft of "+kept.Name+"." || got.Instruction != kept.Instruction {
		t.Errorf("the pass touched a stored draft: %+v", got)
	}
	if got := storedTask(t, f, fromDraft.Name); !got.NeedsReview || got.Instruction != "Draft of "+fromDraft.Name+"." {
		t.Errorf("--accept-mined accepted a task whose text came from a draft: %+v", got)
	}
}
