// Package experiment designs context experiments and previews them: which arms, tasks and repeats, what they may
// cost and what they can detect. An experiment fixes all of this before its first run (the study's §5.6).
package experiment

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/task"
)

// Templates.
const (
	TemplateContextAB = "context-ab" // two contexts: does B change results against A?
	TemplateAA        = "aa"         // one context in both arms: measures the noise, and must find no difference
	// TemplateModelAB compares two Claude Code profiles (a model and an effort level) on the same tasks and one context.
	TemplateModelAB = "model-ab"
)

// Efforts are the effort levels Claude Code's --effort takes; a model-ab arm names one of them or none (the CLI's own).
// A fresh slice, so callers cannot change the set.
func Efforts() []string { return []string{"low", "medium", "high", "xhigh", "max"} }

// Goals pick the primary metric.
const (
	GoalCheaper = "cheaper" // cost is primary; success is the guard that must not drop beyond its margin
	GoalBetter  = "better"  // success is primary
)

// BaseContext names the task base's own context, as the other commands do.
const BaseContext = "base"

// Arm is one side of an experiment.
type Arm struct {
	Name     string `json:"name"`               // A or B
	Context  string `json:"context"`            // BaseContext or a snapshot name
	Snapshot string `json:"snapshot,omitempty"` // the snapshot commit; empty for the base's own context
	// Model, Effort and RunBudgetUSD are a model-ab arm's own profile; context templates leave them empty. An arm with a
	// Model runs on it at its Effort (empty: the CLI's default), not Design.Effort. The JSON name of Model is not "model":
	// a LockedArm embeds Arm and records the model its calibration saw under that name. A zero RunBudgetUSD is the
	// design's.
	Model        string  `json:"requested_model,omitempty"`
	Effort       string  `json:"effort,omitempty"`
	RunBudgetUSD float64 `json:"run_budget_usd,omitempty"`
}

// DesignVersion is the version of a context experiment's stored form. A model-ab design is stored as
// DesignVersionModelAB: an older Agentium, which would run both arms on arm A's model, refuses it. A seq-v1 design, of
// any template, is stored as DesignVersionSeq: an older Agentium, which would run its 16 tasks as one fixed design at
// 95%, refuses it. A design that grades in the sandbox, of any template and method, is stored as DesignVersionSandbox:
// an older Agentium, which would grade its runs on the host, refuses it. A design with judge-graded tasks, of any
// template, method and grader, is stored as DesignVersionJudge: an older Agentium, which would grade those tasks' runs
// by their verification commands as if they were tests, refuses it.
const (
	DesignVersion        = 1
	DesignVersionModelAB = 2
	DesignVersionSeq     = 3
	DesignVersionSandbox = 4
	DesignVersionJudge   = 5
)

// WantVersion is the stored version a design of its template, method, grader and tasks carries. Only the sandbox
// has a version of its own. A mode this Agentium does not grade in (container-v1 until the containers plan's step 4
// gives it one) is never stored: Validate refuses its grader.
func (d Design) WantVersion() int {
	switch {
	case len(d.JudgeGraded) > 0:
		return DesignVersionJudge
	case task.GraderOf(d.Grader) == task.GraderSandbox:
		return DesignVersionSandbox
	case d.Method == MethodSeq:
		return DesignVersionSeq
	case d.Template == TemplateModelAB:
		return DesignVersionModelAB
	}
	return DesignVersion
}

// NewMethod is the method a new experiment with goal is made under: seq-v1 for a cost experiment (cheaper), phase1-v2
// for a success experiment (better), since seq-v1 has no sequential success design.
func NewMethod(goal string) string {
	if goal == GoalCheaper {
		return MethodSeq
	}
	return MethodV2
}

// LockMethod is the method d is locked under: its own, or phase1-v2 for a design stored before designs named one.
func (d Design) LockMethod() string {
	if d.Method == "" {
		return MethodV2
	}
	return d.Method
}

// Sequential reports whether d runs as a seq-v1 design.
func (d Design) Sequential() bool { return d.Method == MethodSeq }

// MaxSeed bounds seeds to 53 bits, which JSON numbers carry exactly, even in readers that use doubles (JavaScript, jq).
const MaxSeed = 1<<53 - 1

// Design is what an experiment fixes before its first run.
type Design struct {
	Version       int           `json:"version"`
	Template      string        `json:"template"`
	Arms          []Arm         `json:"arms"`
	Tasks         []string      `json:"tasks"`
	Repeats       int           `json:"repeats"` // runs per task per arm
	Model         string        `json:"model"`
	Effort        string        `json:"effort,omitempty"`
	Goal          string        `json:"goal"`
	CostMargin    float64       `json:"cost_margin"`    // relative: 0.10 is 10%
	SuccessMargin float64       `json:"success_margin"` // absolute: 0.15 is 15 percentage points
	RunBudgetUSD  float64       `json:"run_budget_usd"` // Claude Code stops a run here
	BudgetUSD     float64       `json:"budget_usd"`     // the whole experiment
	Timeout       time.Duration `json:"timeout"`        // one agent run
	VerifyTimeout time.Duration `json:"verify_timeout"` // one setup or verification command
	Concurrency   int           `json:"concurrency"`
	Seed          uint64        `json:"seed"` // the task sample and, when it runs, the schedule
	// Judge, when set, asks the LLM judge about every graded run (internal/judge). Its verdicts sit beside the tests
	// and decide nothing; its cost counts against BudgetUSD but never toward an arm's cost. Omitted when off, so designs
	// and locks made before the judge read and encode as they did.
	Judge *judge.Settings `json:"judge,omitempty"`
	// JudgePairs, when set, has the pair judge compare each pair's two passing runs (a task's run in each arm with the
	// same repeat index) in both orders (judge.JudgePair). Its Repeats is 1: each order is asked once. Unvalidated, its
	// preferences are exploratory and decide nothing; its cost counts against BudgetUSD but never toward an arm's
	// cost. Omitted when off, so designs and locks made before it read and encode as they did.
	JudgePairs *judge.Settings `json:"judge_pairs,omitempty"`
	// Method is the method the design is made for (NewMethod); empty in designs stored before, which lock under
	// phase1-v2 (LockMethod). A seq-v1 design has one run per task and arm and at most 16 tasks.
	Method string `json:"method,omitempty"`
	// NoFutility turns a seq-v1 design's futility stops off (they are on by default, and non-binding either way).
	NoFutility bool `json:"no_futility,omitempty"`
	// Grader is the mode the runs are graded in, which the lock fixes: task.GraderSandbox, or empty for the host (a host
	// design is stored as designs were before modes, and those grade on the host: task.GraderOf). An existing
	// experiment keeps its mode.
	Grader string `json:"grader,omitempty"`
	// JudgeGraded lists the design's judge-graded tasks (task.GradingJudge), in Tasks' order, and JudgeGrading is the
	// judge that grades their runs, by the majority of its repeats (judge.Grade). Their grades are unvalidated: the
	// analysis keeps them apart from the tests' (MetricJudgeSuccess) and rests no verdict on them. Both are omitted when
	// no task is judge-graded, so other designs read and encode as before (WantVersion).
	JudgeGraded  []string        `json:"judge_graded,omitempty"`
	JudgeGrading *judge.Settings `json:"judge_grading,omitempty"`
}

// Default margins (the study's §5.6).
const (
	DefaultCostMargin    = 0.10
	DefaultSuccessMargin = 0.15
)

// MaxConcurrency bounds parallel runs: each is a full agent session with its own checkout and verification.
const MaxConcurrency = 8

// MaxJudgeRepeats bounds the judge's repeats per run: each is a paid call.
const MaxJudgeRepeats = 9

// ArmModel is the model arm a runs on: its own in a model-ab experiment, else the design's.
func (d Design) ArmModel(a Arm) string {
	if a.Model != "" {
		return a.Model
	}
	return d.Model
}

// ArmEffort is arm a's effort level, "" for the CLI's default. An arm with its own model has its own effort, so a
// default effort does not leak into the other arm.
func (d Design) ArmEffort(a Arm) string {
	if a.Model != "" {
		return a.Effort
	}
	return d.Effort
}

// ArmRunBudgetUSD is the cost Claude Code stops one of arm a's runs at.
func (d Design) ArmRunBudgetUSD(a Arm) float64 {
	if a.RunBudgetUSD > 0 {
		return a.RunBudgetUSD
	}
	return d.RunBudgetUSD
}

// ArmRunCapUSD is what one of arm a's runs of a test-graded task may spend at most: the agent's cap, the turn that may
// cross it on the arm's model (claude.CapOvershootUSD), and its judgement's cap. TaskRunCapUSD is any task's.
func (d Design) ArmRunCapUSD(a Arm) float64 {
	return d.ArmRunBudgetUSD(a) + claude.CapOvershootUSD(d.ArmRunBudgetUSD(a), d.ArmModel(a)) + d.JudgeCapUSD()
}

// IsJudgeGraded reports whether the judge grades the design's task of that name.
func (d Design) IsJudgeGraded(name string) bool { return slices.Contains(d.JudgeGraded, name) }

// judgedTasks counts the design's tasks that are judge-graded.
func (d Design) judgedTasks() int {
	n := 0
	for _, t := range d.Tasks {
		if d.IsJudgeGraded(t) {
			n++
		}
	}
	return n
}

// GradingCapUSD is what grading one judge-graded run may spend at most: each of JudgeGrading's calls at its cap
// (judge.CallCapFor, the overshoot allowance included for any but the default judge), twice, since a malformed reply is
// asked again. Zero without judge-graded tasks.
func (d Design) GradingCapUSD() float64 {
	if d.JudgeGrading == nil || len(d.JudgeGraded) == 0 {
		return 0
	}
	return judge.CapUSD(*d.JudgeGrading)
}

// judgedRunCapUSD is what one of arm a's runs of a judge-graded task may spend at most: the agent's cap and overshoot,
// and its grading's cap (it is given no second-opinion judgement).
func (d Design) judgedRunCapUSD(a Arm) float64 {
	return d.ArmRunBudgetUSD(a) + claude.CapOvershootUSD(d.ArmRunBudgetUSD(a), d.ArmModel(a)) + d.GradingCapUSD()
}

// TaskRunCapUSD is what one of arm a's runs of the named task may spend at most: ArmRunCapUSD for a test-graded task,
// the grading's cap in place of the judgement's for a judge-graded one.
func (d Design) TaskRunCapUSD(a Arm, name string) float64 {
	if d.IsJudgeGraded(name) {
		return d.judgedRunCapUSD(a)
	}
	return d.ArmRunCapUSD(a)
}

// SlotCapUSD is TaskRunCapUSD for the arm named arm (the design's first arm's cap for a name it lacks).
func (d Design) SlotCapUSD(arm, name string) float64 {
	for _, a := range d.Arms {
		if a.Name == arm {
			return d.TaskRunCapUSD(a, name)
		}
	}
	return d.RunCapUSD()
}

// ModelLabel is the model of an experiment in words: the design's, or each arm's in a model-ab experiment.
func (d Design) ModelLabel() string {
	if !d.PerArmProfiles() || len(d.Arms) != 2 {
		return d.Model
	}
	return "A = " + Profile(d.Arms[0].Model, d.Arms[0].Effort) + ", B = " + Profile(d.Arms[1].Model, d.Arms[1].Effort)
}

// PerArmProfiles reports whether the arms differ by model or effort (model-ab) rather than by context.
func (d Design) PerArmProfiles() bool { return d.Template == TemplateModelAB }

// Profile is a model and effort as `--a` and `--b` write them: MODEL or MODEL:EFFORT.
func Profile(model, effort string) string {
	if effort == "" {
		return model
	}
	return model + ":" + effort
}

// ParseProfile reads MODEL[:EFFORT]. It checks the effort level only; the model is Claude Code's to accept.
func ParseProfile(s string) (model, effort string, err error) {
	model, effort, _ = strings.Cut(s, ":")
	switch {
	case model == "":
		return "", "", fmt.Errorf("%q names no model (write MODEL or MODEL:EFFORT)", s)
	case strings.Contains(s, ":") && effort == "":
		return "", "", fmt.Errorf("%q names no effort after the colon (write MODEL or MODEL:EFFORT)", s)
	case effort != "" && !slices.Contains(Efforts(), effort):
		return "", "", fmt.Errorf("%q: unknown effort %q (use %s)", s, effort, strings.Join(Efforts(), ", "))
	}
	return model, effort, nil
}

// InferTemplate is the template experiment new reads from its --b: none makes an A/A; a model (IsModel), with or
// without an effort, a model A/B; anything else names a snapshot, for a context A/B.
func InferTemplate(b string) string {
	switch {
	case b == "":
		return TemplateAA
	case IsModel(b):
		return TemplateModelAB
	}
	return TemplateContextAB
}

// IsModel reports whether s, a MODEL[:EFFORT], names a model as experiment new reads --b: one Agentium's price table
// knows, or a name shaped like a Claude model ID (modelID: claude-next-9, claude-opus-6-20270101). Aliases such as
// "sonnet" and other claude-… names such as "claude-rules" are not: they read as a snapshot's name.
func IsModel(s string) bool {
	model, _, _ := strings.Cut(s, ":")
	if _, known := pricing.Lookup(model); known {
		return true
	}
	return modelID.MatchString(model)
}

// modelID is the shape of a Claude model ID: a family and a version of numbers, optionally dated.
var modelID = regexp.MustCompile(`^claude-[a-z]+-\d+(-\d+)*(-\d{8})?$`)

// SameProfile reports whether two MODEL[:EFFORT]s parse to one model and effort. One that does not parse is never the
// same: ParseProfile's error is reported where it is read.
func SameProfile(a, b string) bool {
	modelA, effortA, errA := ParseProfile(a)
	modelB, effortB, errB := ParseProfile(b)
	return errA == nil && errB == nil && modelA == modelB && effortA == effortB
}

// Runs is the number of agent runs the design asks for.
func (d Design) Runs() int { return len(d.Tasks) * d.Repeats * len(d.Arms) }

// Pairs is the number of pairs the design asks for: a task's run in each arm with the same repeat index.
func (d Design) Pairs() int { return len(d.Tasks) * d.Repeats }

// JudgeCapUSD is what one test-graded run's judgement may spend at most: each repeat's call up to judge.CallCapUSD, twice, since a
// malformed reply is asked again. Zero without the judge. Claude Code checks --max-budget-usd after a turn, so a call
// can pass its cap a little. A judge call has no tools and few turns, and on the default judge (judge.DefaultModel at
// judge.DefaultEffort) CallCapUSD already clears the dearest call measured with room, so it gets no overshoot
// allowance; any other judge model or effort was never measured, so each call also holds the allowance a run on that
// model would (claude.CapOvershootUSD). The reserve is an estimate, as the agent's own cap is.
func (d Design) JudgeCapUSD() float64 {
	if d.Judge == nil {
		return 0
	}
	s := d.Judge.WithDefaults()
	return float64(s.Repeats) * 2 * judgeCallCapUSD(s)
}

// judgeCallCapUSD is what one judge call on s may spend at most: judge.CallCapUSD, with the overshoot allowance of a run
// on its model for any judge but the default one (JudgeCapUSD says why).
func judgeCallCapUSD(s judge.Settings) float64 { return judge.CallCapFor(s) }

// PairJudgeCapUSD is what one pair's comparison may spend at most: judge.PairCalls calls (both orders, each asked again
// after a malformed reply), each at the judge's call cap with the same overshoot allowance as JudgeCapUSD. Zero without
// the pair judge.
func (d Design) PairJudgeCapUSD() float64 {
	if d.JudgePairs == nil {
		return 0
	}
	return judge.PairCalls * judgeCallCapUSD(*d.JudgePairs)
}

// RunCapUSD is what one run may spend at most: the agent's cap, its overshoot (claude.CapOvershootUSD) and its judgement's
// cap, or, with judge-graded tasks, its grading's when that is larger; the larger of the arms' when they differ. Reserve
// holds it back for every run in flight, so spending never passes the budget while each run stays within it.
func (d Design) RunCapUSD() float64 {
	if !d.PerArmProfiles() || len(d.Arms) == 0 { // the arms of a model-ab design may all differ from RunBudgetUSD
		capUSD := d.RunBudgetUSD + claude.CapOvershootUSD(d.RunBudgetUSD, d.Model) + d.JudgeCapUSD()
		if d.judgedTasks() > 0 {
			capUSD = max(capUSD, d.RunBudgetUSD+claude.CapOvershootUSD(d.RunBudgetUSD, d.Model)+d.GradingCapUSD())
		}
		return capUSD
	}
	capUSD := 0.0
	for _, a := range d.Arms {
		capUSD = max(capUSD, d.ArmRunCapUSD(a))
		if d.judgedTasks() > 0 {
			capUSD = max(capUSD, d.judgedRunCapUSD(a))
		}
	}
	return capUSD
}

// PairCapUSD is what a pair of runs of a test-graded task, one per arm, may spend at most: each run's cap with its
// overshoot and judgement, and the pair's comparison (PairJudgeCapUSD). TaskPairCapUSD is any task's.
func (d Design) PairCapUSD() float64 {
	if len(d.Arms) != 2 {
		return 2*d.RunCapUSD() + d.PairJudgeCapUSD()
	}
	return d.ArmRunCapUSD(d.Arms[0]) + d.ArmRunCapUSD(d.Arms[1]) + d.PairJudgeCapUSD()
}

// TaskPairCapUSD is what a pair of runs of the named task may spend at most: PairCapUSD for a test-graded task, each
// run with its grading's cap (TaskRunCapUSD) and the pair's comparison for a judge-graded one.
func (d Design) TaskPairCapUSD(name string) float64 {
	if !d.IsJudgeGraded(name) || len(d.Arms) != 2 {
		return d.PairCapUSD()
	}
	return d.TaskRunCapUSD(d.Arms[0], name) + d.TaskRunCapUSD(d.Arms[1], name) + d.PairJudgeCapUSD()
}

// MaxPairCapUSD is the most any pair of the design's runs may spend: PairCapUSD, or a judge-graded task's pair when
// that is more. A budget must hold at least one.
func (d Design) MaxPairCapUSD() float64 {
	capUSD := d.PairCapUSD()
	for _, t := range d.JudgeGraded {
		if d.IsJudgeGraded(t) && slices.Contains(d.Tasks, t) {
			capUSD = max(capUSD, d.TaskPairCapUSD(t))
		}
	}
	return capUSD
}

// WorstUSD is what every run of the design, its judgement or grading and its pair's comparison would spend at their
// caps: what the budget must allow for.
func (d Design) WorstUSD() float64 {
	judged := d.judgedTasks()
	worst := float64((len(d.Tasks)-judged)*d.Repeats) * d.PairCapUSD() // as a design without judge-graded tasks always had it
	if judged > 0 {
		worst += float64(judged*d.Repeats) * d.TaskPairCapUSD(d.JudgeGraded[0])
	}
	return worst
}

// JudgeEstimateUSD is the judge's expected cost for every test-graded run of d, at the judge pilot's mean cost of a call
// (judge.EstimateUSD): a stated figure, not a measure of this project. Zero without the judge. Judge-graded runs get
// no second opinion: GradingEstimateUSD is theirs.
func (d Design) JudgeEstimateUSD() float64 {
	if d.Judge == nil {
		return 0
	}
	runs := (len(d.Tasks) - d.judgedTasks()) * d.Repeats * len(d.Arms)
	return float64(runs*d.Judge.WithDefaults().Repeats) * judge.EstimateUSD
}

// GradingEstimateUSD is the expected cost of grading every judge-graded run of d (JudgeGrading's calls) at the pilot's
// mean call (judge.EstimateUSD). Zero without judge-graded tasks.
func (d Design) GradingEstimateUSD() float64 {
	if d.JudgeGrading == nil {
		return 0
	}
	runs := d.judgedTasks() * d.Repeats * len(d.Arms)
	return float64(runs*d.JudgeGrading.WithDefaults().Repeats) * judge.EstimateUSD
}

// PairJudgeEstimateUSD is the pair judge's expected cost for every pair of d at the pilot's mean pair
// (judge.PairEstimateUSD), as if both runs of every pair pass: only those are compared. Zero without the pair judge.
func (d Design) PairJudgeEstimateUSD() float64 {
	if d.JudgePairs == nil {
		return 0
	}
	return float64(d.Pairs()) * judge.PairEstimateUSD
}

// JudgingEstimateUSD is the judges' expected cost for d: every run's judgement or grading, and every pair's comparison.
func (d Design) JudgingEstimateUSD() float64 {
	return d.JudgeEstimateUSD() + d.GradingEstimateUSD() + d.PairJudgeEstimateUSD()
}

// Validate checks that the design is complete and consistent.
func (d Design) Validate() error {
	var errs []error
	if d.Version != d.WantVersion() {
		errs = append(errs, fmt.Errorf("design version %d (this Agentium writes %d for a %s experiment)", d.Version, d.WantVersion(), d.Template))
	}
	switch d.Template {
	case TemplateContextAB, TemplateAA, TemplateModelAB:
	default:
		errs = append(errs, fmt.Errorf("unknown template %q (use %s, %s or %s)", d.Template, TemplateContextAB, TemplateAA, TemplateModelAB))
	}
	if len(d.Arms) != 2 || d.Arms[0].Name != "A" || d.Arms[1].Name != "B" {
		errs = append(errs, errors.New("an experiment has two arms, A and B"))
	} else {
		same := d.Arms[0].Context == d.Arms[1].Context && d.Arms[0].Snapshot == d.Arms[1].Snapshot
		switch {
		case d.Template == TemplateContextAB && same:
			errs = append(errs, fmt.Errorf("both arms use context %s: a comparison of one context with itself is an A/A (%s), made without --b", d.Arms[0].Context, TemplateAA))
		case d.Template == TemplateAA && !same:
			errs = append(errs, fmt.Errorf("an %s experiment uses one context in both arms", TemplateAA))
		case d.Template == TemplateModelAB && !same:
			errs = append(errs, fmt.Errorf("a %s experiment uses one context in both arms", TemplateModelAB))
		}
		errs = append(errs, d.validateProfiles()...)
		for _, a := range d.Arms {
			if (a.Context == BaseContext) != (a.Snapshot == "") {
				errs = append(errs, fmt.Errorf("arm %s: context %q and snapshot %q do not match", a.Name, a.Context, a.Snapshot))
			}
		}
	}
	if len(d.Tasks) == 0 {
		errs = append(errs, errors.New("no tasks"))
	}
	for i, t := range d.Tasks {
		if slices.Contains(d.Tasks[:i], t) {
			errs = append(errs, fmt.Errorf("task %s is listed twice", t))
		}
	}
	if d.Repeats < 1 {
		errs = append(errs, errors.New("repeats must be at least 1"))
	}
	if d.Model == "" {
		errs = append(errs, errors.New("no model"))
	}
	switch d.Goal {
	case GoalCheaper, GoalBetter:
	default:
		errs = append(errs, fmt.Errorf("unknown goal %q (use %s or %s)", d.Goal, GoalCheaper, GoalBetter))
	}
	if d.CostMargin <= 0 || d.CostMargin >= 1 || d.SuccessMargin <= 0 || d.SuccessMargin >= 1 {
		errs = append(errs, errors.New("margins must lie between 0 and 1"))
	}
	if d.RunBudgetUSD <= 0 || d.BudgetUSD <= 0 {
		errs = append(errs, errors.New("budgets must be positive"))
	} else if pair := d.MaxPairCapUSD(); d.BudgetUSD < pair {
		errs = append(errs, fmt.Errorf("the budget $%.2f is below one pair of runs at their caps ($%.2f)", d.BudgetUSD, pair))
	}
	if d.Timeout <= 0 || d.VerifyTimeout <= 0 {
		errs = append(errs, errors.New("timeouts must be positive"))
	}
	if d.Concurrency < 1 || d.Concurrency > MaxConcurrency {
		errs = append(errs, fmt.Errorf("concurrency must be 1 to %d", MaxConcurrency))
	}
	if d.Seed > MaxSeed {
		errs = append(errs, fmt.Errorf("the seed must be at most %d", uint64(MaxSeed)))
	}
	errs = append(errs, d.validateMethod()...)
	if !task.KnownGrader(d.Grader) {
		errs = append(errs, task.UnknownGrader(d.Grader))
	}
	if j := d.Judge; j != nil {
		if j.Repeats < 1 || j.Repeats > MaxJudgeRepeats {
			errs = append(errs, fmt.Errorf("the judge's repeats must be 1 to %d", MaxJudgeRepeats))
		}
		if j.Model == "" || j.Effort == "" {
			errs = append(errs, errors.New("the judge needs a model and an effort"))
		}
	}
	errs = append(errs, d.validateJudgeGraded()...)
	if j := d.JudgePairs; j != nil {
		if j.Repeats != 1 {
			errs = append(errs, errors.New("the pair judge asks each order once (repeats 1)"))
		}
		if j.Model == "" || j.Effort == "" {
			errs = append(errs, errors.New("the pair judge needs a model and an effort"))
		}
	}
	return errors.Join(errs...)
}

// validateJudgeGraded checks the judge-graded tasks: each is one of the design's tasks, once, and the judge that grades
// them is given whenever there are some, and only then.
func (d Design) validateJudgeGraded() []error {
	var errs []error
	for i, t := range d.JudgeGraded {
		switch {
		case !slices.Contains(d.Tasks, t):
			errs = append(errs, fmt.Errorf("judge-graded task %s is not one of the experiment's tasks", t))
		case slices.Contains(d.JudgeGraded[:i], t):
			errs = append(errs, fmt.Errorf("judge-graded task %s is listed twice", t))
		}
	}
	switch j := d.JudgeGrading; {
	case j == nil && len(d.JudgeGraded) > 0:
		errs = append(errs, errors.New("judge-graded tasks need the judge that grades them"))
	case j != nil && len(d.JudgeGraded) == 0:
		errs = append(errs, errors.New("a grading judge without judge-graded tasks"))
	case j != nil && (j.Repeats < 1 || j.Repeats > MaxJudgeRepeats || j.Model == "" || j.Effort == ""):
		errs = append(errs, fmt.Errorf("the grading judge needs a model, an effort and 1 to %d repeats", MaxJudgeRepeats))
	}
	return errs
}

// validateMethod checks the design against its method: a seq-v1 design is a cost experiment of one run per task and
// arm, with at most stats.SeqMaxTasks tasks.
func (d Design) validateMethod() []error {
	switch d.Method {
	case "", MethodV2:
		if d.NoFutility {
			return []error{fmt.Errorf("futility stops belong to method %s", MethodSeq)}
		}
		return nil
	case MethodSeq:
	default:
		return []error{fmt.Errorf("unknown method %q (new experiments use %s or %s)", d.Method, MethodSeq, MethodV2)}
	}
	var errs []error
	if d.Goal != GoalCheaper {
		errs = append(errs, fmt.Errorf("method %s is for cost experiments (goal %s); success experiments use %s", MethodSeq, GoalCheaper, MethodV2))
	}
	if d.Repeats != 1 {
		errs = append(errs, fmt.Errorf("method %s runs each task once per arm, not %d times", MethodSeq, d.Repeats))
	}
	if len(d.Tasks) > stats.SeqMaxTasks {
		errs = append(errs, fmt.Errorf("method %s takes at most %d tasks, not %d", MethodSeq, stats.SeqMaxTasks, len(d.Tasks)))
	}
	return errs
}

// validateProfiles checks the arms' own profiles: a model-ab experiment gives each arm a model, the profiles differ,
// and efforts are known levels; context templates give none.
func (d Design) validateProfiles() []error {
	var errs []error
	for _, a := range d.Arms {
		if d.Template != TemplateModelAB {
			if a.Model != "" || a.Effort != "" || a.RunBudgetUSD != 0 {
				errs = append(errs, fmt.Errorf("arm %s: a %s experiment compares contexts, so its arms take no model, effort or run budget of their own (a model A/B does)",
					a.Name, d.Template))
			}
			continue
		}
		switch {
		case a.Model == "":
			errs = append(errs, fmt.Errorf("arm %s: a %s experiment gives each arm a model", a.Name, TemplateModelAB))
		case a.Effort != "" && !slices.Contains(Efforts(), a.Effort):
			errs = append(errs, fmt.Errorf("arm %s: unknown effort %q (use %s)", a.Name, a.Effort, strings.Join(Efforts(), ", ")))
		case a.RunBudgetUSD < 0:
			errs = append(errs, fmt.Errorf("arm %s: the run budget must be positive", a.Name))
		}
	}
	if d.Template == TemplateModelAB && len(d.Arms) == 2 && d.Arms[0].Model == d.Arms[1].Model && d.Arms[0].Effort == d.Arms[1].Effort {
		errs = append(errs, fmt.Errorf("both arms run %s: a comparison of one profile with itself measures noise, not a difference (an A/A, without --b, does that for a context)",
			Profile(d.Arms[0].Model, d.Arms[0].Effort)))
	}
	return errs
}

// Candidate is a stored task as an experiment sees it.
type Candidate struct {
	Name        string
	NeedsReview bool
	Grading     string           // task.GradingTests (or "") or task.GradingJudge
	Validation  *task.Validation // nil when never validated
}

// Ineligible says why a task cannot be in an experiment with these arms and grader mode, or returns "" when it can: it
// must be reviewed for solution leaks, and its hidden tests must fail on the base and pass with the reference in every
// arm's context (the last `task validate`), in the experiment's mode. A task validated in another mode is refused by a
// host experiment; a sandbox experiment takes it, and validates it again in the sandbox when it locks
// (NeedsRevalidation). A judge-graded task has no hidden tests: its last validation must find its instruction and its
// reference's code diff fit for the judge (task.ValidateJudged), in any context and mode, since nothing runs.
func Ineligible(c Candidate, arms []Arm, grader string) string {
	if c.NeedsReview {
		return fmt.Sprintf("its instruction needs a review for solution leaks (then: agentium task edit %s --reviewed)", c.Name)
	}
	v := c.Validation
	if c.Grading == task.GradingJudge {
		switch {
		case v == nil || v.Judge == nil:
			return "not validated (agentium task validate " + c.Name + ")"
		case v.Status != task.StatusValid:
			return "its validation failed: " + strings.Join(v.Judge.Problems, "; ")
		}
		return ""
	}
	switch {
	case v == nil:
		return "not validated" + validateHint(c.Name, arms)
	case v.Status == task.StatusUnchecked:
		return "it has no solution to check with, so its hidden tests are unproven"
	case v.Status == task.StatusFlaky:
		return "its validation is " + v.Summary() + " (make the check deterministic, then revalidate: " + validateCommand(c.Name, arms) + " --repeat 3)"
	case v.Status != task.StatusValid:
		return "its validation failed: " + v.Summary()
	}
	for _, a := range arms {
		if !slices.ContainsFunc(v.Arms, func(va task.Arm) bool { return va.Snapshot == a.Snapshot }) {
			return fmt.Sprintf("not validated in context %s", a.Context) + validateHint(c.Name, arms)
		}
	}
	if !task.KnownGrader(grader) { // container-v1 too, until the containers plan's step 4: no task is eligible for a mode not graded in
		return fmt.Sprintf("this Agentium does not grade in %s, so no task is ready for it", grader)
	}
	if mode := task.GraderOf(grader); task.GraderOf(v.Grader) != mode && mode == task.GraderHost {
		return fmt.Sprintf("it was validated %s, but this experiment grades on the host (%s --grader host)", task.DescribeGrader(v.Grader),
			validateCommand(c.Name, arms))
	}
	return ""
}

// NeedsRevalidation reports whether a task an experiment in mode grader may take was validated in another mode, so the
// experiment validates it again in its own when it locks (the isolation plan's decision 5: time, no money). Only a
// sandbox experiment does that; a host one refuses such a task (Ineligible).
func NeedsRevalidation(c Candidate, grader string) bool {
	mode := task.GraderOf(grader)
	return c.Grading != task.GradingJudge && c.Validation != nil && mode == task.GraderSandbox && task.GraderOf(c.Validation.Grader) != mode
}

func validateHint(name string, arms []Arm) string {
	return " (" + validateCommand(name, arms) + ")"
}

// validateCommand is the task validate command that covers every arm's context.
func validateCommand(name string, arms []Arm) string {
	hint := "agentium task validate " + name
	var named []string
	for _, a := range arms {
		if a.Context != BaseContext && !slices.Contains(named, a.Context) {
			named = append(named, a.Context)
			hint += " --snapshot " + a.Context
		}
	}
	return hint
}

// Sample picks n of names with a seeded shuffle, returned sorted; all of them when there are no more than n.
func Sample(names []string, n int, seed uint64) []string {
	picked := slices.Sorted(slices.Values(names))
	if n < len(picked) {
		r := rand.New(rand.NewPCG(seed, 0))
		r.Shuffle(len(picked), func(i, j int) { picked[i], picked[j] = picked[j], picked[i] })
		picked = picked[:n]
		slices.Sort(picked)
	}
	return picked
}
