package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

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

func experimentRun(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("experiment run", flag.ContinueOnError)
	budget := fs.Float64("budget", 0, "raise the experiment's budget to this total in USD (recorded in its lock)")
	usageLimit := fs.Float64("usage-limit", defaultUsageLimit, "with a subscription, start no pair past this share of the five-hour window (percent)")
	wait := fs.Bool("wait", false, "at the usage limit, wait for the window to reset instead of pausing")
	rest, code, ok := parseArgs(env, fs, args, experimentUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 || *budget < 0 || *usageLimit <= 0 || *usageLimit > 100 {
		fmt.Fprint(env.Stderr, experimentUsage)
		return ExitUsage
	}
	name, out := rest[0], env.Stdout
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	stored, err := w.db.ExperimentByName(ctx, w.project.ID, name)
	if err != nil {
		return fail(env, err)
	}
	d, err := loadExperiment(ctx, w, name)
	if err != nil {
		return fail(env, err)
	}
	cli, err := claudePath(env)
	if err != nil {
		return fail(env, err)
	}
	version, err := project.ClaudeVersion(ctx, cli)
	if err != nil {
		return fail(env, fmt.Errorf("Claude Code at %s: its version could not be read: %w", cli, err))
	}
	mode, _ := signInMode(env)
	release, err := startRuns(ctx, env, w)
	if err != nil {
		return fail(env, err)
	}
	defer release()

	var lock experiment.Lock
	if stored.Lock == nil {
		var raised *experiment.BudgetChange
		if *budget > 0 && *budget != d.BudgetUSD { // the design's budget, changed before anything ran
			if *budget < d.BudgetUSD {
				fmt.Fprintf(env.Stderr, "agentium experiment run: the budget can only be raised (it is $%.2f)\n", d.BudgetUSD)
				return ExitUsage
			}
			raised = &experiment.BudgetChange{At: env.Now().UTC(), From: d.BudgetUSD, To: *budget}
			d.BudgetUSD = *budget
		}
		fmt.Fprintf(out, "Checking experiment %s before its first run:\n", name)
		eligible, reasons, err := eligibleTasks(ctx, w, d.Arms)
		if err != nil {
			return fail(env, err)
		}
		est, err := estimateRun(ctx, w, d.Model)
		if err != nil {
			return fail(env, err)
		}
		if !printReadiness(ctx, env, w, d, eligible, reasons, est) || ctx.Err() != nil {
			return fail(env, errors.Join(ctx.Err(), errors.New("not ready to run: see above (agentium experiment plan "+name+")")))
		}
		if lock, err = buildLock(ctx, env, w, d, cli, version, mode); err != nil {
			return fail(env, err)
		}
		if raised != nil {
			lock.BudgetChanges = append(lock.BudgetChanges, *raised)
		}
		encoded, err := json.Marshal(lock)
		if err != nil {
			return fail(env, fmt.Errorf("encode lock: %w", err))
		}
		if err := w.db.LockExperiment(ctx, stored.ID, encoded); err != nil {
			return fail(env, err)
		}
		if raised != nil {
			fmt.Fprintf(out, "Budget raised to $%.2f (recorded in the lock).\n", raised.To)
		}
		fmt.Fprintf(out, "Locked: Claude Code %s, %s, sign-in %s, %d runs in a seeded order (seed %d), prices of %s.\n",
			lock.ClaudeCode, d.Model, lock.SignIn, len(lock.Schedule), d.Seed, lock.PriceTable)
	} else {
		if err := json.Unmarshal(stored.Lock, &lock); err != nil {
			return fail(env, fmt.Errorf("experiment %s: its lock cannot be read: %w", name, err))
		}
		if err := lock.Check(version, mode); err != nil {
			return fail(env, fmt.Errorf("experiment %s cannot continue: %w", name, err))
		}
		if host := runtime.GOOS + "/" + runtime.GOARCH; host != lock.Host {
			return fail(env, fmt.Errorf("experiment %s cannot continue: its runs ran on %s, this is %s", name, lock.Host, host))
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
			if _, err := gitx.Run(ctx, "--git-dir", w.bare, "cat-file", "-e", c+"^{commit}"); err != nil {
				return fail(env, fmt.Errorf("experiment %s cannot continue: commit %s is gone from Agentium's repository", name, shortCommit(c)))
			}
		}
		fmt.Fprintf(out, "Resuming experiment %s (locked %s on Claude Code %s).\n", name, lock.LockedAt.Format("2006-01-02 15:04"), lock.ClaudeCode)
	}
	if stored.Lock != nil && *budget > 0 && *budget != lock.Design.BudgetUSD {
		if *budget < lock.Design.BudgetUSD {
			fmt.Fprintf(env.Stderr, "agentium experiment run: the budget can only be raised (it is $%.2f)\n", lock.Design.BudgetUSD)
			return ExitUsage
		}
		lock.BudgetChanges = append(lock.BudgetChanges, experiment.BudgetChange{At: env.Now().UTC(), From: lock.Design.BudgetUSD, To: *budget})
		lock.Design.BudgetUSD = *budget
		encoded, err := json.Marshal(lock)
		if err != nil {
			return fail(env, fmt.Errorf("encode lock: %w", err))
		}
		if err := w.db.AmendLock(ctx, stored.ID, encoded); err != nil {
			return fail(env, err)
		}
		fmt.Fprintf(out, "Budget raised to $%.2f (recorded in the lock).\n", *budget)
	}

	runs, err := w.db.ExperimentRuns(ctx, stored.ID)
	if err != nil {
		return fail(env, err)
	}
	var prior []experiment.Attempt
	storedTries := map[int]int{} // runs per slot so far
	for _, r := range runs {
		prior = append(prior, experiment.Attempt{Slot: r.Slot, Outcome: r.Outcome, CostUSD: r.CostUSD})
		storedTries[r.Slot]++
	}
	var triesMu sync.Mutex
	tries := maps.Clone(storedTries)
	projectRuns, err := w.db.Runs(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	var gate *experiment.UsageGate // an API key uses no subscription: its runs never pause for one
	if lock.SignIn != claude.SignInAPIKey {
		samples := usageSamples(projectRuns)
		gate = &experiment.UsageGate{Limit: *usageLimit / 100}
		gate.PerRun, _ = experiment.UsagePerRun(samples)
		gate.Latest, _ = experiment.LatestUsage(samples)
		if *wait {
			gate.Wait = func(ctx context.Context, until time.Time) error { return waitUntil(ctx, env, until) }
		}
	}
	var subagentsMu sync.Mutex
	seenSubagents := subagentModels(runs) // per arm: the models each subagent type ran on in this experiment so far
	runEnv, err := newRunEnv(env, w, lock.Design.VerifyTimeout)
	if err != nil {
		return fail(env, err)
	}
	runEnv.Progress = nil                                                                     // the scheduler reports one line per run
	if err := w.db.SetExperimentStatus(ctx, stored.ID, store.StatusRunning, ""); err != nil { // stays so if this process dies: show tells
		return fail(env, err)
	}
	total, design := len(lock.Schedule), lock.Design
	fmt.Fprintf(out, "Running up to %d at a time; each run up to $%.2f and %s; budget $%.2f. Ctrl-C stops it; run it again to resume.\n",
		design.Concurrency, design.RunBudgetUSD, design.Timeout, design.BudgetUSD)
	progress := func(e experiment.Event) {
		label := fmt.Sprintf("[%d/%d] %s, arm %s, repeat %d", e.Slot.Position+1, total, e.Slot.Task, e.Slot.Arm, e.Slot.Repeat)
		switch e.Kind {
		case "start":
			if e.Attempt > 1 {
				label += fmt.Sprintf(" (attempt %d of %d)", e.Attempt, lock.MaxAttempts)
			}
			fmt.Fprintf(out, "%s: started\n", label)
		case "finish":
			fmt.Fprintf(out, "%s: %s, $%.2f (spent $%.2f of $%.2f)\n", label, orNone(e.Result.Outcome), e.Result.CostUSD, e.SpentUSD, design.BudgetUSD)
		case "retry":
			fmt.Fprintf(out, "%s: retrying in %s\n", label, e.RetryIn)
		case "wait":
			fmt.Fprintf(out, "Usage: the five-hour window is at %.0f%%; waiting for it to reset at %s (Ctrl-C stops; run it again to resume).\n",
				100*e.Usage, clock(e.Until, env.Now()))
		}
	}
	execute := func(ctx context.Context, slot experiment.Slot, attempt int, overlap []int) (experiment.Result, error) {
		arm, ok := lock.Arm(slot.Arm)
		t, ok2 := lock.Task(slot.Task)
		if !ok || !ok2 {
			return experiment.Result{}, fmt.Errorf("the lock has no arm %s or task %s", slot.Arm, slot.Task)
		}
		triesMu.Lock()
		tries[slot.Position]++
		try := tries[slot.Position]
		triesMu.Unlock()
		e := runEnv
		e.Expect = arm.Expect(lock.ClaudeCode)
		e.Workspace = experimentWorkspace(stored.ID, slot.Position, try)
		e.DenyExtra = nil
		for _, q := range overlap { // in this execution a slot runs at most MaxAttempts more times
			for t := 1; t <= storedTries[q]+lock.MaxAttempts; t++ {
				e.DenyExtra = append(e.DenyExtra, e.Predicted(experimentWorkspace(stored.ID, q, t))...)
			}
		}
		meta := runMeta{Kind: "task", ExperimentID: stored.ID, Slot: slot.Position, Attempt: attempt}
		if current, err := w.db.TaskByName(ctx, w.project.ID, t.Name); err == nil && experiment.NewLockedTask(current.Name, current.Instruction,
			task.Spec{Base: current.BaseCommit, Solution: current.SolutionCommit, HiddenTests: current.HiddenTests, Reference: current.Reference,
				Setup: current.Setup, Verify: current.Verify}).Digest == t.Digest {
			meta.TaskID = current.ID // linked only while the task is the one the lock ran
		}
		rec, err := executeRun(ctx, env, w, e, meta, run.Spec{TaskName: t.Name, Instruction: t.Instruction, Task: t.Spec(),
			Arm: task.Arm{Name: arm.Name, Snapshot: arm.Snapshot}, Model: design.Model, Effort: design.Effort, BudgetUSD: design.RunBudgetUSD,
			Timeout: design.Timeout})
		result := experiment.Result{Outcome: rec.Outcome, CostUSD: rec.Metrics.CostUSD, Usage: rec.Metrics.UsageLast}
		switch m := rec.Metrics; {
		case m.SawInit && m.CLIVersion != lock.ClaudeCode:
			result.Stop = fmt.Sprintf("Claude Code reported version %s, but the experiment is locked to %s: later runs would not compare", m.CLIVersion, lock.ClaudeCode)
		case m.SawInit && arm.Model != "" && m.Model != arm.Model:
			result.Stop = fmt.Sprintf("Claude Code reported model %s, but arm %s's calibration saw %s: later runs would not compare", m.Model, arm.Name, arm.Model)
		}
		// A role's model alias can move to a newer model with Claude Code while --model stays pinned: the runs after
		// it would not compare with those before.
		subagentsMu.Lock()
		// Per arm: arms may give a role different models on purpose (that is a context difference to measure).
		if seenSubagents[arm.Name] == nil {
			seenSubagents[arm.Name] = map[string][]string{}
		}
		if changes := claude.SubagentModelChanges(seenSubagents[arm.Name], rec.Metrics.SubagentModels); len(changes) > 0 {
			if result.Stop == "" {
				result.Stop = "arm " + arm.Name + ": " + strings.Join(changes, "; ") + ": later runs would not compare"
			}
		} else {
			mergeSubagentModels(seenSubagents[arm.Name], rec.Metrics.SubagentModels)
		}
		subagentsMu.Unlock()
		return result, err
	}
	backoff := env.Backoff
	if backoff == nil {
		backoff = retryBackoff
	}
	sum, runErr := experiment.Execute(ctx, experiment.Plan{Schedule: lock.Schedule, Concurrency: design.Concurrency, RunCapUSD: design.RunBudgetUSD,
		BudgetUSD: design.BudgetUSD, MaxAttempts: lock.MaxAttempts, Prior: prior, Backoff: backoff, Progress: progress, Usage: gate}, execute)
	if sum.Status == "" { // Execute refused its input
		sum.Status, sum.Note = experiment.StatusStopped, "Agentium could not start the runs: "+runErr.Error()
	}
	if err := w.db.SetExperimentStatus(context.WithoutCancel(ctx), stored.ID, sum.Status, sum.Note); err != nil {
		return fail(env, errors.Join(runErr, err))
	}
	fmt.Fprintln(out)
	if err := printProgress(context.WithoutCancel(ctx), env, w, name, stored.ID, lock); err != nil {
		return fail(env, errors.Join(runErr, err))
	}
	switch {
	case runErr != nil:
		return fail(env, runErr)
	case sum.Status == experiment.StatusDone:
		fmt.Fprintf(out, "Every run is done. The report: agentium experiment report %s\n", name)
		return ExitOK
	case sum.Status == experiment.StatusBudget:
		fmt.Fprintf(out, "Stopped at the budget. To continue: agentium experiment run %s --budget USD (a higher total)\n", name)
		return ExitOK
	case sum.Status == experiment.StatusUsage && !sum.ResumeAt.IsZero():
		fmt.Fprintf(out, "Paused before the usage limit; the window resets at %s. To continue: agentium experiment run %s (--wait waits for the reset)\n",
			clock(sum.ResumeAt, env.Now()), name)
		return ExitOK
	case sum.Status == experiment.StatusUsage:
		fmt.Fprintf(out, "Paused: a pair needs more of the usage window than the limit allows. To continue: agentium experiment run %s --usage-limit PCT\n", name)
		return ExitOK
	}
	fmt.Fprintf(out, "Stopped. To continue: agentium experiment run %s\n", name)
	return ExitError
}

// buildLock fixes an experiment for its first run: the machine's Claude Code, each arm's context files and calibrated
// environment, each task's full specification, and the schedule.
func buildLock(ctx context.Context, env Env, w *workspace, d experiment.Design, cli, version, signIn string) (experiment.Lock, error) {
	l := experiment.Lock{Method: experiment.MethodVersion, Agentium: env.Version, LockedAt: env.Now().UTC(), ClaudeCode: version,
		ClaudePath: cli, SignIn: signIn, Host: runtime.GOOS + "/" + runtime.GOARCH, PriceTable: pricing.Date, Design: d,
		Schedule: experiment.Schedule(d), MaxAttempts: experiment.MaxAttempts}
	for _, a := range d.Arms {
		locked := experiment.LockedArm{Arm: a}
		if a.Snapshot != "" {
			snap, err := w.db.SnapshotByName(ctx, w.project.ID, a.Context)
			if err != nil {
				return l, err
			}
			if snap.CommitID != a.Snapshot {
				return l, fmt.Errorf("snapshot %s now names commit %s, not the experiment's %s", a.Context, shortCommit(snap.CommitID), shortCommit(a.Snapshot))
			}
			var manifest snapshot.Manifest
			if err := json.Unmarshal(snap.Manifest, &manifest); err != nil {
				return l, fmt.Errorf("snapshot %s: %w", a.Context, err)
			}
			for _, f := range manifest.Files {
				locked.Files = append(locked.Files, experiment.FileDigest{Path: f.Path, SHA256: f.SHA256})
			}
		}
		stored, err := w.db.LatestCalibration(ctx, w.project.ID, a.Context, a.Snapshot)
		if err != nil {
			return l, err
		}
		var c calibration
		if err := json.Unmarshal(stored.Result, &c); err != nil {
			return l, fmt.Errorf("calibration of %s: %w", a.Context, err)
		}
		locked.Calibration, locked.Model, locked.Tools, locked.Skills, locked.SlashCommands = stored.RunID, c.Model, c.Tools, c.Skills, c.SlashCommands
		l.Arms = append(l.Arms, locked)
	}
	for _, name := range d.Tasks {
		t, err := w.db.TaskByName(ctx, w.project.ID, name)
		if err != nil {
			return l, err
		}
		l.Tasks = append(l.Tasks, experiment.NewLockedTask(t.Name, t.Instruction, task.Spec{Base: t.BaseCommit, Solution: t.SolutionCommit,
			HiddenTests: t.HiddenTests, Reference: t.Reference, Setup: t.Setup, Verify: t.Verify}))
	}
	return l, nil
}

// armCounts is one arm's progress.
type armCounts struct {
	Fair, Successes, Passed, Unfair, Infra, Cancelled, Settled int
	CostUSD                                                    float64
}

// printProgress shows where an experiment stands, per arm, from its stored runs.
func printProgress(ctx context.Context, env Env, w *workspace, name string, id int64, lock experiment.Lock) error {
	stored, err := w.db.ExperimentByName(ctx, w.project.ID, name)
	if err != nil {
		return err
	}
	runs, err := w.db.ExperimentRuns(ctx, id)
	if err != nil {
		return err
	}
	counts := map[string]*armCounts{}
	for _, a := range lock.Arms {
		counts[a.Name] = &armCounts{}
	}
	settled := map[int]bool{}
	spent := 0.0
	for _, r := range runs {
		c := counts[r.Arm]
		if c == nil {
			continue
		}
		spent += r.CostUSD
		c.CostUSD += r.CostUSD
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return fmt.Errorf("run %s: %w", r.ID, err)
		}
		switch {
		case experiment.Fair(r.Outcome):
			c.Fair++
			if r.Passed != nil && *r.Passed {
				c.Passed++
			}
			if experiment.Success(r.Outcome, r.Passed, rec.Behavior.ConfigChanged) {
				c.Successes++
			}
		case r.Outcome == "unfair":
			c.Unfair++
		case r.Outcome == "cancelled":
			c.Cancelled++
		default:
			c.Infra++
		}
		if experiment.Settles(r.Outcome) && !settled[r.Slot] {
			settled[r.Slot] = true
			c.Settled++
		}
	}
	out := env.Stdout
	status := stored.Status
	if status == experiment.StatusUsage {
		status = "paused at the usage limit"
	}
	if stored.StatusNote != "" {
		status += ": " + stored.StatusNote
	}
	if stored.Status == store.StatusRunning && !w.layout.RunsBusy() {
		status = "stopped (its Agentium process ended; run it again to resume)"
	}
	fmt.Fprintf(out, "Experiment %s: %s\n", name, status)
	fmt.Fprintf(out, "  %d of %d runs settled; spent $%.2f of $%.2f\n", len(settled), len(lock.Schedule), spent, lock.Design.BudgetUSD)
	fmt.Fprintf(out, "%-4s %-20s %8s %6s %10s %7s %6s %10s %9s\n", "ARM", "CONTEXT", "SETTLED", "FAIR", "SUCCESSES", "UNFAIR", "INFRA", "CANCELLED", "COST")
	for _, a := range lock.Arms {
		c := counts[a.Name]
		fmt.Fprintf(out, "%-4s %-20s %8s %6d %10d %7d %6d %10d %9s\n", a.Name, a.Context, fmt.Sprintf("%d/%d", c.Settled, len(lock.Schedule)/2),
			c.Fair, c.Successes, c.Unfair, c.Infra, c.Cancelled, fmt.Sprintf("$%.2f", c.CostUSD))
		if c.Passed > c.Successes {
			fmt.Fprintf(out, "  arm %s: %d passing run(s) changed the test runner's configuration beyond the task's reference: not counted as successes\n", a.Name, c.Passed-c.Successes)
		}
	}
	fmt.Fprintln(out, "Successes need a pass with the hidden tests; unfair (drifted), infrastructure and cancelled runs are not counted.")
	return nil
}

func experimentShow(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("experiment show", flag.ContinueOnError), args, experimentUsage)
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
	d, err := loadExperiment(ctx, w, rest[0])
	if err != nil {
		return fail(env, err)
	}
	out := env.Stdout
	if stored.Lock == nil {
		fmt.Fprintf(out, "Experiment %s: %s; not run yet. Preview: agentium experiment plan %s\n", rest[0], describeArms(d), rest[0])
		return ExitOK
	}
	var lock experiment.Lock
	if err := json.Unmarshal(stored.Lock, &lock); err != nil {
		return fail(env, fmt.Errorf("experiment %s: its lock cannot be read: %w", rest[0], err))
	}
	fmt.Fprintf(out, "%s; %d task(s) × %d run(s) per arm; %s\n", describeArms(d), len(lock.Tasks), d.Repeats, d.Model)
	fmt.Fprintf(out, "Locked %s: Claude Code %s, sign-in %s, %s, method %s, prices of %s\n", lock.LockedAt.Format("2006-01-02 15:04"),
		lock.ClaudeCode, lock.SignIn, lock.Host, lock.Method, lock.PriceTable)
	for _, c := range lock.BudgetChanges {
		fmt.Fprintf(out, "Budget raised %s: $%.2f to $%.2f\n", c.At.Format("2006-01-02 15:04"), c.From, c.To)
	}
	if err := printProgress(ctx, env, w, rest[0], stored.ID, lock); err != nil {
		return fail(env, err)
	}
	return ExitOK
}
