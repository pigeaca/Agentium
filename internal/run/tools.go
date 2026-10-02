package run

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/source"
)

// toolsAtBase names the build-tool profiles of a commit in the bare repository, from the files at its root.
func toolsAtBase(ctx context.Context, bare, commit string) ([]string, error) {
	base, err := source.Commit(ctx, commit, "--git-dir", bare)
	if err != nil {
		return nil, err
	}
	return buildtool.DetectedNames(func(name string) bool { return source.Has(base, name) }), nil
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
// read (projects, records, artifacts, cache, the database), because offline builds must read it. So it must never hold
// anything compiled from hidden tests, and only a run's setup writes it (warmTools), in a checkout
// of the task's base: the hidden tests are added to a separate copy at grading. Build caches are off in every warm-up
// and denied to agents besides (buildtool.DepsDenied). One folder per project (the bare
// repository's folder name), shared by its tasks and runs; it only grows.
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
// found and the base is warmed again.
func (env Env) stampPath(deps, base string, names []string) string {
	return filepath.Join(env.warmState(deps), strings.Join(names, "+")+"-"+buildtool.WarmVersion(buildtool.Select(names))+"-"+filepath.Base(base))
}

// prepareTools is the build tools' part of a run's setup, before the task's own setup commands: it warms the project's
// dependencies into the deps folder (with network, once per base commit and tool set), then prepares the run's own
// folders (Gradle's user home). The warm-up runs in a throwaway checkout of the base commit, never in the run's own:
// whatever it builds (target/, build/, .gradle/) must not be a head start, or a handicap, for one arm. A warm-up that
// fails does not fail the run, whose agent may still build; it returns a note instead. The commands' output goes to
// logPath, setup.log.
func (env Env) prepareTools(ctx context.Context, profiles []buildtool.Profile, inv claude.Invocation, base, logPath string, running func(pid int)) (notes []string, err error) {
	if inv.Deps != "" && len(buildtool.NeedsWarming(profiles)) > 0 {
		note, err := env.warmInThrowaway(ctx, profiles, inv.Deps, base, logPath, running)
		if err != nil {
			return nil, err
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	if err := buildtool.PrepareRun(ctx, profiles, inv.Deps, inv.BuildCache); err != nil {
		return notes, err
	}
	return notes, nil
}

// warmInThrowaway checks the base commit out in the data folder's cache (agents may not read it), warms, and removes it.
func (env Env) warmInThrowaway(ctx context.Context, profiles []buildtool.Profile, deps, base, logPath string, running func(pid int)) (string, error) {
	if _, err := os.Stat(env.stampPath(deps, base, buildtool.NeedsWarming(profiles))); err == nil {
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
		if _, statErr := os.Stat(env.stampPath(deps, base, names)); statErr == nil {
			return "", nil // the other warm-up finished meanwhile, and warmed this base too
		}
		return "", fmt.Errorf("%w: another warm-up of the dependencies held the lock for %s and this base commit is not warmed", errWarmWait, wait)
	case err != nil:
		return "", fmt.Errorf("warm-up lock: %w", err)
	}
	defer unlock()
	stamp := env.stampPath(deps, base, names)
	if _, err := os.Stat(stamp); err == nil {
		return "", nil
	}
	if err := os.MkdirAll(deps, 0o700); err != nil {
		return "", fmt.Errorf("deps folder: %w", err)
	}
	if err := buildtool.PrepareDeps(profiles, deps); err != nil {
		return "", err
	}
	failed := ""
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
	if failed != "" {
		return "dependency warm-up failed (" + failed + "): the agent may not be able to build offline; see setup.log", nil
	}
	// What a step could not warm (Gradle's unresolvable configurations) is a note. Mostly such skips are permanent and
	// the base is stamped; a skip that looks like a network failure is not stamped, so the next run tries again.
	skipped, transient := buildtool.ReadWarmSkipped(repo)
	note := ""
	if len(skipped) > 0 {
		note = fmt.Sprintf("%d configuration(s) not warmed: %s", len(skipped), strings.Join(skipped[:min(len(skipped), 8)], ", "))
		if len(skipped) > 8 {
			note += ", ..."
		}
		note += "; the agent may not be able to use them offline"
		if transient {
			note += " (a network failure: not stamped, the next run warms again)"
			return note, nil
		}
	}
	if err := os.WriteFile(stamp, nil, 0o600); err != nil {
		return "", fmt.Errorf("warm-up state: %w", err)
	}
	return note, nil
}

// lockFile takes an exclusive lock on path, waiting for it until ctx ends (onWait, if set, is called once when it must wait); the lock goes with its holder's process.
func lockFile(ctx context.Context, path string, onWait func()) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, err
		}
		if onWait != nil {
			onWait()
			onWait = nil // once
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
