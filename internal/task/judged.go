package task

import (
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claudectx"
)

// Grading modes: how a task's runs are graded.
const (
	// GradingTests: the hidden tests and verification commands decide a run (every task before judge grading).
	GradingTests = "tests"
	// GradingJudge: the solution changes no test files, so there are no hidden tests; the LLM judge compares a run's
	// change with the reference solution, and its majority of judge.GradeRepeats grades the run (unvalidated: reports
	// keep these grades apart from the tests').
	GradingJudge = "judge"
)

// JudgeCheck is the validation of a judge-graded task. Nothing runs: it checks that the judge has a reference to
// compare with and an instruction to judge against. A task with Problems is invalid.
type JudgeCheck struct {
	CodeFiles    []string `json:"code_files"`    // the reference files the judge reads (not tests, not documents)
	ChangedLines int      `json:"changed_lines"` // added and removed lines in their diff
	Problems     []string `json:"problems,omitempty"`
}

// GradingOf is a task's grading mode as a run specification carries it (Spec.Grading): GradingJudge, or empty for the
// tests, so specifications and lock digests of test-graded tasks stay as they were before judge grading.
func GradingOf(mode string) string {
	if mode == GradingJudge {
		return GradingJudge
	}
	return ""
}

// JudgedFiles lists the reference files the judge compares: those that are neither tests nor documents.
func JudgedFiles(reference []string) []string {
	var code []string
	for _, p := range reference {
		if !IsTestFile(p) && !claudectx.IsDocument(p) {
			code = append(code, p)
		}
	}
	return code
}

// ValidateJudged checks a judge-graded task: the instruction must say something, and the reference must change code.
// diff is the reference's code diff (judge.ReferenceDiff), "" when JudgedFiles is empty.
func ValidateJudged(instruction string, reference []string, diff string, now time.Time) Validation {
	check := &JudgeCheck{CodeFiles: JudgedFiles(reference), ChangedLines: changedLines(diff)}
	if strings.TrimSpace(instruction) == "" {
		check.Problems = append(check.Problems, "the instruction is empty: state what to do (agentium task edit NAME --instruction @FILE)")
	}
	switch {
	case len(check.CodeFiles) == 0:
		check.Problems = append(check.Problems, "the reference changes no code (only tests or documents): the judge has nothing to compare a run with")
	case check.ChangedLines == 0:
		check.Problems = append(check.Problems, "the reference's code diff changes no lines: the judge has nothing to compare a run with")
	}
	status := StatusValid
	if len(check.Problems) > 0 {
		status = StatusInvalid
	}
	return Validation{Status: status, Arms: []Arm{}, Stages: []Stage{}, At: now.UTC(), Judge: check}
}

// changedLines counts a unified diff's added and removed lines inside its hunks (file headers are not changes).
func changedLines(diff string) int {
	n, inHunk := 0, false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff "):
			inHunk = false
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case inHunk && (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")):
			n++
		}
	}
	return n
}
