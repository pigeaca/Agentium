package claude

import (
	"context"
	"io"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/project"
)

// Adapter is Claude Code behind the agent seam (agent.Adapter): each method is this package's own, unchanged, on the
// agent-neutral invocation (Invocation is agent.Invocation with Claude Code's methods).
type Adapter struct{}

var _ agent.Adapter = Adapter{}

// Name is Claude Code's name in records.
func (Adapter) Name() string { return agent.ClaudeCode }

// Command is Invocation.Command.
func (Adapter) Command(inv agent.Invocation, environ []string) (args, env []string, err error) {
	return Invocation(inv).Command(environ)
}

// DeniedPaths is Invocation.DeniedPaths.
func (Adapter) DeniedPaths(inv agent.Invocation, environ []string) []string {
	return Invocation(inv).DeniedPaths(environ)
}

// Parse reads a stream-json transcript (Parse).
func (Adapter) Parse(r io.Reader) (agent.Metrics, error) { return Parse(r) }

// Classify is Classify.
func (Adapter) Classify(m agent.Metrics, timedOut bool, drift []string) string {
	return Classify(m, timedOut, drift)
}

// Check is Check.
func (Adapter) Check(m agent.Metrics, expect agent.Expect) []string { return Check(m, expect) }

// Version is Claude Code's dotted version (project.ClaudeVersion).
func (Adapter) Version(ctx context.Context, cli string) (string, error) {
	return project.ClaudeVersion(ctx, cli)
}
