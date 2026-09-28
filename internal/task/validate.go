package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// solution, the base stage is all that can be run.
const (
	StageSetup       = "setup"        // the task's setup commands, run first in each fresh checkout, must pass
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
	Arm      string    `json:"arm"`
	Stage    string    `json:"stage"`
	Want     string    `json:"want"` // "pass" or "fail"
	Passed   bool      `json:"passed"`
	OK       bool      `json:"ok"` // Passed is what Want asked for
	Commands []Command `json:"commands"`
	Log      string    `json:"log"`
}

// Validation is the outcome of validating a task.
type Validation struct {
	Status string    `json:"status"`
	Arms   []Arm     `json:"arms"`
	Stages []Stage   `json:"stages"`
	At     time.Time `json:"at"`
	// HarnessChanged lists, per arm, settings, hooks and MCP files that differ from the base.
	HarnessChanged map[string][]string `json:"harness_changed,omitempty"`
}

// Validator runs validations. Checkouts are made in WorkDir and removed afterwards unless Keep is set.
type Validator struct {
	Bare     string
	WorkDir  string
	LogDir   string
	Timeout  time.Duration // per command
	Keep     bool
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
		stages, harness, err := v.validateArm(ctx, spec, arm, solution)
		result.Stages = append(result.Stages, stages...)
		if len(harness) > 0 {
			if result.HarnessChanged == nil {
				result.HarnessChanged = map[string][]string{}
			}
			result.HarnessChanged[arm.Name] = harness
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

func (v Validator) validateArm(ctx context.Context, spec Spec, arm Arm, solution source.Source) ([]Stage, []string, error) {
	dir := filepath.Join(v.WorkDir, arm.Name)
	if err := checkout.New(ctx, v.Bare, spec.Base, dir); err != nil {
		return nil, nil, err
	}
	if !v.Keep {
		defer os.RemoveAll(dir)
	}
	var harness []string
	if arm.Snapshot != "" {
		base, err := source.Commit(ctx, spec.Base, "--git-dir", v.Bare)
		if err != nil {
			return nil, nil, err
		}
		snap, err := source.Commit(ctx, arm.Snapshot, "--git-dir", v.Bare)
		if err != nil {
			return nil, nil, err
		}
		overlay, err := snapshot.PlanOverlay(base, snap)
		if err != nil {
			return nil, nil, fmt.Errorf("arm %s: %w", arm.Name, err)
		}
		// Deletes are not in the snapshot, so Write removes them.
		if err := checkout.Write(dir, snap, append(overlay.Writes, overlay.Deletes...)); err != nil {
			return nil, nil, fmt.Errorf("arm %s: %w", arm.Name, err)
		}
		harness = overlay.HarnessChanged
	}
	type step struct {
		name   string
		want   bool         // pass
		before func() error // prepares the checkout
	}
	plan := []step{{StageBase, true, nil}}
	if solution != nil && len(spec.HiddenTests) > 0 && len(spec.Reference) > 0 {
		plan = []step{
			{StageHiddenTests, false, func() error { return checkout.Write(dir, solution, spec.HiddenTests) }},
			{StageReference, true, func() error { return checkout.Write(dir, solution, spec.Reference) }},
		}
	}
	var stages []Stage
	if len(spec.Setup) > 0 {
		stage, err := v.verify(ctx, spec.Setup, dir, arm.Name, StageSetup, true)
		stages = append(stages, stage)
		v.report(stage)
		if err != nil || !stage.OK {
			return stages, harness, err
		}
	}
	for _, step := range plan {
		if step.before != nil {
			if err := step.before(); err != nil {
				return stages, harness, fmt.Errorf("arm %s, %s: %w", arm.Name, step.name, err)
			}
		}
		stage, err := v.verify(ctx, spec.Verify, dir, arm.Name, step.name, step.want)
		stages = append(stages, stage)
		if err != nil {
			return stages, harness, err
		}
		v.report(stage)
		if !stage.OK {
			break
		}
	}
	return stages, harness, nil
}

// report prints one progress line for a stage.
func (v Validator) report(stage Stage) {
	if v.Progress == nil {
		return
	}
	verdict := "ok"
	if !stage.OK {
		verdict = "NOT OK"
	}
	fmt.Fprintf(v.Progress, "  %-10s %-13s want %-4s got %-4s %s\n", stage.Arm, stage.Stage, stage.Want, passFail(stage.Passed), verdict)
}

// verify runs the commands in dir until one fails; the stage passes when all of them pass.
func (v Validator) verify(ctx context.Context, commands []string, dir, arm, name string, want bool) (Stage, error) {
	stage := Stage{Arm: arm, Stage: name, Want: passFail(want), Passed: true, Log: filepath.Join(v.LogDir, arm+"-"+name+".log")}
	log, err := os.Create(stage.Log)
	if err != nil {
		return stage, fmt.Errorf("validation log: %w", err)
	}
	defer log.Close()
	for _, command := range commands {
		fmt.Fprintf(log, "$ %s\n", command)
		result, err := runner.Run(ctx, runner.Spec{Dir: dir, Command: command, Timeout: v.Timeout, Output: log})
		stage.Commands = append(stage.Commands, Command{Command: command, ExitCode: result.ExitCode, TimedOut: result.TimedOut,
			Seconds: result.Duration.Round(time.Millisecond).Seconds()})
		if err != nil {
			return stage, err
		}
		if !result.Passed() {
			if result.TimedOut {
				fmt.Fprintf(log, "[agentium] timed out after %s\n", v.Timeout)
			}
			stage.Passed = false
			break
		}
	}
	stage.OK = stage.Passed == want
	return stage, nil
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
		if !stage.OK {
			failed = append(failed, fmt.Sprintf("%s/%s wanted %s", stage.Arm, stage.Stage, stage.Want))
		}
	}
	if len(failed) == 0 {
		return v.Status
	}
	return v.Status + ": " + strings.Join(failed, "; ")
}
