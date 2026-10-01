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

// armCounts is one arm's progress.
type armCounts struct {
	Fair, Successes, Passed, Unfair, Infra, Cancelled, Settled int
	CostUSD                                                    float64
}

// WriteProgress shows where an experiment stands, per arm, from its stored runs.
func (p Project) WriteProgress(ctx context.Context, out io.Writer, st term.Style, name string, id int64, lock Lock) error {
	stored, err := p.DB.ExperimentByName(ctx, p.ID, name)
	if err != nil {
		return err
	}
	runs, err := p.DB.ExperimentRuns(ctx, id)
	if err != nil {
		return err
	}
	counts := map[string]*armCounts{}
	for _, a := range lock.Arms {
		counts[a.Name] = &armCounts{}
	}
	settled := map[int]bool{}
	spent, judgeSpent := 0.0, 0.0
	unjudgedRuns := 0
	for _, r := range runs {
		c := counts[r.Arm]
		if c == nil {
			continue
		}
		s := storedSpend(r)
		spent += s.TotalUSD() // the budget's spend; the arm's cost is the agent's alone
		judgeSpent += s.JudgeUSD
		c.CostUSD += s.AgentUSD
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return fmt.Errorf("run %s: %w", r.ID, err)
		}
		if needsJudge(lock, r, rec) {
			unjudgedRuns++
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
		if Settles(r.Outcome) && !settled[r.Slot] {
			settled[r.Slot] = true
			c.Settled++
		}
	}
	status := stored.Status
	if status == StatusUsage {
		status = "paused at the usage limit"
	}
	if stored.StatusNote != "" {
		status += ": " + stored.StatusNote
	}
	if stored.Status == store.StatusRunning && !p.Layout.RunsBusy() {
		status = "stopped (its Agentium process ended; run it again to resume)"
	}
	fmt.Fprintf(out, "%s %s\n", st.Heading("Experiment "+name+":"), st.Heading(st.Status(status)))
	judged := ""
	if judgeSpent > 0 {
		judged = fmt.Sprintf(" (the judge $%.2f of it, not in the arms' costs)", judgeSpent)
	}
	fmt.Fprintf(out, "  %d of %d runs settled; spent $%.2f of $%.2f%s\n", len(settled), len(lock.Schedule), spent, lock.Design.BudgetUSD, judged)
	table := term.NewTable(st, term.Left("ARM"), term.Left("CONTEXT"), term.Right("SETTLED"), term.Right("FAIR"), term.Right("SUCCESSES"),
		term.Right("UNFAIR"), term.Right("INFRA"), term.Right("CANCELLED"), term.Right("COST"))
	for _, a := range lock.Arms {
		c := counts[a.Name]
		table.Row(a.Name, a.Context, fmt.Sprintf("%d/%d", c.Settled, len(lock.Schedule)/2), strconv.Itoa(c.Fair), strconv.Itoa(c.Successes),
			strconv.Itoa(c.Unfair), strconv.Itoa(c.Infra), strconv.Itoa(c.Cancelled), fmt.Sprintf("$%.2f", c.CostUSD))
		if c.Passed > c.Successes {
			table.Line(st.Warn(fmt.Sprintf("  arm %s: %d passing run(s) changed the test runner's configuration beyond the task's reference: not counted as successes", a.Name, c.Passed-c.Successes)))
		}
	}
	if err := table.Write(out); err != nil {
		return err
	}
	fmt.Fprintln(out, st.Note("Successes need a pass with the hidden tests; unfair (drifted), infrastructure and cancelled runs are not counted."))
	if unjudgedRuns > 0 {
		resume := "agentium experiment run " + name
		if stored.Status == store.StatusBudget { // the budget left no room to judge them: the same command as the stop's
			resume += " --budget USD"
		}
		fmt.Fprintf(out, "%s %s\n", st.Warn(fmt.Sprintf("%d graded run(s) still need the judge:", unjudgedRuns)), st.Command(resume))
	}
	return nil
}
