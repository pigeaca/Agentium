package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskImportValidateAndManage(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "run_tests.sh", "grep -q Rules CLAUDE.md || { echo 'CLAUDE.md lost its rules'; exit 1; }\n"+
		"for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n")
	writeFile(t, repo, "CLAUDE.md", "# Rules\nKeep tests green.\n")
	writeFile(t, repo, "value.txt", "old\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	writeFile(t, repo, "tests/value_test.sh", "grep -q new value.txt\n")
	writeFile(t, repo, "value.txt", "new\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "Make value new\n\nChange value.txt so the value reads new.")
	solution := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	writeFile(t, repo, "notes.md", "docs only\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "docs only")

	// A fake gh that answers `gh pr view N --json ...` from pr-N.json.
	ghDir := t.TempDir()
	gh := filepath.Join(ghDir, "gh")
	writeFile(t, ghDir, "gh", "#!/bin/sh\ncat \""+ghDir+"/pr-$3.json\" 2>/dev/null || { echo 'no such pull request' >&2; exit 1; }\n")
	if err := os.Chmod(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	files := `"files":[{"path":"tests/value_test.sh","additions":1,"deletions":0},{"path":"value.txt","additions":1,"deletions":1}]`
	writeFile(t, ghDir, "pr-7.json", `{"number":7,"title":"Make value new (PR)","body":"Please.","state":"MERGED","mergeCommit":{"oid":"`+solution+`"},`+files+`}`)
	writeFile(t, ghDir, "pr-8.json", `{"number":8,"title":"Open","body":"","state":"OPEN","mergeCommit":null,"files":[]}`)
	// Rebase-merged: the PR's first commit also touched value.txt, so the whole PR has more lines than its last commit.
	writeFile(t, ghDir, "pr-9.json", `{"number":9,"title":"Rebased","body":"","state":"MERGED","mergeCommit":{"oid":"`+solution+`"},`+
		`"files":[{"path":"tests/value_test.sh","additions":1,"deletions":0},{"path":"value.txt","additions":3,"deletions":1}]}`)

	data := filepath.Join(t.TempDir(), "data")
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": t.TempDir(), "AGENTIUM_CLAUDE": filepath.Join(t.TempDir(), "no-claude")}
	run := func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{
			Args: args, Stdout: &stdout, Stderr: &stderr, Dir: repo,
			Getenv: func(key string) string { return vars[key] },
			LookPath: func(name string) (string, error) {
				if name == "gh" {
					return gh, nil
				}
				return "", os.ErrNotExist
			},
			Now: func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) },
		})
		return cliResult{code, stdout.String(), stderr.String()}
	}
	expect(t, run("init"), ExitOK)
	writeFile(t, repo, "CLAUDE.md", "Be brief.\n") // a context version that breaks the repository's own check
	expect(t, run("context", "snapshot", "broken", "--working-tree"), ExitOK)
	gitIn(t, repo, "checkout", "--", "CLAUDE.md")
	before := repoState(t, repo)

	expect(t, run("task", "import", "--commit", "HEAD~1"), ExitError, "no test commands were detected")
	imported := run("task", "import", "--commit", "HEAD~1", "--verify", "sh run_tests.sh")
	expect(t, imported, ExitOK, "Added task make-value-new-"+solution[:7], "1 hidden test file(s), 1 reference file(s)", "Review the instruction")
	name := "make-value-new-" + solution[:7]
	expect(t, run("task", "import", "--commit", "HEAD", "--verify", "true"), ExitError, "changes no test files")
	expect(t, run("task", "list"), ExitOK, name, "commit "+solution[:12], "not validated (instruction not reviewed)")
	expect(t, run("task", "show", name), ExitOK, "hidden     tests/value_test.sh", "reference  value.txt",
		"review it for solution leaks", "Change value.txt so", "note: the instruction names reference file value.txt")

	expect(t, run("task", "validate", name), ExitOK, "base       hidden-tests", "reference", "Result: valid")
	expect(t, run("task", "validate", name, "--snapshot", "broken"), ExitError, "Result: invalid: broken/reference wanted pass")
	expect(t, run("task", "list"), ExitOK, "invalid: broken/reference wanted pass")
	expect(t, run("task", "validate", name, "--snapshot", "broken", "--snapshot", "broken"), ExitUsage, "repeated or reserved")
	expect(t, run("task", "validate", name, "--snapshot", "base"), ExitUsage, "repeated or reserved")

	expect(t, run("task", "validate", name, "--repeat", "0"), ExitUsage, "--repeat must be 1 to 20")
	repeated := run("task", "validate", name, "--repeat", "3")
	expect(t, repeated, ExitOK, "(1/3)", "(3/3)", "Result: valid")
	if strings.Contains(repeated.stdout, "flaky") {
		t.Errorf("a steady task:\n%s", repeated.stdout)
	}

	// A check that flips between pass and fail (a marker outside every fresh checkout) makes the task flaky.
	marker := filepath.Join(t.TempDir(), "marker")
	flip := "if [ -e " + marker + " ]; then rm " + marker + "; exit 1; else touch " + marker + "; fi"
	expect(t, run("task", "add", "flip", "--base", "HEAD", "--instruction", "Anything.", "--verify", flip), ExitOK)
	expect(t, run("task", "validate", "flip", "--repeat", "3"), ExitError, "flip", "(2/3)", "flaky: base/base passed 2 of 3 times",
		"Result: flaky: base/base passed 2 of 3 times")
	expect(t, run("task", "list"), ExitOK, "flaky: base/base passed 2 of 3 times")
	expect(t, run("task", "show", "flip"), ExitOK, "status     flaky: base/base passed 2 of 3 times")

	expect(t, run("task", "edit", name, "--reviewed"), ExitOK)
	expect(t, run("task", "edit", name, "--verify", "sh run_tests.sh", "--verify", "true"), ExitOK)
	list := run("task", "list")
	if strings.Contains(list.stdout, "not reviewed") || !strings.Contains(list.stdout, "not validated") {
		t.Errorf("after edits:\n%s", list.stdout)
	}

	expect(t, run("task", "add", "manual", "--base", "HEAD", "--instruction", "Document the value.", "--verify", "test -f built", "--verify", "sh run_tests.sh"), ExitOK)
	expect(t, run("task", "validate", "manual"), ExitError, "Result: invalid: base/base wanted pass")
	expect(t, run("task", "edit", "manual", "--setup", "exit 3"), ExitOK)
	expect(t, run("task", "validate", "manual"), ExitError, "got setup failed", "Result: invalid: base/base setup failed")
	expect(t, run("task", "edit", "manual", "--setup", "touch built"), ExitOK)
	expect(t, run("task", "show", "manual"), ExitOK, "setup      touch built", "status     not validated")
	expect(t, run("task", "validate", "manual"), ExitOK, "base       base          want pass got pass ok", "Result: unchecked")
	expect(t, run("task", "edit", "manual", "--setup", "x", "--no-setup"), ExitUsage)
	expect(t, run("task", "add", "manual", "--base", "HEAD", "--instruction", "Again.", "--verify", "true"), ExitError, "already exists")
	expect(t, run("task", "add", "nothing", "--base", "HEAD"), ExitUsage, "an instruction is required")

	expect(t, run("task", "import", "--pr", "7", "--name", "from-pr", "--verify", "sh run_tests.sh"), ExitOK, "Added task from-pr (pr #7)")
	expect(t, run("task", "show", "from-pr"), ExitOK, "Make value new (PR)", "Please.")
	expect(t, run("task", "import", "--pr", "8"), ExitError, "is open, not merged")
	expect(t, run("task", "import", "--pr", "9", "--verify", "true"), ExitError, "probably rebase-merged", "value.txt: +3 -1 in the pull request, +1 -1 in the commit")
	expect(t, run("task", "import", "--pr", "10"), ExitError, "no such pull request")
	expect(t, run("task", "import", "--commit", "HEAD", "--pr", "7"), ExitUsage, "give --commit REF or --pr N")

	expect(t, run("task", "rm", "manual"), ExitOK)
	expect(t, run("task", "rm", "manual"), ExitError, "not found")
	if after := repoState(t, repo); after != before {
		t.Errorf("task commands modified the repository:\nbefore %s\nafter  %s", before, after)
	}
}

func TestTaskName(t *testing.T) {
	commit := "0123456789abcdef"
	for subject, want := range map[string]string{
		"fix(harness): map only the harness files to check harness": "fix-harness-map-only-the-harness-files-0123456",
		"":    "c0123456",
		"!!!": "c0123456",
	} {
		if got := taskName(subject, commit); got != want {
			t.Errorf("taskName(%q) = %q, want %q", subject, got, want)
		}
	}
}

func TestParseNumstat(t *testing.T) {
	out := "3\t1\tsrc/a.go\x00" + "0\t0\t\x00old/name.go\x00new/name.go\x00" + "-\t-\tlogo.png\x00"
	got, err := parseNumstat(out)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]lineCounts{"src/a.go": {3, 1}, "new/name.go": {0, 0}, "logo.png": {-1, -1}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for path, counts := range want {
		if got[path] != counts {
			t.Errorf("%s = %v, want %v", path, got[path], counts)
		}
	}
	if _, err := parseNumstat("garbage\x00"); err == nil {
		t.Error("a malformed record must be an error")
	}
}

func TestTaskFairnessGaps(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "go.mod", "module example.com/m\n\ngo 1.22\n")
	writeFile(t, repo, "m.go", "package m\n\nfunc Check(s string) error { return nil }\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	writeFile(t, repo, "m.go", "package m\n\nimport \"errors\"\n\nfunc Check(s string) error { return errors.New(\"value must not be empty\") }\n")
	writeFile(t, repo, "m_test.go", "package m\n\nimport \"testing\"\n\nfunc TestCheck(t *testing.T) {\n\tif err := Check(\"\"); err == nil || err.Error() != \"value must not be empty\" {\n\t\tt.Fatal(err)\n\t}\n}\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "Reject empty values\n\nCheck should fail on an empty string.")

	vars := map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "data"), "HOME": t.TempDir(), "AGENTIUM_CLAUDE": filepath.Join(t.TempDir(), "no-claude")}
	run := func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{
			Args: args, Stdout: &stdout, Stderr: &stderr, Dir: repo,
			Getenv:   func(key string) string { return vars[key] },
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Now:      func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) },
		})
		return cliResult{code, stdout.String(), stderr.String()}
	}
	expect(t, run("init"), ExitOK)
	writeFile(t, repo, "CLAUDE.md", "# Rules\nKeep it short.\n")
	expect(t, run("context", "snapshot", "lean", "--working-tree"), ExitOK)
	verify := `grep -q "value must not be empty" m.go`
	expect(t, run("task", "import", "--commit", "HEAD", "--name", "empty", "--verify", verify), ExitOK)
	expect(t, run("task", "list"), ExitOK, "1 unstated requirement(s)")
	expect(t, run("task", "show", "empty"), ExitOK, "Unstated requirements (1)", `text "value must not be empty" (m_test.go)`)
	expect(t, run("task", "validate", "empty"), ExitOK, "Result: valid", "Unstated requirements (1)", `text "value must not be empty"`)
	expect(t, run("task", "validate", "empty", "--snapshot", "lean"), ExitOK, "Result: valid")

	// Every way to mark the task reviewed is gated, and nothing changes when it is refused.
	expect(t, run("task", "edit", "empty", "--reviewed"), ExitError, "value must not be empty", "pass --accept-gaps")
	expect(t, run("task", "edit", "empty", "--instruction", "Reject empty values."), ExitError, "value must not be empty", "pass --accept-gaps")
	expect(t, run("task", "list"), ExitOK, "instruction not reviewed")
	expect(t, run("task", "edit", "empty", "--accept-gaps"), ExitUsage, "at least one of")
	// The plan warns while the gap stands.
	expect(t, run("experiment", "new", "gaps-ab", "--b", "lean", "--task", "empty"), ExitError, "instruction needs a review")
	expect(t, run("task", "edit", "empty", "--reviewed", "--accept-gaps"), ExitOK)
	expect(t, run("experiment", "new", "gaps-ab", "--b", "lean", "--task", "empty"), ExitOK)
	expect(t, run("experiment", "plan", "gaps-ab"), ExitOK, "WARNING  hidden tests require what nothing states", "empty (1)")

	// Stating the text in the instruction clears the list, and needs no acceptance.
	expect(t, run("task", "edit", "empty", "--instruction", `Check returns the error "value must not be empty" for an empty string.`), ExitOK)
	if list := run("task", "list"); strings.Contains(list.stdout, "unstated") || strings.Contains(list.stdout, "not reviewed") {
		t.Errorf("after stating it:\n%s", list.stdout)
	}
	if plan := run("experiment", "plan", "gaps-ab"); strings.Contains(plan.stdout, "require what nothing states") {
		t.Errorf("after stating it:\n%s", plan.stdout)
	}

	// A task added by hand is reviewed from the start, so it is gated too.
	base, solution := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD~1")), "HEAD"
	expect(t, run("task", "add", "by-hand", "--base", base, "--solution", solution, "--instruction", "Reject empty values.", "--verify", verify),
		ExitError, "value must not be empty", "pass --accept-gaps")
	expect(t, run("task", "add", "by-hand", "--base", base, "--solution", solution, "--instruction", "Reject empty values.", "--verify", verify,
		"--accept-gaps"), ExitOK)
	expect(t, run("task", "add", "stated", "--base", base, "--solution", solution, "--verify", verify,
		"--instruction", `Check returns the error "value must not be empty" for an empty string.`), ExitOK)
}
