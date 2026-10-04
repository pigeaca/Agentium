package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// timedWriter keeps every write with the seconds since the first one, for scripts/readme_images/cast2svg.py and
// frames2svg.py.
type timedWriter struct {
	mu     sync.Mutex
	start  time.Time
	writes [][2]any
}

func (w *timedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if w.start.IsZero() {
		w.start = now
	}
	w.writes = append(w.writes, [2]any{float64(now.Sub(w.start).Milliseconds()) / 1000, string(p)})
	return len(p), nil
}

// TestRecordLiveRun is the recorder behind the console's animations, not a check: it runs only when
// AGENTIUM_RECORD_LIVE names an output file. It runs "experiment run" as on a terminal, and writes the output as JSON
// [[seconds, text], ...], the input of scripts/readme_images/frames2svg.py (the dashboard) and cast2svg.py (lines
// only). The test stand-in answers for Claude Code at its real speed (AGENTIUM_RECORD_PACE seconds a run, default
// 0.2), so nothing is paid; AGENTIUM_RECORD_COST is what its runs in the second context cost (default $0.27, the first's
// about $0.30). AGENTIUM_RECORD_VIEW picks the view:
//   - dashboard (the default): a seq-v1 experiment on 16 tasks where the second context is cheaper, on an 80 by 34
//     terminal, with the plan's usage shown;
//   - log: the same, in the log view;
//   - plain: the status line and plain lines (NO_COLOR, as a terminal without the designed views), on the one-task
//     experiment fixture.
func TestRecordLiveRun(t *testing.T) {
	out := os.Getenv("AGENTIUM_RECORD_LIVE")
	if out == "" {
		t.Skip("set AGENTIUM_RECORD_LIVE=FILE to record")
	}
	view := os.Getenv("AGENTIUM_RECORD_VIEW")
	pace := os.Getenv("AGENTIUM_RECORD_PACE")
	if pace == "" {
		pace = "0.2"
	}
	costLean := os.Getenv("AGENTIUM_RECORD_COST") // what a run in the second context costs (the first's is about $0.30)
	if costLean == "" {
		costLean = "0.27"
	}
	ctx := context.Background()
	usage := fmt.Sprintf("0.32 0.01 %d\n", time.Now().Add(3*time.Hour).Unix())
	var f runFixture
	var args []string
	if view == "plain" {
		var ctrl string
		f, ctrl = experimentFixture(t)
		control(t, ctrl, map[string]string{"usage": usage, "agent-sleep": pace})
		expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--seed", "5", "--budget", "22"), ExitOK)
		f.vars["NO_COLOR"] = "1"
		args = []string{"experiment", "run", "lean-ab"}
	} else {
		var ctrl string
		f, ctrl = seqFixture(t)
		control(t, ctrl, map[string]string{"usage": usage, "agent-sleep": pace, "cost-lean": costLean, "cost-jitter": "", "fix-lib": ""})
		expect(t, f.run(ctx, "experiment", "new", "lean-vs-base", "--b", "lean", "--seed", "5", "--grader", "sandbox"), ExitOK)
		f.vars["LANG"], f.vars["COLUMNS"], f.vars["LINES"] = "en_US.UTF-8", "80", "34"
		args = []string{"experiment", "run", "lean-vs-base"}
		if view == "log" {
			args = append(args, "--view", "log")
		}
	}
	f.vars["TERM"] = "xterm-256color"
	w := &timedWriter{}
	var stderr bytes.Buffer
	code := Run(ctx, Env{DefaultGrader: "host", Args: args, Stdout: w, Stderr: &stderr, Dir: f.repo, Terminal: true,
		Getenv:   func(key string) string { return f.vars[key] },
		Environ:  func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home} },
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now,
		Backoff: func(int) time.Duration { return 10 * time.Millisecond },
		Sleep:   func(context.Context, time.Duration) error { return nil }})
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	data, err := json.Marshal(w.writes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(out), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRecordConsoleScreens is a recorder, not a check: it runs only when AGENTIUM_RECORD_SCREENS names an existing folder.
// It runs the commands of the gallery's screens as on a 120-column terminal at 256 colors with the test stand-in for
// Claude Code (no paid runs) and writes each output as an .ans file for scripts/readme_images/ansi2svg.py: start (a
// fresh repository, up to its preview), plan, run-show (one of an experiment's runs) and pool-status. Paths of the
// test's temporary folders are written as ~/… so the pictures hold no machine's paths.
func TestRecordConsoleScreens(t *testing.T) {
	dir := os.Getenv("AGENTIUM_RECORD_SCREENS")
	if dir == "" {
		t.Skip("set AGENTIUM_RECORD_SCREENS=DIR to record")
	}
	ctx := context.Background()
	write := func(f runFixture, name, command string, res cliResult) {
		t.Helper()
		if res.code != ExitOK {
			t.Fatalf("%s: exit %d: %s", command, res.code, res.stderr)
		}
		text := res.stdout
		// Longest first, and the resolved /private/var form that macOS reports for /var.
		for _, p := range [][2]string{{"/private" + f.data, "~/.agentium"}, {f.data, "~/.agentium"}, {"/private" + f.repo, "~/code/lib"}, {f.repo, "~/code/lib"},
			{"/private" + f.home, "~"}, {f.home, "~"}} {
			text = strings.ReplaceAll(text, p[0], p[1])
		}
		text = regexp.MustCompile(`…\S*/data/records/`).ReplaceAllString(text, "~/.agentium/records/") // a cut path of the test's folder
		text = regexp.MustCompile(`\S*/claude( \d)`).ReplaceAllString(text, "~/.local/bin/claude$1")
		out := "\x1b[36m$\x1b[39m " + command + "\n" + text
		if err := os.WriteFile(filepath.Join(dir, name+".ans"), []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	terminal := func(f runFixture) {
		*f.terminal = true
		f.vars["TERM"], f.vars["LANG"], f.vars["COLUMNS"], f.vars["LINES"] = "xterm-256color", "en_US.UTF-8", "120", "40"
	}

	s, _ := startFixture(t, 8)
	terminal(s)
	write(s, "start", "agentium start --accept-mined", s.run(ctx, "start", "--accept-mined"))

	f, ctrl := seqFixture(t)
	control(t, ctrl, map[string]string{"cost-lean": "0.25", "cost-jitter": "", "fix-lib": ""})
	expect(t, f.run(ctx, "experiment", "new", "lean-vs-base", "--b", "lean", "--seed", "5", "--grader", "sandbox"), ExitOK)
	terminal(f)
	write(f, "plan", "agentium experiment plan lean-vs-base", f.run(ctx, "experiment", "plan", "lean-vs-base"))
	write(f, "pool-status", "agentium pool status", f.run(ctx, "pool", "status"))
	*f.terminal = false
	expect(t, f.run(ctx, "experiment", "run", "lean-vs-base"), ExitOK)
	id := ""
	for _, r := range jsonRun(t, f, ExitOK, "run", "list").get("runs").([]any) {
		if e := r.(map[string]any); e["arm"] == "B" && e["kind"] != "calibration" {
			id = e["id"].(string)
		}
	}
	terminal(f)
	write(f, "run-show", "agentium run show "+id, f.run(ctx, "run", "show", id))
}
