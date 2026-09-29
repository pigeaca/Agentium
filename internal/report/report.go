// Package report turns an experiment's lock and runs into its report: verdicts in plain words, the metrics with both
// intervals, a per-task table, behavior counts per arm, the context overhead, costs, and honesty notes. It renders
// Markdown (for a pull request) and JSON (with the lock and every run). Both are meant to be shared: skill and command
// names, local paths and what the agents said are left out, and credential-shaped text is redacted.
package report

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
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
}

// Report is an experiment's results, ready to render.
type Report struct {
	Experiment string              `json:"experiment"`
	Template   string              `json:"template"`
	Status     string              `json:"status"`
	StatusNote string              `json:"status_note,omitempty"`
	Slots      int                 `json:"slots"`
	Settled    int                 `json:"settled"`
	SpentUSD   float64             `json:"spent_usd"`
	Lock       experiment.Lock     `json:"lock"` // skill and command names counted; Claude Code's file name only
	Analysis   experiment.Analysis `json:"analysis"`
	Arms       []Arm               `json:"arms"`
	Tasks      []TaskRow           `json:"tasks"`
	Notes      []string            `json:"notes"`
	Runs       []RunRow            `json:"runs"`
}

// Arm summarizes one arm's counted runs. Means are nil when there is nothing to average.
type Arm struct {
	Name     string `json:"name"`
	Context  string `json:"context"`
	Snapshot string `json:"snapshot,omitempty"`
	Counted  int    `json:"counted"`
	// FirstRequest is the mean measured size of the first request: the context overhead Claude Code saw.
	FirstRequest   *float64 `json:"first_request_tokens"`
	CostUSD        *float64 `json:"cost_usd"`         // mean reported cost
	ColdCostUSD    *float64 `json:"cold_cost_usd"`    // mean with every cached read repriced as a one-hour cache write
	CacheReadShare *float64 `json:"cache_read_share"` // of all input tokens
	Behavior       Behavior `json:"behavior"`
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
	Marks     string   `json:"marks"`
	Successes int      `json:"successes"`
	Counted   int      `json:"counted"`
	CostUSD   *float64 `json:"cost_usd"`
}

// RunRow is one run's data, as shared: no local paths, and not what the agent said.
type RunRow struct {
	ID             string         `json:"id"`
	Slot           int            `json:"slot"`
	Attempt        int            `json:"attempt"`
	Task           string         `json:"task"`
	Arm            string         `json:"arm"`
	Outcome        string         `json:"outcome"`
	Passed         *bool          `json:"passed,omitempty"`
	Success        bool           `json:"success"`
	Metrics        claude.Metrics `json:"metrics"` // without the result excerpt
	Behavior       run.Behavior   `json:"behavior"`
	CostEstimated  bool           `json:"cost_estimated,omitempty"`
	Recovered      string         `json:"recovered,omitempty"`
	HarnessChanged []string       `json:"harness_changed,omitempty"`
	Drift          []string       `json:"drift,omitempty"`
	Notes          []string       `json:"notes,omitempty"`
	ContextCommit  string         `json:"context_commit,omitempty"`
	Verify         []taskCommand  `json:"verify,omitempty"`
	Started        string         `json:"started"`
	Finished       string         `json:"finished"`
}

type taskCommand struct {
	Command  string  `json:"command"`
	ExitCode int     `json:"exit_code"`
	Seconds  float64 `json:"seconds"`
}

// Build computes the report.
func Build(in Input) (Report, error) {
	l := in.Lock
	var data []experiment.RunData
	for _, r := range in.Runs {
		m := r.Record.Metrics
		data = append(data, experiment.RunData{Slot: r.Slot, Task: r.Record.Task, Arm: r.Record.Arm, Outcome: r.Record.Outcome,
			Passed: r.Record.Passed, ConfigChanged: r.Record.Behavior.ConfigChanged, CostUSD: m.CostUSD, DurationS: float64(m.DurationMS) / 1000,
			OutputTokens: float64(m.OutputTokens)})
	}
	analysis, err := experiment.Analyze(l, data)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Experiment: in.Name, Template: l.Design.Template, Status: in.Status, StatusNote: in.StatusNote, Slots: len(l.Schedule),
		Lock: redactLock(l), Analysis: analysis}
	settled := map[int]bool{}
	for _, r := range in.Runs {
		rep.SpentUSD += r.Record.Metrics.CostUSD
		if experiment.Settles(r.Record.Outcome) {
			settled[r.Slot] = true
		}
		rec := r.Record
		metrics := rec.Metrics
		metrics.ResultExcerpt = "" // what the agent said can hold anything
		row := RunRow{ID: r.ID, Slot: r.Slot, Attempt: r.Attempt, Task: rec.Task, Arm: rec.Arm, Outcome: rec.Outcome, Passed: rec.Passed,
			Success: experiment.Success(rec.Outcome, rec.Passed, rec.Behavior.ConfigChanged), Metrics: metrics, Behavior: rec.Behavior,
			CostEstimated: rec.CostEstimated, Recovered: rec.Recovered, HarnessChanged: rec.HarnessChanged, Drift: in.scrubAll(rec.Drift),
			Notes: in.scrubAll(rec.Notes), ContextCommit: rec.ContextHead, Started: rec.Started.UTC().Format("2006-01-02T15:04:05Z"),
			Finished: rec.Finished.UTC().Format("2006-01-02T15:04:05Z")}
		for _, c := range rec.Verify {
			row.Verify = append(row.Verify, taskCommand{Command: c.Command, ExitCode: c.ExitCode, Seconds: c.Seconds})
		}
		rep.Runs = append(rep.Runs, row)
	}
	rep.Settled = len(settled)
	for _, a := range l.Arms {
		rep.Arms = append(rep.Arms, armSummary(a, in.Runs))
	}
	rep.Tasks = taskRows(l, in.Runs)
	rep.Notes = notes(rep, in)
	return rep, nil
}

// scrub makes text shareable: the data folder and home folder become placeholders, and credential-shaped strings are
// redacted.
func (in Input) scrub(text string) string {
	for _, p := range []struct{ path, as string }{{in.DataDir, "<agentium data>"}, {in.Home, "~"}} {
		if p.path != "" && p.path != "/" {
			text = strings.ReplaceAll(text, p.path, p.as)
		}
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
		cost = append(cost, m.CostUSD)
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
	return m.CostUSD + float64(m.CacheReadTokens)*(rates.CacheWrite1h-rates.CacheRead)/1e6, true
}

func mean(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	m := stats.Mean(values)
	return &m
}

func taskRows(l experiment.Lock, runs []Run) []TaskRow {
	bySlot := slices.Clone(runs)
	slices.SortStableFunc(bySlot, func(x, y Run) int { return x.Slot - y.Slot })
	var rows []TaskRow
	for _, t := range l.Tasks {
		row := TaskRow{Task: t.Name, Arms: map[string]TaskCell{}}
		for _, a := range l.Arms {
			var cell TaskCell
			var costs []float64
			for _, r := range bySlot {
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
				costs = append(costs, rec.Metrics.CostUSD)
			}
			cell.CostUSD = mean(costs)
			row.Arms[a.Name] = cell
		}
		rows = append(rows, row)
	}
	return rows
}

// notes are the honesty notes: what the verdicts leave out or rest on.
func notes(rep Report, in Input) []string {
	var out []string
	a := rep.Analysis
	if rep.Status != experiment.StatusDone {
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
		if rec.CostEstimated {
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
		if r.Verdict == stats.Exploratory && r.Warning == "" && r.Note == "" {
			out = append(out, fmt.Sprintf("%s is exploratory: %d of %d task(s) have %d counted runs in both arms, below the floor of %s.", title(r.Metric),
				r.FullTasks, r.Tasks, experiment.MinRepeats, floorText(r.Metric)))
		}
	}
	out = append(out, fmt.Sprintf("Verdicts are given for %s; %s are exploratory. A verdict needs the bootstrap and the t-interval to agree: "+
		"the intervals in the summary are the wider of the two, at 95%%, or at 90%% for \"no loss\" and \"equivalent\", which are one-sided tests at 5%%.",
		strings.Join(decided, " and "), strings.Join(exploratory, " and ")))
	if rep.Template == experiment.TemplateAA {
		out = append(out, "Both arms use the same context, so any difference is noise. At the 5% level, about one verdict in twenty shows a difference by chance.")
	}
	if v := a.Variance; v != nil {
		out = append(out, fmt.Sprintf("Measured noise, for planning later experiments: per-run log-cost spread σ = %.2f, success variance w = %.2f, spread across tasks τ = %.2f (cost) and %.2f (success), from %.1f run(s) per task and arm.",
			v.SigmaLogCost, v.WSuccess, v.TauLogCost, v.TauSuccess, v.Repeats))
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

func floorText(metric string) string {
	if metric == experiment.MetricSuccess {
		return fmt.Sprint(experiment.MinTasksSuccess)
	}
	return fmt.Sprint(experiment.MinTasksCost)
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
