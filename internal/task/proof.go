package task

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// The proof that the hidden tests ran (issue #160). A verification passes when its commands exit 0, and code the agent
// writes runs inside them: a TestMain or an init that exits 0 before any test runs passes every task. So, for a task
// whose hidden tests include Go test files, a pass also needs the proof: once the verification has passed, Agentium
// runs `go test -json -count=1 -run '^(NAMES)$' ./DIR` itself, once per package folder, in the same place and mode
// (the module's folder of the graded copy; the host or the grading sandbox, with the same environment and time
// limit), and must see a pass event for each of the task's own hidden tests (PlanGoProof). A skipped test is not a
// pass. What it stops is tests that never ran, not forged output: code that prints a test's framing lines can still
// fake a pass (a known limit).
//
// Grades and validations decide a pass by one rule (PassRuleOf). Experiments locked before the proof keep the rule
// they ran under, the exit codes alone, so their verdicts do not change.

// Pass rules: how a grade decides a pass. Validations and experiment locks record theirs; an empty rule, in what was
// recorded before rules existed, is PassExitCode.
const (
	// PassExitCode: the verification commands exited 0.
	PassExitCode = "exit-code"
	// PassGoTests: the verification commands exited 0 and, for hidden Go tests, the proof saw each of the task's own
	// pass. Tasks without hidden Go tests are graded as by PassExitCode.
	PassGoTests = "go-tests-v1"
)

// PassRuleOf is a recorded rule as it counts: empty is PassExitCode.
func PassRuleOf(rule string) string {
	if rule == "" {
		return PassExitCode
	}
	return rule
}

// KnownPassRule reports whether this Agentium grades by rule (as PassRuleOf reads it).
func KnownPassRule(rule string) bool {
	r := PassRuleOf(rule)
	return r == PassExitCode || r == PassGoTests
}

// DescribePassRule says what a pass is under rule, for people.
func DescribePassRule(rule string) string {
	switch r := PassRuleOf(rule); r {
	case PassExitCode:
		return "the verification's exit codes"
	case PassGoTests:
		return "the verification's exit codes, and a pass of each hidden Go test in Agentium's own go test -json run (" + r + ")"
	default:
		return "an unknown rule (" + r + ")"
	}
}

// NoteHiddenTestsNotRun is the note of a grade whose verification passed without the proof.
const NoteHiddenTestsNotRun = "the hidden tests did not run"

// ProofEvents is the file, in a run's records folder, that holds the proof's `go test -json` events.
const ProofEvents = "proof.jsonl"

// HasGoTestFiles reports whether hidden tests include Go test files (*_test.go): whether the proof applies to the task.
func HasGoTestFiles(hiddenTests []string) bool {
	return slices.ContainsFunc(hiddenTests, func(p string) bool { return strings.HasSuffix(p, "_test.go") })
}

// GoProof is what the proof runs: the proof tests, by package folder, in folder order.
type GoProof struct {
	Packages []ProofPackage
}

// ProofPackage is one package folder's proof tests: Dir is a slash path from the repository's root ("." for the root),
// Tests the top-level test names, sorted.
type ProofPackage struct {
	Dir   string
	Tests []string
}

// Empty reports whether there is nothing to prove.
func (p GoProof) Empty() bool { return len(p.Packages) == 0 }

// Tests counts the proof tests.
func (p GoProof) Tests() int {
	n := 0
	for _, pkg := range p.Packages {
		n += len(pkg.Tests)
	}
	return n
}

// PlanGoProof lists the proof tests of a task: its own Go tests (the top-level Test, Fuzz and Example functions, not
// TestMain, that the solution adds or changes in its hidden _test.go files), by package folder. When the hidden Go files
// hold test functions but none is new or changed (a changed table in a variable, say), the proof tests are all of them.
// Files the go tool never builds as tests (under testdata, or a file or folder whose name starts with "_" or ".") are
// left out. goFiles reports whether the hidden tests include Go test files at all: with goFiles and an empty proof
// there is nothing to prove, and the exit codes alone grade the task (validation warns).
func PlanGoProof(hiddenTests []string, base, solution source.Source) (proof GoProof, goFiles bool) {
	var files []string
	for _, p := range hiddenTests {
		if builtGoTestFile(p) {
			files = append(files, p)
		}
	}
	tests, _ := hiddenGoTests(files, base, solution)
	own := slices.ContainsFunc(tests, func(t goTest) bool { return t.own })
	byDir := map[string][]string{}
	for _, t := range tests {
		if !own || t.own {
			byDir[t.dir] = append(byDir[t.dir], t.name)
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		names := byDir[dir]
		slices.Sort(names)
		proof.Packages = append(proof.Packages, ProofPackage{Dir: dir, Tests: slices.Compact(names)})
	}
	return proof, HasGoTestFiles(hiddenTests)
}

// builtGoTestFile reports whether the go tool builds p as a test file of its package: a _test.go file that is not in
// testdata, and whose name and folders do not start with "_" or ".".
func builtGoTestFile(p string) bool {
	if !strings.HasSuffix(p, "_test.go") {
		return false
	}
	for _, el := range strings.Split(path.Clean(p), "/") {
		if el == "testdata" || strings.HasPrefix(el, "_") || strings.HasPrefix(el, ".") {
			return false
		}
	}
	return true
}

// Target is the package's folder as `go test` names it from the module's folder (module, a slash path from the root; ""
// for the root): "." or "./pkg", or "../pkg" for a folder outside the module's.
func (pkg ProofPackage) Target(module string) string {
	rel := path.Clean(pkg.Dir)
	if module != "" {
		if r, err := filepath.Rel(filepath.FromSlash(path.Clean(module)), filepath.FromSlash(rel)); err == nil {
			rel = filepath.ToSlash(r)
		}
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return rel
	}
	return "./" + rel
}

// Command is the shell command that runs the package's proof tests from the module's folder: `go test -json -count=1
// -run '^(NAMES)$' ./DIR`. -count=1 makes a cached result impossible; the names are Go identifiers, quoted all the same.
func (pkg ProofPackage) Command(module string) string {
	names := make([]string, len(pkg.Tests))
	for i, n := range pkg.Tests {
		names[i] = regexp.QuoteMeta(n)
	}
	return "go test -json -count=1 -run " + shellQuote("^("+strings.Join(names, "|")+")$") + " " + shellQuote(pkg.Target(module))
}

// shellQuote quotes s for a POSIX shell, unless it holds only characters no shell treats specially.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestProof is what the proof saw: how many proof tests it looked for, its commands' results (it stops at the first
// package folder not proven), and the tests it saw no pass for.
type TestProof struct {
	Tests    int           `json:"tests"`
	Commands []Command     `json:"commands"`
	Missing  []MissingTest `json:"missing,omitempty"`
}

// MissingTest is a proof test the proof saw no pass for: its package (as `go test` was given it), its name, and what
// the events showed (SawFail, SawSkip or SawNone).
type MissingTest struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	Saw     string `json:"saw"`
}

// What the proof saw of a test that did not pass.
const (
	SawFail = "fail"
	SawSkip = "skip"
	SawNone = "none" // no result: it never ran, the run stopped first, or its package's command was never run
)

// Proven reports whether the proof saw every proof test pass. A nil proof (none was run) proves nothing.
func (p *TestProof) Proven() bool {
	return p != nil && len(p.Commands) > 0 && len(p.Missing) == 0
}

// summary is the proof's line in the verification's log: what was proven, or which tests were not.
func (p *TestProof) summary(events string) string {
	if p.Proven() {
		return fmt.Sprintf("the hidden tests ran: go test -json showed each of the task's %d hidden test(s) pass (its events: %s)", p.Tests, events)
	}
	var missing []string
	for _, m := range p.Missing {
		what := map[string]string{SawFail: "failed", SawSkip: "skipped", SawNone: "not run"}[m.Saw]
		missing = append(missing, fmt.Sprintf("%s in %s (%s)", m.Test, m.Package, what))
	}
	return fmt.Sprintf("%s: go test -json showed no pass for %s (its events: %s)", NoteHiddenTestsNotRun, strings.Join(missing, ", "), events)
}

// Proving is a proof for a grade to run once its verification has passed: the proof tests, the module's folder its
// commands are named from (a slash path; "" for the root), and the file its events go to. Run sets Result.
type Proving struct {
	Proof  GoProof
	Module string
	Events string
	Result *TestProof
}

// ProofCommand runs one of the proof's shell commands in the grade's module folder and mode, its standard output to
// events (a file, so a process the command leaves cannot hold the run open) and its errors to the verification's log.
// It returns the command's result; an error is Agentium's own (or cancellation), never the tests'.
type ProofCommand func(ctx context.Context, command string, events *os.File) (Command, error)

// Run runs the proof, one package folder at a time through run, until a folder is not proven, and reports whether it
// saw every proof test pass. Each command's line goes to log before it runs, and one line after them says what was
// proven or which tests were not. An empty proof proves nothing and runs nothing (Result stays nil): the caller asks
// only when there is something to prove.
func (p *Proving) Run(ctx context.Context, log io.Writer, run ProofCommand) (bool, error) {
	if p.Proof.Empty() {
		return false, errors.New("the proof has no tests to look for")
	}
	result := &TestProof{Tests: p.Proof.Tests()}
	p.Result = result
	events, err := os.OpenFile(p.Events, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false, fmt.Errorf("the proof's events: %w", err)
	}
	defer events.Close()
	stopped := false
	for _, pkg := range p.Proof.Packages {
		target := pkg.Target(p.Module)
		if stopped { // not run: a folder before it was not proven
			for _, name := range pkg.Tests {
				result.Missing = append(result.Missing, MissingTest{Package: target, Test: name, Saw: SawNone})
			}
			continue
		}
		command := pkg.Command(p.Module)
		fmt.Fprintf(log, "$ %s\n", command)
		info, err := events.Stat()
		if err != nil {
			return false, fmt.Errorf("the proof's events: %w", err)
		}
		c, err := run(ctx, command, events)
		result.Commands = append(result.Commands, c)
		if err != nil {
			return false, err
		}
		if c.TimedOut {
			fmt.Fprintln(log, "[agentium] the proof timed out")
		}
		saw, err := readProofEvents(p.Events, info.Size(), pkg.Tests)
		if err != nil {
			return false, err
		}
		for _, name := range pkg.Tests {
			if saw[name] != "pass" {
				result.Missing = append(result.Missing, MissingTest{Package: target, Test: name, Saw: cmp.Or(saw[name], SawNone)})
				stopped = true
			}
		}
	}
	fmt.Fprintf(log, "[agentium] %s\n", result.summary(filepath.Base(p.Events)))
	return result.Proven(), nil
}

// readProofEvents reads the events file from offset on (one command's events) for the tests in want (testResults).
func readProofEvents(file string, offset int64, want []string) (map[string]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("the proof's events: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("the proof's events: %w", err)
	}
	saw, err := testResults(f, want)
	if err != nil {
		return nil, fmt.Errorf("the proof's events: %w", err)
	}
	return saw, nil
}

// maxEventLine bounds a `go test -json` line testResults reads; longer lines (a test's huge output) are skipped.
const maxEventLine = 1 << 20

// testResults reads `go test -json` events from r and says what each top-level test in want showed: "pass" when a pass
// event names it and no fail or skip event does (so a name two packages share passes only when it passes in both),
// SawFail or SawSkip when such an event names it (a failure wins), and "" when no result names it. Events of subtests
// (TestX/case), of packages (no test), lines that are not events and lines over maxEventLine are skipped. A cached
// result replays the same events; the proof's -count=1 rules caching out.
func testResults(r io.Reader, want []string) (map[string]string, error) {
	wanted := map[string]bool{}
	for _, n := range want {
		wanted[n] = true
	}
	passed, bad := map[string]bool{}, map[string]string{}
	handle := func(line []byte) {
		if !bytes.Contains(line, []byte(`"Test"`)) {
			return
		}
		var e struct{ Action, Test string }
		if json.Unmarshal(line, &e) != nil || !wanted[e.Test] {
			return
		}
		switch e.Action {
		case "pass":
			passed[e.Test] = true
		case "fail":
			bad[e.Test] = SawFail
		case "skip":
			if bad[e.Test] == "" {
				bad[e.Test] = SawSkip
			}
		}
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) <= maxEventLine {
			line = append(line, chunk...)
		} else {
			long = true
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if !long {
			handle(line)
		}
		line, long = line[:0], false
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for n := range wanted {
		switch {
		case bad[n] != "":
			out[n] = bad[n]
		case passed[n]:
			out[n] = "pass"
		}
	}
	return out, nil
}
