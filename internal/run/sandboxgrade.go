package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/sandbox"
	"github.com/pigeaca/agentium/internal/task"
)

// Sandboxed grading (the isolation plan, Part 1, step 3): in sandbox mode (Env.Grader, task.GraderSandbox) a run's
// verification commands, and a validation stage's, run under Agentium's grading profile (internal/sandbox) in a grade
// folder of their own (withGrading), with the agent's recipe for an environment and a cache cloned from the seed.
//
// Fail closed: the canary runs first, and checks the profile file before each command. When the sandbox cannot be
// shown to hold (sandbox-exec missing, refused or nested, a changed profile file), nothing runs: the grade is an
// infrastructure outcome, never a fail, and never a host grade instead.
//
// The denials the kernel logged under the grade's tag are read afterwards (sandbox.ReadDenials). A grade that failed
// with denials the agent's own sandbox does not impose is infrastructure too (decision 3); a pass stays a pass.

// OutcomeSandboxFlagged is the outcome of a run whose sandboxed grade failed with denials the agent's own sandbox does
// not impose (decision 3, as the user decided on 2026-10-03): the failure may be the sandbox's, so the run is not
// counted; and it is not tried again either, since a retry would let an arm re-roll its failures. It settles its slot
// as an unfair run does (experiment.Settles). A canary or usability failure stays ordinary infrastructure, retried.
const OutcomeSandboxFlagged = "infra-sandbox"

// DenialsWait bounds the wait for a grade's denials to reach the unified log.
const DenialsWait = 10 * time.Second

// sandboxed reports whether env grades in the sandbox.
func (env Env) sandboxed() bool { return task.GraderOf(env.Grader) != task.GraderHost }

// SandboxUsable checks, before anything is spent, that this Agentium can grade in mode: a mode it knows, and for the
// sandbox, a sandbox-exec that applies a profile here and a unified log that shows its denials (sandbox.Usable). It
// takes about a second, so commands check it once (an experiment's lock and resume, a validation command); each run
// and validated task checks only sandboxApplies. Every grade still runs the full canary.
func SandboxUsable(ctx context.Context, mode string) error {
	if !task.KnownGrader(mode) {
		return fmt.Errorf("grader %s: this Agentium grades on the host or in %s", mode, task.GraderSandbox)
	}
	if task.GraderOf(mode) == task.GraderHost {
		return nil
	}
	return sandbox.Usable(ctx)
}

// sandboxApplies is the check every run and every validated task makes before it starts: a mode this Agentium knows,
// and for the sandbox, a sandbox-exec that applies a profile here (sandbox.Applies). The log was proven readable once,
// when the command began (SandboxUsable); a grade whose denials the log does not show in time records them as unread.
func sandboxApplies(ctx context.Context, mode string) error {
	if !task.KnownGrader(mode) {
		return fmt.Errorf("grader %s: this Agentium grades on the host or in %s", mode, task.GraderSandbox)
	}
	if task.GraderOf(mode) == task.GraderHost {
		return nil
	}
	return sandbox.Applies(ctx)
}

// fullCommitOf resolves commit in the bare repository to its full ID: a grading seed is per base commit, and a branch
// or short ID could name another commit later.
func fullCommitOf(ctx context.Context, bare, commit string) (string, error) {
	id, err := gitx.Run(ctx, "--git-dir", bare, "rev-parse", "--verify", "--quiet", "--end-of-options", commit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve the base %s: %w", commit, err)
	}
	return id, nil
}

// sandboxGrade is what a sandboxed grade runs, besides Env.
type sandboxGrade struct {
	// Root is the grade's own folder, which must not exist (gradingInput.Root); Copy the checkout to grade, moved into
	// Root and removed with it, or moved back to Keep when Keep is set (--keep).
	Root, Copy, Keep string
	// Agent is the agent's invocation, or one with its tools, deps, JDK, venv, metadata, import root, home and denied
	// paths (Deny, TokenFile, TempRoot as the run had them): the grade gets the agent's recipe and is denied what the
	// agent was.
	Agent claude.Invocation
	// Base is the task's base commit, a full ID: the seed's.
	Base     string
	Commands []string
	// Harmless are the flagged denials the task's reference solution logged while passing its validation in the
	// sandbox (task.Validation.Harmless): not flagged here.
	Harmless []task.DenialKey
	Timeout  time.Duration
	Log      io.Writer
	Running  func(pid int)
	// Warn is told what the grade left behind that was cleaned up (processes stopped, a folder quarantined).
	Warn func(string)
	// Testing, Cleaning and Quarantined, when set, are told when the commands start (the canary passed), when the
	// grade's cleanup starts, and when it moved a folder into the quarantine: for a live display (Env.Step).
	Testing, Cleaning, Quarantined func()
}

// gradeInSandbox runs the commands in the grading sandbox, until one fails, and returns their results, whether all
// passed, and what the sandbox reported. An error wrapping sandbox.ErrUnavailable means the canary (or a profile check
// before a command) showed the sandbox does not hold: report.Canary says why. Any other error is Agentium's own (or
// cancellation).
func (env Env) gradeInSandbox(ctx context.Context, in sandboxGrade) (results []task.Command, ok bool, report *task.SandboxGrade, err error) {
	profiles := buildtool.SelectRun(in.Agent.Tools, in.Agent.AgentTools)
	seed := ""
	if env.Layout.Cache != "" {
		// Seeds are for sandboxed grades only (a host grade could write one); this one holds the profiles' prepared
		// folders. No warm step: building the base on the host for a seed costs about what it saves a cost experiment's
		// one or two grades per base (the isolation plan, step 3).
		if seed, err = env.gradingSeed(profiles, in.Base); err != nil {
			return nil, false, nil, err
		}
		if err = prepareSeed(ctx, profiles, in.Agent.Deps, seed, nil); err != nil {
			return nil, false, nil, err
		}
	}
	report = &task.SandboxGrade{}
	err = withGrading(ctx, gradingInput{Root: in.Root, Seed: seed, Copy: in.Copy, Keep: in.Keep, Agent: in.Agent, Environ: env.environ(),
		Quarantine: quarantine(env.Layout), Warn: in.Warn, Cleaning: in.Cleaning, Quarantined: in.Quarantined}, func(g grading) error {
		tag, err := sandbox.NewTag()
		if err != nil {
			return err
		}
		written, file, digest, err := g.writeProfile(sandbox.Profile{Tag: tag, Home: in.Agent.Home, AccountHome: in.Agent.AccountHome,
			Environ: env.environ(), Data: env.Layout.Root, Denied: in.Agent.DeniedPaths(env.environ()), Loopback: true})
		if err != nil {
			return fmt.Errorf("the grading profile: %w", err)
		}
		report.Profile = digest
		// The grade's folder is not writable while it runs: its processes cannot rename or remove the copy, the cache
		// or the temp root (which the sweep identifies them by), and the profile denies them changing its mode.
		if err := g.lock(); err != nil {
			return err
		}
		since := time.Now()
		canary := sandbox.CanaryProbes
		if env.canary != nil {
			canary = env.canary
		}
		probes, err := canary(ctx, file, digest, written)
		if err != nil {
			report.Canary = err.Error()
			return err
		}
		report.Canary = task.CanaryPassed
		if in.Testing != nil {
			in.Testing()
		}
		ok = true
		for _, command := range in.Commands {
			if err := sandbox.CheckFile(file, digest); err != nil {
				return err
			}
			fmt.Fprintf(in.Log, "$ %s\n", command)
			dir, err := env.moduleDir(g.Copy) // the caller checked it before the copy moved here: no one writes it now
			if err != nil {
				return err
			}
			spec, err := sandbox.Wrap(runner.Spec{Dir: dir, Command: command, Timeout: in.Timeout, Output: in.Log, Started: in.Running,
				Environ: g.Environ}, file)
			if err != nil {
				return err
			}
			result, err := runner.Run(ctx, spec)
			results = append(results, task.Command{Command: command, ExitCode: result.ExitCode, TimedOut: result.TimedOut,
				Seconds: result.Duration.Round(time.Millisecond).Seconds()})
			if err != nil {
				return err
			}
			if !result.Passed() {
				if result.TimedOut {
					fmt.Fprintf(in.Log, "[agentium] timed out after %s\n", in.Timeout)
				}
				ok = false
				break
			}
		}
		read := sandbox.ReadDenials
		if env.readDenials != nil {
			read = env.readDenials
		}
		denials, err := read(ctx, file, written, since, DenialsWait, probes)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			report.Unread = err.Error()
		default:
			classify(report, written, denials, g.Root, in.Harmless)
		}
		return nil
	})
	if err != nil && report.Canary == "" && !errors.Is(err, sandbox.ErrUnavailable) {
		report = nil // Agentium's own failure before the sandbox ran: nothing to report
	}
	return results, ok, report, err
}

// classify counts a grade's denials, noise left out, and keeps the first task.MaxDenials, and those the agent's own
// sandbox does not impose (flagged), except those in harmless (the task's reference logged them while passing). Each
// kept denial's target has the grade's folder (root, in every form) written as task.GradeFolder, so denials of two
// grades of a task compare.
func classify(report *task.SandboxGrade, p sandbox.Profile, denials []sandbox.Denial, root string, harmless []task.DenialKey) {
	for _, d := range denials {
		if d.Noise() {
			continue
		}
		d.Target = inGrade(d.Target, root)
		report.DenialCount += max(d.Repeats, 1)
		if len(report.Denials) < task.MaxDenials {
			report.Denials = append(report.Denials, d)
		}
		if !p.Flagged(d) {
			continue
		}
		if slices.Contains(harmless, task.KeyOf(d)) {
			report.Harmless += max(d.Repeats, 1)
			continue
		}
		report.FlaggedCount += max(d.Repeats, 1)
		if len(report.Flagged) < task.MaxDenials {
			report.Flagged = append(report.Flagged, d)
		}
	}
}

// inGrade writes the grade's folder root (in any of its forms) in target as task.GradeFolder.
func inGrade(target, root string) string {
	if root == "" {
		return target
	}
	for _, form := range sandbox.Forms(root) {
		if target == form || strings.HasPrefix(target, form+"/") {
			return task.GradeFolder + strings.TrimPrefix(target, form)
		}
	}
	return target
}

// lock makes the grade's folder read-only (0500) while the grade runs, so the grade cannot rename or remove its copy,
// cache or temp root: the grading profile lets it write inside them, and removing one needs only write access to its
// parent, which the folder's mode now denies; the profile denies the grade changing that mode (the folder is not among
// its writable ones). stop makes it writable again, after the sweep.
func (g grading) lock() error {
	if err := os.Chmod(g.Root, 0o500); err != nil {
		return fmt.Errorf("the grade's folder: %w", err)
	}
	return nil
}

// sandboxedCommands is task.CheckoutCommands.Sandboxed for a validation (CheckoutCommands): a stage's checkout is
// graded as a run's copy is, in its own grading folder, with agent's recipe and denied paths.
func (env Env) sandboxedCommands(agent claude.Invocation, base string) func(ctx context.Context, dir, root string, keep bool, commands []string, timeout time.Duration, log io.Writer) ([]task.Command, bool, *task.SandboxGrade, error) {
	return func(ctx context.Context, dir, root string, keep bool, commands []string, timeout time.Duration, log io.Writer) ([]task.Command, bool, *task.SandboxGrade, error) {
		in := sandboxGrade{Root: root, Copy: dir, Agent: agent, Base: base, Commands: commands, Timeout: timeout, Log: log,
			Running: func(int) {}, Warn: func(w string) { fmt.Fprintf(log, "[agentium] warning: %s\n", w) }}
		if keep {
			in.Keep = dir
		}
		return env.gradeInSandbox(ctx, in)
	}
}

// gradeInfraNote is the note of a run whose sandboxed grade is infrastructure.
func gradeInfraNote(report *task.SandboxGrade, err error) string {
	if err != nil {
		return "the grading sandbox is unavailable, so nothing was graded (counted as infrastructure: retried or left out): " + err.Error()
	}
	return fmt.Sprintf("the verification failed in the grading sandbox with %d denial(s) the agent's own sandbox does not impose (%s), "+
		"so the failure may be the sandbox's: left out (%s), not counted and not tried again", report.FlaggedCount, report.FlaggedOperations(),
		OutcomeSandboxFlagged)
}
