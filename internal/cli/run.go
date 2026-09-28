package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

const runUsage = `Usage:
  agentium run once TASK [--snapshot NAME] [--model MODEL] [--effort LEVEL] [--budget USD] [--timeout DURATION] [--keep]
                     one real Claude Code run on TASK, in the base's own context or with a snapshot applied.
                     It costs money (up to --budget, default $3) or uses your plan.
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
	cli := env.Getenv("AGENTIUM_CLAUDE")
	if cli == "" {
		if cli, err = env.LookPath("claude"); err != nil {
			return fail(env, errors.New("Claude Code was not found on PATH: install it, or set AGENTIUM_CLAUDE to its path"))
		}
	}
	mode, secret, tokenFile, err := signIn(env)
	if err != nil {
		return fail(env, err)
	}
	var environ []string
	if env.Environ != nil {
		environ = env.Environ()
	}
	id, err := run.NewID(env.Now())
	if err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Starting a real Claude Code run (%s, sign-in %s): it may cost up to $%.2f.\n", *model, mode, *budget)
	rec, runErr := run.Once(ctx, run.Env{ID: id, Layout: w.layout, Bare: w.bare, ProjectRoot: w.root, CLI: cli, Home: env.Getenv("HOME"),
		Environ: environ, SignIn: mode, Secret: secret, TokenFile: tokenFile, VerifyTimeout: *verifyTimeout, Grace: 30 * time.Second,
		Progress: env.Stdout, Now: env.Now}, run.Spec{TaskName: t.Name, Instruction: t.Instruction,
		Task: task.Spec{Base: t.BaseCommit, Solution: t.SolutionCommit, HiddenTests: t.HiddenTests, Reference: t.Reference, Setup: t.Setup, Verify: t.Verify},
		Arm:  arm, Model: *model, Effort: *effort, BudgetUSD: *budget, Timeout: *timeout, Keep: *keep})
	if rec.Outcome != "" { // it ran, or got far enough to have an outcome: keep the record even if interrupted
		encoded, err := json.Marshal(rec)
		if err != nil {
			return fail(env, fmt.Errorf("encode run: %w", err))
		}
		if err := w.db.SaveRun(ctx, store.Run{ID: rec.ID, ProjectID: w.project.ID, TaskID: t.ID, TaskName: t.Name, Arm: rec.Arm,
			Outcome: rec.Outcome, Passed: rec.Passed, CostUSD: rec.Metrics.CostUSD, Record: encoded, Started: rec.Started, Finished: rec.Finished}); err != nil {
			return fail(env, errors.Join(runErr, err))
		}
	}
	if runErr != nil {
		return fail(env, runErr)
	}
	printRun(env, rec)
	return ExitOK
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
	fmt.Fprintf(out, "  changes      %d file(s), +%d -%d, %d commit(s); tests changed: %v\n", b.FilesChanged, b.LinesAdded, b.LinesRemoved, b.Commits, b.TestsChanged)
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
