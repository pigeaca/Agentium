package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/sandbox"
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
	Base, Solution string // commits in the bare repository; Solution may be empty
	// Module is the monorepo folder the task runs in (store.Task.Module; "": the root): its setup and verification
	// commands run there. It is part of a locked task's digest only when set, so old locks keep theirs.
	Module                 string
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
	// Sandbox is what the grading sandbox reported for the verification (sandbox mode only): its profile, and the
	// denials it logged. A run that failed with Flagged denials is not OK (FlaggedFailure).
	Sandbox *SandboxGrade `json:"sandbox,omitempty"`
	// With --repeat N above 1 the stage ran N times in fresh checkouts and the fields above describe one representative
	// run (the first that was not OK, else the first). Runs is N; the counts say how the runs went. Flaky means the runs
	// disagreed (some passed, some did not, or only some were OK); a flaky stage is never OK. All are absent for one run.
	Runs            int  `json:"runs,omitempty"`
	PassedRuns      int  `json:"passed_runs,omitempty"`
	OKRuns          int  `json:"ok_runs,omitempty"`
	SetupFailedRuns int  `json:"setup_failed_runs,omitempty"`
	TimedOutRuns    int  `json:"timed_out_runs,omitempty"`
	Flaky           bool `json:"flaky,omitempty"`
	// repeats are the runs fold folded into this stage (with --repeat above 1), for the sandbox checks that must see
	// every run's denials (harmlessFailure); not stored.
	repeats []Stage
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
	// Toolchain is the build tools' versions the stages ran with (Validator.Toolchain). Absent: unknown (validations
	// before it was recorded, judge-graded tasks, callers that do not detect it), which never makes a validation stale.
	Toolchain Toolchain `json:"toolchain,omitempty"`
	// Notes are what preparing the build tools said (Validator.Checkout): a venv resolved without a lock file, a test
	// runner it lacks, a warm-up that failed.
	Notes []string `json:"notes,omitempty"`
	// Warnings are what the task's commands keep from grading, which never change its status: hidden Go tests a verify
	// command's -skip or -run pattern keeps from running (FilteredHiddenTests).
	Warnings []string `json:"warnings,omitempty"`
	// Grader is the mode the stages' verification ran in (GraderHost or GraderSandbox); empty in validations made before
	// modes, which ran on the host (GraderOf). An experiment takes only tasks validated in its own mode.
	Grader string `json:"grader,omitempty"`
	// Harmless are the flagged denials (DenialKey) the passing reference stages logged in the sandbox: the union over
	// the arms (and over each stage's repeats), so a denial any arm's reference logged counts. Shown harmless for this
	// task and toolchain, a later grade's denial among them is not flagged. Absent in host validations and those made
	// before it was kept: then every flagged denial counts.
	Harmless []DenialKey `json:"harmless_denials,omitempty"`
}

// SandboxGrade is what a sandboxed grade reports beside its commands' results (a run's record, a validation stage).
type SandboxGrade struct {
	// Canary is "passed", or why the canary showed the sandbox does not hold: then nothing ran, and the grade is an
	// infrastructure outcome (never a fail, never a host grade instead).
	Canary string `json:"canary"`
	// Profile is the SHA-256 of the grade's profile file. It differs per grade (the paths do); the mode names the rules.
	Profile string `json:"profile_sha256,omitempty"`
	// DenialCount counts the denials the kernel logged under the grade's tag, noise left out (sandbox.Denial.Noise),
	// repeats included: a lower bound, as the kernel limits the rate. Denials are the first MaxDenials of them.
	DenialCount int              `json:"denial_count,omitempty"`
	Denials     []sandbox.Denial `json:"denials,omitempty"`
	// Flagged are the denials the agent's own sandbox does not impose (sandbox.Profile.Flagged), the first MaxDenials,
	// and FlaggedCount all of them. A failed grade with any is infrastructure (the isolation plan's decision 3).
	FlaggedCount int              `json:"flagged_count,omitempty"`
	Flagged      []sandbox.Denial `json:"flagged,omitempty"`
	// Unread says why the denials could not be read (sandbox.ErrDenialsUnread); the result then stands as the tests gave
	// it, since no flagged denial is known.
	Unread string `json:"denials_unread,omitempty"`
	// Harmless counts the denials that would be flagged but that the task's reference solution logged too while
	// passing its validation in the sandbox (Validation.Harmless): shown harmless for this task, they are not flagged.
	Harmless int `json:"harmless,omitempty"`
}

// DenialKey is a flagged denial as validation keeps it, to compare with later grades of the task: its operation and
// target, a path inside the grade's own folder written from GradeFolder on (each grade's folder has its own path).
type DenialKey struct {
	Operation string `json:"operation"`
	Target    string `json:"target,omitempty"`
}

// GradeFolder stands for a grade's own folder in a denial's target (KeyOf).
const GradeFolder = "<grade>"

// KeyOf is d's key. The grade's folder in its target is already written as GradeFolder (run's classify does that for
// every denial it keeps).
func KeyOf(d sandbox.Denial) DenialKey { return DenialKey{Operation: d.Operation, Target: d.Target} }

// MaxDenials bounds the denials a SandboxGrade keeps: a grade chooses how many it causes.
const MaxDenials = 20

// CanaryPassed is SandboxGrade.Canary when the sandbox held.
const CanaryPassed = "passed"

// FlaggedOperations lists the operations of the flagged denials, each once, for notes that must not print what the
// grade chose (paths and names): "mach-lookup, ipc-posix-shm-read-data".
func (g SandboxGrade) FlaggedOperations() string {
	var ops []string
	for _, d := range g.Flagged {
		if !slices.Contains(ops, d.Operation) {
			ops = append(ops, d.Operation)
		}
	}
	return strings.Join(ops, ", ")
}

// FlaggedFailure reports whether a grade that did not pass counts as infrastructure: it ran in the sandbox and logged
// denials the agent's own sandbox does not impose.
func (g *SandboxGrade) FlaggedFailure(passed bool) bool {
	return g != nil && !passed && g.FlaggedCount > 0
}

// CheckoutCommands is how Agentium's own commands run in a checkout of a base commit once its build tools are warmed
// (run.CheckoutCommands, as setup and grading run in a run): Environ, when not nil, replaces their base environment
// (the user's, less what the agent never gets either, such as PYTHON*), Env(dir) is added after Validator.Env and the
// tools' caches (Python's venv, its PYTHONPATH in dir), and Notes go to the validation.
type CheckoutCommands struct {
	Environ []string
	Env     func(dir string) []string
	Notes   []string
	// Removed, when set, removes what Env gave the checkout in dir alone (Python's hypothesis database in the data
	// folder), once the checkout is removed.
	Removed func(dir string)
	// Sandboxed, when set, runs commands in the grading sandbox, as a run's grade does (run.CheckoutCommands sets it in
	// sandbox mode): the checkout dir is moved into root, a grading folder of its own that must not exist yet, and is
	// removed with it afterwards, unless keep, when the checkout is moved back to dir first. Output goes to log; each
	// command has timeout. An error wrapping sandbox.ErrUnavailable means the sandbox could not be shown to hold, and
	// nothing ran.
	Sandboxed func(ctx context.Context, dir, root string, keep bool, commands []string, timeout time.Duration, log io.Writer) ([]Command, bool, *SandboxGrade, error)
}

// Toolchain maps a build tool ("go", "java", "cargo", ...) to its version as the tool reports it on the host.
type Toolchain map[string]string

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
	// Module is the monorepo module the project measures (store.Settings.Module; "": the root): setup and verification
	// commands run in its folder of each checkout, and its build files decide the tools' caches.
	Module string
	// Cache is the data folder's cache root (home.Layout.Cache): with it, each checkout's own build tools (Maven,
	// Gradle, Cargo) keep their caches there too, or have their wrappers cleared, like Go's in Env. Empty: Env alone.
	Cache    string
	Progress io.Writer  // one line per stage
	Style    term.Style // styles each progress line's verdict; the zero Style prints plain text
	// Started, when set, is called before each stage: it only feeds a status display and must not print.
	Started func(arm, stage string)
	Now     func() time.Time
	// Toolchain, when set, is recorded in every validation made: the build tools' versions on this host, detected once
	// by the caller (internal/pool's DetectToolchain), which compares them later to find stale validations.
	Toolchain Toolchain
	// Checkout, when set, warms the base's build tools once per validation, before any stage, as a run's setup does
	// (the same deps folder, lock and stamp: run.CheckoutCommands), so validation runs the verification with the same
	// interpreter, dependencies and variables as grading. logPath gets the warm-up's commands.
	Checkout func(ctx context.Context, base, module string, verify []string, logPath string) (CheckoutCommands, error)
	// Grader is the mode the verification commands run in (GraderOf: empty is host). In sandbox mode each stage's
	// verification runs through Checkout's Sandboxed, in a grading folder of its own: a validation in sandbox mode needs
	// Checkout. Setup commands run on the host in either mode, as a run's setup does.
	Grader string
	// checkout is what Checkout returned, for this validation's commands.
	checkout CheckoutCommands
}

// Validate checks spec in each arm, stopping an arm at its first stage that does not behave as required.
func (v Validator) Validate(ctx context.Context, spec Spec, arms []Arm) (Validation, error) {
	v.Module = spec.Module // the task's module, whatever the project's setting is now
	result := Validation{Status: StatusValid, Arms: arms, At: v.Now().UTC(), Toolchain: maps.Clone(v.Toolchain), Grader: GraderOf(v.Grader)}
	if !KnownGrader(v.Grader) {
		return Validation{}, fmt.Errorf("grader %s: this Agentium grades on the host or in %s", v.Grader, GraderSandbox)
	}
	if v.Repeats > 1 {
		result.Repeats = v.Repeats
	}
	if spec.Solution == "" || len(spec.HiddenTests) == 0 || len(spec.Reference) == 0 {
		result.Status = StatusUnchecked
	}
	if err := os.MkdirAll(v.LogDir, 0o700); err != nil {
		return Validation{}, fmt.Errorf("validation logs: %w", err)
	}
	if v.Checkout != nil {
		cc, err := v.Checkout(ctx, spec.Base, spec.Module, spec.Verify, filepath.Join(v.LogDir, "warm.log"))
		if err != nil {
			return Validation{}, fmt.Errorf("prepare the build tools: %w", err)
		}
		v.checkout, result.Notes = cc, cc.Notes
		if v.sandboxed() && cc.Sandboxed == nil {
			return Validation{}, errors.New("validation in the sandbox: the build tools' preparation offers no sandboxed commands")
		}
		for _, n := range cc.Notes {
			if v.Progress != nil {
				fmt.Fprintln(v.Progress, "  note: "+n)
			}
		}
	} else if v.sandboxed() {
		return Validation{}, errors.New("validation in the sandbox needs the build tools' preparation (Validator.Checkout)")
	}
	var solution source.Source
	if spec.Solution != "" {
		var err error
		if solution, err = source.Commit(ctx, spec.Solution, "--git-dir", v.Bare); err != nil {
			return Validation{}, err
		}
		if result.Warnings, err = v.filteredHiddenTests(ctx, spec, solution); err != nil {
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
	if v.sandboxed() {
		result.Harmless = harmlessOf(result.Stages)
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

// filteredHiddenTests warns of the hidden Go tests the verify commands keep from running (FilteredHiddenTests), and
// prints each warning to Progress. The base is read only when a verify command filters Go tests.
func (v Validator) filteredHiddenTests(ctx context.Context, spec Spec, solution source.Source) ([]string, error) {
	if !slices.ContainsFunc(spec.Verify, func(c string) bool { return slices.ContainsFunc(goTestFilters(c), goTestFilter.filtered) }) {
		return nil, nil
	}
	base, err := source.Commit(ctx, spec.Base, "--git-dir", v.Bare)
	if err != nil {
		return nil, err
	}
	warnings := FilteredHiddenTests(spec.Verify, spec.HiddenTests, base, solution)
	for _, w := range warnings {
		if v.Progress != nil {
			fmt.Fprintln(v.Progress, v.Style.Warn("  warning: "+w))
		}
	}
	return warnings, nil
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
	for i, s := range plan {
		stage, err := v.runStageRepeated(ctx, spec, arm, s.name, s.want, snap, overlay, solution, s.files)
		stages = append(stages, stage)
		if err != nil {
			return stages, overlay.HarnessChanged, kept, err
		}
		// A hidden-tests stage that failed as wanted but with flagged sandbox denials goes on to the reference stage:
		// denials the reference logs too while passing are harmless for this task (harmlessFailure).
		if !stage.OK && !(s.name == StageHiddenTests && flaggedOnly(stage) && i+1 < len(plan)) {
			break
		}
	}
	if len(stages) == 2 && harmlessFailure(stages[0], stages[1]) {
		stages[0].OK, stages[0].Flaky = true, false
		if stages[0].Runs > 0 {
			stages[0].OKRuns = stages[0].Runs
		}
		if v.Progress != nil {
			fmt.Fprintf(v.Progress, "  %-10s %-13s %s\n", arm.Name, StageHiddenTests,
				v.Style.Status("ok: its sandbox denials are the reference's too, which passed with them"))
		}
	}
	sort.Strings(kept)
	return stages, overlay.HarnessChanged, kept, nil
}

// stageRuns are a stage's runs: the repeats it folded, or the stage itself.
func stageRuns(s Stage) []Stage {
	if len(s.repeats) > 0 {
		return s.repeats
	}
	return []Stage{s}
}

// flaggedOnly reports whether a stage is not OK only for flagged sandbox denials: every run failed as a hidden-tests
// stage wants, without a timeout or a setup failure, and each run that is not OK for all that logged denials the
// agent's sandbox does not impose (with repeats, such runs are what made the stage flaky).
func flaggedOnly(s Stage) bool {
	if s.OK || s.Want != "fail" {
		return false
	}
	flagged := false
	for _, r := range stageRuns(s) {
		if r.Passed || r.SetupFailed || timedOut(r.Commands) {
			return false
		}
		if !r.OK {
			if !r.Sandbox.FlaggedFailure(false) {
				return false
			}
			flagged = true
		}
	}
	return flagged
}

// harmlessFailure reports whether hidden, a hidden-tests stage not OK only for its flagged denials (flaggedOnly),
// failed for its tests after all: the reference stage passed (OK) with every one of the denials of each of hidden's runs
// that is not OK, all of them listed (a list cut at MaxDenials proves nothing about the rest). Then they are the task's
// toolchain's, not the sandbox's doing.
func harmlessFailure(hidden, reference Stage) bool {
	if hidden.Stage != StageHiddenTests || reference.Stage != StageReference || !flaggedOnly(hidden) || !reference.OK || !reference.Passed {
		return false
	}
	seen := harmlessOf([]Stage{reference})
	for _, r := range stageRuns(hidden) {
		if r.OK {
			continue
		}
		listed := 0
		for _, d := range r.Sandbox.Flagged {
			listed += max(d.Repeats, 1)
			if !slices.Contains(seen, KeyOf(d)) {
				return false
			}
		}
		if listed != r.Sandbox.FlaggedCount {
			return false
		}
	}
	return true
}

// harmlessOf is the flagged denials the passing reference stages logged (in every run of each, and so in every arm's),
// each once, in stage order: shown harmless for the task (Validation.Harmless).
func harmlessOf(stages []Stage) []DenialKey {
	var keys []DenialKey
	for _, s := range stages {
		if s.Stage != StageReference || !s.OK || !s.Passed {
			continue
		}
		for _, r := range stageRuns(s) {
			if r.Sandbox == nil {
				continue
			}
			for _, d := range r.Sandbox.Flagged {
				if k := KeyOf(d); !slices.Contains(keys, k) {
					keys = append(keys, k)
				}
			}
		}
	}
	return keys
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
	stage.repeats = runs
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
		defer func() {
			os.RemoveAll(dir)
			if v.checkout.Removed != nil {
				v.checkout.Removed(dir)
			}
		}()
	}
	if snap != nil { // Deletes are not in the snapshot, so Write removes them.
		if err := checkout.Write(dir, snap, append(slices.Clone(overlay.Writes), overlay.Deletes...)); err != nil {
			return stage, fmt.Errorf("arm %s: %w", arm.Name, err)
		}
	}
	if _, err := buildtool.ModuleDir(dir, spec.Module); err != nil { // before setup or anything else runs
		return stage, fmt.Errorf("arm %s: %w", arm.Name, err)
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
	if stage.Commands, stage.Passed, stage.Sandbox, err = v.verify(ctx, log, dir, label, spec.Verify); err != nil {
		return stage, err
	}
	stage.OK = stage.Passed == want
	if !want && timedOut(stage.Commands) { // a timeout is not the failure hidden tests must cause
		stage.OK = false
	}
	if stage.Sandbox.FlaggedFailure(stage.Passed) { // the failure may be the sandbox's, not the tests'
		stage.OK = false
	}
	return stage, nil
}

// sandboxed reports whether the verification runs in the grading sandbox.
func (v Validator) sandboxed() bool { return GraderOf(v.Grader) != GraderHost }

// verify runs the verification commands in the checkout dir: on the host (run), or in the sandbox, in a grading folder
// of the stage's own (label) beside the checkouts, which Sandboxed moves the checkout into and removes with it (the
// checkout comes back to dir when Keep is set).
func (v Validator) verify(ctx context.Context, log io.Writer, dir, label string, commands []string) ([]Command, bool, *SandboxGrade, error) {
	if !v.sandboxed() {
		results, ok, err := v.run(ctx, log, dir, commands)
		return results, ok, nil, err
	}
	root := filepath.Join(filepath.Dir(v.WorkDir), "grading", label)
	return v.checkout.Sandboxed(ctx, dir, root, v.Keep, commands, v.Timeout, log)
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
	case !stage.OK && stage.Sandbox.FlaggedFailure(stage.Passed):
		verdict = "NOT OK (sandbox denials: " + stage.Sandbox.FlaggedOperations() + ")"
	case !stage.OK:
		verdict = "NOT OK"
	}
	line := fmt.Sprintf("  %-10s %-13s want %-4s got %-4s %s", stage.Arm, stage.Stage, stage.Want, got, v.Style.Status(verdict))
	if repeats > 1 {
		line += fmt.Sprintf(" (%d/%d)", repeat, repeats)
	}
	fmt.Fprintln(v.Progress, line)
}

// inModule is the module's folder in dir by name alone, for detecting its build tools; commands run through
// buildtool.ModuleDir, which checks the folder.
func (v Validator) inModule(dir string) string {
	return filepath.Join(dir, filepath.FromSlash(v.Module))
}

// envFor is Env plus the caches of the build tools the checkout in dir uses (see Cache), then what the warmed tools add
// in dir (Checkout: Python's venv).
func (v Validator) envFor(dir string) []string {
	env := v.Env
	if v.Cache != "" {
		env = append(slices.Clone(v.Env), buildtool.CommandEnvFor(buildtool.Select(buildtool.DetectIn(v.inModule(dir))), v.Cache)...)
	}
	if v.checkout.Env != nil {
		env = append(slices.Clone(env), v.checkout.Env(dir)...)
	}
	return env
}

// run runs commands in dir, logging to log, until one fails; ok is whether all of them passed.
func (v Validator) run(ctx context.Context, log io.Writer, dir string, commands []string) (results []Command, ok bool, err error) {
	workDir, err := buildtool.ModuleDir(dir, v.Module) // an error here is Agentium's, before anything runs
	if err != nil {
		return nil, false, fmt.Errorf("validation: %w", err)
	}
	for _, command := range commands {
		fmt.Fprintf(log, "$ %s\n", command)
		result, err := runner.Run(ctx, runner.Spec{Dir: workDir, Command: command, Timeout: v.Timeout, Output: log, Env: v.envFor(dir),
			Environ: v.checkout.Environ})
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
		case !stage.OK && stage.Sandbox.FlaggedFailure(stage.Passed):
			failed = append(failed, fmt.Sprintf("%s/%s failed with sandbox denials the agent's sandbox does not impose (%s)", stage.Arm, stage.Stage,
				stage.Sandbox.FlaggedOperations()))
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
