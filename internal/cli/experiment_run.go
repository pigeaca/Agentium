package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"sync"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/term"
)

func experimentRun(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("experiment run", flag.ContinueOnError)
	var o experiment.RunOptions
	fs.Float64Var(&o.Budget, "budget", 0, "raise the experiment's budget to this total in USD (recorded in its lock)")
	fs.Float64Var(&o.UsageLimit, "usage-limit", experiment.DefaultUsageLimit, "with a subscription, start no pair past this share of the five-hour window (percent)")
	fs.BoolVar(&o.Wait, "wait", false, "at the usage limit, wait for the window to reset instead of pausing")
	rest, code, ok := parseArgs(env, fs, args, experimentUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 || o.Budget < 0 || o.UsageLimit <= 0 || o.UsageLimit > 100 {
		fmt.Fprint(env.Stderr, experimentUsage)
		return ExitUsage
	}
	env, live := liveEnv(env)
	defer live.Stop() // covers early returns and interrupts; the summary below stops it first
	name := rest[0]
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	runner, release := experimentRunner(env, w, live)
	defer release() // the run lock is held until the summary is printed
	outcome, err := runner.Run(ctx, name, o)
	var usage experiment.UsageError
	if errors.As(err, &usage) {
		fmt.Fprintf(env.Stderr, "agentium experiment run: %s\n", usage)
		return ExitUsage
	} else if err != nil {
		return fail(env, err)
	}
	if outcome.Err != nil {
		return fail(env, outcome.Err)
	}
	if !outcome.Conclude(env.Stdout, env.style(), name, env.Now()) {
		return ExitError
	}
	return ExitOK
}

// experimentRunner is what an experiment's execution needs from the command line, with its live status line and its
// one line per run wired to the scheduler's events.
func experimentRunner(env Env, w *workspace, live *term.StatusLine) (r experiment.Runner, release func()) {
	var held func() // set when the run lock is taken
	release = func() {
		if held != nil {
			held()
		}
	}
	mode, _ := signInMode(env)
	var status *runStatus
	var report func(experiment.Event)
	r = experiment.Runner{Project: w.service(), Out: env.Stdout, Style: env.style(), Now: env.Now, Version: env.Version, SignIn: mode,
		Claude:    func() (string, error) { return claudePath(env) },
		Readiness: readinessEnv(env),
		StartRuns: func(ctx context.Context) (err error) {
			held, err = startRuns(ctx, env, w)
			return err
		},
		KeepHead:  func(ctx context.Context) (string, error) { return w.keepCommit(ctx, "HEAD") },
		NewRunEnv: func(verifyTimeout time.Duration) (run.Env, error) { return newRunEnv(env, w, verifyTimeout) },
		NeedsLocalBinding: func(ctx context.Context, bases []string) (needed, allowed bool, err error) {
			needed, err = run.NeedsLocalBinding(ctx, w.bare, bases)
			return needed, w.project.AllowLocalBinding, err
		},
		ExecuteRun: func(ctx context.Context, e run.Env, meta experiment.RunMeta, spec run.Spec) (run.Record, error) {
			kind := "task"
			if meta.Kind == experiment.KindCalibration {
				kind = meta.Kind
			}
			return executeRun(ctx, env, w, e, runMeta{Kind: kind, TaskID: meta.TaskID, ExperimentID: meta.ExperimentID, Slot: meta.Slot,
				Attempt: meta.Attempt}, spec)
		},
		WaitUntil: func(ctx context.Context, until time.Time) error { return waitUntil(ctx, env, until) },
		Backoff:   env.Backoff,
	}
	r.Observer = experiment.Observer{
		Begin: func(lock experiment.Lock, s experiment.Standing) {
			status = &runStatus{total: len(lock.Schedule), budget: lock.Design.BudgetUSD, settled: s.Settled, spent: s.Spent, usage: s.Usage, hasUsage: s.HasUsage}
			report = progressLines(env, lock)
			live.Show(func() string { return status.text(env.Now()) })
		},
		Event: func(e experiment.Event) {
			status.update(e) // every event prints a line below, which redraws the status line with the new numbers
			report(e)
		},
		Finish: live.Stop,
	}
	return r, release
}

// progressLines prints one line per scheduler event: a run started, finished, to be retried, or waiting for the usage
// window.
func progressLines(env Env, lock experiment.Lock) func(experiment.Event) {
	out, st, total, design := env.Stdout, env.style(), len(lock.Schedule), lock.Design
	return func(e experiment.Event) {
		label := fmt.Sprintf("[%d/%d] %s, arm %s, repeat %d", e.Slot.Position+1, total, e.Slot.Task, e.Slot.Arm, e.Slot.Repeat)
		switch e.Kind {
		case "start":
			if e.Attempt > 1 {
				label += fmt.Sprintf(" (attempt %d of %d)", e.Attempt, lock.MaxAttempts)
			}
			fmt.Fprintf(out, "%s: started\n", label)
		case "finish":
			outcome := st.Status(term.OrNone(e.Result.Outcome))
			if e.Result.Outcome == "" && e.Requeued {
				// Execute reruns such a run on resume and does not count it as an attempt.
				outcome = st.Warn("stopped before its agent started (not counted; it runs again on resume)")
			}
			judged := ""
			if e.Result.Judge != "" {
				judged = fmt.Sprintf("; judge: %s, $%.2f", e.Result.Judge, e.Result.JudgeUSD)
			}
			fmt.Fprintf(out, "%s: %s, $%.2f%s (spent $%.2f of $%.2f)\n", label, outcome, e.Result.AgentUSD(), judged, e.SpentUSD, design.BudgetUSD)
		case "retry":
			fmt.Fprintf(out, "%s: %s in %s\n", label, st.Warn("retrying"), e.RetryIn)
		case "wait":
			fmt.Fprintln(out, st.Warn(fmt.Sprintf("Usage: the five-hour window is at %.0f%%; waiting for it to reset at %s (Ctrl-C stops; run it again to resume).",
				100*e.Usage, experiment.Clock(e.Until, env.Now()))))
		case "look":
			if e.Look != nil {
				fmt.Fprintln(out, st.Heading(upperFirst(experiment.DescribeLook(*e.Look, e.Looks))))
			}
		}
	}
}

// upperFirst capitalizes a line's first ASCII letter.
func upperFirst(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
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
	d, err := w.service().Load(ctx, rest[0])
	if err != nil {
		return fail(env, err)
	}
	out := env.Stdout
	if stored.Lock == nil {
		fmt.Fprintf(out, "Experiment %s: %s; not run yet. Preview: %s\n", rest[0], experiment.DescribeArms(d), env.style().Command("agentium experiment plan "+rest[0]))
		if spent, err := w.service().CalibrationSpend(ctx, stored.ID); err != nil {
			return fail(env, err)
		} else if spent > 0 {
			fmt.Fprintf(out, "Calibration runs so far: $%.2f (in its budget)\n", spent)
		}
		return ExitOK
	}
	var lock experiment.Lock
	if err := json.Unmarshal(stored.Lock, &lock); err != nil {
		return fail(env, fmt.Errorf("experiment %s: its lock cannot be read: %w", rest[0], err))
	}
	fmt.Fprintf(out, "%s; %d task(s) × %d run(s) per arm; %s\n", experiment.DescribeArms(d), len(lock.Tasks), d.Repeats, d.ModelLabel())
	fmt.Fprintf(out, "Locked %s: Claude Code %s, sign-in %s, %s, method %s, prices of %s\n", lock.LockedAt.Format("2006-01-02 15:04"),
		lock.ClaudeCode, lock.SignIn, lock.Host, lock.Method, lock.PriceTable)
	if j := lock.Design.Judge; j != nil {
		fmt.Fprintf(out, "Judge: %s (a second opinion beside the tests)\n", experiment.DescribeJudge(*j))
	}
	for _, c := range lock.BudgetChanges {
		fmt.Fprintf(out, "Budget raised %s: $%.2f to $%.2f\n", c.At.Format("2006-01-02 15:04"), c.From, c.To)
	}
	if err := w.service().WriteProgress(ctx, env.Stdout, env.style(), rest[0], stored.ID, lock); err != nil {
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
			experiment.Clock(s.until, now), term.Elapsed(max(s.until.Sub(now), 0)))
	}
	text := fmt.Sprintf("%d of %d settled; %d in flight; $%.2f of $%.2f", len(s.settled), s.total, s.inflight, s.spent, s.budget)
	if s.hasUsage {
		text += fmt.Sprintf("; usage %.0f%%", 100*s.usage.FiveHourAt(now))
	}
	return text
}
