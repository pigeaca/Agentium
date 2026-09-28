// Command agentium measures how coding agents, models and project context change coding-agent results.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/pigeaca/agentium/internal/cli"
)

// version is set for releases: go build -ldflags "-X main.version=v0.1.0" ./cmd/agentium. The linker can only set a
// package-level string, so this is the one accepted exception to "no package-level mutable state"; nothing assigns it.
var version = "dev"

func main() {
	// Interrupts cancel the context so long-running commands can stop agent processes cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.Env{Args: os.Args[1:], Stdout: os.Stdout, Stderr: os.Stderr, Version: version})
	stop()
	os.Exit(code)
}
