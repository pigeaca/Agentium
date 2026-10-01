package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"runtime"
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
// goroutines), and Finish once the final status is stored, before the summary is printed. Each may be nil.
type Observer struct {
	Begin  func(lock Lock, standing Standing)
	Event  func(Event)
	Finish func()
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
	// NewRunEnv resolves what every run needs.
	NewRunEnv func(verifyTimeout time.Duration) (run.Env, error)
	// NeedsLocalBinding tells whether runs on the tasks' base commits need the sandbox's local binding (a Gradle build)
	// and whether the project's user allowed it (agentium init --allow-local-binding); nil: no check.
	NeedsLocalBinding func(ctx context.Context, bases []string) (needed, allowed bool, err error)
	// ExecuteRun runs and stores one run; the caller of Execute holds the run lock.
	ExecuteRun func(ctx context.Context, e run.Env, meta RunMeta, spec run.Spec) (run.Record, error)
	// WaitUntil waits for the usage window to reset (Wait); nil without it.
	WaitUntil func(ctx context.Context, until time.Time) error
	Backoff   func(attempt int) time.Duration // nil: 30 seconds, then 2 minutes
	Observer  Observer
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
	eligible, reasons, err := p.EligibleTasks(ctx, d.Arms)
	if err != nil {
		return Lock{}, err
	}
	est, err := p.EstimateFor(ctx, d.Model)
	if err != nil {
		return Lock{}, err
	}
	ready := CheckReadiness(ctx, p, r.Readiness, d, eligible, reasons, est)
	ready.Write(out, r.Style)
	if !ready.Ready || ctx.Err() != nil {
		return Lock{}, errors.Join(ctx.Err(), errors.New("not ready to run: see above (agentium experiment plan "+name+")"))
	}
	lock, err := r.buildLock(ctx, d, cli, version)
	if err != nil {
		return Lock{}, err
	}
	if raised != nil {
		lock.BudgetChanges = append(lock.BudgetChanges, *raised)
	}
	if lock.LocalBinding, err = r.checkLocalBinding(ctx, lock); err != nil {
		return Lock{}, err
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
	fmt.Fprintf(out, "Locked: Claude Code %s, %s, sign-in %s, %d runs in a seeded order (seed %d), prices of %s.\n",
		lock.ClaudeCode, d.Model, lock.SignIn, len(lock.Schedule), d.Seed, lock.PriceTable)
	if d.Judge != nil {
		fmt.Fprintf(out, "The judge: %s.\n", DescribeJudge(*d.Judge))
	}
	return lock, nil
}

// checkLocalBinding refuses an experiment whose runs need the sandbox's local binding without the user's opt-in, before
// anything is locked or spent, and returns whether the runs get it (recorded in the lock, and shown by the report).
func (r Runner) checkLocalBinding(ctx context.Context, lock Lock) (bool, error) {
	if r.NeedsLocalBinding == nil {
		return false, nil
	}
	bases := make([]string, len(lock.Tasks))
	for i, t := range lock.Tasks {
		bases[i] = t.Base
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

	// judgePaused: a verdict stopped at a usage limit or a sign-in failure, which every later call would hit too.
	judgePaused atomic.Bool
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
	runEnv.Progress = nil                                                                     // the scheduler reports one line per run
	if err := p.DB.SetExperimentStatus(ctx, stored.ID, store.StatusRunning, ""); err != nil { // stays so if this process dies: show tells
		return RunOutcome{}, err
	}
	x := &execution{r: r, stored: stored, lock: lock, runEnv: runEnv, storedTries: map[int]int{}, seenSubagents: SubagentModels(runs)}
	var judgeNote string
	var judgeErr error
	unfunded := 0                 // runs the budget left no room to judge
	if lock.Design.Judge != nil { // first the graded runs a stopped execution left without a verdict
		judgeNote, unfunded, judgeErr = r.judgePending(ctx, runEnv, lock, runs)
	}
	prior, standing := x.priorAttempts(runs)
	x.tries = maps.Clone(x.storedTries)
	gate, err := r.usageGate(ctx, lock, o, &standing)
	if err != nil {
		return RunOutcome{}, err
	}
	design := lock.Design
	judging := ""
	if design.Judge != nil {
		judging = fmt.Sprintf(" and its judgement up to $%.2f", design.JudgeCapUSD())
	}
	fmt.Fprintf(out, "Running up to %d at a time; each run up to $%.2f%s and %s; budget $%.2f. Ctrl-C stops it; run it again to resume.\n",
		design.Concurrency, design.RunBudgetUSD, judging, design.Timeout, design.BudgetUSD)
	if r.Observer.Begin != nil {
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
		sum, runErr = Execute(ctx, Plan{Schedule: lock.Schedule, Concurrency: design.Concurrency, RunCapUSD: design.RunCapUSD(),
			BudgetUSD: design.BudgetUSD, MaxAttempts: lock.MaxAttempts, Prior: prior, Backoff: backoff, Progress: r.Observer.Event, Usage: gate}, x.slot)
	}
	if sum.Status == "" { // Execute refused its input
		sum.Status, sum.Note = StatusStopped, "Agentium could not start the runs: "+runErr.Error()
	}
	runErr = x.settle(ctx, &sum, runErr, unfunded)
	if err := p.DB.SetExperimentStatus(context.WithoutCancel(ctx), stored.ID, sum.Status, sum.Note); err != nil {
		return RunOutcome{}, errors.Join(runErr, err)
	}
	if r.Observer.Finish != nil {
		r.Observer.Finish()
	}
	fmt.Fprintln(out)
	if err := p.WriteProgress(context.WithoutCancel(ctx), out, r.Style, name, stored.ID, lock); err != nil {
		return RunOutcome{}, errors.Join(runErr, err)
	}
	return RunOutcome{Summary: sum, JudgePaused: x.judgePaused.Load(), Err: runErr}, nil
}

// priorAttempts turns the stored runs into the scheduler's earlier attempts, counts each slot's stored runs, and reads
// what is spent and settled.
func (x *execution) priorAttempts(runs []store.Run) ([]Attempt, Standing) {
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
		x.storedTries[r.Slot]++
	}
	return prior, standing
}

// usageGate is the gate that pauses pairs before the subscription's usage limit; nil with an API key, whose runs use no
// subscription. It adds the latest usage reading to standing.
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
	gate.PerRun, _ = UsagePerRun(samples)
	var have bool
	if gate.Latest, have = LatestUsage(samples); have {
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
	for _, q := range overlap { // in this execution a slot runs at most MaxAttempts more times
		for t := 1; t <= x.storedTries[q]+lock.MaxAttempts; t++ {
			e.DenyExtra = append(e.DenyExtra, e.Predicted(experimentWorkspace(x.stored.ID, q, t))...)
		}
	}
	meta := RunMeta{ExperimentID: x.stored.ID, Slot: slot.Position, Attempt: attempt}
	if current, err := r.Project.DB.TaskByName(ctx, r.Project.ID, t.Name); err == nil && NewLockedTask(current.Name, current.Instruction,
		task.Spec{Base: current.BaseCommit, Solution: current.SolutionCommit, HiddenTests: current.HiddenTests, Reference: current.Reference,
			Setup: current.Setup, Verify: current.Verify}).Digest == t.Digest {
		meta.TaskID = current.ID // linked only while the task is the one the lock ran
	}
	rec, err := r.ExecuteRun(ctx, e, meta, run.Spec{TaskName: t.Name, Instruction: t.Instruction, Task: t.Spec(),
		Arm: task.Arm{Name: arm.Name, Snapshot: arm.Snapshot}, Model: design.Model, Effort: design.Effort, BudgetUSD: design.RunBudgetUSD,
		Timeout: design.Timeout, Judge: design.Judge})
	result := spentResult(rec.Spend())
	result.Outcome, result.Usage = rec.Outcome, rec.Metrics.UsageLast
	if v := rec.Judge; v != nil {
		result.Judge = run.Describe(*v)
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
	return result, err
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
// judge: the last runs' judgements may have stopped at a usage limit or an interrupt, or the budget may have left no
// room to judge them (a budget stop, which a higher --budget resumes, not a failure). It returns runErr with any error
// of counting them.
func (x *execution) settle(ctx context.Context, sum *Summary, runErr error, unfunded int) error {
	design := x.lock.Design
	if sum.Status != StatusDone || design.Judge == nil {
		return runErr
	}
	n, err := x.r.unjudged(context.WithoutCancel(ctx), x.stored.ID, x.lock)
	switch {
	case err != nil:
		return errors.Join(runErr, err)
	case n > 0 && x.judgePaused.Load():
		sum.Status, sum.Note = StatusUsage, judgeLimitNote
	case n > 0 && unfunded > 0:
		sum.Status, sum.Note = StatusBudget, fmt.Sprintf("%d run(s) still need the judge, but the budget leaves no room for a judgement ($%.2f)",
			n, design.JudgeCapUSD())
	case n > 0:
		sum.Status, sum.Note = StatusStopped, fmt.Sprintf("%d run(s) still need the judge", n)
	}
	return runErr
}

// Conclude writes how the execution ended and what to do next, and reports whether that is a success (everything done,
// or paused at a limit that a resume lifts); a run that stopped is not.
func (o RunOutcome) Conclude(out io.Writer, st term.Style, name string, now time.Time) bool {
	switch {
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

// unjudged counts the experiment's stored runs that still need the judge.
func (r Runner) unjudged(ctx context.Context, id int64, lock Lock) (int, error) {
	runs, err := r.Project.DB.ExperimentRuns(ctx, id)
	if err != nil {
		return 0, err
	}
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
	return Result{CostUSD: s.TotalUSD(), JudgeUSD: s.JudgeUSD}
}

// judgePending judges, one at a time, the experiment's graded runs that still need it (run.NeedsJudging): those a
// stopped execution left without a verdict, and those whose judgement stopped early. Each is stored with its verdict
// in place (runs[i].Record too), so the spend that follows counts it. A judgement starts only when the spend so far and
// its cap fit the budget; one that does not is left for a resume with a higher budget, and counted in unfunded. It
// returns a pause note when a verdict stopped at a usage limit, and an error only when a record cannot be read or stored. A cancelled ctx ends it
// quietly: the execution that follows sees the cancellation. Judgements that leave no verdict (a missing diff, say)
// are reported here, not stored, so resumes do not repeat their notes.
func (r Runner) judgePending(ctx context.Context, runEnv run.Env, lock Lock, runs []store.Run) (note string, unfunded int, err error) {
	design, out, st := lock.Design, r.Out, r.Style
	spent := 0.0
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
	l := Lock{Method: MethodVersion, Agentium: r.Version, LockedAt: r.Now().UTC(), ClaudeCode: version,
		ClaudePath: cli, SignIn: r.SignIn, Host: runtime.GOOS + "/" + runtime.GOARCH, PriceTable: pricing.Date, Design: d,
		Schedule: Schedule(d), MaxAttempts: MaxAttempts}
	for _, a := range d.Arms {
		locked := LockedArm{Arm: a}
		if a.Snapshot != "" {
			snap, err := p.DB.SnapshotByName(ctx, p.ID, a.Context)
			if err != nil {
				return l, err
			}
			if snap.CommitID != a.Snapshot {
				return l, fmt.Errorf("snapshot %s now names commit %s, not the experiment's %s", a.Context, ShortCommit(snap.CommitID), ShortCommit(a.Snapshot))
			}
			var manifest snapshot.Manifest
			if err := json.Unmarshal(snap.Manifest, &manifest); err != nil {
				return l, fmt.Errorf("snapshot %s: %w", a.Context, err)
			}
			for _, f := range manifest.Files {
				locked.Files = append(locked.Files, FileDigest{Path: f.Path, SHA256: f.SHA256})
			}
		}
		stored, err := p.DB.LatestCalibration(ctx, p.ID, a.Context, a.Snapshot)
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
		l.Tasks = append(l.Tasks, NewLockedTask(t.Name, t.Instruction, task.Spec{Base: t.BaseCommit, Solution: t.SolutionCommit,
			HiddenTests: t.HiddenTests, Reference: t.Reference, Setup: t.Setup, Verify: t.Verify}))
	}
	return l, nil
}
