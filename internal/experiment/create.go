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
	"time"

	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// NewOptions is what `experiment new` was asked for, as flags give it. Prepare checks it and fills the defaults.
type NewOptions struct {
	Template, ContextA, ContextB string
	Tier                         string // a tier's name, or "" (quick unless Tasks are given)
	Tasks                        []string
	Repeats                      int
	Model, Effort, Goal          string
	RunBudget, Budget            float64
	Concurrency                  int
	Timeout, VerifyTimeout       time.Duration
	Seed                         uint64
	Judge                        bool
	JudgeModel, JudgeEffort      string
	JudgeRepeats                 int

	tier     Tier
	prepared bool // Prepare ran: the tier and the seed are set
}

// Prepare checks the options before anything is read: the name, flag combinations, the tier, repeats and a seed (random
// when 0). A mistake in them is a UsageError. It changes o.
func (o *NewOptions) Prepare(name string) error {
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
	case !o.Judge && (o.JudgeModel != "" || o.JudgeEffort != "" || o.JudgeRepeats != 0):
		return UsageError("--judge-model, --judge-effort and --judge-repeats set the judge: add --judge")
	case o.JudgeRepeats < 0:
		return UsageError("--judge-repeats must be positive")
	}
	o.tier = Tiers()[0]
	if o.Tier != "" {
		var found bool
		if o.tier, found = TierByName(o.Tier); !found {
			return UsageError(fmt.Sprintf("unknown tier %q (use quick or confident)", o.Tier))
		}
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
	armA, err := p.ResolveArm(ctx, "A", o.ContextA)
	if err != nil {
		return Created{}, err
	}
	armB := armA
	armB.Name = "B"
	if o.Template == TemplateContextAB {
		if armB, err = p.ResolveArm(ctx, "B", o.ContextB); err != nil {
			return Created{}, err
		}
	}
	d := Design{Version: DesignVersion, Template: o.Template, Arms: []Arm{armA, armB}, Repeats: o.Repeats, Model: o.Model, Effort: o.Effort,
		Goal: o.Goal, CostMargin: DefaultCostMargin, SuccessMargin: DefaultSuccessMargin, RunBudgetUSD: o.RunBudget,
		BudgetUSD: o.Budget, Timeout: o.Timeout, VerifyTimeout: o.VerifyTimeout, Concurrency: o.Concurrency, Seed: o.Seed}
	if o.Judge {
		s := llmjudge.Settings{Model: o.JudgeModel, Effort: o.JudgeEffort, Repeats: o.JudgeRepeats}.WithDefaults()
		d.Judge = &s
	}
	eligible, err := p.chooseTasks(ctx, &d, o)
	if err != nil {
		return Created{}, err
	}
	est, err := p.EstimateFor(ctx, d.Model)
	if err != nil {
		return Created{}, err
	}
	if d.BudgetUSD == 0 {
		if d.BudgetUSD = DefaultBudget(d, est); d.BudgetUSD == 0 {
			return Created{}, fmt.Errorf("the cost of a run cannot be estimated (%s): set --budget", est.Basis)
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

// chooseTasks sets d.Tasks: the named tasks (each must be eligible), or the tier's seeded sample of the eligible ones.
// It returns the eligible tasks; with none to sample from, the error is a *NoTasksError.
func (p Project) chooseTasks(ctx context.Context, d *Design, o NewOptions) ([]string, error) {
	eligible, reasons, err := p.EligibleTasks(ctx, d.Arms)
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
	if d.Judge != nil {
		fmt.Fprintf(out, "The judge: %s, its verdicts a second opinion beside the tests.\n", DescribeJudge(*d.Judge))
	}
	if !c.Explicit && c.Eligible < c.Tier.Tasks {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("note: the %s tier asks for %d tasks; only %d can be in it", c.Tier.Name, c.Tier.Tasks, c.Eligible)))
	}
	fmt.Fprintf(out, "Preview what it costs and can detect: %s\n", st.Command("agentium experiment plan "+name))
}
