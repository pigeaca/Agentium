package experiment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// ArmProgress is one arm's progress.
type ArmProgress struct {
	Name, Context                                              string
	Fair, Successes, Passed, Unfair, Infra, Cancelled, Settled int
	CostUSD                                                    float64
}

// Progress is where an experiment stands, from its stored runs: what WriteProgress prints and the experiment commands'
// JSON documents carry.
type Progress struct {
	Status         string // the stored status; a running one whose process ended reads store.StatusStopped
	StatusNote     string
	Slots          int // the schedule's runs
	Settled        int // slots with a settled run
	Failed         int // slots out of attempts: a resume does not retry them
	SpentUSD       float64
	JudgeUSD       float64 // of SpentUSD: both judges', the pairs' comparisons included
	PairJudgeUSD   float64 // of JudgeUSD: the pairs' comparisons
	CalibrationUSD float64 // of SpentUSD
	BudgetUSD      float64
	Arms           []ArmProgress
	UnjudgedRuns   int
	// UncomparedPairs counts the pairs of passing runs the pair judge has still to compare (PairRuns.NeedsComparing).
	UncomparedPairs int
	// Sequential is a seq-v1 experiment's looks; nil for other methods.
	Sequential *SeqStatus
	// Orphaned: a "running" status was stored by a process that is gone.
	Orphaned bool
}

// LoadProgress reads where a locked experiment stands.
func (p Project) LoadProgress(ctx context.Context, name string, id int64, lock Lock) (Progress, error) {
	stored, err := p.DB.ExperimentByName(ctx, p.ID, name)
	if err != nil {
		return Progress{}, err
	}
	runs, err := p.DB.ExperimentRuns(ctx, id)
	if err != nil {
		return Progress{}, err
	}
	counts := map[string]*ArmProgress{}
	out := Progress{Slots: len(lock.Schedule), BudgetUSD: lock.Design.BudgetUSD, Status: stored.Status, StatusNote: stored.StatusNote}
	for _, a := range lock.Arms {
		counts[a.Name] = &ArmProgress{Name: a.Name, Context: a.Context}
	}
	settled := map[int]bool{}
	attempts := map[int]int{} // infrastructure failures per slot (cancelled runs are not attempts), as Execute counts them
	for _, r := range runs {
		c := counts[r.Arm]
		if c == nil {
			continue
		}
		s := storedSpend(r)
		out.SpentUSD += s.TotalUSD() // the budget's spend; the arm's cost is the agent's alone
		out.JudgeUSD += s.JudgeUSD + s.PairJudgeUSD
		out.PairJudgeUSD += s.PairJudgeUSD
		c.CostUSD += s.AgentUSD
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return Progress{}, fmt.Errorf("run %s: %w", r.ID, err)
		}
		if needsJudge(lock, r, rec) {
			out.UnjudgedRuns++
		}
		switch {
		case Fair(r.Outcome):
			c.Fair++
			if r.Passed != nil && *r.Passed {
				c.Passed++
			}
			if Success(r.Outcome, r.Passed, rec.Behavior.ConfigChanged) {
				c.Successes++
			}
		case r.Outcome == "unfair":
			c.Unfair++
		case r.Outcome == "cancelled":
			c.Cancelled++
		default:
			c.Infra++
		}
		if !Settles(r.Outcome) && r.Outcome != "cancelled" {
			attempts[r.Slot]++
		}
		if Settles(r.Outcome) && !settled[r.Slot] {
			settled[r.Slot] = true
			c.Settled++
		}
	}
	out.Settled = len(settled)
	if out.UncomparedPairs, err = uncompared(lock, runs); err != nil {
		return Progress{}, err
	}
	for slot, n := range attempts {
		if !settled[slot] && n >= lock.MaxAttempts && slot >= 0 && slot < out.Slots {
			out.Failed++
		}
	}
	if stored.Status == store.StatusRunning && !p.Layout.RunsBusy() {
		out.Orphaned = true
	}
	if out.CalibrationUSD, err = p.CalibrationSpend(ctx, id); err != nil {
		return Progress{}, err
	}
	out.SpentUSD += out.CalibrationUSD
	for _, a := range lock.Arms {
		out.Arms = append(out.Arms, *counts[a.Name])
	}
	if lock.Method == MethodSeq {
		data, err := RunDataOfStored(runs)
		if err != nil {
			return Progress{}, err
		}
		status, _, err := SequentialStatus(lock, data)
		if err != nil {
			return Progress{}, err
		}
		out.Sequential = &status
	}
	return out, nil
}

// WriteProgress shows where an experiment stands, per arm, from its stored runs.
func (p Project) WriteProgress(ctx context.Context, out io.Writer, st term.Style, name string, id int64, lock Lock) error {
	pr, err := p.LoadProgress(ctx, name, id, lock)
	if err != nil {
		return err
	}
	status := pr.Status
	if status == StatusUsage {
		status = "paused at the usage limit"
	}
	if pr.StatusNote != "" {
		status += ": " + pr.StatusNote
	}
	if pr.Orphaned {
		status = "stopped (its Agentium process ended; run it again to resume)"
	}
	fmt.Fprintf(out, "%s %s\n", st.Heading("Experiment "+name+":"), st.Heading(st.Status(status)))
	judged := ""
	if pr.JudgeUSD > 0 {
		judged = fmt.Sprintf(" (the judge $%.2f of it, not in the arms' costs)", pr.JudgeUSD)
	}
	if pr.CalibrationUSD > 0 {
		judged += fmt.Sprintf(" (calibration $%.2f of it, not in the arms' costs)", pr.CalibrationUSD)
	}
	fmt.Fprintf(out, "  %d of %d runs settled; spent $%.2f of $%.2f%s\n", pr.Settled, pr.Slots, pr.SpentUSD, pr.BudgetUSD, judged)
	table := term.NewTable(st, term.Left("ARM"), term.Left("CONTEXT"), term.Right("SETTLED"), term.Right("FAIR"), term.Right("SUCCESSES"),
		term.Right("UNFAIR"), term.Right("INFRA"), term.Right("CANCELLED"), term.Right("COST"))
	for _, c := range pr.Arms {
		table.Row(c.Name, c.Context, fmt.Sprintf("%d/%d", c.Settled, pr.Slots/2), strconv.Itoa(c.Fair), strconv.Itoa(c.Successes),
			strconv.Itoa(c.Unfair), strconv.Itoa(c.Infra), strconv.Itoa(c.Cancelled), fmt.Sprintf("$%.2f", c.CostUSD))
		if c.Passed > c.Successes {
			table.Line(st.Warn(fmt.Sprintf("  arm %s: %d passing run(s) changed the test runner's configuration beyond the task's reference: not counted as successes", c.Name, c.Passed-c.Successes)))
		}
	}
	if err := table.Write(out); err != nil {
		return err
	}
	fmt.Fprintln(out, st.Note("Successes need a pass with the hidden tests; unfair (drifted), infrastructure and cancelled runs are not counted."))
	if s := pr.Sequential; s != nil {
		fmt.Fprintf(out, "Looks (method %s): %s\n", MethodSeq, s.Describe())
		for _, l := range s.Looks {
			fmt.Fprintf(out, "  %s\n", DescribeLook(l, len(s.Planned)))
		}
	}
	if pr.UnjudgedRuns > 0 {
		resume := "agentium experiment run " + name
		if pr.Status == store.StatusBudget { // the budget left no room to judge them: the same command as the stop's
			resume += " --budget USD"
		}
		fmt.Fprintf(out, "%s %s\n", st.Warn(fmt.Sprintf("%d graded run(s) still need the judge:", pr.UnjudgedRuns)), st.Command(resume))
	}
	if pr.UncomparedPairs > 0 {
		resume := "agentium experiment run " + name
		if pr.Status == store.StatusBudget {
			resume += " --budget USD"
		}
		fmt.Fprintf(out, "%s %s\n", st.Warn(fmt.Sprintf("%d pair(s) of passing runs still need the pair judge:", pr.UncomparedPairs)), st.Command(resume))
	}
	return nil
}
