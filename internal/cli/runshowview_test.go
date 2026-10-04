package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// runScenes are runs as run show draws them: passed in the sandbox, failed on the host, graded by the judge, not
// graded after an infrastructure failure, and one whose sandbox check failed.
func runScenes() map[string]run.Record {
	started := time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)
	base := func() run.Record {
		return run.Record{
			ID: "20261003T140000Z-a1b2c3", Task: "fix-parser", Arm: "B", ContextHead: "9f3c2d1e5a", Outcome: claude.OutcomeOK, Passed: ptr(true),
			Grader: task.GraderSandbox, Started: started, Finished: started.Add(3*time.Minute + 12*time.Second), RecordsDir: "~/runs/20261003T140000Z-a1b2c3",
			Metrics: claude.Metrics{CLIVersion: "2.1.281", Model: "claude-sonnet-5", Turns: 9, DurationMS: 161000, CostUSD: 0.4231,
				ToolUses: map[string]int{"Bash": 5, "Edit": 3, "Read": 9, "Grep": 2}},
			Behavior: run.Behavior{FilesChanged: 3, LinesAdded: 41, LinesRemoved: 2},
			Setup:    []task.Command{{Command: "go mod download", Seconds: 4}},
			Verify:   []task.Command{{Command: "go test ./...", Seconds: 18}},
			Sandbox:  &task.SandboxGrade{Canary: task.CanaryPassed, DenialCount: 2},
		}
	}
	passed := base()

	host := base()
	host.Passed, host.Grader, host.Sandbox, host.Setup = ptr(false), task.GraderHost, nil, nil
	host.Notes = []string{"the agent changed go.mod"}

	judged := base()
	judged.GradedBy, judged.Sandbox, judged.Verify, judged.Grader = task.GradingJudge, nil, nil, ""
	judged.Judge = &judge.Verdict{Fixed: judge.Yes, Answers: []string{judge.Yes, judge.Yes, judge.Partly}, Requested: 3, Model: "claude-opus-5-5", CostUSD: 0.21}

	infra := base()
	infra.Outcome, infra.Passed, infra.Sandbox, infra.Verify, infra.Metrics.DurationMS = claude.OutcomeInfra, nil, nil, nil, 4000
	infra.Metrics.ToolUses = nil

	canary := base()
	canary.Passed, canary.Verify = nil, nil
	canary.Sandbox = &task.SandboxGrade{Canary: "the sandbox let a read through"}
	canary.Outcome = claude.OutcomeCapped
	canary.Drift = []string{"permission mode \"default\", not \"acceptEdits\""}
	return map[string]run.Record{"passed": passed, "host": host, "judged": judged, "infra": infra, "canary": canary}
}

var runSceneNames = []string{"passed", "host", "judged", "infra", "canary"}

// TestRunShowPreview writes each scene's view for scripts/readme_images/ansi2svg.py: AGENTIUM_RUN_DEMO names the folder.
func TestRunShowPreview(t *testing.T) {
	dir := os.Getenv("AGENTIUM_RUN_DEMO")
	if dir == "" {
		t.Skip("set AGENTIUM_RUN_DEMO to a folder to write run show's previews")
	}
	sh := term.Shapes{Style: term.Colored().WithDepth(term.Color256)}
	for name, rec := range runScenes() {
		text := "\x1b[36m$\x1b[39m agentium run show " + rec.ID + "\n" + strings.Join(runShowView(rec, "experiment ctx-ab, slot 3 (from 0), attempt 1", sh, 80), "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "run-show-"+name+".ans"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Each scene at the designed widths in plain Unicode, and the first in 256 colors and in ASCII. No line is wider than
// the terminal.
func TestRunShowViewGoldens(t *testing.T) {
	scenes := runScenes()
	for _, name := range runSceneNames {
		var b strings.Builder
		for _, width := range []int{term.MinWidth, 80, 120} {
			fmt.Fprintf(&b, "=== %d columns\n", width)
			for _, line := range runShowView(scenes[name], "experiment ctx-ab, slot 3 (from 0), attempt 1", plainUnicode, width) {
				if w := term.Width(line); w > width {
					t.Errorf("%s at %d columns: a line of %d cells: %q", name, width, w, line)
				}
				b.WriteString(line + "\n")
			}
		}
		checkGolden(t, "runshow-"+name+".golden", b.String())
	}
	where := "experiment ctx-ab, slot 3 (from 0), attempt 1"
	checkGolden(t, "runshow-passed-color.golden", strings.Join(runShowView(scenes["passed"], where, color256, 80), "\n")+"\n")
	checkGolden(t, "runshow-passed-ascii.golden", strings.Join(runShowView(scenes["passed"], where, plainASCII, 80), "\n")+"\n")
}

// The chain says what happened in plain words, and nothing from outside Agentium reaches the screen as an escape.
func TestRunShowViewWords(t *testing.T) {
	scenes := runScenes()
	view := func(name string) string { return strings.Join(runShowView(scenes[name], "", plainUnicode, 100), "\n") }
	for name, want := range map[string][]string{
		"passed": {"fresh copy", "Claude works", "hidden tests", "result", "in a sandbox", "used Read ×9, Bash ×5, Edit ×3 and 1 more", "✓ passed", "sandbox check passed"},
		"host":   {"✗ failed", "run on your machine, outside the sandbox"},
		"judged": {"the judge", "fixed (2 of 3)", "unvalidated", "the judge says fixed"},
		"infra":  {"infrastructure failed", "! no fair attempt", "nothing was graded"},
		"canary": {"hit its cap", "the sandbox check failed: nothing was graded", "unfair: permission mode"},
	} {
		v := view(name)
		for _, w := range want {
			if !strings.Contains(v, w) {
				t.Errorf("%s: want %q in:\n%s", name, w, v)
			}
		}
	}
	if v := view("judged"); strings.Contains(v, "hidden tests") {
		t.Errorf("a judge-graded run has no hidden tests box:\n%s", v)
	}
	if v := view("passed"); strings.Contains(v, "▼") || strings.Contains(v, "▶") {
		t.Errorf("the chain has arrowheads:\n%s", v)
	}
	evil := scenes["passed"]
	evil.Task, evil.Arm, evil.Notes = "x\x1b[31m", "B\x1b]0;t\a", []string{"\x1b[2Jnote"}
	if v := strings.Join(runShowView(evil, "", plainUnicode, 80), "\n"); strings.Contains(v, "\x1b") {
		t.Errorf("an escape from a name reached the screen: %q", v)
	}
}

// Each box is two lines at most, and the sandbox's boxes are dashed in purple.
func TestRunShowViewBoxesAreSmall(t *testing.T) {
	for name, rec := range runScenes() {
		lines := runShowView(rec, "", plainUnicode, 100)
		inBox, rows := false, 0
		for _, l := range lines {
			switch {
			case strings.HasPrefix(strings.TrimSpace(l), "╭") || strings.HasPrefix(strings.TrimSpace(l), "┌"):
				inBox, rows = true, 0
			case strings.HasPrefix(strings.TrimSpace(l), "╰") || strings.HasPrefix(strings.TrimSpace(l), "└"):
				inBox = false
			case inBox:
				rows++
				if rows > 2 {
					t.Errorf("%s: a box has more than two lines:\n%s", name, strings.Join(lines, "\n"))
				}
			}
		}
	}
	colored := strings.Join(runShowView(runScenes()["passed"], "", color256, 80), "\n")
	if !strings.Contains(colored, "\x1b[38;5;141m") || !strings.Contains(colored, "┄") {
		t.Errorf("the sandbox boxes are not dashed purple:\n%s", colored)
	}
}

// On a terminal run show is the chain; --details, NO_COLOR, TERM=dumb, a narrow terminal, a pipe and --json give the
// lines as before, and --diff and --log still follow the picture.
func TestRunShowOnATerminal(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "1", "--seed", "5"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "lean-ab", "--budget", "30"), ExitOK)
	id := ""
	for _, r := range jsonRun(t, f, ExitOK, "run", "list").get("runs").([]any) {
		id = r.(map[string]any)["id"].(string)
	}
	piped := f.run(ctx, "run", "show", id)
	expect(t, piped, ExitOK, "Run "+id, "  outcome      ok;", "  experiment   lean-ab, slot ", "  files        ")

	*f.terminal = true
	designed := f.run(ctx, "run", "show", id)
	expect(t, cliResult{designed.code, term.Plain(designed.stdout), designed.stderr}, ExitOK, "fresh copy", "Claude works", "hidden tests", "result",
		"experiment lean-ab, slot ", "agentium run show "+id+" --details")
	if strings.Contains(term.Plain(designed.stdout), "outcome      ok") {
		t.Errorf("the designed view carries the plain lines:\n%s", designed.stdout)
	}
	withDiff := f.run(ctx, "run", "show", id, "--diff")
	expect(t, cliResult{withDiff.code, term.Plain(withDiff.stdout), withDiff.stderr}, ExitOK, "fresh copy", "--- agent.diff")

	details := f.run(ctx, "run", "show", id, "--details")
	if details.code != ExitOK || term.Plain(details.stdout) != piped.stdout {
		t.Errorf("--details on a terminal differs from the piped lines:\n%s", details.stdout)
	}
	f.vars["NO_COLOR"] = "1"
	if got := f.run(ctx, "run", "show", id); got.stdout != piped.stdout {
		t.Errorf("NO_COLOR differs from the piped lines:\n%s", got.stdout)
	}
	delete(f.vars, "NO_COLOR")
	for name, value := range map[string]string{"TERM": "dumb", "COLUMNS": "40"} {
		f.vars[name] = value
		if got := f.run(ctx, "run", "show", id); term.Plain(got.stdout) != piped.stdout {
			t.Errorf("%s=%s differs from the piped lines:\n%s", name, value, got.stdout)
		}
		delete(f.vars, name)
	}
	if js := f.run(ctx, "run", "show", id, "--json"); js.code != ExitOK || strings.Contains(js.stdout, "\x1b") || !strings.Contains(js.stdout, `"command": "run show"`) {
		t.Errorf("--json on a terminal:\n%s", js.stdout)
	}
}
