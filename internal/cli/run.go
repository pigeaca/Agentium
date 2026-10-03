package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// runUsage is run's help. It lists neither run once's expert flags (runHidden) nor run calibrate, which still works
// but which experiment run makes unneeded: the guide's "Advanced flags" table lists them.
const runUsage = `Usage:
  agentium run once TASK [--snapshot NAME] [--model MODEL[:EFFORT]] [--budget USD]
                     one real Claude Code run on TASK, in the base's own context or with a snapshot applied, on the
                     model (default ` + experiment.DefaultExperimentModel + `) at the effort (default: the CLI's).
                     It costs money (up to --budget, default $3) or uses your plan.
  agentium run list
  agentium run show ID [--diff] [--log]
                     one run: outcome, cost, behavior, environment; --diff adds the agent's change, --log the setup
                     and verification output

once, list and show take --json: one JSON document instead of text (docs/guide.md, "Scripting and automation").
Expert flags (timeouts, keeping the workspace) are in docs/guide.md, "Advanced flags".

Sign-in: ANTHROPIC_API_KEY when set, else a token file from ` + "`claude setup-token`" + ` (AGENTIUM_CLAUDE_TOKEN_FILE or
~/.config/agentium/claude-oauth-token), else your own login with project settings only.
`

func runRun(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, runUsage)
		return ExitUsage
	}
	switch args[0] {
	case "once":
		return runOnce(ctx, env, args[1:])
	case "calibrate":
		return runCalibrate(ctx, env, args[1:])
	case "list":
		return runList(ctx, env, args[1:])
	case "show":
		return runShow(ctx, env, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(env.Stdout, runUsage)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "agentium run: unknown subcommand %q\n\n%s", args[0], runUsage)
	return ExitUsage
}

// signIn picks how runs sign in: an API key, a token file, or the user's own login. Secrets are only passed on.
func signIn(env Env) (mode, secret, tokenFile string, err error) {
	switch mode, tokenFile = signInMode(env); mode {
	case claude.SignInAPIKey:
		return mode, env.Getenv("ANTHROPIC_API_KEY"), "", nil
	case claude.SignInTokenFile:
		token, err := claude.ReadToken(tokenFile)
		return mode, token, tokenFile, err
	}
	return mode, "", "", nil
}

// signInMode is signIn's choice, from the credentials' presence only.
func signInMode(env Env) (mode, tokenFile string) {
	if env.Getenv("ANTHROPIC_API_KEY") != "" {
		return claude.SignInAPIKey, ""
	}
	if file := project.TokenFile(project.Env{Getenv: env.Getenv}); file != "" {
		if _, err := os.Stat(file); err == nil {
			return claude.SignInTokenFile, file
		}
	}
	return claude.SignInLogin, ""
}

// runHidden are run once's expert flags: they parse, but runUsage leaves them out (docs/guide.md, "Advanced flags").
var runHidden = []string{"timeout", "verify-timeout", "keep", "grader"}

// runOnceRemoved are run once's removed flags, each with what replaces it.
var runOnceRemoved = map[string]string{"effort": "put the effort in --model: --model MODEL:EFFORT"}

func runOnce(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("run once", flag.ContinueOnError)
	snapshotName := fs.String("snapshot", "", "apply this context snapshot (default: the base's own context)")
	profile := fs.String("model", experiment.DefaultExperimentModel, "the model, MODEL[:EFFORT] (effort default: the CLI's)")
	budget := fs.Float64("budget", 3, "stop the run at this cost in USD")
	// Hidden (runHidden): the guide's "Advanced flags".
	timeout := fs.Duration("timeout", 20*time.Minute, "stop the run after this long")
	verifyTimeout := fs.Duration("verify-timeout", 10*time.Minute, "time limit for each setup or verification command")
	keep := fs.Bool("keep", false, "keep the workspace and the verification copy")
	grader := addGraderFlag(fs)
	removeFlags(fs, runOnceRemoved)
	rest, code, ok := parseArgs(env, fs, args, runUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 || *budget <= 0 || *timeout <= 0 {
		fmt.Fprint(env.Stderr, runUsage)
		return ExitUsage
	}
	model, effort, mode, err := onceProfile(env, *profile, grader)
	if err != nil {
		fmt.Fprintf(env.Stderr, "agentium run once: %v\n", err)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	t, err := w.db.TaskByName(ctx, w.project.ID, rest[0])
	if err != nil {
		return fail(env, err)
	}
	if t.Grading == task.GradingJudge { // its verification commands would grade it as if they were its tests
		return fail(env, fmt.Errorf("task %s is judge-graded (no hidden tests); run once takes judge-graded tasks in a later version", t.Name))
	}
	arm := task.Arm{Name: "base"}
	if *snapshotName != "" {
		snap, err := w.db.SnapshotByName(ctx, w.project.ID, *snapshotName)
		if err != nil {
			return fail(env, err)
		}
		arm = task.Arm{Name: *snapshotName, Snapshot: snap.CommitID}
	}
	env, live := liveEnv(env)
	defer live.Stop()
	runEnv, err := newRunEnv(env, w, *verifyTimeout)
	if err != nil {
		return fail(env, err)
	}
	runEnv.Step, runEnv.Grader = live.Step, mode
	switch cal, err := checkAgainstCalibration(ctx, env, w, arm, model); {
	case err != nil:
		return fail(env, err)
	case cal != nil:
		runEnv.Expect = *cal
	}
	fmt.Fprintf(env.Stdout, "Starting a real Claude Code run (%s, sign-in %s, graded %s): it may cost up to $%.2f.\n", *profile, runEnv.SignIn, task.DescribeGrader(mode), *budget)
	release, err := startRuns(ctx, env, w)
	if err != nil {
		return fail(env, err)
	}
	defer release()
	rec, err := executeRun(ctx, env, w, runEnv, runMeta{TaskID: t.ID, Kind: "task"}, run.Spec{TaskName: t.Name, Instruction: t.Instruction,
		Task: task.Spec{Base: t.BaseCommit, Solution: t.SolutionCommit, HiddenTests: t.HiddenTests, Reference: t.Reference, Setup: t.Setup, Verify: t.Verify},
		Arm:  arm, Model: model, Effort: effort, BudgetUSD: *budget, Timeout: *timeout, Keep: *keep, HarmlessDenials: harmlessFor(t, mode)})
	live.Stop()
	if err != nil {
		return fail(env, err)
	}
	if env.JSON {
		return env.emit(runOnceDoc{header: hdr("run once"), Run: runInfoOf(env, rec)})
	}
	printRun(env, rec)
	return ExitOK
}

// harmlessFor is the flagged denials t's reference logged while passing its last validation, when that ran in mode.
func harmlessFor(t store.Task, mode string) []task.DenialKey {
	if v := task.ValidationOf(t); task.GraderOf(v.Grader) == task.GraderOf(mode) {
		return v.Harmless
	}
	return nil
}

// onceProfile reads run once's --model and --grader.
func onceProfile(env Env, profile string, grader graderFlag) (model, effort, mode string, err error) {
	if model, effort, err = experiment.ParseProfile(profile); err != nil {
		return "", "", "", fmt.Errorf("--model %w", err)
	}
	mode, err = grader.mode(env)
	return model, effort, mode, err
}

// liveEnv returns env with its output printing above a live status line where Stdout is a terminal (the status line
// is a pass-through elsewhere, so the returned env behaves as before). The caller defers Stop on the line, which
// leaves the terminal clean; errors written to Stderr go above the line too.
func liveEnv(env Env) (Env, *term.StatusLine) {
	live := term.NewStatusLine(env.Stdout, term.StatusOptions{Terminal: env.Terminal, Getenv: env.Getenv, Style: env.style(),
		Columns: env.Columns, Now: env.Now})
	if live.Live() {
		env.Stdout, env.Stderr = live, live.Over(env.Stderr)
	}
	return env, live
}

// newRunEnv resolves what every run of this project needs: the CLI, the sign-in and the environment.
func newRunEnv(env Env, w *workspace, verifyTimeout time.Duration) (run.Env, error) {
	cli, err := claudePath(env)
	if err != nil {
		return run.Env{}, err
	}
	mode, secret, tokenFile, err := signIn(env)
	if err != nil {
		return run.Env{}, err
	}
	var environ []string
	if env.Environ != nil {
		environ = env.Environ()
	}
	buildEnv, err := run.BuildEnv(w.layout)
	if err != nil {
		return run.Env{}, err
	}
	accountHome := ""
	if env.AccountHome != nil {
		accountHome = env.AccountHome()
	}
	return run.Env{Layout: w.layout, Bare: w.bare, ProjectRoot: w.root, CLI: cli, Home: env.Getenv("HOME"), AccountHome: accountHome,
		Environ: environ, SignIn: mode, Secret: secret, TokenFile: tokenFile, VerifyTimeout: verifyTimeout, Grace: 30 * time.Second,
		CommandEnv: buildEnv, AllowLocalBinding: w.project.AllowLocalBinding,
		Progress: env.Stdout, Style: env.style(), Now: env.Now}, nil
}

// claudePath finds Claude Code: AGENTIUM_CLAUDE, else PATH.
func claudePath(env Env) (string, error) {
	if cli := env.Getenv("AGENTIUM_CLAUDE"); cli != "" {
		return cli, nil
	}
	cli, err := env.LookPath("claude")
	if err != nil {
		return "", errors.New("Claude Code was not found on PATH: install it, or set AGENTIUM_CLAUDE to its path")
	}
	return cli, nil
}

// runMeta places a run: the project, the task, the kind, and for experiments the slot and attempt. It is stored with
// the run and kept in its start file, so a run whose Agentium process died is stored where it belongs (startRuns).
type runMeta struct {
	ProjectID    int64  `json:"project_id"`
	TaskID       int64  `json:"task_id,omitempty"`
	Kind         string `json:"kind"` // "task" or "calibration"
	ExperimentID int64  `json:"experiment_id,omitempty"`
	Slot         int    `json:"slot,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
}

// startRuns takes the data folder's run lock, which commands hold while they start agents, and stores the runs a dead
// Agentium process left behind (cancelled, with what their transcripts show they spent). Call release when done.
func startRuns(ctx context.Context, env Env, w *workspace) (release func(), err error) {
	release, err = w.layout.LockRuns()
	if err != nil {
		return nil, err
	}
	_, secret, _, err := signIn(env)
	if err != nil {
		release()
		return nil, err
	}
	// What a dead run's grade left that cannot be cleaned up is a warning, never a reason to stop (run.RecoverWarn).
	// These notices print at once, even under the dashboard: if Agentium is killed before the run ends, they are not lost.
	out := env.noticeOut()
	warn := func(msg string) { fmt.Fprintf(out, "%s\n", env.style().Warn("warning: "+msg)) }
	orphans, recoverErr := run.RecoverWarn(ctx, w.layout, func(id string) (bool, error) { return w.db.HasRun(ctx, id) }, secret, env.Now(), warn)
	for _, o := range orphans {
		if o.Unreadable != "" { // task, arm and slot unknown: reported, not stored
			fmt.Fprintf(out, "Run %s left behind by a stopped Agentium has an unreadable start file, so it is not stored. "+
				"Its transcript shows $%.2f spent (judge spend, if any, is not included), which neither experiment budgets nor `agentium experiment show` count; the file was moved to %s\n",
				o.Record.ID, o.Record.Spend().AgentUSD, o.Unreadable)
			continue
		}
		meta := runMeta{ProjectID: w.project.ID, Kind: "task"}
		if len(o.Meta) > 0 {
			if err := json.Unmarshal(o.Meta, &meta); err != nil {
				release()
				return nil, fmt.Errorf("run %s: %w", o.Record.ID, err)
			}
		}
		if err := saveRun(ctx, w, o.Record, meta); err != nil {
			release()
			return nil, err
		}
		fmt.Fprintf(out, "Recovered run %s (task %s, arm %s), left behind by a stopped Agentium: %s, $%.2f\n",
			o.Record.ID, o.Record.Task, o.Record.Arm, env.style().Status("cancelled"), o.Record.Spend().AgentUSD)
	}
	if recoverErr != nil {
		release()
		return nil, recoverErr
	}
	return release, nil
}

// saveRun stores a run's record. An interrupted run is saved all the same: ctx is cancelled by then, and its spend
// must not be lost.
func saveRun(ctx context.Context, w *workspace, rec run.Record, meta runMeta) error {
	if meta.Kind == "calibration" {
		rec.Passed = nil
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode run: %w", err)
	}
	return w.db.SaveRun(context.WithoutCancel(ctx), store.Run{ID: rec.ID, ProjectID: meta.ProjectID, TaskID: meta.TaskID, TaskName: rec.Task,
		Kind: meta.Kind, Arm: rec.Arm, Outcome: rec.Outcome, Passed: rec.Passed, CostUSD: rec.Spend().AgentUSD, Record: encoded,
		Started: rec.Started, Finished: rec.Finished, ExperimentID: meta.ExperimentID, Slot: meta.Slot, Attempt: meta.Attempt})
}

// executeRun runs spec with a fresh id and stores the record whenever the agent started, even when interrupted. A
// calibration run is stored with kind "calibration" and no pass or fail: it has no task to grade. The caller holds
// the run lock (startRuns).
func executeRun(ctx context.Context, env Env, w *workspace, runEnv run.Env, meta runMeta, spec run.Spec) (run.Record, error) {
	id, err := run.NewID(env.Now())
	if err != nil {
		return run.Record{}, err
	}
	runEnv.ID = id
	if meta.ProjectID == 0 {
		meta.ProjectID = w.project.ID
	}
	if runEnv.Meta, err = json.Marshal(meta); err != nil {
		return run.Record{}, fmt.Errorf("encode run: %w", err)
	}
	rec, runErr := run.Once(ctx, runEnv, spec)
	if _, err := os.Stat(filepath.Join(rec.RecordsDir, "stream.jsonl")); rec.Outcome == "" && err != nil {
		os.RemoveAll(rec.RecordsDir) // the agent never started: nothing to keep
		return rec, runErr
	}
	if meta.Kind == "calibration" {
		rec.Passed = nil
	}
	if err := saveRun(ctx, w, rec, meta); err != nil {
		return rec, errors.Join(runErr, err)
	}
	return rec, runErr
}

func printRun(env Env, rec run.Record) {
	out, st := env.Stdout, env.style()
	passed := "not run"
	if rec.Passed != nil {
		passed = st.Status(map[bool]string{true: "passed", false: "failed"}[*rec.Passed])
	}
	fmt.Fprintln(out, st.Heading(fmt.Sprintf("Run %s: task %s, arm %s", rec.ID, rec.Task, rec.Arm)))
	fmt.Fprintf(out, "  outcome      %s; verification %s\n", st.Status(rec.Outcome), passed)
	m, b := rec.Metrics, rec.Behavior
	fmt.Fprintf(out, "  cost         $%.4f, %d turn(s), %s, first request %d tokens\n", rec.Spend().AgentUSD, m.Turns,
		(time.Duration(m.DurationMS) * time.Millisecond).Round(time.Second), m.FirstRequest)
	fmt.Fprintf(out, "  changes      %d file(s), +%d -%d, %d commit(s); tests changed: %v, test files removed: %d\n", b.FilesChanged, b.LinesAdded,
		b.LinesRemoved, b.Commits, b.TestsChanged, b.TestsRemoved)
	if len(b.ChecksChanged) > 0 {
		fmt.Fprintf(out, "  checks       the agent changed %s\n", strings.Join(b.ChecksChanged, ", "))
	}
	fmt.Fprintf(out, "  behavior     ran tests: %v, ran the checks: %v, %d Bash command(s), %d denial(s)\n", b.RanTests, b.RanChecks, b.BashCommands, b.Denials)
	fmt.Fprintf(out, "  environment  Claude Code %s, %s, permission mode %s, %d tool(s), %d skill(s)\n", term.OrNone(m.CLIVersion), term.OrNone(m.Model),
		term.OrNone(m.PermissionMode), len(m.Tools), m.SkillCount)
	if len(m.SubagentModels) > 0 {
		var kinds []string
		for kind, models := range m.SubagentModels {
			kinds = append(kinds, kind+" on "+strings.Join(models, ", "))
		}
		sort.Strings(kinds)
		fmt.Fprintf(out, "  subagents    %s\n", strings.Join(kinds, "; "))
	}
	if m.UsageFirst != nil && m.UsageLast != nil {
		fmt.Fprintf(out, "  usage        five-hour window %.0f%% → %.0f%%, seven-day %.0f%%\n", 100*m.UsageFirst.FiveHour, 100*m.UsageLast.FiveHour, 100*m.UsageLast.SevenDay)
	}
	if line := gradingLine(rec); line != "" {
		fmt.Fprintf(out, "  grading      %s\n", line)
	}
	for _, d := range rec.Drift {
		fmt.Fprintf(out, "%s %s\n", st.Warn("unfair:"), d)
	}
	for _, n := range rec.Notes {
		fmt.Fprintln(out, note(st, n))
	}
	fmt.Fprintf(out, "  records      %s\n", rec.RecordsDir)
}

// gradingLine says where a run was graded and what the sandbox reported; empty for a record made before grader modes.
func gradingLine(rec run.Record) string {
	if rec.Grader == "" {
		return ""
	}
	line := task.DescribeGrader(rec.Grader)
	if g := rec.Sandbox; g != nil {
		switch {
		case g.Canary != task.CanaryPassed:
			line += ": the canary failed, nothing was graded"
		case g.Unread != "":
			line += ", canary passed, denials unread"
		default:
			line += fmt.Sprintf(", canary passed, %d denial(s), %d the agent's sandbox does not impose", g.DenialCount, g.FlaggedCount)
			if g.FlaggedCount > 0 {
				line += " (" + g.FlaggedOperations() + ")"
			}
		}
	}
	return line
}

func runList(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("run list", flag.ContinueOnError), args, runUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 {
		fmt.Fprint(env.Stderr, runUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	runs, err := w.db.Runs(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	if env.JSON {
		doc := runListDoc{header: hdr("run list"), Runs: []runListEntry{}}
		for _, r := range runs {
			entry := runListEntry{ID: r.ID, Task: r.TaskName, Kind: r.Kind, Arm: r.Arm, Outcome: r.Outcome, Passed: r.Passed, CostUSD: r.CostUSD, Started: r.Started}
			if entry.Kind == "" {
				entry.Kind = "task"
			}
			if r.Kind == "calibration" {
				entry.Task = ""
			}
			doc.Runs = append(doc.Runs, entry)
		}
		return env.emit(doc)
	}
	if len(runs) == 0 {
		fmt.Fprintln(env.Stdout, "No runs yet: "+env.style().Command("agentium run once TASK"))
		return ExitOK
	}
	st := env.style()
	table := term.NewTable(st, term.Left("ID"), term.Left("TASK"), term.Left("ARM"), term.Left("OUTCOME"), term.Left("PASSED"), term.Right("COST"))
	for _, r := range runs {
		passed := "-"
		if r.Passed != nil {
			passed = map[bool]string{true: "yes", false: "no"}[*r.Passed]
		}
		name := r.TaskName
		if r.Kind == "calibration" {
			name = st.Note("(calibration)")
		}
		table.Row(r.ID, name, r.Arm, st.Status(r.Outcome), st.Status(passed), fmt.Sprintf("$%.2f", r.CostUSD))
	}
	if err := table.Write(env.Stdout); err != nil {
		return fail(env, err)
	}
	return ExitOK
}

func runShow(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("run show", flag.ContinueOnError)
	diff := fs.Bool("diff", false, "print the agent's change (against the context commit)")
	logs := fs.Bool("log", false, "print the setup and verification output")
	rest, code, ok := parseArgs(env, fs, args, runUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		fmt.Fprint(env.Stderr, runUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	stored, err := w.db.RunByID(ctx, w.project.ID, rest[0])
	if err != nil {
		return fail(env, err)
	}
	var rec run.Record
	if err := json.Unmarshal(stored.Record, &rec); err != nil {
		return fail(env, fmt.Errorf("run %s: %w", stored.ID, err))
	}
	if env.JSON {
		return env.emit(runShowDocument(ctx, env, w, stored, rec, *diff, *logs))
	}
	printRun(env, rec)
	if stored.ExperimentID != 0 {
		name := fmt.Sprintf("#%d", stored.ExperimentID)
		if all, err := w.db.Experiments(ctx, w.project.ID); err == nil {
			for _, e := range all {
				if e.ID == stored.ExperimentID {
					name = e.Name
				}
			}
		}
		if stored.Kind == "calibration" {
			fmt.Fprintf(env.Stdout, "  experiment   %s (a calibration before its first pair)\n", name)
		} else {
			fmt.Fprintf(env.Stdout, "  experiment   %s, slot %d (from 0), attempt %d\n", name, stored.Slot, stored.Attempt)
		}
	}
	if entries, err := os.ReadDir(rec.RecordsDir); err == nil {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		fmt.Fprintf(env.Stdout, "  files        %s\n", strings.Join(names, ", "))
	}
	var shown []string
	if *diff {
		shown = append(shown, "agent.diff")
	}
	if *logs {
		shown = append(shown, "setup.log", "verify.log")
	}
	for _, name := range shown {
		data, err := os.ReadFile(filepath.Join(rec.RecordsDir, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(env.Stdout, "\n%s: none (%s)\n", env.style().Heading("--- "+name), map[string]string{"agent.diff": "the run was not graded",
				"setup.log": "the task has no setup", "verify.log": "the verification did not run"}[name])
		case err != nil:
			return fail(env, fmt.Errorf("run %s: %w", stored.ID, err))
		default:
			fmt.Fprintf(env.Stdout, "\n%s\n%s", env.style().Heading("--- "+name), data)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				fmt.Fprintln(env.Stdout)
			}
		}
	}
	return ExitOK
}

func runCalibrate(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("run calibrate", flag.ContinueOnError)
	var snapshots stringList
	fs.Var(&snapshots, "snapshot", "also calibrate this snapshot's context (repeatable)")
	profile := fs.String("model", experiment.DefaultExperimentModel, "the model, MODEL[:EFFORT]")
	budget := fs.Float64("budget", 0.5, "stop each calibration run at this cost in USD")
	timeout := fs.Duration("timeout", 5*time.Minute, "stop each calibration run after this long")
	rest, code, ok := parseArgs(env, fs, args, runUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 || *budget <= 0 || *timeout <= 0 {
		fmt.Fprint(env.Stderr, runUsage)
		return ExitUsage
	}
	// A calibration is of a context on a model (experiment.Project.CalibrationOn): runs at any effort are checked
	// against their model's, so an effort is accepted, as --model takes it everywhere, and not used.
	model, _, err := experiment.ParseProfile(*profile)
	if err != nil {
		fmt.Fprintf(env.Stderr, "agentium run calibrate: --model %v\n", err)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	head, snaps, err := calibrationTargets(ctx, w, snapshots)
	var bad experiment.UsageError
	if errors.As(err, &bad) {
		fmt.Fprintf(env.Stderr, "agentium run calibrate: %s\n", bad)
		return ExitUsage
	} else if err != nil {
		return fail(env, err)
	}
	arms, err := run.ArmSources(ctx, w.bare, head, snaps)
	if err != nil {
		return fail(env, err)
	}
	env, live := liveEnv(env)
	defer live.Stop()
	runEnv, err := newRunEnv(env, w, time.Minute)
	if err != nil {
		return fail(env, err)
	}
	release, err := startRuns(ctx, env, w)
	if err != nil {
		return fail(env, err)
	}
	defer release()
	fmt.Fprintf(env.Stdout, "Calibrating %d arm(s) at %s with real Claude Code runs (%s, sign-in %s): up to $%.2f each.\n",
		len(arms), experiment.ShortCommit(head), model, runEnv.SignIn, *budget)
	results, err := run.Calibrator{Head: head, Arms: arms, Model: model, Budget: *budget, Timeout: *timeout, SignIn: runEnv.SignIn, Now: env.Now,
		Execute: func(ctx context.Context, arm task.Arm, spec run.Spec) (run.Record, error) {
			runEnv.Step = func(step string) { live.Step("calibrating arm " + arm.Name + ": " + step) }
			return executeRun(ctx, env, w, runEnv, runMeta{Kind: "calibration"}, spec)
		},
		Save: func(ctx context.Context, c run.Calibration) error { return saveCalibration(ctx, env, w, c) },
	}.Run(ctx)
	if err != nil {
		return fail(env, err)
	}
	live.Stop()
	if err := run.WriteCalibrations(env.Stdout, env.style(), results); err != nil {
		return fail(env, err)
	}
	if !run.AllHealthy(results) {
		fmt.Fprintln(env.Stdout, env.style().Bad("Not every arm passed: failed arms were not saved as calibrations (see the run records)."))
		return ExitError
	}
	return ExitOK
}

// calibrationTargets resolves what a calibration covers: the commit HEAD names, kept in Agentium's repository, and the
// named snapshots. A name that is repeated, or "base", is a usage error.
func calibrationTargets(ctx context.Context, w *workspace, names []string) (head string, snaps []task.Arm, err error) {
	if head, err = w.keepCommit(ctx, "HEAD"); err != nil {
		return "", nil, err
	}
	for i, name := range names {
		if name == "base" || slices.Contains(names[:i], name) {
			return "", nil, experiment.UsageError(fmt.Sprintf("--snapshot %q is repeated or reserved", name))
		}
		snap, err := w.db.SnapshotByName(ctx, w.project.ID, name)
		if err != nil {
			return "", nil, err
		}
		snaps = append(snaps, task.Arm{Name: name, Snapshot: snap.CommitID})
	}
	return head, snaps, nil
}

// saveCalibration stores a healthy calibration as what later runs of its arm are checked against.
func saveCalibration(ctx context.Context, env Env, w *workspace, c run.Calibration) error {
	encoded, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode calibration: %w", err)
	}
	return w.db.SaveCalibration(ctx, store.Calibration{ProjectID: w.project.ID, Arm: c.Arm, Snapshot: c.Snapshot, RunID: c.RunID,
		Result: encoded, CreatedAt: env.Now()})
}

// checkAgainstCalibration finds the calibration a run of arm on model is checked against: the newest of the arm's
// context on that very model (tools and skills can differ by model, as experiments check), and says so. Without one it
// falls back to the newest on any model, with a note that the model differs, and without any it says the run's tools
// and skills are not checked. It returns nil when there is nothing to check against.
func checkAgainstCalibration(ctx context.Context, env Env, w *workspace, arm task.Arm, model string) (*claude.Expect, error) {
	stored, err := w.service().CalibrationOn(ctx, arm.Name, arm.Snapshot, model)
	if errors.Is(err, store.ErrNotFound) {
		stored, err = w.db.LatestCalibration(ctx, w.project.ID, arm.Name, arm.Snapshot)
	}
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintln(env.Stdout, note(env.style(), fmt.Sprintf("arm %s is not calibrated, so its tools and skills are not checked: agentium run calibrate --model %s", arm.Name, model)))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found run.Calibration
	if err := json.Unmarshal(stored.Result, &found); err != nil {
		return nil, fmt.Errorf("calibration of %s: %w", arm.Name, err)
	}
	fmt.Fprintf(env.Stdout, "Checking the environment against the calibration of %s (%s).\n", arm.Name, stored.CreatedAt.Format("2006-01-02 15:04"))
	if found.RequestedModel != model {
		fmt.Fprintln(env.Stdout, note(env.style(), fmt.Sprintf("the calibration used %s, this run %s: its tool set may differ by model", found.RequestedModel, model)))
	}
	return &claude.Expect{CLIVersion: found.CLIVersion, Tools: found.Tools, Skills: found.Skills, SlashCommands: found.SlashCommands}, nil
}
