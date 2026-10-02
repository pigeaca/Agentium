package experiment

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// Review is a stored experiment looked over before it runs: its design, the tasks that can be in it, what a run is
// estimated to cost, whether everything running needs is in place, the sizes the study offers, and the project's runs
// (for the usage preview).
type Review struct {
	Design    Design
	Eligible  []string
	Reasons   map[string]string
	Estimates ArmEstimates // each arm's, on its own model
	Readiness Readiness
	Rows      []Row
	Runs      []store.Run
}

// LoadReview reads experiment name and checks it. Nothing is changed.
func LoadReview(ctx context.Context, p Project, e ReadinessEnv, name string) (Review, error) {
	d, err := p.Load(ctx, name)
	if err != nil {
		return Review{}, err
	}
	stored, err := p.DB.ExperimentByName(ctx, p.ID, name)
	if err != nil {
		return Review{}, err
	}
	eligible, reasons, err := p.EligibleTasks(ctx, d.Arms)
	if err != nil {
		return Review{}, err
	}
	if stored.Lock != nil {
		// A locked experiment keeps the tasks it locked: whatever has happened to them since (retired, revalidated,
		// removed) does not affect resuming, so it is not a readiness problem.
		eligible, reasons = d.Tasks, map[string]string{}
	}
	est, err := p.EstimatesFor(ctx, d)
	if err != nil {
		return Review{}, err
	}
	runs, err := p.DB.Runs(ctx, p.ID)
	if err != nil {
		return Review{}, err
	}
	return Review{Design: d, Eligible: eligible, Reasons: reasons, Estimates: est, Runs: runs,
		Readiness: CheckReadiness(ctx, p, e, d, eligible, reasons, est), Rows: PreviewFor(d, eligible, est)}, nil
}

// Write prints the review: the design, what is missing before it runs, the sizes with their costs and detectable
// effects, and the notes that explain them. now and signIn (the sign-in runs would use) feed the usage preview. A
// cancelled ctx stops it after the readiness checks, whose answers it may have cut short.
func (r Review) Write(ctx context.Context, out io.Writer, st term.Style, name, signIn string, now time.Time) error {
	r.writeDesign(out, st, name)
	fmt.Fprintln(out, "\n"+st.Heading("Before it runs:"))
	r.Readiness.Write(out, st)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	d := r.Design
	if d.Sequential() && len(d.Tasks) > 0 {
		fmt.Fprintln(out, "\n"+st.Heading(fmt.Sprintf("Looks (method %s; runs count both arms):", MethodSeq)))
		if err := r.writeSequential(out, st); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "\n"+st.Heading("Sizes (runs count both arms):"))
		if err := r.writeSizes(out, st); err != nil {
			return err
		}
	}
	r.writeCostBasis(out, st)
	r.writeNoHistory(out, st)
	r.writeCalibration(out, st)
	r.writeWorstCase(out, st)
	if !d.Sequential() {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("Detectable effects: 80%% power; changes two-sided at 5%%, the no-loss guard one-sided at 5%%. Planning defaults until an\n"+
			"A/A calibration measures this repository's: per-run log-cost spread σ = %.2f, success variance w = %.2f, and a spread of\n"+
			"the true effect across tasks τ = %.2f–%.2f (the range shown), in log cost and in success rate alike. Phase 0 measured\n"+
			"τ only for cost; the study assumed 0.05 for success, so the success columns lean cautious.",
			SigmaLogCost, WSuccess, TauLow, TauHigh)))
	}
	if own := Detect(len(d.Tasks), d.Repeats); !d.Sequential() && d.Goal == GoalCheaper && len(d.Tasks) > 0 && own.Guard[0] > d.SuccessMargin {
		fmt.Fprintln(out, st.Note("note: "+fmt.Sprintf("at this size the no-loss guard certifies only about %s, wider than the %.0f pp success margin: expect the\n"+
			"success verdict to be inconclusive unless there is no real difference and the noise is low.", percentRange(own.Guard, " pp"), 100*d.SuccessMargin)))
	}
	method := d.LockMethod()
	floors := FloorsFor(method)
	fmt.Fprintf(out, "Floors (method %s): verdicts on cost need %d tasks with %d or more runs per arm, and on success %d tasks with %d or more;\n"+
		"below them a metric is exploratory.\n", method, floors.CostTasks, floors.CostRepeats, floors.SuccessTasks, floors.SuccessRepeats)
	r.Usage(signIn, now).Write(out, st, now)
	if d.PerArmProfiles() && d.ArmModel(d.Arms[0]) == d.ArmModel(d.Arms[1]) {
		fmt.Fprintln(out, st.Note("The usage figures above are per model, not per effort: a higher effort uses more of the window per run."))
	}
	if !r.Readiness.Ready {
		fmt.Fprintln(out, st.Bad("Not ready to run: see above."))
	}
	return nil
}

// Usage previews the experiment's use of the subscription's five-hour window, each arm's runs on its own model, at the
// default --usage-limit; signIn is the sign-in runs would use.
func (r Review) Usage(signIn string, now time.Time) UsagePreview {
	d := r.Design
	var arms []UsageModel
	for _, a := range d.Arms {
		arms = append(arms, UsageModel{UsageRate: UsageRate{Model: d.ArmModel(a)}, PlannedRuns: len(d.Tasks) * d.Repeats})
	}
	return PlanUsage(r.Runs, arms, signIn, DefaultUsageLimit/100, now)
}

// writeSequential prints a seq-v1 design's looks (each one's tasks, runs, estimated and worst-case spend by then, and
// its intervals' levels) and its spend: the maximum, which the budget is sized for, and the expected spend with no true
// change and at a PreviewCut cut.
func (r Review) writeSequential(out io.Writer, st term.Style) error {
	d := r.Design
	p, err := PreviewSequential(d, r.Estimates)
	if err != nil {
		return err
	}
	usd := func(v float64) string {
		if !p.Known {
			return "unknown"
		}
		return fmt.Sprintf("$%.2f", v)
	}
	t := term.NewTable(st, term.Left("LOOK"), term.Right("AFTER TASKS"), term.Right("RUNS"), term.Right("EST. COST BY THEN"), term.Right("WORST CASE"),
		term.Right("COST INTERVAL"), term.Right("EQUIVALENCE INTERVAL"))
	for k, l := range p.Looks {
		t.Row(fmt.Sprintf("%d of %d", k+1, len(p.Looks)), strconv.Itoa(l.Tasks), strconv.Itoa(l.Runs), usd(l.CostUSD), fmt.Sprintf("$%.2f", l.WorstUSD),
			fmt.Sprintf("%.2f%%", 100*l.EffLevel), fmt.Sprintf("%.2f%%", 100*l.EqLevel))
	}
	if err := t.Write(out); err != nil {
		return err
	}
	fmt.Fprintf(out, "Spend: at most %s if every look runs (all %d tasks; %s if every run reaches its cap, overshoot included); expected\n"+
		"about %s if nothing changed (%.1f tasks on average) and %s at a %.0f%% cut (%.1f tasks). The budget is sized for the\n"+
		"maximum, and stops every run past it.\n",
		usd(p.MaxUSD), len(d.Tasks), fmt.Sprintf("$%.2f", p.WorstUSD), usd(p.NoneUSD), p.TasksNone, usd(p.CutUSD), 100*PreviewCut, p.TasksCut)
	futility := fmt.Sprintf("an interim look without one stops for futility when the chance\nof a verdict by the last look is below %.0f%%", 100*stats.SeqFutility)
	if d.NoFutility {
		futility = "futility stops are off"
	}
	fmt.Fprintln(out, st.Note(fmt.Sprintf("Cost decides at each look, at the levels shown (two-sided %.1f%% over all looks, O'Brien–Fleming-type spending); the\n"+
		"experiment stops at the first look with a verdict, and %s. Success, time and output tokens are\n"+
		"exploratory: one run per arm is below success's floor. Expected spend assumes σ = %.2f and τ = %.2f, the planning defaults.",
		100*stats.SeqAlpha, futility, SigmaLogCost, TauLow)))
	return nil
}

// writeNoHistory warns, for each arm model with fewer than MinPastRuns earlier task runs whose default estimate is held
// to the run cap, that the estimated spend assumes every run reaches it: safe for the budget, but often several times
// the real spend (the seq-v1 smoke rerun's $0.30 cap against $0.09 a run).
func (r Review) writeNoHistory(out io.Writer, st term.Style) {
	var seen []string
	for _, e := range r.Estimates {
		if !e.FewRuns() || e.EstimateBasis() != BasisCap || slices.Contains(seen, e.Model) {
			continue
		}
		seen = append(seen, e.Model)
		words := e.FewRunsWords(e.Model)
		fmt.Fprintln(out, st.Warn(fmt.Sprintf("%s: the estimated spend above assumes each run reaches its cap, so\n"+
			"it is likely high (safe for the budget); the experiment's first runs measure the real cost.", strings.ToUpper(words[:1])+words[1:])))
	}
}

// writeDesign prints the experiment's arms, model, goal, tasks and judge.
func (r Review) writeDesign(out io.Writer, st term.Style, name string) {
	d := r.Design
	goal := "cheaper, with success as the guard"
	if d.Goal == GoalBetter {
		goal = "better success"
	}
	fmt.Fprintln(out, st.Heading(fmt.Sprintf("Experiment %s: %s", name, DescribeArms(d))))
	for _, a := range d.Arms {
		commit := ""
		if a.Snapshot != "" {
			commit = " (" + ShortCommit(a.Snapshot) + ")"
		}
		if d.PerArmProfiles() {
			fmt.Fprintf(out, "  arm %s: model %s, effort %s, context %s%s; each run up to $%.2f\n", a.Name, a.Model, orDefaultEffort(a.Effort), a.Context, commit, d.ArmRunBudgetUSD(a))
			continue
		}
		fmt.Fprintf(out, "  arm %s: context %s%s\n", a.Name, a.Context, commit)
	}
	if d.PerArmProfiles() {
		fmt.Fprintf(out, "  each run up to %s; %d at a time\n", d.Timeout, d.Concurrency)
	} else {
		fmt.Fprintf(out, "  model %s, effort %s; each run up to $%.2f and %s; %d at a time\n", d.Model, orDefaultEffort(d.Effort), d.RunBudgetUSD, d.Timeout, d.Concurrency)
	}
	fmt.Fprintf(out, "  goal: %s (margins: cost %.0f%%, success %.0f pp); budget $%.2f\n", goal, 100*d.CostMargin, 100*d.SuccessMargin, d.BudgetUSD)
	fmt.Fprintf(out, "  tasks (%d, seed %d): %s\n", len(d.Tasks), d.Seed, strings.Join(d.Tasks, ", "))
	if d.Sequential() && len(d.Tasks) > 0 {
		fmt.Fprintf(out, "  method %s: %s\n", MethodSeq, DescribeLooks(d))
	}
	if d.Judge != nil {
		fmt.Fprintf(out, "  judge: %s; each run's judgement up to $%.2f; a second opinion, it decides nothing\n", DescribeJudge(*d.Judge), d.JudgeCapUSD())
	}
	if d.JudgePairs != nil {
		fmt.Fprintf(out, "  judge pairs: %s; each comparison up to $%.2f; unvalidated and exploratory, it decides nothing\n",
			DescribePairJudge(*d.JudgePairs), d.PairJudgeCapUSD())
	}
}

func orDefaultEffort(effort string) string {
	if effort == "" {
		return "the CLI's default"
	}
	return effort
}

// writeSizes prints the table of sizes, and the footnote of a tier asking for more tasks than are eligible.
func (r Review) writeSizes(out io.Writer, st term.Style) error {
	sizes := term.NewTable(st, term.Left("SIZE"), term.Right("TASKS"), term.Right("RUNS/ARM"), term.Right("RUNS"), term.Right("EST. COST"),
		term.Right("WORST CASE"), term.Right("COST CHANGE"), term.Right("SUCCESS CHANGE"), term.Right("NO-LOSS GUARD"), term.Left("EXPLORATORY"))
	marked := false // a row's task count carries a footnote mark; the others get a space so the digits line up
	for _, row := range r.Rows {
		marked = marked || row.Short
	}
	for i, row := range r.Rows {
		tasks := fmt.Sprint(row.Tasks)
		if row.Short {
			tasks += "*"
		} else if marked {
			tasks += " "
		}
		cost := "unknown"
		if row.CostKnown {
			cost = fmt.Sprintf("$%.2f", row.CostUSD+row.JudgeUSD) // the judge's share is stated below the table
		}
		effects := []string{percentRange(row.Detect.Cost, "%"), percentRange(row.Detect.Success, " pp"), percentRange(row.Detect.Guard, " pp")}
		exploratory := strings.Join(row.Exploratory, ", ")
		if row.Tasks == 0 {
			effects, exploratory = []string{"-", "-", "-"}, st.Warn("no tasks")
		} else if exploratory == "" {
			exploratory = "-"
		} else {
			exploratory = st.Warn(exploratory)
		}
		name := row.Name
		if i == len(r.Rows)-1 { // this experiment's own size
			name = st.Heading(name)
		}
		sizes.Row(name, tasks, strconv.Itoa(row.Repeats), strconv.Itoa(row.Runs), cost, fmt.Sprintf("$%.2f", row.WorstUSD),
			effects[0], effects[1], effects[2], exploratory)
	}
	if err := sizes.Write(out); err != nil {
		return err
	}
	tiers := Tiers()
	if len(r.Eligible) < tiers[len(tiers)-1].Tasks {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("* only %d task(s) can be in this experiment; a tier asking for more uses them all", len(r.Eligible))))
	}
	return nil
}

// writeCalibration says what the calibrations the experiment makes when it runs cost, when there are any: they come out
// of the budget, apart from the table's costs.
func (r Review) writeCalibration(out io.Writer, st term.Style) {
	needs := r.Readiness.Calibrations
	if len(needs) == 0 {
		return
	}
	estimate, capUSD := CalibrationCosts(needs)
	fmt.Fprintln(out, st.Note(fmt.Sprintf("Calibration: %d context calibration(s) are made when the experiment runs, about $%.2f in all and at most $%.2f (not in the\n"+
		"table's costs; counted in the budget). A calibration that fails its checks stops the experiment before any task run.", len(needs), estimate, capUSD)))
}

// writeWorstCase prints the judges' share of the cost, when there are judges, and the worst case the budget guards.
func (r Review) writeWorstCase(out io.Writer, st term.Style) {
	d := r.Design
	own := r.Rows[len(r.Rows)-1]
	agent := "unknown"
	if own.CostKnown {
		agent = fmt.Sprintf("$%.2f", own.CostUSD)
	}
	if d.Judge != nil {
		j := d.Judge
		fmt.Fprintf(out, "The judge: about $%.2f for this experiment's %d runs × %d call(s) at $%.3f a call (EST. COST includes it; the agent's\n"+
			"runs are %s). $%.3f is the judge pilot's mean call on %s at effort %s, not a measure of this project.\n",
			d.JudgeEstimateUSD(), own.Runs, j.Repeats, llmjudge.EstimateUSD, agent, llmjudge.EstimateUSD, llmjudge.DefaultModel, llmjudge.DefaultEffort)
	}
	if d.JudgePairs != nil {
		fmt.Fprintf(out, "The pair judge (unvalidated): about $%.2f if it compares all %d pairs at $%.3f a pair, both orders (EST. COST includes it;\n"+
			"only pairs whose two runs pass are compared). $%.3f is the judge pilot's mean pair on %s at effort %s, not a measure\n"+
			"of this project. Its preferences are exploratory: never a verdict.\n",
			d.PairJudgeEstimateUSD(), d.Pairs(), llmjudge.PairEstimateUSD, llmjudge.PairEstimateUSD, llmjudge.DefaultModel, llmjudge.DefaultEffort)
	}
	floor := fmt.Sprintf("$%.2f", claude.CapOvershootFloorUSD(d.ArmModel(d.Arms[0])))
	if fb := claude.CapOvershootFloorUSD(d.ArmModel(d.Arms[1])); fb != claude.CapOvershootFloorUSD(d.ArmModel(d.Arms[0])) {
		floor = fmt.Sprintf("%s (arm A) or $%.2f (arm B)", floor, fb)
	}
	guard := fmt.Sprintf("\nClaude Code checks a cap after each turn, so a run can pass it by the turn that crosses it: the budget holds back\n"+
		"%.0f%% of each cap for that, at least %s (the floor follows the model's output price). A run starts only when the\n"+
		"spend so far and the caps of the runs in flight, with that allowance, leave room for its own, so spending stays within\n"+
		"the $%.2f budget unless a single turn costs more.",
		100*claude.CapOvershootShare, floor, d.BudgetUSD)
	if d.JudgePairs != nil {
		guard = fmt.Sprintf(" Each pair's comparison may reach $%.2f (%d calls at $%.2f: both orders, each asked twice at most).", d.PairJudgeCapUSD(),
			llmjudge.PairCalls, d.PairJudgeCapUSD()/llmjudge.PairCalls) + guard
	}
	if d.PerArmProfiles() {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("Worst case: every run reaches its cap (arm A $%.2f, arm B $%.2f%s).%s", d.ArmRunBudgetUSD(d.Arms[0]),
			d.ArmRunBudgetUSD(d.Arms[1]), judgeCapNote(d), guard)))
	} else if d.Judge == nil {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("Worst case: every run reaches its $%.2f cap.%s", d.RunBudgetUSD, guard)))
	} else {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("Worst case: every run reaches its $%.2f cap, and its judgement $%.2f (%d call(s) at $%.2f, each asked twice at\n"+
			"most).%s", d.RunBudgetUSD, d.JudgeCapUSD(), d.Judge.Repeats, d.JudgeCapUSD()/float64(2*d.Judge.WithDefaults().Repeats), guard)))
	}
}

func judgeCapNote(d Design) string {
	if d.Judge == nil {
		return ""
	}
	return fmt.Sprintf(", and its judgement $%.2f", d.JudgeCapUSD())
}

// writeCostBasis says how each arm's runs are estimated: once for a context experiment, whose arms share a model, and
// per arm for a model-ab experiment, with a note when the arms' per-run costs differ.
func (r Review) writeCostBasis(out io.Writer, st term.Style) {
	d := r.Design
	if !d.PerArmProfiles() {
		WriteCostBasis(out, st, d, r.Eligible, r.Estimates[0])
		return
	}
	for i, a := range d.Arms {
		fmt.Fprintf(out, "Arm %s: ", a.Name)
		WriteCostBasis(out, st, withModel(d, a.Model+map[bool]string{true: " at effort " + a.Effort}[a.Effort != ""]), r.Eligible, r.Estimates[i])
	}
}

func withModel(d Design, model string) Design {
	d.Model = model
	return d
}

// WriteCostBasis says how each of the experiment's tasks is estimated: from its own earlier runs, or from the fallback
// for tasks without any. The tiers draw from the eligible tasks, so they average those tasks' estimates, which may
// include tasks outside the experiment: the note shows that average (a seq-v1 preview shows no tiers, and no note).
func WriteCostBasis(out io.Writer, st term.Style, d Design, eligible []string, est Estimate) {
	var own, other []string
	for _, t := range d.Tasks {
		if c, ok := est.Tasks[t]; ok {
			perRun, _ := est.TaskUSD(t)
			at := fmt.Sprintf("%s $%.2f (%d run(s))", t, perRun, c.Runs)
			if est.Capped(c.PerRunUSD) {
				at = fmt.Sprintf("%s $%.2f, the run cap (their median $%.2f, %d run(s))", t, perRun, c.PerRunUSD, c.Runs)
			}
			own = append(own, at)
		} else {
			other = append(other, t)
		}
	}
	fmt.Fprintf(out, "Estimated cost per run on %s:\n", d.Model)
	if len(own) > 0 {
		fmt.Fprintf(out, "  from each task's own earlier runs (their median): %s\n", strings.Join(own, ", "))
	}
	if len(other) > 0 {
		fallback := "no estimate (" + est.Basis + ")"
		switch {
		case est.Known && est.Capped(est.PerRunUSD) && est.FewRuns():
			fallback = fmt.Sprintf("$%.2f, the run cap: %s: the estimate assumes each run reaches its cap ($%.2f\n"+
				"    from %s)", est.CapUSD, est.FewRunsWords("this model"), est.PerRunUSD, est.Basis)
		case est.Known && est.Capped(est.PerRunUSD):
			fallback = fmt.Sprintf("$%.2f, the run cap: runs are expected to reach it ($%.2f from %s)", est.CapUSD, est.PerRunUSD, est.Basis)
		case est.Known:
			fallback = fmt.Sprintf("$%.2f, %s", est.PerRunUSD, est.Basis)
		}
		fmt.Fprintf(out, "  %s, without runs of their own: %s\n", strings.Join(other, ", "), fallback)
	}
	if mean, known := est.MeanUSD(eligible); known && !d.Sequential() && slices.ContainsFunc(eligible, func(t string) bool { _, ok := est.Tasks[t]; return ok }) {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("The tiers' estimates average the %d eligible task(s), each estimated the same way: $%.2f a run.", len(eligible), mean)))
	}
}

// percentRange shows a detectable effect's range; success effects of 100 pp or more cannot be detected at all.
func percentRange(v [2]float64, unit string) string {
	end := func(x float64) string {
		if unit == " pp" && x >= 1 {
			return "100+"
		}
		return fmt.Sprintf("%.0f", 100*x)
	}
	if lo, hi := end(v[0]), end(v[1]); lo != hi {
		return lo + "–" + hi + unit
	}
	return end(v[1]) + unit
}
