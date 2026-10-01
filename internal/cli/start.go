package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
)

const startUsage = `Usage: agentium start [--yes] [--budget USD] [--b SNAPSHOT] [--reviewed]

Goes from a repository to a previewed experiment, skipping every stage that is already done, so running it again resumes:
  1. registers the repository (as init);
  2. saves the committed context as the snapshot "baseline", if the project has no snapshot (else it uses the newest
     one that --b does not name);
  3. mines and validates tasks until 8 are ready, the experiment's cost floor, or the candidates run out;
  4. creates the experiment "quick-..." at the cost floor: 8 tasks × 1 run per arm. With --b it compares the context
     with that snapshot; without it, it is an A/A calibration of the context (both arms the same, which must find no
     difference: it measures this repository's noise, it does not compare contexts);
  5. prints its preview: runs, estimated cost, detectable effect and what is missing.

It stops before any paid run. --yes runs the experiment (as agentium experiment run NAME); on a terminal, it asks
instead. --budget raises the experiment's total in USD. Mined tasks wait for your review of their instructions for
solution leaks (agentium task show NAME, then agentium task edit NAME --reviewed); --reviewed accepts those whose
instructions show no solution sections and state every requirement of the hidden tests.
`

// startArgs is what start was asked for.
type startArgs struct {
	yes, reviewed bool
	budget        float64
	b             string // the snapshot to compare the context with; "" for an A/A calibration
}

// errReported is returned by a stage that has already told the user why it stopped.
var errReported = errors.New("reported")

func runStart(ctx context.Context, env Env, args []string) int {
	var a startArgs
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.BoolVar(&a.yes, "yes", false, "run the experiment without asking (real, paid runs)")
	fs.BoolVar(&a.reviewed, "reviewed", false, "accept the mined instructions that pass the leak and gap checks as reviewed")
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
	s := &starter{env: env, args: a}
	defer s.close()
	name, err := s.prepare(ctx)
	switch {
	case errors.Is(err, errReported):
		return ExitError
	case err != nil:
		return fail(env, err)
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
	// contexts are the snapshots the experiment compares: a (always) and b (with --b).
	a, b string
}

func (s *starter) close() {
	if s.w != nil {
		s.w.Close()
	}
}

// prepare runs the stages up to the experiment's creation and returns the experiment's name; "" means too few tasks.
func (s *starter) prepare(ctx context.Context) (string, error) {
	if err := s.register(ctx); err != nil {
		return "", err
	}
	if err := s.chooseContexts(ctx); err != nil {
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
	if !strings.Contains(err.Error(), "is not registered") {
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
	for i := len(snaps) - 1; i >= 0; i-- { // newest first
		if snaps[i].Name != s.b {
			s.a = snaps[i].Name
			fmt.Fprintf(out, "Context: snapshot %s (skipped)\n", s.a)
			return nil
		}
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

// experimentName is stable for the same contexts, so running start again finds the experiment it made.
func (s *starter) experimentName() string {
	if s.b == "" {
		return "quick-aa-" + s.a
	}
	return "quick-" + s.a + "-vs-" + s.b
}

// createExperiment stores the cost-floor experiment unless it exists.
func (s *starter) createExperiment(ctx context.Context) (string, error) {
	w, out, name := s.w, s.env.Stdout, s.experimentName()
	if _, err := w.db.ExperimentByName(ctx, w.project.ID, name); err == nil {
		fmt.Fprintf(out, "Experiment %s: exists (skipped)\n", name)
		return name, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	floor := experiment.FloorsFor(experiment.MethodVersion)
	o := experiment.NewOptions{Template: experiment.TemplateAA, ContextA: s.a, Repeats: floor.CostRepeats, Model: "claude-sonnet-5",
		Goal: experiment.GoalCheaper, RunBudget: 3, Budget: s.args.budget, Concurrency: 2, Timeout: 20 * time.Minute, VerifyTimeout: 10 * time.Minute}
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
	o.Tasks = experiment.Sample(eligible, floor.CostTasks, o.Seed)
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
	if s.b == "" {
		fmt.Fprintf(env.Stdout, "\n%s\n", st.Note("note: no second context was given, so this is an A/A calibration of "+s.a+": both arms run the same context and "+
			"should show no difference. It measures this repository's noise and checks the method; it does not compare contexts "+
			"(agentium start --b SNAPSHOT does)."))
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
	if code := s.northStar(ctx); code != ExitOK {
		return code
	}
	if !review.Readiness.Ready {
		fmt.Fprintf(env.Stdout, "Nothing was run: fix what is missing above, then %s (or %s)\n", st.Command("agentium start --yes"),
			st.Command("agentium experiment run "+name))
		if s.args.yes {
			return ExitError
		}
		return ExitOK
	}
	if !s.args.yes && !s.confirm(review.Design.BudgetUSD) {
		fmt.Fprintf(env.Stdout, "\nNothing was run and nothing was spent. To run it (real Claude Code runs, up to $%.2f): %s\n", review.Design.BudgetUSD,
			st.Command("agentium experiment run "+name))
		return ExitOK
	}
	runArgs := []string{name}
	if s.args.budget > 0 {
		runArgs = append(runArgs, "--budget", fmt.Sprint(s.args.budget))
	}
	s.w.Close()
	s.w = nil // experimentRun opens the project itself
	return experimentRun(ctx, env, runArgs)
}

// confirm asks whether to run the experiment, only when a person can answer: stdin and stdout are terminals.
func (s *starter) confirm(budget float64) bool {
	if !s.env.StdinTerminal || !s.env.Terminal || s.env.Stdin == nil {
		return false
	}
	fmt.Fprintf(s.env.Stdout, "\nRun it now? It makes real Claude Code runs and spends up to $%.2f. [y/N] ", budget)
	line, err := bufio.NewReader(io.LimitReader(s.env.Stdin, 1<<10)).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// northStar prints the project's time and spend to its first decisive verdict.
func (s *starter) northStar(ctx context.Context) int {
	star, err := report.LoadNorthStar(ctx, s.w.service())
	if err != nil {
		return fail(s.env, err)
	}
	fmt.Fprintf(s.env.Stdout, "\n%s\n", star.Line())
	return ExitOK
}
