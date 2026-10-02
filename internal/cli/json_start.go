package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/report"
)

// startDoc is start's --json document. Status says where it stopped, always before any paid run:
//   - preview: the experiment exists and everything it needs is in place; run_command starts it (spending money);
//   - not_ready: it exists, but something is missing (readiness lists what; mined tasks may await review);
//   - too_few_tasks: fewer valid, reviewed tasks than the cost floor, so no experiment was created (the exit code is 1);
//   - finished: the experiment has finished; its report is `agentium experiment report`.
type startDoc struct {
	header
	Status         string            `json:"status"`
	NothingRun     bool              `json:"nothing_was_run"`
	Project        *projectDoc       `json:"project"`
	ContextA       string            `json:"context_a"`
	ContextB       string            `json:"context_b"` // empty for an A/A calibration
	Experiment     *startExperiment  `json:"experiment"`
	Ready          bool              `json:"ready"`
	Readiness      []readinessDoc    `json:"readiness"`
	Calibrations   int               `json:"calibration_runs_needed"`
	CalibrationUSD float64           `json:"calibration_estimate_usd"`
	RunCommand     string            `json:"run_command"`
	NorthStar      *report.NorthStar `json:"north_star"`
	Log            []string          `json:"log"` // the stages' text, plain
}

type startExperiment struct {
	Name          string  `json:"name"`
	Template      string  `json:"template"`
	Model         string  `json:"model"`
	Tasks         int     `json:"tasks"`
	RepeatsPerArm int     `json:"repeats_per_arm"`
	Runs          int     `json:"runs"`
	BudgetUSD     float64 `json:"budget_usd"` // what a run of it may spend in total
}

type readinessDoc struct {
	Status string `json:"status"` // ok, MISSING, or a warning's label
	Text   string `json:"text"`
}

// emitJSON prints start's document and returns code. review and budget are set once the experiment's preview exists.
func (s *starter) emitJSON(ctx context.Context, status string, code int, name string, review *experiment.Review, budget budgetPlan) int {
	doc := startDoc{header: hdr("start"), Status: status, NothingRun: true, ContextA: s.a, ContextB: s.b, Readiness: []readinessDoc{}, Log: []string{}}
	if s.w != nil {
		doc.Project = &projectDoc{ID: s.w.project.ID, Name: s.w.project.Name}
		if star, err := report.LoadNorthStar(ctx, s.w.service()); err == nil {
			doc.NorthStar = &star
		}
	}
	// Claude Code's location is not the document's business (a path under the home folder is also redacted by emit).
	clean := func(text string) string {
		if cli, err := claudePath(s.env); err == nil {
			text = strings.ReplaceAll(text, cli, "claude")
		}
		return text
	}
	if text := strings.TrimRight(clean(s.log.String()), "\n"); text != "" {
		doc.Log = strings.Split(text, "\n")
	}
	if review != nil {
		d := review.Design
		doc.Experiment = &startExperiment{Name: name, Template: d.Template, Model: d.Model, Tasks: len(d.Tasks), RepeatsPerArm: d.Repeats, Runs: d.Runs(), BudgetUSD: budget.total}
		doc.Ready = review.Readiness.Ready
		for _, c := range review.Readiness.Checks {
			doc.Readiness = append(doc.Readiness, readinessDoc{Status: c.Status, Text: clean(c.Text)})
		}
		doc.Calibrations = len(review.Readiness.Calibrations)
		doc.CalibrationUSD, _ = experiment.CalibrationCosts(review.Readiness.Calibrations)
		doc.RunCommand = "agentium experiment run " + name
		if s.args.budget > 0 {
			doc.RunCommand += " --budget " + strconv.FormatFloat(s.args.budget, 'f', -1, 64)
		}
	} else if name != "" {
		doc.Experiment = &startExperiment{Name: name}
	}
	return s.env.emitCode(doc, code)
}
