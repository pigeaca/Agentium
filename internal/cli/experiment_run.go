package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
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
	env, live := liveEnv(env)
	defer live.Stop() // covers early returns and interrupts; the summary below stops it first
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
		fmt.Fprintln(out, env.style().Heading(fmt.Sprintf("Checking experiment %s before its first run:", name)))
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
		if d.Judge != nil {
			fmt.Fprintf(out, "The judge: %s.\n", describeJudge(*d.Judge))
		}
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
	runEnv, err := newRunEnv(env, w, lock.Design.VerifyTimeout)
	if err != nil {
		return fail(env, err)
	}
	runEnv.Progress = nil                                                                     // the scheduler reports one line per run
	if err := w.db.SetExperimentStatus(ctx, stored.ID, store.StatusRunning, ""); err != nil { // stays so if this process dies: show tells
		return fail(env, err)
	}
	// The judge's pause: a verdict stopped at a usage limit or a sign-in failure, which every later call would hit too.
	var judgePaused atomic.Bool
	var judgeNote string
	var judgeErr error
	if lock.Design.Judge != nil { // first the graded runs a stopped execution left without a verdict
		judgeNote, judgeErr = judgePending(ctx, env, w, runEnv, lock, runs)
	}
	var prior []experiment.Attempt
	storedTries := map[int]int{} // runs per slot so far
	status := runStatus{total: len(lock.Schedule), budget: lock.Design.BudgetUSD, settled: map[int]bool{}}
	for _, r := range runs {
		// The budget counts the judge's spend too; the cost column (r.CostUSD) is the agent's alone.
		spentOn := r.CostUSD + judgeCostUSD(r.Record)
		prior = append(prior, experiment.Attempt{Slot: r.Slot, Outcome: r.Outcome, CostUSD: spentOn})
		status.spent += spentOn
		if experiment.Settles(r.Outcome) {
			status.settled[r.Slot] = true
		}
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
		var have bool
		if gate.Latest, have = experiment.LatestUsage(samples); have {
			status.usage, status.hasUsage = gate.Latest, true
		}
		if *wait {
			gate.Wait = func(ctx context.Context, until time.Time) error { return waitUntil(ctx, env, until) }
		}
	}
	var subagentsMu sync.Mutex
	seenSubagents := subagentModels(runs) // per arm: the models each subagent type ran on in this experiment so far
	total, design, st := len(lock.Schedule), lock.Design, env.style()
	judging := ""
	if design.Judge != nil {
		judging = fmt.Sprintf(" and its judgement up to $%.2f", design.JudgeCapUSD())
	}
	fmt.Fprintf(out, "Running up to %d at a time; each run up to $%.2f%s and %s; budget $%.2f. Ctrl-C stops it; run it again to resume.\n",
		design.Concurrency, design.RunBudgetUSD, judging, design.Timeout, design.BudgetUSD)
	live.Show(func() string { return status.text(env.Now()) })
	progress := func(e experiment.Event) {
		status.update(e) // every event prints a line below, which redraws the status line with the new numbers
		label := fmt.Sprintf("[%d/%d] %s, arm %s, repeat %d", e.Slot.Position+1, total, e.Slot.Task, e.Slot.Arm, e.Slot.Repeat)
		switch e.Kind {
		case "start":
			if e.Attempt > 1 {
				label += fmt.Sprintf(" (attempt %d of %d)", e.Attempt, lock.MaxAttempts)
			}
			fmt.Fprintf(out, "%s: started\n", label)
		case "finish":
			outcome := st.Status(orNone(e.Result.Outcome))
			if e.Result.Outcome == "" && e.Requeued {
				// Execute reruns such a run on resume and does not count it as an attempt.
				outcome = st.Warn("stopped before its agent started (not counted; it runs again on resume)")
			}
			judged := ""
			if e.Result.Judge != "" {
				judged = fmt.Sprintf("; judge: %s, $%.2f", e.Result.Judge, e.Result.JudgeUSD)
			}
			fmt.Fprintf(out, "%s: %s, $%.2f%s (spent $%.2f of $%.2f)\n", label, outcome, e.Result.CostUSD-e.Result.JudgeUSD, judged, e.SpentUSD, design.BudgetUSD)
		case "retry":
			fmt.Fprintf(out, "%s: %s in %s\n", label, st.Warn("retrying"), e.RetryIn)
		case "wait":
			fmt.Fprintln(out, st.Warn(fmt.Sprintf("Usage: the five-hour window is at %.0f%%; waiting for it to reset at %s (Ctrl-C stops; run it again to resume).",
				100*e.Usage, clock(e.Until, env.Now()))))
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
			Timeout: design.Timeout, Judge: design.Judge})
		// The budget counts what the judge spent; the agent's cost stays rec.Metrics.CostUSD, the analysis's.
		result := experiment.Result{Outcome: rec.Outcome, CostUSD: rec.Metrics.CostUSD + rec.JudgeCostUSD(), JudgeUSD: rec.JudgeCostUSD(),
			Usage: rec.Metrics.UsageLast}
		if v := rec.Judge; v != nil {
			result.Judge = run.Describe(*v)
			if v.Stopped == llmjudge.StoppedLimit {
				result.Pause = judgeLimitNote
				judgePaused.Store(true)
			}
		}
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
	var sum experiment.Summary
	var runErr error
	switch {
	case judgeErr != nil:
		sum, runErr = experiment.Summary{Status: experiment.StatusStopped, Note: "Agentium could not store a judgement: " + judgeErr.Error()}, judgeErr
	case judgeNote != "":
		sum = experiment.Summary{Status: experiment.StatusUsage, Note: judgeNote}
		judgePaused.Store(true)
	default:
		sum, runErr = experiment.Execute(ctx, experiment.Plan{Schedule: lock.Schedule, Concurrency: design.Concurrency, RunCapUSD: design.RunCapUSD(),
			BudgetUSD: design.BudgetUSD, MaxAttempts: lock.MaxAttempts, Prior: prior, Backoff: backoff, Progress: progress, Usage: gate}, execute)
	}
	if sum.Status == "" { // Execute refused its input
		sum.Status, sum.Note = experiment.StatusStopped, "Agentium could not start the runs: "+runErr.Error()
	}
	// Every slot settled is not done while a graded run still needs the judge: the last runs' judgements may have
	// stopped at a usage limit or an interrupt.
	if sum.Status == experiment.StatusDone && design.Judge != nil {
		if n, err := unjudged(context.WithoutCancel(ctx), w, stored.ID, lock); err != nil {
			runErr = errors.Join(runErr, err)
		} else if n > 0 && judgePaused.Load() {
			sum.Status, sum.Note = experiment.StatusUsage, judgeLimitNote
		} else if n > 0 {
			sum.Status, sum.Note = experiment.StatusStopped, fmt.Sprintf("%d run(s) still need the judge", n)
		}
	}
	if err := w.db.SetExperimentStatus(context.WithoutCancel(ctx), stored.ID, sum.Status, sum.Note); err != nil {
		return fail(env, errors.Join(runErr, err))
	}
	live.Stop()
	fmt.Fprintln(out)
	if err := printProgress(context.WithoutCancel(ctx), env, w, name, stored.ID, lock); err != nil {
		return fail(env, errors.Join(runErr, err))
	}
	switch {
	case runErr != nil:
		return fail(env, runErr)
	case sum.Status == experiment.StatusDone:
		fmt.Fprintf(out, "%s The report: %s\n", st.Good("Every run is done."), st.Command("agentium experiment report "+name))
		return ExitOK
	case sum.Status == experiment.StatusBudget:
		fmt.Fprintf(out, "%s To continue: %s (a higher total)\n", st.Warn("Stopped at the budget."), st.Command("agentium experiment run "+name+" --budget USD"))
		return ExitOK
	case sum.Status == experiment.StatusUsage && judgePaused.Load():
		fmt.Fprintf(out, "%s To continue, once it resets: %s (runs without a verdict are judged first; --wait does not wait for the judge)\n",
			st.Warn("Paused: the judge hit a usage limit or a sign-in failure."), st.Command("agentium experiment run "+name))
		return ExitOK
	case sum.Status == experiment.StatusUsage && !sum.ResumeAt.IsZero():
		fmt.Fprintf(out, "%s To continue: %s (--wait waits for the reset)\n",
			st.Warn(fmt.Sprintf("Paused before the usage limit; the window resets at %s.", clock(sum.ResumeAt, env.Now()))), st.Command("agentium experiment run "+name))
		return ExitOK
	case sum.Status == experiment.StatusUsage:
		fmt.Fprintf(out, "%s To continue: %s\n", st.Warn("Paused: a pair needs more of the usage window than the limit allows."),
			st.Command("agentium experiment run "+name+" --usage-limit PCT"))
		return ExitOK
	}
	fmt.Fprintf(out, "%s To continue: %s\n", st.Warn("Stopped."), st.Command("agentium experiment run "+name))
	return ExitError
}

// needsJudge reports whether a stored run of the experiment still needs the judge (run.NeedsJudging): only in an
// experiment with the judge, and only for fair runs of the lock's tasks.
func needsJudge(lock experiment.Lock, r store.Run, rec run.Record) bool {
	t, ok := lock.Task(r.TaskName)
	return lock.Design.Judge != nil && ok && experiment.Fair(r.Outcome) && run.NeedsJudging(rec, t.Spec())
}

// unjudged counts the experiment's stored runs that still need the judge.
func unjudged(ctx context.Context, w *workspace, id int64, lock experiment.Lock) (int, error) {
	runs, err := w.db.ExperimentRuns(ctx, id)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range runs {
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return 0, fmt.Errorf("run %s: %w", r.ID, err)
		}
		if needsJudge(lock, r, rec) {
			n++
		}
	}
	return n, nil
}

// judgeLimitNote is the status note of an experiment paused by the judge (Verdict.Stopped is judge.StoppedLimit).
const judgeLimitNote = "the judge hit a usage limit or a sign-in failure, which the next calls would hit too"

// judgeCostUSD is what the judge spent on a stored run, from its record: it counts against the budget, but is not in
// the run's cost column, which is the agent's alone and feeds estimates and the cost analysis.
func judgeCostUSD(record []byte) float64 {
	var r struct {
		Judge *struct {
			CostUSD float64 `json:"cost_usd"`
		} `json:"judge"`
	}
	if json.Unmarshal(record, &r) != nil || r.Judge == nil {
		return 0
	}
	return r.Judge.CostUSD
}

// judgePending judges, one at a time, the experiment's graded runs that still need it (run.NeedsJudging): those a
// stopped execution left without a verdict, and those whose judgement stopped early. Each is stored with its verdict
// in place (runs[i].Record too), so the spend that follows counts it. A judgement starts only when the spend so far and
// its cap fit the budget; one that does not is left for a resume with a higher budget. It returns a pause note when a
// verdict stopped at a usage limit, and an error only when a record cannot be read or stored. A cancelled ctx ends it
// quietly: the execution that follows sees the cancellation. Judgements that leave no verdict (a missing diff, say)
// are reported here, not stored, so resumes do not repeat their notes.
func judgePending(ctx context.Context, env Env, w *workspace, runEnv run.Env, lock experiment.Lock, runs []store.Run) (string, error) {
	design, out, st := lock.Design, env.Stdout, env.style()
	spent := 0.0
	for _, r := range runs {
		spent += r.CostUSD + judgeCostUSD(r.Record)
	}
	unfunded := 0
	for i, r := range runs {
		if ctx.Err() != nil {
			return "", nil
		}
		t, ok := lock.Task(r.TaskName)
		if !ok || !experiment.Fair(r.Outcome) {
			continue
		}
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return "", fmt.Errorf("run %s: %w", r.ID, err)
		}
		if !run.NeedsJudging(rec, t.Spec()) {
			continue
		}
		if spent+design.JudgeCapUSD() > design.BudgetUSD+1e-9 {
			unfunded++
			continue
		}
		before := rec.JudgeCostUSD()
		notes := len(rec.Notes)
		runEnv.Judge(ctx, run.Spec{TaskName: t.Name, Instruction: t.Instruction, Task: t.Spec()}, *design.Judge, &rec)
		label := fmt.Sprintf("Judged run %s (task %s, arm %s)", r.ID, r.TaskName, r.Arm)
		if rec.Judge == nil {
			fmt.Fprintf(out, "%s: %s\n", label, st.Warn(strings.Join(rec.Notes[notes:], "; ")))
			continue
		}
		spent += rec.JudgeCostUSD() - before
		encoded, err := json.Marshal(rec)
		if err != nil {
			return "", fmt.Errorf("encode run %s: %w", r.ID, err)
		}
		if err := w.db.SetRunRecord(context.WithoutCancel(ctx), r.ID, encoded); err != nil {
			return "", err
		}
		runs[i].Record = encoded
		fmt.Fprintf(out, "%s: %s, $%.2f (spent $%.2f of $%.2f)\n", label, run.Describe(*rec.Judge), rec.JudgeCostUSD()-before, spent, design.BudgetUSD)
		if rec.Judge.Stopped == llmjudge.StoppedLimit {
			return judgeLimitNote, nil
		}
	}
	if unfunded > 0 {
		fmt.Fprintln(out, st.Warn(fmt.Sprintf("%d run(s) still need the judge, but the budget leaves no room for a judgement ($%.2f): raise it with --budget",
			unfunded, design.JudgeCapUSD())))
	}
	return "", nil
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
	spent, judgeSpent := 0.0, 0.0
	unjudgedRuns := 0
	for _, r := range runs {
		c := counts[r.Arm]
		if c == nil {
			continue
		}
		judged := judgeCostUSD(r.Record)
		spent += r.CostUSD + judged // the budget's spend; the arm's cost is the agent's alone
		judgeSpent += judged
		c.CostUSD += r.CostUSD
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return fmt.Errorf("run %s: %w", r.ID, err)
		}
		if needsJudge(lock, r, rec) {
			unjudgedRuns++
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
	out, st := env.Stdout, env.style()
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
	fmt.Fprintf(out, "%s %s\n", st.Heading("Experiment "+name+":"), st.Heading(st.Status(status)))
	judged := ""
	if judgeSpent > 0 {
		judged = fmt.Sprintf(" (the judge $%.2f of it, not in the arms' costs)", judgeSpent)
	}
	fmt.Fprintf(out, "  %d of %d runs settled; spent $%.2f of $%.2f%s\n", len(settled), len(lock.Schedule), spent, lock.Design.BudgetUSD, judged)
	table := term.NewTable(st, term.Left("ARM"), term.Left("CONTEXT"), term.Right("SETTLED"), term.Right("FAIR"), term.Right("SUCCESSES"),
		term.Right("UNFAIR"), term.Right("INFRA"), term.Right("CANCELLED"), term.Right("COST"))
	for _, a := range lock.Arms {
		c := counts[a.Name]
		table.Row(a.Name, a.Context, fmt.Sprintf("%d/%d", c.Settled, len(lock.Schedule)/2), strconv.Itoa(c.Fair), strconv.Itoa(c.Successes),
			strconv.Itoa(c.Unfair), strconv.Itoa(c.Infra), strconv.Itoa(c.Cancelled), fmt.Sprintf("$%.2f", c.CostUSD))
		if c.Passed > c.Successes {
			table.Line(st.Warn(fmt.Sprintf("  arm %s: %d passing run(s) changed the test runner's configuration beyond the task's reference: not counted as successes", a.Name, c.Passed-c.Successes)))
		}
	}
	if err := table.Write(out); err != nil {
		return err
	}
	fmt.Fprintln(out, st.Note("Successes need a pass with the hidden tests; unfair (drifted), infrastructure and cancelled runs are not counted."))
	if unjudgedRuns > 0 {
		fmt.Fprintf(out, "%s %s\n", st.Warn(fmt.Sprintf("%d graded run(s) still need the judge:", unjudgedRuns)), st.Command("agentium experiment run "+name))
	}
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
		fmt.Fprintf(out, "Experiment %s: %s; not run yet. Preview: %s\n", rest[0], describeArms(d), env.style().Command("agentium experiment plan "+rest[0]))
		return ExitOK
	}
	var lock experiment.Lock
	if err := json.Unmarshal(stored.Lock, &lock); err != nil {
		return fail(env, fmt.Errorf("experiment %s: its lock cannot be read: %w", rest[0], err))
	}
	fmt.Fprintf(out, "%s; %d task(s) × %d run(s) per arm; %s\n", describeArms(d), len(lock.Tasks), d.Repeats, d.Model)
	fmt.Fprintf(out, "Locked %s: Claude Code %s, sign-in %s, %s, method %s, prices of %s\n", lock.LockedAt.Format("2006-01-02 15:04"),
		lock.ClaudeCode, lock.SignIn, lock.Host, lock.Method, lock.PriceTable)
	if j := lock.Design.Judge; j != nil {
		fmt.Fprintf(out, "Judge: %s (a second opinion beside the tests)\n", describeJudge(*j))
	}
	for _, c := range lock.BudgetChanges {
		fmt.Fprintf(out, "Budget raised %s: $%.2f to $%.2f\n", c.At.Format("2006-01-02 15:04"), c.From, c.To)
	}
	if err := printProgress(ctx, env, w, rest[0], stored.ID, lock); err != nil {
		return fail(env, err)
	}
	return ExitOK
}

// runStatus is what an experiment's live status line says, kept from the scheduler's events. The scheduler reports
// from several goroutines and the status line reads it from its ticker, so it has its own lock.
type runStatus struct {
	mu       sync.Mutex
	total    int
	budget   float64
	settled  map[int]bool // slots with a settled run, stored ones included
	inflight int
	spent    float64
	// usage is the latest reading, shown for the window that is open when the line is drawn: after a reset it reads
	// 0% until a run reports again.
	usage    claude.UsageReading
	hasUsage bool
	until    time.Time // when the usage window resets, while waiting for it
}

// read keeps u if it is later than the reading kept so far.
func (s *runStatus) read(u claude.UsageReading) {
	if !s.hasUsage || u.Newer(s.usage) {
		s.usage, s.hasUsage = u, true
	}
}

func (s *runStatus) update(e experiment.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Kind {
	case "start":
		s.inflight++
		s.until = time.Time{}
	case "finish":
		s.inflight = max(s.inflight-1, 0)
		s.spent = e.SpentUSD
		if experiment.Settles(e.Result.Outcome) {
			s.settled[e.Slot.Position] = true
		}
		if u := e.Result.Usage; u != nil {
			s.read(*u)
		}
	case "wait":
		s.until = e.Until
		s.read(claude.UsageReading{FiveHour: e.Usage, FiveHourResets: e.Until})
	}
}

// text is the status line's plain text at now.
func (s *runStatus) text(now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.until.IsZero() {
		return fmt.Sprintf("%d of %d settled; waiting for the usage window to reset at %s (in %s)", len(s.settled), s.total,
			clock(s.until, now), term.Elapsed(max(s.until.Sub(now), 0)))
	}
	text := fmt.Sprintf("%d of %d settled; %d in flight; $%.2f of $%.2f", len(s.settled), s.total, s.inflight, s.spent, s.budget)
	if s.hasUsage {
		text += fmt.Sprintf("; usage %.0f%%", 100*s.usage.FiveHourAt(now))
	}
	return text
}
