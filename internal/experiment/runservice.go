package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// RunOptions is what `experiment run` was asked for.
type RunOptions struct {
	Budget     float64 // raise the budget to this total in USD; 0 leaves it
	UsageLimit float64 // percent of the five-hour window past which no pair starts
	Wait       bool    // wait for the usage window to reset instead of pausing
}

// RunMeta places a run an experiment starts: the task it is linked to (0 when the task changed after the lock), the
// experiment, the slot and the attempt.
type RunMeta struct {
	Kind         string // "" for a task run, KindCalibration for a calibration run
	TaskID       int64
	ExperimentID int64
	Slot         int
	Attempt      int
}

// Standing is where an experiment stands when its runs begin: what is spent, which slots are settled and the latest
// usage reading.
type Standing struct {
	Spent    float64
	Settled  map[int]bool
	Usage    claude.UsageReading
	HasUsage bool
}

// Observer follows an execution: Begin comes before the first run, Event with every scheduler event (from several
// goroutines, one at a time), and Finish once the final status is stored, with how the execution ended, before the
// summary is printed. Each may be nil.
type Observer struct {
	Begin  func(lock Lock, standing Standing)
	Event  func(Event)
	Finish func(Summary)
	// Steps asks for each run's steps as events too (Kind "step"): only for a display that keeps up, since Event is
	// called on the run's goroutine at each step boundary, and a stalled one would hold the run there.
	Steps bool
}

// Runner runs an experiment: what it needs from the command line, as parameters. It holds no state of its own.
type Runner struct {
	Project   Project
	Out       io.Writer // progress and the checks before the first run
	Style     term.Style
	Now       func() time.Time
	Version   string // Agentium's, recorded in the lock
	Claude    func() (string, error)
	SignIn    string
	Readiness ReadinessEnv
	// StartRuns takes the data folder's run lock (and stores runs a dead process left behind). The caller keeps the
	// release function and calls it when it is done with the experiment, summary included.
	StartRuns func(ctx context.Context) error
	// KeepHead copies the commit HEAD names in the user's repository into Agentium's bare one and returns it: the commit
	// the base context is calibrated at.
	KeepHead func(ctx context.Context) (string, error)
	// NewRunEnv resolves what every run needs.
	NewRunEnv func(verifyTimeout time.Duration) (run.Env, error)
	// NeedsLocalBinding tells whether runs on the tasks' base commits need the sandbox's local binding (a Gradle build)
	// and whether the project's user allowed it (agentium init --allow-local-binding); nil: no check.
	NeedsLocalBinding func(ctx context.Context, bases []string) (needed, allowed bool, err error)
	// ExecuteRun runs and stores one run; the caller of Execute holds the run lock.
	ExecuteRun func(ctx context.Context, e run.Env, meta RunMeta, spec run.Spec) (run.Record, error)
	// WaitUntil waits for the usage window to reset (Wait); nil without it.
	WaitUntil func(ctx context.Context, until time.Time) error
	// Revalidate validates the named tasks again, in grader mode and the arms' contexts, and stores the validations:
	// before a sandbox experiment locks, its tasks validated in another mode are validated in the sandbox (the isolation
	// plan's decision 5). It reports progress to Out itself. nil: such tasks keep the experiment from locking.
	Revalidate func(ctx context.Context, tasks []string, arms []task.Arm, grader string) error
	// SandboxUsable checks that this machine can grade in a mode; nil: run.SandboxUsable (tests replace it).
	SandboxUsable func(ctx context.Context, mode string) error
	Backoff       func(attempt int) time.Duration // nil: 30 seconds, then 2 minutes
	Observer      Observer
	// Quiet says Out goes nowhere (a --json run): errors then must not point at output.
	Quiet bool
}

// RunOutcome is how an execution ended: its summary, whether the judge paused it, and the error that stopped it, if
// one did (the progress was written all the same).
type RunOutcome struct {
	Summary
	JudgePaused bool
	Err         error
}

// retryBackoff is how long a slot waits after an infrastructure failure before its next attempt.
func retryBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 30 * time.Second
	}
	return 2 * time.Minute
}

// experimentWorkspace names the workspace of a slot's try (its runs in order, cancelled ones included), so every run
// gets a fresh folder and session folder (a run reusing a cancelled one's could read that run's session) while staying
// predictable for the runs that may overlap it.
func experimentWorkspace(experimentID int64, slot, try int) string {
	return fmt.Sprintf("e%d-s%d-t%d", experimentID, slot, try)
}

// judgeLimitNote is the status note of an experiment paused by the judge (Verdict.Stopped is judge.StoppedLimit).
const judgeLimitNote = "the judge hit a usage limit or a sign-in failure, which the next calls would hit too"

// Run locks the experiment (the first time) or checks it can resume, then runs it until it is done, paused or stopped,
// and writes where it stands. A UsageError is a mistake in o; any other error stopped it before or after the runs.
func (r Runner) Run(ctx context.Context, name string, o RunOptions) (RunOutcome, error) {
	p := r.Project
	stored, err := p.DB.ExperimentByName(ctx, p.ID, name)
	if err != nil {
		return RunOutcome{}, err
	}
	d, err := p.Load(ctx, name)
	if err != nil {
		return RunOutcome{}, err
	}
	cli, err := r.Claude()
	if err != nil {
		return RunOutcome{}, err
	}
	version, err := project.ClaudeVersion(ctx, cli)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("Claude Code at %s: its version could not be read: %w", cli, err)
	}
	// The run lock stays held after Run returns: the caller releases it once it has printed the summary.
	if err := r.StartRuns(ctx); err != nil {
		return RunOutcome{}, err
	}

	var lock Lock
	if stored.Lock == nil {
		lock, err = r.lockFirst(ctx, stored, d, name, cli, version, o)
	} else if lock, err = r.resume(ctx, stored, name, version); err == nil {
		err = r.raiseLockedBudget(ctx, stored, &lock, o)
	}
	if err != nil {
		return RunOutcome{}, err
	}
	return r.execute(ctx, stored, name, lock, o)
}

// lockFirst checks that everything is in place, raises the budget if asked, and locks the experiment for its first run.
func (r Runner) lockFirst(ctx context.Context, stored store.Experiment, d Design, name, cli, version string, o RunOptions) (Lock, error) {
	p, out := r.Project, r.Out
	var raised *BudgetChange
	if o.Budget > 0 && o.Budget != d.BudgetUSD { // the design's budget, changed before anything ran
		if o.Budget < d.BudgetUSD {
			return Lock{}, UsageError(fmt.Sprintf("the budget can only be raised (it is $%.2f)", d.BudgetUSD))
		}
		raised = &BudgetChange{At: r.Now().UTC(), From: d.BudgetUSD, To: o.Budget}
		d.BudgetUSD = o.Budget
	}
	fmt.Fprintln(out, r.Style.Heading(fmt.Sprintf("Checking experiment %s before its first run:", name)))
	if err := r.sandboxUsable(ctx, d.Grader); err != nil { // its runs could not be graded: stop before anything is spent
		return Lock{}, err
	}
	if err := r.revalidate(ctx, d); err != nil {
		return Lock{}, err
	}
	eligible, reasons, err := p.EligibleTasks(ctx, d.Arms, d.Grader)
	if err != nil {
		return Lock{}, err
	}
	est, err := p.EstimatesFor(ctx, d)
	if err != nil {
		return Lock{}, err
	}
	readiness := r.Readiness
	readiness.Locking = true // a task still validated in another mode now is missing, not one to validate later
	ready := CheckReadiness(ctx, p, readiness, d, eligible, reasons, est)
	ready.Write(out, r.Style)
	if !ready.Ready || ctx.Err() != nil {
		if r.Quiet { // the checks above went nowhere
			return Lock{}, errors.Join(ctx.Err(), errors.New("not ready to run (agentium experiment plan "+name+")"))
		}
		return Lock{}, errors.Join(ctx.Err(), errors.New("not ready to run: see above (agentium experiment plan "+name+")"))
	}
	// Everything that can refuse the experiment without a calibration comes first: a refused run spends nothing on them.
	localBinding, err := r.checkRefusals(ctx, d)
	if err != nil {
		return Lock{}, err
	}
	if err := r.calibrate(ctx, stored, d, version); err != nil {
		return Lock{}, err
	}
	lock, err := r.buildLock(ctx, d, cli, version)
	if err != nil {
		return Lock{}, err
	}
	lock.LocalBinding = localBinding
	if raised != nil {
		lock.BudgetChanges = append(lock.BudgetChanges, *raised)
	}
	encoded, err := json.Marshal(lock)
	if err != nil {
		return Lock{}, fmt.Errorf("encode lock: %w", err)
	}
	if err := p.DB.LockExperiment(ctx, stored.ID, encoded); err != nil {
		return Lock{}, err
	}
	if raised != nil {
		fmt.Fprintf(out, "Budget raised to $%.2f (recorded in the lock).\n", raised.To)
	}
	fmt.Fprintf(out, "Locked: Claude Code %s, %s, sign-in %s, %d runs in a seeded order (seed %d), prices of %s, graded %s.\n",
		lock.ClaudeCode, d.ModelLabel(), lock.SignIn, len(lock.Schedule), d.Seed, lock.PriceTable, task.DescribeGrader(lock.Grader))
	if d.Judge != nil {
		fmt.Fprintf(out, "The judge: %s.\n", DescribeJudge(*d.Judge))
	}
	if d.JudgePairs != nil {
		fmt.Fprintf(out, "The pair judge (unvalidated): %s.\n", DescribePairJudge(*d.JudgePairs))
	}
	return lock, nil
}

// revalidate validates d's tasks that were validated in another mode again, in d's (Revalidations): only a sandbox
// experiment has any. Without Revalidate it does nothing, and readiness reports them.
func (r Runner) revalidate(ctx context.Context, d Design) error {
	names, err := r.Project.Revalidations(ctx, d)
	if err != nil || len(names) == 0 || r.Revalidate == nil {
		return err
	}
	fmt.Fprintf(r.Out, "Validating %d task(s) again %s, the experiment's grader: they were validated in another mode (time, no money).\n",
		len(names), task.DescribeGrader(d.Grader))
	return r.Revalidate(ctx, names, ValidationArms(d.Arms), task.GraderOf(d.Grader))
}

// ValidationArms are the contexts a validation covers for an experiment's arms: the base's own first, then each
// distinct snapshot.
func ValidationArms(arms []Arm) []task.Arm {
	out := []task.Arm{{Name: BaseContext}}
	for _, a := range arms {
		if a.Snapshot != "" && !slices.ContainsFunc(out, func(t task.Arm) bool { return t.Snapshot == a.Snapshot }) {
			out = append(out, task.Arm{Name: a.Context, Snapshot: a.Snapshot})
		}
	}
	return out
}

// sandboxUsable checks that this Agentium can grade in mode (Runner.SandboxUsable, else run.SandboxUsable), before an
// experiment locks or resumes: a mode it knows, and for the sandbox, a sandbox-exec that works here and a log that
// shows its denials.
func (r Runner) sandboxUsable(ctx context.Context, mode string) error {
	usable := run.SandboxUsable
	if r.SandboxUsable != nil {
		usable = r.SandboxUsable
	}
	if err := usable(ctx, mode); err != nil {
		return fmt.Errorf("the experiment grades %s: %w (with --grader host, a new experiment grades on the host)", task.DescribeGrader(mode), err)
	}
	return nil
}

// checkRefusals runs the checks that need no calibration, before anything is spent: each snapshot still names the
// commit the design holds, the tasks' bases need no local binding the user has not allowed, and no build configuration
// would make every run refuse. It returns whether the runs get local binding (recorded in the lock).
func (r Runner) checkRefusals(ctx context.Context, d Design) (bool, error) {
	p := r.Project
	for _, a := range d.Arms {
		if _, err := p.snapshotOf(ctx, a); err != nil {
			return false, err
		}
	}
	bases := make([]string, len(d.Tasks))
	for i, name := range d.Tasks {
		t, err := p.DB.TaskByName(ctx, p.ID, name)
		if err != nil {
			return false, err
		}
		bases[i] = t.BaseCommit
	}
	binding, err := r.checkLocalBinding(ctx, bases)
	if err != nil {
		return false, err
	}
	if r.NewRunEnv != nil {
		if runEnv, envErr := r.NewRunEnv(d.VerifyTimeout); envErr == nil { // its own errors surface when the runs start
			if err := runEnv.CheckBuildConfigs(ctx); err != nil { // every run would refuse: stop before locking
				return false, err
			}
		}
	}
	return binding, nil
}

// snapshotOf reads arm a's snapshot (none for the base) and checks that its name still means the commit the design holds.
func (p Project) snapshotOf(ctx context.Context, a Arm) (store.Snapshot, error) {
	if a.Snapshot == "" {
		return store.Snapshot{}, nil
	}
	snap, err := p.DB.SnapshotByName(ctx, p.ID, a.Context)
	if err != nil {
		return snap, err
	}
	if snap.CommitID != a.Snapshot {
		return snap, fmt.Errorf("snapshot %s now names commit %s, not the experiment's %s", a.Context, ShortCommit(snap.CommitID), ShortCommit(a.Snapshot))
	}
	return snap, nil
}

// checkLocalBinding refuses an experiment whose runs need the sandbox's local binding without the user's opt-in, before
// anything is locked or spent, and returns whether the runs get it (recorded in the lock, and shown by the report).
func (r Runner) checkLocalBinding(ctx context.Context, bases []string) (bool, error) {
	if r.NeedsLocalBinding == nil {
		return false, nil
	}
	needed, allowed, err := r.NeedsLocalBinding(ctx, bases)
	if err != nil {
		return false, err
	}
	if needed && !allowed {
		return false, claude.LocalBindingRefusal([]string{"gradle"}, false)
	}
	return needed && allowed, nil
}

// checkResumeLocalBinding stops a resume whose runs need the sandbox's local binding that the lock does not record: it
// was locked before Gradle projects needed it, so it holds no opt-in and cannot be given one (every run would refuse).
func (r Runner) checkResumeLocalBinding(ctx context.Context, name string, lock Lock) error {
	if r.NeedsLocalBinding == nil || lock.LocalBinding {
		return nil
	}
	bases := make([]string, len(lock.Tasks))
	for i, t := range lock.Tasks {
		bases[i] = t.Base
	}
	needed, _, err := r.NeedsLocalBinding(ctx, bases)
	if err != nil {
		return err
	}
	if needed {
		return fmt.Errorf("experiment %s cannot continue: it was locked before agent runs on Gradle projects needed the sandbox's local binding, so its lock records no opt-in and its runs would all refuse to start; start a new experiment (agentium init --allow-local-binding, then agentium experiment new)", name)
	}
	return nil
}

// resume reads a locked experiment's lock and checks that it can go on: the same Claude Code, sign-in and host, and
// every commit it needs still in Agentium's repository.
func (r Runner) resume(ctx context.Context, stored store.Experiment, name, version string) (Lock, error) {
	var lock Lock
	if err := json.Unmarshal(stored.Lock, &lock); err != nil {
		return Lock{}, fmt.Errorf("experiment %s: its lock cannot be read: %w", name, err)
	}
	if err := lock.Check(version, r.SignIn); err != nil {
		return Lock{}, fmt.Errorf("experiment %s cannot continue: %w", name, err)
	}
	if host := runtime.GOOS + "/" + runtime.GOARCH; host != lock.Host {
		return Lock{}, fmt.Errorf("experiment %s cannot continue: its runs ran on %s, this is %s", name, lock.Host, host)
	}
	if err := r.sandboxUsable(ctx, lock.Grader); err != nil {
		return Lock{}, fmt.Errorf("experiment %s cannot continue: %w", name, err)
	}
	if err := r.checkResumeLocalBinding(ctx, name, lock); err != nil {
		return Lock{}, err
	}
	var commits []string
	for _, a := range lock.Arms {
		commits = append(commits, a.Snapshot)
	}
	for _, t := range lock.Tasks {
		commits = append(commits, t.Base, t.Solution)
	}
	for _, c := range commits {
		if c == "" {
			continue
		}
		if _, err := gitx.Run(ctx, "--git-dir", r.Project.Bare, "cat-file", "-e", c+"^{commit}"); err != nil {
			return Lock{}, fmt.Errorf("experiment %s cannot continue: commit %s is gone from Agentium's repository", name, ShortCommit(c))
		}
	}
	fmt.Fprintf(r.Out, "Resuming experiment %s (locked %s on Claude Code %s).\n", name, lock.LockedAt.Format("2006-01-02 15:04"), lock.ClaudeCode)
	return lock, nil
}

// raiseLockedBudget raises a locked experiment's budget to o.Budget and records the change in its lock.
func (r Runner) raiseLockedBudget(ctx context.Context, stored store.Experiment, lock *Lock, o RunOptions) error {
	if o.Budget <= 0 || o.Budget == lock.Design.BudgetUSD {
		return nil
	}
	if o.Budget < lock.Design.BudgetUSD {
		return UsageError(fmt.Sprintf("the budget can only be raised (it is $%.2f)", lock.Design.BudgetUSD))
	}
	lock.BudgetChanges = append(lock.BudgetChanges, BudgetChange{At: r.Now().UTC(), From: lock.Design.BudgetUSD, To: o.Budget})
	lock.Design.BudgetUSD = o.Budget
	encoded, err := json.Marshal(lock)
	if err != nil {
		return fmt.Errorf("encode lock: %w", err)
	}
	if err := r.Project.DB.AmendLock(ctx, stored.ID, encoded); err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "Budget raised to $%.2f (recorded in the lock).\n", o.Budget)
	return nil
}

// execution is one `experiment run`'s runs: what is stored, and what the scheduler's goroutines share.
type execution struct {
	r      Runner
	stored store.Experiment
	lock   Lock
	runEnv run.Env

	storedTries map[int]int // runs per slot so far
	triesMu     sync.Mutex
	tries       map[int]int

	subagentsMu   sync.Mutex
	seenSubagents map[string]map[string][]string // per arm: the models each subagent type ran on in this experiment so far

	// judgePaused: a verdict or a pair's comparison stopped at a usage limit or a sign-in failure, which every later call
	// would hit too.
	judgePaused atomic.Bool
	// pairs compares the pairs beside the runs, with the pair judge; nil without it.
	pairs *pairJudge
	// event reports a step of a run in flight (Kind "step") to the observer, as the scheduler's events are.
	event func(Event)
}

// execute runs the locked experiment's slots (after judging what a stopped execution left unjudged) and writes where
// it stands.
func (r Runner) execute(ctx context.Context, stored store.Experiment, name string, lock Lock, o RunOptions) (RunOutcome, error) {
	p, out := r.Project, r.Out
	runs, err := p.DB.ExperimentRuns(ctx, stored.ID)
	if err != nil {
		return RunOutcome{}, err
	}
	runEnv, err := r.NewRunEnv(lock.Design.VerifyTimeout)
	if err != nil {
		return RunOutcome{}, err
	}
	// The lock decides: a resumed experiment never gets local binding its first run did not have (and not after the user
	// turned it off, either: NewRunEnv holds the project's setting now).
	runEnv.AllowLocalBinding = runEnv.AllowLocalBinding && lock.LocalBinding
	runEnv.Grader = task.GraderOf(lock.Grader)                                                // the lock's mode, whatever the default is now: one experiment never mixes modes
	runEnv.Progress = nil                                                                     // the scheduler reports one line per run
	if err := p.DB.SetExperimentStatus(ctx, stored.ID, store.StatusRunning, ""); err != nil { // stays so if this process dies: show tells
		return RunOutcome{}, err
	}
	calibrationSpent, err := p.CalibrationSpend(ctx, stored.ID) // before the first pair: part of the budget's spend
	if err != nil {
		return RunOutcome{}, err
	}
	x := &execution{r: r, stored: stored, lock: lock, runEnv: runEnv, storedTries: map[int]int{}, seenSubagents: SubagentModels(runs)}
	var eventMu sync.Mutex // the pair judge reports from its own goroutine: one event at a time
	event := func(e Event) {
		if x.pairs != nil { // no comparison starts while the execution waits for the usage window to reset
			switch e.Kind {
			case "wait":
				x.pairs.hold(true)
			case "start":
				x.pairs.hold(false)
			}
		}
		if r.Observer.Event != nil {
			eventMu.Lock()
			defer eventMu.Unlock()
			r.Observer.Event(e)
		}
	}
	x.event = event
	var judgeNote string
	var judgeErr error
	unfunded := 0                 // runs the budget left no room to judge
	if lock.Design.Judge != nil { // first the graded runs a stopped execution left without a verdict
		judgeNote, unfunded, judgeErr = r.judgePending(ctx, runEnv, lock, runs, calibrationSpent)
	}
	// And the grades it left pending: graded from their changes, never run again (a seq-v1 experiment's stages do it).
	if judgeNote == "" && judgeErr == nil && lock.Method != MethodSeq {
		var graded regradeResult
		graded, judgeErr = x.gradePending(ctx, runs, calibrationSpent)
		judgeNote = graded.note
	}
	prior, standing := x.priorAttempts(runs)
	standing.Spent += calibrationSpent
	x.tries = maps.Clone(x.storedTries)
	gate, err := r.usageGate(ctx, lock, o, &standing)
	if err != nil {
		return RunOutcome{}, err
	}
	design := lock.Design
	if design.JudgePairs != nil && judgeNote == "" && judgeErr == nil {
		if x.pairs, err = newPairJudge(x, runs, standing.Spent, event); err != nil {
			return RunOutcome{}, err
		}
	}
	if r.Observer.Begin != nil { // the plain lines' observer prints RunningLine here
		r.Observer.Begin(lock, standing)
	}
	backoff := r.Backoff
	if backoff == nil {
		backoff = retryBackoff
	}
	var sum Summary
	var runErr error
	switch {
	case judgeErr != nil:
		sum, runErr = Summary{Status: StatusStopped, Note: "Agentium could not store a judgement: " + judgeErr.Error()}, judgeErr
	case judgeNote != "":
		sum = Summary{Status: StatusUsage, Note: judgeNote}
		x.judgePaused.Store(true)
	default:
		plan := lockPlan(lock, calibrationSpent)
		plan.Prior, plan.Backoff, plan.Progress, plan.Usage, plan.Paused = prior, backoff, event, gate, x.paused
		if x.pairs != nil {
			plan.PairHoldUSD, plan.Outside = design.PairJudgeCapUSD(), x.pairs.outside
			x.pairs.start(ctx)
		}
		if lock.Method == MethodSeq {
			sum, runErr = x.runStages(ctx, plan, o)
		} else {
			sum, runErr = Execute(ctx, plan, x.slot)
			if sum.Status == StatusDone && runErr == nil { // the grades the runs left pending
				var regraded float64
				regraded, runErr = x.regradeAfterRuns(ctx, calibrationSpent)
				sum.SpentUSD += regraded
			}
		}
	}
	if sum.Status == "" { // Execute refused its input
		sum.Status, sum.Note = StatusStopped, "Agentium could not start the runs: "+runErr.Error()
	}
	if x.pairs != nil { // the queued comparisons end the execution (they were funded), unless it paused at the usage limit
		unread, err := x.pairs.finish(sum.Status != StatusUsage)
		sum.SpentUSD += unread
		if err != nil {
			if runErr == nil {
				sum.Status, sum.Note = StatusStopped, "Agentium could not store a pair's comparison: "+err.Error()
			}
			runErr = errors.Join(runErr, err)
		}
	}
	runErr = x.settle(ctx, &sum, runErr, unfunded)
	if err := p.DB.SetExperimentStatus(context.WithoutCancel(ctx), stored.ID, sum.Status, sum.Note); err != nil {
		return RunOutcome{}, errors.Join(runErr, err)
	}
	if r.Observer.Finish != nil {
		r.Observer.Finish(sum)
	}
	fmt.Fprintln(out)
	if err := p.WriteProgress(context.WithoutCancel(ctx), out, r.Style, name, stored.ID, lock); err != nil {
		return RunOutcome{}, errors.Join(runErr, err)
	}
	return RunOutcome{Summary: sum, JudgePaused: x.judgePaused.Load(), Err: runErr}, nil
}

// lockPlan is the scheduler's plan of a locked experiment as far as the lock decides it: its schedule, concurrency,
// attempts, budget (calibrationSpent spent outside its slots) and each slot's cap. A judge-graded task's run holds its
// grading's cap, a test-graded one its judgement's (Design.SlotCapUSD); without judge-graded tasks the caps are the
// arms' as they always were.
func lockPlan(lock Lock, calibrationSpent float64) Plan {
	design := lock.Design
	plan := Plan{Schedule: lock.Schedule, Concurrency: design.Concurrency, RunCapUSD: design.RunCapUSD(), ArmCapUSD: armCaps(design),
		BudgetUSD: design.BudgetUSD, SpentUSD: calibrationSpent, MaxAttempts: lock.MaxAttempts}
	if len(design.JudgeGraded) > 0 {
		plan.CapOf = func(s Slot) float64 { return design.SlotCapUSD(s.Arm, s.Task) }
	}
	return plan
}

// RunningLine is how the runs are run, as the plain lines say it before the first run: "Running up to 2 at a time; each
// run up to $3.00 and 20m0s; budget $18.00. Ctrl-C stops it; run it again to resume."
func RunningLine(design Design) string {
	judging := ""
	if design.Judge != nil {
		judging = fmt.Sprintf(" and its judgement up to $%.2f", design.JudgeCapUSD())
	}
	if len(design.JudgeGraded) > 0 {
		judging += fmt.Sprintf(" (a judge-graded task's run: its grading up to $%.2f)", design.GradingCapUSD())
	}
	comparing := ""
	if design.JudgePairs != nil {
		comparing = fmt.Sprintf("; each pair's comparison up to $%.2f", design.PairJudgeCapUSD())
	}
	runCap := fmt.Sprintf("$%.2f", design.RunBudgetUSD)
	if design.PerArmProfiles() {
		runCap = fmt.Sprintf("$%.2f", design.ArmRunBudgetUSD(design.Arms[0]))
		if capB := design.ArmRunBudgetUSD(design.Arms[1]); capB != design.ArmRunBudgetUSD(design.Arms[0]) {
			runCap = fmt.Sprintf("$%.2f (arm A) or $%.2f (arm B)", design.ArmRunBudgetUSD(design.Arms[0]), capB)
		}
	}
	return fmt.Sprintf("Running up to %d at a time; each run up to %s%s and %s%s; budget $%.2f. Ctrl-C stops it; run it again to resume.",
		design.Concurrency, runCap, judging, design.Timeout, comparing, design.BudgetUSD)
}

// priorAttempts turns the stored runs into the scheduler's earlier attempts, counts each slot's stored runs, and reads
// what is spent and settled.
func (x *execution) priorAttempts(runs []store.Run) ([]Attempt, Standing) {
	prior, standing := attemptsOf(runs)
	for _, r := range runs {
		x.storedTries[r.Slot]++
	}
	return prior, standing
}

// attemptsOf turns stored runs into the scheduler's earlier attempts, and reads what is spent and settled.
func attemptsOf(runs []store.Run) ([]Attempt, Standing) {
	var prior []Attempt
	standing := Standing{Settled: map[int]bool{}}
	for _, r := range runs {
		// The budget counts the judge's spend too; the cost column is the agent's alone.
		spentOn := storedSpend(r).TotalUSD()
		prior = append(prior, Attempt{Slot: r.Slot, Outcome: r.Outcome, CostUSD: spentOn})
		standing.Spent += spentOn
		if Settles(r.Outcome) {
			standing.Settled[r.Slot] = true
		}
	}
	return prior, standing
}

// runStages runs a seq-v1 experiment stage by stage. Before each stage it reads the stored runs and makes the looks
// (SequentialStatus), reporting each look once per execution; it ends at a look that ends the experiment, and otherwise
// runs the next unsettled stage to its end (Plan.Until). Execute returns only when nothing is in flight, so no run of a
// stage starts before the look of the stage before it: the stage barrier, retries and concurrency included. Every
// stage's plan keeps the design's budget, caps and reserve: nothing is released by an early look. A stop, a pause or
// the budget ending a stage is returned as Execute gave it; the results keep the last look's verdict.
func (x *execution) runStages(ctx context.Context, p Plan, o RunOptions) (Summary, error) {
	r, lock := x.r, x.lock
	made := 0           // looks reported in this execution
	spent := p.SpentUSD // as of the last stage's end
	erred := 0          // passes in a row that left grades pending for an error
	for {
		if ctx.Err() != nil { // cancelled between stages: Execute has nothing in flight
			return Summary{Status: StatusStopped, Note: "interrupted", SpentUSD: spent}, nil
		}
		if erred > 0 && !x.regradeWait(ctx, erred) {
			continue // cancelled while waiting: the check above ends it
		}
		runs, err := x.storedRuns(ctx)
		if err != nil {
			return Summary{}, err
		}
		// Grades left pending (by a judge error, the usage limit or an interrupt) are graded again before a look: a look
		// waits for every grade of its stages (slotsDone), and the agent never runs again for them.
		graded, err := x.gradePending(ctx, runs, p.SpentUSD)
		if err != nil {
			return Summary{}, err
		}
		if graded.pending > 0 && graded.attempted > 0 { // the next pass waits, longer each time in a row
			erred++
		} else {
			erred = 0
		}
		data, err := RunDataOfStored(runs)
		if err != nil {
			return Summary{}, err
		}
		if graded.note != "" || graded.unfunded > 0 {
			_, standing := attemptsOf(runs) // runs hold the grades just stored
			sum := Summary{Status: StatusUsage, Note: graded.note, SpentUSD: p.SpentUSD + standing.Spent}
			if graded.note != "" {
				x.judgePaused.Store(true)
			} else {
				sum.Status, sum.Note = StatusBudget, fmt.Sprintf("%d run(s) still wait for the judge's grade, but the budget leaves no room for one ($%.2f)",
					graded.unfunded, lock.Design.GradingCapUSD())
			}
			return sum, nil
		}
		// Not reachable today: every stored run's task is in the lock, so a pass that leaves grades pending (with no
		// pause, budget stop or cancel) has attempted one. Were that to change, the loop would spin: Execute has nothing to
		// run while slotsDone waits for those grades.
		if graded.pending > 0 && graded.attempted == 0 && ctx.Err() == nil {
			return Summary{}, fmt.Errorf("%d run(s) wait for the judge's grade, but none could be graded again", graded.pending)
		}
		status, _, err := SequentialStatus(lock, data)
		if err != nil {
			return Summary{}, err
		}
		for i := made; i < len(status.Looks); i++ {
			if p.Progress != nil {
				p.Progress(Event{Kind: "look", Look: &status.Looks[i], Looks: len(status.Planned)})
			}
		}
		made = len(status.Looks)
		prior, standing := attemptsOf(runs)
		spent = p.SpentUSD + standing.Spent
		if status.Ended != "" {
			sum := Summary{Status: StatusDone, SpentUSD: spent}
			if status.Ended != LookFinal {
				sum.Note = status.Describe()
			}
			for i, done := range slotsDone(lock, data) {
				switch {
				case standing.Settled[i]:
					sum.Settled++
				case done:
					sum.Failed++
				}
			}
			return sum, nil
		}
		if note := x.paused(); note != "" { // a judge at a usage limit: no stage starts
			return Summary{Status: StatusUsage, Note: note, SpentUSD: spent}, nil
		}
		stage := p
		stage.Prior, stage.Until = prior, lock.Sequential.StageEnd(status.NextStage)
		if p.Usage != nil { // the latest reading, which the last stage's runs may have moved
			if stage.Usage, err = r.usageGate(ctx, lock, o, &standing); err != nil {
				return Summary{}, err
			}
		}
		sum, err := Execute(ctx, stage, x.slot)
		if err != nil || sum.Status != StatusDone {
			return sum, err
		}
	}
}

// storedRuns reads the experiment's stored runs for a budget: with the pair judge, with no comparison stored meanwhile,
// so what it stores later is counted apart (pairJudge.outside).
func (x *execution) storedRuns(ctx context.Context) (runs []store.Run, err error) {
	read := func() error {
		runs, err = x.r.Project.DB.ExperimentRuns(ctx, x.stored.ID)
		return err
	}
	if x.pairs == nil {
		return runs, read()
	}
	return runs, x.pairs.read(read)
}

// RunDataOfStored decodes stored runs into what the analysis reads, in their stored order.
func RunDataOfStored(runs []store.Run) ([]RunData, error) {
	out := make([]RunData, 0, len(runs))
	for _, s := range runs {
		var rec run.Record
		if err := json.Unmarshal(s.Record, &rec); err != nil {
			return nil, fmt.Errorf("run %s: %w", s.ID, err)
		}
		out = append(out, RunDataOf(s.Slot, rec))
	}
	return out, nil
}

// RunDataOf is what the analysis reads of a run: the one mapping the executor's looks, the report and the north star
// share, so the verdict a look stops on is the one the report shows.
func RunDataOf(slot int, rec run.Record) RunData {
	return RunData{Slot: slot, Task: rec.Task, Arm: rec.Arm, Outcome: rec.Outcome, Passed: rec.Passed,
		ConfigChanged: rec.Behavior.ConfigChanged, CostUSD: rec.Spend().AgentUSD, DurationS: float64(rec.Metrics.DurationMS) / 1000,
		OutputTokens: float64(rec.Metrics.OutputTokens), Judged: rec.GradedBy == task.GradingJudge, Pending: run.NeedsGrading(rec)}
}

// usageGate is the gate that pauses pairs before the subscription's usage limit; nil with an API key, whose runs use no
// subscription. Its rate per run is the larger of the arms' models' (UsageRateFor), each measured on its own runs. It adds the latest usage reading to standing.
func (r Runner) usageGate(ctx context.Context, lock Lock, o RunOptions, standing *Standing) (*UsageGate, error) {
	projectRuns, err := r.Project.DB.Runs(ctx, r.Project.ID)
	if err != nil {
		return nil, err
	}
	if lock.SignIn == claude.SignInAPIKey {
		return nil, nil
	}
	samples := UsageSamples(projectRuns)
	gate := &UsageGate{Limit: o.UsageLimit / 100}
	d := lock.Design
	gate.PerRun = UsageRateFor(samples, d.ArmModel(d.Arms[0]), d.ArmModel(d.Arms[1])).PerRun
	if latest, have := LatestUsage(samples); have {
		gate.Latest = latest.Last
		standing.Usage, standing.HasUsage = gate.Latest, true
	}
	if o.Wait {
		gate.Wait = r.WaitUntil
	}
	return gate, nil
}

// slot runs one attempt of a slot and turns the record into the scheduler's result.
func (x *execution) slot(ctx context.Context, slot Slot, attempt int, overlap []int) (Result, error) {
	r, lock, design := x.r, x.lock, x.lock.Design
	arm, ok := lock.Arm(slot.Arm)
	t, ok2 := lock.Task(slot.Task)
	if !ok || !ok2 {
		return Result{}, fmt.Errorf("the lock has no arm %s or task %s", slot.Arm, slot.Task)
	}
	x.triesMu.Lock()
	x.tries[slot.Position]++
	try := x.tries[slot.Position]
	x.triesMu.Unlock()
	e := x.runEnv
	e.Expect = arm.Expect(lock.ClaudeCode)
	e.Workspace = experimentWorkspace(x.stored.ID, slot.Position, try)
	e.DenyExtra = nil
	if x.event != nil && r.Observer.Event != nil && r.Observer.Steps { // each step of the run, for a live display
		e.Step = func(step string) { x.event(Event{Kind: "step", Slot: slot, Attempt: attempt, Step: step}) }
	}
	for _, q := range overlap { // in this execution a slot runs at most MaxAttempts more times
		for t := 1; t <= x.storedTries[q]+lock.MaxAttempts; t++ {
			e.DenyExtra = append(e.DenyExtra, e.Predicted(experimentWorkspace(x.stored.ID, q, t))...)
		}
	}
	meta := RunMeta{ExperimentID: x.stored.ID, Slot: slot.Position, Attempt: attempt}
	if current, err := r.Project.DB.TaskByName(ctx, r.Project.ID, t.Name); err == nil && NewLockedTask(current.Name, current.Instruction,
		task.Spec{Base: current.BaseCommit, Solution: current.SolutionCommit, HiddenTests: current.HiddenTests, Reference: current.Reference,
			Setup: current.Setup, Verify: current.Verify, Module: current.Module, Grading: current.Grading}).Digest == t.Digest {
		meta.TaskID = current.ID // linked only while the task is the one the lock ran
	}
	rec, err := r.ExecuteRun(ctx, e, meta, run.Spec{TaskName: t.Name, Instruction: t.Instruction, Task: t.Spec(),
		Arm: task.Arm{Name: arm.Name, Snapshot: arm.Snapshot}, Model: design.ArmModel(arm.Arm), Effort: design.ArmEffort(arm.Arm),
		BudgetUSD: design.ArmRunBudgetUSD(arm.Arm), HarmlessDenials: lock.Harmless[t.Name],
		Timeout: design.Timeout, Judge: design.Judge, JudgeGrading: design.JudgeGrading})
	result := spentResult(rec.Spend())
	result.Outcome, result.Usage, result.WarmWait, result.Passed = rec.Outcome, rec.Metrics.UsageLast, rec.WarmWait, rec.Passed
	result.JudgeGraded, result.GradePending = rec.GradedBy == task.GradingJudge, run.NeedsGrading(rec)
	if o := rec.Overshoot; o != nil && o.Exceeded() {
		result.Overshoot = run.OvershootNote(*o)
	}
	if rec.Outcome == run.OutcomeSandboxFlagged && rec.Sandbox != nil {
		result.SandboxFlagged = rec.Sandbox.FlaggedOperations()
	}
	if v := rec.Judge; v != nil {
		result.Judge = run.Describe(*v)
		if result.JudgeGraded {
			result.Judge, result.JudgeVotes = run.GradeWords(rec), run.GradeVotes(rec)
		}
		if v.Stopped == llmjudge.StoppedLimit {
			result.Pause = judgeLimitNote
			x.judgePaused.Store(true)
		}
	}
	switch m := rec.Metrics; {
	case m.SawInit && m.CLIVersion != lock.ClaudeCode:
		result.Stop = fmt.Sprintf("Claude Code reported version %s, but the experiment is locked to %s: later runs would not compare", m.CLIVersion, lock.ClaudeCode)
	case m.SawInit && arm.Model != "" && m.Model != arm.Model:
		result.Stop = fmt.Sprintf("Claude Code reported model %s, but arm %s's calibration saw %s: later runs would not compare", m.Model, arm.Name, arm.Model)
	}
	x.checkSubagents(arm.Name, rec.Metrics.SubagentModels, &result)
	if x.pairs != nil && err == nil && Settles(rec.Outcome) { // before the result returns: the pair's hold passes to its comparison
		x.pairs.settled(slot, rec.ID, rec)
	}
	return result, err
}

// paused is Plan.Paused: a judgement or a pair's comparison stopped at a usage limit or a sign-in failure, which every
// later call would hit too, so no run starts.
func (x *execution) paused() string {
	if x.judgePaused.Load() {
		return judgeLimitNote
	}
	return ""
}

// checkSubagents stops the experiment when a role's model alias moved to a newer model with Claude Code while --model
// stays pinned: the runs after it would not compare with those before. It is per arm, as arms may give a role different
// models on purpose (that is a context difference to measure).
func (x *execution) checkSubagents(arm string, models map[string][]string, result *Result) {
	x.subagentsMu.Lock()
	defer x.subagentsMu.Unlock()
	if x.seenSubagents[arm] == nil {
		x.seenSubagents[arm] = map[string][]string{}
	}
	if changes := claude.SubagentModelChanges(x.seenSubagents[arm], models); len(changes) > 0 {
		if result.Stop == "" {
			result.Stop = "arm " + arm + ": " + strings.Join(changes, "; ") + ": later runs would not compare"
		}
	} else {
		MergeSubagentModels(x.seenSubagents[arm], models)
	}
}

// settle adjusts the summary of a finished execution. Every slot settled is not done while a graded run still needs the
// judge, or a pair of passing runs still needs comparing: the last judgements may have stopped at a usage limit or an
// interrupt, or the budget may have left no room for them (a budget stop, which a higher --budget resumes, not a
// failure). unfunded counts the runs the budget left no room to judge before the runs; the pair judge counts its own.
// It returns runErr with any error of counting them.
func (x *execution) settle(ctx context.Context, sum *Summary, runErr error, unfunded int) error {
	design := x.lock.Design
	if sum.Status != StatusDone || design.Judge == nil && design.JudgePairs == nil && !x.lock.JudgeGraded() {
		return runErr
	}
	runs, err := x.r.Project.DB.ExperimentRuns(context.WithoutCancel(ctx), x.stored.ID)
	if err != nil {
		return errors.Join(runErr, err)
	}
	n, err := unjudgedOf(x.lock, runs)
	if err != nil {
		return errors.Join(runErr, err)
	}
	g, err := pendingGrades(x.lock, runs)
	if err != nil {
		return errors.Join(runErr, err)
	}
	m, err := uncompared(x.lock, runs)
	if err != nil {
		return errors.Join(runErr, err)
	}
	pairsUnfunded := 0
	if x.pairs != nil {
		pairsUnfunded = x.pairs.unfunded
	}
	var budget, waiting []string
	if g > 0 {
		waiting = append(waiting, fmt.Sprintf("%d run(s) still wait for the judge's grade", g))
		spent := sum.SpentUSD
		if design.BudgetUSD-spent < design.GradingCapUSD()-1e-9 {
			budget = append(budget, fmt.Sprintf("%d run(s) still wait for the judge's grade, but the budget leaves no room for one ($%.2f)", g, design.GradingCapUSD()))
		}
	}
	if n > 0 {
		waiting = append(waiting, fmt.Sprintf("%d run(s) still need the judge", n))
		if unfunded > 0 {
			budget = append(budget, fmt.Sprintf("%d run(s) still need the judge, but the budget leaves no room for a judgement ($%.2f)", n, design.JudgeCapUSD()))
		}
	}
	if m > 0 {
		waiting = append(waiting, fmt.Sprintf("%d pair(s) still need comparing", m))
		if pairsUnfunded > 0 {
			budget = append(budget, fmt.Sprintf("%d pair(s) still need comparing, but the budget leaves no room for a comparison ($%.2f)", m, design.PairJudgeCapUSD()))
		}
	}
	switch {
	case len(waiting) > 0 && x.judgePaused.Load():
		sum.Status, sum.Note = StatusUsage, judgeLimitNote
	case len(budget) > 0:
		sum.Status, sum.Note = StatusBudget, strings.Join(budget, "; ")
	case len(waiting) > 0:
		sum.Status, sum.Note = StatusStopped, strings.Join(waiting, "; ")
	}
	return runErr
}

// Conclude writes how the execution ended and what to do next, and reports whether that is a success (everything done,
// or paused at a limit that a resume lifts); a run that stopped is not.
func (o RunOutcome) Conclude(out io.Writer, st term.Style, name string, now time.Time) bool {
	switch {
	case o.Status == StatusDone && o.Note != "": // a seq-v1 experiment that ended at an early look
		fmt.Fprintf(out, "%s The report: %s\n", st.Good("Done: "+o.Note+"."), st.Command("agentium experiment report "+name))
	case o.Status == StatusDone:
		fmt.Fprintf(out, "%s The report: %s\n", st.Good("Every run is done."), st.Command("agentium experiment report "+name))
	case o.Status == StatusBudget:
		fmt.Fprintf(out, "%s To continue: %s (a higher total)\n", st.Warn("Stopped at the budget."), st.Command("agentium experiment run "+name+" --budget USD"))
	case o.Status == StatusUsage && o.JudgePaused:
		fmt.Fprintf(out, "%s To continue, once it resets: %s (runs without a verdict are judged first; --wait does not wait for the judge)\n",
			st.Warn("Paused: the judge hit a usage limit or a sign-in failure."), st.Command("agentium experiment run "+name))
	case o.Status == StatusUsage && !o.ResumeAt.IsZero():
		fmt.Fprintf(out, "%s To continue: %s (--wait waits for the reset)\n",
			st.Warn(fmt.Sprintf("Paused before the usage limit; the window resets at %s.", Clock(o.ResumeAt, now))), st.Command("agentium experiment run "+name))
	case o.Status == StatusUsage:
		fmt.Fprintf(out, "%s To continue: %s\n", st.Warn("Paused: a pair needs more of the usage window than the limit allows."),
			st.Command("agentium experiment run "+name+" --usage-limit PCT"))
	default:
		fmt.Fprintf(out, "%s To continue: %s\n", st.Warn("Stopped."), st.Command("agentium experiment run "+name))
		return false
	}
	return true
}

// needsJudge reports whether a stored run of the experiment still needs the judge (run.NeedsJudging): only in an
// experiment with the judge, and only for fair runs of the lock's tasks.
func needsJudge(lock Lock, r store.Run, rec run.Record) bool {
	t, ok := lock.Task(r.TaskName)
	return lock.Design.Judge != nil && ok && Fair(r.Outcome) && run.NeedsJudging(rec, t.Spec())
}

// unjudgedOf counts the stored runs that still need the judge.
func unjudgedOf(lock Lock, runs []store.Run) (int, error) {
	n := 0
	for _, s := range runs {
		var rec run.Record
		if err := json.Unmarshal(s.Record, &rec); err != nil {
			return 0, fmt.Errorf("run %s: %w", s.ID, err)
		}
		if needsJudge(lock, s, rec) {
			n++
		}
	}
	return n, nil
}

// spentResult is an attempt's result for the scheduler as far as spend goes: the budget counts all it spent
// (Result.CostUSD), and the progress line tells the agent's part from the judge's. The analysis's cost metric stays the
// record's agent's cost.
func spentResult(s run.Spend) Result {
	return Result{CostUSD: s.TotalUSD(), JudgeUSD: s.JudgeUSD + s.PairJudgeUSD}
}

// judgePending judges, one at a time, the experiment's graded runs that still need it (run.NeedsJudging): those a
// stopped execution left without a verdict, and those whose judgement stopped early. Each is stored with its verdict
// in place (runs[i].Record too), so the spend that follows counts it. A judgement starts only when the spend so far and
// its cap fit the budget; one that does not is left for a resume with a higher budget, and counted in unfunded. It
// returns a pause note when a verdict stopped at a usage limit, and an error only when a record cannot be read or stored. A cancelled ctx ends it
// quietly: the execution that follows sees the cancellation. Judgements that leave no verdict (a missing diff, say)
// are reported here, not stored, so resumes do not repeat their notes.
func (r Runner) judgePending(ctx context.Context, runEnv run.Env, lock Lock, runs []store.Run, calibrationSpent float64) (note string, unfunded int, err error) {
	design, out, st := lock.Design, r.Out, r.Style
	spent := calibrationSpent
	for _, s := range runs {
		spent += storedSpend(s).TotalUSD()
	}
	for i, s := range runs {
		if ctx.Err() != nil {
			return "", unfunded, nil
		}
		t, ok := lock.Task(s.TaskName)
		if !ok || !Fair(s.Outcome) {
			continue
		}
		var rec run.Record
		if err := json.Unmarshal(s.Record, &rec); err != nil {
			return "", 0, fmt.Errorf("run %s: %w", s.ID, err)
		}
		if !run.NeedsJudging(rec, t.Spec()) {
			continue
		}
		if spent+design.JudgeCapUSD() > design.BudgetUSD+1e-9 {
			unfunded++
			continue
		}
		before := rec.Spend().JudgeUSD
		notes := len(rec.Notes)
		runEnv.Judge(ctx, run.Spec{TaskName: t.Name, Instruction: t.Instruction, Task: t.Spec()}, *design.Judge, &rec)
		label := fmt.Sprintf("Judged run %s (task %s, arm %s)", s.ID, s.TaskName, s.Arm)
		if rec.Judge == nil {
			fmt.Fprintf(out, "%s: %s\n", label, st.Warn(strings.Join(rec.Notes[notes:], "; ")))
			continue
		}
		spent += rec.Spend().JudgeUSD - before
		encoded, err := json.Marshal(rec)
		if err != nil {
			return "", 0, fmt.Errorf("encode run %s: %w", s.ID, err)
		}
		if err := r.Project.DB.SetRunRecord(context.WithoutCancel(ctx), s.ID, encoded); err != nil {
			return "", 0, err
		}
		runs[i].Record = encoded
		fmt.Fprintf(out, "%s: %s, $%.2f (spent $%.2f of $%.2f)\n", label, run.Describe(*rec.Judge), rec.Spend().JudgeUSD-before, spent, design.BudgetUSD)
		if rec.Judge.Stopped == llmjudge.StoppedLimit {
			return judgeLimitNote, unfunded, nil
		}
	}
	if unfunded > 0 {
		fmt.Fprintln(out, st.Warn(fmt.Sprintf("%d run(s) still need the judge, but the budget leaves no room for a judgement ($%.2f): raise it with --budget",
			unfunded, design.JudgeCapUSD())))
	}
	return "", unfunded, nil
}

// buildLock fixes an experiment for its first run: the machine's Claude Code, each arm's context files and calibrated
// environment, each task's full specification, and the schedule.
func (r Runner) buildLock(ctx context.Context, d Design, cli, version string) (Lock, error) {
	p := r.Project
	l := Lock{Method: d.LockMethod(), Agentium: r.Version, LockedAt: r.Now().UTC(), ClaudeCode: version,
		ClaudePath: cli, SignIn: r.SignIn, Host: runtime.GOOS + "/" + runtime.GOARCH, PriceTable: pricing.Date, Design: d,
		Schedule: Schedule(d), MaxAttempts: MaxAttempts, Grader: task.GraderOf(d.Grader)}
	if d.Sequential() {
		seq, err := NewSequential(len(d.Tasks), !d.NoFutility)
		if err != nil {
			return l, err
		}
		l.Sequential, l.Schedule = &seq, seq.stage(l.Schedule)
	}
	for _, a := range d.Arms {
		locked := LockedArm{Arm: a}
		if a.Snapshot != "" {
			snap, err := p.snapshotOf(ctx, a)
			if err != nil {
				return l, err
			}
			var manifest snapshot.Manifest
			if err := json.Unmarshal(snap.Manifest, &manifest); err != nil {
				return l, fmt.Errorf("snapshot %s: %w", a.Context, err)
			}
			for _, f := range manifest.Files {
				locked.Files = append(locked.Files, FileDigest{Path: f.Path, SHA256: f.SHA256})
			}
		}
		stored, err := p.CalibrationFor(ctx, d, a)
		if err != nil {
			return l, err
		}
		var c run.Calibration
		if err := json.Unmarshal(stored.Result, &c); err != nil {
			return l, fmt.Errorf("calibration of %s: %w", a.Context, err)
		}
		locked.Calibration, locked.Model, locked.Tools, locked.Skills, locked.SlashCommands = stored.RunID, c.Model, c.Tools, c.Skills, c.SlashCommands
		l.Arms = append(l.Arms, locked)
	}
	for _, name := range d.Tasks {
		t, err := p.DB.TaskByName(ctx, p.ID, name)
		if err != nil {
			return l, err
		}
		// The design fixed which tasks the judge grades, and so its version and budget: a task whose grading changed since
		// would run under a design that does not hold it.
		if (t.Grading == task.GradingJudge) != d.IsJudgeGraded(t.Name) {
			return l, fmt.Errorf("task %s is %s now, but the experiment was made when it was not: make the experiment again (agentium experiment new)",
				t.Name, map[bool]string{true: "judge-graded", false: "graded by its tests"}[t.Grading == task.GradingJudge])
		}
		l.Tasks = append(l.Tasks, NewLockedTask(t.Name, t.Instruction, task.Spec{Base: t.BaseCommit, Solution: t.SolutionCommit,
			HiddenTests: t.HiddenTests, Reference: t.Reference, Setup: t.Setup, Verify: t.Verify, Module: t.Module, Grading: t.Grading}))
		if v := task.ValidationOf(t); l.Grader != task.GraderHost && v.Grader == l.Grader && len(v.Harmless) > 0 {
			if l.Harmless == nil {
				l.Harmless = map[string][]task.DenialKey{}
			}
			l.Harmless[t.Name] = v.Harmless
		}
	}
	return l, nil
}

// armCaps is each arm's run cap by name for a model-ab experiment (even equal ones: they may all differ from the
// design's RunBudgetUSD), and nil for a context experiment, whose arms share the design's.
func armCaps(d Design) map[string]float64 {
	if len(d.Arms) != 2 || !d.PerArmProfiles() {
		return nil
	}
	return map[string]float64{d.Arms[0].Name: d.ArmRunCapUSD(d.Arms[0]), d.Arms[1].Name: d.ArmRunCapUSD(d.Arms[1])}
}
