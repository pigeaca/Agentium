package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/term"
)

// On a terminal the report is rendered for reading (no Markdown markup, colored unless NO_COLOR); --markdown, --out
// and a pipe keep the Markdown, byte for byte; --json is unchanged.
func TestExperimentReportOnATerminal(t *testing.T) {
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--repeats", "3", "--seed", "5"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "lean-ab", "--budget", "30"), ExitOK)

	piped := f.run(ctx, "experiment", "report", "lean-ab")
	expect(t, piped, ExitOK, "# Experiment lean-ab", "**Cost**: no result", "| value | ●●● 3/3 | ●●● 3/3 | $0.300 → $0.300 |")

	*f.terminal = true
	colored := f.run(ctx, "experiment", "report", "lean-ab")
	expect(t, cliResult{colored.code, term.Plain(colored.stdout), colored.stderr}, ExitOK, "Experiment lean-ab", "Cost: no result (fewer than two tasks", "6 of 6 runs settled")
	if !strings.Contains(colored.stdout, "\x1b[") {
		t.Errorf("a terminal report has no styles:\n%s", colored.stdout)
	}
	for _, markup := range []string{"# Experiment", "**", "`", "| value"} {
		if strings.Contains(term.Plain(colored.stdout), markup) {
			t.Errorf("the terminal report has markup %q:\n%s", markup, colored.stdout)
		}
	}

	f.vars["NO_COLOR"] = "1"
	plain := f.run(ctx, "experiment", "report", "lean-ab")
	expect(t, plain, ExitOK, "Experiment lean-ab")
	if strings.Contains(plain.stdout, "\x1b") || plain.stdout != term.Plain(colored.stdout) {
		t.Errorf("NO_COLOR report differs from the colored one without its styles:\n%s", plain.stdout)
	}
	delete(f.vars, "NO_COLOR")

	// --markdown forces Markdown on a terminal: what a pipe gets.
	if forced := f.run(ctx, "experiment", "report", "lean-ab", "--markdown"); forced.code != ExitOK || forced.stdout != piped.stdout {
		t.Errorf("--markdown on a terminal differs from the piped report (exit %d):\n%s", forced.code, forced.stdout)
	}
	// --out writes Markdown, and --json is JSON, whatever the terminal.
	out := filepath.Join(t.TempDir(), "report.md")
	expect(t, f.run(ctx, "experiment", "report", "lean-ab", "--out", out), ExitOK, "Wrote the report of lean-ab to "+out)
	if data, err := os.ReadFile(out); err != nil || string(data) != piped.stdout {
		t.Errorf("--out on a terminal: %v; it must hold the Markdown", err)
	}
	if js := f.run(ctx, "experiment", "report", "lean-ab", "--json"); js.code != ExitOK || !strings.Contains(js.stdout, `"experiment": "lean-ab"`) || strings.Contains(js.stdout, "\x1b") {
		t.Errorf("--json on a terminal:\n%s", js.stdout)
	}
}
