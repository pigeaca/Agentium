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
	if d.Version != DesignVersion || len(d.Arms) != 2 {
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

// EligibleTasks returns the names of the tasks that can be in an experiment with these arms, and why each other task
// cannot.
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
		if why := Ineligible(c, arms); why != "" {
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

// EstimateFor estimates runs on model from the project's earlier fair task runs on it that reported their cost (a run
// stopped before Claude Code's result has none): each task from its own runs, when it has some. A run is a task's own
// only while it is linked to it: a removed task's runs, and an experiment's runs of a task changed after the lock, are
// not, so a task imported again under the same name starts without history. A task edited in place keeps its ID, and
// so its earlier runs.
func (p Project) EstimateFor(ctx context.Context, model string) (Estimate, error) {
	runs, err := p.DB.Runs(ctx, p.ID)
	if err != nil {
		return Estimate{}, err
	}
	var past []PastRun
	for _, r := range runs {
		if r.Kind != "task" || !slices.Contains([]string{claude.OutcomeOK, claude.OutcomeCapped, claude.OutcomeTimeout}, r.Outcome) {
			continue
		}
		var rec struct {
			Model   string `json:"model"`
			Metrics struct {
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
			past = append(past, PastRun{Task: own, CostUSD: agent})
		}
	}
	return EstimateRun(model, past), nil
}

// DescribeJudge is the judge's settings in words.
func DescribeJudge(s llmjudge.Settings) string {
	return fmt.Sprintf("%s at effort %s, %d call(s) per run", s.Model, s.Effort, s.Repeats)
}

// DescribeArms is the experiment's arms in words.
func DescribeArms(d Design) string {
	if d.Template == TemplateAA {
		return "A/A calibration of context " + d.Arms[0].Context
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
