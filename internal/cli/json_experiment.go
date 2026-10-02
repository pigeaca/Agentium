package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
)

// The --json documents of the experiment commands (docs/guide.md, "Scripting and automation"). They carry no path and
// no storage type: every struct here is owned by this package and fixed by key-set tests.

// experimentArmDoc is one arm of a design. Model and Effort are what the arm runs on ("" effort: the CLI's default).
type experimentArmDoc struct {
	Name    string `json:"name"` // A | B
	Context string `json:"context"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
}

// experimentDoc is an experiment's design as new, plan, show and run describe it.
type experimentDoc struct {
	Name          string             `json:"name"`
	Template      string             `json:"template"` // context-ab | aa | model-ab
	Goal          string             `json:"goal"`     // cheaper | better
	Method        string             `json:"method"`   // seq-v1 for a cost experiment
	Arms          []experimentArmDoc `json:"arms"`
	Tasks         []string           `json:"tasks"`
	RepeatsPerArm int                `json:"repeats_per_arm"`
	Runs          int                `json:"runs"`       // all the design's runs, both arms (a seq-v1 experiment may stop earlier)
	BudgetUSD     float64            `json:"budget_usd"` // the whole experiment, raised on resume
	RunBudgetUSD  float64            `json:"run_budget_usd"`
	Concurrency   int                `json:"concurrency"`
	Judge         bool               `json:"judge"`
}

func experimentOf(name string, d experiment.Design) experimentDoc {
	doc := experimentDoc{Name: name, Template: d.Template, Goal: d.Goal, Method: d.LockMethod(), Arms: []experimentArmDoc{}, Tasks: list(slices.Clone(d.Tasks)),
		RepeatsPerArm: d.Repeats, Runs: d.Runs(), BudgetUSD: d.BudgetUSD, RunBudgetUSD: d.RunBudgetUSD, Concurrency: d.Concurrency, Judge: d.Judge != nil}
	for _, a := range d.Arms {
		doc.Arms = append(doc.Arms, experimentArmDoc{Name: a.Name, Context: a.Context, Model: d.ArmModel(a), Effort: d.ArmEffort(a)})
	}
	return doc
}

// intervalDoc is an estimate with its interval, in the metric's units (a ratio B / A for cost, a difference for success).
type intervalDoc struct {
	Estimate float64 `json:"estimate"`
	Low      float64 `json:"low"`
	High     float64 `json:"high"`
}

// finite is v, or null when it is NaN or infinite: a document is written after the money was spent, so a number JSON
// cannot hold must never make its encoding fail.
func finite(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	return v
}

func finiteOf(v float64) *float64 { return finite(&v) }

// intervalOf is the interval, or null when any of its numbers is not finite.
func intervalOf(i *stats.Interval) *intervalDoc {
	if i == nil || finite(&i.Estimate) == nil || finite(&i.Low) == nil || finite(&i.High) == nil {
		return nil
	}
	return &intervalDoc{Estimate: i.Estimate, Low: i.Low, High: i.High}
}

// lookDoc is one look of a seq-v1 experiment. Level, Verdict, Interval and ConditionalPower are null for a look that
// gave no verdict for want of tasks (Analysed false) and, for the power, for a look that ended the experiment.
type lookDoc struct {
	Look             int          `json:"look"` // from 1
	TasksPlanned     int          `json:"tasks_planned"`
	TasksCounted     int          `json:"tasks_counted"`
	Analysed         bool         `json:"analysed"`
	Level            *float64     `json:"level"`   // of the interval the verdict reads, as a share (0.9984)
	Verdict          *string      `json:"verdict"` // the cost verdict at this look
	Interval         *intervalDoc `json:"interval"`
	ConditionalPower *float64     `json:"conditional_power"`
	Decision         string       `json:"decision"` // continue | stop | futility | final
	Note             string       `json:"note"`     // why a look gave no verdict; human text
}

func looksOf(s *experiment.SeqStatus) []lookDoc {
	out := []lookDoc{}
	if s == nil {
		return out
	}
	for _, l := range s.Looks {
		d := lookDoc{Look: l.Look, TasksPlanned: l.Planned, TasksCounted: l.Counted, Analysed: l.Analysed, Decision: l.Decision, Note: l.Note,
			ConditionalPower: finite(l.ConditionalPower)}
		if l.Analysed {
			verdict := l.Verdict
			d.Level, d.Verdict, d.Interval = finiteOf(l.LevelOfVerdict()), &verdict, intervalOf(l.IntervalOfVerdict())
		}
		out = append(out, d)
	}
	return out
}

// spendDoc is what a design may spend, before it runs. Known is false when a task has no estimate yet: the
// estimates are then null. Max is every look (or every run); WorstCase is every run, and its judgement, at its cap, which
// is what the budget is sized for. Expected is what a seq-v1 experiment spends on average if nothing changed (the
// planner's noise), and IfCut at a 20% cut in arm B's cost; both are null for other methods, whose expected spend is Max.
type spendDoc struct {
	Known         bool     `json:"known"`
	ExpectedUSD   *float64 `json:"expected_usd"`
	ExpectedTasks *float64 `json:"expected_tasks"`
	IfCutUSD      *float64 `json:"if_cut_usd"`
	MaxUSD        *float64 `json:"max_usd"`
	WorstCaseUSD  float64  `json:"worst_case_usd"`
}

// lookPlanDoc is one planned look: what has been run and spent by then, at most.
type lookPlanDoc struct {
	Look             int      `json:"look"`
	Tasks            int      `json:"tasks"`
	Runs             int      `json:"runs"` // both arms
	EstimatedUSD     *float64 `json:"estimated_usd"`
	WorstCaseUSD     float64  `json:"worst_case_usd"`
	EfficacyLevel    float64  `json:"efficacy_level"`
	EquivalenceLevel float64  `json:"equivalence_level"`
}

// sizePlanDoc is a size a fixed design (a success experiment) could have: a tier, or the design itself.
type sizePlanDoc struct {
	Name         string   `json:"name"`
	Tasks        int      `json:"tasks"`
	Repeats      int      `json:"repeats_per_arm"`
	Runs         int      `json:"runs"`
	CostUSD      *float64 `json:"cost_usd"` // the agents' expected cost, with the judge's in JudgeUSD
	JudgeUSD     float64  `json:"judge_usd"`
	WorstCaseUSD float64  `json:"worst_case_usd"`
	Short        bool     `json:"short"` // fewer tasks are eligible than the size asks for
}

func ptr[T any](v T) *T { return &v }

// spendPlan is the experiment's spend and planned looks (seq-v1) or sizes (other methods).
func spendPlan(review experiment.Review) (spend spendDoc, looks []lookPlanDoc, sizes []sizePlanDoc, err error) {
	looks, sizes = []lookPlanDoc{}, []sizePlanDoc{}
	d := review.Design
	if d.Sequential() && len(d.Tasks) > 0 {
		p, err := experiment.PreviewSequential(d, review.Estimates)
		if err != nil {
			return spend, looks, sizes, err
		}
		spend = spendDoc{Known: p.Known, WorstCaseUSD: p.WorstUSD, ExpectedTasks: ptr(p.TasksNone)}
		if p.Known {
			spend.ExpectedUSD, spend.IfCutUSD, spend.MaxUSD = ptr(p.NoneUSD), ptr(p.CutUSD), ptr(p.MaxUSD)
		}
		for k, l := range p.Looks {
			doc := lookPlanDoc{Look: k + 1, Tasks: l.Tasks, Runs: l.Runs, WorstCaseUSD: l.WorstUSD, EfficacyLevel: l.EffLevel, EquivalenceLevel: l.EqLevel}
			if p.Known {
				doc.EstimatedUSD = ptr(l.CostUSD)
			}
			looks = append(looks, doc)
		}
		return spend, looks, sizes, nil
	}
	for _, r := range review.Rows {
		doc := sizePlanDoc{Name: r.Name, Tasks: r.Tasks, Repeats: r.Repeats, Runs: r.Runs, JudgeUSD: r.JudgeUSD, WorstCaseUSD: r.WorstUSD, Short: r.Short}
		if r.CostKnown {
			doc.CostUSD = ptr(r.CostUSD)
		}
		sizes = append(sizes, doc)
	}
	if n := len(review.Rows); n > 0 { // the last row is the design itself
		own := review.Rows[n-1]
		spend = spendDoc{Known: own.CostKnown, WorstCaseUSD: own.WorstUSD}
		if own.CostKnown {
			spend.ExpectedUSD, spend.MaxUSD = ptr(own.CostUSD+own.JudgeUSD), ptr(own.CostUSD+own.JudgeUSD)
		}
	}
	return spend, looks, sizes, nil
}

// --- experiment new ---

type experimentNewDoc struct {
	header
	Experiment    experimentDoc `json:"experiment"`
	EligibleTasks int           `json:"eligible_tasks"` // how many tasks could have been in it
	Notes         []string      `json:"notes"`
	PlanCommand   string        `json:"plan_command"`
}

func newDocument(name string, c experiment.Created) experimentNewDoc {
	doc := experimentNewDoc{header: hdr("experiment new"), Experiment: experimentOf(name, c.Design), EligibleTasks: c.Eligible, Notes: []string{},
		PlanCommand: "agentium experiment plan " + name}
	if !c.Explicit && c.Eligible < c.Tier.Tasks {
		doc.Notes = append(doc.Notes, fmt.Sprintf("the %s tier asks for %d tasks; only %d can be in it", c.Tier.Name, c.Tier.Tasks, c.Eligible))
	}
	return doc
}

// --- experiment plan ---

type ineligibleDoc struct {
	Task   string `json:"task"`
	Reason string `json:"reason"` // human text
}

type experimentPlanDoc struct {
	header
	Experiment     experimentDoc   `json:"experiment"`
	Ready          bool            `json:"ready"`
	Readiness      []readinessDoc  `json:"readiness"`
	Calibrations   int             `json:"calibration_runs_needed"`
	CalibrationUSD float64         `json:"calibration_estimate_usd"`
	EligibleTasks  []string        `json:"eligible_tasks"`
	Ineligible     []ineligibleDoc `json:"ineligible_tasks"`
	Spend          spendDoc        `json:"spend"`
	Looks          []lookPlanDoc   `json:"looks"` // seq-v1 designs; [] for others
	Sizes          []sizePlanDoc   `json:"sizes"` // other designs; [] for seq-v1
}

func planDocument(env Env, name string, review experiment.Review) (experimentPlanDoc, error) {
	spend, looks, sizes, err := spendPlan(review)
	if err != nil {
		return experimentPlanDoc{}, err
	}
	doc := experimentPlanDoc{header: hdr("experiment plan"), Experiment: experimentOf(name, review.Design), Ready: review.Readiness.Ready,
		Readiness: []readinessDoc{}, Calibrations: len(review.Readiness.Calibrations), EligibleTasks: list(slices.Clone(review.Eligible)),
		Ineligible: []ineligibleDoc{}, Spend: spend, Looks: looks, Sizes: sizes}
	doc.CalibrationUSD, _ = experiment.CalibrationCosts(review.Readiness.Calibrations)
	for _, c := range review.Readiness.Checks {
		doc.Readiness = append(doc.Readiness, readinessDoc{Status: readinessStatus(c.Status), Text: env.cleanText(c.Text)})
	}
	for task, reason := range review.Reasons {
		doc.Ineligible = append(doc.Ineligible, ineligibleDoc{Task: task, Reason: env.redact(reason)})
	}
	slices.SortFunc(doc.Ineligible, func(a, b ineligibleDoc) int { return cmp.Compare(a.Task, b.Task) })
	return doc, nil
}

// cleanText redacts free text and names Claude Code's location "claude": its path is not a document's business.
func (env Env) cleanText(text string) string {
	if cli, err := claudePath(env); err == nil {
		text = replacePath(text, cli, "claude")
	}
	return env.redact(text)
}

// --- experiment show ---

type budgetChangeDoc struct {
	At      time.Time `json:"at"`
	FromUSD float64   `json:"from_usd"`
	ToUSD   float64   `json:"to_usd"`
}

type lockDoc struct {
	LockedAt      time.Time         `json:"locked_at"`
	ClaudeCode    string            `json:"claude_code"` // the version every run must report
	SignIn        string            `json:"sign_in"`
	Method        string            `json:"method"`
	PriceTable    string            `json:"price_table"`
	LocalBinding  bool              `json:"local_binding"`
	BudgetChanges []budgetChangeDoc `json:"budget_changes"`
}

type armProgressDoc struct {
	Name      string  `json:"name"`
	Context   string  `json:"context"`
	Settled   int     `json:"settled"`
	Fair      int     `json:"fair"`
	Successes int     `json:"successes"`
	Unfair    int     `json:"unfair"`
	Infra     int     `json:"infra"`
	Cancelled int     `json:"cancelled"`
	CostUSD   float64 `json:"cost_usd"` // the agent's alone
}

// progressDoc is where a locked experiment stands. Slots are the schedule's runs (both arms); Settled counts slots with
// a settled run; SpentUSD is all the budget counts, the judge's and the calibrations' included.
type progressDoc struct {
	Slots          int              `json:"slots"`
	Settled        int              `json:"settled"`
	SpentUSD       float64          `json:"spent_usd"`
	BudgetUSD      float64          `json:"budget_usd"`
	JudgeUSD       float64          `json:"judge_usd"`
	CalibrationUSD float64          `json:"calibration_usd"`
	UnjudgedRuns   int              `json:"unjudged_runs"`
	Arms           []armProgressDoc `json:"arms"`
	Looks          []lookDoc        `json:"looks"`
	EndedBy        *string          `json:"ended_by"` // seq-v1: stop | futility | final once it has ended
}

func progressOf(p experiment.Progress) *progressDoc {
	doc := &progressDoc{Slots: p.Slots, Settled: p.Settled, SpentUSD: p.SpentUSD, BudgetUSD: p.BudgetUSD, JudgeUSD: p.JudgeUSD,
		CalibrationUSD: p.CalibrationUSD, UnjudgedRuns: p.UnjudgedRuns, Arms: []armProgressDoc{}, Looks: looksOf(p.Sequential)}
	for _, a := range p.Arms {
		doc.Arms = append(doc.Arms, armProgressDoc{Name: a.Name, Context: a.Context, Settled: a.Settled, Fair: a.Fair, Successes: a.Successes,
			Unfair: a.Unfair, Infra: a.Infra, Cancelled: a.Cancelled, CostUSD: a.CostUSD})
	}
	if s := p.Sequential; s != nil && s.Ended != "" {
		doc.EndedBy = &s.Ended
	}
	return doc
}

type experimentShowDoc struct {
	header
	Experiment experimentDoc `json:"experiment"`
	Status     string        `json:"status"` // draft | running | stopped | budget | usage | done
	StatusNote string        `json:"status_note"`
	Locked     bool          `json:"locked"`
	Lock       *lockDoc      `json:"lock"`     // null before the first run
	Progress   *progressDoc  `json:"progress"` // null before the first run
	// CalibrationUSD is what the calibration runs before the lock spent (they are in the budget).
	CalibrationUSD float64 `json:"calibration_usd"`
}

func lockOf(l experiment.Lock) *lockDoc {
	doc := &lockDoc{LockedAt: l.LockedAt, ClaudeCode: l.ClaudeCode, SignIn: l.SignIn, Method: l.Method, PriceTable: l.PriceTable,
		LocalBinding: l.LocalBinding, BudgetChanges: []budgetChangeDoc{}}
	for _, c := range l.BudgetChanges {
		doc.BudgetChanges = append(doc.BudgetChanges, budgetChangeDoc{At: c.At, FromUSD: c.From, ToUSD: c.To})
	}
	return doc
}

// storedStatus is the status an experiment shows: a "running" one whose process ended is stopped.
func storedStatus(w *workspace, e store.Experiment) string {
	if e.Status == store.StatusRunning && !w.layout.RunsBusy() {
		return store.StatusStopped
	}
	return e.Status
}

// --- experiment list ---

type listArmDoc struct {
	Name    string `json:"name"`
	Context string `json:"context"`
}

type experimentListEntry struct {
	Name          string       `json:"name"`
	Template      string       `json:"template"`
	Goal          string       `json:"goal"`
	Method        string       `json:"method"`
	Arms          []listArmDoc `json:"arms"`
	Tasks         int          `json:"tasks"`
	RepeatsPerArm int          `json:"repeats_per_arm"`
	Model         string       `json:"model"` // "A = MODEL, B = MODEL" in a model-ab experiment
	BudgetUSD     float64      `json:"budget_usd"`
	Status        string       `json:"status"`
	Created       time.Time    `json:"created"`
}

type experimentListDoc struct {
	header
	Experiments []experimentListEntry `json:"experiments"`
}

type experimentRemoveDoc struct {
	header
	Removed string `json:"removed"`
}

// --- experiment run ---

// runCountsDoc counts an experiment's slots (one run of a pair each): Total is Settled + Pending + Skipped + Failed.
type runCountsDoc struct {
	Total   int `json:"total"`
	Settled int `json:"settled"`
	Pending int `json:"pending"` // would run on a resume
	Skipped int `json:"skipped"` // a seq-v1 experiment chose not to run them: it ended at a look
	Failed  int `json:"failed"`  // out of attempts: a resume does not retry them
}

// metricDoc is one metric's comparison of arm B with arm A: a difference for success, a ratio for the others.
type metricDoc struct {
	Metric   string       `json:"metric"` // cost, success, duration, ...
	Role     string       `json:"role"`   // primary | guard | secondary
	Verdict  string       `json:"verdict"`
	Decisive bool         `json:"decisive"` // improved, improved but small, regressed, no loss beyond the margin or equivalent
	Tasks    int          `json:"tasks"`    // with counted runs in both arms
	A        *float64     `json:"a"`
	B        *float64     `json:"b"`
	Interval *intervalDoc `json:"interval"` // the one the verdict reads, at Level
	Level    *float64     `json:"level"`
	Note     string       `json:"note"`
}

// verdictDoc is the experiment's verdicts so far. Decisive is true when the primary metric or the guard has a decisive
// verdict, the north star's rule. Summary is human text.
type verdictDoc struct {
	Decisive bool        `json:"decisive"`
	Summary  string      `json:"summary"`
	Metrics  []metricDoc `json:"metrics"`
}

// verdictInterval is the interval a verdict rests on: the wider of the bootstrap and the t-interval, at 90% for "no
// loss" and "equivalent" and at 95% otherwise; a seq-v1 look's primary metric at the look's levels.
func verdictInterval(r experiment.MetricResult) (stats.Interval, float64) {
	a, b, level := r.Boot95, r.T95, 0.95
	if r.Level > 0 {
		level = r.Level
	}
	if r.Verdict == stats.NoLoss || r.Verdict == stats.Equivalent {
		a, b, level = r.Boot90, r.T90, 0.90
		if r.EqLevel > 0 {
			level = r.EqLevel
		}
	}
	return stats.Interval{Estimate: a.Estimate, Low: math.Min(a.Low, b.Low), High: math.Max(a.High, b.High)}, level
}

func verdictOf(env Env, a experiment.Analysis) *verdictDoc {
	doc := &verdictDoc{Metrics: []metricDoc{}}
	if a.Sequential != nil {
		doc.Summary = env.redact(a.Sequential.Describe())
	}
	for _, r := range a.Results {
		m := metricDoc{Metric: r.Metric, Role: r.Role, Verdict: r.Verdict, Decisive: report.Decisive(r.Verdict), Tasks: r.Tasks, A: finite(r.A), B: finite(r.B), Note: env.redact(r.Note)}
		if r.Tasks >= 2 { // with fewer, there is no interval
			i, level := verdictInterval(r)
			m.Interval, m.Level = intervalOf(&i), finiteOf(level)
		}
		if m.Decisive && r.Role != experiment.RoleSecondary {
			doc.Decisive = true
		}
		doc.Metrics = append(doc.Metrics, m)
	}
	return doc
}

// runResultDoc is how an experiment run ended. Status:
//   - done: every slot settled, or a seq-v1 experiment ended at a look (EndedBy stop, futility or final);
//   - budget: the next run would not fit the budget; a higher --budget continues it;
//   - usage: paused before the subscription's usage limit (or the judge hit it); ResumeAt says when it resets;
//   - stopped: interrupted, repeated infrastructure failures, a changed environment, or an error (exit 1);
//   - refused: nothing ran, because `--json` needs `--yes` to spend money (exit 1). Only Status, Note and NextCommand
//     mean anything then: the rest is null or empty.
//
// Looks, Verdict, the counts and the spend are the experiment's, as stored when the command ended.
type runResultDoc struct {
	Status        string        `json:"status"`
	Note          string        `json:"note"` // human text
	Method        *string       `json:"method"`
	EndedBy       *string       `json:"ended_by"`        // seq-v1: stop | futility | final once it has ended
	StoppedAtLook *int          `json:"stopped_at_look"` // when it ended early at a look (stop or futility)
	Looks         []lookDoc     `json:"looks"`
	SpentUSD      *float64      `json:"spent_usd"` // all the budget counts, calibrations and the judge included
	BudgetUSD     *float64      `json:"budget_usd"`
	Runs          *runCountsDoc `json:"runs"`
	ResumeAt      *time.Time    `json:"resume_at"` // status usage: when the window resets
	JudgePaused   bool          `json:"judge_paused"`
	Verdict       *verdictDoc   `json:"verdict"`      // null when nothing was analysed (no runs)
	NorthStar     *northStarDoc `json:"north_star"`   // the project's time and spend to its first decisive verdict
	NextCommand   string        `json:"next_command"` // what continues the experiment, or reports it
}

type experimentRunDoc struct {
	header
	Experiment string       `json:"experiment"`
	Run        runResultDoc `json:"run"`
}

// refusedRun is the result of an `experiment run --json` without --yes: next_command is the command that would run,
// with the flags it was given.
func refusedRun(name string, o experiment.RunOptions) runResultDoc {
	next := "agentium experiment run " + name + " --yes --json"
	if o.Budget > 0 {
		next += " --budget " + strconv.FormatFloat(o.Budget, 'f', -1, 64)
	}
	if o.UsageLimit != experiment.DefaultUsageLimit {
		next += " --usage-limit " + strconv.FormatFloat(o.UsageLimit, 'f', -1, 64)
	}
	if o.Wait {
		next += " --wait"
	}
	return runResultDoc{Status: "refused", Note: "--json never asks and starts no paid run without --yes: add --yes to run the experiment (real Claude Code runs, within its budget)",
		Looks: []lookDoc{}, NextCommand: next}
}

// runResultOf describes how an execution ended, from what is stored.
//
// It runs after the money was spent, even after an interrupt: callers pass a context that is not cancelled, and what
// cannot be analysed leaves the verdict null instead of failing the document.
func runResultOf(ctx context.Context, env Env, w *workspace, name string, out experiment.RunOutcome) (runResultDoc, error) {
	stored, err := w.db.ExperimentByName(ctx, w.project.ID, name)
	if err != nil {
		return runResultDoc{}, err
	}
	var lock experiment.Lock
	if err := json.Unmarshal(stored.Lock, &lock); err != nil {
		return runResultDoc{}, fmt.Errorf("experiment %s: its lock cannot be read: %w", name, err)
	}
	progress, err := w.service().LoadProgress(ctx, name, stored.ID, lock)
	if err != nil {
		return runResultDoc{}, err
	}
	runs := runCountsDoc{Total: progress.Slots, Settled: progress.Settled, Failed: progress.Failed, Pending: progress.Slots - progress.Settled - progress.Failed}
	res := runResultDoc{Status: out.Status, Note: env.redact(out.Note), Method: &lock.Method, Looks: looksOf(progress.Sequential), SpentUSD: finiteOf(progress.SpentUSD),
		BudgetUSD: finiteOf(progress.BudgetUSD), Runs: &runs, JudgePaused: out.JudgePaused, NextCommand: "agentium experiment run " + name}
	if !out.ResumeAt.IsZero() {
		res.ResumeAt = &out.ResumeAt
	}
	if s := progress.Sequential; s != nil && s.Ended != "" {
		res.EndedBy = &s.Ended
		if l := s.ReportedLook(); l != nil && (s.Ended == experiment.LookStop || s.Ended == experiment.LookFutility) {
			res.StoppedAtLook = &l.Look
		}
	}
	if res.EndedBy != nil { // the experiment ended at a look: what is left was skipped, not pending
		runs.Skipped, runs.Pending = runs.Pending, 0
	}
	switch {
	case out.Status == experiment.StatusDone:
		res.NextCommand = "agentium experiment report " + name
	case out.Status == experiment.StatusBudget:
		res.NextCommand += " --budget USD"
	case out.Status == experiment.StatusUsage && out.ResumeAt.IsZero() && !out.JudgePaused:
		res.NextCommand += " --usage-limit PCT" // a pair needs more of the window than the limit allows
	}
	// What cannot be analysed leaves the verdict null, and the note says why: the money was spent, the document is written.
	noVerdict := func(err error) {
		res.Note = strings.TrimSpace(res.Note + "; the verdict could not be computed: " + env.redact(err.Error()))
		res.Note = strings.TrimPrefix(res.Note, "; ")
	}
	if stored, err := w.db.ExperimentRuns(ctx, stored.ID); err != nil {
		noVerdict(err)
	} else if len(stored) > 0 {
		data, err := experiment.RunDataOfStored(stored)
		if err == nil {
			var analysis experiment.Analysis
			if analysis, err = experiment.Analyze(lock, data); err == nil {
				res.Verdict = verdictOf(env, analysis)
			}
		}
		if err != nil {
			noVerdict(err)
		}
	}
	if star, err := report.LoadNorthStar(ctx, w.service()); err == nil {
		res.NorthStar = northStarOf(star)
	}
	return res, nil
}

// runExitCode is the exit code of a run that ended with status, as in human mode: a stop that a resume cannot lift
// without looking (an error, an interrupt, a changed environment) is a failure; done, the budget and a usage pause are not.
func runExitCode(status string) int {
	if status == experiment.StatusDone || status == experiment.StatusBudget || status == experiment.StatusUsage {
		return ExitOK
	}
	return ExitError
}
