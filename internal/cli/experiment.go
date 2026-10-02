package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/pigeaca/agentium/internal/experiment"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// experimentUsage is the help of experiment's subcommands. It is computed once from constants and never changed.
var experimentUsage = `Usage:
  agentium experiment new NAME --b SNAPSHOT [--a CONTEXT] [--task NAME...] [--model MODEL] [--effort LEVEL]
                     [--goal cheaper|better [--tier quick|confident] [--repeats N]] [--no-futility] [--run-budget USD]
                     [--budget USD] [--concurrency N] [--timeout DURATION] [--verify-timeout DURATION] [--seed N]
                     [--judge [--judge-repeats N]] [--judge-pairs] [--judge-model MODEL] [--judge-effort LEVEL]
                     a context A/B: arm A (default: base, each task's own context) against snapshot B
  agentium experiment new NAME --template aa [--a CONTEXT] [...]
                     an A/A calibration: one context in both arms, which must find no difference
  agentium experiment new NAME --template model-ab --a MODEL[:EFFORT] --b MODEL[:EFFORT] [--context SNAPSHOT] [--run-budget-a USD]
                     [--run-budget-b USD] [...]
                     a model A/B: two Claude Code profiles (a model, and an effort level: low, medium, high, xhigh or
                     max) on the same tasks in one context (default: base). Each arm's model is calibrated when the
                     experiment runs, if it is not yet
  agentium experiment plan NAME
                     the runs, the estimated cost (calibrations included) and the effects each size can detect; what is missing
  agentium experiment run NAME [--budget USD] [--usage-limit PCT] [--wait] [--yes]
                     lock the experiment (first time) and run it: real Claude Code runs, interleaved in pairs, within
                     the budget; infrastructure failures are retried. First, each context without a calibration on this Claude Code
                     and model is calibrated (a short paid run each, in the budget; a failed calibration stops the
                     experiment before any task run). Run it again to resume; --budget raises the total.
                     With a subscription, no pair starts past --usage-limit (default 85) of the five-hour window:
                     it pauses, or with --wait waits for the window to reset. --yes is consent to spend for a script:
                     with --json (one JSON document when the run ends, no progress), a run without --yes starts
                     nothing and exits 1; a person's own run needs no --yes
  agentium experiment show NAME
                     the lock and the progress per arm
  agentium experiment report NAME [--json | --markdown] [--out FILE]
                     verdicts, metrics with their intervals, per-task results, behavior, costs and notes (with
                     --judge, its verdicts too): styled for a terminal; as Markdown (for a pull request) when piped,
                     with --out or --markdown; or JSON (with the lock and every run)
  agentium experiment list
  agentium experiment rm NAME        (only one that has not run)

Every experiment command but report takes --json (one JSON document on stdout; see docs/guide.md, "Scripting and automation").

Tasks must be reviewed and valid in both arms' contexts (agentium task validate NAME --snapshot SNAPSHOT); --task picks
them (repeatable), else a seeded sample does. A cost experiment (--goal cheaper, the default) runs method seq-v1: up to
16 tasks × 1 run per arm in stages, with a look after 8, 12 and 16 tasks; it stops at the first look with a cost
verdict, or when one has become unlikely (futility; --no-futility turns that off). A success experiment (--goal better)
samples a tier: quick is 12 tasks × 3 runs per arm, confident 23 × 5.

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

--judge-pairs asks the pair judge, unvalidated, which of a pair's two changes is the better fix, when both runs pass
(a task's run in each arm with the same repeat index): in both orders, on --judge-model at --judge-effort; when the
orders disagree, the pair is a tie. It reads what --judge reads. Its preferences are exploratory: they never make a
verdict. Each pair is compared beside the runs, so it holds no run's slot and no look; the budget holds back %d calls
× $%.2f per pair for it at the default judge (another model or effort adds each call's overshoot allowance: experiment
plan states the cap), about $%.2f a pair at the pilot's mean. A pair judge that hits a usage limit pauses the
experiment as the judge does; queued comparisons wait while the experiment waits for the usage window, and a pause at
the usage limit leaves them for the resume.
`, llmjudge.DefaultRepeats, experiment.MaxJudgeRepeats, llmjudge.DefaultModel, llmjudge.DefaultEffort, llmjudge.CallCapUSD,
	int(llmjudge.CallTimeout.Minutes()), report.MaxFlagged, llmjudge.PairCalls, llmjudge.CallCapUSD, llmjudge.PairEstimateUSD)

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
	var o experiment.NewOptions
	fs.StringVar(&o.Template, "template", experiment.TemplateContextAB, "context-ab, aa or model-ab")
	var a, b, contextName string
	fs.StringVar(&a, "a", "", "arm A's context: base (default) or a snapshot; with model-ab, its MODEL[:EFFORT]")
	fs.StringVar(&b, "b", "", "arm B's context: a snapshot (context-ab only); with model-ab, its MODEL[:EFFORT]")
	fs.StringVar(&contextName, "context", "", "model-ab only: the context both arms run: base (default) or a snapshot")
	fs.Float64Var(&o.RunBudgetA, "run-budget-a", 0, "model-ab only: stop arm A's runs at this cost in USD (default --run-budget)")
	fs.Float64Var(&o.RunBudgetB, "run-budget-b", 0, "model-ab only: stop arm B's runs at this cost in USD (default --run-budget)")
	fs.StringVar(&o.Tier, "tier", "", "--goal better: quick (12 tasks × 3 runs) or confident (23 × 5); default quick unless --task is given")
	var tasks stringList
	fs.Var(&tasks, "task", "a task to include (repeatable; instead of a sample)")
	fs.IntVar(&o.Repeats, "repeats", 0, "--goal better: runs per task per arm (default: the tier's, or 3); a cost experiment runs 1")
	fs.BoolVar(&o.NoFutility, "no-futility", false, "a cost experiment: no futility stops (it then runs to a verdict or its last look)")
	fs.StringVar(&o.Model, "model", experiment.DefaultExperimentModel, "the model")
	fs.StringVar(&o.Effort, "effort", "", "the effort level (default: the CLI's)")
	fs.StringVar(&o.Goal, "goal", experiment.GoalCheaper, "cheaper (cost, with success as the guard) or better (success)")
	fs.Float64Var(&o.RunBudget, "run-budget", experiment.DefaultRunBudgetUSD, "stop each run at this cost in USD")
	fs.Float64Var(&o.Budget, "budget", 0, "stop the experiment at this total in USD (default: a quarter above the estimate)")
	fs.IntVar(&o.Concurrency, "concurrency", experiment.DefaultConcurrency, "runs at a time")
	fs.DurationVar(&o.Timeout, "timeout", experiment.DefaultRunTimeout, "stop each run after this long")
	fs.DurationVar(&o.VerifyTimeout, "verify-timeout", experiment.DefaultVerifyTimeout, "time limit for each setup or verification command")
	fs.Uint64Var(&o.Seed, "seed", 0, "the seed for the task sample and the run order (default: random)")
	fs.BoolVar(&o.Judge, "judge", false, "ask the LLM judge about every graded run (a second opinion; it decides nothing)")
	fs.BoolVar(&o.JudgePairs, "judge-pairs", false, "ask the pair judge which arm fixed each task better, when both pass (unvalidated, exploratory)")
	fs.StringVar(&o.JudgeModel, "judge-model", "", "the judges' model (default "+llmjudge.DefaultModel+")")
	fs.StringVar(&o.JudgeEffort, "judge-effort", "", "the judges' effort level (default "+llmjudge.DefaultEffort+")")
	fs.IntVar(&o.JudgeRepeats, "judge-repeats", 0, fmt.Sprintf("the judge's calls per run, the majority wins (default %d, at most %d)", llmjudge.DefaultRepeats, experiment.MaxJudgeRepeats))
	rest, code, ok := parseArgs(env, fs, args, experimentUsage)
	if !ok {
		return code
	}
	name, ok := oneName(env, "experiment new", rest, experimentUsage)
	if !ok {
		return ExitUsage
	}
	o.Tasks = tasks
	if err := setExperimentArms(&o, fs, a, b, contextName); err != nil {
		return failNew(env, err)
	}
	if err := o.Prepare(name); err != nil {
		return failNew(env, err)
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	created, err := experiment.Create(ctx, w.service(), name, o, env.Now())
	if err != nil {
		return failNew(env, err)
	}
	if env.JSON {
		return env.emit(newDocument(name, created))
	}
	created.Write(env.Stdout, env.style(), name)
	return ExitOK
}

// setExperimentArms puts --a, --b and --context where the template reads them: contexts for the context templates,
// models for model-ab (whose --model and --effort, left at their defaults, are the arms' own to set).
func setExperimentArms(o *experiment.NewOptions, fs *flag.FlagSet, a, b, contextName string) error {
	if o.Template != experiment.TemplateModelAB {
		if contextName != "" {
			return experiment.UsageError("--context belongs to the model-ab template; the others take --a and --b")
		}
		if o.ContextA, o.ContextB = a, b; a == "" {
			o.ContextA = experiment.BaseContext
		}
		return nil
	}
	o.ProfileA, o.ProfileB, o.ContextA = a, b, contextName
	if contextName == "" {
		o.ContextA = experiment.BaseContext
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if !given["model"] {
		o.Model = ""
	}
	return nil
}

// failNew reports what experiment new's services return: a usage error under the command's name, the tasks that cannot
// be in the experiment with their reasons, or a runtime failure.
func failNew(env Env, err error) int {
	var usage experiment.UsageError
	var none *experiment.NoTasksError
	switch {
	case errors.As(err, &usage):
		fmt.Fprintf(env.Stderr, "agentium experiment new: %s\n", usage)
		return ExitUsage
	case errors.As(err, &none):
		experiment.WriteIneligible(env.Stderr, none.Reasons)
	}
	return fail(env, err)
}

func experimentPlan(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("experiment plan", flag.ContinueOnError), args, experimentUsage)
	if !ok {
		return code
	}
	if _, ok := oneName(env, "experiment plan", rest, experimentUsage); !ok {
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	review, err := experiment.LoadReview(ctx, w.service(), readinessEnv(env), rest[0])
	if err != nil {
		return fail(env, err)
	}
	if env.JSON {
		doc, err := planDocument(env, rest[0], review)
		if err != nil {
			return fail(env, err)
		}
		return env.emit(doc)
	}
	mode, _ := signInMode(env)
	if err := review.Write(ctx, env.Stdout, env.style(), rest[0], mode, env.Now()); err != nil {
		return fail(env, err)
	}
	return ExitOK
}

// readinessEnv is what the readiness checks need from the machine: where Claude Code is, how runs would sign in, and
// how to style the commands they suggest.
func readinessEnv(env Env) experiment.ReadinessEnv {
	mode, _ := signInMode(env)
	return experiment.ReadinessEnv{Claude: func() (string, error) { return claudePath(env) }, SignIn: mode, Style: env.style()}
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
	if len(all) == 0 && !env.JSON {
		fmt.Fprintln(env.Stdout, "No experiments yet: "+env.style().Command("agentium experiment new NAME --b SNAPSHOT"))
		return ExitOK
	}
	st := env.style()
	entries := []experimentListEntry{}
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
		entry := experimentListEntry{Name: e.Name, Template: e.Template, Goal: d.Goal, Method: d.LockMethod(), Arms: []listArmDoc{}, Tasks: len(d.Tasks),
			RepeatsPerArm: d.Repeats, Model: listModel(d), BudgetUSD: budget, Status: storedStatus(w, e), Created: e.CreatedAt}
		for _, a := range d.Arms {
			entry.Arms = append(entry.Arms, listArmDoc{Name: a.Name, Context: a.Context})
		}
		entries = append(entries, entry)
		table.Row(e.Name, e.Template, arms, fmt.Sprintf("%d × %d", len(d.Tasks), d.Repeats), listModel(d), fmt.Sprintf("$%.2f", budget),
			st.Status(status), e.CreatedAt.Format("2006-01-02 15:04"))
	}
	if env.JSON {
		return env.emit(experimentListDoc{header: hdr("experiment list"), Experiments: entries})
	}
	if err := table.Write(env.Stdout); err != nil {
		return fail(env, err)
	}
	return ExitOK
}

// listModel is the model column: the design's, or each arm's profile in a model-ab experiment.
func listModel(d experiment.Design) string {
	if !d.PerArmProfiles() {
		return d.Model
	}
	return experiment.Profile(d.Arms[0].Model, d.Arms[0].Effort) + " / " + experiment.Profile(d.Arms[1].Model, d.Arms[1].Effort)
}

func experimentRemove(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("experiment rm", flag.ContinueOnError), args, experimentUsage)
	if !ok {
		return code
	}
	if _, ok := oneName(env, "experiment rm", rest, experimentUsage); !ok {
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
	if env.JSON {
		return env.emit(experimentRemoveDoc{header: hdr("experiment rm"), Removed: rest[0]})
	}
	fmt.Fprintf(env.Stdout, "Removed experiment %s.\n", rest[0])
	return ExitOK
}
