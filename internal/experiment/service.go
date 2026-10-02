package experiment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// The services in this package (`Create`, `LoadReview`, `Runner.Run`, `WriteProgress`) hold what the experiment commands
// do; the command handlers in internal/cli parse flags, call one, and print. They keep no state: everything comes in
// as parameters, output goes to the writers given, and errors are wrapped with context.

// Project is a registered project opened for a command: its database, its identity and its folders.
type Project struct {
	DB     *store.Store
	ID     int64
	Layout home.Layout
	Root   string // the user's repository
	Bare   string // Agentium's bare repository for the project
}

// UsageError is a mistake in how a command was called that only a service could find (a name, a flag combination, a
// design that does not validate). Handlers report it as a usage error, not a runtime failure.
type UsageError string

func (e UsageError) Error() string { return string(e) }

// Load reads a stored experiment's design.
func (p Project) Load(ctx context.Context, name string) (Design, error) {
	stored, err := p.DB.ExperimentByName(ctx, p.ID, name)
	if err != nil {
		return Design{}, err
	}
	var d Design
	if err := json.Unmarshal(stored.Design, &d); err != nil {
		return Design{}, fmt.Errorf("experiment %s: %w", name, err)
	}
	if d.Version != d.WantVersion() || len(d.Arms) != 2 { // a model-ab design is version 2, any other 1
		return Design{}, fmt.Errorf("experiment %s: its design (version %d) is not one this Agentium reads", name, d.Version)
	}
	return d, nil
}

// ResolveArm resolves a context name: base, or a snapshot.
func (p Project) ResolveArm(ctx context.Context, name, contextName string) (Arm, error) {
	if contextName == BaseContext {
		return Arm{Name: name, Context: contextName}, nil
	}
	snap, err := p.DB.SnapshotByName(ctx, p.ID, contextName)
	if err != nil {
		return Arm{}, err
	}
	return Arm{Name: name, Context: contextName, Snapshot: snap.CommitID}, nil
}

// CalibrationFor returns the newest calibration of arm a's context on the arm's model (d.ArmModel(a)): tools and
// skills can differ by model, so a calibration on another model is none, and a model-ab calibration on one model does
// not displace a context experiment's on another. ErrNotFound when there is none; the caller names the command that
// makes it.
func (p Project) CalibrationFor(ctx context.Context, d Design, a Arm) (store.Calibration, error) {
	return p.CalibrationOn(ctx, a.Context, a.Snapshot, d.ArmModel(a))
}

// CalibrationOn is the newest calibration of a context (BaseContext or a snapshot's name, with its commit; "" for the
// base) on a model, as asked for with --model. ErrNotFound when there is none. `run once` uses it for its own model.
func (p Project) CalibrationOn(ctx context.Context, contextName, snapshot, model string) (store.Calibration, error) {
	all, err := p.DB.Calibrations(ctx, p.ID, contextName, snapshot)
	if err != nil {
		return store.Calibration{}, err
	}
	for _, c := range all {
		var cal run.Calibration
		if err := json.Unmarshal(c.Result, &cal); err != nil {
			return store.Calibration{}, fmt.Errorf("calibration of %s: %w", contextName, err)
		}
		if cal.RequestedModel == model {
			return c, nil
		}
	}
	return store.Calibration{}, fmt.Errorf("calibration of %s on %s: %w", contextName, model, store.ErrNotFound)
}

// EstimatesFor estimates each arm on its own model and effort (the same estimate twice when the arms share a profile),
// each limited to that arm's run cap.
func (p Project) EstimatesFor(ctx context.Context, d Design) (ArmEstimates, error) {
	var out ArmEstimates
	for i, a := range d.Arms[:2] {
		if i == 1 && d.ArmModel(a) == d.ArmModel(d.Arms[0]) && d.ArmEffort(a) == d.ArmEffort(d.Arms[0]) {
			out[1] = out[0]
		} else {
			est, err := p.EstimateFor(ctx, d.ArmModel(a), d.ArmEffort(a))
			if err != nil {
				return out, err
			}
			out[i] = est
		}
		out[i].CapUSD = d.ArmRunBudgetUSD(a)
	}
	return out, nil
}

// EligibleTasks returns the names of the tasks that can be in an experiment with these arms, and why each other task
// cannot. Retired tasks (the task pool's) are out. Only designs not yet locked consult it (new, plan, the first run):
// a locked experiment keeps its locked tasks, so resuming and reporting never check eligibility again.
func (p Project) EligibleTasks(ctx context.Context, arms []Arm) ([]string, map[string]string, error) {
	all, err := p.DB.Tasks(ctx, p.ID)
	if err != nil {
		return nil, nil, err
	}
	var eligible []string
	reasons := map[string]string{}
	for _, t := range all {
		c := Candidate{Name: t.Name, NeedsReview: t.NeedsReview, Grading: t.Grading}
		if t.Validation != nil {
			var v task.Validation
			if err := json.Unmarshal(t.Validation, &v); err != nil {
				return nil, nil, fmt.Errorf("task %s: validation: %w", t.Name, err)
			}
			c.Validation = &v
		}
		if t.Retired() {
			reasons[t.Name] = "it is retired: " + t.RetiredReason
		} else if why := Ineligible(c, arms); why != "" {
			reasons[t.Name] = why
		} else {
			eligible = append(eligible, t.Name)
		}
	}
	return eligible, reasons, nil
}

// WriteIneligible lists why tasks cannot be in an experiment, by name.
func WriteIneligible(w io.Writer, reasons map[string]string) {
	for _, name := range slices.Sorted(maps.Keys(reasons)) {
		fmt.Fprintf(w, "  %s: %s\n", name, reasons[name])
	}
}

// storedSpend is what a stored run spent (run.StoredSpend): its cost column is the agent's alone, and feeds estimates
// and the arms' costs; the judge's spend, from its record, counts against the budget with it.
func storedSpend(r store.Run) run.Spend { return run.StoredSpend(r.CostUSD, r.Record) }

// EstimateFor estimates runs on model (and effort, once any of the model's runs records one) from the project's earlier fair task runs on it that reported their cost (a run
// stopped before Claude Code's result has none): each task from its own runs, when it has some. A run is a task's own
// only while it is linked to it: a removed task's runs, and an experiment's runs of a task changed after the lock, are
// not, so a task imported again under the same name starts without history. A task edited in place keeps its ID, and
// so its earlier runs.
func (p Project) EstimateFor(ctx context.Context, model, effort string) (Estimate, error) {
	runs, err := p.DB.Runs(ctx, p.ID)
	if err != nil {
		return Estimate{}, err
	}
	var matching, unrecorded []PastRun // runs at this effort; runs from before efforts were recorded (effort unknown)
	for _, r := range runs {
		if r.Kind != "task" || !slices.Contains([]string{claude.OutcomeOK, claude.OutcomeCapped, claude.OutcomeTimeout}, r.Outcome) {
			continue
		}
		var rec struct {
			Model          string `json:"model"`
			Effort         string `json:"effort"`
			EffortRecorded bool   `json:"effort_recorded"`
			Metrics        struct {
				SawResult bool `json:"saw_result"`
			} `json:"metrics"`
		}
		if json.Unmarshal(r.Record, &rec) != nil || rec.Model != model || !rec.Metrics.SawResult {
			continue
		}
		// The agent's cost alone: what the judge spends is estimated apart (Design.JudgeEstimateUSD).
		if agent := storedSpend(r).AgentUSD; agent > 0 {
			own := ""
			if r.TaskID != 0 {
				own = r.TaskName
			}
			switch run := (PastRun{Task: own, CostUSD: agent}); {
			case !rec.EffortRecorded:
				unrecorded = append(unrecorded, run)
			case rec.Effort == effort:
				matching = append(matching, run)
			}
		}
	}
	// Runs at another effort never count. Runs of unknown effort fill in only while too few runs match.
	if len(matching) >= MinPastRuns {
		return EstimateRunAt(model, effort, matching, 0), nil
	}
	return EstimateRunAt(model, effort, append(matching, unrecorded...), len(unrecorded)), nil
}

// DescribeJudge is the judge's settings in words.
func DescribeJudge(s llmjudge.Settings) string {
	return fmt.Sprintf("%s at effort %s, %d call(s) per run", s.Model, s.Effort, s.Repeats)
}

// DescribeArms is the experiment's arms in words.
func DescribeArms(d Design) string {
	switch d.Template {
	case TemplateAA:
		return "A/A calibration of context " + d.Arms[0].Context
	case TemplateModelAB:
		return fmt.Sprintf("model A/B on context %s, A = %s, B = %s", d.Arms[0].Context, Profile(d.Arms[0].Model, d.Arms[0].Effort),
			Profile(d.Arms[1].Model, d.Arms[1].Effort))
	}
	return fmt.Sprintf("context A/B, A = %s, B = %s", d.Arms[0].Context, d.Arms[1].Context)
}

// ShortCommit abbreviates a commit ID for display.
func ShortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
