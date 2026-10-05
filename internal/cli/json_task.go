package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// The --json documents of the task commands.

// taskInfo is a task as list, add, import, edit and the batches show it.
type taskInfo struct {
	Name           string  `json:"name"`
	Source         string  `json:"source"`
	BaseCommit     string  `json:"base_commit"`
	SolutionCommit *string `json:"solution_commit"`  // null for a task without a solution
	GradedBy       string  `json:"graded_by"`        // tests | judge
	Module         string  `json:"module,omitempty"` // the monorepo folder the task runs in; absent for the root
	HiddenTests    int     `json:"hidden_test_files"`
	Reference      int     `json:"reference_files"`
	// Status is the stored validation's: valid, invalid, flaky, unchecked, or unvalidated; StatusSummary says why in words.
	Status        string `json:"status"`
	StatusSummary string `json:"status_summary"`
	NeedsReview   bool   `json:"needs_review"` // the instruction awaits a person's review for solution leaks
	UntestedHunks int    `json:"untested_hunks"`
	// UnstatedRequirements counts what the hidden tests need that nothing states; null when it could not be checked.
	UnstatedRequirements *int `json:"unstated_requirements"`
}

func taskInfoOf(ctx context.Context, fair *task.Fairness, t store.Task) taskInfo {
	info := taskInfo{Name: t.Name, Source: t.Source, BaseCommit: t.BaseCommit, Module: t.Module, GradedBy: grading(t),
		HiddenTests: len(t.HiddenTests), Reference: len(t.Reference), Status: task.StatusOf(t), StatusSummary: validationStatus(t),
		NeedsReview: t.NeedsReview, UntestedHunks: untestedCount(t)}
	if t.SolutionCommit != "" {
		info.SolutionCommit = &t.SolutionCommit
	}
	if gaps, err := task.Gaps(ctx, fair, t); err == nil {
		n := len(gaps)
		info.UnstatedRequirements = &n
	}
	return info
}

// taskListInfo is a task as task list shows it: taskInfo and what the task's graded runs tell. Only task list has
// these keys; the other task documents share taskInfo and do not change.
type taskListInfo struct {
	taskInfo
	GradedRuns int `json:"graded_runs"` // the task's fair runs with a recorded grade, of every version together
	PassedRuns int `json:"passed_runs"` // how many of them passed
	// Tells is the first that fits: retired, invalid, flaky, unchecked, not-validated, unstated-requirements, not-reviewed
	// (these block the task from an experiment), not-run, never-passed (0 of 2 or more), few-runs (under 4 runs),
	// too-easy (all passed), separates.
	Tells string `json:"tells"`
}

type taskListDoc struct {
	header
	Tasks []taskListInfo `json:"tasks"`
}

type taskSavedDoc struct {
	header
	Task                 taskInfo `json:"task"`
	Setup                []string `json:"setup"`
	Verify               []string `json:"verify"`
	SolutionLeakSections []string `json:"solution_leak_sections"` // instruction headings that may give the solution away (unreviewed tasks)
	NextCommand          string   `json:"next_command"`
}

type taskEditDoc struct {
	header
	Task    taskInfo `json:"task"`
	Updated bool     `json:"updated"`
}

type taskRemovedDoc struct {
	header
	Removed string `json:"removed"`
}

type weakTestsDoc struct {
	Checked  int      `json:"checked"`
	Skipped  int      `json:"skipped"`
	Untested []string `json:"untested"`
	TimedOut int      `json:"timed_out"`
	Reason   string   `json:"reason"` // why nothing was checked
}

func weakTestsOf(w *task.WeakTests) *weakTestsDoc {
	if w == nil {
		return nil
	}
	doc := &weakTestsDoc{Checked: w.Checked, Skipped: w.Skipped, TimedOut: w.TimedOut, Reason: w.Reason, Untested: []string{}}
	for _, h := range w.Untested {
		doc.Untested = append(doc.Untested, h.String())
	}
	return doc
}

type taskShowDoc struct {
	header
	taskInfo
	Instruction string        `json:"instruction"`
	Review      string        `json:"review"` // draft | ticket | history: why the instruction needs a review; empty when it does not
	Setup       []string      `json:"setup"`
	Verify      []string      `json:"verify"`
	HiddenTests []string      `json:"hidden_tests"`
	Reference   []string      `json:"reference"`
	WeakTests   *weakTestsDoc `json:"weak_tests"`
	Gaps        []gapDoc      `json:"unstated_requirement_details"`
	// InstructionNamesFiles lists reference files the instruction names: it tells the agent where the fix goes.
	InstructionNamesFiles []string `json:"instruction_names_reference_files"`
	// Warnings are the stored validation's (task.Validation.Warnings): what the verify commands keep from grading.
	Warnings []string `json:"warnings"`
	// Draft is the text `task draft` stored beside the instruction, which it does not replace (`task edit
	// --accept-draft` does); DraftWrittenAt and DraftModel say when and by which model. All three are null with no draft.
	Draft          *string `json:"draft"`
	DraftWrittenAt *string `json:"draft_written_at"` // RFC 3339, UTC
	DraftModel     *string `json:"draft_model"`
	// DraftingSpendUSD is what every draft call on the task cost, stored or refused (0 when none was made).
	DraftingSpendUSD float64 `json:"drafting_spend_usd"`
}

func taskShowDocument(ctx context.Context, env Env, w *workspace, t store.Task) taskShowDoc {
	doc := taskShowDoc{header: env.hdr(), taskInfo: taskInfoOf(ctx, task.NewFairness("--git-dir", w.bare), t), Instruction: t.Instruction,
		Setup: list(t.Setup), Verify: list(t.Verify), HiddenTests: list(t.HiddenTests), Reference: list(t.Reference), Gaps: []gapDoc{},
		InstructionNamesFiles: []string{}, Warnings: []string{}, DraftingSpendUSD: t.DraftSpendUSD}
	if t.Draft != "" {
		at := t.DraftAt.UTC().Format(time.RFC3339)
		doc.Draft, doc.DraftWrittenAt, doc.DraftModel = &t.Draft, &at, &t.DraftModel
	}
	switch {
	case t.NeedsReview && t.FromDraft:
		doc.Review = "draft"
	case t.NeedsReview && isTicket(t):
		doc.Review = "ticket"
	case t.NeedsReview:
		doc.Review = "history"
	}
	var stored task.Validation
	if t.Validation != nil && json.Unmarshal(t.Validation, &stored) == nil {
		doc.WeakTests = weakTestsOf(stored.WeakTests)
		doc.Warnings = list(env.redactAll(stored.Warnings))
	}
	if gaps, err := task.Gaps(ctx, task.NewFairness("--git-dir", w.bare), t); err == nil {
		doc.Gaps = gapDocs(gaps)
	}
	for _, p := range t.Reference {
		if strings.Contains(t.Instruction, p) || strings.Contains(t.Instruction, filepath.Base(p)) {
			doc.InstructionNamesFiles = append(doc.InstructionNamesFiles, p)
		}
	}
	return doc
}

// taskDraftDoc is `task draft --json`'s document: how the one paid call ended, and what it cost.
type taskDraftDoc struct {
	header
	Task string `json:"task"`
	// Outcome is stored (the draft passed both checks and is stored beside the instruction), refused (refused_by says
	// why: consent, when --yes was missing and nothing ran; fairness or giveaway, when a check refused the draft and
	// nothing was stored) or failed (the call brought no draft: reason says why). Only stored exits 0.
	Outcome   string   `json:"outcome"`
	RefusedBy []string `json:"refused_by"`
	Reason    string   `json:"reason"` // for people; empty when stored
	// Draft is the text the call brought, stored or refused; null when it brought none (or nothing ran).
	Draft     *string  `json:"draft"`
	Gaps      []gapDoc `json:"unstated_requirement_details"` // what the fairness check found unstated in the draft
	Giveaways []string `json:"giveaway_names"`               // names in the draft only the reference has
	Model     string   `json:"model"`
	// MaxCostUSD is the most the call may cost: its cap and the overshoot a call can pass it by.
	MaxCostUSD float64 `json:"max_cost_usd"`
	// CostUSD is what this call reported it cost, counted before anything else; null when it reported none or nothing ran.
	CostUSD *float64 `json:"cost_usd"`
	// DraftingSpendUSD is what every draft call on the task has cost, this one included.
	DraftingSpendUSD float64  `json:"drafting_spend_usd"`
	Notes            []string `json:"notes"` // for people: draft calls a stopped Agentium left, cut prompts
	NextCommand      string   `json:"next_command"`
}

// gapDoc is task.Gap as the public schema has it: something a hidden test needs that nothing states.
type gapDoc struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	File string `json:"file"` // the hidden test file that needs it
}

func gapDocs(gaps []task.Gap) []gapDoc {
	docs := []gapDoc{}
	for _, g := range gaps {
		docs = append(docs, gapDoc{Kind: g.Kind, Text: g.Text, File: g.File})
	}
	return docs
}

// validationExit is task validate's exit code for a result: a task that cannot be trusted is a failure.
func validationExit(status string) int {
	if status == task.StatusInvalid || status == task.StatusFlaky {
		return ExitError
	}
	return ExitOK
}

type judgeCheckDoc struct {
	InstructionWords       int      `json:"instruction_words"`
	CodeFiles              []string `json:"code_files"`
	ChangedLines           int      `json:"changed_lines"`
	ReferenceDiffTruncated bool     `json:"reference_diff_truncated"` // the judge reads a cut copy
	// InstructionTruncated: the instruction is longer than the grading judge reads (judge.MaxInstructionChars).
	InstructionTruncated bool `json:"instruction_truncated,omitempty"`
}

type validatedDoc struct {
	header
	Task           string              `json:"task"`
	Status         string              `json:"status"`
	Summary        string              `json:"summary"`
	Arms           []string            `json:"arms"`
	Repeats        int                 `json:"repeats"`
	NeedsReview    bool                `json:"needs_review"`
	Gaps           []gapDoc            `json:"unstated_requirement_details"`
	HarnessChanged map[string][]string `json:"harness_changed"` // per arm: settings, hooks or MCP the arm changes
	WeakTests      *weakTestsDoc       `json:"weak_tests"`
	Judge          *judgeCheckDoc      `json:"judge"` // judge-graded tasks only
	// Warnings are what the verify commands keep from grading (task.Validation.Warnings); they never change the status.
	Warnings []string `json:"warnings"`
	// Grader is where the stages' verification ran: "host" or the sandbox's version ("sandbox-v1"). A judge-graded
	// task runs nothing; its validation names no mode, which reads as "host" (task.GraderOf).
	Grader string `json:"grader"`
}

func validatedDocument(ctx context.Context, env Env, w *workspace, t store.Task, o task.ValidateOptions, result task.Validation) validatedDoc {
	doc := validatedDoc{header: env.hdr(), Task: t.Name, Status: result.Status, Summary: result.Summary(), Arms: []string{}, Repeats: result.RepeatCount(),
		NeedsReview: t.NeedsReview, Gaps: []gapDoc{}, HarnessChanged: map[string][]string{}, WeakTests: weakTestsOf(result.WeakTests),
		Warnings: list(env.redactAll(result.Warnings)), Grader: task.GraderOf(result.Grader)}
	for _, a := range o.Arms {
		doc.Arms = append(doc.Arms, a.Name)
	}
	for arm, files := range result.HarnessChanged {
		doc.HarnessChanged[arm] = list(files)
	}
	if gaps, err := task.Gaps(ctx, task.NewFairness("--git-dir", w.bare), t); err == nil {
		doc.Gaps = gapDocs(gaps)
	}
	return doc
}

// batchRowDoc is a row of pool update's and task validate --all's table: a task, or a commit that did not become one.
type batchRowDoc struct {
	Name    string    `json:"name"`
	Commit  string    `json:"commit"`
	Problem string    `json:"problem"` // why it was not imported or validated
	Task    *taskInfo `json:"task"`    // null when the commit did not become a task
}

func batchRowDocs(ctx context.Context, env Env, w *workspace, rows []batchRow) []batchRowDoc {
	ctx = context.WithoutCancel(ctx)
	fair := task.NewFairness("--git-dir", w.bare)
	docs := []batchRowDoc{}
	for _, r := range rows {
		if r.task == nil {
			docs = append(docs, batchRowDoc{Name: r.name, Commit: r.commit, Problem: env.redact(r.problem)})
			continue
		}
		info := taskInfoOf(ctx, fair, *r.task)
		docs = append(docs, batchRowDoc{Name: r.task.Name, Commit: r.task.SolutionCommit, Problem: env.redact(r.problem), Task: &info})
	}
	return docs
}

type validateAllDoc struct {
	header
	Rows        []batchRowDoc `json:"tasks"`
	Valid       int           `json:"valid"`
	Total       int           `json:"total"`
	Interrupted bool          `json:"interrupted"`
}

// mineCandidateDoc is a mining candidate in pool update's document.
type mineCandidateDoc struct {
	Commit  string   `json:"commit"`
	Score   int      `json:"score"`
	Subject string   `json:"subject"`
	Tests   int      `json:"test_files"`
	Code    int      `json:"code_files"`
	Lines   int      `json:"changed_lines"`
	Reasons []string `json:"score_parts"`
}
