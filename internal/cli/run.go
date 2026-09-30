package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
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
  agentium run show ID [--diff] [--log]
                     one run: outcome, cost, behavior, environment; --diff adds the agent's change, --log the setup
                     and verification output

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
		if found.RequestedModel != *model {
			fmt.Fprintf(env.Stdout, "note: the calibration used %s, this run %s: its tool set may differ by model\n", found.RequestedModel, *model)
		}
	} else if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(env.Stdout, "note: arm %s is not calibrated, so its tools and skills are not checked: agentium run calibrate\n", arm.Name)
	} else {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Starting a real Claude Code run (%s, sign-in %s): it may cost up to $%.2f.\n", *model, runEnv.SignIn, *budget)
	release, err := startRuns(ctx, env, w)
	if err != nil {
		return fail(env, err)
	}
	defer release()
	rec, err := executeRun(ctx, env, w, runEnv, runMeta{TaskID: t.ID, Kind: "task"}, run.Spec{TaskName: t.Name, Instruction: t.Instruction,
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
	return run.Env{Layout: w.layout, Bare: w.bare, ProjectRoot: w.root, CLI: cli, Home: env.Getenv("HOME"), Environ: environ,
		SignIn: mode, Secret: secret, TokenFile: tokenFile, VerifyTimeout: verifyTimeout, Grace: 30 * time.Second, CommandEnv: buildEnv,
		Progress: env.Stdout, Now: env.Now}, nil
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
	orphans, recoverErr := run.Recover(ctx, w.layout, func(id string) (bool, error) { return w.db.HasRun(ctx, id) }, secret, env.Now())
	for _, o := range orphans {
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
		fmt.Fprintf(env.Stdout, "Recovered run %s (task %s, arm %s), left behind by a stopped Agentium: cancelled, $%.2f\n",
			o.Record.ID, o.Record.Task, o.Record.Arm, o.Record.Metrics.CostUSD)
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
		Kind: meta.Kind, Arm: rec.Arm, Outcome: rec.Outcome, Passed: rec.Passed, CostUSD: rec.Metrics.CostUSD, Record: encoded,
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
	fmt.Fprintf(env.Stdout, "%-24s %-40s %-14s %-9s %-6s %8s\n", "ID", "TASK", "ARM", "OUTCOME", "PASSED", "COST")
	for _, r := range runs {
		passed := "-"
		if r.Passed != nil {
			passed = map[bool]string{true: "yes", false: "no"}[*r.Passed]
		}
		name := r.TaskName
		if r.Kind == "calibration" {
			name = "(calibration)"
		}
		fmt.Fprintf(env.Stdout, "%-24s %-40s %-14s %-9s %-6s %8s\n", r.ID, name, r.Arm, r.Outcome, passed, fmt.Sprintf("$%.2f", r.CostUSD))
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
		fmt.Fprintf(env.Stdout, "  experiment   %s, slot %d (from 0), attempt %d\n", name, stored.Slot, stored.Attempt)
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
			fmt.Fprintf(env.Stdout, "\n--- %s: none (%s)\n", name, map[string]string{"agent.diff": "the run was not graded",
				"setup.log": "the task has no setup", "verify.log": "the verification did not run"}[name])
		case err != nil:
			return fail(env, fmt.Errorf("run %s: %w", stored.ID, err))
		default:
			fmt.Fprintf(env.Stdout, "\n--- %s\n%s", name, data)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				fmt.Fprintln(env.Stdout)
			}
		}
	}
	return ExitOK
}

// calibrationPrompt asks for what every real task needs (a sandboxed shell command, a large output read back) and for
// the codeword Agentium added to the arm's instruction file. The checks rest on the transcript's tool results; the
// answer only has to agree.
const calibrationPrompt = "This is an environment check: do not change any files and do not search the repository.\n" +
	"1. Run this command with the Bash tool: printf 'agentium-sandbox-ok\\n'\n" +
	"2. Run exactly this command with the Bash tool, without redirecting its output: seq 1 40000\n" +
	"   Its output is too large to show in full: read the full output that Claude Code saved for you, and find its 20000th line.\n" +
	"3. Your project instructions, as loaded at the start, end with a calibration codeword. If you see none, it is NONE.\n" +
	"Then reply with exactly one line: SANDBOX=<what command 1 printed> LINE=<the 20000th line of command 2's output> CODEWORD=<the codeword>"

// Check states.
const (
	checkOK         = "ok"
	checkFailed     = "FAILED"
	checkUnverified = "unverified" // the run did not show the evidence (for example, it worked around the saved output)
	checkNA         = "n/a"
)

// calibration is what a calibration run found for one arm.
type calibration struct {
	Arm              string `json:"arm"`
	Snapshot         string `json:"snapshot,omitempty"`
	RunID            string `json:"run_id"`
	Outcome          string `json:"outcome"`
	Sandbox          string `json:"sandbox"`      // a sandboxed Bash command returned its output
	LargeOutput      string `json:"large_output"` // the output Claude Code saved was read back
	Instructions     string `json:"instructions"` // the codeword in the arm's instruction file came back without reading it
	FirstRequest     int64  `json:"first_request_tokens"`
	EstimatedContext int    `json:"estimated_context_tokens"` // the resolver's session-start estimate
	CLIVersion       string `json:"cli_version"`
	Model            string `json:"model"`           // as Claude Code reported it
	RequestedModel   string `json:"requested_model"` // as asked for (--model)
	// SignIn is how the calibration signed in: with the user's login, Claude Code keeps large outputs in the user's
	// config, which runs must still read back; with a key or token, in the run's own.
	SignIn string   `json:"sign_in,omitempty"`
	Tools  []string `json:"tools"`
	// Skills and SlashCommands are Claude Code's bundled ones: the arm's own project skills and commands are left out
	// and added back when a run is checked. Stored locally to check later runs; never printed.
	Skills        []string `json:"skills"`
	SlashCommands []string `json:"slash_commands"`
	Drift         []string `json:"drift,omitempty"`
	CostUSD       float64  `json:"cost_usd"`
}

// healthy reports whether a calibration can be what later runs are checked against.
func (c calibration) healthy() bool {
	return c.Outcome == claude.OutcomeOK && len(c.Drift) == 0 && c.Sandbox == checkOK && c.LargeOutput == checkOK &&
		(c.Instructions == checkOK || c.Instructions == checkNA)
}

// judge reads the checks from the run's tool calls and results.
func judge(calls []claude.ToolCall, answer, codeword, probeFile string) (sandbox, large, instructions string) {
	sandbox, large, instructions = checkFailed, checkUnverified, checkFailed
	saved := ""
	for i, c := range calls {
		command, _ := c.Input["command"].(string)
		switch {
		case c.Name == "Bash" && strings.Contains(command, "agentium-sandbox-ok"):
			if !c.IsError && strings.Contains(c.Result, "agentium-sandbox-ok") {
				sandbox = checkOK
			}
		case c.Name == "Bash" && strings.Contains(command, "seq 1 40000") && strings.Contains(c.Result, "<persisted-output>"):
			if _, rest, ok := strings.Cut(c.Result, "saved to: "); ok && len(strings.Fields(rest)) > 0 {
				saved = strings.Fields(rest)[0]
				large = checkFailed // there is a saved output: now it must be read back
				for _, later := range calls[i+1:] {
					file, _ := later.Input["file_path"].(string)
					cmd, _ := later.Input["command"].(string)
					if (file == saved || strings.Contains(cmd, saved)) && !later.IsError && strings.Contains(later.Result, "20000") {
						large = checkOK
						break
					}
				}
			}
		}
	}
	if large == checkOK && !strings.Contains(answer, "LINE=20000") {
		large = checkFailed
	}
	switch {
	case probeFile == "":
		instructions = checkNA
	case strings.Contains(answer, "CODEWORD="+codeword):
		instructions = checkOK
		for _, c := range calls { // finding the codeword with a tool (reading, grepping, a symlink) is not loading it
			if input, _ := json.Marshal(c.Input); strings.Contains(c.Result, codeword) || strings.Contains(string(input), path.Base(probeFile)) {
				instructions = checkUnverified
			}
		}
	}
	return sandbox, large, instructions
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
	if len(rest) != 0 || *budget <= 0 || *timeout <= 0 {
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
	release, err := startRuns(ctx, env, w)
	if err != nil {
		return fail(env, err)
	}
	defer release()
	fmt.Fprintf(env.Stdout, "Calibrating %d arm(s) at %s with real Claude Code runs (%s, sign-in %s): up to $%.2f each.\n",
		len(arms), shortCommit(head), *model, runEnv.SignIn, *budget)
	var results []calibration
	healthy := true
	for _, a := range arms {
		resolved, err := claudectx.Resolve(a.source)
		if err != nil {
			return fail(env, err)
		}
		suffix, err := run.NewID(env.Now()) // random enough to be unguessable
		if err != nil {
			return fail(env, err)
		}
		codeword := "AGENTIUM-" + strings.ToUpper(suffix[len(suffix)-6:])
		rec, err := executeRun(ctx, env, w, runEnv, runMeta{Kind: "calibration"}, run.Spec{TaskName: "calibration", Instruction: calibrationPrompt,
			PlainPrompt: true, Probe: "Calibration codeword: " + codeword, Task: task.Spec{Base: head, Verify: []string{"true"}},
			Arm: a.arm, Model: *model, BudgetUSD: *budget, Timeout: *timeout})
		if err != nil {
			return fail(env, err)
		}
		transcript, err := os.Open(filepath.Join(rec.RecordsDir, "stream.jsonl"))
		if err != nil {
			return fail(env, fmt.Errorf("calibration transcript: %w", err))
		}
		calls, err := claude.ToolCalls(transcript)
		transcript.Close()
		if err != nil {
			return fail(env, err)
		}
		m := rec.Metrics
		c := calibration{Arm: a.arm.Name, Snapshot: a.arm.Snapshot, RunID: rec.ID, Outcome: rec.Outcome, FirstRequest: m.FirstRequest,
			EstimatedContext: claudectx.EstimateTokens(resolved.StartupBytes()), CLIVersion: m.CLIVersion, Model: m.Model,
			RequestedModel: *model, SignIn: runEnv.SignIn, Tools: m.Tools, Skills: without(m.Skills, rec.ProjectSkills),
			SlashCommands: without(m.SlashCommands, rec.ProjectSkills, rec.ProjectCommands), Drift: rec.Drift, CostUSD: m.CostUSD}
		c.Sandbox, c.LargeOutput, c.Instructions = judge(calls, m.ResultExcerpt, codeword, rec.ProbeFile)
		results = append(results, c)
		if !c.healthy() { // only a calibration that passed every check becomes what later runs are checked against
			healthy = false
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
		fmt.Fprintln(env.Stdout, "Not every arm passed: failed arms were not saved as calibrations (see the run records).")
		return ExitError
	}
	return ExitOK
}

// without lists the names in list that none of the others hold.
func without(list []string, others ...[]string) []string {
	out := []string{}
	for _, x := range list {
		found := false
		for _, o := range others {
			found = found || slices.Contains(o, x)
		}
		if !found {
			out = append(out, x)
		}
	}
	return out
}

// printCalibration reports each arm and compares the measured context sizes with Agentium's estimates.
func printCalibration(env Env, results []calibration) {
	out := env.Stdout
	fmt.Fprintf(out, "%-16s %-8s %-10s %-12s %-12s %13s %13s %6s %7s %8s\n", "ARM", "OUTCOME", "SANDBOX", "LARGE OUTPUT", "INSTRUCTIONS",
		"FIRST REQUEST", "ESTIMATED CTX", "TOOLS", "SKILLS", "COST")
	for _, c := range results {
		fmt.Fprintf(out, "%-16s %-8s %-10s %-12s %-12s %13d %13d %6d %7d %8s\n", c.Arm, c.Outcome, c.Sandbox, c.LargeOutput, c.Instructions,
			c.FirstRequest, c.EstimatedContext, len(c.Tools), len(c.Skills), fmt.Sprintf("$%.3f", c.CostUSD))
		for _, d := range c.Drift {
			fmt.Fprintf(out, "  unfair: %s\n", d)
		}
	}
	fmt.Fprintln(out, "Checks rest on the transcript: SANDBOX, the Bash output; LARGE OUTPUT, a read of the output Claude Code saved;")
	fmt.Fprintln(out, "INSTRUCTIONS, the codeword Agentium added to the arm's instruction file, repeated without reading that file.")
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
