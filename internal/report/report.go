// Package report turns an experiment's lock and runs into its report: verdicts in plain words, the metrics with both
// intervals, a per-task table, behavior counts per arm, the context overhead, costs, and honesty notes. It renders
// Markdown (for a pull request) and JSON (with the lock and every run).
package report

import (
	"fmt"
	"maps"
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

// Input is what a report is made from. Runs come in the order they started.
type Input struct {
	Name       string
	Lock       experiment.Lock
	Status     string
	StatusNote string
	Runs       []Run
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
	Lock       experiment.Lock     `json:"lock"` // skill and command names replaced by counts
	Analysis   experiment.Analysis `json:"analysis"`
	Arms       []Arm               `json:"arms"`
	Tasks      []TaskRow           `json:"tasks"`
	Notes      []string            `json:"notes"`
	Runs       []RunRow            `json:"runs"`
}

// Arm summarizes one arm's counted runs.
type Arm struct {
	Name     string `json:"name"`
	Context  string `json:"context"`
	Snapshot string `json:"snapshot,omitempty"`
	Counted  int    `json:"counted"`
	// FirstRequest is the mean measured size of the first request: the context overhead Claude Code saw.
	FirstRequest   float64  `json:"first_request_tokens"`
	CostUSD        float64  `json:"cost_usd"`         // mean reported cost
	ColdCostUSD    float64  `json:"cold_cost_usd"`    // mean with every cached read priced as a one-hour cache write
	CacheReadShare float64  `json:"cache_read_share"` // of all input tokens
	Behavior       Behavior `json:"behavior"`
}

// Behavior counts what the agents did, over an arm's counted runs.
type Behavior struct {
	TestsChanged  int     `json:"tests_changed"`  // runs that changed a test file
	TestsRemoved  int     `json:"tests_removed"`  // test files removed, in total
	RanTests      int     `json:"ran_tests"`      // runs that ran a test runner
	RanChecks     int     `json:"ran_checks"`     // runs that ran one of the task's verification commands
	Committed     int     `json:"committed"`      // runs that committed
	ChecksChanged int     `json:"checks_changed"` // runs that changed verification scripts or runner configuration
	ConfigChanged int     `json:"config_changed"` // runs graded with changed runner configuration
	Denials       int     `json:"denials"`        // in total
	OutsideReads  int     `json:"outside_reads"`  // in total
	FilesChanged  float64 `json:"files_changed"`  // mean
	LinesChanged  float64 `json:"lines_changed"`  // mean, added and removed
	BashCommands  float64 `json:"bash_commands"`  // mean
}

// TaskRow is one task's runs per arm.
type TaskRow struct {
	Task string              `json:"task"`
	Arms map[string]TaskCell `json:"arms"`
}

// TaskCell is a task's runs in one arm: marks in schedule order (● success, ○ failure, × not counted), and the mean cost
// of the counted runs.
type TaskCell struct {
	Marks     string  `json:"marks"`
	Successes int     `json:"successes"`
	Counted   int     `json:"counted"`
	CostUSD   float64 `json:"cost_usd"`
}

// RunRow is one run's data, without local paths.
type RunRow struct {
	ID            string         `json:"id"`
	Slot          int            `json:"slot"`
	Attempt       int            `json:"attempt"`
	Task          string         `json:"task"`
	Arm           string         `json:"arm"`
	Outcome       string         `json:"outcome"`
	Passed        *bool          `json:"passed,omitempty"`
	Success       bool           `json:"success"`
	Metrics       claude.Metrics `json:"metrics"`
	Behavior      run.Behavior   `json:"behavior"`
	Drift         []string       `json:"drift,omitempty"`
	Notes         []string       `json:"notes,omitempty"`
	ContextCommit string         `json:"context_commit,omitempty"`
	Verify        []taskCommand  `json:"verify,omitempty"`
	Started       string         `json:"started"`
	Finished      string         `json:"finished"`
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
		row := RunRow{ID: r.ID, Slot: r.Slot, Attempt: r.Attempt, Task: rec.Task, Arm: rec.Arm, Outcome: rec.Outcome, Passed: rec.Passed,
			Success: experiment.Success(rec.Outcome, rec.Passed, rec.Behavior.ConfigChanged), Metrics: rec.Metrics, Behavior: rec.Behavior,
			Drift: rec.Drift, Notes: rec.Notes, ContextCommit: rec.ContextHead, Started: rec.Started.UTC().Format("2006-01-02T15:04:05Z"),
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

// redactLock keeps the lock for sharing: skill and command names, which can be personal, become counts, and Claude
// Code's path (often under the user's home folder) its file name.
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
		cold = append(cold, coldCost(rec))
		cacheRead += float64(m.CacheReadTokens)
		input += float64(m.InputTokens + m.CacheReadTokens + m.CacheWriteTokens)
		files, lines, bash = append(files, float64(b.FilesChanged)), append(lines, float64(b.LinesAdded+b.LinesRemoved)), append(bash, float64(b.BashCommands))
		counts := &arm.Behavior
		counts.TestsRemoved += b.TestsRemoved
		counts.Denials += b.Denials
		counts.OutsideReads += b.OutsideReads
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
		if len(b.ConfigChanged) > 0 {
			counts.ConfigChanged++
		}
	}
	arm.FirstRequest, arm.CostUSD, arm.ColdCostUSD = mean(first), mean(cost), mean(cold)
	arm.Behavior.FilesChanged, arm.Behavior.LinesChanged, arm.Behavior.BashCommands = mean(files), mean(lines), mean(bash)
	if input > 0 {
		arm.CacheReadShare = cacheRead / input
	}
	return arm
}

// coldCost prices a run as if nothing had been cached: every cache read at the one-hour write rate, which Claude Code
// uses. It is the reported cost when the model has no list price.
func coldCost(rec run.Record) float64 {
	m := rec.Metrics
	model := m.Model
	if model == "" {
		model = rec.Model
	}
	rates, ok := pricing.Lookup(model)
	if !ok || m.CacheReadTokens == 0 {
		return m.CostUSD
	}
	return m.CostUSD + float64(m.CacheReadTokens)*(rates.CacheWrite1h-rates.CacheRead)/1e6
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return stats.Mean(values)
}

func taskRows(l experiment.Lock, runs []Run) []TaskRow {
	var rows []TaskRow
	for _, t := range l.Tasks {
		row := TaskRow{Task: t.Name, Arms: map[string]TaskCell{}}
		for _, a := range l.Arms {
			var cell TaskCell
			var costs []float64
			bySlot := slices.Clone(runs)
			slices.SortStableFunc(bySlot, func(x, y Run) int { return x.Slot - y.Slot })
			for _, r := range bySlot {
				rec := r.Record
				if rec.Task != t.Name || rec.Arm != a.Name {
					continue
				}
				switch {
				case !experiment.Fair(rec.Outcome):
					cell.Marks += "×"
				case experiment.Success(rec.Outcome, rec.Passed, rec.Behavior.ConfigChanged):
					cell.Marks += "●"
					cell.Successes++
					cell.Counted++
					costs = append(costs, rec.Metrics.CostUSD)
				default:
					cell.Marks += "○"
					cell.Counted++
					costs = append(costs, rec.Metrics.CostUSD)
				}
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
			status += ": " + rep.StatusNote
		}
		out = append(out, fmt.Sprintf("The experiment is not finished (%s): %d of %d runs settled, and the results cover those.", status, rep.Settled, rep.Slots))
	}
	if len(a.Excluded) > 0 {
		var parts []string
		for _, outcome := range []string{claude.OutcomeUnfair, claude.OutcomeInfra, claude.OutcomeCancelled} {
			if n := a.Excluded[outcome]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, map[string]string{claude.OutcomeUnfair: "unfair (the environment drifted)",
					claude.OutcomeInfra: plural(n, "infrastructure failure"), claude.OutcomeCancelled: "cancelled"}[outcome]))
			}
		}
		for outcome, n := range a.Excluded {
			if !slices.Contains([]string{claude.OutcomeUnfair, claude.OutcomeInfra, claude.OutcomeCancelled}, outcome) {
				parts = append(parts, fmt.Sprintf("%d %s", n, outcome))
			}
		}
		out = append(out, "Runs not counted: "+strings.Join(parts, ", ")+". Their spend is in the total.")
	}
	var drift []string
	estimated, recovered, harness := 0, 0, map[string]bool{}
	for _, r := range in.Runs {
		for _, d := range r.Record.Drift {
			if !slices.Contains(drift, d) {
				drift = append(drift, d)
			}
		}
		for _, n := range r.Record.Notes {
			switch {
			case strings.Contains(n, "estimated from the transcript"):
				estimated++
			case strings.HasPrefix(n, "Agentium stopped during this run"), strings.HasPrefix(n, "stored on recovery"):
				recovered++
			case strings.HasPrefix(n, "the arm changes what runs: "):
				harness[r.Record.Arm+": "+strings.TrimPrefix(n, "the arm changes what runs: ")] = true
			}
		}
	}
	if len(drift) > 0 {
		out = append(out, "Environment drift in unfair runs: "+strings.Join(drift, "; ")+".")
	}
	if estimated > 0 {
		out = append(out, fmt.Sprintf("%d run(s) ended without Claude Code's cost: it was estimated from their transcripts at list prices.", estimated))
	}
	if recovered > 0 {
		out = append(out, fmt.Sprintf("%d run(s) were recovered after Agentium stopped during them.", recovered))
	}
	if len(harness) > 0 {
		out = append(out, "An arm changes what runs, not only what the agent reads (hooks, settings or MCP): "+strings.Join(slices.Sorted(maps.Keys(harness)), "; ")+".")
	}
	for _, arm := range rep.Arms {
		if arm.Behavior.ConfigChanged > 0 {
			out = append(out, fmt.Sprintf("Arm %s: %d run(s) passed with test-runner configuration changed beyond the task's reference; they count as failures.", arm.Name, arm.Behavior.ConfigChanged))
		}
	}
	if len(a.NotDiscriminating) > 0 {
		out = append(out, fmt.Sprintf("Not discriminating for success (every run passed, or every run failed, in both arms): %s. They stay in for cost.",
			strings.Join(a.NotDiscriminating, ", ")))
	}
	for _, r := range a.Results {
		if r.Role != experiment.RoleSecondary && r.Verdict == stats.Exploratory && r.Warning == "" {
			out = append(out, fmt.Sprintf("%s is exploratory: %d of %d task(s) have %d counted runs in both arms, below the floor of %s.", title(r.Metric),
				r.FullTasks, r.Tasks, experiment.MinRepeats, floorText(r.Metric)))
		}
	}
	out = append(out, "Verdicts need the bootstrap and the t-interval to agree; the intervals in the summary are the wider of the two. Time and output tokens are exploratory: one primary metric, and the success guard, get verdicts.")
	if rep.Template == experiment.TemplateAA {
		out = append(out, "Both arms use the same context, so any difference is noise. At the 5% level, about one verdict in twenty shows a difference by chance.")
	}
	if v := a.Variance; v != nil {
		out = append(out, fmt.Sprintf("Measured noise, for planning later experiments: per-run log-cost spread σ = %.2f, success variance w = %.2f, spread across tasks τ = %.2f (cost) and %.2f (success), from %.1f run(s) per task and arm.",
			v.SigmaLogCost, v.WSuccess, v.TauLogCost, v.TauSuccess, v.Repeats))
	}
	out = append(out, fmt.Sprintf("Cold-cache cost prices every cached read as a one-hour cache write, at Agentium's list prices of %s.", rep.Lock.PriceTable))
	return out
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

// widest95 is the interval the verdict rests on: the wider of the bootstrap and the t-interval at 95%.
func widest95(r experiment.MetricResult) stats.Interval {
	return stats.Interval{Estimate: r.Boot95.Estimate, Low: math.Min(r.Boot95.Low, r.T95.Low), High: math.Max(r.Boot95.High, r.T95.High)}
}
