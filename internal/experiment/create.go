package experiment

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// NewOptions is what `experiment new` was asked for, as flags give it. Prepare checks it and fills the defaults.
type NewOptions struct {
	// Template is what --b implies (InferTemplate); `start` names one itself.
	Template, ContextA, ContextB string
	// ProfileA and ProfileB, for model-ab only, are the arms' MODEL[:EFFORT] (--a, else --model, and --b); ContextA is
	// then the one context both arms run (default: the base's). Both arms' runs stop at RunBudget.
	ProfileA, ProfileB     string
	Tier                   string // a tier's name, or "" (quick unless Tasks are given)
	Tasks                  []string
	Repeats                int
	Model, Effort, Goal    string
	RunBudget, Budget      float64
	Concurrency            int
	Timeout, VerifyTimeout time.Duration
	Seed                   uint64
	// Judge has the judge decide on every graded run, on JudgeModel at JudgeEffort ("": the default judge's), with the
	// default repeats (judge.DefaultRepeats).
	Judge                   bool
	JudgeModel, JudgeEffort string
	// JudgePairs has the pair judge compare each pair of passing runs (Design.JudgePairs), on PairJudgeModel at
	// PairJudgeEffort ("": the default judge's).
	JudgePairs                      bool
	PairJudgeModel, PairJudgeEffort string
	// NoFutility turns off a cost experiment's futility stops (Design.NoFutility).
	NoFutility bool
	// Grader is the mode its runs grade in (Design.Grader): task.GraderHost or task.GraderSandbox. The command line
	// gives the platform's default (task.DefaultGrader) unless --grader names one; empty is host.
	Grader string

	tier         Tier
	profA, profB profile // model-ab: ProfileA and ProfileB, parsed
	prepared     bool    // Prepare ran: the tier and the seed are set
}

type profile struct{ model, effort string }

// Prepare checks the options before anything is read: the name, flag combinations, the tier, repeats and a seed (random
// when 0). A mistake in them is a UsageError. It changes o. A cost experiment (goal cheaper) is made under method
// seq-v1 (NewMethod): one run per task and arm and up to 16 tasks, so it takes no tier and no other repeats.
func (o *NewOptions) Prepare(name string) error {
	if err := o.prepareProfiles(); err != nil {
		return UsageError(err.Error())
	}
	cost := NewMethod(o.Goal) == MethodSeq
	sequential := fmt.Sprintf("a cost experiment (--goal %s) runs method %s: up to %d tasks × 1 run per arm, with looks after %d, 12 and %d tasks",
		GoalCheaper, MethodSeq, stats.SeqMaxTasks, stats.SeqFirstLook, stats.SeqMaxTasks)
	switch {
	case cost && o.Tier != "":
		return UsageError("--tier sizes success experiments (--goal " + GoalBetter + "); " + sequential)
	case cost && o.Repeats > 1:
		return UsageError("--repeats is for success experiments (--goal " + GoalBetter + "); " + sequential)
	case cost && len(o.Tasks) > stats.SeqMaxTasks:
		return UsageError(fmt.Sprintf("%d tasks are too many: %s", len(o.Tasks), sequential))
	case !cost && o.NoFutility:
		return UsageError("--no-futility belongs to cost experiments (--goal " + GoalCheaper + ")")
	}
	switch {
	case !snapshot.ValidName(name):
		return UsageError(fmt.Sprintf("%q cannot name an experiment: use letters, digits, '.', '_' and '-'", name))
	case o.Template == TemplateContextAB && o.ContextB == "":
		return UsageError("a context A/B needs --b SNAPSHOT")
	case o.Template == TemplateAA && o.ContextB != "":
		return UsageError("an A/A experiment runs one context (--a) in both arms; drop --b")
	case o.Tier != "" && len(o.Tasks) > 0:
		return UsageError("--tier and --task both choose the tasks: use one")
	case o.Repeats < 0:
		return UsageError("--repeats must be positive")
	}
	o.tier = Tiers()[0]
	if o.Tier != "" {
		var found bool
		if o.tier, found = TierByName(o.Tier); !found {
			return UsageError(fmt.Sprintf("unknown tier %q (use quick or confident)", o.Tier))
		}
	}
	if cost {
		o.tier, o.Repeats = SeqTier(), 1
	}
	if o.Repeats == 0 {
		o.Repeats = o.tier.Repeats
		if len(o.Tasks) > 0 {
			o.Repeats = MinRepeats
		}
	}
	if o.Seed == 0 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		o.Seed = binary.LittleEndian.Uint64(b[:])&MaxSeed | 1 // never 0, which means "random"
	}
	o.prepared = true
	return nil
}

// prepareProfiles checks the options that only model-ab takes, and reads its profiles.
func (o *NewOptions) prepareProfiles() error {
	if o.Template != TemplateModelAB {
		if o.ProfileA != "" || o.ProfileB != "" {
			return errors.New("per-arm models belong to a model A/B: agentium experiment new NAME --b MODEL[:EFFORT] [--a MODEL[:EFFORT]]")
		}
		return nil
	}
	switch {
	case o.ProfileA == "" || o.ProfileB == "":
		return errors.New("a model A/B needs arm A's MODEL[:EFFORT] (--a, or --model) and arm B's (--b)")
	case o.ContextB != "":
		return errors.New("a model A/B runs one context in both arms (--context NAME); --a and --b name models")
	case o.Model != "" || o.Effort != "":
		return errors.New("a model A/B takes each arm's model and effort from --a and --b")
	}
	var err error
	if o.profA.model, o.profA.effort, err = ParseProfile(o.ProfileA); err != nil {
		return fmt.Errorf("--a %w", err)
	}
	if o.profB.model, o.profB.effort, err = ParseProfile(o.ProfileB); err != nil {
		return fmt.Errorf("--b %w", err)
	}
	return nil
}

// Created is a stored experiment: its design, and how many tasks could have been in it.
type Created struct {
	Design   Design
	Eligible int
	Tier     Tier
	Explicit bool // the tasks were named, not sampled by the tier
}

// Create designs the experiment from prepared options and stores it under name: the arms, the tasks (named, or a
// seeded sample of the eligible ones), the budget (a quarter above the estimate unless set) and the design's checks.
func Create(ctx context.Context, p Project, name string, o NewOptions, now time.Time) (Created, error) {
	if !o.prepared {
		return Created{}, errors.New("experiment: Create needs options that Prepare has checked")
	}
	if err := p.refuseAmbiguousB(ctx, o); err != nil {
		return Created{}, err
	}
	armA, err := p.ResolveArm(ctx, "A", o.ContextA)
	if err != nil {
		return Created{}, explainMissingArm(err, "--a", o)
	}
	armB := armA
	armB.Name = "B"
	if o.Template == TemplateContextAB {
		if armB, err = p.ResolveArm(ctx, "B", o.ContextB); err != nil {
			return Created{}, explainMissingArm(err, "--b", o)
		}
	}
	model, effort := o.Model, o.Effort
	if o.Template == TemplateModelAB { // Design.Model and Effort are arm A's, for what reads one model of an experiment
		armA.Model, armA.Effort = o.profA.model, o.profA.effort
		armB.Model, armB.Effort = o.profB.model, o.profB.effort
		model, effort = armA.Model, armA.Effort
	}
	d := Design{Template: o.Template, Arms: []Arm{armA, armB}, Repeats: o.Repeats, Model: model, Effort: effort,
		Goal: o.Goal, CostMargin: DefaultCostMargin, SuccessMargin: DefaultSuccessMargin, RunBudgetUSD: o.RunBudget,
		BudgetUSD: o.Budget, Timeout: o.Timeout, VerifyTimeout: o.VerifyTimeout, Concurrency: o.Concurrency, Seed: o.Seed,
		Method: NewMethod(o.Goal), NoFutility: o.NoFutility}
	if task.GraderOf(o.Grader) != task.GraderHost { // a host design is stored as before modes, byte for byte
		d.Grader = o.Grader
	}
	d.Version = d.WantVersion()
	if o.Judge {
		s := llmjudge.Settings{Model: o.JudgeModel, Effort: o.JudgeEffort}.WithDefaults()
		d.Judge = &s
	}
	if o.JudgePairs {
		s := llmjudge.Settings{Model: o.PairJudgeModel, Effort: o.PairJudgeEffort, Repeats: 1}.WithDefaults()
		d.JudgePairs = &s
	}
	eligible, err := p.chooseTasks(ctx, &d, o)
	if err != nil {
		return Created{}, err
	}
	ests, err := p.EstimatesFor(ctx, d)
	if err != nil {
		return Created{}, err
	}
	if d.BudgetUSD == 0 {
		needs, err := p.CalibrationNeeds(ctx, d, "", "") // Claude Code's version is not read here: stale ones are not counted
		if err != nil {
			return Created{}, err
		}
		calibrating, _ := CalibrationCosts(needs)
		if d.BudgetUSD = DefaultBudgetWith(d, ests, calibrating); d.BudgetUSD == 0 {
			return Created{}, fmt.Errorf("the cost of a run cannot be estimated (%s): set --budget", unknownBasis(d, ests))
		}
	}
	if err := d.Validate(); err != nil { // everything it checks came from flags
		return Created{}, UsageError(err.Error())
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		return Created{}, fmt.Errorf("encode experiment: %w", err)
	}
	if _, err := p.DB.SaveExperiment(ctx, store.Experiment{ProjectID: p.ID, Name: name, Template: d.Template, Design: encoded,
		CreatedAt: now}); err != nil {
		return Created{}, err
	}
	return Created{Design: d, Eligible: len(eligible), Tier: o.tier, Explicit: len(o.Tasks) > 0}, nil
}

// refuseAmbiguousB refuses a model A/B whose --b is also a snapshot's name: InferTemplate read it as the model, and
// the user may have meant the snapshot's context A/B. A MODEL:EFFORT is never a snapshot's name (no colon is allowed).
func (p Project) refuseAmbiguousB(ctx context.Context, o NewOptions) error {
	if o.Template != TemplateModelAB {
		return nil
	}
	switch _, err := p.DB.SnapshotByName(ctx, p.ID, o.ProfileB); {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	return UsageError(fmt.Sprintf("--b %s names both a model and a snapshot: it was read as the model (a model A/B), and is refused "+
		"since the snapshot would make a context A/B. To compare that context, snapshot it again under a name that is not a model's", o.ProfileB))
}

// explainMissingArm adds to a context that is not found (flag --a or --b) how the template was read from --b, when the
// name looks like what another template takes: a model where a context was read, or a name that is neither.
func explainMissingArm(err error, flag string, o NewOptions) error {
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	switch {
	case flag == "--b":
		return fmt.Errorf("%w: --b names a snapshot (a context A/B), or a model for a model A/B: one Agentium's price table knows, or a claude-… model ID", err)
	case o.Template == TemplateAA && IsModel(o.ContextA):
		return fmt.Errorf("%w: without --b this is an A/A, whose --a names its one context; for a model A/B, give --b a model too", err)
	case o.Template == TemplateContextAB && IsModel(o.ContextA):
		return fmt.Errorf("%w: --b %s is a snapshot, so this is a context A/B, whose --a names a context; for a model A/B, give --b a model", err, o.ContextB)
	}
	return err
}

// unknownBasis says why the cost cannot be estimated: for a model-ab experiment, in which arm.
func unknownBasis(d Design, ests ArmEstimates) string {
	if !d.PerArmProfiles() {
		return ests[0].Basis
	}
	var why []string
	for i, a := range d.Arms {
		if _, ok := ests[i].DesignUSD(Design{Tasks: d.Tasks, Repeats: 1, Arms: []Arm{a}}); !ok {
			why = append(why, "arm "+a.Name+": "+ests[i].Basis)
		}
	}
	return strings.Join(why, "; ")
}

// chooseTasks sets d.Tasks: the named tasks (each must be eligible), or the tier's seeded sample of the eligible ones.
// It returns the eligible tasks; with none to sample from, the error is a *NoTasksError.
func (p Project) chooseTasks(ctx context.Context, d *Design, o NewOptions) ([]string, error) {
	eligible, reasons, err := p.EligibleTasks(ctx, d.Arms, d.Grader)
	if err != nil {
		return nil, err
	}
	if len(o.Tasks) > 0 {
		for _, t := range o.Tasks {
			if why, known := reasons[t]; known {
				return nil, fmt.Errorf("task %s cannot be in this experiment: %s", t, why)
			}
			if !slices.Contains(eligible, t) {
				return nil, fmt.Errorf("task %q: %w", t, store.ErrNotFound)
			}
		}
		d.Tasks = slices.Sorted(slices.Values(o.Tasks))
		return eligible, nil
	}
	if len(eligible) == 0 {
		return nil, &NoTasksError{Reasons: reasons}
	}
	d.Tasks = Sample(eligible, o.tier.Tasks, d.Seed)
	return eligible, nil
}

// NoTasksError is Create's failure when no task can be in the experiment yet; Reasons says why for each task.
type NoTasksError struct{ Reasons map[string]string }

func (e *NoTasksError) Error() string { return "no task can be in this experiment yet" }

// Write reports the stored experiment: its size and budget, the judge, a short tier, and what to do next.
func (c Created) Write(out io.Writer, st term.Style, name string) {
	d := c.Design
	fmt.Fprintf(out, "Created experiment %s: %s, %d task(s) × %d run(s) per arm = %d runs, budget $%.2f.\n", name,
		DescribeArms(d), len(d.Tasks), d.Repeats, d.Runs(), d.BudgetUSD)
	if d.Sequential() {
		fmt.Fprintf(out, "Method %s: %s.\n", MethodSeq, DescribeLooks(d))
	}
	if d.Judge != nil {
		fmt.Fprintf(out, "The judge: %s, its verdicts a second opinion beside the tests.\n", DescribeJudge(*d.Judge))
	}
	if d.JudgePairs != nil {
		fmt.Fprintf(out, "The pair judge: %s; unvalidated, its preferences are exploratory and decide nothing.\n", DescribePairJudge(*d.JudgePairs))
	}
	switch {
	case !c.Explicit && c.Eligible < c.Tier.Tasks && d.Sequential():
		fmt.Fprintln(out, st.Note(fmt.Sprintf("note: a cost experiment takes up to %d tasks; only %d can be in it", c.Tier.Tasks, c.Eligible)))
	case !c.Explicit && c.Eligible < c.Tier.Tasks:
		fmt.Fprintln(out, st.Note(fmt.Sprintf("note: the %s tier asks for %d tasks; only %d can be in it", c.Tier.Name, c.Tier.Tasks, c.Eligible)))
	}
	fmt.Fprintf(out, "Preview what it costs and can detect: %s\n", st.Command("agentium experiment plan "+name))
}
