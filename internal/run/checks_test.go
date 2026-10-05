package run

import (
	"bytes"
	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/codex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestChangedPaths(t *testing.T) {
	data, err := os.ReadFile("testdata/checks/rename-delete.diff")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ChangedPaths(bytes.NewReader(data))
	if !ok {
		t.Fatal("the change cannot be read")
	}
	// A rename counts under both names, a deleted file is changed, a name with spaces is whole, a quoted name is read, and a
	// line inside a hunk that looks like a header is not one.
	want := []string{"docs/old name.md", "docs/new name.md", "internal/gone.go", "src/my file.go", "src/café.go"}
	if !slices.Equal(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
	if got, ok := ChangedPaths(bytes.NewReader(nil)); len(got) != 0 || !ok {
		t.Errorf("an empty change touches %q (read: %v)", got, ok)
	}
}

func TestCheckPaths(t *testing.T) {
	paths := []string{"internal/gone.go", "docs/new name.md"}
	for _, c := range []struct {
		kind, glob string
		want       bool
	}{
		{CheckChanged, "docs/**", true},
		{CheckChanged, "**/*_test.go", false},
		{CheckNotChanged, "**/*_test.go", true},
		{CheckNotChanged, "internal/**", false}, // a deleted file is changed
	} {
		if got := CheckPaths(paths, c.kind, c.glob); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.kind, c.glob, got, c.want)
		}
	}
	// An empty change is a change of nothing.
	if CheckPaths(nil, CheckChanged, "**") || !CheckPaths(nil, CheckNotChanged, "**") {
		t.Error("an empty change: changed must be unmet and not-changed met")
	}
}

func TestCheckReaderResults(t *testing.T) {
	records := t.TempDir()
	transcript, err := os.ReadFile("testdata/denied-tests.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	diff, err := os.ReadFile("testdata/checks/rename-delete.diff")
	if err != nil {
		t.Fatal(err)
	}
	put := func(id, name string, data []byte) {
		if err := os.MkdirAll(filepath.Join(records, id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(records, id, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("full", "stream.jsonl", transcript)
	put("full", "agent.diff", diff)
	put("empty", "agent.diff", nil)
	checks := []Check{
		{"grep", CheckRan, "grep -rn"},
		{"denied", CheckRan, "pytest"}, // the only command with it was denied: it never ran
		{"case", CheckRan, "GREP"},
		{"docs", CheckChanged, "docs/**"},
		{"tests", CheckChanged, "**/*_test.go"},
		{"quiet", CheckNotChanged, "**/*_test.go"},
	}
	r := CheckReader{Records: records}
	cases := []struct {
		rec  Record
		want []CheckResult
	}{
		{Record{ID: "full"}, []CheckResult{CheckMet, CheckUnmet, CheckUnmet, CheckMet, CheckUnmet, CheckMet}},
		// An empty change is a change of nothing; no transcript: the ran checks are unread.
		{Record{ID: "empty"}, []CheckResult{CheckUnread, CheckUnread, CheckUnread, CheckUnmet, CheckUnmet, CheckMet}},
		{Record{ID: "missing"}, []CheckResult{CheckUnread, CheckUnread, CheckUnread, CheckUnread, CheckUnread, CheckUnread}},
		// A run whose agent this Agentium does not know cannot have its transcript read; its change still can.
		{Record{ID: "full", Agent: "other-agent"}, []CheckResult{CheckUnread, CheckUnread, CheckUnread, CheckMet, CheckUnmet, CheckMet}},
	}
	for _, c := range cases {
		if got := r.Results(c.rec, checks); !slices.Equal(got, c.want) {
			t.Errorf("%s (%q) = %v, want %v", c.rec.ID, c.rec.Agent, got, c.want)
		}
	}
	// A record whose own folder holds the files is read too (a moved data folder).
	own := t.TempDir()
	if err := os.WriteFile(filepath.Join(own, "agent.diff"), diff, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := r.Results(Record{ID: "elsewhere", RecordsDir: own}, checks[3:4]); got[0] != CheckMet {
		t.Errorf("a record's own folder: %v", got)
	}
	// A file that exists but cannot be read is unread, not unmet.
	if err := os.MkdirAll(filepath.Join(records, "dir", "agent.diff"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := r.Results(Record{ID: "dir"}, checks[3:4]); got[0] != CheckUnread {
		t.Errorf("an unreadable change: %v", got)
	}
	// No checks read nothing.
	if got := r.Results(Record{ID: "full"}, nil); len(got) != 0 {
		t.Errorf("no checks: %v", got)
	}
}

// A Codex run's commands are read through its own adapter, from the files Codex names, in its records folder.
func TestCheckReaderReadsCodexRuns(t *testing.T) {
	records := t.TempDir()
	dir := filepath.Join(records, "codex-run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{"exec-ok.jsonl": agent.Transcript, "rollout-ok.jsonl": codex.Rollout} {
		data, err := os.ReadFile(filepath.Join("..", "codex", "testdata", src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checks := []Check{{"rg", CheckRan, "rg --files"}, {"heredoc", CheckRan, "cat > notes.txt"}, {"none", CheckRan, "no such command"}}
	r := CheckReader{Records: records}
	got := r.Results(Record{ID: "codex-run", Agent: codex.Name}, checks)
	if want := []CheckResult{CheckMet, CheckMet, CheckUnmet}; !slices.Equal(got, want) {
		t.Errorf("a Codex run = %v, want %v", got, want)
	}
}

// Headers as older runs stored them (no prefix, mnemonic prefixes), the fallback to the ---/+++ lines, and a block that
// gives no path at all, which makes the change unread.
func TestChangedPathsHeaderForms(t *testing.T) {
	for name, c := range map[string]struct {
		diff string
		want []string
		ok   bool
	}{
		"no prefix":         {"diff --git docs/x.md docs/x.md\nindex 1..2 100644\n--- docs/x.md\n+++ docs/x.md\n@@ -1 +1 @@\n-a\n+b\n", []string{"docs/x.md"}, true},
		"mnemonic index":    {"diff --git i/docs/x.md w/docs/x.md\n--- i/docs/x.md\n+++ w/docs/x.md\n@@ -1 +1 @@\n-a\n+b\n", []string{"docs/x.md"}, true},
		"mnemonic commit":   {"diff --git c/a o/a\n@@ -1 +1 @@\n", []string{"a"}, true},
		"no prefix, spaces": {"diff --git my dir/x y.md my dir/x y.md\n", []string{"my dir/x y.md"}, true},
		"quoted, no prefix": {"diff --git \"caf\\303\\251.go\" \"caf\\303\\251.go\"\n", []string{"café.go"}, true},
		// An unreadable header takes its names from the ---/+++ lines; /dev/null is no file.
		"fallback":     {"diff --git old name new name\n--- a/old name\n+++ b/new name\n@@ -1 +1 @@\n", []string{"old name", "new name"}, true},
		"fallback new": {"diff --git x y\nnew file mode 100644\n--- /dev/null\n+++ b/z.go\n@@ -0,0 +1 @@\n", []string{"z.go"}, true},
		// A block that gives no path at all: unread, never "nothing changed".
		"no path":        {"diff --git x y\nBinary files differ\n", nil, false},
		"one bad of two": {"diff --git a/ok b/ok\n@@ -1 +1 @@\n-a\n+b\ndiff --git x y\n", nil, false},
	} {
		got, ok := ChangedPaths(strings.NewReader(c.diff))
		if ok != c.ok || (c.ok && !slices.Equal(got, c.want)) {
			t.Errorf("%s: %q, %v; want %q, %v", name, got, ok, c.want, c.ok)
		}
	}
	// A very long line is cut, not an error.
	long := "diff --git a/p b/p\n@@ -1 +1 @@\n+" + strings.Repeat("x", 3<<20) + "\ndiff --git a/q b/q\n"
	if got, ok := ChangedPaths(strings.NewReader(long)); !ok || !slices.Equal(got, []string{"p", "q"}) {
		t.Errorf("a long line: %q, %v", got, ok)
	}
	records := t.TempDir()
	if err := os.MkdirAll(filepath.Join(records, "r"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(records, "r", "agent.diff"), []byte("diff --git x y\nBinary files differ\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := CheckReader{Records: records}.Results(Record{ID: "r"}, []Check{{"c", CheckChanged, "**"}, {"n", CheckNotChanged, "**"}})
	if got[0] != CheckUnread || got[1] != CheckUnread {
		t.Errorf("a change with a block that names no file: %v, want unread", got)
	}
	// An old diff without prefixes is counted: docs changed.
	if err := os.WriteFile(filepath.Join(records, "r", "agent.diff"), []byte("diff --git docs/x.md docs/x.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = CheckReader{Records: records}.Results(Record{ID: "r"}, []Check{{"c", CheckChanged, "docs/**"}, {"n", CheckNotChanged, "docs/**"}})
	if got[0] != CheckMet || got[1] != CheckUnmet {
		t.Errorf("an old diff without prefixes: %v", got)
	}
}

// When the data folder's records folder for the run holds no transcript, the record's own folder is tried.
func TestCheckReaderTranscriptFallbackFolder(t *testing.T) {
	records, own := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(records, "r"), 0o700); err != nil { // exists, empty
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/denied-tests.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "stream.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	checks := []Check{{"grep", CheckRan, "grep -rn"}, {"none", CheckRan, "zzz"}}
	got := CheckReader{Records: records}.Results(Record{ID: "r", RecordsDir: own}, checks)
	if got[0] != CheckMet || got[1] != CheckUnmet {
		t.Errorf("the record's own folder = %v", got)
	}
	if got := (CheckReader{Records: records}).Results(Record{ID: "r"}, checks); got[0] != CheckUnread {
		t.Errorf("no transcript anywhere = %v, want unread", got)
	}
}
