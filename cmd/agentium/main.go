// Command agentium measures how coding agents, models and project context change coding-agent results.
package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/cli"
	"github.com/pigeaca/agentium/internal/term"
)

// version is set for releases: go build -ldflags "-X main.version=v0.1.0" ./cmd/agentium. The linker can only set a
// package-level string, so this is the one accepted exception to "no package-level mutable state"; nothing assigns it.
var version = "dev"

func main() {
	dir, err := os.Getwd()
	if err != nil {
		dir = "" // commands that need it say so; help and version still work
	}
	// Interrupts cancel the context so long-running commands can stop agent processes cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.Env{
		Args: os.Args[1:], Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Version: version, Terminal: term.IsTerminal(os.Stdout),
		StdinTerminal: term.IsTerminal(os.Stdin),
		Columns:       func() int { return term.Columns(os.Stdout) },
		Dir:           dir, Getenv: os.Getenv, Environ: os.Environ, LookPath: exec.LookPath, Now: time.Now,
	})
	stop()
	os.Exit(code)
}
