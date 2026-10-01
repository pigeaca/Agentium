package mine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fixture is a git repository built for one test, with commits dated one day apart from 2026-01-01.
type fixture struct {
	t    *testing.T
	root string
	day  int
}

// isolateHome points HOME at a temporary folder holding a git configuration that would break a scan reading the
// user's settings (colour, renames, decorations, a file order, the patience algorithm), so the scan must fix them
// itself, and the user's own configuration cannot leak into the test.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := os.WriteFile(filepath.Join(home, "order"), []byte("*_test.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := "[user]\n\tname = Fixture\n\temail = fixture@example.com\n[init]\n\tdefaultBranch = main\n[color]\n\tui = always\n" +
		"[diff]\n\trenames = copies\n\talgorithm = patience\n\torderFile = " + filepath.Join(home, "order") + "\n[log]\n\tdecorate = full\n" +
		"[core]\n\tquotePath = true\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	isolateHome(t)
	f := &fixture{t: t, root: t.TempDir()}
	f.git("init", "-q", "-b", "main")
	return f
}

func (f *fixture) git(args ...string) string { return gitAt(f.t, f.root, f.day, args...) }

// gitAt runs git in dir with author and committer dates on the given day of January 2026.
func gitAt(t *testing.T, dir string, day int, args ...string) string {
	t.Helper()
	date := fmt.Sprintf("2026-01-%02dT12:00:00Z", day)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files (path to content; "" deletes the file) and commits them with message on the next day,
// returning the hash.
func (f *fixture) commit(message string, files map[string]string) string {
	f.t.Helper()
	f.day++
	for p, content := range files {
		full := filepath.Join(f.root, filepath.FromSlash(p))
		if content == "" {
			if err := os.Remove(full); err != nil {
				f.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.git("add", "-A")
	f.git("commit", "-q", "--allow-empty", "-m", message)
	return f.git("rev-parse", "HEAD")
}

// lines returns n distinct lines starting with prefix.
func lines(prefix string, n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return b.String()
}

// history holds the hashes of the fixture's commits by role.
type history struct {
	root, good, small, large, docs, depsFiles, depsMessage, testsOnly, feature, merge, rust, revert, sweep, generated,
	style, fixup, noSource, python, review, deleted, wip string
}

func buildHistory(t *testing.T) (*fixture, history) {
	f := newFixture(t)
	var h history
	h.root = f.commit("Initial commit", map[string]string{
		"parse.go": "package parse\n", "parse_test.go": "package parse\n", "README.md": "# parse\n",
		"go.mod": "module example.com/parse\n", "go.sum": "x\n", "lib.rs": "pub fn f() {}\n",
		"a b.go": "package parse\n\nfunc helper() {}\n",
	})
	h.good = f.commit("Add Parse for durations\n\nParse reads 1h30m-style strings and returns a time.Duration.\nCloses #12.\n\nCo-Authored-By: Someone <s@example.com>",
		map[string]string{"parse.go": "package parse\n" + lines("// parse", 30), "parse_test.go": "package parse\n" + lines("// test", 20)})
	h.small = f.commit("parse: handle negative durations", map[string]string{
		"parse.go": "package parse\n" + lines("// parse", 35), "parse_test.go": "package parse\n" + lines("// test", 25)})
	h.large = f.commit("Rewrite the parser around a state machine\n\nA table-driven state machine replaces the hand-written scanner.",
		map[string]string{"machine.go": lines("// state", 650), "machine_test.go": lines("// case", 100)})
	h.docs = f.commit("Document duration syntax in the README", map[string]string{"README.md": "# parse\n\n" + lines("syntax", 10)})
	h.depsFiles = f.commit("Refresh module requirements for the parser", map[string]string{
		"go.mod": "module example.com/parse\n\nrequire example.com/x v1.2.0\n", "go.sum": lines("example.com/x", 2), "mod_test.go": "package parse\n"})
	h.depsMessage = f.commit("chore(deps): bump yaml to v3 and adapt the loader", map[string]string{
		"go.mod": "module example.com/parse\n\nrequire example.com/yaml v3.0.0\n", "load.go": lines("// load", 20), "load_test.go": lines("// load test", 20)})
	h.testsOnly = f.commit("Cover leap seconds in the parse tests", map[string]string{"parse_test.go": "package parse\n" + lines("// test", 40)})

	f.git("checkout", "-q", "-b", "feature")
	h.feature = f.commit("Support week units in Parse", map[string]string{"week.go": lines("// week", 20), "week_test.go": lines("// week test", 15)})
	f.git("checkout", "-q", "main")
	f.day++
	f.git("merge", "-q", "--no-ff", "-m", "Merge branch 'feature'", "feature")
	h.merge = f.git("rev-parse", "HEAD")

	h.rust = f.commit("Add g to the Rust library with its unit test", map[string]string{
		"lib.rs":      "pub fn f() {}\npub fn g() -> u8 { 1 }\n\n#[cfg(test)]\nmod tests {\n    #[test]\n    fn g_is_one() { assert_eq!(super::g(), 1); }\n}\n",
		"tests/it.rs": "#[test]\nfn it() {}\n"})
	h.revert = f.commit("Revert \"parse: handle negative durations\"\n\nThis reverts commit "+h.small+".", map[string]string{
		"parse.go": "package parse\n" + lines("// parse", 30), "parse_test.go": "package parse\n" + lines("// test", 39)})
	sweep := map[string]string{"sweep_test.go": "// renamed\n"}
	for i := range 6 {
		sweep[fmt.Sprintf("pkg%d/x.go", i)] = "// renamed\n"
	}
	h.sweep = f.commit("Rename the receiver in every package", sweep)
	h.generated = f.commit("Regenerate the kind strings for the new units", map[string]string{
		"kind_string.go": "// Code generated by \"stringer -type=Kind\"; DO NOT EDIT.\n\npackage parse\n" + lines("// kind", 20),
		"kind_test.go":   lines("// kind test", 10)})
	h.style = f.commit("style: align the parser's tables", map[string]string{
		"parse.go": "package parse\n" + lines("//  parse", 30), "parse_test.go": "package parse\n" + lines("//  test", 39)})
	h.fixup = f.commit("fixup! Add Parse for durations", map[string]string{"load.go": lines("// load", 22), "load_test.go": lines("// load test", 22)})
	h.noSource = f.commit("ci: run the tests on every push\n\nThe workflow runs go test and keeps the verdicts.", map[string]string{
		".github/workflows/ci.yml": "on: push\n", "verdicts.jsonl": "{}\n", "load_test.go": lines("// load test", 23)})
	h.python = f.commit("Add the pilot script for duration samples\n\nThe script draws samples and prints a summary per unit.", map[string]string{
		"tools/pilot.py": lines("# pilot", 40), "tools/test_pilot.py": lines("# test", 30)})
	h.review = f.commit("fix(load): address review of the loader\n\nThe loader closes its file and reports the path in errors.", map[string]string{
		"load.go": lines("// load", 40), "load_test.go": lines("// load test", 40)})
	h.deleted = f.commit("Fold the helper into Parse; keep the logo\n\nThe helper had one caller, so Parse does its work inline now.", map[string]string{
		"a b.go": "", "café.go": "package parse\n" + lines("// café", 30), "logo.png": "\x89PNG\x00\x01\x02",
		"helper_test.go": "package parse\n" + lines("// helper test", 25)})
	h.wip = f.commit("wip", map[string]string{"week.go": lines("// week", 22), "week_test.go": lines("// week test", 17)})
	return f, h
}

func hashes(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Hash)
	}
	return out
}

func find(t *testing.T, cs []Candidate, hash string) Candidate {
	t.Helper()
	for _, c := range cs {
		if c.Hash == hash {
			return c
		}
	}
	t.Fatalf("%s is not a candidate", hash)
	return Candidate{}
}

func TestScanCandidatesAndReasons(t *testing.T) {
	f, h := buildHistory(t)
	res, err := Scan(context.Background(), f.root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != "main" || res.Head != h.wip || res.Shallow || res.Scanned != 21 {
		t.Fatalf("ref %q, head %s, shallow %v, scanned %d; want main, %s, false, 21", res.Ref, res.Head, res.Shallow, res.Scanned, h.wip)
	}
	if want := []string{h.python, h.good, h.deleted, h.review, h.feature, h.small, h.wip}; !slices.Equal(hashes(res.Candidates), want) {
		t.Fatalf("candidates %v,\nwant %v (python, good, deleted, review, feature, small, wip)\n%+v", hashes(res.Candidates), want, res.Candidates)
	}

	best := find(t, res.Candidates, h.good)
	if best.Parent != h.root || best.Subject != "Add Parse for durations" || best.Body != "Parse reads 1h30m-style strings and returns a time.Duration.\nCloses #12." ||
		!slices.Equal(best.Tests, []string{"parse_test.go"}) || !slices.Equal(best.Code, []string{"parse.go"}) ||
		best.Lines != 50 || best.TestLines != 20 || best.Date.Day() != 2 {
		t.Fatalf("best candidate %+v", best)
	}
	if got := best.Instruction(); got != "Add Parse for durations\n\nParse reads 1h30m-style strings and returns a time.Duration.\nCloses #12." {
		t.Fatalf("instruction %q", got)
	}
	if best.Score != 10 || !slices.Equal(texts(best.Reasons), []string{
		"+3 the message has a subject and a body", "+2 references an issue", "+3 a moderate size (50 lines)",
		"+2 tests in proportion to code (20 test lines, 30 code lines)", "+0 in the oldest third of the scanned history",
	}) {
		t.Fatalf("best score %d, reasons %q", best.Score, texts(best.Reasons))
	}

	// A deleted path with a space, a non-ASCII path and a binary file: the deleted file is skipped by the generated
	// check, the binary one counts as a code file with no lines.
	del := find(t, res.Candidates, h.deleted)
	if !slices.Equal(del.Code, []string{"a b.go", "café.go", "logo.png"}) || del.Lines != 3+31+26 || !slices.Equal(del.Tests, []string{"helper_test.go"}) {
		t.Fatalf("deleted-file candidate %+v", del)
	}
	if small := find(t, res.Candidates, h.small); !slices.Contains(texts(small.Reasons), "+1 a small change (10 lines)") ||
		!slices.Contains(texts(small.Reasons), "+0 the message is a subject only") {
		t.Fatalf("small reasons %q", texts(small.Reasons))
	}
	review := find(t, res.Candidates, h.review)
	for _, want := range []string{"-3 a review follow-up", "+1 a fix commit", "+2 among the newest third of the scanned history"} {
		if !slices.Contains(texts(review.Reasons), want) {
			t.Errorf("review reasons %q lack %q", texts(review.Reasons), want)
		}
	}
	wip := res.Candidates[len(res.Candidates)-1]
	if wip.Score >= 0 || !slices.Contains(texts(wip.Reasons), "-3 a very short message") ||
		!slices.Contains(texts(wip.Reasons), "-4 work in progress or a typo fix") {
		t.Fatalf("wip score %d, reasons %q", wip.Score, texts(wip.Reasons))
	}

	rejected := map[string]Rejection{}
	for _, r := range res.Rejected {
		rejected[r.Hash] = r
		if r.Subject == "" || r.Date.IsZero() {
			t.Errorf("rejection without subject or date: %+v", r)
		}
	}
	want := map[string]struct {
		reason Reason
		detail string
	}{
		h.root:        {ReasonRoot, "the first commit has no base to start from"},
		h.large:       {ReasonTooLarge, "750 changed lines (limit 600)"},
		h.docs:        {ReasonDocsOnly, "1 documents"},
		h.depsFiles:   {ReasonDependencies, "only manifests or lockfiles change besides tests: go.mod, go.sum"},
		h.depsMessage: {ReasonDependencies, "the subject reads as a dependency update and go.mod changes"},
		h.testsOnly:   {ReasonTestsOnly, "1 test files, nothing to implement"},
		h.merge:       {ReasonMerge, "2 parents"},
		h.rust:        {ReasonInlineRust, "lib.rs"},
		h.revert:      {ReasonRevert, ""},
		h.sweep:       {ReasonFormatting, "6 code files with 6 changed lines in all"},
		h.generated:   {ReasonGenerated, "kind_string.go"},
		h.style:       {ReasonFormatting, "the subject names a formatting change"},
		h.fixup:       {ReasonFixup, "to be squashed into another commit"},
		h.noSource:    {ReasonNoSource, "only .github/workflows/ci.yml, verdicts.jsonl"},
	}
	if len(rejected) != len(want) {
		t.Errorf("rejected %d commits, want %d: %v", len(rejected), len(want), res.Rejected)
	}
	for hash, w := range want {
		if r := rejected[hash]; r.Reason != w.reason || r.Detail != w.detail {
			t.Errorf("commit %s rejected as %q (%q), want %q (%q)", hash[:7], r.Reason, r.Detail, w.reason, w.detail)
		}
	}
	if c := res.Counts(); c[ReasonDependencies] != 2 || c[ReasonFormatting] != 2 || c[ReasonMerge] != 1 {
		t.Errorf("counts %v", c)
	}
	for reason := range res.Counts() {
		if !slices.Contains(ReasonOrder(), reason) {
			t.Errorf("ReasonOrder lacks %q", reason)
		}
	}
}

func TestScanOptions(t *testing.T) {
	f, h := buildHistory(t)
	ctx := context.Background()

	res, err := Scan(ctx, f.root, Options{Exclude: map[string]bool{h.good: true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidates[1].Hash != h.deleted || slices.Contains(hashes(res.Candidates), h.good) || res.Counts()[ReasonImported] != 1 {
		t.Fatalf("an excluded commit is still a candidate: %v, counts %v", hashes(res.Candidates), res.Counts())
	}

	res, err = Scan(ctx, f.root, Options{MaxLines: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(hashes(res.Candidates), h.large) {
		t.Fatal("MaxLines 1000 does not admit the 750-line commit")
	}
	// Every candidate has two or more test and code files, so a limit of one rejects them all as too large.
	res, err = Scan(ctx, f.root, Options{MaxFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 || res.Counts()[ReasonTooLarge] != 10 {
		t.Fatalf("MaxFiles 1: %d candidates, counts %v", len(res.Candidates), res.Counts())
	}

	res, err = Scan(ctx, f.root, Options{MaxCommits: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 3 || !slices.Equal(hashes(res.Candidates), []string{h.deleted, h.review, h.wip}) {
		t.Fatalf("MaxCommits 3: scanned %d, candidates %v", res.Scanned, hashes(res.Candidates))
	}

	res, err = Scan(ctx, f.root, Options{Since: time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 3 {
		t.Fatalf("Since: scanned %d, want 3", res.Scanned)
	}

	// Only Go tests run: the Python commit is set aside, with the languages in the detail.
	res, err = Scan(ctx, f.root, Options{RunsTest: func(p string) bool { return strings.HasSuffix(p, "_test.go") }, TestCommand: "go test"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(hashes(res.Candidates), h.python) {
		t.Fatal("python tests were accepted for go test")
	}
	for _, r := range res.Rejected {
		if r.Hash == h.python && (r.Reason != ReasonTestLanguage || r.Detail != "python tests; the verify command is go test") {
			t.Fatalf("python commit rejected as %q (%q)", r.Reason, r.Detail)
		}
	}
	if res.Counts()[ReasonTestLanguage] != 2 { // the Rust commit's tests/it.rs is not a Go test either
		t.Fatalf("counts %v", res.Counts())
	}

	res, err = Scan(ctx, f.root, Options{Ref: "feature"})
	if err != nil || res.Ref != "feature" || res.Scanned != 9 {
		t.Fatalf("Ref feature: %v, ref %q, scanned %d", err, res.Ref, res.Scanned)
	}
	if _, err := Scan(ctx, f.root, Options{Ref: "no-such-branch"}); err == nil {
		t.Fatal("an unknown ref was accepted")
	}
}

func TestScanDefaultBranchAndTags(t *testing.T) {
	f, h := buildHistory(t)
	ctx := context.Background()
	clone := filepath.Join(t.TempDir(), "clone")
	gitAt(t, f.root, 25, "clone", "-q", f.root, clone)

	res, err := Scan(ctx, clone, Options{})
	if err != nil || res.Ref != "main" || res.Head != h.wip {
		t.Fatalf("clone: %v, ref %q, head %s", err, res.Ref, res.Head)
	}

	// origin/main moves ahead: it contains the local main, so it is scanned.
	ahead := f.commit("Add Format for durations\n\nFormat writes the shortest string Parse reads back.", map[string]string{
		"format.go": lines("// format", 20), "format_test.go": lines("// format test", 20)})
	gitAt(t, clone, 25, "fetch", "-q")
	if res, err = Scan(ctx, clone, Options{}); err != nil || res.Ref != "origin/main" || res.Head != ahead {
		t.Fatalf("behind origin: %v, ref %q, head %s", err, res.Ref, res.Head)
	}

	// The local main diverges: it wins.
	gitAt(t, clone, 30, "commit", "-q", "--allow-empty", "-m", "Local work")
	head := gitAt(t, clone, 25, "rev-parse", "HEAD")
	if res, err = Scan(ctx, clone, Options{}); err != nil || res.Ref != "main" || res.Head != head {
		t.Fatalf("diverged: %v, ref %q, head %s", err, res.Ref, res.Head)
	}

	// A tag named main must not shadow the branch, given or by default.
	gitAt(t, clone, 25, "tag", "main", h.root)
	for _, ref := range []string{"", "main"} {
		if res, err = Scan(ctx, clone, Options{Ref: ref}); err != nil || res.Head != head || res.Scanned < 20 {
			t.Fatalf("tag main, ref %q: %v, head %s, scanned %d", ref, err, res.Head, res.Scanned)
		}
	}
}

func TestScanShallowClone(t *testing.T) {
	f, h := buildHistory(t)
	clone := filepath.Join(t.TempDir(), "shallow")
	gitAt(t, f.root, 25, "clone", "-q", "--depth", "3", "file://"+f.root, clone)
	res, err := Scan(context.Background(), clone, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Shallow || res.Scanned != 3 {
		t.Fatalf("shallow %v, scanned %d", res.Shallow, res.Scanned)
	}
	if c := res.Counts(); c[ReasonShallow] != 1 || c[ReasonRoot] != 0 {
		t.Fatalf("counts %v", c)
	}
	if !slices.Equal(hashes(res.Candidates), []string{h.deleted, h.wip}) { // the review commit is the boundary
		t.Fatalf("candidates %v", hashes(res.Candidates))
	}
}

func TestScanCancelled(t *testing.T) {
	f, _ := buildHistory(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, f.root, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled scan returned %v", err)
	}
}

func TestParseLogSeparatorsInMessages(t *testing.T) {
	hash := func(c byte) string { return strings.Repeat(string(c), 40) }
	rec := func(h, subject, body string) string {
		return recordSep + h + fieldSep + hash('0') + fieldSep + "1767268800" + fieldSep + subject + fieldSep + body + fieldSep +
			"\x00\n1\t2\ta b.go\x00-\t-\tlogo.png\x00"
	}
	out := rec(hash('a'), "Plain", "body") + rec(hash('b'), "Odd \x1f subject", "") + rec(hash('c'), "Odd body", "a \x1e stray") +
		rec(hash('d'), "Last", "")
	commits := parseLog([]byte(out))
	if len(commits) != 4 {
		t.Fatalf("%d commits: %+v", len(commits), commits)
	}
	if c := commits[0]; c.unreadable != "" || c.hash != hash('a') || len(c.files) != 2 || c.files[0] != (fileStat{path: "a b.go", lines: 3}) ||
		c.files[1] != (fileStat{path: "logo.png", binary: true}) {
		t.Fatalf("plain record %+v", c)
	}
	for _, c := range commits[1:3] {
		if c.unreadable == "" {
			t.Errorf("record %s with a separator in its message was parsed: %+v", c.hash[:1], c)
		}
		if _, rej := classify(c, withDefaults(Options{}), false); rej == nil || rej.Reason != ReasonUnreadable {
			t.Errorf("record %s: rejection %+v", c.hash[:1], rej)
		}
	}
	if commits[3].unreadable != "" || commits[3].subject != "Last" {
		t.Fatalf("the record after the odd ones %+v", commits[3])
	}
}

func TestStripTrailers(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		"Body text.":                        "Body text.",
		"Body.\n\nCo-Authored-By: A <a@b>":  "Body.",
		"Co-Authored-By: A <a@b>":           "",
		"Body.\n\nNote: this stays":         "Body.\n\nNote: this stays",
		"Body.\n\nNote: this stays\nreally": "Body.\n\nNote: this stays\nreally",
		"One.\n\nTwo.\n\nSigned-off-by: X\nCo-Authored-By: Y\n  continued": "One.\n\nTwo.",
		"Body.\n\nFixes: #3\nCo-Authored-By: Y":                            "Body.\n\nFixes: #3\nCo-Authored-By: Y",
	} {
		if got := stripTrailers(in); got != want {
			t.Errorf("stripTrailers(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIssueRef(t *testing.T) {
	for text, want := range map[string]bool{
		"Fix the parser (#12)":                           true,
		"Closes #7":                                      true,
		"#42 at the start":                               true,
		"See https://github.com/o/r/issues/9":            true,
		"From https://gitlab.com/o/r/-/merge_requests/3": true,
		"Fixes ABC-123":                                  true,
		"refs: PROJ-9":                                   true,
		"Supports UTF-8 and SHA-256":                     false,
		"Color #fff and issue#3":                         false,
		"Version 1.2#3":                                  false,
	} {
		if got := issueRef.MatchString(text); got != want {
			t.Errorf("issueRef(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestIsGenerated(t *testing.T) {
	for content, want := range map[string]bool{
		"// Code generated by protoc-gen-go. DO NOT EDIT.\npackage x\n":     true,
		"# Code generated by tool. DO NOT EDIT.\n":                          true,
		"package x\n\n// Code generated by x. DO NOT EDIT.\n":               true,
		"package x\n// A \"Code generated ... DO NOT EDIT\" header\n":       false,
		strings.Repeat("\n", 12) + "// Code generated by x. DO NOT EDIT.\n": false,
	} {
		if got := isGenerated([]byte(content)); got != want {
			t.Errorf("isGenerated(%q) = %v, want %v", content, got, want)
		}
	}
}

func texts(parts []Part) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p.String())
	}
	return out
}
