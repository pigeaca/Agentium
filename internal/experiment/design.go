// Package experiment designs context experiments and previews them: which arms, tasks and repeats, what they may
// cost and what they can detect. An experiment fixes all of this before its first run (the study's §5.6).
package experiment

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/judge"
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
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

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

// DesignVersion is the version of Design's stored form.
const DesignVersion = 1

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

// ArmRunCapUSD is what one of arm a's runs may spend at most: the agent's cap and its judgement's.
func (d Design) ArmRunCapUSD(a Arm) float64 { return d.ArmRunBudgetUSD(a) + d.JudgeCapUSD() }

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
	case effort != "" && !slices.Contains(Efforts, effort):
		return "", "", fmt.Errorf("%q: unknown effort %q (use %s)", s, effort, strings.Join(Efforts, ", "))
	}
	return model, effort, nil
}

// Runs is the number of agent runs the design asks for.
func (d Design) Runs() int { return len(d.Tasks) * d.Repeats * len(d.Arms) }

// JudgeCapUSD is what one run's judgement may spend at most: each repeat's call up to judge.CallCapUSD, twice, since a
// malformed reply is asked again. Zero without the judge. Claude Code checks --max-budget-usd after a turn, so a call
// can pass its cap a little: the reserve is an estimate, as the agent's own cap is.
func (d Design) JudgeCapUSD() float64 {
	if d.Judge == nil {
		return 0
	}
	return float64(d.Judge.WithDefaults().Repeats) * 2 * judge.CallCapUSD
}

// RunCapUSD is what one run may spend at most: the agent's cap and its judgement's; the larger of the arms' when they
// differ. Reserve holds it back for every run in flight, so spending never passes the budget.
func (d Design) RunCapUSD() float64 {
	capUSD := d.RunBudgetUSD + d.JudgeCapUSD()
	for _, a := range d.Arms {
		capUSD = max(capUSD, d.ArmRunCapUSD(a))
	}
	return capUSD
}

// PairCapUSD is what a pair of runs, one per arm, may spend at most.
func (d Design) PairCapUSD() float64 {
	if len(d.Arms) != 2 {
		return 2 * d.RunCapUSD()
	}
	return d.ArmRunCapUSD(d.Arms[0]) + d.ArmRunCapUSD(d.Arms[1])
}

// JudgeEstimateUSD is the judge's expected cost for every run of d, at the judge pilot's mean cost of a call
// (judge.EstimateUSD): a stated figure, not a measure of this project. Zero without the judge.
func (d Design) JudgeEstimateUSD() float64 {
	if d.Judge == nil {
		return 0
	}
	return float64(d.Runs()*d.Judge.WithDefaults().Repeats) * judge.EstimateUSD
}

// Validate checks that the design is complete and consistent.
func (d Design) Validate() error {
	var errs []error
	if d.Version != DesignVersion {
		errs = append(errs, fmt.Errorf("design version %d (this Agentium writes %d)", d.Version, DesignVersion))
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
			errs = append(errs, fmt.Errorf("both arms use context %s: a comparison of one context with itself is the %s template", d.Arms[0].Context, TemplateAA))
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
	} else if pair := d.PairCapUSD(); d.BudgetUSD < pair {
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
	if j := d.Judge; j != nil {
		if j.Repeats < 1 || j.Repeats > MaxJudgeRepeats {
			errs = append(errs, fmt.Errorf("the judge's repeats must be 1 to %d", MaxJudgeRepeats))
		}
		if j.Model == "" || j.Effort == "" {
			errs = append(errs, errors.New("the judge needs a model and an effort"))
		}
	}
	return errors.Join(errs...)
}

// validateProfiles checks the arms' own profiles: a model-ab experiment gives each arm a model, the profiles differ,
// and efforts are known levels; context templates give none.
func (d Design) validateProfiles() []error {
	var errs []error
	for _, a := range d.Arms {
		if d.Template != TemplateModelAB {
			if a.Model != "" || a.Effort != "" || a.RunBudgetUSD != 0 {
				errs = append(errs, fmt.Errorf("arm %s: a %s experiment compares contexts, so its arms take no model, effort or run budget of their own (use the %s template)",
					a.Name, d.Template, TemplateModelAB))
			}
			continue
		}
		switch {
		case a.Model == "":
			errs = append(errs, fmt.Errorf("arm %s: a %s experiment gives each arm a model", a.Name, TemplateModelAB))
		case a.Effort != "" && !slices.Contains(Efforts, a.Effort):
			errs = append(errs, fmt.Errorf("arm %s: unknown effort %q (use %s)", a.Name, a.Effort, strings.Join(Efforts, ", ")))
		case a.RunBudgetUSD < 0:
			errs = append(errs, fmt.Errorf("arm %s: the run budget must be positive", a.Name))
		}
	}
	if d.Template == TemplateModelAB && len(d.Arms) == 2 && d.Arms[0].Model == d.Arms[1].Model && d.Arms[0].Effort == d.Arms[1].Effort {
		errs = append(errs, fmt.Errorf("both arms run %s: a comparison of one profile with itself measures noise, not a difference (the %s template does that for a context)",
			Profile(d.Arms[0].Model, d.Arms[0].Effort), TemplateAA))
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

// Ineligible says why a task cannot be in an experiment with these arms, or returns "" when it can: it must be
// reviewed for solution leaks, and its hidden tests must fail on the base and pass with the reference in every arm's
// context (the last `task validate`). Judge-graded tasks are refused: experiments grade by tests only, and must not
// count such a task's runs as if its verification commands decided them.
func Ineligible(c Candidate, arms []Arm) string {
	if c.Grading == task.GradingJudge {
		return "it is judge-graded (its solution has no tests); experiments take judge-graded tasks in a later version"
	}
	if c.NeedsReview {
		return fmt.Sprintf("its instruction needs a review for solution leaks (then: agentium task edit %s --reviewed)", c.Name)
	}
	v := c.Validation
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
	return ""
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
