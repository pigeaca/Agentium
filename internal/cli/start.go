package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
)

const startUsage = `Usage: agentium start [--yes] [--budget USD] [--b SNAPSHOT] [--accept-mined]

Goes from a repository to a previewed experiment, skipping every stage that is already done, so running it again resumes:
  1. registers the repository (as init);
  2. saves the committed context as the snapshot "baseline", if the project has none; arm A is "baseline" when it
     exists, else the newest snapshot that --b does not name;
  3. mines and validates tasks until 8 are ready, the experiment's cost floor, or the candidates run out;
  4. creates the experiment "quick-..." at the cost floor: 8 tasks × 1 run per arm. With --b it compares the context
     with that snapshot; without it, it is an A/A calibration of the context (both arms the same, which must find no
     difference: it measures this repository's noise, it does not compare contexts);
  5. prints its preview: runs, estimated cost, detectable effect and what is missing.

It stops before any paid run. --yes runs the experiment (as agentium experiment run NAME), which first calibrates each
context that lacks a calibration on this Claude Code and model (a paid run each, counted in the budget); on a terminal,
it asks instead. --budget raises the experiment's total in USD. Mined tasks wait for your review of their instructions for
solution leaks (agentium task show NAME, then agentium task edit NAME --reviewed). --accept-mined accepts, without
your review, the tasks start itself mined: it checks only solution headings, reference-file names and unstated test
requirements, so a message that explains the fix passes. Tasks from pull requests, tickets or task import are never
accepted. --b must name a snapshot; --budget can only raise an experiment's budget.
`

// startArgs is what start was asked for.
type startArgs struct {
	yes, acceptMined bool
	budget           float64
	b                string // the snapshot to compare the context with; "" for an A/A calibration
}

// errReported is returned by a stage that has already told the user why it stopped.
var errReported = errors.New("reported")

func runStart(ctx context.Context, env Env, args []string) int {
	var a startArgs
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.BoolVar(&a.yes, "yes", false, "run the experiment without asking (real, paid runs)")
	fs.BoolVar(&a.acceptMined, "accept-mined", false, "accept the tasks start mined without your review (only automatic checks)")
	fs.Float64Var(&a.budget, "budget", 0, "stop the experiment at this total in USD (default: a quarter above the estimate)")
	fs.StringVar(&a.b, "b", "", "compare the context with this snapshot (default: an A/A calibration)")
	rest, code, ok := parseArgs(env, fs, args, startUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 || a.budget < 0 || (a.b != "" && !snapshot.ValidName(a.b)) {
		fmt.Fprint(env.Stderr, startUsage)
		return ExitUsage
	}
	s := &starter{env: env, args: a, importedNow: map[string]bool{}, held: map[string]string{}}
	defer s.close()
	name, err := s.prepare(ctx)
	switch {
	case errors.Is(err, errReported):
		return ExitError
	case err != nil:
		var usage experiment.UsageError
		if errors.As(err, &usage) {
			fmt.Fprintf(env.Stderr, "agentium start: %s\n", usage)
			return ExitUsage
		}
		return failNew(env, err) // tasks that cannot be in the experiment, listed with their reasons
	case name == "": // fewer tasks than the floor: said already
		return ExitError
	}
	return s.finish(ctx, name)
}

// starter is one run of start: its arguments and the project it works on.
type starter struct {
	env  Env
	args startArgs
	w    *workspace
	// a and b are the snapshots the experiment compares: a always, b with --b.
	a, b string
	// mined holds the tasks start mined (now or earlier, checked against the project's tasks), records and dismissed are
	// the state file's content (see minedState), importedNow the tasks this run imported, held why --accept-mined held a
	// task back, and stopped why mining ended with too few tasks.
	mined       map[string]bool
	records     []minedRecord
	dismissed   map[string]bool
	importedNow map[string]bool
	held        map[string]string
	imported    int
	stopped     string
}

// notRegisteredError is openProject's failure for a repository that was not registered with init.
type notRegisteredError struct{ root string }

func (e notRegisteredError) Error() string {
	return fmt.Sprintf("%s is not registered: run `agentium init` first", e.root)
}

func (s *starter) close() {
	if s.w != nil {
		s.w.Close()
	}
}

// prepare runs the stages up to the experiment's creation and returns the experiment's name; "" means too few tasks.
// An experiment that exists already needs no tasks, so that stage is skipped then.
func (s *starter) prepare(ctx context.Context) (string, error) {
	if err := s.register(ctx); err != nil {
		return "", err
	}
	if err := s.chooseContexts(ctx); err != nil {
		return "", err
	}
	name := s.experimentName()
	if _, err := s.w.db.ExperimentByName(ctx, s.w.project.ID, name); err == nil {
		fmt.Fprintf(s.env.Stdout, "Tasks: skipped (the experiment exists)\nExperiment %s: exists (skipped)\n", name)
		return name, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	ready, err := s.supplyTasks(ctx)
	if err != nil || !ready {
		return "", err
	}
	return s.createExperiment(ctx)
}

// register opens the project, registering the repository first as init does when it is not.
func (s *starter) register(ctx context.Context) error {
	w, err := openProject(ctx, s.env)
	if err == nil {
		s.w = w
		fmt.Fprintf(s.env.Stdout, "Project %s: registered (skipped)\n", w.project.Name)
		return nil
	}
	var unregistered notRegisteredError
	if !errors.As(err, &unregistered) {
		return err
	}
	if code := runInit(ctx, s.env, nil); code != ExitOK {
		return errReported
	}
	s.w, err = openProject(ctx, s.env)
	return err
}

// chooseContexts picks the snapshots the experiment uses, saving "baseline" (the committed context) when none exists.
func (s *starter) chooseContexts(ctx context.Context) error {
	w, out := s.w, s.env.Stdout
	if s.args.b != "" {
		if _, err := w.db.SnapshotByName(ctx, w.project.ID, s.args.b); err != nil {
			return fmt.Errorf("--b: %w (save it with agentium context snapshot %s)", err, s.args.b)
		}
		s.b = s.args.b
	}
	snaps, err := w.db.Snapshots(ctx, w.project.ID)
	if err != nil {
		return err
	}
	// Arm A is the snapshot named baseline, the committed context start saved; without it, the newest other snapshot.
	var chosen *store.Snapshot
	for i := len(snaps) - 1; i >= 0; i-- { // newest first
		if snaps[i].Name == s.b {
			continue
		}
		if chosen == nil || snaps[i].Name == "baseline" {
			chosen = &snaps[i]
		}
	}
	if chosen != nil {
		s.a = chosen.Name
		fmt.Fprintf(out, "Context: snapshot %s from %s (skipped)\n", s.a, experiment.ShortCommit(chosen.SourceCommit))
		return s.noteDrift(ctx, *chosen)
	}
	if s.b == "baseline" {
		return errors.New(`--b baseline is the only snapshot: save the context to compare it with first (agentium context snapshot NAME), or drop --b`)
	}
	src, commit, err := w.read(ctx, "HEAD")
	if err != nil {
		return err
	}
	manifest, err := saveSnapshot(ctx, s.env, w, "baseline", "HEAD", commit, src, nil)
	if err != nil {
		return err
	}
	s.a = "baseline"
	fmt.Fprintf(out, "Context: saved snapshot baseline from HEAD (%s): %d file(s)\n", experiment.ShortCommit(commit), len(manifest.Files))
	return nil
}

// noteDrift says when the context committed at HEAD is no longer the snapshot's, which an experiment of that snapshot
// would not reflect.
func (s *starter) noteDrift(ctx context.Context, snap store.Snapshot) error {
	src, _, err := s.w.read(ctx, "HEAD")
	if err != nil {
		return err
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return err
	}
	var saved snapshot.Manifest
	if err := json.Unmarshal(snap.Manifest, &saved); err != nil {
		return fmt.Errorf("snapshot %s: %w", snap.Name, err)
	}
	// Read only: hash what HEAD's context loads and compare it with the snapshot's own files.
	var now, was []string
	for _, e := range resolved.Entries {
		data, err := src.ReadFile(e.Path)
		if err != nil {
			return err
		}
		now = append(now, fmt.Sprintf("%s %x", e.Path, sha256.Sum256(data)))
	}
	for _, f := range saved.Files {
		if f.Kind != snapshot.KindIncluded {
			was = append(was, f.Path+" "+f.SHA256)
		}
	}
	slices.Sort(now)
	slices.Sort(was)
	// Documents added with --include are not context files, but they are part of the version: compare them too, and a
	// document missing at HEAD counts as drift.
	drift := !slices.Equal(now, was)
	for _, f := range saved.Files {
		if f.Kind != snapshot.KindIncluded {
			continue
		}
		if data, err := src.ReadFile(f.Path); err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != f.SHA256 {
			drift = true
		}
	}
	if drift {
		fmt.Fprintln(s.env.Stdout, note(s.env.style(), fmt.Sprintf("the context committed at HEAD differs from snapshot %s: save it with agentium context snapshot NAME, "+
			"then agentium start --b NAME compares it with %s", snap.Name, snap.Name)))
	}
	return nil
}

// experimentName is stable for the same contexts, so running start again finds the experiment it made.
func (s *starter) experimentName() string {
	if s.b == "" {
		return "quick-aa-" + s.a
	}
	return "quick-" + s.a + "-vs-" + s.b
}

// createExperiment stores the cost experiment (method seq-v1) on up to its maximum of tasks unless it exists.
func (s *starter) createExperiment(ctx context.Context) (string, error) {
	w, out, name := s.w, s.env.Stdout, s.experimentName()
	floor := experiment.FloorsFor(experiment.MethodVersion)
	o := experiment.NewOptions{Template: experiment.TemplateAA, ContextA: s.a, Repeats: floor.CostRepeats, Model: experiment.DefaultExperimentModel,
		Goal: experiment.GoalCheaper, RunBudget: experiment.DefaultRunBudgetUSD, Budget: s.args.budget, Concurrency: experiment.DefaultConcurrency,
		Timeout: experiment.DefaultRunTimeout, VerifyTimeout: experiment.DefaultVerifyTimeout}
	if s.b != "" {
		o.Template, o.ContextB = experiment.TemplateContextAB, s.b
	}
	if err := o.Prepare(name); err != nil {
		return "", err
	}
	arms, err := s.arms(ctx)
	if err != nil {
		return "", err
	}
	eligible, _, err := w.service().EligibleTasks(ctx, arms)
	if err != nil {
		return "", err
	}
	o.Tasks = experiment.Sample(eligible, experiment.SeqTier().Tasks, o.Seed) // a cost experiment: up to its maximum, at least the floor
	created, err := experiment.Create(ctx, w.service(), name, o, s.env.Now())
	if err != nil {
		return "", err
	}
	d := created.Design
	fmt.Fprintf(out, "Experiment %s: created, %d task(s) × %d run per arm = %d runs, budget $%.2f\n", name, len(d.Tasks), d.Repeats, d.Runs(), d.BudgetUSD)
	return name, nil
}

// finish prints the preview and the north star, then runs the experiment if the user said so.
func (s *starter) finish(ctx context.Context, name string) int {
	env, w, st := s.env, s.w, s.env.style()
	stored, err := w.db.ExperimentByName(ctx, w.project.ID, name)
	if err != nil {
		return fail(env, err)
	}
	if stored.Status == store.StatusDone {
		fmt.Fprintf(env.Stdout, "\nExperiment %s has finished: %s\n", name, st.Command("agentium experiment report "+name))
		return s.northStar(ctx)
	}
	budget, err := s.effectiveBudget(ctx, stored, name)
	if err != nil {
		return s.failStart(err)
	}
	if s.b == "" {
		fmt.Fprintf(env.Stdout, "\n%s\n", st.Note("note: no second context was given, so this is an A/A calibration of "+s.a+": both arms run the same context and "+
			"should show no difference. It measures this repository's noise and checks the method; it does not compare contexts "+
			"(agentium start --b SNAPSHOT does). It never counts toward the first decisive verdict."))
	}
	fmt.Fprintln(env.Stdout)
	review, err := experiment.LoadReview(ctx, w.service(), readinessEnv(env), name)
	if err != nil {
		return fail(env, err)
	}
	mode, _ := signInMode(env)
	if err := review.Write(ctx, env.Stdout, st, name, mode, env.Now()); err != nil {
		return fail(env, err)
	}
	if budget.total != review.Design.BudgetUSD { // the preview above quotes the design's budget
		why := "raised earlier"
		if budget.raised {
			why = "raised by --budget"
		}
		fmt.Fprintf(env.Stdout, "Budget for this run: $%.2f (the design's $%.2f, %s)\n", budget.total, review.Design.BudgetUSD, why)
	}
	if code := s.northStar(ctx); code != ExitOK {
		return code
	}
	runCommand := "agentium experiment run " + name
	if s.args.budget > 0 {
		runCommand += " --budget " + strconv.FormatFloat(s.args.budget, 'f', -1, 64)
	}
	if !review.Readiness.Ready {
		fmt.Fprintf(env.Stdout, "Nothing was run: fix what is missing above, then %s (or %s)\n", st.Command("agentium start --yes"), st.Command(runCommand))
		if s.args.yes {
			return ExitError
		}
		return ExitOK
	}
	calibrating := calibrationNote(review.Readiness.Calibrations)
	if !s.args.yes && !s.confirm(ctx, budget.total, calibrating) {
		if ctx.Err() != nil {
			fmt.Fprintln(env.Stdout, "\nInterrupted: nothing was run.")
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "\nNothing was run%s. To run it (real Claude Code runs%s, up to $%.2f): %s\n", s.spentBefore(ctx, stored.ID), calibrating, budget.total, st.Command(runCommand))
		return ExitOK
	}
	runArgs := []string{name}
	if s.args.budget > 0 {
		runArgs = append(runArgs, "--budget", strconv.FormatFloat(s.args.budget, 'f', -1, 64))
	}
	s.w.Close()
	s.w = nil // experimentRun opens the project itself
	return experimentRun(ctx, env, runArgs)
}

// failStart reports an error from the stages after the preview's start: a usage error as one, the rest as failures.
func (s *starter) failStart(err error) int {
	var usage experiment.UsageError
	if errors.As(err, &usage) {
		fmt.Fprintf(s.env.Stderr, "agentium start: %s\n", usage)
		return ExitUsage
	}
	return fail(s.env, err)
}

// spentBefore is what "Nothing was run" adds: in this command only, when the experiment has runs from earlier ones.
func (s *starter) spentBefore(ctx context.Context, experimentID int64) string {
	runs, err := s.w.db.ExperimentRuns(ctx, experimentID)
	if err != nil {
		return " in this command" // what the experiment spent before is not known: claim nothing about it
	}
	calibrations, err := s.w.db.ExperimentCalibrationRuns(ctx, experimentID)
	if err != nil {
		return " in this command"
	}
	switch {
	case len(runs)+len(calibrations) == 0:
		return " and nothing was spent"
	case len(runs) == 0:
		return fmt.Sprintf(" in this command (the experiment has %d calibration run(s) from before)", len(calibrations))
	}
	return fmt.Sprintf(" in this command (the experiment has %d run(s) from before)", len(runs)+len(calibrations))
}

// budgetPlan is what a run of the experiment may spend in total.
type budgetPlan struct {
	current float64 // the lock's budget (raised on earlier resumes) or the design's
	total   float64 // current, raised to --budget
	raised  bool
}

// effectiveBudget works out what `experiment run` would be allowed to spend, so the preview, the prompt and the printed
// command quote the same number. A budget can only be raised: a lower --budget is a usage error, found before asking.
func (s *starter) effectiveBudget(ctx context.Context, stored store.Experiment, name string) (budgetPlan, error) {
	design, err := s.w.service().Load(ctx, name)
	if err != nil {
		return budgetPlan{}, err
	}
	b := budgetPlan{current: design.BudgetUSD}
	if stored.Lock != nil {
		var lock experiment.Lock
		if err := json.Unmarshal(stored.Lock, &lock); err != nil {
			return budgetPlan{}, fmt.Errorf("experiment %s: its lock cannot be read: %w", name, err)
		}
		b.current = lock.Design.BudgetUSD
	}
	b.total = b.current
	switch {
	case s.args.budget > b.current:
		b.total, b.raised = s.args.budget, true
	case s.args.budget > 0 && s.args.budget < b.current:
		return b, experiment.UsageError(fmt.Sprintf("--budget $%.2f is below the experiment's $%.2f: a budget can only be raised", s.args.budget, b.current))
	}
	return b, nil
}

// confirm asks whether to run the experiment, only when a person can answer: stdin and stdout are terminals. Ctrl-C
// while it waits for the answer counts as no.
func (s *starter) confirm(ctx context.Context, budget float64, calibrating string) bool {
	if !s.env.StdinTerminal || !s.env.Terminal || s.env.Stdin == nil {
		return false
	}
	fmt.Fprintf(s.env.Stdout, "\nRun it now? It makes real Claude Code runs%s and spends up to $%.2f. [y/N] ", calibrating, budget)
	type answer struct {
		line string
		err  error
	}
	got := make(chan answer, 1)
	go func() { // a blocked read cannot be cancelled: the process ends soon after, so the goroutine is left to it
		line, err := bufio.NewReader(io.LimitReader(s.env.Stdin, 1<<10)).ReadString('\n')
		got <- answer{line, err}
	}()
	select {
	case <-ctx.Done():
		return false
	case a := <-got:
		if a.err != nil && a.line == "" {
			return false
		}
		reply := strings.ToLower(strings.TrimSpace(a.line))
		return reply == "y" || reply == "yes"
	}
}

// calibrationNote words the calibrations the experiment makes first, for the consent: they are paid runs too.
func calibrationNote(needs []experiment.CalibrationNeed) string {
	if len(needs) == 0 {
		return ""
	}
	estimate, _ := experiment.CalibrationCosts(needs)
	return fmt.Sprintf(", first %d calibration run(s) of about $%.2f", len(needs), estimate)
}

// northStar prints the project's time and spend to its first decisive verdict.
func (s *starter) northStar(ctx context.Context) int {
	star, err := report.LoadNorthStar(ctx, s.w.service())
	if ctx.Err() != nil {
		return fail(s.env, ctx.Err())
	} else if err != nil { // a line must not stop the run the user asked for, as the report leaves it out
		fmt.Fprintf(s.env.Stderr, "agentium: the first-decisive-verdict line is left out: %v\n", err)
		return ExitOK
	}
	fmt.Fprintf(s.env.Stdout, "\n%s\n", star.Line())
	return ExitOK
}
