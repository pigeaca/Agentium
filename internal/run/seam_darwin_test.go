package run

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/task"
)

// A sandboxed validation is denied what its agent is: its stages' grades ask the seam (CommandsEnv.Agent) for the
// denied paths, as a run's grade does.
func TestValidationDeniesWhatTheAgentIsDenied(t *testing.T) {
	needSandbox(t)
	ctx := context.Background()
	f := newModuleOnce(t, "", "decoy", "")
	var calls []string
	c := CommandsEnv{Layout: f.env.Layout, Bare: f.env.Bare, Environ: f.env.Environ, Timeout: time.Minute, Grader: task.GraderSandbox,
		Home: f.env.Home, ProjectRoot: f.env.ProjectRoot, Agent: recordingAgent{calls: &calls}}
	stage := filepath.Join(f.env.Layout.Artifacts, "tasks", "1", "v1")
	cc, err := CheckoutCommands(ctx, c, f.spec.Task.Base, []string{"true"}, filepath.Join(stage, "warm.log"))
	if err != nil {
		t.Fatal(err)
	}
	if cc.Isolated == nil {
		t.Fatal("no sandboxed commands")
	}
	dir := filepath.Join(stage, "base")
	must(t, checkout.New(ctx, f.env.Bare, f.spec.Task.Base, dir))
	results, ok, _, err := cc.Isolated(ctx, dir, filepath.Join(stage, "grading", "base"), false, []string{"true"}, time.Minute, io.Discard, nil)
	skipLogBlind(t, err)
	if err != nil || !ok || len(results) != 1 {
		t.Fatalf("results %+v, ok %v, %v", results, ok, err)
	}
	if !slices.Equal(calls, []string{"DeniedPaths"}) {
		t.Errorf("calls through the seam %q, want the denied paths alone", calls)
	}
}
