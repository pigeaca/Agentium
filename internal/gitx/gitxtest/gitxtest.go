// Package gitxtest counts the git processes a test starts, so a test can say how often git is asked, not only what
// was answered.
package gitxtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Calls puts a git in front of PATH that records each call's arguments and then runs the real git, for the rest of
// the test. The test must not run in parallel with others: it sets PATH. The function returned gives the calls
// recorded since it was last called, in the order they started, each without the settings every Agentium git call
// begins with ("-c name=value").
func Calls(t *testing.T) func() [][]string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	// Arguments end with a unit separator and calls with a record separator: an argument may hold spaces and newlines.
	// A call is appended in one write, so calls made at the same time do not mix.
	script := "#!/bin/sh\ncall=$(printf '%s\\037' \"$@\")\nprintf '%s\\036' \"$call\" >> " + quote(log) + "\nexec " + quote(git) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	read := 0
	return func() [][]string {
		t.Helper()
		data, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		records := strings.Split(string(data), "\x1e")
		records = records[:len(records)-1] // what follows the last separator is empty, or a call still being written
		var calls [][]string
		for _, record := range records[read:] {
			args := strings.Split(strings.TrimSuffix(record, "\x1f"), "\x1f")
			for len(args) >= 2 && args[0] == "-c" {
				args = args[2:]
			}
			calls = append(calls, args)
		}
		read = len(records)
		return calls
	}
}

// Subcommand is the git command a call runs: its first argument after where the repository is ("-C" or "--git-dir",
// each with its value). It is "" for a call with none.
func Subcommand(call []string) string {
	for len(call) >= 2 && (call[0] == "-C" || call[0] == "--git-dir") {
		call = call[2:]
	}
	if len(call) == 0 {
		return ""
	}
	return call[0]
}

// Count is how many of calls run the git command sub.
func Count(calls [][]string, sub string) int {
	n := 0
	for _, call := range calls {
		if Subcommand(call) == sub {
			n++
		}
	}
	return n
}

// Repeated lists the calls whose arguments an earlier call already had: git asked the same thing twice.
func Repeated(calls [][]string) [][]string {
	seen := map[string]bool{}
	var repeated [][]string
	for _, call := range calls {
		key := strings.Join(call, "\x1f")
		if seen[key] {
			repeated = append(repeated, call)
		}
		seen[key] = true
	}
	return repeated
}

// quote writes s as one word of a shell command.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
