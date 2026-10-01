package cli

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/gitx"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// experimentUsage is the help of experiment's subcommands. It is computed once from constants and never changed.
var experimentUsage = `Usage:
  agentium experiment new NAME --b SNAPSHOT [--a CONTEXT] [--tier quick|confident | --task NAME...] [--repeats N]
                     [--model MODEL] [--effort LEVEL] [--goal cheaper|better] [--run-budget USD] [--budget USD]
                     [--concurrency N] [--timeout DURATION] [--verify-timeout DURATION] [--seed N]
                     [--judge [--judge-model MODEL] [--judge-effort LEVEL] [--judge-repeats N]]
                     a context A/B: arm A (default: base, each task's own context) against snapshot B
  agentium experiment new NAME --template aa [--a CONTEXT] [...]
                     an A/A calibration: one context in both arms, which must find no difference
  agentium experiment plan NAME
                     the runs, the estimated cost and the effects each size can detect; what is missing before it runs
  agentium experiment run NAME [--budget USD] [--usage-limit PCT] [--wait]
                     lock the experiment (first time) and run it: real Claude Code runs, interleaved in pairs, within
                     the budget; infrastructure failures are retried. Run it again to resume; --budget raises the total.
                     With a subscription, no pair starts past --usage-limit (default 85) of the five-hour window:
                     it pauses, or with --wait waits for the window to reset
  agentium experiment show NAME
                     the lock and the progress per arm
  agentium experiment report NAME [--json | --markdown] [--out FILE]
                     verdicts, metrics with their intervals, per-task results, behavior, costs and notes (with
                     --judge, its verdicts too): styled for a terminal; as Markdown (for a pull request) when piped,
                     with --out or --markdown; or JSON (with the lock and every run)
  agentium experiment list
  agentium experiment rm NAME        (only one that has not run)

Tasks must be reviewed and valid in both arms' contexts (agentium task validate NAME --snapshot SNAPSHOT). A tier
samples them: quick is 12 tasks × 3 runs per arm, confident 23 × 5; --task picks them instead (repeatable).

` + fmt.Sprintf(`--judge asks an LLM judge about every graded run: does its change do what the task asks, as the task's reference
solution does? It reads the instruction and both changes' code (never the tests), --judge-repeats times (default %d,
up to %d; the majority wins) on --judge-model (default %s) at --judge-effort (default %s). Its verdict
(fixed, partly or no, with a reason) is a second opinion beside the tests: it decides nothing. Its calls count against
the budget, which holds back repeats × 2 × $%.2f per run for them, but never toward an arm's cost. Tasks without a
reference solution in code are not judged. Judging can hold a run's slot for up to repeats × %d minutes more. The usage
limit's per-run estimate leaves out the judge's use of a subscription; a judge that hits a usage limit pauses the
experiment, and --wait does not wait for it. The report's Judge section shows each arm's verdicts among passing and
failing runs with 95%% intervals, the runs not judged and why, how often the repeats agreed, the judge's cost, and the
passing runs it did not call fixed, with its reasons (the first %d; --json lists them all).
`, llmjudge.DefaultRepeats, experiment.MaxJudgeRepeats, llmjudge.DefaultModel, llmjudge.DefaultEffort, llmjudge.CallCapUSD,
	int(llmjudge.CallTimeout.Minutes()), report.MaxFlagged)

func runExperiment(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, experimentUsage)
		return ExitUsage
	}
	switch args[0] {
	case "new":
		return experimentNew(ctx, env, args[1:])
	case "plan":
		return experimentPlan(ctx, env, args[1:])
	case "run":
		return experimentRun(ctx, env, args[1:])
	case "show":
		return experimentShow(ctx, env, args[1:])
	case "report":
		return experimentReport(ctx, env, args[1:])
	case "list":
		return experimentList(ctx, env, args[1:])
	case "rm":
		return experimentRemove(ctx, env, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(env.Stdout, experimentUsage)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "agentium experiment: unknown subcommand %q\n\n%s", args[0], experimentUsage)
	return ExitUsage
}

func experimentNew(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("experiment new", flag.ContinueOnError)
	template := fs.String("template", experiment.TemplateContextAB, "context-ab or aa")
	contextA := fs.String("a", experiment.BaseContext, "arm A's context: base or a snapshot")
	contextB := fs.String("b", "", "arm B's context: a snapshot (context-ab only)")
	tierName := fs.String("tier", "", "quick (12 tasks × 3 runs) or confident (23 × 5); default quick unless --task is given")
	var tasks stringList
	fs.Var(&tasks, "task", "a task to include (repeatable; instead of a tier)")
	repeats := fs.Int("repeats", 0, "runs per task per arm (default: the tier's, or 3)")
	model := fs.String("model", "claude-sonnet-5", "the model")
	effort := fs.String("effort", "", "the effort level (default: the CLI's)")
	goal := fs.String("goal", experiment.GoalCheaper, "cheaper (cost, with success as the guard) or better (success)")
	runBudget := fs.Float64("run-budget", 3, "stop each run at this cost in USD")
	budget := fs.Float64("budget", 0, "stop the experiment at this total in USD (default: a quarter above the estimate)")
	concurrency := fs.Int("concurrency", 2, "runs at a time")
	timeout := fs.Duration("timeout", 20*time.Minute, "stop each run after this long")
	verifyTimeout := fs.Duration("verify-timeout", 10*time.Minute, "time limit for each setup or verification command")
	seed := fs.Uint64("seed", 0, "the seed for the task sample and the run order (default: random)")
	judgeOn := fs.Bool("judge", false, "ask the LLM judge about every graded run (a second opinion; it decides nothing)")
	judgeModel := fs.String("judge-model", "", "the judge's model (default "+llmjudge.DefaultModel+")")
	judgeEffort := fs.String("judge-effort", "", "the judge's effort level (default "+llmjudge.DefaultEffort+")")
	judgeRepeats := fs.Int("judge-repeats", 0, fmt.Sprintf("the judge's calls per run, the majority wins (default %d, at most %d)", llmjudge.DefaultRepeats, experiment.MaxJudgeRepeats))
	rest, code, ok := parseArgs(env, fs, args, experimentUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		fmt.Fprint(env.Stderr, experimentUsage)
		return ExitUsage
	}
	name := rest[0]
	usage := func(format string, a ...any) int {
		fmt.Fprintf(env.Stderr, "agentium experiment new: "+format+"\n", a...)
		return ExitUsage
	}
	if !snapshot.ValidName(name) {
		return usage("%q cannot name an experiment: use letters, digits, '.', '_' and '-'", name)
	}
	switch {
	case *template == experiment.TemplateContextAB && *contextB == "":
		return usage("a context A/B needs --b SNAPSHOT")
	case *template == experiment.TemplateAA && *contextB != "":
		return usage("an A/A experiment runs one context (--a) in both arms; drop --b")
	case *tierName != "" && len(tasks) > 0:
		return usage("--tier and --task both choose the tasks: use one")
	case *repeats < 0:
		return usage("--repeats must be positive")
	case !*judgeOn && (*judgeModel != "" || *judgeEffort != "" || *judgeRepeats != 0):
		return usage("--judge-model, --judge-effort and --judge-repeats set the judge: add --judge")
	case *judgeRepeats < 0:
		return usage("--judge-repeats must be positive")
	}
	tier := experiment.Tiers()[0]
	if *tierName != "" {
		var found bool
		if tier, found = experiment.TierByName(*tierName); !found {
			return usage("unknown tier %q (use quick or confident)", *tierName)
		}
	}
	if *repeats == 0 {
		*repeats = tier.Repeats
		if len(tasks) > 0 {
			*repeats = experiment.MinRepeats
		}
	}
	if *seed == 0 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return fail(env, fmt.Errorf("seed: %w", err))
		}
		*seed = binary.LittleEndian.Uint64(b[:])&experiment.MaxSeed | 1 // never 0, which means "random"
	}

	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	armA, err := experimentArm(ctx, w, "A", *contextA)
	if err != nil {
		return fail(env, err)
	}
	armB := armA
	armB.Name = "B"
	if *template == experiment.TemplateContextAB {
		if armB, err = experimentArm(ctx, w, "B", *contextB); err != nil {
			return fail(env, err)
		}
	}
	d := experiment.Design{Version: experiment.DesignVersion, Template: *template, Arms: []experiment.Arm{armA, armB}, Repeats: *repeats, Model: *model, Effort: *effort,
		Goal: *goal, CostMargin: experiment.DefaultCostMargin, SuccessMargin: experiment.DefaultSuccessMargin, RunBudgetUSD: *runBudget,
		BudgetUSD: *budget, Timeout: *timeout, VerifyTimeout: *verifyTimeout, Concurrency: *concurrency, Seed: *seed}
	if *judgeOn {
		s := llmjudge.Settings{Model: *judgeModel, Effort: *judgeEffort, Repeats: *judgeRepeats}.WithDefaults()
		d.Judge = &s
	}

	eligible, reasons, err := eligibleTasks(ctx, w, d.Arms)
	if err != nil {
		return fail(env, err)
	}
	if len(tasks) > 0 {
		for _, t := range tasks {
			if why, known := reasons[t]; known {
				return fail(env, fmt.Errorf("task %s cannot be in this experiment: %s", t, why))
			}
			if !slices.Contains(eligible, t) {
				return fail(env, fmt.Errorf("task %q: %w", t, store.ErrNotFound))
			}
		}
		d.Tasks = slices.Sorted(slices.Values(tasks))
	} else {
		if len(eligible) == 0 {
			printIneligible(env.Stderr, reasons)
			return fail(env, errors.New("no task can be in this experiment yet"))
		}
		d.Tasks = experiment.Sample(eligible, tier.Tasks, d.Seed)
	}
	est, err := estimateRun(ctx, w, d.Model)
	if err != nil {
		return fail(env, err)
	}
	if d.BudgetUSD == 0 {
		if d.BudgetUSD = experiment.DefaultBudget(d, est); d.BudgetUSD == 0 {
			return fail(env, fmt.Errorf("the cost of a run cannot be estimated (%s): set --budget", est.Basis))
		}
	}
	if err := d.Validate(); err != nil { // everything it checks came from flags
		return usage("%v", err)
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		return fail(env, fmt.Errorf("encode experiment: %w", err))
	}
	if _, err := w.db.SaveExperiment(ctx, store.Experiment{ProjectID: w.project.ID, Name: name, Template: d.Template, Design: encoded,
		CreatedAt: env.Now()}); err != nil {
		return fail(env, err)
	}
	st := env.style()
	fmt.Fprintf(env.Stdout, "Created experiment %s: %s, %d task(s) × %d run(s) per arm = %d runs, budget $%.2f.\n", name,
		describeArms(d), len(d.Tasks), d.Repeats, d.Runs(), d.BudgetUSD)
	if d.Judge != nil {
		fmt.Fprintf(env.Stdout, "The judge: %s, its verdicts a second opinion beside the tests.\n", describeJudge(*d.Judge))
	}
	if len(tasks) == 0 && len(eligible) < tier.Tasks {
		fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("the %s tier asks for %d tasks; only %d can be in it", tier.Name, tier.Tasks, len(eligible))))
	}
	fmt.Fprintf(env.Stdout, "Preview what it costs and can detect: %s\n", st.Command("agentium experiment plan "+name))
	return ExitOK
}

// experimentArm resolves a context name: base, or a snapshot.
func experimentArm(ctx context.Context, w *workspace, name, contextName string) (experiment.Arm, error) {
	if contextName == experiment.BaseContext {
		return experiment.Arm{Name: name, Context: contextName}, nil
	}
	snap, err := w.db.SnapshotByName(ctx, w.project.ID, contextName)
	if err != nil {
		return experiment.Arm{}, err
	}
	return experiment.Arm{Name: name, Context: contextName, Snapshot: snap.CommitID}, nil
}

// eligibleTasks returns the names of the tasks that can be in an experiment with these arms, and why each other
// task cannot.
func eligibleTasks(ctx context.Context, w *workspace, arms []experiment.Arm) ([]string, map[string]string, error) {
	all, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return nil, nil, err
	}
	var eligible []string
	reasons := map[string]string{}
	for _, t := range all {
		c := experiment.Candidate{Name: t.Name, NeedsReview: t.NeedsReview, Grading: t.Grading}
		if t.Validation != nil {
			var v task.Validation
			if err := json.Unmarshal(t.Validation, &v); err != nil {
				return nil, nil, fmt.Errorf("task %s: validation: %w", t.Name, err)
			}
			c.Validation = &v
		}
		if why := experiment.Ineligible(c, arms); why != "" {
			reasons[t.Name] = why
		} else {
			eligible = append(eligible, t.Name)
		}
	}
	return eligible, reasons, nil
}

func printIneligible(w io.Writer, reasons map[string]string) {
	for _, name := range slices.Sorted(maps.Keys(reasons)) {
		fmt.Fprintf(w, "  %s: %s\n", name, reasons[name])
	}
}

// estimateRun estimates runs on model from the project's earlier fair task runs on it that reported their cost (a run
// stopped before Claude Code's result has none): each task from its own runs, when it has some. A run is a task's own
// only while it is linked to it: a removed task's runs, and an experiment's runs of a task changed after the lock, are
// not, so a task imported again under the same name starts without history. A task edited in place keeps its ID, and
// so its earlier runs.
func estimateRun(ctx context.Context, w *workspace, model string) (experiment.Estimate, error) {
	runs, err := w.db.Runs(ctx, w.project.ID)
	if err != nil {
		return experiment.Estimate{}, err
	}
	var past []experiment.PastRun
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
		// The agent's cost alone: what the judge spends is estimated apart (Design.JudgeEstimateUSD).
		agent := storedSpend(r).AgentUSD
		if json.Unmarshal(r.Record, &rec) == nil && rec.Model == model && rec.Metrics.SawResult && agent > 0 {
			own := ""
			if r.TaskID != 0 {
				own = r.TaskName
			}
			past = append(past, experiment.PastRun{Task: own, CostUSD: agent})
		}
	}
	return experiment.EstimateRun(model, past), nil
}

// describeJudge is the judge's settings in words.
func describeJudge(s llmjudge.Settings) string {
	return fmt.Sprintf("%s at effort %s, %d call(s) per run", s.Model, s.Effort, s.Repeats)
}

func describeArms(d experiment.Design) string {
	if d.Template == experiment.TemplateAA {
		return "A/A calibration of context " + d.Arms[0].Context
	}
	return fmt.Sprintf("context A/B, A = %s, B = %s", d.Arms[0].Context, d.Arms[1].Context)
}

// loadExperiment reads a stored experiment's design.
func loadExperiment(ctx context.Context, w *workspace, name string) (experiment.Design, error) {
	stored, err := w.db.ExperimentByName(ctx, w.project.ID, name)
	if err != nil {
		return experiment.Design{}, err
	}
	var d experiment.Design
	if err := json.Unmarshal(stored.Design, &d); err != nil {
		return experiment.Design{}, fmt.Errorf("experiment %s: %w", name, err)
	}
	if d.Version != experiment.DesignVersion || len(d.Arms) != 2 {
		return experiment.Design{}, fmt.Errorf("experiment %s: its design (version %d) is not one this Agentium reads", name, d.Version)
	}
	return d, nil
}

func experimentPlan(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("experiment plan", flag.ContinueOnError), args, experimentUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		fmt.Fprint(env.Stderr, experimentUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	d, err := loadExperiment(ctx, w, rest[0])
	if err != nil {
		return fail(env, err)
	}
	eligible, reasons, err := eligibleTasks(ctx, w, d.Arms)
	if err != nil {
		return fail(env, err)
	}
	est, err := estimateRun(ctx, w, d.Model)
	if err != nil {
		return fail(env, err)
	}
	out, st := env.Stdout, env.style()
	goal := "cheaper, with success as the guard"
	if d.Goal == experiment.GoalBetter {
		goal = "better success"
	}
	fmt.Fprintln(out, st.Heading(fmt.Sprintf("Experiment %s: %s", rest[0], describeArms(d))))
	for _, a := range d.Arms {
		commit := ""
		if a.Snapshot != "" {
			commit = " (" + shortCommit(a.Snapshot) + ")"
		}
		fmt.Fprintf(out, "  arm %s: context %s%s\n", a.Name, a.Context, commit)
	}
	effort := d.Effort
	if effort == "" {
		effort = "the CLI's default"
	}
	fmt.Fprintf(out, "  model %s, effort %s; each run up to $%.2f and %s; %d at a time\n", d.Model, effort, d.RunBudgetUSD, d.Timeout, d.Concurrency)
	fmt.Fprintf(out, "  goal: %s (margins: cost %.0f%%, success %.0f pp); budget $%.2f\n", goal, 100*d.CostMargin, 100*d.SuccessMargin, d.BudgetUSD)
	fmt.Fprintf(out, "  tasks (%d, seed %d): %s\n", len(d.Tasks), d.Seed, strings.Join(d.Tasks, ", "))
	if d.Judge != nil {
		fmt.Fprintf(out, "  judge: %s; each run's judgement up to $%.2f; a second opinion, it decides nothing\n", describeJudge(*d.Judge), d.JudgeCapUSD())
	}

	fmt.Fprintln(out, "\n"+st.Heading("Before it runs:"))
	ready := printReadiness(ctx, env, w, d, eligible, reasons, est)
	if ctx.Err() != nil {
		return fail(env, ctx.Err())
	}

	fmt.Fprintln(out, "\n"+st.Heading("Sizes (runs count both arms):"))
	sizes := term.NewTable(st, term.Left("SIZE"), term.Right("TASKS"), term.Right("RUNS/ARM"), term.Right("RUNS"), term.Right("EST. COST"),
		term.Right("WORST CASE"), term.Right("COST CHANGE"), term.Right("SUCCESS CHANGE"), term.Right("NO-LOSS GUARD"), term.Left("EXPLORATORY"))
	rows := experiment.Preview(d, eligible, est)
	marked := false // a row's task count carries a footnote mark; the others get a space so the digits line up
	for _, r := range rows {
		marked = marked || r.Short
	}
	for i, r := range rows {
		tasks := fmt.Sprint(r.Tasks)
		if r.Short {
			tasks += "*"
		} else if marked {
			tasks += " "
		}
		cost := "unknown"
		if r.CostKnown {
			cost = fmt.Sprintf("$%.2f", r.CostUSD+r.JudgeUSD) // the judge's share is stated below the table
		}
		effects := []string{percentRange(r.Detect.Cost, "%"), percentRange(r.Detect.Success, " pp"), percentRange(r.Detect.Guard, " pp")}
		exploratory := strings.Join(r.Exploratory, ", ")
		if r.Tasks == 0 {
			effects, exploratory = []string{"-", "-", "-"}, st.Warn("no tasks")
		} else if exploratory == "" {
			exploratory = "-"
		} else {
			exploratory = st.Warn(exploratory)
		}
		name := r.Name
		if i == len(rows)-1 { // this experiment's own size
			name = st.Heading(name)
		}
		sizes.Row(name, tasks, strconv.Itoa(r.Repeats), strconv.Itoa(r.Runs), cost, fmt.Sprintf("$%.2f", r.WorstUSD),
			effects[0], effects[1], effects[2], exploratory)
	}
	if err := sizes.Write(out); err != nil {
		return fail(env, err)
	}
	tiers := experiment.Tiers()
	if len(eligible) < tiers[len(tiers)-1].Tasks {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("* only %d task(s) can be in this experiment; a tier asking for more uses them all", len(eligible))))
	}
	printCostBasis(out, st, d, eligible, est)
	if d.Judge != nil {
		j := d.Judge
		own := rows[len(rows)-1]
		agent := "unknown"
		if own.CostKnown {
			agent = fmt.Sprintf("$%.2f", own.CostUSD)
		}
		fmt.Fprintf(out, "The judge: about $%.2f for this experiment's %d runs × %d call(s) at $%.3f a call (EST. COST includes it; the agent's\n"+
			"runs are %s). $%.3f is the judge pilot's mean call on %s at effort %s, not a measure of this project.\n",
			own.JudgeUSD, own.Runs, j.Repeats, llmjudge.EstimateUSD, agent, llmjudge.EstimateUSD, llmjudge.DefaultModel, llmjudge.DefaultEffort)
	}
	if d.Judge == nil {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("Worst case: every run reaches its $%.2f cap. A run starts only when the spend so far and the caps of the runs in flight\n"+
			"leave room for its own cap, so spending never passes the $%.2f budget.", d.RunBudgetUSD, d.BudgetUSD)))
	} else {
		fmt.Fprintln(out, st.Note(fmt.Sprintf("Worst case: every run reaches its $%.2f cap, and its judgement $%.2f (%d call(s) at $%.2f, each asked twice at\n"+
			"most). A run starts only when the spend so far and the caps of the runs in flight leave room for its own cap, so\n"+
			"spending never passes the $%.2f budget.", d.RunBudgetUSD, d.JudgeCapUSD(), d.Judge.Repeats, llmjudge.CallCapUSD, d.BudgetUSD)))
	}
	fmt.Fprintln(out, st.Note(fmt.Sprintf("Detectable effects: 80%% power; changes two-sided at 5%%, the no-loss guard one-sided at 5%%. Planning defaults until an\n"+
		"A/A calibration measures this repository's: per-run log-cost spread σ = %.2f, success variance w = %.2f, and a spread of\n"+
		"the true effect across tasks τ = %.2f–%.2f (the range shown), in log cost and in success rate alike. Phase 0 measured\n"+
		"τ only for cost; the study assumed 0.05 for success, so the success columns lean cautious.",
		experiment.SigmaLogCost, experiment.WSuccess, experiment.TauLow, experiment.TauHigh)))
	if own := experiment.Detect(len(d.Tasks), d.Repeats); d.Goal == experiment.GoalCheaper && len(d.Tasks) > 0 && own.Guard[0] > d.SuccessMargin {
		fmt.Fprintln(out, note(st, fmt.Sprintf("at this size the no-loss guard certifies only about %s, wider than the %.0f pp success margin: expect the\n"+
			"success verdict to be inconclusive unless there is no real difference and the noise is low.", percentRange(own.Guard, " pp"), 100*d.SuccessMargin)))
	}
	floors := experiment.FloorsFor(experiment.MethodVersion)
	fmt.Fprintf(out, "Floors (method %s): verdicts on cost need %d tasks with %d or more runs per arm, and on success %d tasks with %d or more;\n"+
		"below them a metric is exploratory.\n", experiment.MethodVersion, floors.CostTasks, floors.CostRepeats, floors.SuccessTasks, floors.SuccessRepeats)
	runs, err := w.db.Runs(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	mode, _ := signInMode(env)
	printUsagePreview(out, st, runs, 2*len(d.Tasks)*d.Repeats, mode, defaultUsageLimit/100, env.Now())
	if !ready {
		fmt.Fprintln(out, st.Bad("Not ready to run: see above."))
	}
	return ExitOK
}

// printCostBasis says how each of the experiment's tasks is estimated: from its own earlier runs, or from the fallback
// for tasks without any. The tiers draw from the eligible tasks, so they average those tasks' estimates, which may
// include tasks outside the experiment: the note shows that average.
func printCostBasis(out io.Writer, st term.Style, d experiment.Design, eligible []string, est experiment.Estimate) {
	var own, other []string
	for _, t := range d.Tasks {
		if c, ok := est.Tasks[t]; ok {
			own = append(own, fmt.Sprintf("%s $%.2f (%d run(s))", t, c.PerRunUSD, c.Runs))
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
		if est.Known {
			fallback = fmt.Sprintf("$%.2f, %s", est.PerRunUSD, est.Basis)
		}
		fmt.Fprintf(out, "  %s, without runs of their own: %s\n", strings.Join(other, ", "), fallback)
	}
	if mean, known := est.MeanUSD(eligible); known && slices.ContainsFunc(eligible, func(t string) bool { _, ok := est.Tasks[t]; return ok }) {
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

// printReadiness checks what running needs: Claude Code, each context's calibration on its version and the
// experiment's model, the snapshot commits, and every task still eligible. It reports whether all is in place.
func printReadiness(ctx context.Context, env Env, w *workspace, d experiment.Design, eligible []string, reasons map[string]string,
	est experiment.Estimate) bool {
	out, st := env.Stdout, env.style()
	ready := true
	check := func(status, text string) {
		fmt.Fprintf(out, "  %s %s\n", st.Status(fmt.Sprintf("%-8s", status)), text)
	}
	line := func(ok bool, format string, a ...any) {
		ready = ready && ok
		check(map[bool]string{true: "ok", false: "MISSING"}[ok], fmt.Sprintf(format, a...))
	}
	mode, _ := signInMode(env)
	version := ""
	if cli, err := claudePath(env); err != nil {
		line(false, "%v", err)
	} else if version, err = project.ClaudeVersion(ctx, cli); err != nil {
		line(false, "Claude Code at %s: its version could not be read: %v", cli, err)
	} else {
		line(true, "Claude Code %s at %s", version, cli)
	}
	var seen []experiment.Arm
	for _, a := range d.Arms {
		if slices.ContainsFunc(seen, func(s experiment.Arm) bool { return s.Context == a.Context && s.Snapshot == a.Snapshot }) {
			continue
		}
		seen = append(seen, a)
		if a.Snapshot != "" {
			if _, err := gitx.Run(ctx, "--git-dir", w.bare, "cat-file", "-e", a.Snapshot+"^{commit}"); err != nil {
				line(false, "context %s: its snapshot commit %s is gone from Agentium's repository", a.Context, shortCommit(a.Snapshot))
				continue
			}
		}
		calibrate := "agentium run calibrate --model " + d.Model
		if a.Snapshot != "" {
			calibrate += " --snapshot " + a.Context
		}
		calibrate = st.Command(calibrate)
		stored, err := w.db.LatestCalibration(ctx, w.project.ID, a.Context, a.Snapshot)
		if errors.Is(err, store.ErrNotFound) {
			line(false, "context %s is not calibrated: %s", a.Context, calibrate)
			continue
		}
		if err != nil {
			line(false, "context %s: %v", a.Context, err)
			continue
		}
		var c calibration
		if err := json.Unmarshal(stored.Result, &c); err != nil {
			line(false, "context %s: its calibration cannot be read: %v", a.Context, err)
			continue
		}
		if c.SignIn == "" { // saved before calibrations recorded it: the calibration run's record has it
			if r, err := w.db.RunByID(ctx, w.project.ID, stored.RunID); err == nil {
				var rec struct {
					SignIn string `json:"sign_in"`
				}
				if json.Unmarshal(r.Record, &rec) == nil {
					c.SignIn = rec.SignIn
				}
			}
		}
		switch {
		case version != "" && c.CLIVersion != version:
			line(false, "context %s was calibrated on Claude Code %s, not %s: %s", a.Context, c.CLIVersion, version, calibrate)
		case c.RequestedModel != d.Model:
			line(false, "context %s was calibrated with %s, not %s: %s", a.Context, orNone(c.RequestedModel), d.Model, calibrate)
		case c.SignIn != mode:
			line(false, "context %s was calibrated with sign-in %s, and runs would now use %s: %s", a.Context, orNone(c.SignIn), mode, calibrate)
		default:
			line(true, "context %s calibrated %s: first request %d tokens", a.Context, stored.CreatedAt.Format("2006-01-02 15:04"), c.FirstRequest)
		}
	}
	var missing []string
	for _, t := range d.Tasks {
		if why, known := reasons[t]; known {
			line(false, "task %s: %s", t, why)
		} else if !slices.Contains(eligible, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		line(false, "task(s) removed since the experiment was made: %s", strings.Join(missing, ", "))
	}
	var unfair, unchecked, few, unreadable []string
	fair := task.NewFairness("--git-dir", w.bare)
	for _, name := range d.Tasks {
		t, err := w.db.TaskByName(ctx, w.project.ID, name)
		if err != nil {
			continue // reported above as removed
		}
		if gaps, err := taskGaps(ctx, fair, t); err != nil {
			unchecked = append(unchecked, name)
		} else if len(gaps) > 0 {
			unfair = append(unfair, fmt.Sprintf("%s (%d)", name, len(gaps)))
		}
		if t.Validation != nil && slices.Contains(eligible, name) {
			var v task.Validation
			if err := json.Unmarshal(t.Validation, &v); err != nil {
				unreadable = append(unreadable, name)
			} else if v.RepeatCount() < minValidateRepeats {
				few = append(few, name)
			}
		}
	}
	if len(unfair) > 0 {
		check("WARNING", "hidden tests require what nothing states, so a fair agent may fail them (task show lists it): "+strings.Join(unfair, ", "))
	}
	if len(unchecked) > 0 {
		check("WARNING", "what the hidden tests require could not be checked for: "+strings.Join(unchecked, ", "))
	}
	if len(unreadable) > 0 {
		check("WARNING", "the validation of these tasks cannot be read, so their repeats are unknown: "+strings.Join(unreadable, ", "))
	}
	if len(few) > 0 {
		check("WARNING", fmt.Sprintf("validated with fewer than %d repeats, so flaky checks may not show yet: %s (%s)", minValidateRepeats,
			strings.Join(few, ", "), st.Command(fmt.Sprintf("agentium task validate NAME --repeat %d%s", minValidateRepeats, snapshotFlags(d.Arms)))))
	}
	if ready {
		check("ok", fmt.Sprintf("%d task(s), each valid in every arm's context", len(d.Tasks)))
	}
	expected, known := est.DesignUSD(d)
	expected += d.JudgeEstimateUSD()
	if reserve := experiment.Reserve(d); known && d.BudgetUSD < expected+reserve {
		check("WARNING", fmt.Sprintf("the budget $%.2f is below the estimated $%.2f plus $%.2f held for runs in flight: expect it to stop the experiment early",
			d.BudgetUSD, expected, reserve))
	}
	return ready
}

// minValidateRepeats is how many validation repeats experiment plan asks for before it stops warning.
const minValidateRepeats = 3

// snapshotFlags is the arms' " --snapshot NAME" flags for a task validate command.
func snapshotFlags(arms []experiment.Arm) string {
	var flags string
	var named []string
	for _, a := range arms {
		if a.Context != experiment.BaseContext && !slices.Contains(named, a.Context) {
			named = append(named, a.Context)
			flags += " --snapshot " + a.Context
		}
	}
	return flags
}

func experimentList(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("experiment list", flag.ContinueOnError), args, experimentUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 {
		fmt.Fprint(env.Stderr, experimentUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	all, err := w.db.Experiments(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	if len(all) == 0 {
		fmt.Fprintln(env.Stdout, "No experiments yet: "+env.style().Command("agentium experiment new NAME --b SNAPSHOT"))
		return ExitOK
	}
	st := env.style()
	table := term.NewTable(st, term.Left("NAME"), term.Left("TEMPLATE"), term.Left("ARMS (A / B)"), term.Left("SIZE"), term.Left("MODEL"),
		term.Right("BUDGET"), term.Left("STATUS"), term.Left("CREATED"))
	for _, e := range all {
		var d experiment.Design
		if err := json.Unmarshal(e.Design, &d); err != nil || len(d.Arms) != 2 {
			return fail(env, fmt.Errorf("experiment %s: unreadable design: %w", e.Name, errors.Join(err, errors.New("want two arms"))))
		}
		arms := d.Arms[0].Context + " / " + d.Arms[1].Context
		budget := d.BudgetUSD
		var lock experiment.Lock
		if e.Lock != nil && json.Unmarshal(e.Lock, &lock) == nil {
			budget = lock.Design.BudgetUSD // raised on resume
		}
		status := e.Status
		if status == store.StatusRunning && !w.layout.RunsBusy() {
			status = store.StatusStopped // its process ended without saying so
		}
		table.Row(e.Name, e.Template, arms, fmt.Sprintf("%d × %d", len(d.Tasks), d.Repeats), d.Model, fmt.Sprintf("$%.2f", budget),
			st.Status(status), e.CreatedAt.Format("2006-01-02 15:04"))
	}
	if err := table.Write(env.Stdout); err != nil {
		return fail(env, err)
	}
	return ExitOK
}

func experimentRemove(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("experiment rm", flag.ContinueOnError), args, experimentUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		fmt.Fprint(env.Stderr, experimentUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	stored, err := w.db.ExperimentByName(ctx, w.project.ID, rest[0])
	if err != nil {
		return fail(env, err)
	}
	if stored.Lock != nil {
		return fail(env, fmt.Errorf("experiment %s has run: it stays, with its lock and runs", rest[0]))
	}
	if err := w.db.DeleteExperiment(ctx, w.project.ID, rest[0]); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Removed experiment %s.\n", rest[0])
	return ExitOK
}
