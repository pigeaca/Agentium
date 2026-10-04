package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"slices"

	"github.com/pigeaca/agentium/internal/experiment"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// experimentUsage is the help of experiment's subcommands. It is computed once from constants and never changed. It
// lists no expert flag (experimentHidden): the guide's "Advanced flags" table does.
var experimentUsage = `Usage:
  agentium experiment new NAME [--b SNAPSHOT|MODEL[:EFFORT]] [--a CONTEXT|MODEL[:EFFORT]] [--context SNAPSHOT]
                     [--task NAME...] [--model MODEL[:EFFORT]] [--goal cheaper|better] [--run-budget USD] [--budget USD]
                     [--judge[=MODEL[:EFFORT]]] [--judge-pairs[=MODEL[:EFFORT]]] [--grader sandbox|host]
                     --b decides what the experiment compares:
                       no --b             an A/A calibration: one context (--a, default base: each task's own
                                          context) in both arms, which must find no difference
                       --b SNAPSHOT       a context A/B: arm A's context (--a, default base) against snapshot B
                       --b MODEL[:EFFORT] a model A/B: two Claude Code profiles, arm A's (--a, default --model) against
                                          B's, on the same tasks in one context (--context, default base). A model is
                                          one Agentium's price table knows, or a claude-… model ID; an effort is low,
                                          medium, high, xhigh or max (default: the CLI's). Each arm's model is
                                          calibrated when the experiment runs, if it is not yet
                     A --b that names both a snapshot and a model is refused. --model (default ` + experiment.DefaultExperimentModel + `) is
                     the model a context experiment runs on; --run-budget stops each run, in both arms, at its cost.
                     --grader is where its runs are graded: sandbox (Agentium's grading sandbox, the default on
                     macOS: no network but this machine's, writes only to the grade's own folders) or host (unsandboxed,
                     the default elsewhere). The lock fixes it; tasks validated on the host are validated again in the
                     sandbox when it locks
  agentium experiment plan NAME [--details]
                     the runs, the estimated cost (calibrations included) and the effects each size can detect; what is missing
  agentium experiment run NAME [--budget USD] [--usage-limit PCT] [--wait] [--yes] [--view dashboard|log]
                     lock the experiment (first time) and run it: real Claude Code runs, interleaved in pairs, within
                     the budget; infrastructure failures are retried. First, each context without a calibration on this Claude Code
                     and model is calibrated (a short paid run each, in the budget; a failed calibration stops the
                     experiment before any task run). Run it again to resume; --budget raises the total.
                     With a subscription, no pair starts past --usage-limit (default 85) of the five-hour window:
                     it pauses, or with --wait waits for the window to reset. --yes is consent to spend for a script:
                     with --json (one JSON document when the run ends, no progress), a run without --yes starts
                     nothing and exits 1; a person's own run needs no --yes. On a terminal it shows a live dashboard
                     (each arm's run as step boxes, the answer so far, the log); --view log, or AGENTIUM_VIEW=log,
                     prints styled lines instead, nothing redrawn. Piped, or with NO_COLOR, it prints plain lines
  agentium experiment show NAME
                     the lock and the progress per arm
  agentium experiment report NAME [--details | --json | --markdown] [--out FILE]
                     on a terminal, the answer in plain words: how sure it is, each arm's passes, cost and time, the
                     tasks where the arms differ, the judges' opinion; --details shows every number instead (metrics
                     with their intervals, per-task results, behavior, costs, notes). As Markdown (for a pull
                     request) when piped, with --out or --markdown; or JSON (with the lock and every run)
  agentium experiment list
  agentium experiment rm NAME        (only one that has not run)

Every experiment command but report takes --json (one JSON document on stdout; see docs/guide.md, "Scripting and automation").

Tasks must be reviewed and valid in both arms' contexts (agentium task validate NAME --snapshot SNAPSHOT); --task picks
them (repeatable), else a seeded sample does. A judge-graded task (a ticket's, whose fix has no tests) is in an
experiment only when --task names it: the judge's majority of 5 calls grades its runs (each run's grading held at $5.00
in the budget), unvalidated and reported apart as "the judge says fixed", never in success, a verdict or the north star;
a judge error leaves a grade pending (graded again from the run's change, never by running the agent again), and a
refusal or a tie leaves the run out. A cost experiment (--goal cheaper, the default) runs method seq-v1: up to
16 tasks × 1 run per arm in stages, with a look after 8, 12 and 16 tasks; it stops at the first look with a cost
verdict, or when one has become unlikely (futility). A success experiment (--goal better) runs 12 tasks × 3 runs per
arm. Expert flags (a larger sample, repeats, the seed, timeouts, concurrency) are in docs/guide.md, "Advanced flags".

` + fmt.Sprintf(`--judge asks an LLM judge about every graded run: does its change do what the task asks, as the task's reference
solution does? It reads the instruction and both changes' code (never the tests), %d times (the majority wins) on
%s at effort %s, or on --judge=MODEL[:EFFORT]. Its verdict (fixed, partly or no, with a reason) is a
second opinion beside the tests: it decides nothing. Its calls count against the budget, which holds back %d × 2 ×
$%.2f per run for them, but never toward an arm's cost. Tasks without a reference solution in code are not judged.
Judging can hold a run's slot for up to %d minutes more. The usage limit's per-run estimate leaves out the judge's use
of a subscription; a judge that hits a usage limit pauses the experiment, and --wait does not wait for it. The
report's Judge section shows each arm's verdicts among passing and failing runs with 95%% intervals, the runs not
judged and why, how often the repeats agreed, the judge's cost, and the passing runs it did not call fixed, with its
reasons (the first %d; --json lists them all).

--judge-pairs asks the pair judge, unvalidated, which of a pair's two changes is the better fix, when both runs pass
(a task's run in each arm with the same repeat index): in both orders, on --judge's model (default %s at
effort %s), or on --judge-pairs=MODEL[:EFFORT]; when the orders disagree, the pair is a tie. It reads what
--judge reads. Its preferences are exploratory: they never make a verdict. Each pair is compared beside the runs, so
it holds no run's slot and no look; the budget holds back %d calls × $%.2f per pair for it at the default judge
(another model or effort adds each call's overshoot allowance: experiment plan states the cap), about $%.2f a pair at
the pilot's mean. A pair judge that hits a usage limit pauses the experiment as the judge does; queued comparisons
wait while the experiment waits for the usage window, and a pause at the usage limit leaves them for the resume.
The report counts one vote per task (the arm its comparisons preferred more often) and says "too few to say" below
%d tasks with a preference; its Judge pairs section gives each task's vote with a reason.
`, llmjudge.DefaultRepeats, llmjudge.DefaultModel, llmjudge.DefaultEffort, llmjudge.DefaultRepeats, llmjudge.CallCapUSD,
	llmjudge.DefaultRepeats*int(llmjudge.CallTimeout.Minutes()), report.MaxFlagged, llmjudge.DefaultModel, llmjudge.DefaultEffort,
	llmjudge.PairCalls, llmjudge.CallCapUSD, llmjudge.PairEstimateUSD, llmjudge.MinPreferences)

// experimentHidden are experiment new's expert flags: they parse, but experimentUsage leaves them out (docs/guide.md,
// "Advanced flags", lists each with its default).
var experimentHidden = []string{"tier", "repeats", "no-futility", "concurrency", "timeout", "verify-timeout", "seed"}

// experimentRemoved are experiment new's removed flags, each with what replaces it.
var experimentRemoved = map[string]string{
	"template":      "--b decides it: no --b for an A/A, --b SNAPSHOT for a context A/B, --b MODEL[:EFFORT] for a model A/B",
	"effort":        "put the effort in --model: --model MODEL:EFFORT",
	"run-budget-a":  "--run-budget stops each run, in both arms",
	"run-budget-b":  "--run-budget stops each run, in both arms",
	"judge-model":   "name the judge in its flag: --judge=MODEL[:EFFORT] or --judge-pairs=MODEL[:EFFORT]",
	"judge-effort":  "name the judge in its flag: --judge=MODEL:EFFORT or --judge-pairs=MODEL:EFFORT",
	"judge-repeats": fmt.Sprintf("the judge asks %d times per run (the majority wins)", llmjudge.DefaultRepeats),
}

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
	var a, b, contextName, model string
	fs.StringVar(&a, "a", "", "arm A: its context (base, the default, or a snapshot); in a model A/B, its MODEL[:EFFORT] (default --model)")
	fs.StringVar(&b, "b", "", "arm B, which decides the template: none (A/A), a snapshot (context A/B) or MODEL[:EFFORT] (model A/B)")
	fs.StringVar(&contextName, "context", "", "a model A/B's one context: base (default) or a snapshot")
	var tasks stringList
	fs.Var(&tasks, "task", "a task to include (repeatable; instead of a sample)")
	fs.StringVar(&model, "model", experiment.DefaultExperimentModel, "the model, MODEL[:EFFORT] (effort default: the CLI's)")
	fs.StringVar(&o.Goal, "goal", experiment.GoalCheaper, "cheaper (cost, with success as the guard) or better (success)")
	fs.Float64Var(&o.RunBudget, "run-budget", experiment.DefaultRunBudgetUSD, "stop each run at this cost in USD")
	fs.Float64Var(&o.Budget, "budget", 0, "stop the experiment at this total in USD (default: a quarter above the estimate)")
	judge, pairs := judgeFlag{name: "judge"}, judgeFlag{name: "judge-pairs"}
	fs.Var(&judge, "judge", "ask the LLM judge about every graded run, on its default model or MODEL[:EFFORT] (a second opinion)")
	fs.Var(&pairs, "judge-pairs", "ask the pair judge which arm fixed each task better, when both pass (unvalidated, exploratory)")
	grader := addGraderFlag(fs)
	// Hidden (experimentHidden): the guide's "Advanced flags".
	fs.StringVar(&o.Tier, "tier", "", "--goal better: quick (12 tasks × 3 runs) or confident (23 × 5); default quick unless --task is given")
	fs.IntVar(&o.Repeats, "repeats", 0, "--goal better: runs per task per arm (default: the tier's, or 3); a cost experiment runs 1")
	fs.BoolVar(&o.NoFutility, "no-futility", false, "a cost experiment: no futility stops (it then runs to a verdict or its last look)")
	fs.IntVar(&o.Concurrency, "concurrency", experiment.DefaultConcurrency, "runs at a time")
	fs.DurationVar(&o.Timeout, "timeout", experiment.DefaultRunTimeout, "stop each run after this long")
	fs.DurationVar(&o.VerifyTimeout, "verify-timeout", experiment.DefaultVerifyTimeout, "time limit for each setup or verification command")
	fs.Uint64Var(&o.Seed, "seed", 0, "the seed for the task sample and the run order (default: random)")
	removeFlags(fs, experimentRemoved)
	rest, code, ok := parseArgs(env, fs, args, experimentUsage)
	if !ok {
		return code
	}
	if (judge.on || pairs.on) && slices.ContainsFunc(rest, experiment.IsModel) { // a model given as a separate word
		fmt.Fprintln(env.Stderr, "agentium experiment new: --judge and --judge-pairs take their model after an equals sign: --judge=MODEL[:EFFORT]")
		return ExitUsage
	}
	name, ok := oneName(env, "experiment new", rest, experimentUsage)
	if !ok {
		return ExitUsage
	}
	o.Tasks = tasks
	mode, err := grader.mode(env)
	if err != nil {
		return failNew(env, experiment.UsageError(err.Error()))
	}
	o.Grader = mode
	o.Judge, o.JudgeModel, o.JudgeEffort = judge.on, judge.model, judge.effort
	o.JudgePairs, o.PairJudgeModel, o.PairJudgeEffort = pairs.on, pairs.model, pairs.effort
	if pairs.on && pairs.model == "" { // a bare --judge-pairs takes --judge's model and effort, as one --judge-model set both
		o.PairJudgeModel, o.PairJudgeEffort = judge.model, judge.effort
	}
	if err := setExperimentArms(&o, fs, a, b, contextName, model); err != nil {
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

// setExperimentArms sets the template --b implies (experiment.InferTemplate) and puts --a, --b, --context and --model
// where it reads them. An A/A or a context A/B compares contexts (--a, default base, and --b) on one MODEL[:EFFORT]
// (--model). A model A/B compares arm A's profile (--a, else --model) with arm B's (--b) in one context (--context,
// default base).
func setExperimentArms(o *experiment.NewOptions, fs *flag.FlagSet, a, b, contextName, model string) error {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	o.Template = experiment.InferTemplate(b)
	if o.Template != experiment.TemplateModelAB {
		switch {
		case given["context"] && b == "":
			return experiment.UsageError("--context is a model A/B's one context (--b MODEL[:EFFORT]); an A/A, without --b, runs --a's context in both arms")
		case given["context"]:
			return experiment.UsageError(fmt.Sprintf("--context is a model A/B's one context, but --b %s is not a model (one Agentium's price table knows, "+
				"or a claude-… model ID), so this is a context A/B: --a and --b name its contexts", b))
		}
		var err error
		if o.Model, o.Effort, err = experiment.ParseProfile(model); err != nil {
			return experiment.UsageError("--model " + err.Error())
		}
		if o.ContextA, o.ContextB = a, b; a == "" {
			o.ContextA = experiment.BaseContext
		}
		return nil
	}
	if a != "" && given["model"] {
		return experiment.UsageError(fmt.Sprintf("--b %s makes a model A/B, whose arm A runs --a, else --model: give one of them", b))
	}
	// Arm A's profile must read as a model, as --b's did: a context name in --a (or an alias) would otherwise become a
	// model that Claude Code is asked to run, after paid calibrations. One that does not parse is Prepare's to report.
	if m, _, err := experiment.ParseProfile(a); a != "" && err == nil && !experiment.IsModel(m) {
		return experiment.UsageError(fmt.Sprintf("--a %s is not a model: a model A/B's context is --context (a model is one Agentium's "+
			"price table knows, or a claude-… model ID)", a))
	}
	if m, _, err := experiment.ParseProfile(model); a == "" && err == nil && !experiment.IsModel(m) {
		return experiment.UsageError(fmt.Sprintf("--model %s is not a model Agentium reads as one, and arm A of this model A/B runs it: "+
			"use a model the price table knows, or a claude-… model ID", model))
	}
	o.ProfileA, o.ProfileB, o.ContextA = a, b, contextName
	if a == "" {
		o.ProfileA = model
		if experiment.SameProfile(model, b) { // say how --b was read: the design's profile check alone would not
			return experiment.UsageError(fmt.Sprintf("--b %s is a model, so this is a model A/B, and arm A runs --model (%s), the same profile: "+
				"give arm A another with --a MODEL[:EFFORT]", b, model))
		}
	}
	if contextName == "" {
		o.ContextA = experiment.BaseContext
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
	fs := flag.NewFlagSet("experiment plan", flag.ContinueOnError)
	details := fs.Bool("details", false, "on a terminal, print every line instead of the picture")
	rest, code, ok := parseArgs(env, fs, args, experimentUsage)
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
	if err := writeReview(ctx, env, review, rest[0], mode, *details); err != nil {
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
