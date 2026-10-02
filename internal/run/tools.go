package run

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/task"
)

// toolsAtBase names the build-tool profiles of a commit in the bare repository, from the files at its root.
func toolsAtBase(ctx context.Context, bare, commit string) ([]string, error) {
	tools, _, err := baseLayout(ctx, bare, commit)
	return tools, err
}

// baseLayout is what a run takes from its task's base commit, never from the overlaid checkout or the agent's work:
// the build-tool profiles (files at its root) and where Python code imports from (buildtool.ImportRoot).
func baseLayout(ctx context.Context, bare, commit string) (tools []string, importRoot string, err error) {
	base, err := source.Commit(ctx, commit, "--git-dir", bare)
	if err != nil {
		return nil, "", err
	}
	return buildtool.DetectedNames(func(name string) bool { return source.Has(base, name) }), buildtool.ImportRoot(base.Paths()), nil
}

// NeedsLocalBinding reports whether runs on any of the commits need the sandbox's local binding (a Gradle build), so
// that a caller can ask for the user's opt-in before anything runs.
func NeedsLocalBinding(ctx context.Context, bare string, commits []string) (bool, error) {
	for _, c := range commits {
		tools, err := toolsAtBase(ctx, bare, c)
		if err != nil {
			return false, err
		}
		if buildtool.LocalBinding(buildtool.Select(tools)) {
			return true, nil
		}
	}
	return false, nil
}

// depsFolder is the project's folder of warmed dependencies, or "" when the data folder's layout names none.
//
// Agents read it and cannot write it: the sandbox lets them write only their checkout and their run's build cache,
// and Invocation.Deps is denied for writing besides. It lies in the data folder but outside the folders runs may not
// read (projects, records, artifacts, cache, the database), because offline builds must read it. So what agents may
// read of it must never hold anything compiled from hidden tests, and only a run's setup writes it (warmTools), in a
// checkout of the task's base: the hidden tests are added to a separate copy at grading. A later task's base holds
// earlier tasks' hidden tests as ordinary tests, and its warm-up compiles them, so what a tool records of its builds
// there (Gradle's whole home) and build caches (off in every warm-up) are denied to agents (buildtool.DepsDenied). One
// folder per project (the bare repository's folder name), shared by its tasks and runs; it only grows.
func (env Env) depsFolder() string {
	if env.Layout.Deps == "" || env.Layout.Cache == "" { // warm-up state lives in the cache, which agents cannot read
		return ""
	}
	key := "default"
	if env.Bare != "" {
		key = filepath.Base(filepath.Dir(env.Bare))
	}
	return filepath.Join(env.Layout.Deps, key)
}

// errWarmWait means a run waited out the warm-up lock and its dependencies are not warmed: the run ends as an
// infrastructure failure, without cloning anything, so it is retried or left out and never counted against an arm.
var errWarmWait = errors.New("dependencies not warmed")

// DefaultWarmWait is how long a run waits for another warm-up of the same project before it goes on without warming.
const DefaultWarmWait = 15 * time.Minute

// warmState is where warm-ups keep their lock and stamps: in the data folder's cache, which agents may not read. In
// the deps folder, which they can read, an agent could hold the lock (flock works on a read-only open) and stall every
// later warm-up, or plant stamps that make warm-ups skip.
func (env Env) warmState(deps string) string {
	return filepath.Join(env.Layout.Cache, "warm-state", filepath.Base(deps))
}

// stampPath is the file whose existence says the base commit's dependencies are warmed for the tool set by the current
// recipe: the name holds buildtool.WarmVersion, so a stamp left by an older recipe (or from before versions) is never
// found and the base is warmed again. It holds what the warm-up found for the base's runs (buildtool.Warmed, as JSON:
// Python's venv and notes); tools that find nothing leave it empty.

func (env Env) stampPath(deps, base string, names []string) string {
	return filepath.Join(env.warmState(deps), strings.Join(names, "+")+"-"+buildtool.WarmVersion(buildtool.Select(names))+"-"+filepath.Base(base))
}

// readStamp reads a base's stamp (stampPath): whether the base is warmed, and what its runs get. A stamp that does not
// parse, or names a venv that is no longer ready (removed by hand, its interpreter uninstalled, changed since), is not
// warmed: the base is warmed again, under the lock. With a profile that warms in Go (Python), the stamp always holds
// what it found, so an empty one is a write that did not finish, not a warmed base.
func readStamp(path string, profiles []buildtool.Profile) (buildtool.Warmed, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return buildtool.Warmed{}, false
	}
	if len(data) == 0 && warmsInGo(profiles) {
		return buildtool.Warmed{}, false
	}
	var w buildtool.Warmed
	if len(data) > 0 && json.Unmarshal(data, &w) != nil {
		return buildtool.Warmed{}, false
	}
	if w.Venv != "" && !buildtool.VenvReady(w.Venv) {
		return buildtool.Warmed{}, false
	}
	return w, true
}

// prepareTools is the build tools' part of a run's setup, before the task's own setup commands: it warms the project's
// dependencies into the deps folder (with network, once per base commit and tool set), then prepares the run's own
// folders (Gradle's user home). The warm-up runs in a throwaway checkout of the base commit, never in the run's own:
// whatever it builds (target/, build/, .gradle/) must not be a head start, or a handicap, for one arm. A warm-up that
// fails does not fail the run, whose agent may still build; it returns a note instead. The commands' output goes to
// logPath, setup.log. warmed is what the base's stamp holds for its runs (Python's venv), read once the warm-up is done;
// every run of the base repeats its notes.
func (env Env) prepareTools(ctx context.Context, profiles []buildtool.Profile, inv claude.Invocation, base, logPath string, running func(pid int)) (warmed buildtool.Warmed, notes []string, err error) {
	if names := buildtool.NeedsWarming(profiles); inv.Deps != "" && len(names) > 0 {
		note, err := env.warmInThrowaway(ctx, profiles, inv.Deps, base, logPath, running)
		if err != nil {
			return buildtool.Warmed{}, nil, err
		}
		// The stamp is the base's own (no other warm-up writes it), so it can be read after the lock is released.
		warmed, _ = readStamp(env.stampPath(inv.Deps, base, names), profiles)
		notes = append(notes, warmed.Notes...)
		if note != "" {
			notes = append(notes, note)
		}
	}
	if err := buildtool.PrepareRun(ctx, profiles, inv.Deps, inv.BuildCache); err != nil {
		return warmed, notes, err
	}
	return warmed, notes, nil
}

// warmInThrowaway checks the base commit out in the data folder's cache (agents may not read it), warms, and removes it.
func (env Env) warmInThrowaway(ctx context.Context, profiles []buildtool.Profile, deps, base, logPath string, running func(pid int)) (string, error) {
	if _, ok := readStamp(env.stampPath(deps, base, buildtool.NeedsWarming(profiles)), profiles); ok {
		return "", nil // warmed already: no checkout needed (warmTools checks again under the lock)
	}
	parent := filepath.Join(env.Layout.Cache, "warm")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("warm-up checkout: %w", err)
	}
	dir, err := os.MkdirTemp(parent, "w-")
	if err != nil {
		return "", fmt.Errorf("warm-up checkout: %w", err)
	}
	defer os.RemoveAll(dir)
	checkoutDir := filepath.Join(dir, "repo")
	if err := checkout.New(ctx, env.Bare, base, checkoutDir); err != nil {
		return "", fmt.Errorf("warm-up checkout: %w", err)
	}
	steps := buildtool.WarmSteps(profiles, checkoutDir, deps)
	return env.warmTools(ctx, checkoutDir, deps, base, profiles, buildtool.NeedsWarming(profiles), steps, logPath, running)
}

// warmTools runs the warm-up steps in the checkout. Runs that may overlap wait for each other here (a lock file in
// warmState, up to Env.WarmWait, else DefaultWarmWait; a run that waits in vain ends as an infrastructure failure, errWarmWait), so two warm-ups never write one dependency cache together; a stamp per base commit and tool set skips
// repeats. Agents of other runs may read the folder meanwhile, so a warm-up must leave what they read stable: it adds
// files, and the Gradle profile turns the user home's cache cleanup off, which would delete them.
func (env Env) warmTools(ctx context.Context, repo, deps, base string, profiles []buildtool.Profile, names []string, steps []buildtool.WarmStep, logPath string, running func(pid int)) (string, error) {
	state := env.warmState(deps)
	if err := os.MkdirAll(state, 0o700); err != nil {
		return "", fmt.Errorf("warm-up state: %w", err)
	}
	wait := cmp.Or(env.WarmWait, DefaultWarmWait)
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	unlock, err := lockFile(waitCtx, filepath.Join(state, "lock"), func() {
		env.progress("  waiting for another warm-up of the dependencies (up to %s)", wait)
	})
	cancel()
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		if _, ok := readStamp(env.stampPath(deps, base, names), profiles); ok {
			return "", nil // the other warm-up finished meanwhile, and warmed this base too
		}
		return "", fmt.Errorf("%w: another warm-up of the dependencies held the lock for %s and this base commit is not warmed", errWarmWait, wait)
	case err != nil:
		return "", fmt.Errorf("warm-up lock: %w", err)
	}
	defer unlock()
	stamp := env.stampPath(deps, base, names)
	if _, ok := readStamp(stamp, profiles); ok {
		return "", nil
	}
	if err := os.MkdirAll(deps, 0o700); err != nil {
		return "", fmt.Errorf("deps folder: %w", err)
	}
	if err := buildtool.PrepareDeps(profiles, deps); err != nil {
		return "", err
	}
	// The fixed steps, unless they already succeeded for this base and tool set while a Go warm-up (Python's) failed
	// in a way worth retrying: that failure never makes the other tools warm again (stepsDone). The marker is written
	// only when the steps are done for good: none failed and none skipped for a network failure (Gradle's skip file),
	// and it keeps their note (permanent skips), which the stamp then carries as if the steps had just run.
	failed, note, transient := "", "", false
	stepsDone := stamp + ".steps"
	if done, err := os.ReadFile(stepsDone); err == nil && warmsInGo(profiles) {
		note = string(done)
	} else {
		for _, step := range steps {
			warm := env
			warm.CommandEnv = append(slices.Clone(env.CommandEnv), step.Env...)
			_, ok, err := warm.commands(ctx, repo, []string{step.Command}, logPath, running)
			if err != nil {
				return "", err
			}
			if !ok && failed == "" {
				failed = step.Command
			}
		}
		// What a step could not warm (Gradle's unresolvable configurations) is a note. Mostly such skips are permanent
		// and the base is stamped; a skip that looks like a network failure is not stamped, so the next run tries again.
		var skipped []string
		skipped, transient = buildtool.ReadWarmSkipped(repo)
		if len(skipped) > 0 {
			note = fmt.Sprintf("%d configuration(s) not warmed: %s", len(skipped), strings.Join(skipped[:min(len(skipped), 8)], ", "))
			if len(skipped) > 8 {
				note += ", ..."
			}
			note += "; the agent may not be able to use them offline"
		}
		if failed == "" && !transient && len(steps) > 0 && warmsInGo(profiles) {
			if err := buildtool.WriteFileSynced(stepsDone, []byte(note), 0o600); err != nil {
				return "", fmt.Errorf("warm-up state: %w", err)
			}
		}
	}
	// Then what warms in Go (Python's venv), in the same checkout, under the same lock. A failure the repository or the
	// host causes (no interpreter meets requires-python, uv.lock without uv) is stamped with its note, like Gradle's
	// permanent skips, so later runs of the base say so without trying again; one a download may cause is not.
	warmed, err := env.warmFuncs(ctx, repo, deps, profiles, logPath, running)
	if err != nil {
		return "", err
	}
	if failed != "" {
		return "dependency warm-up failed (" + failed + "): the agent may not be able to build offline; see setup.log", nil
	}
	if warmed.Failed != "" {
		why := "dependency warm-up failed (" + warmed.Failed + "): the agent may not be able to build offline; see setup.log"
		if warmed.Transient {
			return why + " (not stamped: the next run warms again)", nil
		}
		warmed.Notes = append(warmed.Notes, why)
	}
	if transient {
		return note + " (a network failure: not stamped, the next run warms again)", nil
	}
	var content []byte
	if warmsInGo(profiles) || warmed.Venv != "" || len(warmed.Notes) > 0 {
		if content, err = json.Marshal(warmed); err != nil {
			return "", err
		}
	}
	// Whole or not at all, and on disk: a stamp cut short by a crash would read as warmed with nothing in it.
	if err := buildtool.WriteFileSynced(stamp, content, 0o600); err != nil {
		return "", fmt.Errorf("warm-up state: %w", err)
	}
	return note, nil
}

// warmsInGo reports whether a selected profile warms in Go (buildtool.Profile.WarmFunc: Python).
func warmsInGo(profiles []buildtool.Profile) bool {
	return slices.ContainsFunc(profiles, func(p buildtool.Profile) bool { return p.WarmFunc != nil })
}

// CommandsEnv is what CheckoutCommands needs: the data folder, the project's bare repository, the user's environment,
// the environment of Agentium's own commands (BuildEnv), the per-command timeout and the warm-up's wait.
type CommandsEnv struct {
	Layout     home.Layout
	Bare       string
	Environ    []string
	CommandEnv []string
	Timeout    time.Duration
	WarmWait   time.Duration
	Now        func() time.Time
}

// CheckoutCommands warms a base commit's build tools for Agentium's own commands outside a run (validation), as a
// run's setup does: the same deps folder, lock and stamp, so a venv validation builds is the one runs get, and the other
// way round. It returns how commands in a checkout of the base run: without the user's variables the agent never gets
// (buildtool.CheckoutEnviron), with the warmed tools' (buildtool.CheckoutEnv: the venv, the base's import root in the
// checkout), and the notes runs of the base get, with a note for a test runner the verify commands use and the venv
// lacks. A warm-up that waited out the lock is a note, not an error: validation then runs without the venv.
func CheckoutCommands(ctx context.Context, c CommandsEnv, base string, verify []string, logPath string) (task.CheckoutCommands, error) {
	tools, importRoot, err := baseLayout(ctx, c.Bare, base)
	if err != nil {
		return task.CheckoutCommands{}, err
	}
	profiles := buildtool.Select(tools)
	env := Env{Layout: c.Layout, Bare: c.Bare, Environ: c.Environ, CommandEnv: c.CommandEnv, VerifyTimeout: c.Timeout, WarmWait: c.WarmWait, Now: c.Now}
	if c.Layout.Cache != "" {
		env.CommandEnv = append(slices.Clone(env.CommandEnv), buildtool.CommandEnvFor(profiles, c.Layout.Cache)...)
	}
	var warmed buildtool.Warmed
	var notes []string
	if deps, names := env.depsFolder(), buildtool.NeedsWarming(profiles); deps != "" && len(names) > 0 {
		note, err := env.warmInThrowaway(ctx, profiles, deps, base, logPath, func(int) {})
		switch {
		case errors.Is(err, errWarmWait):
			note = err.Error() + ": validated without the warmed dependencies"
		case err != nil:
			return task.CheckoutCommands{}, err
		}
		warmed, _ = readStamp(env.stampPath(deps, base, names), profiles)
		notes = append(notes, warmed.Notes...)
		if note != "" {
			notes = append(notes, note)
		}
		notes = append(notes, buildtool.MissingRunners(ctx, warmed.Venv, verify, env.environ())...)
	}
	return task.CheckoutCommands{
		Environ: runner.Environ(buildtool.CheckoutEnviron(profiles, env.environ())),
		Env: func(dir string) []string {
			return buildtool.CheckoutEnv(profiles, buildtool.AgentContext{Allowed: env.environ(), Environ: env.environ(), Repo: dir,
				BuildCache: c.Layout.Cache, Deps: env.depsFolder(), Venv: warmed.Venv, ImportRoot: importRoot})
		},
		Notes: notes,
	}, nil
}

// environ is the user's environment as Agentium's own commands inherit it: Environ, else the process's.
func (env Env) environ() []string {
	if env.Environ != nil {
		return env.Environ
	}
	return os.Environ()
}

// warmFuncs runs the profiles' warm-ups that are Go code (buildtool.WarmFuncs) in the warm-up checkout repo, with the
// environment of Agentium's own commands, their output appended to logPath.
func (env Env) warmFuncs(ctx context.Context, repo, deps string, profiles []buildtool.Profile, logPath string, running func(pid int)) (buildtool.Warmed, error) {
	if !slices.ContainsFunc(profiles, func(p buildtool.Profile) bool { return p.WarmFunc != nil }) {
		return buildtool.Warmed{}, nil
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return buildtool.Warmed{}, fmt.Errorf("log: %w", err)
	}
	defer log.Close()
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	return buildtool.WarmFuncs(ctx, profiles, buildtool.WarmInput{Dir: repo, Deps: deps, Environ: env.Environ, Env: env.CommandEnv,
		Log: log, Started: running, Timeout: env.VerifyTimeout, Now: now()})
}

// lockFile is home.LockFile: an exclusive flock on path, waited for until ctx ends.
func lockFile(ctx context.Context, path string, onWait func()) (unlock func(), err error) {
	return home.LockFile(ctx, path, onWait)
}
