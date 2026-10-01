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

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/term"
)

// Validation statuses.
const (
	StatusValid     = "valid"     // in every arm: the hidden tests fail on the base and the reference passes them
	StatusInvalid   = "invalid"   // some stage did not behave as required
	StatusUnchecked = "unchecked" // no solution to check with: only the base's verification was run
	StatusFlaky     = "flaky"     // some stage gave different results in repeated runs: the task cannot be trusted
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
	// With --repeat N above 1 the stage ran N times in fresh checkouts and the fields above describe one representative
	// run (the first that was not OK, else the first). Runs is N; the counts say how the runs went. Flaky means the runs
	// disagreed (some passed, some did not, or only some were OK); a flaky stage is never OK. All are absent for one run.
	Runs            int  `json:"runs,omitempty"`
	PassedRuns      int  `json:"passed_runs,omitempty"`
	OKRuns          int  `json:"ok_runs,omitempty"`
	SetupFailedRuns int  `json:"setup_failed_runs,omitempty"`
	TimedOutRuns    int  `json:"timed_out_runs,omitempty"`
	Flaky           bool `json:"flaky,omitempty"`
}

// Validation is the outcome of validating a task.
type Validation struct {
	Status string    `json:"status"`
	Arms   []Arm     `json:"arms"`
	Stages []Stage   `json:"stages"`
	At     time.Time `json:"at"`
	// Repeats is how many times each stage ran when that was more than once; absent (older validations, a single run)
	// means once. See RepeatCount.
	Repeats int `json:"repeats,omitempty"`
	// HarnessChanged lists, per arm, settings, hooks and MCP files that differ from the base.
	HarnessChanged map[string][]string `json:"harness_changed,omitempty"`
	// ContextKept lists, per arm, context files the solution also changes: the arm keeps its own version of them.
	ContextKept map[string][]string `json:"context_kept,omitempty"`
	// WeakTests is the weak-tests check (--weak-tests); nil means it was not run. It is a warning, not a status.
	WeakTests *WeakTests `json:"weak_tests,omitempty"`
	// Judge is the check of a judge-graded task (ValidateJudged), which runs nothing; nil for test-graded tasks.
	Judge *JudgeCheck `json:"judge,omitempty"`
}

// RepeatCount is how many times each stage ran: 1 when Repeats is absent.
func (v Validation) RepeatCount() int {
	return max(v.Repeats, 1)
}

// Validator runs validations. Checkouts are made in WorkDir and removed afterwards unless Keep is set.
type Validator struct {
	Bare      string
	WorkDir   string
	LogDir    string
	Timeout   time.Duration // per command
	Keep      bool
	WeakTests bool     // also try the reference solution without each of its hunks (see weakTests)
	MaxHunks  int      // how many hunks that tries; 0 means DefaultMaxHunks
	Repeats   int      // runs per stage, each in a fresh checkout; below 2 means once
	Env       []string // added to every setup and verification command (a build cache of Agentium's own)
	// Cache is the data folder's cache root (home.Layout.Cache): with it, each checkout's own build tools (Maven,
	// Gradle, Cargo) keep their caches there too, or have their wrappers cleared, like Go's in Env. Empty: Env alone.
	Cache    string
	Progress io.Writer  // one line per stage
	Style    term.Style // styles each progress line's verdict; the zero Style prints plain text
	// Started, when set, is called before each stage: it only feeds a status display and must not print.
	Started func(arm, stage string)
	Now     func() time.Time
}

// Validate checks spec in each arm, stopping an arm at its first stage that does not behave as required.
func (v Validator) Validate(ctx context.Context, spec Spec, arms []Arm) (Validation, error) {
	result := Validation{Status: StatusValid, Arms: arms, At: v.Now().UTC()}
	if v.Repeats > 1 {
		result.Repeats = v.Repeats
	}
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
			switch {
			case stage.Flaky:
				result.Status = StatusFlaky
			case !stage.OK && result.Status != StatusFlaky:
				result.Status = StatusInvalid
			}
		}
	}
	if v.WeakTests {
		if solution == nil {
			return result, ErrNoSolution
		}
		switch reason := weakSkipReason(result.Stages); {
		case reason != "":
			result.WeakTests = &WeakTests{Reason: reason}
		default:
			weak, err := v.weakTests(ctx, spec, solution)
			result.WeakTests = weak
			if err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

// weakSkipReason says why the weak-tests check cannot run: the base context's reference stage must have run and been OK.
// It is "" when it can.
func weakSkipReason(stages []Stage) string {
	reference := false
	for _, s := range stages {
		switch {
		case s.Arm != "base":
		case s.Flaky:
			return fmt.Sprintf("the base context's %s stage is flaky, so it cannot be trusted to compare against", s.Stage)
		case !s.OK:
			return fmt.Sprintf("the base context's %s stage did not behave as required, so the reference was not proven to pass", s.Stage)
		case s.Stage == StageReference:
			reference = true
		}
	}
	if !reference {
		return "the reference stage did not run in the base context"
	}
	return ""
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
		stage, err := v.runStageRepeated(ctx, spec, arm, s.name, s.want, snap, overlay, solution, s.files)
		stages = append(stages, stage)
		if err != nil {
			return stages, overlay.HarnessChanged, kept, err
		}
		if !stage.OK {
			break
		}
	}
	sort.Strings(kept)
	return stages, overlay.HarnessChanged, kept, nil
}

// runStageRepeated runs a stage v.Repeats times (once by default), each in a fresh checkout, and folds the runs into one
// Stage. Every repeat runs even after a failure, since disagreement between runs is what it looks for. An error (setup
// that cannot run, cancellation) stops it at once and returns the runs so far as the stage, not OK.
func (v Validator) runStageRepeated(ctx context.Context, spec Spec, arm Arm, name string, want bool, snap source.Source,
	overlay snapshot.Overlay, solution source.Source, files []string) (Stage, error) {
	n := max(v.Repeats, 1)
	var runs []Stage
	for i := 1; i <= n; i++ {
		if v.Started != nil {
			if n > 1 {
				v.Started(arm.Name, fmt.Sprintf("%s %d/%d", name, i, n))
			} else {
				v.Started(arm.Name, name)
			}
		}
		run, err := v.runStage(ctx, spec, arm, name, want, snap, overlay, solution, files, i, n)
		if err != nil {
			return fold(append(runs, run), n), err
		}
		runs = append(runs, run)
		v.report(run, i, n)
	}
	if n == 1 {
		return runs[0], nil
	}
	stage := fold(runs, n)
	if stage.Flaky && v.Progress != nil {
		fmt.Fprintf(v.Progress, "  %-10s %-13s %s\n", stage.Arm, stage.Stage, v.Style.Status("flaky: "+flakyText(stage)))
	}
	return stage, nil
}

// fold combines the runs of one stage: the first run that was not OK represents it (else the first), counts say how
// the runs went, and disagreement makes it flaky and so not OK. Runs is how many runs happened: below n after an
// error, which also is never flaky (the partial result is not OK). With one run (n == 1) it returns that run as is.
func fold(runs []Stage, n int) Stage {
	stage := runs[0]
	if n == 1 {
		return stage
	}
	for _, r := range runs {
		if !r.OK {
			stage = r
			break
		}
	}
	stage.Runs = len(runs)
	stage.PassedRuns, stage.OKRuns, stage.SetupFailedRuns, stage.TimedOutRuns = 0, 0, 0, 0
	for _, r := range runs {
		if r.Passed {
			stage.PassedRuns++
		}
		if r.OK {
			stage.OKRuns++
		}
		if r.SetupFailed {
			stage.SetupFailedRuns++
		}
		if timedOut(r.Commands) {
			stage.TimedOutRuns++
		}
	}
	stage.Flaky = len(runs) == n && (stage.PassedRuns != 0 && stage.PassedRuns != n || stage.OKRuns != 0 && stage.OKRuns != n)
	if stage.Flaky {
		stage.OK = false
	}
	return stage
}

// flakyText says how a flaky stage's runs disagreed.
func flakyText(s Stage) string {
	var text string
	if s.PassedRuns != 0 && s.PassedRuns != s.Runs {
		text = fmt.Sprintf("%s/%s passed %d of %d times", s.Arm, s.Stage, s.PassedRuns, s.Runs)
	} else { // the runs agree on passing or failing, but only some behaved as required (the others timed out)
		text = fmt.Sprintf("%s/%s behaved as required %d of %d times", s.Arm, s.Stage, s.OKRuns, s.Runs)
	}
	if s.TimedOutRuns > 0 {
		text += fmt.Sprintf("; %d timed out", s.TimedOutRuns)
	}
	if s.SetupFailedRuns > 0 {
		text += fmt.Sprintf(" (its setup failed %d %s)", s.SetupFailedRuns, map[bool]string{true: "time", false: "times"}[s.SetupFailedRuns == 1])
	}
	return text
}

// runStage prepares a fresh checkout for one stage and runs the verification in it.
func (v Validator) runStage(ctx context.Context, spec Spec, arm Arm, name string, want bool, snap source.Source,
	overlay snapshot.Overlay, solution source.Source, files []string, repeat, repeats int) (Stage, error) {
	label := arm.Name + "-" + name
	if repeats > 1 { // each repeat has its own checkout and log
		label += fmt.Sprintf("-r%d", repeat)
	}
	stage := Stage{Arm: arm.Name, Stage: name, Want: passFail(want), Log: filepath.Join(v.LogDir, label+".log")}
	dir := filepath.Join(v.WorkDir, label)
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

// report prints one progress line for a run of a stage; with several repeats it ends with the run's number.
func (v Validator) report(stage Stage, repeat, repeats int) {
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
	line := fmt.Sprintf("  %-10s %-13s want %-4s got %-4s %s", stage.Arm, stage.Stage, stage.Want, got, v.Style.Status(verdict))
	if repeats > 1 {
		line += fmt.Sprintf(" (%d/%d)", repeat, repeats)
	}
	fmt.Fprintln(v.Progress, line)
}

// envFor is Env plus the caches of the build tools the checkout in dir uses (see Cache).
func (v Validator) envFor(dir string) []string {
	if v.Cache == "" {
		return v.Env
	}
	return append(slices.Clone(v.Env), buildtool.CommandEnvFor(buildtool.Select(buildtool.DetectIn(dir)), v.Cache)...)
}

// run runs commands in dir, logging to log, until one fails; ok is whether all of them passed.
func (v Validator) run(ctx context.Context, log io.Writer, dir string, commands []string) (results []Command, ok bool, err error) {
	for _, command := range commands {
		fmt.Fprintf(log, "$ %s\n", command)
		result, err := runner.Run(ctx, runner.Spec{Dir: dir, Command: command, Timeout: v.Timeout, Output: log, Env: v.envFor(dir)})
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
		case stage.Flaky:
			failed = append(failed, flakyText(stage))
		case stage.SetupFailed:
			failed = append(failed, fmt.Sprintf("%s/%s setup failed", stage.Arm, stage.Stage))
		case !stage.OK && timedOut(stage.Commands):
			failed = append(failed, fmt.Sprintf("%s/%s timed out", stage.Arm, stage.Stage))
		case !stage.OK:
			failed = append(failed, fmt.Sprintf("%s/%s wanted %s", stage.Arm, stage.Stage, stage.Want))
		}
	}
	if v.Judge != nil {
		failed = append(failed, v.Judge.Problems...)
	}
	if len(failed) == 0 {
		return v.Status
	}
	return v.Status + ": " + strings.Join(failed, "; ")
}
