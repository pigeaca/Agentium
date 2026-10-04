package mine

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/pool"
)

// moduleHistory is a monorepo history with a Go module in svc/billing and Python code in tools/report: commits inside the
// module, beside it, across it, and one whose tests sit outside it.
type moduleHistory struct {
	inside, docsBeside, otherModule, testsOutside, codeOutside, depsOutside, large string
	// sibling's test is in svc/billing2, a folder whose name starts with the module's; siblingOnly changes only that folder.
	sibling, siblingOnly string
}

func buildModuleHistory(t *testing.T) (*fixture, moduleHistory) {
	t.Helper()
	f := newFixture(t)
	f.commit("Start the monorepo", map[string]string{"README.md": "monorepo\n", "svc/billing/go.mod": "module example.com/billing\n",
		"svc/billing/pay.go": lines("// pay", 5), "svc/billing/pay_test.go": lines("// t", 5),
		"tools/report/pyproject.toml": "[project]\nname = \"report\"\n", "tools/report/report.py": lines("# r", 5),
		"tools/report/test_report.py": lines("# t", 5), "shared/util.go": lines("// u", 5), "shared/util_test.go": lines("// t", 5)})
	var h moduleHistory
	h.inside = f.commit("Round payments to cents in billing\n\nAmounts are rounded half to even before they are stored.",
		map[string]string{"svc/billing/pay.go": lines("// pay", 30), "svc/billing/pay_test.go": lines("// t", 25)})
	h.docsBeside = f.commit("Refund partial payments in billing\n\nA refund may now be smaller than the payment it returns.",
		map[string]string{"svc/billing/pay.go": lines("// pay", 50), "svc/billing/pay_test.go": lines("// t", 40), "docs/billing.md": "refunds\n"})
	h.otherModule = f.commit("Sort the report by date\n\nThe report lists the newest entries first.",
		map[string]string{"tools/report/report.py": lines("# r", 30), "tools/report/test_report.py": lines("# t", 20)})
	// The hostile case: the module's code changes, but its tests are elsewhere. The module's verify commands would never run
	// them, so a task of it would have hidden tests that grade nothing.
	h.testsOutside = f.commit("Charge a fee on late payments in billing\n\nA late payment pays a fixed fee.",
		map[string]string{"svc/billing/pay.go": lines("// pay", 70), "shared/util_test.go": lines("// t", 30)})
	h.codeOutside = f.commit("Share the rounding helper with billing\n\nThe helper moves to shared and billing calls it.",
		map[string]string{"svc/billing/pay.go": lines("// pay", 90), "svc/billing/pay_test.go": lines("// t", 60), "shared/util.go": lines("// u", 20)})
	h.depsOutside = f.commit("Bump the shared requirement for billing\n\nThe module needs a newer helper.",
		map[string]string{"svc/billing/pay.go": lines("// pay", 110), "svc/billing/pay_test.go": lines("// t", 80), "tools/report/pyproject.toml": "[project]\nname = \"report2\"\n"})
	// Large outside the module, small inside: but nothing may change outside, so the size rule never sees it.
	files := map[string]string{"svc/billing/pay.go": lines("// pay", 120), "svc/billing/pay_test.go": lines("// t", 90)}
	for i := range 20 {
		files[filepath.ToSlash(filepath.Join("svc/billing/gen", "f"+string(rune('a'+i))+".go"))] = lines("// g", 40)
	}
	h.sibling = f.commit("Bill in two currencies\n\nInvoices may now be in euros or dollars.",
		map[string]string{"svc/billing/pay.go": lines("// pay", 130), "svc/billing2/x_test.go": lines("// t", 10)})
	h.siblingOnly = f.commit("Start the second billing service\n\nA copy of billing for another market.",
		map[string]string{"svc/billing2/x.go": lines("// x", 10), "svc/billing2/x_test.go": lines("// t", 12)})
	h.large = f.commit("Add the invoice generator to billing\n\nInvoices are generated per customer.", files)
	return f, h
}

func rejectedFor(res Result, hash string) (Rejection, bool) {
	i := slices.IndexFunc(res.Rejected, func(r Rejection) bool { return r.Hash == hash })
	if i < 0 {
		return Rejection{}, false
	}
	return res.Rejected[i], true
}

// In a module, a candidate changes the module and nothing else but documents: commits of another module are outside
// it, and commits whose tests, code or build files reach beyond it are set aside, the hostile one (tests outside the
// module) among them. The size limits count the module's files. Without a module the same history mines as before.
func TestScanInAModule(t *testing.T) {
	f, h := buildModuleHistory(t)
	ctx := context.Background()
	res, err := Scan(ctx, f.root, Options{Module: "svc/billing"})
	if err != nil {
		t.Fatal(err)
	}
	if got := hashes(res.Candidates); !slices.Equal(sorted(got), sorted([]string{h.inside, h.docsBeside})) {
		t.Fatalf("candidates %v, want %s and %s", got, h.inside, h.docsBeside)
	}
	for _, c := range []struct {
		hash   string
		reason Reason
		detail string
	}{
		{h.otherModule, ReasonNotInModule, "changes nothing in svc/billing"},
		{h.testsOutside, ReasonOutsideModule, "1 test, code or build file(s) outside svc/billing, such as shared/util_test.go"},
		{h.codeOutside, ReasonOutsideModule, "such as shared/util.go"},
		{h.depsOutside, ReasonOutsideModule, "such as tools/report/pyproject.toml"},
		{h.large, ReasonTooLarge, "22 files"},
		{h.sibling, ReasonOutsideModule, "such as svc/billing2/x_test.go"},
		{h.siblingOnly, ReasonNotInModule, "changes nothing in svc/billing"},
	} {
		rej, ok := rejectedFor(res, c.hash)
		if !ok || rej.Reason != c.reason || !strings.Contains(rej.Detail, c.detail) {
			t.Errorf("%s: rejected %v as %q (%s), want %q (%s)", c.hash[:7], ok, rej.Reason, rej.Detail, c.reason, c.detail)
		}
	}
	docs := find(t, res.Candidates, h.docsBeside)
	if !slices.Equal(docs.Docs, []string{"docs/billing.md"}) || !slices.Equal(docs.Tests, []string{"svc/billing/pay_test.go"}) {
		t.Errorf("the candidate with a document beside the module: %+v", docs)
	}

	// The root: every commit with tests and code is a candidate, as before modules.
	root, err := Scan(ctx, f.root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{h.inside, h.otherModule, h.testsOutside, h.codeOutside} {
		if !slices.Contains(hashes(root.Candidates), hash) {
			t.Errorf("at the root %s is no candidate: %+v", hash[:7], root.Rejected)
		}
	}
	if n := root.Counts()[ReasonNotInModule] + root.Counts()[ReasonOutsideModule]; n != 0 {
		t.Errorf("the root set %d commit(s) aside for a module", n)
	}
}

// The pool's range scan scopes to the module as Scan does.
func TestScanRangeInAModule(t *testing.T) {
	f, h := buildModuleHistory(t)
	head := f.git("rev-parse", "HEAD")
	res, err := ScanRange(context.Background(), RangeInput{Root: f.root, Bare: filepath.Join(f.root, ".git"),
		Range: pool.ScanRange{Head: head, Since: time.Time{}}, Options: Options{Module: "svc/billing"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := hashes(res.Scanned.Candidates); !slices.Equal(sorted(got), sorted([]string{h.inside, h.docsBeside})) {
		t.Fatalf("candidates %v", got)
	}
	if rej, _ := rejectedFor(res.Result, h.testsOutside); rej.Reason != ReasonOutsideModule {
		t.Errorf("tests outside the module: %+v", rej)
	}
}

// The tests mining picks commits by are detected in the module's folder: the module's Go tests there, not the root's
// (which has no build file).
func TestTestLanguagesOfAModule(t *testing.T) {
	f, _ := buildModuleHistory(t)
	langs, commands := TestLanguages(filepath.Join(f.root, "svc", "billing"))
	if !slices.Equal(langs, []string{"go"}) || !slices.Equal(commands, []string{"go test ./..."}) {
		t.Errorf("the module's languages %v, commands %v", langs, commands)
	}
	if langs, _ := TestLanguages(f.root); len(langs) != 0 {
		t.Errorf("the root has no build file, yet languages %v", langs)
	}
}

// A Python module's lock is looked for in the module's folder: a module with a uv.lock is locked, and the root's
// missing lock does not matter.
func TestBaseLockInAModule(t *testing.T) {
	f := newFixture(t)
	f.commit("Start", map[string]string{"py/pyproject.toml": "[project]\nname = \"p\"\n", "py/uv.lock": "version = 1\n",
		"loose/pyproject.toml": "[project]\nname = \"l\"\n", "loose/requirements.txt": "attrs>=23\n",
		"pinned/pyproject.toml": "[project]\nname = \"q\"\n", "pinned/requirements.txt": "-r requirements/test.txt\n",
		"pinned/requirements/test.txt": "pytest==8.0.0\n"})
	head := f.git("rev-parse", "HEAD")
	for _, c := range []struct {
		module         string
		python, locked bool
	}{{"py", true, true}, {"loose", true, false}, {"pinned", true, true}, {"", false, false}} {
		python, locked, err := baseLock(context.Background(), f.root, head, c.module)
		if err != nil || python != c.python || locked != c.locked {
			t.Errorf("baseLock(%q) = %v, %v, %v; want %v, %v", c.module, python, locked, err, c.python, c.locked)
		}
	}
}

func sorted(s []string) []string { return slices.Sorted(slices.Values(s)) }
