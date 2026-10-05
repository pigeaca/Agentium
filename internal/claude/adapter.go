package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/project"
)

// Adapter is Claude Code behind the agent seam (agent.Adapter): each method is this package's own, unchanged, on the
// agent-neutral invocation (Invocation is agent.Invocation with Claude Code's methods).
type Adapter struct{}

var _ agent.Adapter = Adapter{}

// Name is Claude Code's name in records.
func (Adapter) Name() string { return agent.ClaudeCode }

// Command is Invocation.Command: arguments and environment only (the prompt is an argument).
func (Adapter) Command(inv agent.Invocation, environ []string) (agent.Command, error) {
	args, env, err := Invocation(inv).Command(environ)
	if err != nil {
		return agent.Command{}, err
	}
	return agent.Command{Args: args, Env: env}, nil
}

// DeniedPaths is Invocation.DeniedPaths.
func (Adapter) DeniedPaths(inv agent.Invocation, environ []string) []string {
	return Invocation(inv).DeniedPaths(environ)
}

// Gather does nothing: Claude Code's whole transcript is its standard output, already in the records.
func (Adapter) Gather(string, string) error { return nil }

// Parse reads the records' stream-json transcript (Parse).
func (Adapter) Parse(records string) (agent.Metrics, error) {
	f, err := os.Open(filepath.Join(records, agent.Transcript))
	if err != nil {
		return agent.Metrics{}, fmt.Errorf("run transcript: %w", err)
	}
	defer f.Close()
	return Parse(f)
}

// Classify is Classify: Agentium stops Claude Code only at its timeout (Claude Code keeps its own cost cap).
func (Adapter) Classify(m agent.Metrics, stop agent.Stop, drift []string) string {
	return Classify(m, stop == agent.StopTimeout, drift)
}

// Check is Check.
func (Adapter) Check(m agent.Metrics, expect agent.Expect) []string { return Check(m, expect) }

// Version is Claude Code's dotted version (project.ClaudeVersion).
func (Adapter) Version(ctx context.Context, cli string) (string, error) {
	return project.ClaudeVersion(ctx, cli)
}
