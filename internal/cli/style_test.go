package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/term"
)

// TestOutputIsStyledOnlyOnATerminal runs each command's main path. Without a terminal the output has no escape
// codes; on one, or with FORCE_COLOR, it is styled unless NO_COLOR or a dumb TERM says otherwise. Errors on stderr
// are never styled.
func TestOutputIsStyledOnlyOnATerminal(t *testing.T) {
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--seed", "5"), ExitOK)
	commands := [][]string{
		{"context", "show"}, {"context", "list"},
		{"task", "list"}, {"task", "show", "value"}, {"task", "validate", "value", "--snapshot", "lean"},
		{"experiment", "plan", "lean-ab"}, {"experiment", "run", "lean-ab"}, {"experiment", "list"}, {"experiment", "show", "lean-ab"},
		{"run", "once", "value"}, {"run", "list"},
	}
	escape := "\x1b["
	plain := func(args ...string) cliResult {
		t.Helper()
		r := f.run(ctx, args...)
		if r.code != ExitOK {
			t.Fatalf("%v: exit %d\nstdout %s\nstderr %s", args, r.code, r.stdout, r.stderr)
		}
		if strings.Contains(r.stdout+r.stderr, escape) {
			t.Errorf("%v without a terminal printed escape codes:\n%q", args, r.stdout)
		}
		return r
	}
	for _, args := range commands {
		plain(args...)
	}
	// The commands that change things, and run show with an ID from the list.
	plain("init")
	plain("context", "snapshot", "other", "--working-tree")
	plain("context", "diff", "lean", "other")
	plain("task", "add", "by-hand", "--base", "HEAD~1", "--instruction", "Make the value new.", "--solution", "HEAD", "--verify", "sh run_tests.sh")
	plain("task", "edit", "by-hand", "--reviewed")
	plain("experiment", "new", "second", "--b", "lean", "--task", "value", "--seed", "3")
	listed := strings.Split(plain("run", "list").stdout, "\n")
	plain("run", "show", strings.Fields(listed[1])[0], "--diff", "--log")
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")
	plain("run", "calibrate", "--snapshot", "lean")

	styled := func(what string, want bool) {
		t.Helper()
		for _, args := range [][]string{{"task", "list"}, {"run", "list"}, {"experiment", "list"}, {"experiment", "show", "lean-ab"},
			{"experiment", "plan", "lean-ab"}, {"context", "show"}} {
			if r := f.run(ctx, args...); strings.Contains(r.stdout, escape) != want {
				t.Errorf("%s: %v styled = %v, want %v:\n%q", what, args, !want, want, r.stdout)
			}
		}
		if r := f.run(ctx, "task", "show", "missing"); strings.Contains(r.stderr, escape) {
			t.Errorf("%s: an error was styled: %q", what, r.stderr)
		}
	}
	*f.terminal = true
	styled("a terminal", true)
	f.vars["TERM"] = "dumb"
	styled("a dumb terminal", false)
	delete(f.vars, "TERM")
	f.vars["NO_COLOR"] = "1"
	styled("NO_COLOR on a terminal", false)
	delete(f.vars, "NO_COLOR")
	*f.terminal = false
	f.vars["FORCE_COLOR"] = "1"
	styled("FORCE_COLOR into a pipe", true)

	// Styling changes no words: the plain text of a styled list is the list without a terminal.
	styledList := f.run(ctx, "run", "list").stdout
	delete(f.vars, "FORCE_COLOR")
	if plain := f.run(ctx, "run", "list").stdout; term.Plain(styledList) != plain {
		t.Errorf("styled run list differs from the plain one:\n%s\nplain:\n%s", term.Plain(styledList), plain)
	}
}
