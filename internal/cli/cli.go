// Package cli parses the agentium command line and dispatches to commands.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/pigeaca/agentium/internal/term"
)

// Exit codes: 0 success (including "nothing to do"), 1 runtime failure or a bad result (an invalid task), 2 usage error.
// Scripts may rely on these; tests fix them (docs/guide.md, "Scripting and automation").
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Env is everything a command needs from the process, so tests can supply their own.
type Env struct {
	Args    []string
	Stdin   io.Reader // only `context lint --hook` reads it; nil means empty
	Stdout  io.Writer
	Stderr  io.Writer
	Version string
	// Terminal is whether Stdout is a terminal. With NO_COLOR, FORCE_COLOR and TERM it decides whether output is
	// styled (term.Detect); tests leave it false and get plain text.
	Terminal bool
	// StdinTerminal is whether Stdin is a terminal. A command that asks a question asks only when both Stdin and Stdout
	// are terminals; tests leave it false and are never asked.
	StdinTerminal bool
	// Columns is the terminal's width in columns, for the live status line and the dashboard; nil or 0 means unknown.
	Columns func() int
	// Rows is the terminal's height in rows, for the dashboard; nil or 0 means unknown ($LINES, else 24).
	Rows     func() int
	Dir      string                       // working directory; empty when it cannot be read
	Getenv   func(string) string          // os.Getenv
	Environ  func() []string              // os.Environ: the environment runs start from (filtered there)
	LookPath func(string) (string, error) // exec.LookPath
	Now      func() time.Time             // time.Now
	// AccountHome is the account's home folder in the user database (user.Current().HomeDir), which HOME may not be;
	// runs deny the login keychain in both. nil or "" means unknown.
	AccountHome func() string
	// Backoff is how long an experiment waits before retrying a run that failed for infrastructure reasons; nil means
	// 30 seconds, then 2 minutes.
	Backoff func(attempt int) time.Duration
	// Sleep waits for d or until ctx is cancelled (experiment run --wait); nil means a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Docker opens the local Docker daemon for the images and clean commands and for recovery (SystemDocker in main);
	// nil means Docker is not used: clean and recovery then leave containers alone and say so.
	Docker func(ctx context.Context, environ []string) (DockerClient, error)
	// DefaultGrader is the grader mode commands use without --grader (task.GraderHost or task.GraderSandbox); empty
	// means the platform's (task.DefaultGrader: the sandbox on macOS). main leaves it empty.
	DefaultGrader string

	// JSON is set by Run for a command given --json: the handler prints its document with emit instead of text, never
	// asks a question, and uses no styles. Plain is the styling half alone (no escape codes whatever the terminal or
	// FORCE_COLOR say); a command run inside another one's JSON output keeps Plain and clears JSON. Both are Run's, not
	// the caller's: tests and main leave them false.
	JSON, Plain bool
	// flagSink, when set, receives every flag set a command builds before it parses (parseArgs). Only the contract
	// test sets it, to list the real flags (contract_test.go).
	flagSink func(*flag.FlagSet)
	json     *jsonState
	// notice, when set, is where notices go that must reach the screen at once even while a screen holds standard output
	// (the dashboard holds it until the run ends): a run recovered from a dead Agentium, say. nil means Stdout.
	notice io.Writer
}

// noticeOut is where a notice that must not wait goes (Env.notice): standard output, or above the dashboard at once.
func (env Env) noticeOut() io.Writer {
	if env.notice != nil {
		return env.notice
	}
	return env.Stdout
}

const usage = `agentium measures how coding agents, models and project context change coding-agent results.

Usage:
  agentium <command> [arguments]

Commands:
  start         From a repository to a previewed experiment: register, snapshot, mine tasks, create it; resumes (agentium start -h)
  init [path]   Register the repository at path (default: current directory) and report what Agentium found
  context       Show what Claude Code loads; save, list and compare versions (agentium context for details)
  task          Add, import and validate coding tasks (agentium task for details)
  pool          Keep the task pool fresh: mine new commits, re-validate and retire tasks; no agent runs (agentium pool for details)
  run           Run Claude Code on a task and grade it; list and show runs (agentium run for details)
  experiment    Design, preview and run context experiments (agentium experiment for details)
  clean         Show what caches nothing uses take, and with --yes remove them (agentium clean -h)
  images        The images container grading runs in: list them, pull and build them with consent, remove them
  version       Print the version and build information
  help          Show this help

Agentium keeps its data in ~/.agentium (override with AGENTIUM_HOME) and never writes to your repository.
`

// Run executes the command in env.Args and returns the process exit code. ctx is cancelled on interrupt; commands
// that start agent processes must stop them when it is done.
func Run(ctx context.Context, env Env) int {
	if len(env.Args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	command, args := env.Args[0], env.Args[1:]
	if rest, want := splitJSONFlag(command, args); want {
		return runJSON(ctx, env, command, rest)
	} else if len(rest) != len(args) {
		args = rest // --json with a help request: the help is printed as text
	}
	return dispatch(ctx, env, command, args)
}

// dispatch runs a command whose --json flag, if any, has been dealt with.
func dispatch(ctx context.Context, env Env, command string, args []string) int {
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return ExitOK
	case "version", "--version":
		fmt.Fprintf(env.Stdout, "agentium %s (%s %s/%s)\n", env.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return ExitOK
	case "start":
		return runStart(ctx, env, args)
	case "init":
		return runInit(ctx, env, args)
	case "context":
		return runContext(ctx, env, args)
	case "task":
		return runTask(ctx, env, args)
	case "pool":
		return runPool(ctx, env, args)
	case "run":
		return runRun(ctx, env, args)
	case "experiment":
		return runExperiment(ctx, env, args)
	case "clean":
		return runClean(ctx, env, args)
	case "images":
		return runImages(ctx, env, args)
	default:
		fmt.Fprintf(env.Stderr, "agentium: unknown command %q\n\n%s", command, usage)
		return ExitUsage
	}
}

// fail reports a runtime error and returns ExitError.
func fail(env Env, err error) int {
	if env.json != nil {
		env.json.err = err.Error()
	}
	fmt.Fprintf(env.Stderr, "agentium: %v\n", err)
	return ExitError
}

// style is how output to Stdout is styled.
func (env Env) style() term.Style {
	if env.Plain {
		return term.Style{}
	}
	return term.Detect(env.Terminal, env.Getenv)
}

// warning is a warning line's text, its label styled.
func warning(st term.Style, text string) string { return st.Warn("warning:") + " " + text }

// note is a note line's text, dimmed.
func note(st term.Style, text string) string { return st.Note("note: " + text) }
