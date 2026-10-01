package mine

import (
	"context"
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

// newFixture creates the repository. HOME points at a temporary folder holding a git configuration that would break a
// scan reading the user's settings (colour, renames, quoted paths), so the scan must fix them itself.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	config := "[user]\n\tname = Fixture\n\temail = fixture@example.com\n[color]\n\tui = always\n[diff]\n\trenames = copies\n[log]\n\tdecorate = full\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: t.TempDir()}
	f.git("init", "-q", "-b", "main")
	return f
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	date := fmt.Sprintf("2026-01-%02dT12:00:00Z", f.day)
	cmd := exec.Command("git", args...)
	cmd.Dir = f.root
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files (path to content) and commits them with message on the next day, returning the hash.
func (f *fixture) commit(message string, files map[string]string) string {
	f.t.Helper()
	f.day++
	for p, content := range files {
		full := filepath.Join(f.root, filepath.FromSlash(p))
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
	root, good, small, large, docs, depsFiles, depsMessage, testsOnly, feature, merge, rust, revert, sweep, generated, wip string
}

func buildHistory(t *testing.T) (*fixture, history) {
	f := newFixture(t)
	var h history
	h.root = f.commit("Initial commit", map[string]string{
		"parse.go": "package parse\n", "parse_test.go": "package parse\n", "README.md": "# parse\n",
		"go.mod": "module example.com/parse\n", "go.sum": "", "lib.rs": "pub fn f() {}\n",
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
	h.depsMessage = f.commit("Bump example.com/yaml to v3 and adapt the loader", map[string]string{
		"load.go": lines("// load", 20), "load_test.go": lines("// load test", 20)})
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
	h.wip = f.commit("wip", map[string]string{"parse.go": "package parse\n" + lines("// parse", 32), "parse_test.go": "package parse\n" + lines("// test", 41)})
	return f, h
}

var fixtureNow = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func TestScanCandidatesAndReasons(t *testing.T) {
	f, h := buildHistory(t)
	res, err := Scan(context.Background(), f.root, Options{Now: fixtureNow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != "main" || res.Scanned != 15 {
		t.Fatalf("ref %q, scanned %d; want main, 15", res.Ref, res.Scanned)
	}
	var order []string
	for _, c := range res.Candidates {
		order = append(order, c.Hash)
	}
	if want := []string{h.good, h.feature, h.small, h.wip}; !slices.Equal(order, want) {
		t.Fatalf("candidates %v, want %v (good, feature, small, wip)\n%+v", order, want, res.Candidates)
	}

	best := res.Candidates[0]
	if best.Parent != h.root || best.Subject != "Add Parse for durations" || !strings.HasSuffix(best.Body, "Closes #12.") ||
		strings.Contains(best.Body, "Co-Authored-By") || !slices.Equal(best.Tests, []string{"parse_test.go"}) ||
		!slices.Equal(best.Code, []string{"parse.go"}) || best.Lines != 50 || best.TestLines != 20 || best.Date.Day() != 2 {
		t.Fatalf("best candidate %+v", best)
	}
	if best.Score != 12 || !slices.Equal(texts(best.Reasons), []string{
		"+3 the message has a subject and a body", "+2 references an issue", "+3 a moderate size (50 lines)",
		"+2 tests in proportion to code (20 test lines, 30 code lines)", "+2 recent (57 days)",
	}) {
		t.Fatalf("best score %d, reasons %q", best.Score, texts(best.Reasons))
	}
	wip := res.Candidates[3]
	if wip.Score >= 0 || !slices.Contains(texts(wip.Reasons), "-3 a very short message") ||
		!slices.Contains(texts(wip.Reasons), "-4 work in progress or a typo fix") {
		t.Fatalf("wip score %d, reasons %q", wip.Score, texts(wip.Reasons))
	}

	rejected := map[string]Reason{}
	for _, r := range res.Rejected {
		rejected[r.Hash] = r.Reason
		if r.Subject == "" || r.Date.IsZero() {
			t.Errorf("rejection without subject or date: %+v", r)
		}
	}
	want := map[string]Reason{
		h.root: ReasonRoot, h.large: ReasonTooLarge, h.docs: ReasonDocsOnly, h.depsFiles: ReasonDependencies,
		h.depsMessage: ReasonDependencies, h.testsOnly: ReasonTestsOnly, h.merge: ReasonMerge, h.rust: ReasonInlineRust,
		h.revert: ReasonRevert, h.sweep: ReasonFormatting, h.generated: ReasonGenerated,
	}
	if len(rejected) != len(want) {
		t.Errorf("rejected %d commits, want %d: %v", len(rejected), len(want), res.Rejected)
	}
	for hash, reason := range want {
		if rejected[hash] != reason {
			t.Errorf("commit %s rejected as %q, want %q", hash[:7], rejected[hash], reason)
		}
	}
	for _, r := range res.Rejected {
		switch r.Reason {
		case ReasonInlineRust:
			if r.Detail != "lib.rs" {
				t.Errorf("inline Rust detail %q", r.Detail)
			}
		case ReasonGenerated:
			if r.Detail != "kind_string.go" {
				t.Errorf("generated detail %q", r.Detail)
			}
		case ReasonTooLarge:
			if r.Detail != "750 changed lines (limit 600)" {
				t.Errorf("too large detail %q", r.Detail)
			}
		}
	}
	if c := res.Counts(); c[ReasonDependencies] != 2 || c[ReasonMerge] != 1 {
		t.Errorf("counts %v", c)
	}
}

func TestScanExcludeAndLimits(t *testing.T) {
	f, h := buildHistory(t)
	ctx := context.Background()

	res, err := Scan(ctx, f.root, Options{Now: fixtureNow, Exclude: map[string]bool{h.good: true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidates[0].Hash != h.feature || slices.ContainsFunc(res.Candidates, func(c Candidate) bool { return c.Hash == h.good }) {
		t.Fatalf("an excluded commit is still a candidate: %+v", res.Candidates)
	}
	if res.Counts()[ReasonImported] != 1 {
		t.Fatalf("counts %v", res.Counts())
	}

	// Raising the line limit admits the large commit; lowering the file limit rejects none here (each has 2 files).
	res, err = Scan(ctx, f.root, Options{Now: fixtureNow, MaxLines: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Candidates, func(c Candidate) bool { return c.Hash == h.large }) {
		t.Fatal("MaxLines 1000 does not admit the 750-line commit")
	}
	res, err = Scan(ctx, f.root, Options{Now: fixtureNow, MaxFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 || res.Counts()[ReasonTooLarge] != 7 {
		t.Fatalf("MaxFiles 1: %d candidates, counts %v", len(res.Candidates), res.Counts())
	}

	res, err = Scan(ctx, f.root, Options{Now: fixtureNow, MaxCommits: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 3 || len(res.Candidates) != 1 || res.Candidates[0].Hash != h.wip {
		t.Fatalf("MaxCommits 3: scanned %d, candidates %+v", res.Scanned, res.Candidates)
	}

	// Since keeps the commits from day 13 (the sweep) on.
	res, err = Scan(ctx, f.root, Options{Now: fixtureNow, Since: time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 3 {
		t.Fatalf("Since: scanned %d, want 3", res.Scanned)
	}

	res, err = Scan(ctx, f.root, Options{Now: fixtureNow, Ref: "feature"})
	if err != nil || res.Ref != "feature" || res.Scanned != 9 {
		t.Fatalf("Ref feature: %v, ref %q, scanned %d", err, res.Ref, res.Scanned)
	}
	if _, err := Scan(ctx, f.root, Options{Ref: "no-such-branch"}); err == nil {
		t.Fatal("an unknown ref was accepted")
	}
}

func TestScanCancelled(t *testing.T) {
	f, _ := buildHistory(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, f.root, Options{}); err == nil {
		t.Fatal("a cancelled scan succeeded")
	}
}

func TestStripTrailers(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		"Body text.":                        "Body text.",
		"Body.\n\nCo-Authored-By: A <a@b>":  "Body.",
		"Co-Authored-By: A <a@b>":           "",
		"Body.\n\nNote: this stays\nreally": "Body.\n\nNote: this stays\nreally",
		"One.\n\nTwo.\n\nSigned-off-by: X\nCo-Authored-By: Y": "One.\n\nTwo.",
	} {
		if got := stripTrailers(in); got != want {
			t.Errorf("stripTrailers(%q) = %q, want %q", in, got, want)
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
