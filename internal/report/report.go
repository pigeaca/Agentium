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

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
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
	// NorthStar is the project's time and spend to its first decisive verdict; Load sets it (Build does not: it needs the
	// project's other experiments).
	NorthStar *NorthStar `json:"north_star,omitempty"`
	Notes     []string   `json:"notes"`
	Runs      []RunRow   `json:"runs"`
}

// Arm summarizes one arm's counted runs. Means are nil when there is nothing to average.
type Arm struct {
	Name     string `json:"name"`
	Context  string `json:"context"`
	Snapshot string `json:"snapshot,omitempty"`
	// Profile is the arm's model and effort ("MODEL" or "MODEL:EFFORT"), in a model-ab experiment only.
	Profile string `json:"profile,omitempty"`
	Counted int    `json:"counted"`
	// FirstRequest is the mean measured size of the first request: the context overhead Claude Code saw.
	FirstRequest   *float64 `json:"first_request_tokens"`
	CostUSD        *float64 `json:"cost_usd"`         // mean reported cost
	ColdCostUSD    *float64 `json:"cold_cost_usd"`    // mean with every cached read repriced as a one-hour cache write
	CacheReadShare *float64 `json:"cache_read_share"` // of all input tokens
	Behavior       Behavior `json:"behavior"`
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
}

// TaskCell is a task's runs in one arm: marks in schedule order (● success, ○ failure, × not counted), and the mean cost
// of the counted runs (nil without any).
type TaskCell struct {
	Profile   string   `json:"profile,omitempty"` // the arm's model and effort, in a model-ab experiment only
	Marks     string   `json:"marks"`
	Successes int      `json:"successes"`
	Counted   int      `json:"counted"`
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
	Metrics        claude.Metrics  `json:"metrics"` // without the result excerpt
	Behavior       run.Behavior    `json:"behavior"`
	CostEstimated  bool            `json:"cost_estimated,omitempty"`
	Recovered      string          `json:"recovered,omitempty"`
	HarnessChanged []string        `json:"harness_changed,omitempty"`
	Drift          []string        `json:"drift,omitempty"`
	Notes          []string        `json:"notes,omitempty"`
	ContextCommit  string          `json:"context_commit,omitempty"`
	ContextUse     *run.ContextUse `json:"context_use,omitempty"`
	Judge          *judge.Verdict  `json:"judge,omitempty"` // the judge's verdict, its texts scrubbed
	Verify         []taskCommand   `json:"verify,omitempty"`
	Started        string          `json:"started"`
	Finished       string          `json:"finished"`
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
		data = append(data, runData(r.Slot, r.Record))
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
			Notes: in.scrubAll(rec.Notes), ContextCommit: rec.ContextHead, ContextUse: rec.ContextUse, Judge: in.shareVerdict(rec.Judge), Started: rec.Started.UTC().Format("2006-01-02T15:04:05Z"),
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
	rep.Judge = judgeSummary(in)
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
			// Whole components only: /Users/v must not turn /Users/vlad into ~lad, nor /var/x match in /private/var/x.
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

// redactLock keeps the lock for sharing: skill and command names, which can be personal, become counts, and Claude
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
	return out
}

func armSummary(a experiment.LockedArm, runs []Run) Arm {
	arm := Arm{Name: a.Name, Context: a.Context, Snapshot: a.Snapshot}
	var first, cost, cold, files, lines, bash []float64
	var cacheRead, input float64
	for _, r := range runs {
		rec := r.Record
		if rec.Arm != a.Name || !experiment.Fair(rec.Outcome) {
			continue
		}
		arm.Counted++
		m, b := rec.Metrics, rec.Behavior
		if m.FirstRequest > 0 {
			first = append(first, float64(m.FirstRequest))
		}
		cost = append(cost, rec.Spend().AgentUSD)
		if c, ok := coldCost(rec); ok {
			cold = append(cold, c)
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
		row := TaskRow{Task: t.Name, Arms: map[string]TaskCell{}}
		for _, a := range l.Arms {
			var cell TaskCell
			var costs []float64
			for _, r := range ordered {
				rec := r.Record
				if rec.Task != t.Name || rec.Arm != a.Name {
					continue
				}
				switch {
				case !experiment.Fair(rec.Outcome):
					cell.Marks += "×"
					continue
				case experiment.Success(rec.Outcome, rec.Passed, rec.Behavior.ConfigChanged):
					cell.Marks += "●"
					cell.Successes++
				default:
					cell.Marks += "○"
				}
				cell.Counted++
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
		out = append(out, fmt.Sprintf("The experiment is not finished (%s): %d of %d runs settled, and the results cover those.", status, rep.Settled, rep.Slots))
	}
	if len(a.Excluded) > 0 {
		var parts []string
		known := []string{claude.OutcomeUnfair, claude.OutcomeInfra, claude.OutcomeCancelled}
		for _, outcome := range known {
			if n := a.Excluded[outcome]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, map[string]string{claude.OutcomeUnfair: "unfair (the environment drifted)",
					claude.OutcomeInfra: plural(n, "infrastructure failure"), claude.OutcomeCancelled: "cancelled"}[outcome]))
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
	estimated, stopped, finished, unpriced, unrepriced := 0, 0, 0, 0, 0
	for _, r := range in.Runs {
		rec := r.Record
		if rec.Outcome == claude.OutcomeUnfair {
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
	}
	if len(drift) > 0 {
		out = append(out, "Environment drift in unfair runs: "+strings.Join(drift, "; ")+".")
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
		case r.Verdict == stats.Exploratory && r.Warning == "" && r.Note == "":
			out = append(out, fmt.Sprintf("%s is exploratory: %d of %d task(s) have %d or more counted %s in both arms, below the floor of %d tasks (method %s).",
				title(r.Metric), r.FullTasks, r.Tasks, r.FloorRepeats, plural(r.FloorRepeats, "run"), r.FloorTasks, rep.Lock.Method))
		case r.Verdict != stats.Exploratory && r.FloorRepeats < experiment.MinRepeats && r.Repeats < experiment.MinRepeats:
			out = append(out, fmt.Sprintf("%s's verdict rests on tasks with fewer than %d runs per arm, as method %s allows: each task's difference "+
				"carries the run-to-run noise, and %s decides. A seeded simulation of 8–12 tasks × 1 run (σ = 0.19, τ = 0.10–0.25, normal and skewed noise) "+
				"checked it: false differences in at most %.0f%% of experiments without a true difference, and 95%% intervals that cover the true effect at least %.0f%% of the time.",
				title(r.Metric), experiment.MinRepeats, rep.Lock.Method, wider(r.T95, r.Boot95), 100*stats.OneRunMaxFalseDifferences, 100*stats.OneRunMinCoverage))
		}
	}
	out = append(out, fmt.Sprintf("Verdicts are given for %s; %s are exploratory. A verdict needs the bootstrap and the t-interval to agree: "+
		"the intervals in the summary are the wider of the two, at 95%%, or at 90%% for \"no loss\" and \"equivalent\", which are one-sided tests at 5%%.",
		strings.Join(decided, " and "), strings.Join(exploratory, " and ")))
	if rep.Template == experiment.TemplateAA {
		out = append(out, "Both arms use the same context, so any difference is noise. At the 5% level, about one verdict in twenty shows a difference by chance.")
	}
	cold := fmt.Sprintf("Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of %s.", rep.Lock.PriceTable)
	if unrepriced > 0 {
		cold += fmt.Sprintf(" %d counted run(s) use a model without a list price, so their arm has no cold-cache cost.", unrepriced)
	}
	return append(out, cold)
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

func title(metric string) string {
	return map[string]string{experiment.MetricSuccess: "Success", experiment.MetricCost: "Cost", experiment.MetricTime: "Time",
		experiment.MetricOutput: "Output tokens"}[metric]
}

// verdictInterval is the interval a verdict rests on: the wider of the bootstrap and the t-interval, at 90% for "no
// loss" and "equivalent" (one-sided tests at 5%) and at 95% otherwise.
func verdictInterval(r experiment.MetricResult) (stats.Interval, string) {
	a, b, level := r.Boot95, r.T95, "95%"
	if r.Verdict == stats.NoLoss || r.Verdict == stats.Equivalent {
		a, b, level = r.Boot90, r.T90, "90%"
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

// localBindingNote is shown when the experiment's lock records the sandbox's local binding.
const localBindingNote = "The agents' sandbox allowed local binding (a Gradle project, with the user's opt-in): an agent could bind any local port and connect to services listening on localhost, and concurrent runs could reach each other."
