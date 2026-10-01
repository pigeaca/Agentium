package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// experimentAgent writes a fake Claude Code for experiments. It matches the calibration of calibratingAgent (tools,
// skills, slash commands), solves the fixture's task, and reads its instructions from files in ctrl, keyed by its
// workspace ("s2-t1": slot 2, first try): "infra" lists runs that end without a result, "hang" runs that wait to be
// killed after starting. "version" overrides what --version prints, "init-version" what the transcript reports.
// Called as the judge (--json-schema), it keeps its prompt and folder in ctrl ("judge-prompt-PID", "judge-dir-PID") and
// answers "yes" at $0.05 a call; with "judge-limit" it fails with a usage limit ($0.01), as it does from the call
// numbered in "judge-limit-after" on (counting from 0); with "judge-broken" it prints no JSON. "cost" sets what a run
// reports it cost (default 0.30). "usage" holds a subscription's five-hour window ("used step resets"): each run reports it at its start and at its
// end, one step further. "subagent" lines ("s2-t1 model") make a run call an investigator subagent on that model;
// with "subagent-by-arm", the arm "lean" calls it on claude-sonnet-5-5 and the other on claude-sonnet-5. With
// "read-value", every run reads value.txt with the Read tool. It leaves its settings argument and process ID in ctrl.
func experimentAgent(t *testing.T, ctrl string) string {
	t.Helper()
	script := `#!/bin/sh
CTRL='` + ctrl + `'
version=2.1.281; [ -f "$CTRL/version" ] && version=$(cat "$CTRL/version")
[ "$1" = --version ] && { echo "$version (Claude Code)"; exit 0; }
case " $* " in *" --json-schema "*)
  calls=$(ls "$CTRL" | grep -c '^judge-prompt-')
  cat > "$CTRL/judge-prompt-$$"; pwd > "$CTRL/judge-dir-$$"
  [ -f "$CTRL/judge-limit-after" ] && [ "$calls" -ge "$(cat "$CTRL/judge-limit-after")" ] && touch "$CTRL/judge-limit"
  [ -f "$CTRL/judge-limit" ] && { echo '{"type":"result","subtype":"error","is_error":true,"result":"Claude AI usage limit reached","total_cost_usd":0.01}'; exit 1; }
  [ -f "$CTRL/judge-broken" ] && { echo 'not json'; exit 1; }
  echo '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"fixed":"yes","reason":"Sets the value as the reference does."},"total_cost_usd":0.05}'; exit 0;;
esac
[ -f "$CTRL/init-version" ] && version=$(cat "$CTRL/init-version")
ws=$(basename "$(dirname "$PWD")")
key=${ws#*-}
prev=""; for a in "$@"; do [ "$prev" = "--settings" ] && printf '%s' "$a" > "$CTRL/settings-$ws"; prev=$a; done
echo $$ > "$CTRL/pid-$ws"
echo '{"type":"system","subtype":"init","claude_code_version":"'"$version"'","model":"claude-sonnet-5","permissionMode":"acceptEdits","tools":["Bash","Edit","Read"],"skills":["review"],"slash_commands":["compact"]}'
echo '{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":1000,"cache_creation_input_tokens":20000,"cache_read_input_tokens":0,"output_tokens":10},"content":[]}}'
limit() { echo '{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":'"$1"',"resetsAt":'"$resets"'},"seven_day":{"utilization":0.2,"resetsAt":'"$resets"'}}}}'; }
[ -f "$CTRL/usage" ] && { read used step resets < "$CTRL/usage"; limit "$used"; }
sub=$(grep "^$key " "$CTRL/subagent" 2>/dev/null | cut -d' ' -f2)
if [ -f "$CTRL/subagent-by-arm" ]; then grep -q "Keep it short" CLAUDE.md && sub=claude-sonnet-5-5 || sub=claude-sonnet-5; fi
if [ -n "$sub" ]; then
  echo '{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m-agent","model":"claude-sonnet-5","content":[{"type":"tool_use","id":"agent1","name":"Agent","input":{"subagent_type":"investigator","prompt":"look"}}]}}'
  echo '{"type":"assistant","parent_tool_use_id":"agent1","message":{"id":"m-sub","model":"'"$sub"'","content":[]}}'
fi
[ -f "$CTRL/read-value" ] && echo '{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m-read","model":"claude-sonnet-5","content":[{"type":"tool_use","id":"read1","name":"Read","input":{"file_path":"'"$PWD"'/value.txt"}}]}}'
grep -qx "$key" "$CTRL/infra" 2>/dev/null && exit 1
if grep -qx "$key" "$CTRL/hang" 2>/dev/null; then touch "$CTRL/hanging-$ws"; sleep 60; fi
sleep 0.2
printf 'new\n' > value.txt
if [ -f "$CTRL/usage" ]; then
  used=$(awk "BEGIN{print $used + $step}"); echo "$used $step $resets" > "$CTRL/usage"; limit "$used"
fi
cost=0.30; [ -f "$CTRL/cost" ] && cost=$(cat "$CTRL/cost")
echo '{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":'"$cost"',"num_turns":2,"duration_ms":1000,"modelUsage":{}}'
`
	cli := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli
}

// experimentFixture is a project with the task "value", reviewed and valid in its own context and in snapshot "lean",
// both calibrated, and experimentAgent as Claude Code.
func experimentFixture(t *testing.T) (runFixture, string) {
	t.Helper()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	ctx := context.Background()
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nKeep it short.\n")
	expect(t, f.run(ctx, "context", "snapshot", "lean", "--working-tree"), ExitOK)
	gitIn(t, f.repo, "checkout", "--", "CLAUDE.md")
	expect(t, f.run(ctx, "task", "edit", "value", "--reviewed"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "value", "--snapshot", "lean"), ExitOK)
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")
	expect(t, f.run(ctx, "run", "calibrate", "--snapshot", "lean"), ExitOK)
	ctrl := t.TempDir()
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, ctrl)
	return f, ctrl
}

func experimentRuns(t *testing.T, f runFixture, name string) []store.Run {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	e, err := db.ExperimentByName(ctx, projects[0].ID, name)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := db.ExperimentRuns(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// armRow is the fields of an arm's line in an experiment's progress table.
func armRow(out, arm string) []string {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 2 && fields[0] == arm {
			return fields
		}
	}
	return nil
}

func emptyWorkspaces(t *testing.T, f runFixture) {
	t.Helper()
	if entries, _ := os.ReadDir(filepath.Join(f.data, "workspaces")); len(entries) != 0 {
		t.Errorf("workspaces left behind: %v", entries)
	}
}

func TestExperimentRunEndToEnd(t *testing.T) {
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--repeats", "3", "--seed", "5"), ExitOK)
	first := f.run(ctx, "experiment", "run", "lean-ab", "--budget", "30")
	expect(t, first, ExitOK, "Budget raised to $30.00 (recorded in the lock)", "Locked: Claude Code 2.1.281, claude-sonnet-5, sign-in login, 6 runs in a seeded order (seed 5)",
		"Running up to 2 at a time", "[1/6] value, arm ", "[6/6] value, arm ", "ok, $0.30", "spent $1.80 of $",
		"Experiment lean-ab: done", "6 of 6 runs settled; spent $1.80", "Every run is done. The report: agentium experiment report lean-ab")
	for _, arm := range []string{"A", "B"} {
		if row := armRow(first.stdout, arm); len(row) < 5 || row[2] != "3/3" || row[3] != "3" || row[4] != "3" {
			t.Errorf("arm %s = %v: want 3 settled, 3 fair, 3 successes:\n%s", arm, row, first.stdout)
		}
	}

	runs := experimentRuns(t, f, "lean-ab")
	slots := map[int]bool{}
	for _, r := range runs {
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			t.Fatal(err)
		}
		if r.Outcome != "ok" || r.Passed == nil || !*r.Passed || r.Attempt != 1 || r.Kind != "task" || slots[r.Slot] {
			t.Errorf("run %+v", r)
		}
		slots[r.Slot] = true
	}
	if len(runs) != 6 || len(slots) != 6 {
		t.Fatalf("%d runs over %d slots", len(runs), len(slots))
	}
	// Each run denied the folders of the runs that could overlap it, before they existed: with concurrency 2, slot 0
	// may overlap slots 1 to 3, each up to its third try.
	settings, err := os.ReadFile(filepath.Join(ctrl, "settings-e1-s0-t1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"e1-s1-t1", "e1-s3-t3", "-e1-s2-t2-repo"} { // workspaces, and the login's session folders
		if !strings.Contains(string(settings), name) {
			t.Errorf("slot 0's settings do not deny %s", name)
		}
	}
	var parsed struct {
		Sandbox struct {
			Filesystem struct {
				DenyRead   []string `json:"denyRead"`
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(settings, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, p := range parsed.Sandbox.Filesystem.DenyRead {
		if strings.Contains(p, "e1-s4-") || strings.Contains(p, "e1-s0-t1") {
			t.Errorf("slot 0 denied %s: a slot outside its window, or its own workspace", p)
		}
	}
	allow := parsed.Sandbox.Filesystem.AllowWrite // the cache as given and as resolved (/var is /private/var on macOS)
	if len(allow) == 0 || slices.ContainsFunc(allow, func(p string) bool { return !strings.HasSuffix(p, "e1-s0-t1/go-build") }) {
		t.Errorf("slot 0 may write %v; want its own build cache", allow)
	}
	emptyWorkspaces(t, f)

	// Nothing left to run: a resume changes nothing; a run experiment cannot be removed.
	again := f.run(ctx, "experiment", "run", "lean-ab")
	expect(t, again, ExitOK, "Resuming experiment lean-ab", "6 of 6 runs settled")
	if strings.Contains(again.stdout, "started") {
		t.Errorf("a finished experiment ran again:\n%s", again.stdout)
	}
	expect(t, f.run(ctx, "experiment", "rm", "lean-ab"), ExitError, "has run")

	// The report: one task only, so no intervals; the counts and notes are there.
	report := f.run(ctx, "experiment", "report", "lean-ab")
	expect(t, report, ExitOK, "# Experiment lean-ab", "Context A/B: A = `base`, B = `lean`", "**Cost**: no result (fewer than two tasks",
		"6 of 6 runs settled (done); spent $1.80", "| value | ●●● 3/3 | ●●● 3/3 | $0.300 → $0.300 |", "## Notes")
	out := filepath.Join(t.TempDir(), "report.json")
	expect(t, f.run(ctx, "experiment", "report", "lean-ab", "--json", "--out", out), ExitOK, "Wrote the report of lean-ab to "+out)
	if data, err := os.ReadFile(out); err != nil || !strings.Contains(string(data), `"experiment": "lean-ab"`) || strings.Contains(string(data), f.data) {
		t.Errorf("JSON report: %v; it must not name the data folder", err)
	}
	expect(t, f.run(ctx, "experiment", "new", "later", "--b", "lean", "--task", "value"), ExitOK)
	expect(t, f.run(ctx, "experiment", "report", "later"), ExitError, "has not run yet")
	expect(t, f.run(ctx, "experiment", "list"), ExitOK, "lean-ab", "done")
	expect(t, f.run(ctx, "experiment", "show", "lean-ab"), ExitOK, "Locked ", "Claude Code 2.1.281, sign-in login", "method "+experiment.MethodVersion,
		"Budget raised ", "$22.00 to $30.00")
}

func TestExperimentRunBudgetRetriesAndLock(t *testing.T) {
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	// Runs cost $0.30 at a $1 cap, 2 at a time: a pair starts only while the spend so far and the caps of the runs in
	// flight leave room for both of its caps in the $3 budget, so the third pair never starts.
	expect(t, f.run(ctx, "experiment", "new", "tight", "--b", "lean", "--task", "value", "--repeats", "3", "--run-budget", "1", "--budget", "3"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "tight"), ExitOK,
		"Experiment tight: budget: the next run would not fit the $3.00 budget ($1.20 spent, $1.00 per run at most)",
		"4 of 6 runs settled", "--budget USD (a higher total)")
	expect(t, f.run(ctx, "experiment", "run", "tight", "--budget", "2"), ExitUsage, "can only be raised")

	// A resume on another Claude Code is refused; the lock holds the version the runs used.
	if err := os.WriteFile(filepath.Join(ctrl, "version"), []byte("2.1.300"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "run", "tight", "--budget", "10"), ExitError, "Claude Code is 2.1.300 now, but the experiment's runs used 2.1.281")
	os.Remove(filepath.Join(ctrl, "version"))

	// An infrastructure failure is retried in a fresh workspace; its spend (estimated from the transcript) counts.
	if err := os.WriteFile(filepath.Join(ctrl, "infra"), []byte("s4-t1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "run", "tight", "--budget", "10"), ExitOK, "Budget raised to $10.00", "infra, $0.08", "retrying in 10ms",
		"(attempt 2 of 3): started", "Experiment tight: done", "6 of 6 runs settled")
	runs := experimentRuns(t, f, "tight")
	if len(runs) != 7 {
		t.Fatalf("%d runs, want 6 and a retry", len(runs))
	}
	for _, r := range runs {
		if r.Outcome == "infra" && (r.Slot != 4 || r.Attempt != 1 || r.CostUSD < 0.08 || !strings.Contains(string(r.Record), "estimated from the transcript")) {
			t.Errorf("the failed attempt: %+v", r)
		}
	}
	expect(t, f.run(ctx, "experiment", "show", "tight"), ExitOK, "Budget raised ", "$3.00 to $10.00")
	emptyWorkspaces(t, f)

	// Claude Code reporting another version during a run stops the experiment: later runs would not compare.
	expect(t, f.run(ctx, "experiment", "new", "drift", "--b", "lean", "--task", "value", "--repeats", "3", "--concurrency", "1"), ExitOK)
	if err := os.WriteFile(filepath.Join(ctrl, "init-version"), []byte("2.1.300"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "run", "drift"), ExitError, "unfair", "Experiment drift: stopped: Claude Code reported version 2.1.300",
		"1 of 6 runs settled")
}

// TestExperimentHelperProcess is `agentium experiment run` in a process of its own, for TestExperimentSurvivesAKill.
func TestExperimentHelperProcess(t *testing.T) {
	if os.Getenv("AGENTIUM_TEST_HELPER") != "1" {
		t.Skip("run by TestExperimentSurvivesAKill")
	}
	dir, _ := os.Getwd()
	code := Run(context.Background(), Env{Args: strings.Split(os.Getenv("AGENTIUM_TEST_ARGS"), " "), Stdout: os.Stdout, Stderr: os.Stderr,
		Dir: dir, Getenv: os.Getenv, Environ: os.Environ, LookPath: exec.LookPath, Now: time.Now,
		Backoff: func(int) time.Duration { return 10 * time.Millisecond }})
	os.Exit(code)
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A runner killed outright (SIGKILL, no chance to clean up) loses no finished run. Its run in progress is found on the
// next start: while that agent still runs, the resume refuses; once it is gone, the run is stored as cancelled with
// what its transcript shows it spent, its workspace is removed, and the slot runs again in a fresh workspace.
func TestExperimentSurvivesAKill(t *testing.T) {
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "kill", "--b", "lean", "--task", "value", "--repeats", "3", "--concurrency", "1"), ExitOK)
	if err := os.WriteFile(filepath.Join(ctrl, "hang"), []byte("s2-t1\n"), 0o644); err != nil {
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
	helper.Env = append(environ, "AGENTIUM_TEST_HELPER=1", "AGENTIUM_TEST_ARGS=experiment run kill", "AGENTIUM_HOME="+f.data,
		"HOME="+f.home, "AGENTIUM_CLAUDE="+f.vars["AGENTIUM_CLAUDE"])
	var out bytes.Buffer
	helper.Stdout, helper.Stderr = &out, &out
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // agents left running if the test fails early
		pids, _ := filepath.Glob(filepath.Join(ctrl, "pid-*"))
		for _, p := range pids {
			if data, err := os.ReadFile(p); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
					syscall.Kill(-pid, syscall.SIGKILL)
				}
			}
		}
	})
	waitFor(t, "the third run's agent", func() bool {
		_, err := os.Stat(filepath.Join(ctrl, "hanging-e1-s2-t1"))
		return err == nil
	})
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	helper.Wait()
	before := experimentRuns(t, f, "kill")
	if len(before) != 2 || before[0].Outcome != "ok" || before[1].Outcome != "ok" {
		t.Fatalf("runs finished before the kill: %+v\n%s", before, out.String())
	}

	// The killed runner's agent is still working (and could still be spending).
	expect(t, f.run(ctx, "experiment", "run", "kill"), ExitError, "may still be running", "process group")
	data, err := os.ReadFile(filepath.Join(ctrl, "pid-e1-s2-t1"))
	if err != nil {
		t.Fatal(err)
	}
	pgid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	syscall.Kill(-pgid, syscall.SIGKILL)
	waitFor(t, "the agent's process group to end", func() bool { return syscall.Kill(-pgid, 0) != nil })
	os.Remove(filepath.Join(ctrl, "hang"))
	// Its status still says running; with no process holding the run lock, it is shown as stopped.
	expect(t, f.run(ctx, "experiment", "list"), ExitOK, "stopped")
	expect(t, f.run(ctx, "experiment", "show", "kill"), ExitOK, "stopped (its Agentium process ended; run it again to resume)")

	resumed := f.run(ctx, "experiment", "run", "kill")
	expect(t, resumed, ExitOK, "Recovered run ", "left behind by a stopped Agentium: cancelled, $0.08", "Resuming experiment kill",
		"Experiment kill: done", "6 of 6 runs settled")
	after := experimentRuns(t, f, "kill")
	if len(after) != 7 || after[0].ID != before[0].ID || after[1].ID != before[1].ID {
		t.Fatalf("runs after the resume: %d, first %s %s (want %s %s)", len(after), after[0].ID, after[1].ID, before[0].ID, before[1].ID)
	}
	cancelled := 0
	for _, r := range after {
		if r.Outcome == "cancelled" {
			cancelled++
			if r.Slot != 2 || r.CostUSD < 0.08 || !strings.Contains(string(r.Record), "Agentium stopped during this run") {
				t.Errorf("recovered run %+v", r)
			}
		}
	}
	if cancelled != 1 {
		t.Errorf("%d cancelled runs, want the one recovered", cancelled)
	}
	if _, err := os.Stat(filepath.Join(ctrl, "settings-e1-s2-t2")); err != nil {
		t.Error("the slot ran again in a fresh workspace (its second try)")
	}
	emptyWorkspaces(t, f)
}

// Ctrl-C stops an experiment: the run in progress is interrupted and stored as cancelled (not an attempt), and the
// next run resumes with that slot.
func TestExperimentRunInterrupted(t *testing.T) {
	f, ctrl := experimentFixture(t)
	expect(t, f.run(context.Background(), "experiment", "new", "stop", "--b", "lean", "--task", "value", "--repeats", "1", "--concurrency", "1"), ExitOK)
	if err := os.WriteFile(filepath.Join(ctrl, "hang"), []byte("s1-t1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(t, "the second run's agent", func() bool {
			_, err := os.Stat(filepath.Join(ctrl, "hanging-e1-s1-t1"))
			return err == nil
		})
		cancel()
	}()
	expect(t, f.run(ctx, "experiment", "run", "stop"), ExitError, "cancelled", "Experiment stop: stopped: interrupted", "1 of 2 runs settled",
		"To continue: agentium experiment run stop")
	os.Remove(filepath.Join(ctrl, "hang"))
	expect(t, f.run(context.Background(), "experiment", "run", "stop"), ExitOK, "[2/2] value", "Experiment stop: done", "2 of 2 runs settled")
	runs := experimentRuns(t, f, "stop")
	if len(runs) != 3 || runs[1].Outcome != "cancelled" || runs[1].CostUSD < 0.08 || runs[2].Attempt != 1 || runs[2].Slot != runs[1].Slot {
		t.Errorf("runs %+v: the interrupted run is stored with its spend, and the slot's next run is still its first attempt", runs)
	}
	if _, err := os.Stat(filepath.Join(ctrl, "settings-e1-s1-t2")); err != nil {
		t.Error("the slot's next run did not get a fresh workspace (its second try)")
	}
	emptyWorkspaces(t, f)
}

// Ctrl-C while a run is still in its setup: the run has no outcome and no record, and the progress line says it was
// stopped before its agent started (not "none found"); it is not an attempt, so the resume runs the whole schedule.
func TestExperimentRunInterruptedBeforeAgent(t *testing.T) {
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	slow := filepath.Join(ctrl, "slow")
	setup := "if [ -f " + slow + " ]; then touch " + filepath.Join(ctrl, "in-setup") + "; sleep 30; fi"
	expect(t, f.run(ctx, "task", "edit", "value", "--setup", setup, "--reviewed"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "value", "--snapshot", "lean"), ExitOK)
	expect(t, f.run(ctx, "experiment", "new", "early", "--b", "lean", "--task", "value", "--repeats", "1", "--concurrency", "1"), ExitOK)
	writeFile(t, ctrl, "slow", "")
	stop, cancel := context.WithCancel(ctx)
	go func() {
		waitFor(t, "the run's setup", func() bool {
			_, err := os.Stat(filepath.Join(ctrl, "in-setup"))
			return err == nil
		})
		cancel()
	}()
	res := f.run(stop, "experiment", "run", "early")
	expect(t, res, ExitError, "stopped before its agent started", "Experiment early: stopped: interrupted")
	if strings.Contains(res.stdout, "none found") {
		t.Errorf("the interrupted run reads as none found:\n%s", res.stdout)
	}
	os.Remove(slow)
	expect(t, f.run(ctx, "experiment", "run", "early"), ExitOK, "Experiment early: done", "2 of 2 runs settled")
	runs := experimentRuns(t, f, "early")
	if len(runs) != 2 || runs[0].Attempt != 1 || runs[1].Attempt != 1 {
		t.Errorf("runs %+v: want the two slots' first attempts, and nothing stored for the interrupted try", runs)
	}
	emptyWorkspaces(t, f)
}
