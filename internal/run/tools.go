package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claude"
)

// depsFolder is the project's folder of warmed dependencies, or "" when the data folder's layout names none.
//
// Agents read it and cannot write it: the sandbox lets them write only their checkout and their run's build cache,
// and Invocation.Deps is denied for writing besides. It lies in the data folder but outside the folders runs may not
// read (projects, records, artifacts, cache, the database), because offline builds must read it. So it must never hold
// anything compiled from hidden tests, and only a run's setup writes it (warmTools), in a checkout of the task's base
// and the arm's context: the hidden tests are added to a separate copy at grading. One folder per project (the bare
// repository's folder name), shared by its tasks and runs; it only grows.
func (env Env) depsFolder() string {
	if env.Layout.Deps == "" {
		return ""
	}
	key := "default"
	if env.Bare != "" {
		key = filepath.Base(filepath.Dir(env.Bare))
	}
	return filepath.Join(env.Layout.Deps, key)
}

// prepareTools is the build tools' part of a run's setup, before the task's own setup commands: it warms the project's
// dependencies into the deps folder (with network, once per base commit and tool set), then prepares the run's own
// folders (Gradle's user home). A warm-up that fails does not fail the run, whose agent may still build; it returns a
// note instead. The commands' output goes to logPath, setup.log.
func (env Env) prepareTools(ctx context.Context, profiles []buildtool.Profile, repo string, inv claude.Invocation, base, logPath string, running func(pid int)) (notes []string, err error) {
	if inv.Deps != "" {
		has := func(name string) bool { _, err := os.Stat(filepath.Join(repo, name)); return err == nil }
		if steps := buildtool.WarmSteps(profiles, has, inv.Deps); len(steps) > 0 {
			note, err := env.warmTools(ctx, repo, inv.Deps, base, buildtool.NeedsWarming(profiles), steps, logPath, running)
			if err != nil {
				return nil, err
			}
			if note != "" {
				notes = append(notes, note)
			}
		}
	}
	if err := buildtool.PrepareRun(ctx, profiles, inv.Deps, inv.BuildCache); err != nil {
		return notes, err
	}
	return notes, nil
}

// warmTools runs the warm-up steps in the checkout. Runs that may overlap wait for each other here (a lock file in the
// deps folder), so two warm-ups never write one dependency cache together; a stamp per base commit and tool set skips
// repeats. Agents of other runs may read the folder meanwhile: a warm-up only adds files.
func (env Env) warmTools(ctx context.Context, repo, deps, base string, names []string, steps []buildtool.WarmStep, logPath string, running func(pid int)) (string, error) {
	if err := os.MkdirAll(filepath.Join(deps, "stamps"), 0o700); err != nil {
		return "", fmt.Errorf("deps folder: %w", err)
	}
	unlock, err := lockFile(ctx, filepath.Join(deps, ".lock"))
	if err != nil {
		return "", fmt.Errorf("deps folder: %w", err)
	}
	defer unlock()
	stamp := filepath.Join(deps, "stamps", strings.Join(names, "+")+"-"+filepath.Base(base))
	if _, err := os.Stat(stamp); err == nil {
		return "", nil
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
	if err := os.WriteFile(stamp, nil, 0o600); err != nil {
		return "", fmt.Errorf("deps folder: %w", err)
	}
	return "", nil
}

// lockFile takes an exclusive lock on path, waiting for it until ctx ends; the lock goes with its holder's process.
func lockFile(ctx context.Context, path string) (unlock func(), err error) {
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
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
