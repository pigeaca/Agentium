// Package report turns an experiment's lock and runs into its report: verdicts in plain words, the metrics with both
// intervals, a per-task table, behavior counts per arm, the context overhead and what the runs used of it, costs, and
// honesty notes. It renders Markdown (for a pull request) and JSON (with the lock and every run). Both are meant to be
// shared: Claude Code's and personal skill and command names, personal subagent types, local paths and what the agents
// said are left out, and credential-shaped text is redacted. The project's own context files, skills, commands and
// subagents are named, as its repository names them, and so are Claude Code's own subagent types.
package report

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/task"
)

// Run is one stored run of the experiment.
type Run struct {
	ID      string
	Slot    int
	Attempt int
	Record  run.Record
}

// Input is what a report is made from. Runs come in the order they were stored (started_at, id), which keeps the
// bootstrap's draws, and so the report, the same every time.
type Input struct {
	Name       string
	Lock       experiment.Lock
	Status     string
	StatusNote string
	Runs       []Run
	// DataDir and Home are replaced in any text the report shows, so it names no local paths.
	DataDir string
	Home    string
	// Checks are the project's rule checks, and CheckResults each counted run's results for them by run ID, in the
	// order of Checks (Load reads them from the stored runs; Build reads no files). Without Checks the report has none.
	Checks       []run.Check
	CheckResults map[string][]run.CheckResult

	paths []pathPattern // Build resolves DataDir and Home once (see scrub); empty, scrub resolves them per call
}

// Report is an experiment's results, ready to render.
type Report struct {
	Experiment string  `json:"experiment"`
	Template   string  `json:"template"`
	Status     string  `json:"status"`
	StatusNote string  `json:"status_note,omitempty"`
	Slots      int     `json:"slots"`
	Settled    int     `json:"settled"`
	SpentUSD   float64 `json:"spent_usd"` // everything the budget counts, the calibrations' included (Load adds them)
	// CalibrationUSD is the part of SpentUSD that went to the calibration runs the experiment made for its arms; Load
	// sets it (Build does not: calibration runs are not slots).
	CalibrationUSD float64             `json:"calibration_usd,omitempty"`
	Lock           experiment.Lock     `json:"lock"` // skill and command names counted; Claude Code's file name only
	Analysis       experiment.Analysis `json:"analysis"`
	Arms           []Arm               `json:"arms"`
	Tasks          []TaskRow           `json:"tasks"`
	// Summary is a model-ab experiment's verdicts in one sentence, naming the arm by profile.
	Summary string `json:"summary,omitempty"`
	Judge   *Judge `json:"judge,omitempty"` // only with the judge
	// PairJudge is the pair judge's preference, only with the pair judge (--judge-pairs): unvalidated and exploratory.
	PairJudge *PairJudge `json:"pair_judge,omitempty"`
	// JudgeGrading is how the judge graded the judge-graded tasks, only when there are some: unvalidated, apart from the
	// tests (experiment.MetricJudgeSuccess).
	JudgeGrading *JudgeGrading `json:"judge_grading,omitempty"`
	// NorthStar is the project's time and spend to its first decisive verdict; Load sets it (Build does not: it needs the
	// project's other experiments).
	NorthStar *NorthStar `json:"north_star,omitempty"`
	// Checks counts the project's rule checks per arm (agentium check add): omitted when the report was made without
	// them (Build), `[]` when the project has none (Load). Counts, never a verdict.
	Checks []CheckRow `json:"checks,omitzero"`
	Notes  []string   `json:"notes"`
	Runs   []RunRow   `json:"runs"`
}

// Arm summarizes one arm's counted runs. Means are nil when there is nothing to average.
type Arm struct {
	Name     string `json:"name"`
	Context  string `json:"context"`
	Snapshot string `json:"snapshot,omitempty"`
	// Profile is the arm's model and effort ("MODEL" or "MODEL:EFFORT"), in a model-ab experiment only.
	Profile string `json:"profile,omitempty"`
	Counted int    `json:"counted"`
	// Capped counts the counted runs Claude Code stopped at their cost cap or turn limit, and TimedOut those Agentium
	// stopped at the timeout: each one's cost is a lower bound of what it would have spent (Censored adds them up).
	Capped   int `json:"capped"`
	TimedOut int `json:"timed_out"`
	// FirstRequest is the mean measured size of the first request: the context overhead Claude Code saw.
	FirstRequest *float64 `json:"first_request_tokens"`
	CostUSD      *float64 `json:"cost_usd"`      // mean reported cost
	ColdCostUSD  *float64 `json:"cold_cost_usd"` // mean with every cached read repriced as a one-hour cache write
	// IsolatedCostUSD is the mean cost had no other run warmed the prompt cache (run.Record.IsolatedCostUSD); nil unless
	// every counted run has one. Verdicts use CostUSD.
	IsolatedCostUSD *float64 `json:"isolated_cost_usd"`
	CacheReadShare  *float64 `json:"cache_read_share"` // of all input tokens
	Behavior        Behavior `json:"behavior"`
	// ContextUse counts what the counted runs used of their context.
	ContextUse ArmContextUse `json:"context_use"`
}

// ArmContextUse counts what an arm's counted runs used of their context (see run.ContextUse). Recorded is how many of
// them have it: runs recorded before Agentium kept it, whose transcripts are gone, do not. Each map counts runs.
type ArmContextUse struct {
	Recorded  int            `json:"recorded"`
	Start     []string       `json:"start"` // loaded at start, in any of the runs
	Files     map[string]int `json:"files,omitempty"`
	Skills    map[string]int `json:"skills,omitempty"`
	Subagents map[string]int `json:"subagents,omitempty"`
	// OtherSubagents counts runs that started subagents of other types, which are not named.
	OtherSubagents int `json:"other_subagents,omitempty"`
}

// Behavior counts what the agents did, over an arm's counted runs. (Runs that read outside their checkout are unfair,
// so they are not among them: the notes list the drift.)
type Behavior struct {
	TestsChanged  int `json:"tests_changed"`  // runs that changed a test file
	TestsRemoved  int `json:"tests_removed"`  // test files removed, in total
	RanTests      int `json:"ran_tests"`      // runs that ran a test runner
	RanChecks     int `json:"ran_checks"`     // runs that ran one of the task's verification commands
	Committed     int `json:"committed"`      // runs that committed
	ChecksChanged int `json:"checks_changed"` // runs that changed verification scripts or runner configuration
	// ConfigPasses are runs that passed with runner configuration changed beyond the task's reference: failures here.
	ConfigPasses int      `json:"config_passes"`
	Denials      int      `json:"denials"`       // in total
	FilesChanged *float64 `json:"files_changed"` // mean
	LinesChanged *float64 `json:"lines_changed"` // mean, added and removed
	BashCommands *float64 `json:"bash_commands"` // mean
}

// TaskRow is one task's runs per arm.
type TaskRow struct {
	Task string              `json:"task"`
	Arms map[string]TaskCell `json:"arms"`
	// Judged: the task is judge-graded, so its marks and successes are the judge's grades (unvalidated).
	Judged bool `json:"judged,omitempty"`
}

// TaskCell is a task's runs in one arm: marks in schedule order (● success, ○ failure, × not counted), and the mean cost
// of the counted runs (nil without any). Capped and TimedOut count the counted runs stopped at their cap or the
// timeout: with any, CostUSD is a lower bound.
type TaskCell struct {
	Profile   string   `json:"profile,omitempty"` // the arm's model and effort, in a model-ab experiment only
	Marks     string   `json:"marks"`
	Successes int      `json:"successes"`
	Counted   int      `json:"counted"`
	Capped    int      `json:"capped"`
	TimedOut  int      `json:"timed_out"`
	CostUSD   *float64 `json:"cost_usd"`
}

// RunRow is one run's data, as shared: no local paths, and not what the agent said.
type RunRow struct {
	ID             string          `json:"id"`
	Slot           int             `json:"slot"`
	Attempt        int             `json:"attempt"`
	Task           string          `json:"task"`
	Arm            string          `json:"arm"`
	Outcome        string          `json:"outcome"`
	Passed         *bool           `json:"passed,omitempty"`
	Success        bool            `json:"success"`
	Metrics        agent.Metrics   `json:"metrics"` // without the result excerpt
	Behavior       run.Behavior    `json:"behavior"`
	CostEstimated  bool            `json:"cost_estimated,omitempty"`
	Recovered      string          `json:"recovered,omitempty"`
	HarnessChanged []string        `json:"harness_changed,omitempty"`
	Drift          []string        `json:"drift,omitempty"`
	Notes          []string        `json:"notes,omitempty"`
	ContextCommit  string          `json:"context_commit,omitempty"`
	ContextUse     *run.ContextUse `json:"context_use,omitempty"`
	Judge          *judge.Verdict  `json:"judge,omitempty"` // the judge's verdict, its texts scrubbed
	// GradedBy is "judge" for a run of a judge-graded task: Passed and Success are then the judge's grade (Judge holds
	// its verdict), unvalidated. Absent for runs graded by tests.
	GradedBy string        `json:"graded_by,omitempty"`
	Verify   []taskCommand `json:"verify,omitempty"`
	// Grader is the mode the run was graded in (absent in runs recorded before modes: the host), and Sandbox what the
	// grading sandbox reported, as counts: the denials' targets are what the grade chose, so they stay in the records.
	Grader   string      `json:"grader,omitempty"`
	Sandbox  *SandboxRow `json:"sandbox,omitempty"`
	Started  string      `json:"started"`
	Finished string      `json:"finished"`
}

// SandboxRow is a sandboxed grade as shared: whether the canary passed, how many denials the kernel logged (noise left
// out), how many of them the agent's own sandbox does not impose, and their operations.
type SandboxRow struct {
	Canary            string   `json:"canary"` // "passed" or "failed"
	Denials           int      `json:"denials"`
	Flagged           int      `json:"flagged"`
	FlaggedOperations []string `json:"flagged_operations,omitempty"`
	DenialsUnread     bool     `json:"denials_unread,omitempty"`
}

// SandboxOf shares what a record's sandbox reported: counts and operations, never the denials' targets; nil for nil.
func SandboxOf(g *task.SandboxGrade) *SandboxRow { return sandboxRow(g) }

// sandboxRow shares what a record's sandbox reported.
func sandboxRow(g *task.SandboxGrade) *SandboxRow {
	if g == nil {
		return nil
	}
	row := &SandboxRow{Canary: "failed", Denials: g.DenialCount, Flagged: g.FlaggedCount, DenialsUnread: g.Unread != ""}
	if g.Canary == task.CanaryPassed {
		row.Canary = "passed"
	}
	for _, d := range g.Flagged {
		if !slices.Contains(row.FlaggedOperations, d.Operation) {
			row.FlaggedOperations = append(row.FlaggedOperations, d.Operation)
		}
	}
	return row
}

type taskCommand struct {
	Command  string  `json:"command"`
	ExitCode int     `json:"exit_code"`
	Seconds  float64 `json:"seconds"`
}

// Build computes the report.
func Build(in Input) (Report, error) {
	in.paths = in.pathPatterns()
	l := in.Lock
	var data []experiment.RunData
	for _, r := range in.Runs {
		data = append(data, experiment.RunDataOf(r.Slot, r.Record))
	}
	analysis, err := experiment.Analyze(l, data)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Experiment: in.Name, Template: l.Design.Template, Status: in.Status, StatusNote: in.StatusNote, Slots: len(l.Schedule),
		Lock: redactLock(l), Analysis: analysis}
	settled := map[int]bool{}
	for _, r := range in.Runs {
		rep.SpentUSD += r.Record.Spend().TotalUSD() // all the budget counts; the arms' cost is the agent's
		if experiment.Settles(r.Record.Outcome) {
			settled[r.Slot] = true
		}
		rec := r.Record
		metrics := rec.Metrics
		metrics.ResultExcerpt = "" // what the agent said can hold anything
		metrics.SubagentModels = shareableModels(rec)
		row := RunRow{ID: r.ID, Slot: r.Slot, Attempt: r.Attempt, Task: rec.Task, Arm: rec.Arm, Outcome: rec.Outcome, Passed: rec.Passed,
			Success: experiment.Success(rec.Outcome, rec.Passed, rec.Behavior.ConfigChanged), Metrics: metrics, Behavior: rec.Behavior,
			CostEstimated: rec.Spend().AgentEstimated, Recovered: rec.Recovered, HarnessChanged: rec.HarnessChanged, Drift: in.scrubAll(rec.Drift),
			Notes: in.scrubAll(rec.Notes), ContextCommit: rec.ContextHead, ContextUse: rec.ContextUse, Judge: in.shareVerdict(rec.Judge),
			GradedBy: rec.GradedBy, Grader: rec.Grader, Sandbox: sandboxRow(rec.Sandbox), Started: rec.Started.UTC().Format("2006-01-02T15:04:05Z"),
			Finished: rec.Finished.UTC().Format("2006-01-02T15:04:05Z")}
		for _, c := range rec.Verify {
			row.Verify = append(row.Verify, taskCommand{Command: c.Command, ExitCode: c.ExitCode, Seconds: c.Seconds})
		}
		rep.Runs = append(rep.Runs, row)
	}
	rep.Settled = len(settled)
	for _, a := range l.Arms {
		arm := armSummary(a, in.Runs)
		arm.Profile = armProfile(l.Design, a)
		rep.Arms = append(rep.Arms, arm)
	}
	rep.Tasks = taskRows(l, in.Runs)
	rep.Summary = summarize(rep)
	for _, t := range rep.Tasks {
		for _, a := range rep.Arms {
			if cell, ok := t.Arms[a.Name]; ok && a.Profile != "" {
				cell.Profile = a.Profile
				t.Arms[a.Name] = cell
			}
		}
	}
	rep.Checks = checkRows(in, rep.Arms)
	rep.Judge = judgeSummary(in)
	rep.JudgeGrading = judgeGradingSummary(in)
	if rep.PairJudge, err = pairJudgeSummary(in); err != nil {
		return Report{}, err
	}
	rep.Notes = notes(rep, in)
	return rep, nil
}

// pathPattern replaces one spelling of a local folder with its placeholder.
type pathPattern struct {
	whole *regexp.Regexp
	as    string
}

// pathPatterns resolves the data folder and home folder, as written and as resolved (/var and /private/var on macOS),
// into patterns that match them as whole path components. Resolving touches the file system (an automounted /home
// can take a second), so Build does it once per report.
func (in Input) pathPatterns() []pathPattern {
	var out []pathPattern
	for _, p := range []struct{ path, as string }{{in.DataDir, "<agentium data>"}, {in.Home, "~"}} {
		if p.path == "" || p.path == "/" {
			continue
		}
		spellings := []string{filepath.Clean(p.path)}
		if resolved, err := filepath.EvalSymlinks(p.path); err == nil && resolved != spellings[0] {
			spellings = append(spellings, resolved)
		}
		slices.SortFunc(spellings, func(a, b string) int { return len(b) - len(a) }) // /private/var/x before /var/x
		for _, s := range spellings {
			// Whole components only: /Users/a must not turn /Users/alex into ~lex, nor /var/x match in /private/var/x.
			out = append(out, pathPattern{regexp.MustCompile(`(^|[^A-Za-z0-9._/-])` + regexp.QuoteMeta(s) + `([/\s"':;,)\]]|$)`), p.as})
		}
	}
	return out
}

// scrub makes text shareable: the data folder and home folder become placeholders where they are whole path
// components (pathPatterns), and credential-shaped strings are redacted.
func (in Input) scrub(text string) string {
	patterns := in.paths
	if patterns == nil {
		patterns = in.pathPatterns()
	}
	for _, p := range patterns {
		text = p.whole.ReplaceAllString(text, "${1}"+p.as+"${2}")
	}
	return string(run.Redact([]byte(text), ""))
}

func (in Input) scrubAll(texts []string) []string {
	var out []string
	for _, t := range texts {
		out = append(out, in.scrub(t))
	}
	return out
}

// redactLock keeps the lock for sharing: skill and command names, which can be personal, and the tasks' harmless
// sandbox denials (paths and a grade's text) become counts, and Claude
// Code's path (often under the user's home folder) its file name. The tasks' instructions and commands stay: they are
// what was run.
func redactLock(l experiment.Lock) experiment.Lock {
	out := l
	out.ClaudePath = filepath.Base(l.ClaudePath)
	out.Arms = slices.Clone(l.Arms)
	for i := range out.Arms {
		out.Arms[i].Skills = []string{fmt.Sprintf("(%d %s)", len(l.Arms[i].Skills), plural(len(l.Arms[i].Skills), "skill"))}
		out.Arms[i].SlashCommands = []string{fmt.Sprintf("(%d %s)", len(l.Arms[i].SlashCommands), plural(len(l.Arms[i].SlashCommands), "slash command"))}
	}
	// The tasks' harmless sandbox denials name paths (the grader's credential stores, sockets) and text a grade chose:
	// shared as a count per task.
	if len(l.Harmless) > 0 {
		out.Harmless = map[string][]task.DenialKey{}
		for name, keys := range l.Harmless {
			out.Harmless[name] = []task.DenialKey{{Operation: fmt.Sprintf("(%d harmless %s)", len(keys), plural(len(keys), "denial"))}}
		}
	}
	return out
}

func armSummary(a experiment.LockedArm, runs []Run) Arm {
	arm := Arm{Name: a.Name, Context: a.Context, Snapshot: a.Snapshot}
	var first, cost, cold, isolated, files, lines, bash []float64
	var cacheRead, input float64
	for _, r := range runs {
		rec := r.Record
		if rec.Arm != a.Name || !counted(rec) {
			continue
		}
		arm.Counted++
		switch rec.Outcome {
		case agent.OutcomeCapped:
			arm.Capped++
		case agent.OutcomeTimeout:
			arm.TimedOut++
		}
		m, b := rec.Metrics, rec.Behavior
		if m.FirstRequest > 0 {
			first = append(first, float64(m.FirstRequest))
		}
		cost = append(cost, rec.Spend().AgentUSD)
		if c, ok := coldCost(rec); ok {
			cold = append(cold, c)
		}
		if rec.IsolatedCostUSD != nil {
			isolated = append(isolated, *rec.IsolatedCostUSD)
		}
		cacheRead += float64(m.CacheReadTokens)
		input += float64(m.InputTokens + m.CacheReadTokens + m.CacheWriteTokens)
		files, lines, bash = append(files, float64(b.FilesChanged)), append(lines, float64(b.LinesAdded+b.LinesRemoved)), append(bash, float64(b.BashCommands))
		counts := &arm.Behavior
		counts.TestsRemoved += b.TestsRemoved
		counts.Denials += b.Denials
		if b.TestsChanged {
			counts.TestsChanged++
		}
		if b.RanTests {
			counts.RanTests++
		}
		if b.RanChecks {
			counts.RanChecks++
		}
		if b.Commits > 0 {
			counts.Committed++
		}
		if len(b.ChecksChanged) > 0 {
			counts.ChecksChanged++
		}
		if len(b.ConfigChanged) > 0 && rec.Passed != nil && *rec.Passed {
			counts.ConfigPasses++
		}
		if u := rec.ContextUse; u != nil {
			use := &arm.ContextUse
			use.Recorded++
			for _, p := range u.Start {
				if !slices.Contains(use.Start, p) {
					use.Start = append(use.Start, p)
				}
			}
			use.Files, use.Skills, use.Subagents = tally(use.Files, u.Files), tally(use.Skills, u.Skills), tally(use.Subagents, u.Subagents)
			if u.OtherSubagents > 0 {
				use.OtherSubagents++
			}
		}
	}
	slices.Sort(arm.ContextUse.Start)
	if arm.ContextUse.Start == nil {
		arm.ContextUse.Start = []string{}
	}
	arm.FirstRequest, arm.CostUSD = mean(first), mean(cost)
	if len(cold) == len(cost) { // every counted run could be repriced
		arm.ColdCostUSD = mean(cold)
	}
	if len(isolated) == len(cost) { // every counted run has one
		arm.IsolatedCostUSD = mean(isolated)
	}
	arm.Behavior.FilesChanged, arm.Behavior.LinesChanged, arm.Behavior.BashCommands = mean(files), mean(lines), mean(bash)
	if input > 0 {
		share := cacheRead / input
		arm.CacheReadShare = &share
	}
	return arm
}

// shareableModels keeps the models of the subagent types a report may name, which are those the run's context use
// names (the project's and Claude Code's own); every other type, and every type of a run without context use, shares one
// "other" entry. The database keeps them all, for the check that a subagent's model did not change.
func shareableModels(rec run.Record) map[string][]string {
	if rec.Metrics.SubagentModels == nil {
		return nil
	}
	out := map[string][]string{}
	for kind, models := range rec.Metrics.SubagentModels {
		key := "other"
		if rec.ContextUse != nil && slices.Contains(rec.ContextUse.Subagents, kind) {
			key = kind
		}
		for _, m := range models {
			if !slices.Contains(out[key], m) {
				out[key] = append(out[key], m)
			}
		}
		slices.Sort(out[key])
	}
	return out
}

// tally adds one to counts for each name, creating counts when there are names.
func tally(counts map[string]int, names []string) map[string]int {
	for _, n := range names {
		if counts == nil {
			counts = map[string]int{}
		}
		counts[n]++
	}
	return counts
}

// coldCost is a run's cost with every cached read repriced as a one-hour cache write (the cache Claude Code writes):
// what the run would have cost had nothing it read been cached before. ok is false for a model without a list price.
func coldCost(rec run.Record) (float64, bool) {
	m := rec.Metrics
	model := m.Model
	if model == "" {
		model = rec.Model
	}
	rates, ok := pricing.Lookup(model)
	if !ok {
		return 0, false
	}
	return rec.Spend().AgentUSD + float64(m.CacheReadTokens)*(rates.CacheWrite1h-rates.CacheRead)/1e6, true
}

func mean(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	m := stats.Mean(values)
	return &m
}

func taskRows(l experiment.Lock, runs []Run) []TaskRow {
	ordered := bySlot(runs)
	var rows []TaskRow
	for _, t := range l.Tasks {
		row := TaskRow{Task: t.Name, Arms: map[string]TaskCell{}, Judged: t.JudgeGraded()}
		for _, a := range l.Arms {
			var cell TaskCell
			var costs []float64
			for _, r := range ordered {
				rec := r.Record
				if rec.Task != t.Name || rec.Arm != a.Name {
					continue
				}
				switch {
				case !counted(rec):
					cell.Marks += "×"
					continue
				case experiment.Success(rec.Outcome, rec.Passed, rec.Behavior.ConfigChanged):
					cell.Marks += "●"
					cell.Successes++
				default:
					cell.Marks += "○"
				}
				cell.Counted++
				switch rec.Outcome {
				case agent.OutcomeCapped:
					cell.Capped++
				case agent.OutcomeTimeout:
					cell.TimedOut++
				}
				costs = append(costs, rec.Spend().AgentUSD)
			}
			cell.CostUSD = mean(costs)
			row.Arms[a.Name] = cell
		}
		rows = append(rows, row)
	}
	return rows
}

// bySlot is runs in schedule order; a slot's tries keep their stored order.
func bySlot(runs []Run) []Run {
	out := slices.Clone(runs)
	slices.SortStableFunc(out, func(x, y Run) int { return x.Slot - y.Slot })
	return out
}

// notes are the honesty notes: what the verdicts leave out or rest on.
func notes(rep Report, in Input) []string {
	var out []string
	a := rep.Analysis
	if note, ok := pendingNote(rep, in); ok && rep.Status != experiment.StatusDone {
		out = append(out, note)
	} else if rep.Status != experiment.StatusDone {
		status := rep.Status
		if rep.StatusNote != "" {
			status += ": " + in.scrub(rep.StatusNote)
		}
		covers := "and the results cover those"
		if s := rep.Analysis.Sequential; s != nil {
			covers = "and no look has been analysed yet, so the results cover every run so far"
			if l := s.ReportedLook(); l != nil {
				covers = fmt.Sprintf("and the results are look %d's", l.Look)
			}
		}
		out = append(out, fmt.Sprintf("The experiment is not finished (%s): %d of %d runs settled, %s.", status, rep.Settled, rep.Slots, covers))
	}
	out = append(out, seqNotes(rep, in)...)
	if len(a.Excluded) > 0 {
		var parts []string
		known := []string{agent.OutcomeUnfair, agent.OutcomeInfra, run.OutcomeSandboxFlagged, agent.OutcomeCancelled, experiment.OutcomeUngraded,
			experiment.OutcomeGradePending}
		for _, outcome := range known {
			if n := a.Excluded[outcome]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, map[string]string{agent.OutcomeUnfair: "unfair (the environment drifted)",
					agent.OutcomeInfra: plural(n, "infrastructure failure"), agent.OutcomeCancelled: "cancelled",
					run.OutcomeSandboxFlagged: "left out for sandbox denials (not tried again)", experiment.OutcomeUngraded: "judge-graded, left without a grade (not tried again)",
					experiment.OutcomeGradePending: "judge-graded, waiting for the judge's grade"}[outcome]))
			}
		}
		for _, outcome := range slices.Sorted(func(yield func(string) bool) {
			for o := range a.Excluded {
				if !slices.Contains(known, o) && !yield(o) {
					return
				}
			}
		}) {
			parts = append(parts, fmt.Sprintf("%d %s", a.Excluded[outcome], outcome))
		}
		out = append(out, "Runs not counted: "+strings.Join(parts, ", ")+". Their spend is in the total.")
	}
	var drift, harness []string
	estimated, stopped, finished, unpriced, unrepriced, noIsolated := 0, 0, 0, 0, 0, 0
	for _, r := range in.Runs {
		rec := r.Record
		if rec.Outcome == agent.OutcomeUnfair {
			for _, d := range in.scrubAll(rec.Drift) {
				if !slices.Contains(drift, d) {
					drift = append(drift, d)
				}
			}
		}
		for _, h := range rec.HarnessChanged {
			if entry := rec.Arm + ": " + h; !slices.Contains(harness, entry) {
				harness = append(harness, entry)
			}
		}
		if rec.Spend().AgentEstimated {
			estimated++
		}
		switch rec.Recovered {
		case run.RecoveredStopped:
			stopped++
		case run.RecoveredFinished:
			finished++
		}
		if rec.Metrics.UnpricedRequests > 0 {
			unpriced++
		}
		if _, ok := coldCost(rec); !ok && experiment.Fair(rec.Outcome) {
			unrepriced++
		}
		if rec.IsolatedCostUSD == nil && experiment.Fair(rec.Outcome) {
			noIsolated++
		}
	}
	if len(drift) > 0 {
		out = append(out, "Environment drift in unfair runs: "+strings.Join(drift, "; ")+".")
	}
	if note := sandboxNote(rep, in.Runs); note != "" {
		out = append(out, note)
	}
	if note := rep.judgeGradingNote(); note != "" {
		out = append(out, note)
	}
	if note := cappedNote(rep); note != "" {
		out = append(out, note)
	}
	if note := overshootNote(in.Runs); note != "" {
		out = append(out, note)
	}
	if estimated > 0 {
		out = append(out, fmt.Sprintf("%d run(s) ended without Claude Code's cost: it was estimated from their transcripts at list prices.", estimated))
	}
	if unpriced > 0 {
		out = append(out, fmt.Sprintf("%d run(s) made requests on models without a list price: those requests are not in their estimated cost.", unpriced))
	}
	if stopped > 0 {
		out = append(out, fmt.Sprintf("%d run(s) were cut short when Agentium stopped, and recovered with what they spent.", stopped))
	}
	if finished > 0 {
		out = append(out, fmt.Sprintf("%d run(s) finished just before Agentium stopped, and were stored when it restarted.", finished))
	}
	if len(harness) > 0 {
		slices.Sort(harness)
		out = append(out, "An arm changes what runs, not only what the agent reads (hooks, settings or MCP): "+strings.Join(harness, "; ")+".")
	}
	for _, arm := range rep.Arms {
		if arm.Behavior.ConfigPasses > 0 {
			out = append(out, fmt.Sprintf("Arm %s: %d run(s) passed with test-runner configuration changed beyond the task's reference; they count as failures.", arm.Name, arm.Behavior.ConfigPasses))
		}
	}
	if len(a.NotDiscriminating) > 0 {
		out = append(out, fmt.Sprintf("Not discriminating for success (every run passed, or every run failed, in both arms): %s. They stay in for cost.",
			strings.Join(slices.Sorted(slices.Values(a.NotDiscriminating)), ", ")))
	}
	var decided, exploratory []string
	for _, r := range a.Results {
		if r.Role == experiment.RoleSecondary {
			exploratory = append(exploratory, strings.ToLower(title(r.Metric)))
			continue
		}
		decided = append(decided, fmt.Sprintf("%s (%s)", strings.ToLower(title(r.Metric)), r.Role))
		switch {
		case r.Verdict == stats.Exploratory && r.Warning == "" && r.Note == "" && r.FullTasks < r.FloorTasks:
			out = append(out, fmt.Sprintf("%s is exploratory: %d of %d task(s) have %d or more counted %s in both arms, below the floor of %d tasks (method %s).",
				title(r.Metric), r.FullTasks, r.Tasks, r.FloorRepeats, plural(r.FloorRepeats, "run"), r.FloorTasks, rep.Lock.Method))
		case r.Verdict != stats.Exploratory && r.FloorRepeats < experiment.MinRepeats && r.Repeats < experiment.MinRepeats && a.Sequential == nil:
			out = append(out, fmt.Sprintf("%s's verdict rests on tasks with fewer than %d runs per arm, as method %s allows: each task's difference "+
				"carries the run-to-run noise, and %s decides. A seeded simulation of 8–12 tasks × 1 run (σ = 0.19, τ = 0.10–0.25, normal and skewed noise) "+
				"checked it: false differences in at most %.0f%% of experiments without a true difference, and 95%% intervals that cover the true effect at least %.0f%% of the time.",
				title(r.Metric), experiment.MinRepeats, rep.Lock.Method, wider(r.T95, r.Boot95), 100*stats.OneRunMaxFalseDifferences, 100*stats.OneRunMinCoverage))
		}
	}
	if a.Sequential != nil {
		out = append(out, fmt.Sprintf("Verdicts are given for %s; %s are exploratory. A verdict needs the bootstrap and the t-interval to agree: "+
			"the intervals in the summary are the wider of the two, cost's at its look's levels (\"equivalent\" is two one-sided tests at the look's "+
			"equivalence level), the others' at 95%%, or at 90%% for \"no loss\".", strings.Join(decided, " and "), strings.Join(exploratory, " and ")))
	} else {
		out = append(out, fmt.Sprintf("Verdicts are given for %s; %s are exploratory. A verdict needs the bootstrap and the t-interval to agree: "+
			"the intervals in the summary are the wider of the two, at 95%%, or at 90%% for \"no loss\" and \"equivalent\", which are one-sided tests at 5%%.",
			strings.Join(decided, " and "), strings.Join(exploratory, " and ")))
	}
	if rep.Template == experiment.TemplateAA {
		if rep.Lock.Sequential != nil {
			out = append(out, fmt.Sprintf("Both arms use the same context, so any difference is noise. Method %s spends %.1f%% over all its looks: "+
				"about one experiment in thirty shows a difference by chance.", experiment.MethodSeq, 100*rep.Lock.Sequential.Alpha))
		} else {
			out = append(out, "Both arms use the same context, so any difference is noise. At the 5% level, about one verdict in twenty shows a difference by chance.")
		}
	}
	cold := fmt.Sprintf("Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of %s.", rep.Lock.PriceTable)
	if unrepriced > 0 {
		cold += fmt.Sprintf(" %d counted run(s) use a model without a list price, so their arm has no cold-cache cost.", unrepriced)
	}
	isolatedNote := fmt.Sprintf("Isolated-run cost is each run's cost had no other run warmed the prompt cache: the cache reads of the main session's first request "+
		"and of each subagent launch that could not have read this run's own cache (a type's first, a parallel one, or one after its prefix expired) are repriced "+
		"as cache writes, at the time to live the run wrote with, at Agentium's list prices of %s. "+
		"Unlike cold-cache cost, which reprices every cached read (the run's own included) as a bound, it keeps a run's reads of its own cache. "+
		"It is at most the cold-cache cost, except when a subagent runs on a pricier model than the session or a run ended without Claude Code's result. "+
		"Verdicts use the actual cost.", rep.Lock.PriceTable)
	if noIsolated > 0 {
		isolatedNote += fmt.Sprintf(" %d counted run(s) have no isolated-run cost (recorded before Agentium kept it, no reported cost, a model without a list price, "+
			"a subagent request without a model, or a subagent of unknown type), so their arm shows none.", noIsolated)
	}
	return append(out, cold, isolatedNote)
}

// Censored is how many of the arm's counted runs were cut short (capped or timed out): their costs are lower bounds.
func (a Arm) Censored() int { return a.Capped + a.TimedOut }

// cappedNote says how many counted runs were cut short at their cap or the timeout, and what that means for their cost
// and the verdicts; "" when none was.
func cappedNote(rep Report) string {
	total := 0
	var parts []string
	for _, a := range rep.Arms {
		total += a.Censored()
		parts = append(parts, fmt.Sprintf("%s %d capped, %d timed out", a.Name, a.Capped, a.TimedOut))
	}
	if total == 0 {
		return ""
	}
	note := fmt.Sprintf("%d counted run(s) were cut short (%s): Claude Code stopped them at their cost cap or turn limit, or Agentium at "+
		"the timeout, so each one's cost is a lower bound of what it would have spent, and a mean that includes one is marked ≥ in the "+
		"per-task table. They count as they ended, as every run does: graded with the hidden tests (a success when they pass) and at the "+
		"cost they reached, which makes an arm cut short more often look cheaper than it would be without the cap; a cost verdict "+
		"that favours such an arm says so in its headline.", total, strings.Join(parts, "; "))
	if d := rep.Lock.Design; d.PerArmProfiles() && len(d.Arms) == 2 && d.ArmRunBudgetUSD(d.Arms[0]) != d.ArmRunBudgetUSD(d.Arms[1]) {
		note += " The arms' caps differ, so the arm with the lower cap is cut shorter."
	}
	return note
}

// overshootNote names the runs that passed their cost cap by more than the allowance the budget held for it; "" when
// none did.
func overshootNote(runs []Run) string {
	n, worst := 0, (*claude.Overshoot)(nil)
	for _, r := range runs {
		if o := r.Record.Overshoot; o != nil && o.Exceeded() {
			n++
			if worst == nil || o.OverUSD-o.AllowanceUSD > worst.OverUSD-worst.AllowanceUSD {
				worst = o
			}
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("Warning: %d run(s) passed their cost cap by more than the allowance the budget holds for that (the most: $%.3f past a "+
		"$%.2f cap, against a $%.2f allowance), so the spending may have passed the budget by the difference.", n, worst.OverUSD, worst.CapUSD, worst.AllowanceUSD)
}

// censoredCaveat is the headline's caveat on a cost verdict that runs cut short favour: "improved", "improved (small)"
// and "no loss" favour arm B (it looks cheaper), "regressed" arm A, and "equivalent" either (a cut-off cost narrows a
// difference). "" otherwise.
func (r Report) censoredCaveat(res experiment.MetricResult) string {
	if res.Metric != experiment.MetricCost || len(r.Arms) != 2 {
		return ""
	}
	var favoured []Arm
	switch res.Verdict {
	case stats.Improved, stats.ImprovedSmall, stats.NoLoss:
		favoured = r.Arms[1:]
	case stats.Regressed:
		favoured = r.Arms[:1]
	case stats.Equivalent:
		favoured = r.Arms
	}
	var parts []string
	for _, a := range favoured {
		if a.Censored() > 0 {
			parts = append(parts, fmt.Sprintf("%d in arm %s", a.Censored(), a.Name))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf(" (caveat: runs cut short at their cap or the timeout, %s, cost at least what they reached and favour this verdict)",
		strings.Join(parts, " and "))
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

func title(metric string) string {
	return map[string]string{experiment.MetricSuccess: "Success", experiment.MetricCost: "Cost", experiment.MetricTime: "Time",
		experiment.MetricOutput: "Output tokens", experiment.MetricJudgeSuccess: "The judge says fixed"}[metric]
}

// VerdictInterval is the interval a verdict rests on (see verdictInterval), without its level.
func VerdictInterval(r experiment.MetricResult) stats.Interval {
	i, _ := verdictInterval(r)
	return i
}

// verdictInterval is the interval a verdict rests on: the wider of the bootstrap and the t-interval, at 90% for "no
// loss" and "equivalent" (one-sided tests at 5%) and at 95% otherwise; a seq-v1 look's primary metric at the look's
// equivalence and efficacy levels.
func verdictInterval(r experiment.MetricResult) (stats.Interval, string) {
	a, b, level := r.Boot95, r.T95, "95%"
	if r.Level > 0 {
		level = fmt.Sprintf("%.2f%%", 100*r.Level)
	}
	if r.Verdict == stats.NoLoss || r.Verdict == stats.Equivalent {
		a, b, level = r.Boot90, r.T90, "90%"
		if r.EqLevel > 0 {
			level = fmt.Sprintf("%.2f%%", 100*r.EqLevel)
		}
	}
	return stats.Interval{Estimate: a.Estimate, Low: math.Min(a.Low, b.Low), High: math.Max(a.High, b.High)}, level
}

// wider names the interval that reaches further, side by side: Decide takes the wider bound on each side.
func wider(t, boot stats.Interval) string {
	switch {
	case t.Low <= boot.Low && t.High >= boot.High:
		return "the t-interval across tasks, the wider of the two here,"
	case boot.Low <= t.Low && boot.High >= t.High:
		return "the bootstrap, the wider of the two here,"
	}
	return "the wider of the t-interval and the bootstrap on each side"
}

// sandboxNote says what the grading sandbox did to the runs: grades that did not run because it did not hold (the
// canary: infrastructure, retried or left out), failed grades with denials the agents' own sandbox does not impose
// (left out, not tried again: run.OutcomeSandboxFlagged), counted per arm against the counted pairs, with the
// demotion when the arms differ (experiment.SandboxCheck), passing grades with such denials, and unread denials.
// Empty when no run was graded in the sandbox.
func sandboxNote(rep Report, runs []Run) string {
	canary, flaggedPasses, unread, harmless, graded := 0, 0, 0, 0, 0
	for _, r := range runs {
		g := r.Record.Sandbox
		if g == nil {
			continue
		}
		graded++
		switch {
		case g.Canary != task.CanaryPassed:
			canary++
		case g.FlaggedCount > 0 && r.Record.Outcome != run.OutcomeSandboxFlagged:
			flaggedPasses++
		}
		if g.Unread != "" {
			unread++
		}
		if g.Harmless > 0 {
			harmless++
		}
	}
	check := rep.Analysis.Sandbox
	left := 0
	if check != nil {
		for _, n := range check.Flagged {
			left += n
		}
	}
	if graded == 0 || canary+flaggedPasses+unread+harmless+left == 0 {
		return ""
	}
	var parts []string
	if canary > 0 {
		parts = append(parts, fmt.Sprintf("%d grade(s) did not run because the sandbox did not hold (its canary failed): infrastructure, retried or left out", canary))
	}
	if left > 0 {
		var arms []string
		for _, a := range rep.Arms {
			arms = append(arms, fmt.Sprintf("%s %d", a.Name, check.Flagged[a.Name]))
		}
		part := fmt.Sprintf("failed grades with denials the agents' own sandbox does not impose were left out, not tried again (%s; %d counted pair(s)); "+
			"with one run per arm a task left out in one arm drops out of the paired comparison (with repeats it stays paired on its other runs), "+
			"and the other arm's run of it counts in that arm's own rates; %s",
			strings.Join(arms, ", "), check.Pairs, asFailsText(check))
		switch {
		case check.Imbalanced:
			part += "; the arms differ, so the cost and success verdicts are demoted to inconclusive"
		case len(check.Disagrees) > 0:
			which := "that verdict is"
			if len(check.Disagrees) > 1 {
				which = "those verdicts are"
			}
			part += "; that changes the " + strings.Join(check.Disagrees, " and ") + " verdict, so " + which + " demoted to inconclusive"
		}
		parts = append(parts, part)
	}
	if flaggedPasses > 0 {
		parts = append(parts, fmt.Sprintf("%d passing grade(s) logged such denials and stay passes", flaggedPasses))
	}
	if harmless > 0 {
		parts = append(parts, fmt.Sprintf("%d grade(s) logged denials the task's reference also logged while passing its validation, which count as harmless", harmless))
	}
	if unread > 0 {
		parts = append(parts, fmt.Sprintf("the denials of %d grade(s) could not be read, so their results stand as the tests gave them", unread))
	}
	return "Grading sandbox: " + strings.Join(parts, "; ") + "."
}

// asFailsText is the sensitivity check in words: "counting them as fails gives cost improved, success inconclusive".
func asFailsText(c *experiment.SandboxCheck) string {
	var parts []string
	for _, m := range []string{experiment.MetricCost, experiment.MetricSuccess} {
		if v, ok := c.AsFails[m]; ok {
			parts = append(parts, strings.ToLower(title(m))+" "+v)
		}
	}
	return "counting them as fails gives " + strings.Join(parts, ", ")
}

// graderNote says where the experiment's runs were graded; empty for a lock made before grader modes (the host).
func graderNote(l experiment.Lock) string {
	switch {
	case l.Grader == "":
		return ""
	case task.GraderOf(l.Grader) == task.GraderHost:
		return "Graded on the host, without a sandbox: the agents' code ran its builds and tests with the user's access (--grader host)."
	case task.GraderOf(l.Grader) != task.GraderSandbox: // container-v1 included, until the containers plan's step 4
		return fmt.Sprintf("Graded in %s, a mode this Agentium does not describe.", l.Grader)
	}
	return fmt.Sprintf("Graded in Agentium's grading sandbox (%s): no network but this machine's, writes only to each grade's own folders. "+
		"On macOS the sandbox's localhost is every address of the machine, so a grade could accept connections from the network.", l.Grader)
}

// localBindingNote is shown when the experiment's lock records the sandbox's local binding.
const localBindingNote = "The agents' sandbox allowed local binding (a Gradle project, with the user's opt-in): an agent could bind any local port and connect to services listening on localhost, and concurrent runs could reach each other."
