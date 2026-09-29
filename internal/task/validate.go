package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/source"
)

// Validation statuses.
const (
	StatusValid     = "valid"     // in every arm: the hidden tests fail on the base and the reference passes them
	StatusInvalid   = "invalid"   // some stage did not behave as required
	StatusUnchecked = "unchecked" // no solution to check with: only the base's verification was run
)

// Stage names. With a solution, each arm runs hidden-tests then reference; the base need not pass on its own (a task
// may start where the checks cannot run yet), and an arm whose context breaks the checks fails at reference. Without a
// solution, the base stage is all that can be run. Every stage runs the task's setup commands first; if they fail, so
// does the stage.
const (
	StageBase        = "base"         // the base (with the arm's context) must pass its verification
	StageHiddenTests = "hidden-tests" // with the hidden tests added, it must fail
	StageReference   = "reference"    // with the reference solution too, it must pass
)

// Spec is what validation needs from a task.
type Spec struct {
	Base, Solution         string // commits in the bare repository; Solution may be empty
	HiddenTests, Reference []string
	Setup                  []string // shell commands a fresh checkout needs first (for example, building embedded assets)
	Verify                 []string // shell commands, run in order in the checkout
}

// Arm is a context version to validate in: Snapshot is a snapshot commit, or empty for the base's own context.
type Arm struct {
	Name     string `json:"name"`
	Snapshot string `json:"snapshot,omitempty"`
}

// Command is one verification command's outcome.
type Command struct {
	Command  string  `json:"command"`
	ExitCode int     `json:"exit_code"`
	TimedOut bool    `json:"timed_out,omitempty"`
	Seconds  float64 `json:"seconds"`
}

// Stage is one verification run.
type Stage struct {
	Arm         string    `json:"arm"`
	Stage       string    `json:"stage"`
	Want        string    `json:"want"` // "pass" or "fail"
	Passed      bool      `json:"passed"`
	OK          bool      `json:"ok"` // Passed is what Want asked for (a timeout never is), after a passing setup
	Setup       []Command `json:"setup,omitempty"`
	SetupFailed bool      `json:"setup_failed,omitempty"`
	Commands    []Command `json:"commands"`
	Log         string    `json:"log"`
}

// Validation is the outcome of validating a task.
type Validation struct {
	Status string    `json:"status"`
	Arms   []Arm     `json:"arms"`
	Stages []Stage   `json:"stages"`
	At     time.Time `json:"at"`
	// HarnessChanged lists, per arm, settings, hooks and MCP files that differ from the base.
	HarnessChanged map[string][]string `json:"harness_changed,omitempty"`
	// ContextKept lists, per arm, context files the solution also changes: the arm keeps its own version of them.
	ContextKept map[string][]string `json:"context_kept,omitempty"`
}

// Validator runs validations. Checkouts are made in WorkDir and removed afterwards unless Keep is set.
type Validator struct {
	Bare     string
	WorkDir  string
	LogDir   string
	Timeout  time.Duration // per command
	Keep     bool
	Env      []string  // added to every setup and verification command (a build cache of Agentium's own)
	Progress io.Writer // one line per stage
	Now      func() time.Time
}

// Validate checks spec in each arm, stopping an arm at its first stage that does not behave as required.
func (v Validator) Validate(ctx context.Context, spec Spec, arms []Arm) (Validation, error) {
	result := Validation{Status: StatusValid, Arms: arms, At: v.Now().UTC()}
	if spec.Solution == "" || len(spec.HiddenTests) == 0 || len(spec.Reference) == 0 {
		result.Status = StatusUnchecked
	}
	if err := os.MkdirAll(v.LogDir, 0o700); err != nil {
		return Validation{}, fmt.Errorf("validation logs: %w", err)
	}
	var solution source.Source
	if spec.Solution != "" {
		var err error
		if solution, err = source.Commit(ctx, spec.Solution, "--git-dir", v.Bare); err != nil {
			return Validation{}, err
		}
	}
	for _, arm := range arms {
		stages, harness, kept, err := v.validateArm(ctx, spec, arm, solution)
		result.Stages = append(result.Stages, stages...)
		if len(harness) > 0 {
			if result.HarnessChanged == nil {
				result.HarnessChanged = map[string][]string{}
			}
			result.HarnessChanged[arm.Name] = harness
		}
		if len(kept) > 0 {
			if result.ContextKept == nil {
				result.ContextKept = map[string][]string{}
			}
			result.ContextKept[arm.Name] = kept
		}
		if err != nil {
			return result, err
		}
		for _, stage := range stages {
			if !stage.OK {
				result.Status = StatusInvalid
			}
		}
	}
	return result, nil
}

// validateArm runs an arm's stages. Each stage gets a fresh checkout, prepared as a run would be: the base, the arm's
// context, the setup commands, then files from the solution. So nothing one stage leaves behind (a snapshot a failing
// test wrote, build outputs) reaches the next. Solution files that are the arm's context keep the arm's version.
func (v Validator) validateArm(ctx context.Context, spec Spec, arm Arm, solution source.Source) (stages []Stage, harness, kept []string, err error) {
	var snap source.Source
	var overlay snapshot.Overlay
	armContext := map[string]bool{}
	if arm.Snapshot != "" {
		base, err := source.Commit(ctx, spec.Base, "--git-dir", v.Bare)
		if err != nil {
			return nil, nil, nil, err
		}
		if snap, err = source.Commit(ctx, arm.Snapshot, "--git-dir", v.Bare); err != nil {
			return nil, nil, nil, err
		}
		if overlay, err = snapshot.PlanOverlay(base, snap); err != nil {
			return nil, nil, nil, fmt.Errorf("arm %s: %w", arm.Name, err)
		}
		for _, p := range append(slices.Clone(overlay.Writes), overlay.Deletes...) {
			armContext[p] = true
		}
	}
	solutionFiles := func(paths []string) []string {
		var files []string
		for _, p := range paths {
			if armContext[p] {
				if !slices.Contains(kept, p) {
					kept = append(kept, p)
				}
				continue
			}
			files = append(files, p)
		}
		return files
	}
	type step struct {
		name  string
		want  bool     // pass
		files []string // from the solution
	}
	plan := []step{{StageBase, true, nil}}
	if solution != nil && len(spec.HiddenTests) > 0 && len(spec.Reference) > 0 {
		plan = []step{
			{StageHiddenTests, false, solutionFiles(spec.HiddenTests)},
			{StageReference, true, solutionFiles(append(slices.Clone(spec.HiddenTests), spec.Reference...))},
		}
	}
	for _, s := range plan {
		stage, err := v.runStage(ctx, spec, arm, s.name, s.want, snap, overlay, solution, s.files)
		stages = append(stages, stage)
		if err != nil {
			return stages, overlay.HarnessChanged, kept, err
		}
		v.report(stage)
		if !stage.OK {
			break
		}
	}
	sort.Strings(kept)
	return stages, overlay.HarnessChanged, kept, nil
}

// runStage prepares a fresh checkout for one stage and runs the verification in it.
func (v Validator) runStage(ctx context.Context, spec Spec, arm Arm, name string, want bool, snap source.Source,
	overlay snapshot.Overlay, solution source.Source, files []string) (Stage, error) {
	stage := Stage{Arm: arm.Name, Stage: name, Want: passFail(want), Log: filepath.Join(v.LogDir, arm.Name+"-"+name+".log")}
	dir := filepath.Join(v.WorkDir, arm.Name+"-"+name)
	if err := checkout.New(ctx, v.Bare, spec.Base, dir); err != nil {
		return stage, err
	}
	if !v.Keep {
		defer os.RemoveAll(dir)
	}
	if snap != nil { // Deletes are not in the snapshot, so Write removes them.
		if err := checkout.Write(dir, snap, append(slices.Clone(overlay.Writes), overlay.Deletes...)); err != nil {
			return stage, fmt.Errorf("arm %s: %w", arm.Name, err)
		}
	}
	log, err := os.Create(stage.Log)
	if err != nil {
		return stage, fmt.Errorf("validation log: %w", err)
	}
	defer log.Close()
	if len(spec.Setup) > 0 {
		var ok bool
		if stage.Setup, ok, err = v.run(ctx, log, dir, spec.Setup); err != nil || !ok {
			stage.SetupFailed = !ok
			return stage, err
		}
	}
	if len(files) > 0 {
		if err := checkout.Write(dir, solution, files); err != nil {
			return stage, fmt.Errorf("arm %s, %s: %w", arm.Name, name, err)
		}
	}
	if stage.Commands, stage.Passed, err = v.run(ctx, log, dir, spec.Verify); err != nil {
		return stage, err
	}
	stage.OK = stage.Passed == want
	if !want && timedOut(stage.Commands) { // a timeout is not the failure hidden tests must cause
		stage.OK = false
	}
	return stage, nil
}

// report prints one progress line for a stage.
func (v Validator) report(stage Stage) {
	if v.Progress == nil {
		return
	}
	got, verdict := passFail(stage.Passed), "ok"
	switch {
	case stage.SetupFailed:
		got, verdict = "setup failed", "NOT OK"
	case !stage.OK && timedOut(stage.Commands):
		verdict = "NOT OK (timed out)"
	case !stage.OK:
		verdict = "NOT OK"
	}
	fmt.Fprintf(v.Progress, "  %-10s %-13s want %-4s got %-4s %s\n", stage.Arm, stage.Stage, stage.Want, got, verdict)
}

// run runs commands in dir, logging to log, until one fails; ok is whether all of them passed.
func (v Validator) run(ctx context.Context, log io.Writer, dir string, commands []string) (results []Command, ok bool, err error) {
	for _, command := range commands {
		fmt.Fprintf(log, "$ %s\n", command)
		result, err := runner.Run(ctx, runner.Spec{Dir: dir, Command: command, Timeout: v.Timeout, Output: log, Env: v.Env})
		results = append(results, Command{Command: command, ExitCode: result.ExitCode, TimedOut: result.TimedOut,
			Seconds: result.Duration.Round(time.Millisecond).Seconds()})
		if err != nil {
			return results, false, err
		}
		if !result.Passed() {
			if result.TimedOut {
				fmt.Fprintf(log, "[agentium] timed out after %s\n", v.Timeout)
			}
			return results, false, nil
		}
	}
	return results, true, nil
}

func timedOut(commands []Command) bool {
	for _, c := range commands {
		if c.TimedOut {
			return true
		}
	}
	return false
}

func passFail(pass bool) string {
	if pass {
		return "pass"
	}
	return "fail"
}

// ErrNoVerify means a task has no verification commands.
var ErrNoVerify = errors.New("no verification commands")

// Summary is a one-line account of a validation for listings.
func (v Validation) Summary() string {
	var failed []string
	for _, stage := range v.Stages {
		switch {
		case stage.SetupFailed:
			failed = append(failed, fmt.Sprintf("%s/%s setup failed", stage.Arm, stage.Stage))
		case !stage.OK && timedOut(stage.Commands):
			failed = append(failed, fmt.Sprintf("%s/%s timed out", stage.Arm, stage.Stage))
		case !stage.OK:
			failed = append(failed, fmt.Sprintf("%s/%s wanted %s", stage.Arm, stage.Stage, stage.Want))
		}
	}
	if len(failed) == 0 {
		return v.Status
	}
	return v.Status + ": " + strings.Join(failed, "; ")
}
