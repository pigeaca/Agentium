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
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

const runUsage = `Usage:
  agentium run once TASK [--snapshot NAME] [--model MODEL] [--effort LEVEL] [--budget USD] [--timeout DURATION]
                     [--verify-timeout DURATION] [--keep]
                     one real Claude Code run on TASK, in the base's own context or with a snapshot applied.
                     It costs money (up to --budget, default $3) or uses your plan.
  agentium run calibrate [--snapshot NAME]... [--model MODEL] [--budget USD]
                     one short real run per arm (the base's own context, and each snapshot): checks that
                     sandboxed commands work and large outputs read back, compares the first request's size with
                     Agentium's estimate, and records the tools, skills and slash commands later runs must get
  agentium run list
  agentium run show ID

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
	if key := env.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return claude.SignInAPIKey, key, "", nil
	}
	if file := project.TokenFile(project.Env{Getenv: env.Getenv}); file != "" {
		if _, statErr := os.Stat(file); statErr == nil {
			token, err := claude.ReadToken(file)
			return claude.SignInTokenFile, token, file, err
		}
	}
	return claude.SignInLogin, "", "", nil
}

func runOnce(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("run once", flag.ContinueOnError)
	snapshotName := fs.String("snapshot", "", "apply this context snapshot (default: the base's own context)")
	model := fs.String("model", "claude-sonnet-5", "the model")
	effort := fs.String("effort", "", "the effort level (default: the CLI's)")
	budget := fs.Float64("budget", 3, "stop the run at this cost in USD")
	timeout := fs.Duration("timeout", 20*time.Minute, "stop the run after this long")
	verifyTimeout := fs.Duration("verify-timeout", 10*time.Minute, "time limit for each setup or verification command")
	keep := fs.Bool("keep", false, "keep the workspace and the verification copy")
	rest, code, ok := parseArgs(env, fs, args, runUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 || *budget <= 0 || *timeout <= 0 {
		fmt.Fprint(env.Stderr, runUsage)
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
	arm := task.Arm{Name: "base"}
	if *snapshotName != "" {
		snap, err := w.db.SnapshotByName(ctx, w.project.ID, *snapshotName)
		if err != nil {
			return fail(env, err)
		}
		arm = task.Arm{Name: *snapshotName, Snapshot: snap.CommitID}
	}
	runEnv, err := newRunEnv(env, w, *verifyTimeout)
	if err != nil {
		return fail(env, err)
	}
	if cal, err := w.db.LatestCalibration(ctx, w.project.ID, arm.Name, arm.Snapshot); err == nil {
		var found calibration
		if err := json.Unmarshal(cal.Result, &found); err != nil {
			return fail(env, fmt.Errorf("calibration of %s: %w", arm.Name, err))
		}
		runEnv.Expect = claude.Expect{CLIVersion: found.CLIVersion, Tools: found.Tools, Skills: found.Skills, SlashCommands: found.SlashCommands}
		fmt.Fprintf(env.Stdout, "Checking the environment against the calibration of %s (%s).\n", arm.Name, cal.CreatedAt.Format("2006-01-02 15:04"))
	} else if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(env.Stdout, "note: arm %s is not calibrated, so its tools and skills are not checked: agentium run calibrate\n", arm.Name)
	} else {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Starting a real Claude Code run (%s, sign-in %s): it may cost up to $%.2f.\n", *model, runEnv.SignIn, *budget)
	rec, err := executeRun(ctx, env, w, runEnv, t.ID, run.Spec{TaskName: t.Name, Instruction: t.Instruction,
		Task: task.Spec{Base: t.BaseCommit, Solution: t.SolutionCommit, HiddenTests: t.HiddenTests, Reference: t.Reference, Setup: t.Setup, Verify: t.Verify},
		Arm:  arm, Model: *model, Effort: *effort, BudgetUSD: *budget, Timeout: *timeout, Keep: *keep})
	if err != nil {
		return fail(env, err)
	}
	printRun(env, rec)
	return ExitOK
}

// newRunEnv resolves what every run of this project needs: the CLI, the sign-in and the environment.
func newRunEnv(env Env, w *workspace, verifyTimeout time.Duration) (run.Env, error) {
	cli := env.Getenv("AGENTIUM_CLAUDE")
	if cli == "" {
		var err error
		if cli, err = env.LookPath("claude"); err != nil {
			return run.Env{}, errors.New("Claude Code was not found on PATH: install it, or set AGENTIUM_CLAUDE to its path")
		}
	}
	mode, secret, tokenFile, err := signIn(env)
	if err != nil {
		return run.Env{}, err
	}
	var environ []string
	if env.Environ != nil {
		environ = env.Environ()
	}
	return run.Env{Layout: w.layout, Bare: w.bare, ProjectRoot: w.root, CLI: cli, Home: env.Getenv("HOME"), Environ: environ,
		SignIn: mode, Secret: secret, TokenFile: tokenFile, VerifyTimeout: verifyTimeout, Grace: 30 * time.Second,
		Progress: env.Stdout, Now: env.Now}, nil
}

// executeRun runs spec with a fresh id and stores the record whenever the agent started, even when interrupted.
func executeRun(ctx context.Context, env Env, w *workspace, runEnv run.Env, taskID int64, spec run.Spec) (run.Record, error) {
	id, err := run.NewID(env.Now())
	if err != nil {
		return run.Record{}, err
	}
	runEnv.ID = id
	rec, runErr := run.Once(ctx, runEnv, spec)
	if _, err := os.Stat(filepath.Join(rec.RecordsDir, "stream.jsonl")); rec.Outcome == "" && err != nil {
		os.RemoveAll(rec.RecordsDir) // the agent never started: nothing to keep
		return rec, runErr
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return rec, errors.Join(runErr, fmt.Errorf("encode run: %w", err))
	}
	// An interrupted run is saved all the same: ctx is cancelled by then, and its spend must not be lost.
	if err := w.db.SaveRun(context.WithoutCancel(ctx), store.Run{ID: rec.ID, ProjectID: w.project.ID, TaskID: taskID, TaskName: spec.TaskName,
		Arm: rec.Arm, Outcome: rec.Outcome, Passed: rec.Passed, CostUSD: rec.Metrics.CostUSD, Record: encoded, Started: rec.Started,
		Finished: rec.Finished}); err != nil {
		return rec, errors.Join(runErr, err)
	}
	return rec, runErr
}

func printRun(env Env, rec run.Record) {
	out := env.Stdout
	passed := "not run"
	if rec.Passed != nil {
		passed = map[bool]string{true: "passed", false: "failed"}[*rec.Passed]
	}
	fmt.Fprintf(out, "Run %s: task %s, arm %s\n", rec.ID, rec.Task, rec.Arm)
	fmt.Fprintf(out, "  outcome      %s; verification %s\n", rec.Outcome, passed)
	m, b := rec.Metrics, rec.Behavior
	fmt.Fprintf(out, "  cost         $%.4f, %d turn(s), %s, first request %d tokens\n", m.CostUSD, m.Turns,
		(time.Duration(m.DurationMS) * time.Millisecond).Round(time.Second), m.FirstRequest)
	fmt.Fprintf(out, "  changes      %d file(s), +%d -%d, %d commit(s); tests changed: %v, test files removed: %d\n", b.FilesChanged, b.LinesAdded,
		b.LinesRemoved, b.Commits, b.TestsChanged, b.TestsRemoved)
	if len(b.ChecksChanged) > 0 {
		fmt.Fprintf(out, "  checks       the agent changed %s\n", strings.Join(b.ChecksChanged, ", "))
	}
	fmt.Fprintf(out, "  behavior     ran tests: %v, ran the checks: %v, %d Bash command(s), %d denial(s)\n", b.RanTests, b.RanChecks, b.BashCommands, b.Denials)
	fmt.Fprintf(out, "  environment  Claude Code %s, %s, permission mode %s, %d tool(s), %d skill(s)\n", orNone(m.CLIVersion), orNone(m.Model),
		orNone(m.PermissionMode), len(m.Tools), m.SkillCount)
	for _, d := range rec.Drift {
		fmt.Fprintf(out, "unfair: %s\n", d)
	}
	for _, n := range rec.Notes {
		fmt.Fprintf(out, "note: %s\n", n)
	}
	fmt.Fprintf(out, "  records      %s\n", rec.RecordsDir)
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
	if len(runs) == 0 {
		fmt.Fprintln(env.Stdout, "No runs yet: agentium run once TASK")
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "%-24s %-40s %-14s %-8s %-8s %8s\n", "ID", "TASK", "ARM", "OUTCOME", "PASSED", "COST")
	for _, r := range runs {
		passed := "-"
		if r.Passed != nil {
			passed = map[bool]string{true: "yes", false: "no"}[*r.Passed]
		}
		fmt.Fprintf(env.Stdout, "%-24s %-40s %-14s %-8s %-8s %8s\n", r.ID, r.TaskName, r.Arm, r.Outcome, passed, fmt.Sprintf("$%.2f", r.CostUSD))
	}
	return ExitOK
}

func runShow(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("run show", flag.ContinueOnError), args, runUsage)
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
	printRun(env, rec)
	if entries, err := os.ReadDir(rec.RecordsDir); err == nil {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		fmt.Fprintf(env.Stdout, "  files        %s\n", strings.Join(names, ", "))
	}
	return ExitOK
}

// calibrationPrompt asks for what every real task needs (a sandboxed shell command, a large output read back) and for
// the codeword Agentium added to the arm's instruction file, which proves that file loads.
const calibrationPrompt = "This is an environment check: do not change any files and do not search the repository.\n" +
	"1. Run this command with the Bash tool: printf 'agentium-sandbox-ok\\n'\n" +
	"2. Run exactly this command with the Bash tool, without redirecting its output: seq 1 40000\n" +
	"   Its output is too large to show in full: read the full output that Claude Code saved for you, and find its 20000th line.\n" +
	"3. Your project instructions, as loaded at the start, end with a calibration codeword. If you see none, it is NONE.\n" +
	"Then reply with exactly one line: SANDBOX=<what command 1 printed> LINE=<the 20000th line of command 2's output> CODEWORD=<the codeword>"

// calibration is what a calibration run found for one arm.
type calibration struct {
	Arm              string   `json:"arm"`
	Snapshot         string   `json:"snapshot,omitempty"`
	RunID            string   `json:"run_id"`
	Outcome          string   `json:"outcome"`
	SandboxOK        bool     `json:"sandbox_ok"`
	LargeOutputOK    bool     `json:"large_output_ok"`
	InstructionsOK   bool     `json:"instructions_ok"` // the agent repeated the codeword added to the arm's instruction file
	FirstRequest     int64    `json:"first_request_tokens"`
	EstimatedContext int      `json:"estimated_context_tokens"` // the resolver's session-start estimate
	CLIVersion       string   `json:"cli_version"`
	Model            string   `json:"model"`
	Tools            []string `json:"tools"`
	Skills           []string `json:"skills"`         // stored locally to check later runs; never printed
	SlashCommands    []string `json:"slash_commands"` // likewise
	Drift            []string `json:"drift,omitempty"`
	CostUSD          float64  `json:"cost_usd"`
}

func runCalibrate(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("run calibrate", flag.ContinueOnError)
	var snapshots stringList
	fs.Var(&snapshots, "snapshot", "also calibrate this snapshot's context (repeatable)")
	model := fs.String("model", "claude-sonnet-5", "the model")
	budget := fs.Float64("budget", 0.5, "stop each calibration run at this cost in USD")
	timeout := fs.Duration("timeout", 5*time.Minute, "stop each calibration run after this long")
	rest, code, ok := parseArgs(env, fs, args, runUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 || *budget <= 0 {
		fmt.Fprint(env.Stderr, runUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	head, err := w.keepCommit(ctx, "HEAD")
	if err != nil {
		return fail(env, err)
	}
	baseSource, err := source.Commit(ctx, head, "--git-dir", w.bare)
	if err != nil {
		return fail(env, err)
	}
	type armContext struct {
		arm    task.Arm
		source source.Source
	}
	arms := []armContext{{task.Arm{Name: "base"}, baseSource}}
	for i, name := range snapshots {
		if name == "base" || slices.Contains(snapshots[:i], name) {
			fmt.Fprintf(env.Stderr, "agentium run calibrate: --snapshot %q is repeated or reserved\n", name)
			return ExitUsage
		}
		snap, err := w.db.SnapshotByName(ctx, w.project.ID, name)
		if err != nil {
			return fail(env, err)
		}
		src, err := source.Commit(ctx, snap.CommitID, "--git-dir", w.bare)
		if err != nil {
			return fail(env, err)
		}
		arms = append(arms, armContext{task.Arm{Name: name, Snapshot: snap.CommitID}, src})
	}
	runEnv, err := newRunEnv(env, w, time.Minute)
	if err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Calibrating %d arm(s) at %s with real Claude Code runs (%s, sign-in %s): up to $%.2f each.\n",
		len(arms), shortCommit(head), *model, runEnv.SignIn, *budget)
	var results []calibration
	healthy := true
	for _, a := range arms {
		resolved, err := claudectx.Resolve(a.source)
		if err != nil {
			return fail(env, err)
		}
		codeword, err := run.NewID(env.Now()) // random enough to be unguessable
		if err != nil {
			return fail(env, err)
		}
		codeword = "AGENTIUM-" + strings.ToUpper(codeword[len(codeword)-6:])
		rec, err := executeRun(ctx, env, w, runEnv, 0, run.Spec{TaskName: "calibration", Instruction: calibrationPrompt, PlainPrompt: true,
			Probe: "Calibration codeword: " + codeword, Task: task.Spec{Base: head, Verify: []string{"true"}}, Arm: a.arm, Model: *model,
			BudgetUSD: *budget, Timeout: *timeout})
		if err != nil {
			return fail(env, err)
		}
		m := rec.Metrics
		c := calibration{Arm: a.arm.Name, Snapshot: a.arm.Snapshot, RunID: rec.ID, Outcome: rec.Outcome,
			SandboxOK: strings.Contains(m.ResultExcerpt, "SANDBOX=agentium-sandbox-ok"), LargeOutputOK: strings.Contains(m.ResultExcerpt, "LINE=20000"),
			InstructionsOK: strings.Contains(m.ResultExcerpt, "CODEWORD="+codeword),
			FirstRequest:   m.FirstRequest, EstimatedContext: claudectx.EstimateTokens(resolved.StartupBytes()), CLIVersion: m.CLIVersion,
			Model: m.Model, Tools: m.Tools, Skills: m.Skills, SlashCommands: m.SlashCommands, Drift: rec.Drift, CostUSD: m.CostUSD}
		results = append(results, c)
		clean := rec.Outcome == claude.OutcomeOK && len(rec.Drift) == 0
		healthy = healthy && clean && c.SandboxOK && c.LargeOutputOK && c.InstructionsOK
		if !clean { // a drifted or failed calibration must not become what later runs are checked against
			continue
		}
		encoded, err := json.Marshal(c)
		if err != nil {
			return fail(env, fmt.Errorf("encode calibration: %w", err))
		}
		if err := w.db.SaveCalibration(context.WithoutCancel(ctx), store.Calibration{ProjectID: w.project.ID, Arm: c.Arm,
			Snapshot: c.Snapshot, RunID: rec.ID, Result: encoded, CreatedAt: env.Now()}); err != nil {
			return fail(env, err)
		}
	}
	printCalibration(env, results)
	if !healthy {
		return ExitError
	}
	return ExitOK
}

// printCalibration reports each arm and compares the measured context sizes with Agentium's estimates.
func printCalibration(env Env, results []calibration) {
	out := env.Stdout
	yes := func(ok bool) string { return map[bool]string{true: "ok", false: "FAILED"}[ok] }
	fmt.Fprintf(out, "%-16s %-8s %-8s %-13s %-13s %14s %14s %6s %7s %8s\n", "ARM", "OUTCOME", "SANDBOX", "LARGE OUTPUT", "INSTRUCTIONS",
		"FIRST REQUEST", "ESTIMATED CTX", "TOOLS", "SKILLS", "COST")
	for _, c := range results {
		fmt.Fprintf(out, "%-16s %-8s %-8s %-13s %-13s %14d %14d %6d %7d %8s\n", c.Arm, c.Outcome, yes(c.SandboxOK), yes(c.LargeOutputOK),
			yes(c.InstructionsOK), c.FirstRequest, c.EstimatedContext, len(c.Tools), len(c.Skills), fmt.Sprintf("$%.3f", c.CostUSD))
		for _, d := range c.Drift {
			fmt.Fprintf(out, "  unfair: %s (not saved as the arm's calibration)\n", d)
		}
	}
	fmt.Fprintf(out, "INSTRUCTIONS: the agent repeated a codeword Agentium added to the arm's startup instruction file, so that file loads.\n")
	if len(results) > 0 {
		fmt.Fprintf(out, "Claude Code %s, %s. The first request also holds Claude Code's own system prompt and tools; between arms:\n",
			orNone(results[0].CLIVersion), orNone(results[0].Model))
	}
	base := results[0]
	for _, c := range results[1:] {
		// Estimates count about four bytes per token; real counts run higher for text dense with paths and links, and
		// Claude Code wraps each file. Experiments report the measured size; this ratio says how far off estimates are.
		measured, estimated := c.FirstRequest-base.FirstRequest, int64(c.EstimatedContext-base.EstimatedContext)
		ratio := "n/a"
		if estimated != 0 {
			ratio = fmt.Sprintf("%.2f", float64(measured)/float64(estimated))
		}
		fmt.Fprintf(out, "  %s: measured %+d tokens, estimated %+d (measured/estimated %s)\n", c.Arm, measured, estimated, ratio)
	}
}
