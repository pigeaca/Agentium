// Package run executes one agent run on a task and grades it. The agent works in a workspace prepared as the arm: a
// checkout of the task's base holding only that commit, the arm's context, the task's setup, then a context commit the
// agent's changes are measured from. Claude Code runs isolated (internal/claude) and denied everything else in the
// data folder and the user's repository. Grading happens afterwards on a copy in the run's records, which the agent
// could never read: the hidden tests are added there and the verification commands run.
package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// Spec is what to run.
type Spec struct {
	TaskName    string
	Instruction string
	Task        task.Spec // base, solution, hidden tests, reference, setup, verification
	Arm         task.Arm
	Model       string
	Effort      string
	BudgetUSD   float64
	Timeout     time.Duration // the agent's run
	Keep        bool          // keep the workspace and the verification copy
	PlainPrompt bool          // send Instruction as it is, without the task suffix (calibration)
	// Probe, when set, is appended to the arm's startup instruction file (CLAUDE.md, .claude/CLAUDE.md or AGENTS.md, as
	// the resolver finds it) before the context commit: calibration asks the agent to repeat it, which proves that
	// file really loads.
	Probe string
	// Judge, when set, has the judge give a graded run a verdict (Env.Judge), after grading and inside the run, so the
	// experiment's concurrency bounds the calls too. It never changes the run's outcome or result.
	Judge *judge.Settings
}

// Env is what a run needs from Agentium and the machine.
type Env struct {
	ID            string // from NewID
	Layout        home.Layout
	Bare          string // the project's bare repository
	ProjectRoot   string // the user's repository: the agent may not read it
	CLI           string // the claude executable
	Home          string
	AccountHome   string   // the account's home folder in the user database, when known (claude.Invocation.AccountHome)
	Environ       []string // the parent's environment; the run gets an allowlisted part
	SignIn        string   // claude.SignInAPIKey, SignInTokenFile or SignInLogin
	Secret        string   // for API key and token sign-in; redacted from every record
	TokenFile     string
	VerifyTimeout time.Duration // each setup or verification command
	Grace         time.Duration // between SIGINT and SIGKILL when the agent is stopped
	// Expect is the arm's calibrated environment (CLI version, model, tools, skills, slash commands); its personal and
	// project skills are filled in by the run.
	Expect   claude.Expect
	Progress io.Writer
	Style    term.Style // styles the progress lines' outcomes; the zero Style prints plain text
	// Step, when set, is called as each step begins (preparing the workspace, Claude Code working, grading), so a
	// caller can show what is in progress. It only feeds a status display and must not print.
	Step func(step string)
	Now  func() time.Time
	// Workspace names the run's folder under Layout.Workspaces (default: ID). Experiments name it by slot and try, so
	// runs that may overlap can deny each other's folders before they exist (see Predicted).
	Workspace string
	// DenyExtra adds paths the agent may not read: the predicted folders of runs that may overlap it.
	DenyExtra []string
	// Meta is kept in the run's start file and returned by Recover: what the caller needs to store a run whose
	// Agentium process died (its project and experiment slot, say).
	Meta json.RawMessage
	// AllowLocalBinding is the project's opt-in for the sandbox's local binding (store.Project): a run on a Gradle
	// project does not start without it (claude.LocalBindingRefusal).
	AllowLocalBinding bool
	// WarmWait bounds the wait for another warm-up of the project's dependencies; zero: DefaultWarmWait.
	WarmWait time.Duration
	// CommandEnv is added to the setup and verification commands' environment (BuildEnv); setup also gets the agent's
	// own build caches (buildtool.AgentCacheEnv: Go's GOCACHE).
	CommandEnv []string
	// checkoutEnv, set by Once once the run's tools are warmed, is what Agentium's own commands in a checkout (dir) add
	// to CommandEnv: buildtool.CheckoutEnv, Python's venv.
	checkoutEnv func(dir string) []string
	// checkoutBase is the base environment of those commands (buildtool.CheckoutEnviron), and commandBase the one
	// commands use (nil: the process's, filtered by runner.Environ); only setup and grading set it.
	checkoutBase, commandBase []string
	// judgeSpent, when set, learns what a judgement has spent so far (its earlier verdict's included) after each call
	// that reported a cost: Once keeps it in the start file, so a crash while judging loses none of it.
	judgeSpent func(usd float64)
}

// CheckBuildConfigs reports whether the user's build configuration can be read safely (see buildtool.ProjectCaches): an
// experiment asks before it locks, since every run would refuse to start otherwise.
func (env Env) CheckBuildConfigs(ctx context.Context) error {
	return buildtool.CheckConfigs(env.Environ, env.Home, env.repositoryPaths(ctx))
}

// BuildEnv points the caches and temporary files of the commands Agentium runs itself (setup, validation, grading) into
// the data folder, which agents may not read, and creates it: in the user's own folders they would leave compiled
// hidden tests for agents to read (Go's build cache and its temporary builds, Jest's cache in TMPDIR). The build tools'
// profiles say which variables (buildtool.CommandEnv: Go's, for every project). Once a run or validation knows its
// repository's build tools, their caches are added (buildtool.CommandEnvFor: Maven's and Gradle's under the cache
// folder, rustc wrappers such as sccache cleared). Caches no profile knows (Bazel's output base) stay where their tools
// keep them.
func BuildEnv(layout home.Layout) ([]string, error) {
	tmp := filepath.Join(layout.Cache, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("build cache: %w", err)
	}
	if err := buildtool.PrepareCommands(layout.Cache); err != nil {
		return nil, fmt.Errorf("build cache: %w", err)
	}
	return buildtool.CommandEnv(layout.Cache, tmp), nil
}

// Record is a finished run.
type Record struct {
	ID       string `json:"id"`
	Task     string `json:"task"`
	Arm      string `json:"arm"`
	Snapshot string `json:"snapshot,omitempty"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"` // as asked for (--effort); empty: the CLI's default
	// EffortRecorded marks a record made when runs recorded their effort: without it an empty Effort is unknown (an
	// older record), with it the CLI's default.
	EffortRecorded bool           `json:"effort_recorded,omitempty"`
	SignIn         string         `json:"sign_in"`
	Outcome        string         `json:"outcome"`          // claude.Outcome*
	Passed         *bool          `json:"passed,omitempty"` // the verification with hidden tests; nil when it did not run
	Drift          []string       `json:"drift,omitempty"`
	Notes          []string       `json:"notes,omitempty"`
	Metrics        claude.Metrics `json:"metrics"`
	Behavior       Behavior       `json:"behavior"`
	Setup          []task.Command `json:"setup,omitempty"`
	Verify         []task.Command `json:"verify,omitempty"`
	ExitCode       int            `json:"exit_code"`
	// WarmWait: the run ended as an infrastructure failure because it waited out another run's dependency warm-up
	// (Once); an experiment does not count it toward an outage.
	WarmWait    bool      `json:"warm_wait,omitempty"`
	Started     time.Time `json:"started"`
	Finished    time.Time `json:"finished"`
	RecordsDir  string    `json:"records"`
	ContextHead string    `json:"context_commit,omitempty"`
	ProbeFile   string    `json:"probe_file,omitempty"` // the instruction file Spec.Probe was added to
	// ContextUse is what the run used of its arm's context; nil in records made before Agentium kept it, and in runs
	// that ended before their transcript could be read.
	ContextUse *ContextUse `json:"context_use,omitempty"`
	// CostEstimated: Claude Code reported no cost, so Metrics.CostUSD prices the transcript's requests at list prices.
	// Read costs through Spend, which keeps the agent's and the judge's apart.
	CostEstimated bool `json:"cost_estimated,omitempty"`
	// IsolatedCostUSD is the run's cost had no other run warmed the prompt cache: Metrics.CostUSD with the first-request
	// cache reads of Metrics.FirstReads repriced as cache writes (isolatedCost). Actual cost (Spend) stays the primary
	// metric; this is a counterfactual beside it, not spend. Nil when it cannot be computed and in records made before
	// it existed: absent, never zero.
	IsolatedCostUSD *float64 `json:"isolated_cost_usd,omitempty"`
	// Recovered says how a run left behind by a dead Agentium process was stored: RecoveredStopped (it was cut short,
	// and is cancelled) or RecoveredFinished (it had finished).
	Recovered string `json:"recovered,omitempty"`
	// HarnessChanged lists what the arm's context changes that runs (hooks, settings, MCP), not only what the agent reads.
	HarnessChanged []string `json:"harness_changed,omitempty"`
	// ProjectSkills and ProjectCommands are the arm's own skill and command names at the context commit; calibration
	// subtracts them to keep only what Claude Code bundles.
	ProjectSkills   []string `json:"-"`
	ProjectCommands []string `json:"-"`
	// Judge is the judge's verdict on a graded run, when its experiment asked for one: a second opinion beside Passed
	// that decides nothing. Its cost is kept here, apart from Metrics.CostUSD, which stays the agent's alone (Spend).
	Judge *judge.Verdict `json:"judge,omitempty"`
}

// Behavior is what the agent did, beyond passing or failing.
type Behavior struct {
	FilesChanged int  `json:"files_changed"`
	LinesAdded   int  `json:"lines_added"`
	LinesRemoved int  `json:"lines_removed"`
	TestsChanged bool `json:"tests_changed"` // changed a test file
	TestsRemoved int  `json:"tests_removed"` // test files deleted
	// ChecksChanged are verification scripts and test-runner configuration the agent changed; scripts the task did not
	// need changed were restored before grading.
	ChecksChanged []string `json:"checks_changed,omitempty"`
	// ConfigChanged is the test-runner configuration among them that the reference solution does not change: it was
	// graded as the agent left it, so a pass with it is not counted as a success.
	ConfigChanged []string `json:"config_changed,omitempty"`
	RanTests      bool     `json:"ran_tests"`  // ran a test runner
	RanChecks     bool     `json:"ran_checks"` // ran one of the task's verification commands
	Commits       int      `json:"commits"`    // commits on top of the context commit
	BashCommands  int      `json:"bash_commands"`
	Denials       int      `json:"denials"`
	OutsideReads  int      `json:"outside_reads"` // file tool calls on Agentium's data, the user's repository or Claude's data
}

// NewID makes a run id: a UTC timestamp and a random suffix, so ids sort by start time.
func NewID(now time.Time) (string, error) {
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("run id: %w", err)
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix), nil
}

// suffix tells the agent how to work in the run's checkout.
const suffix = "\n\nYou are working in this task's own checkout of the repository. Make the change here, in the working " +
	"tree. Do not commit, push, open a pull request, or create branches or worktrees. When you are done, reply with a " +
	"short summary of what you changed and how you verified it."

// Once runs spec once and grades it. The error is for runs that could not be carried out (Agentium's own setup,
// cancellation); an agent's failure is a Record. A cancelled run still returns its record, with what the transcript
// shows it spent.
func Once(ctx context.Context, env Env, spec Spec) (rec Record, err error) {
	rec = Record{ID: env.ID, Task: spec.TaskName, Arm: spec.Arm.Name, Snapshot: spec.Arm.Snapshot, Model: spec.Model, Effort: spec.Effort, EffortRecorded: true,
		SignIn: env.SignIn, Started: env.Now().UTC(), RecordsDir: filepath.Join(env.Layout.Records, env.ID)}
	workspace := filepath.Join(env.Layout.Workspaces, env.workspaceName())
	tempRoot := env.Layout.RunTemp(env.workspaceName()) // Claude Code's temp root for the agent (see temp.go)
	repo := filepath.Join(workspace, "repo")
	graded := filepath.Join(rec.RecordsDir, "verify") // Agentium's own repository of the context commit, for grading
	// The start file follows the run (see Recover): whether the agent started, the process group of whatever runs now
	// (setup, the agent, verification), and at the end the finished record, so a runner killed before storing it
	// loses nothing.
	var agentStarted, recordsReady bool
	// stopTools ends what the build tools left running (Gradle daemons) once the agent is done; set when the run's tools
	// are known, called when the agent ends and again, harmlessly, at the very end.
	var stopTools func()
	var pgid int
	var startErr error
	writeStart := func(finished bool) error {
		return env.writeStart(start{Record: rec, Workspace: workspace, AgentStarted: agentStarted, PGID: pgid, Finished: finished, Meta: env.Meta})
	}
	running := func(pid int) {
		pgid = pid
		if err := writeStart(false); err != nil && startErr == nil {
			startErr = err
		}
	}
	defer func() {
		rec.Finished = env.Now().UTC()
		if recordsReady {
			if startErr := writeStart(true); startErr != nil && err == nil {
				err = startErr
			}
		}
		if redactErr := env.redactRecords(rec.RecordsDir); redactErr != nil && err == nil {
			err = redactErr
		}
		if stopTools != nil {
			stopTools()
		}
		if !spec.Keep {
			os.RemoveAll(workspace)
			os.RemoveAll(graded)
		}
		// Even a kept run's temp root goes: it holds only Claude Code's own temp files, in a folder shared with other users.
		if tempRoot != "" {
			if tempErr := removeRunTemp(tempRoot); tempErr != nil && err == nil {
				err = tempErr
			}
		}
	}()
	if tempRoot == "" {
		return rec, errors.New("the data folder's layout names no folder for the runs' temp roots")
	}
	if found := claudectx.InstructionFilesAbove(repo); len(found) > 0 {
		return rec, fmt.Errorf("%s: Claude Code would load it into every run from above the workspace; move it, or set AGENTIUM_HOME elsewhere", strings.Join(found, ", "))
	}
	// The build tools come from the task's base commit, not the checkout: an arm's snapshot cannot add a build file and so
	// change one arm's sandbox, warm-up or environment. A run that needs the user's opt-in for local binding stops here,
	// before it costs anything.
	// Where Python code imports from is decided here too, once: the agent, setup and grading agree on it whatever the
	// agent adds or removes under src/.
	tools, importRoot, err := baseLayout(ctx, env.Bare, spec.Task.Base)
	if err != nil {
		return rec, err
	}
	if err := claude.LocalBindingRefusal(tools, env.AllowLocalBinding); err != nil {
		return rec, err
	}
	prompt := spec.Instruction + suffix
	if spec.PlainPrompt {
		prompt = spec.Instruction
	}
	inv := claude.Invocation{CLI: env.CLI, Dir: repo, Prompt: prompt, Model: spec.Model, Effort: spec.Effort,
		BudgetUSD: spec.BudgetUSD, SignIn: env.SignIn, Secret: env.Secret, TokenFile: env.TokenFile, Home: env.Home,
		AccountHome: env.AccountHome, TempRoot: tempRoot, UID: os.Getuid()}
	deny, err := env.denied(ctx, workspace)
	if err != nil {
		return rec, err
	}
	inv.Deny = append(deny, env.DenyExtra...)
	if env.SignIn != claude.SignInLogin {
		inv.ConfigDir = filepath.Join(workspace, "config")
	}
	// The run's own build cache: nothing compiled before it, nothing after. Its name predates the build-tool profiles
	// and stays, so a Go run's folders are unchanged.
	inv.BuildCache = filepath.Join(workspace, "go-build")
	// A denied path that holds the workspace would hide the agent's own checkout from it: every run would fail for a
	// reason that is not the agent's. So would one that holds its temp root.
	deniedPaths := inv.DeniedPaths(env.Environ)
	if denied, ok := insideDenied(workspace, deniedPaths); ok {
		return rec, fmt.Errorf("the run's workspace %s lies inside %s, which runs may not read: set AGENTIUM_HOME (or the token file) elsewhere", workspace, denied)
	}
	if denied, ok := insideDenied(tempRoot, deniedPaths); ok {
		return rec, fmt.Errorf("the run's temp root %s lies inside %s, which runs may not read", tempRoot, denied)
	}
	if err := claude.TempRootFits(tempRoot, inv.UID); err != nil {
		return rec, err
	}
	if err := os.MkdirAll(rec.RecordsDir, 0o700); err != nil {
		return rec, fmt.Errorf("run records: %w", err)
	}
	if err := writeStart(false); err != nil {
		return rec, err
	}
	recordsReady = true
	// An experiment slot's retry reuses its workspace name; a dead process's unreadable run left it behind unstored
	// (Recover could not find it). The caller holds the run lock, so nothing else uses it.
	if err := removeStaleWorkspace(env.Layout.Workspaces, workspace, tempRoot); err != nil {
		return rec, err
	}
	for _, dir := range []string{workspace, inv.ConfigDir, inv.BuildCache} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return rec, fmt.Errorf("run folder %s: %w", filepath.Base(dir), err)
		}
	}
	// Resolved once, before the agent starts, and only this string goes to the stop hook: the sandbox lets the agent
	// write the build cache path itself, so later it could replace the folder with a link to another run's, and
	// anything resolved at stop time would follow it.
	buildCacheReal, err := filepath.EvalSymlinks(inv.BuildCache)
	if err != nil {
		return rec, fmt.Errorf("run folder go-build: %w", err)
	}
	// After the start file, so a dead process's root is found and removed (Recover); before the agent's settings are
	// made, so the root's resolved form (/private/tmp) is known.
	if err := makeRunTemp(tempRoot); err != nil {
		return rec, err
	}
	env.step("preparing the workspace")
	env.progress("%s", env.Style.Heading(fmt.Sprintf("Run %s: task %s, arm %s, model %s, sign-in %s", env.ID, spec.TaskName, spec.Arm.Name, spec.Model, env.SignIn)))

	// The workspace: the base, the arm's context, the setup, then the context commit.
	if err := checkout.New(ctx, env.Bare, spec.Task.Base, repo); err != nil {
		return rec, err
	}
	if spec.Arm.Snapshot != "" {
		base, err := source.Commit(ctx, spec.Task.Base, "--git-dir", env.Bare)
		if err != nil {
			return rec, err
		}
		snap, err := source.Commit(ctx, spec.Arm.Snapshot, "--git-dir", env.Bare)
		if err != nil {
			return rec, err
		}
		overlay, err := snapshot.PlanOverlay(base, snap)
		if err != nil {
			return rec, fmt.Errorf("arm %s: %w", spec.Arm.Name, err)
		}
		if err := checkout.Write(repo, snap, append(overlay.Writes, overlay.Deletes...)); err != nil {
			return rec, fmt.Errorf("arm %s: %w", spec.Arm.Name, err)
		}
		if len(overlay.HarnessChanged) > 0 {
			rec.HarnessChanged = overlay.HarnessChanged
			rec.Notes = append(rec.Notes, "the arm changes what runs: "+strings.Join(overlay.HarnessChanged, ", "))
		}
	}
	// The repository's build tools (profiles) choose the agent's environment and sandbox, add their caches to the
	// environment of Agentium's own commands, and warm the dependencies the agent will read.
	inv.Tools, inv.Deps, inv.AllowLocalBinding = tools, env.depsFolder(), env.AllowLocalBinding
	profiles := buildtool.Select(inv.Tools)
	if slices.Contains(inv.Tools, "maven") || slices.Contains(inv.Tools, "gradle") {
		inv.JavaHome = buildtool.ResolveJavaHome(ctx, env.Environ, buildtool.CommandOutput)
	}
	if env.Layout.Cache != "" {
		env.CommandEnv = append(slices.Clone(env.CommandEnv), buildtool.CommandEnvFor(profiles, env.Layout.Cache)...)
	}
	stopped := false
	stopTools = func() {
		if stopped {
			return
		}
		stopped = true
		// Even a cancelled run stops what it started, within half a minute.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if stopErr := buildtool.StopRun(stopCtx, profiles, buildCacheReal, buildtool.SystemHost()); stopErr != nil {
			rec.Notes = append(rec.Notes, "a build tool could not be stopped: "+stopErr.Error())
		}
	}
	warmed, notes, err := env.prepareTools(ctx, profiles, inv, spec.Task.Base, filepath.Join(rec.RecordsDir, "setup.log"), running)
	rec.Notes = append(rec.Notes, notes...)
	// What the warm-up found (Python's venv and the project's metadata) goes to the agent, and to Agentium's own commands in a checkout: the task's
	// setup in the run's, grading in its copy, with their caches in the data folder.
	// They also lose the user's variables the agent never gets (buildtool.CheckoutEnviron: PYTHON*, PIP_*, UV_*), so the
	// tests run with the same settings for the agent and for grading.
	inv.Venv, inv.ProjectMetadata, inv.ImportRoot = warmed.Venv, warmed.Metadata, importRoot
	env.checkoutEnv = func(dir string) []string {
		return buildtool.CheckoutEnv(profiles, buildtool.AgentContext{Allowed: env.environ(), Environ: env.environ(), Home: env.Home, Repo: dir,
			BuildCache: env.Layout.Cache, Deps: inv.Deps, Venv: warmed.Venv, Metadata: warmed.Metadata, ImportRoot: importRoot})
	}
	env.checkoutBase = runner.Environ(buildtool.CheckoutEnviron(profiles, env.environ()))
	rec.Notes = append(rec.Notes, buildtool.MissingRunners(ctx, warmed.Venv, spec.Task.Verify, env.environ())...)
	if errors.Is(err, errWarmWait) {
		// The dependencies were not warmed and the agent would build without them: not the arm's doing, so the run is
		// not counted against it (an infrastructure failure is retried or left out).
		rec.Outcome, rec.WarmWait = claude.OutcomeInfra, true
		rec.Notes = append(rec.Notes, err.Error())
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	if len(spec.Task.Setup) > 0 {
		// Setup builds into the agent's own cache, so a warming step (`go build ./...`) spares every agent a cold
		// build; the workspace holds no hidden tests yet.
		setup := env
		setup.CommandEnv = append(slices.Clone(env.CommandEnv), buildtool.AgentCacheEnv(profiles, inv.BuildCache)...)
		setup.CommandEnv = append(setup.CommandEnv, env.checkoutEnv(repo)...)
		setup.commandBase = env.checkoutBase
		var ok bool
		if rec.Setup, ok, err = setup.commands(ctx, repo, spec.Task.Setup, filepath.Join(rec.RecordsDir, "setup.log"), running); err != nil {
			return rec, err
		}
		if !ok {
			rec.Outcome = claude.OutcomeInfra
			rec.Notes = append(rec.Notes, "setup failed: see setup.log")
			return rec, nil
		}
	}
	if spec.Probe != "" {
		if rec.ProbeFile, err = appendProbe(ctx, repo, spec.Probe); err != nil {
			return rec, err
		}
		if rec.ProbeFile == "" {
			rec.Notes = append(rec.Notes, "the arm loads no instruction file at start, so the codeword probe was skipped")
		}
	}
	// Setup outputs that git does not ignore are part of the starting point, not the agent's work.
	if _, err := gitx.Run(ctx, "-C", repo, "add", "-A"); err != nil {
		return rec, err
	}
	if _, err := gitx.Run(ctx, "-C", repo, "-c", "user.name=agentium", "-c", "user.email=agentium@localhost", "-c", "commit.gpgsign=false",
		"commit", "--quiet", "--allow-empty", "--no-verify", "-m", "agentium: context "+spec.Arm.Name); err != nil {
		return rec, err
	}
	if rec.ContextHead, err = gitx.Run(ctx, "-C", repo, "rev-parse", "HEAD"); err != nil {
		return rec, err
	}
	// Grading never trusts the agent's .git (its config could name filters that run outside the sandbox): the context
	// commit is copied now, before the agent starts, into a repository of Agentium's own.
	if err := checkout.New(ctx, repo, rec.ContextHead, graded); err != nil {
		return rec, fmt.Errorf("grading repository: %w", err)
	}

	// The agent.
	transcriptPath := filepath.Join(rec.RecordsDir, "stream.jsonl")
	transcript, err := os.Create(transcriptPath)
	if err != nil {
		return rec, fmt.Errorf("run transcript: %w", err)
	}
	stderr, err := os.Create(filepath.Join(rec.RecordsDir, "stderr.txt"))
	if err != nil {
		transcript.Close()
		return rec, fmt.Errorf("run transcript: %w", err)
	}
	activeConfig := inv.ConfigDir
	if env.SignIn == claude.SignInLogin {
		activeConfig = claude.UserConfigDir(env.Environ, env.Home)
	}
	// Claude Code keeps the run's session, with its saved large outputs, in a folder named after the checkout. Reads
	// there are the run's own; any other session folder, even one created during the run, is someone else's.
	ownSession := claude.SessionFolder(activeConfig, repo)
	pastSessions := claude.SessionFolders(activeConfig)
	agentStarted, pgid = true, 0
	if err := writeStart(false); err != nil {
		transcript.Close()
		stderr.Close()
		return rec, err
	}
	inv.Started = running
	env.step("Claude Code is working")
	env.progress("  workspace ready; Claude Code is working (up to %s)", spec.Timeout)
	result, runErr := claude.Run(ctx, inv, env.Environ, transcript, stderr, spec.Timeout, env.Grace)
	if runErr == nil && startErr != nil {
		runErr = startErr
	}
	transcript.Close()
	stderr.Close()
	stopTools() // before grading: a daemon would sit on its heap meanwhile
	rec.ExitCode = result.ExitCode
	// From here on the agent has run and may have spent money: any error still leaves a record with an outcome.
	unfinished := func(err error) (Record, error) {
		switch {
		case ctx.Err() != nil: // interrupted, even during grading: the run is not usable, and not the agent's failure
			rec.Outcome, rec.Passed = claude.OutcomeCancelled, nil
		default: // Agentium's own failure, even after a fair attempt (grading failed): not the agent's result
			rec.Outcome, rec.Passed = claude.OutcomeInfra, nil
		}
		rec.Notes = append(rec.Notes, "Agentium could not finish the run: "+err.Error())
		return rec, err
	}
	var parseErr error
	rec.Metrics, parseErr = parseFile(transcriptPath)               // partial metrics are kept even when reading fails
	if !rec.Metrics.SawResult && rec.Metrics.EstimatedCostUSD > 0 { // stopped before Claude Code's result: still spent
		rec.Metrics.CostUSD = rec.Metrics.EstimatedCostUSD
		rec.CostEstimated = true
		rec.Notes = append(rec.Notes, "Claude Code reported no cost: estimated from the transcript's requests at list prices")
	}
	// The isolated-run cost starts from the cost, so it comes after the cost is settled.
	rec.IsolatedCostUSD = isolatedCost(rec)
	if runErr != nil { // cancelled: keep what the run reported (Claude Code reports its result on SIGINT)
		return unfinished(runErr)
	}
	if parseErr != nil {
		return unfinished(parseErr)
	}
	userConfig := claude.UserConfigDir(env.Environ, env.Home)
	// The context commit (Agentium's grading repository), not the agent's tree, which may have lost its .git.
	armSource, armContext, err := contextAt(ctx, graded)
	if err != nil {
		return unfinished(err)
	}
	rec.ProjectSkills, rec.ProjectCommands = claudectx.SkillNames(armContext, armSource), claudectx.CommandNames(armContext)
	realRepo, _ := filepath.EvalSymlinks(repo) // Claude Code may name files under the resolved path (/private/var)
	use := UseOf(armContext, armSource, rec.Metrics, repo, realRepo, rec.Metrics.CWD)
	rec.ContextUse = &use
	expect := env.Expect
	expect.PersonalSkills, expect.ProjectSkills = claude.PersonalSkills(userConfig), rec.ProjectSkills
	// A calibration holds only what Claude Code bundles: the arm's own skills and commands at its base are added here.
	if expect.Skills != nil {
		expect.Skills = union(expect.Skills, rec.ProjectSkills)
	}
	if expect.SlashCommands != nil {
		expect.SlashCommands = union(expect.SlashCommands, rec.ProjectSkills, rec.ProjectCommands)
	}
	rec.Drift = claude.Check(rec.Metrics, expect)
	watched := append([]string{env.Layout.Root, filepath.Join(env.Home, ".claude"), userConfig}, env.repositoryPaths(ctx)...)
	rec.Behavior.OutsideReads = outsideReads(ownSessionExcluded(rec.Metrics.FilePaths, ownSession), repo, workspace, watched)
	if _, err := os.Stat(ownSession); err != nil && len(claude.SessionFolders(activeConfig)) > len(pastSessions) {
		rec.Notes = append(rec.Notes, "Claude Code kept this run's session in an unexpected folder: reads of its saved outputs count as outside reads")
	}
	if rec.Behavior.OutsideReads > 0 {
		rec.Drift = append(rec.Drift, fmt.Sprintf("%d file tool call(s) reached Agentium's data, the repository or Claude's data", rec.Behavior.OutsideReads))
	}
	rec.Outcome = claude.Classify(rec.Metrics, result.TimedOut, rec.Drift)
	env.progress("  Claude Code: %s, $%.2f, %d turn(s)", env.Style.Status(rec.Outcome), rec.Spend().AgentUSD, rec.Metrics.Turns)

	// Grading, only for fair attempts (infra and unfair runs are never counted).
	switch rec.Outcome {
	case claude.OutcomeOK, claude.OutcomeCapped, claude.OutcomeTimeout:
		if err := env.grade(ctx, spec, repo, graded, &rec, running); err != nil {
			return unfinished(err)
		}
		if spec.Judge != nil {
			// The graded run is complete. Judging can take repeats × judge.CallTimeout, so the records are redacted and
			// the start file marked finished first: if Agentium dies while judging, recovery stores the graded run (not a
			// cancelled one) with what the judge spent so far, as a verdict stopped early that a resume judges again.
			if err := env.redactRecords(rec.RecordsDir); err != nil {
				return unfinished(err)
			}
			rec.Finished = env.Now().UTC() // the deferred write sets it again once judged
			if err := writeStart(true); err != nil {
				return unfinished(err)
			}
			settings := spec.Judge.WithDefaults()
			judging := env
			judging.judgeSpent = func(usd float64) {
				partial := rec
				partial.Judge = &judge.Verdict{Version: judge.Version, Answers: []string{}, Reasons: []string{}, Requested: settings.Repeats,
					Model: settings.Model, Effort: settings.Effort, CostUSD: usd, Stopped: judge.StoppedCall,
					Errors: []string{"Agentium stopped while judging"}}
				// Best effort: the final write follows, and a failure here only risks this spend if Agentium also dies.
				_ = env.writeStart(start{Record: partial, Workspace: workspace, AgentStarted: agentStarted, PGID: pgid, Finished: true, Meta: env.Meta})
			}
			env.step("judging")
			judging.Judge(ctx, spec, *spec.Judge, &rec)
		}
	}
	return rec, nil
}

// grade brings the agent's work tree (never its .git) into the grading repository, measures the changes from the
// context commit, restores the verification scripts the task never needed changed, adds the hidden tests and runs the
// verification commands.
func (env Env) grade(ctx context.Context, spec Spec, repo, graded string, rec *Record, running func(pid int)) error {
	env.step("grading")
	unreadable, err := syncWorkTree(repo, graded)
	if err != nil {
		return fmt.Errorf("grading copy: %w", err)
	}
	if len(unreadable) > 0 { // the agent's doing, so its result: graded without them
		rec.Notes = append(rec.Notes, "graded without what the agent left unreadable: "+strings.Join(unreadable, ", "))
	}
	if _, err := gitx.Run(ctx, "-C", graded, "add", "-A"); err != nil {
		return err
	}
	numstat, err := gitx.Output(ctx, nil, "-C", graded, "diff", "--cached", "--numstat", "-z", "--no-renames", rec.ContextHead)
	if err != nil {
		return err
	}
	changed := measure(string(numstat), &rec.Behavior)
	deleted, err := gitx.Output(ctx, nil, "-C", graded, "diff", "--cached", "--name-only", "-z", "--no-renames", "--diff-filter=D", rec.ContextHead)
	if err != nil {
		return err
	}
	for _, p := range strings.Split(string(deleted), "\x00") {
		if p != "" && task.IsTestFile(p) {
			rec.Behavior.TestsRemoved++
		}
	}
	// The patch's form is pinned against the user's git settings (renames, prefixes): the judge reads its file headers.
	patch, err := gitx.Output(ctx, nil, "-C", graded, "-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false", "diff", "--cached",
		"--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", rec.ContextHead)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(rec.RecordsDir, "agent.diff"), patch, 0o600); err != nil {
		return fmt.Errorf("agent diff: %w", err)
	}
	// Commits are read from the agent's repository, which is only read: rev-list runs no filters. --git-dir, so that a
	// removed .git fails here instead of git finding an enclosing repository.
	if commits, err := gitx.Run(ctx, "--git-dir", filepath.Join(repo, ".git"), "rev-list", "--count", rec.ContextHead+"..HEAD"); err == nil {
		rec.Behavior.Commits, _ = strconv.Atoi(commits)
	} else {
		rec.Notes = append(rec.Notes, "the agent's commits could not be counted: its repository was altered")
	}
	rec.Behavior.BashCommands = len(rec.Metrics.Commands)
	rec.Behavior.Denials = rec.Metrics.Denials
	rec.Behavior.RanTests = ranTests(rec.Metrics.Commands)
	rec.Behavior.RanChecks = ranChecks(rec.Metrics.Commands, spec.Task.Verify)
	for _, p := range changed {
		rec.Behavior.TestsChanged = rec.Behavior.TestsChanged || task.IsTestFile(p)
	}

	// The checks themselves: scripts the verification commands name are restored to their version in the context
	// commit (where the agent started, setup included), unless the reference solution changes them too (then changing
	// them is part of the task). Other runner configuration the agent changed is reported.
	start, err := source.Commit(ctx, rec.ContextHead, "-C", graded)
	if err != nil {
		return err
	}
	scripts, configs := checkFiles(spec.Task.Verify, start)
	var restore []string
	for _, p := range changed {
		switch {
		case slices.Contains(scripts, p) && !slices.Contains(spec.Task.Reference, p):
			restore = append(restore, p)
		case slices.Contains(scripts, p) || slices.Contains(configs, p):
			rec.Behavior.ChecksChanged = append(rec.Behavior.ChecksChanged, p)
			if slices.Contains(configs, p) && !slices.Contains(spec.Task.Reference, p) {
				rec.Behavior.ConfigChanged = append(rec.Behavior.ConfigChanged, p)
			}
		}
	}
	if len(restore) > 0 {
		if err := checkout.Write(graded, start, restore); err != nil {
			return fmt.Errorf("restore the checks: %w", err)
		}
		rec.Behavior.ChecksChanged = append(rec.Behavior.ChecksChanged, restore...)
		rec.Notes = append(rec.Notes, "the agent changed the verification's own scripts; graded with the starting version: "+strings.Join(restore, ", "))
	}

	failed := false
	if spec.Task.Solution != "" && len(spec.Task.HiddenTests) > 0 {
		solution, err := source.Commit(ctx, spec.Task.Solution, "--git-dir", env.Bare)
		if err != nil {
			return err
		}
		if err := checkout.Write(graded, solution, spec.Task.HiddenTests); err != nil {
			// The agent turned a hidden test's folder into a link, say: the tests cannot run as written.
			rec.Notes = append(rec.Notes, "the hidden tests could not be added: "+err.Error())
			failed = true
		}
	}
	if !failed {
		var commands []task.Command
		var ok bool
		verify := env
		if env.checkoutEnv != nil {
			verify.CommandEnv = append(slices.Clone(env.CommandEnv), env.checkoutEnv(graded)...)
			verify.commandBase = env.checkoutBase
		}
		commands, ok, err = verify.commands(ctx, graded, spec.Task.Verify, filepath.Join(rec.RecordsDir, "verify.log"), running)
		rec.Verify = commands
		if err != nil {
			return err
		}
		failed = !ok
	}
	passed := !failed
	rec.Passed = &passed
	env.progress("  verification: %s", env.Style.Status(map[bool]string{true: "passed", false: "failed"}[passed]))
	return nil
}

// checkFiles lists what the verification commands depend on in base: the files they name (scripts, which grading
// restores) and the configuration of the test runners they call (reported when changed).
func checkFiles(verify []string, base source.Source) (scripts, configs []string) {
	split := func(r rune) bool { return strings.ContainsRune(" \t\n;&|()<>\"'`", r) }
	// The build tools' runners come from their profiles; the rest are runners without one.
	runners := buildtool.RunnerConfigs()
	for word, files := range map[string][]string{
		"make": {"Makefile", "GNUmakefile"}, "npm": {"package.json"}, "pnpm": {"package.json"}, "yarn": {"package.json"},
		"jest": {"jest.config.js", "jest.config.ts"}, "vitest": {"vitest.config.ts", "vitest.config.js"},
	} {
		runners[word] = append(runners[word], files...)
	}
	for _, command := range verify {
		for _, token := range strings.FieldsFunc(command, split) {
			if p := path.Clean(strings.TrimPrefix(token, "./")); source.Has(base, p) && !slices.Contains(scripts, p) {
				scripts = append(scripts, p)
			}
		}
		for word, files := range runners {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`).MatchString(command) {
				for _, f := range matching(base, files) {
					if !slices.Contains(configs, f) {
						configs = append(configs, f)
					}
				}
			}
		}
	}
	return scripts, configs
}

// matching lists the files of base that names give: a name that exists, or every path a pattern with "*" matches
// (path.Match: within one folder, requirements*.txt never reaches into a subfolder), in base's order.
func matching(base source.Source, names []string) []string {
	var out []string
	for _, name := range names {
		if !strings.Contains(name, "*") {
			if source.Has(base, name) {
				out = append(out, name)
			}
			continue
		}
		for _, p := range base.Paths() {
			if ok, _ := path.Match(name, p); ok {
				out = append(out, p)
			}
		}
	}
	return out
}

// syncWorkTree makes dst's work tree (everything but .git) a copy of src's, and lists what in src could not be read.
func syncWorkTree(src, dst string) (unreadable []string, err error) {
	entries, err := os.ReadDir(dst)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			if err := os.RemoveAll(filepath.Join(dst, e.Name())); err != nil {
				return nil, err
			}
		}
	}
	entries, err = os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			skipped, err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			if err != nil {
				return nil, err
			}
			for _, p := range skipped {
				unreadable = append(unreadable, filepath.ToSlash(filepath.Join(e.Name(), p)))
			}
		}
	}
	return unreadable, nil
}

// insideDenied returns the first of denied (as DeniedPaths lists them) that holds p, as written or resolved. The
// denied paths are compared as listed, not resolved again: DeniedPaths already lists each one's real form, except
// through another user's entry in /tmp, which the sandbox listing does not follow either (see claude's realForm).
func insideDenied(p string, denied []string) (string, bool) {
	forms := []string{filepath.Clean(p), realPath(p)}
	for _, d := range denied {
		if within(forms[0], d) || within(forms[1], d) {
			return d, true
		}
	}
	return "", false
}

// within reports whether p is root or inside it.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// commands runs shell commands in dir until one fails, logging to logPath; running learns each one's process group.
func (env Env) commands(ctx context.Context, dir string, commands []string, logPath string, running func(pid int)) ([]task.Command, bool, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("log: %w", err)
	}
	defer log.Close()
	var results []task.Command
	for _, command := range commands {
		fmt.Fprintf(log, "$ %s\n", command)
		result, err := runner.Run(ctx, runner.Spec{Dir: dir, Command: command, Timeout: env.VerifyTimeout, Output: log, Started: running, Env: env.CommandEnv,
			Environ: env.commandBase})
		results = append(results, task.Command{Command: command, ExitCode: result.ExitCode, TimedOut: result.TimedOut,
			Seconds: result.Duration.Round(time.Millisecond).Seconds()})
		if err != nil {
			return results, false, err
		}
		if !result.Passed() {
			return results, false, nil
		}
	}
	return results, true, nil
}

// denied lists what the agent may not read: Agentium's data except its own workspace (projects and hidden tests,
// records, artifacts, the database, other runs' workspaces), other runs' temp roots, and the user's repository: every
// worktree of it, which can sit at a later commit holding the solution, and its git data. Workspaces and temp roots
// created after this run starts are not listed unless predicted (Env.DenyExtra): a known gap for concurrent runs,
// whose workspaces hold no hidden tests.
func (env Env) denied(ctx context.Context, workspace string) ([]string, error) {
	db := env.Layout.Database
	paths := []string{filepath.Join(env.Layout.Root, "projects"), env.Layout.Records, env.Layout.Artifacts, env.Layout.Cache, db, db + "-wal", db + "-shm"}
	paths = append(paths, env.repositoryPaths(ctx)...)
	// What the user's build configuration names (a Cargo target-dir or build-dir), for the repository and its worktrees;
	// a configuration that cannot be read safely stops the run here, before anything starts.
	configured, err := buildtool.ProjectCaches(env.Environ, env.Home, env.repositoryPaths(ctx))
	if err != nil {
		return nil, err
	}
	paths = append(paths, configured...)
	if entries, err := os.ReadDir(env.Layout.Workspaces); err == nil {
		for _, e := range entries {
			if other := filepath.Join(env.Layout.Workspaces, e.Name()); other != workspace {
				paths = append(paths, other)
			}
		}
	}
	temps, err := runTemps(env.Layout, env.Layout.RunTemp(filepath.Base(workspace)))
	if err != nil {
		return nil, err
	}
	return append(paths, temps...), nil
}

// repositoryPaths are the user's repository, all its worktrees, and its shared git data.
func (env Env) repositoryPaths(ctx context.Context) []string {
	paths := []string{env.ProjectRoot}
	if common, err := gitx.Run(ctx, "-C", env.ProjectRoot, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		paths = append(paths, common, filepath.Dir(common))
	}
	if list, err := gitx.Run(ctx, "-C", env.ProjectRoot, "worktree", "list", "--porcelain"); err == nil {
		for _, line := range strings.Split(list, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok && !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// step tells the caller a step is starting.
func (env Env) step(name string) {
	if env.Step != nil {
		env.Step(name)
	}
}

func (env Env) progress(format string, args ...any) {
	if env.Progress != nil {
		fmt.Fprintf(env.Progress, format+"\n", args...)
	}
}

// redactRecords removes the sign-in secret and credential-shaped strings from every text record of the run.
func (env Env) redactRecords(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && d.Name() == "verify" {
				return filepath.SkipDir // the verification copy is the agent's work tree, removed after grading
			}
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("redact %s: %w", p, err)
		}
		if clean := Redact(data, env.Secret); len(clean) != len(data) || string(clean) != string(data) {
			if err := os.WriteFile(p, clean, 0o600); err != nil {
				return fmt.Errorf("redact %s: %w", p, err)
			}
		}
		return nil
	})
}

// secretPatterns are credential shapes removed from records (as the pre-commit hook's scan knows them).
var secretPatterns = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}|\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,}|\bAKIA[0-9A-Z]{16}\b|` +
	`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})|\bxox[abprs]-[A-Za-z0-9-]{10,}|` +
	`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)

// Redact replaces secret, when not empty, and credential-shaped strings with [REDACTED].
func Redact(data []byte, secret string) []byte {
	text := string(data)
	if secret != "" {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return []byte(secretPatterns.ReplaceAllString(text, "[REDACTED]"))
}

func parseFile(p string) (claude.Metrics, error) {
	f, err := os.Open(p)
	if err != nil {
		return claude.Metrics{}, fmt.Errorf("run transcript: %w", err)
	}
	defer f.Close()
	return claude.Parse(f)
}

// appendProbe appends line to the first startup instruction file of the context in repo and returns that file's path,
// or "" when the context loads no instruction file at start.
func appendProbe(ctx context.Context, repo, line string) (string, error) {
	src, err := source.WorkingTree(ctx, repo)
	if err != nil {
		return "", err
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return "", err
	}
	for _, e := range resolved.Entries {
		if e.Kind == claudectx.KindInstructions {
			f, err := os.OpenFile(filepath.Join(repo, filepath.FromSlash(e.Path)), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return "", fmt.Errorf("probe: %w", err)
			}
			defer f.Close()
			if _, err := fmt.Fprintf(f, "\n%s\n", line); err != nil {
				return "", fmt.Errorf("probe: %w", err)
			}
			return e.Path, nil
		}
	}
	return "", nil
}

// contextAt resolves the context of the working tree at repo: the arm's context as its run started.
func contextAt(ctx context.Context, repo string) (source.Source, claudectx.Context, error) {
	src, err := source.WorkingTree(ctx, repo)
	if err != nil {
		return nil, claudectx.Context{}, err
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return nil, claudectx.Context{}, err
	}
	return src, resolved, nil
}

// outsideReads counts file tool paths inside a watched root but outside the run's workspace. Relative paths are the
// checkout's own.
func outsideReads(paths []string, repo, workspace string, watched []string) int {
	inside := func(p, root string) bool {
		rel, err := filepath.Rel(root, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	resolve := realPath // a path to a missing file must still match its root's resolved form
	count := 0
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(repo, p)
		}
		p = resolve(p)
		if inside(p, resolve(workspace)) {
			continue
		}
		for _, root := range watched {
			if root != "" && inside(p, resolve(root)) {
				count++
				break
			}
		}
	}
	return count
}

// realPath resolves symbolic links in the longest existing prefix of p (/var and /private/var on macOS), so paths to
// files that do not exist compare like the ones that do.
func realPath(p string) string {
	var missing []string
	p = filepath.Clean(p)
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, missing...)...)
		}
		missing = append([]string{filepath.Base(p)}, missing...)
		p = parent
	}
}

// ownSessionExcluded drops paths in the run's own session folder, where Claude Code saves large tool outputs for the
// agent to read back.
func ownSessionExcluded(paths []string, ownSession string) []string {
	own := realPath(ownSession)
	var kept []string
	for _, p := range paths {
		if !within(realPath(p), own) {
			kept = append(kept, p)
		}
	}
	return kept
}

func union(lists ...[]string) []string {
	var out []string
	for _, list := range lists {
		for _, x := range list {
			if !slices.Contains(out, x) {
				out = append(out, x)
			}
		}
	}
	sort.Strings(out)
	return out
}

// measure reads `git diff --numstat -z` into the behavior counts and returns the changed paths.
func measure(numstat string, b *Behavior) []string {
	var paths []string
	for _, record := range strings.Split(numstat, "\x00") {
		fields := strings.SplitN(record, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		added, _ := strconv.Atoi(fields[0]) // "-" for binary files counts as 0
		removed, _ := strconv.Atoi(fields[1])
		b.FilesChanged++
		b.LinesAdded += added
		b.LinesRemoved += removed
		paths = append(paths, fields[2])
	}
	return paths
}

// testRunner matches commands that run tests: the build tools' patterns from their profiles (Go, Maven, Gradle, Cargo,
// Python), then other runners.
var testRunner = regexp.MustCompile(`\b(` + strings.Join(append(buildtool.TestPatterns(),
	`(npm|pnpm|yarn|bun) (run )?test|jest|vitest|make test|rspec|dotnet test|harness\.py check`), "|") + `)\b`)

func ranTests(commands []string) bool {
	for _, c := range commands {
		if testRunner.MatchString(c) {
			return true
		}
	}
	return false
}

func ranChecks(commands, verify []string) bool {
	for _, c := range commands {
		for _, v := range verify {
			if v = strings.TrimSpace(v); v != "" && strings.Contains(c, v) {
				return true
			}
		}
	}
	return false
}

// copyTree copies src to dst (which must not exist), keeping modes and symbolic links as links. What in src cannot be
// read is skipped and listed (relative to src); failures to write dst are errors.
func copyTree(src, dst string) (unreadable []string, err error) {
	if _, err := os.Lstat(dst); err == nil {
		return nil, errors.New(dst + " already exists")
	}
	err = filepath.WalkDir(src, func(p string, d os.DirEntry, walkErr error) error {
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if walkErr != nil { // src itself, or a folder, that cannot be read
			if p == src && d == nil {
				return walkErr
			}
			unreadable = append(unreadable, rel)
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			unreadable = append(unreadable, rel)
			return nil
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				unreadable = append(unreadable, rel)
				return nil
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				unreadable = append(unreadable, rel)
				return nil
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
		return nil // sockets and devices are not copied
	})
	return unreadable, err
}

// removeStaleWorkspace removes a workspace and its temp root left by a run that no one stored, before a new run reuses
// the name. Only a real folder (not a symlink) that is a direct child of the workspaces folder is removed, so a name
// that escapes it is left alone and the run's own checks fail as before.
func removeStaleWorkspace(workspaces, workspace, tempRoot string) error {
	resolved := realPath(workspace)
	if filepath.Dir(resolved) != realPath(workspaces) || !within(resolved, realPath(workspaces)) {
		return nil
	}
	if info, err := os.Lstat(workspace); err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil // nothing there, or a link: not ours to remove
	}
	if err := os.RemoveAll(workspace); err != nil {
		return fmt.Errorf("remove the stale workspace %s: %w", workspace, err)
	}
	return removeRunTemp(tempRoot)
}
