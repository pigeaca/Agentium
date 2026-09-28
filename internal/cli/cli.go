// Package cli parses the agentium command line and dispatches to commands.
package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"
)

// Exit codes: 0 success, 1 runtime failure, 2 usage error.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Env is everything a command needs from the process, so tests can supply their own.
type Env struct {
	Args     []string
	Stdout   io.Writer
	Stderr   io.Writer
	Version  string
	Dir      string                       // working directory
	Getenv   func(string) string          // os.Getenv
	LookPath func(string) (string, error) // exec.LookPath
	Now      func() time.Time             // time.Now
}

const usage = `agentium measures how coding agents, models and project context change coding-agent results.

Usage:
  agentium <command> [arguments]

Commands:
  init [path]   Register the repository at path (default: current directory) and report what Agentium found
  context       Show what Claude Code loads; save, list and compare versions (agentium context for details)
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
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return ExitOK
	case "version", "--version":
		fmt.Fprintf(env.Stdout, "agentium %s (%s %s/%s)\n", env.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return ExitOK
	case "init":
		return runInit(ctx, env, args)
	case "context":
		return runContext(ctx, env, args)
	default:
		fmt.Fprintf(env.Stderr, "agentium: unknown command %q\n\n%s", command, usage)
		return ExitUsage
	}
}

// fail reports a runtime error and returns ExitError.
func fail(env Env, err error) int {
	fmt.Fprintf(env.Stderr, "agentium: %v\n", err)
	return ExitError
}
