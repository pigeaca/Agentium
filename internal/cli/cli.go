// Package cli parses the agentium command line and dispatches to commands.
package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
)

// Exit codes: 0 success, 1 runtime failure, 2 usage error.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Env is everything a command needs from the process, so tests can supply their own.
type Env struct {
	Args    []string
	Stdout  io.Writer
	Stderr  io.Writer
	Version string
}

const usage = `agentium measures how coding agents, models and project context change coding-agent results.

Usage:
  agentium <command>

Commands:
  version   Print the version and build information
  help      Show this help
`

// Run executes the command in env.Args and returns the process exit code. ctx is cancelled on interrupt; commands
// that start agent processes must stop them when it is done.
func Run(ctx context.Context, env Env) int {
	if len(env.Args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	switch env.Args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return ExitOK
	case "version", "--version":
		fmt.Fprintf(env.Stdout, "agentium %s (%s %s/%s)\n", env.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "agentium: unknown command %q\n\n%s", env.Args[0], usage)
		return ExitUsage
	}
}
