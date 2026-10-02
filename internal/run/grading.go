package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/sandbox"
)

// The grading environment of sandboxed grading (the isolation plan, Part 1, step 2): what a grade gets besides its
// grading copy, made by functions the wiring (step 3) calls. Nothing here is wired into Once or validation yet: grading
// still runs on the host, with the shared CommandEnv, exactly as before.
//
// A grade runs the agent's own code (its build scripts, conftest.py, build.rs), so a cache it writes is a poisoning
// path: a cache later grades read could turn another run's fail into a pass. So:
//   - each grade has its own cache (Grading.Cache), a clone of a seed, made fresh and removed after the grade
//     (withGrading), even when it is cancelled; a dead process's is removed by Recover. Two grades never share one:
//     the grade's folder is created exclusively, and a clone never replaces anything;
//   - the seed (gradingSeed) is written only by trusted code before it is published (prepareSeed: the profiles'
//     PrepareRun hooks and a trusted warm step of the caller's), never by a grade, and never after it is published. It
//     lies in the data folder's cache, which the grading sandbox denies whole; and a clone is independent of it, so a
//     grade's writes to its clone never reach it (buildtool.CloneFolder);
//   - the seed is per project, tool set and base commit, and holds nothing but what its base's own files make: the
//     caller's warm step may build the base (whose tests the base's agents see anyway), never hidden tests or a
//     reference solution. A base holds earlier tasks' solutions and hidden tests as ordinary files, so a seed warmed on
//     one base must never serve a grade of another (an earlier task's grade would find its own reference compiled).
//     What a grade adds to its clone (the hidden tests' builds) goes with the clone.
//
// A grade's environment is the agent's recipe (buildtool.GraderEnv), its writable folders the grading copy, the cache
// and the temp root (Grading.WriteProfile).

// Names in a run's records: the grade's folder, and the seed folders in the data folder's cache.
const (
	gradingFolder = "grading"
	seedsFolder   = "grading-seed"
)

// gradingSeed is the seed of the grading caches of runs on base with the selected profiles:
// <cache>/grading-seed/<project>/<buildtool.SeedKey>-<base>, or "" when the layout names no cache folder. The project is
// the deps folder's (depsFolder): the bare repository's project folder.
func (env Env) gradingSeed(profiles []buildtool.Profile, base string) string {
	if env.Layout.Cache == "" {
		return ""
	}
	project := "default"
	if env.Bare != "" {
		project = filepath.Base(filepath.Dir(env.Bare))
	}
	return filepath.Join(env.Layout.Cache, seedsFolder, project, buildtool.SeedKey(profiles)+"-"+filepath.Base(base))
}

// prepareSeed makes the seed folder once, if it is not there yet: the selected profiles' PrepareRun hooks (Gradle's
// user home with no daemon and the wrapper cloned from deps; Python's cache folders), then warm, when set, with the
// folder in the making. warm is trusted code only, run before any grade: the base's own commands (a warm-up, or
// validation's base stage), never anything that holds hidden tests or a reference solution, and never a grade.
//
// It is published whole by a rename, under a lock beside it (seed+".lock"), and never changed after: concurrent runs
// wait for one maker, and a folder that exists is a finished seed. What a dead maker left (seed+".tmp") is removed under
// the lock before the next one starts.
func prepareSeed(ctx context.Context, profiles []buildtool.Profile, deps, seed string, warm func(ctx context.Context, dir string) error) error {
	if !filepath.IsAbs(seed) {
		return fmt.Errorf("the grading seed %q is not absolute", seed)
	}
	if ready, err := seedReady(seed); ready || err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(seed), 0o700); err != nil {
		return fmt.Errorf("grading seed: %w", err)
	}
	unlock, err := lockFile(ctx, seed+".lock", nil)
	if err != nil {
		return fmt.Errorf("grading seed lock: %w", err)
	}
	defer unlock()
	if ready, err := seedReady(seed); ready || err != nil {
		return err
	}
	tmp := seed + ".tmp"
	if err := removeTree(tmp); err != nil {
		return fmt.Errorf("grading seed: %w", err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fmt.Errorf("grading seed: %w", err)
	}
	if err := fillSeed(ctx, profiles, deps, tmp, warm); err != nil {
		return errors.Join(err, removeTree(tmp))
	}
	if err := os.Rename(tmp, seed); err != nil {
		return errors.Join(fmt.Errorf("grading seed: %w", err), removeTree(tmp))
	}
	return nil
}

// fillSeed runs the hooks and the warm step in dir.
func fillSeed(ctx context.Context, profiles []buildtool.Profile, deps, dir string, warm func(ctx context.Context, dir string) error) error {
	if err := buildtool.PrepareRun(ctx, profiles, deps, dir); err != nil {
		return fmt.Errorf("grading seed: %w", err)
	}
	if warm != nil {
		if err := warm(ctx, dir); err != nil {
			return fmt.Errorf("grading seed: warm: %w", err)
		}
	}
	return nil
}

// seedReady reports whether seed is a finished seed: a real folder. Anything else there (a file, a link) is an error,
// never cloned or followed.
func seedReady(seed string) (bool, error) {
	info, err := os.Lstat(seed)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("grading seed: %w", err)
	case !info.IsDir():
		return false, fmt.Errorf("grading seed %s is not a folder", seed)
	}
	return true, nil
}

// gradingInput is what a grade's folders and environment are made from.
type gradingInput struct {
	// Root is the grade's own folder (a run's: <records>/<id>/grading). It must not exist: it is created here,
	// owner-only, and holds the cache, the temp root and the sandbox profile file. Its parent is created when missing.
	Root string
	// Seed is the seed the cache is cloned from (gradingSeed, made by prepareSeed). Empty: the cache is made fresh by
	// the profiles' PrepareRun hooks, as a seed is.
	Seed string
	// Copy is the grading copy, which must exist: the grade's working folder.
	Copy string
	// Agent is the run's agent invocation, or one with the same Tools, AgentTools, Home, Deps, JavaHome, Venv,
	// ProjectMetadata and ImportRoot: the grade gets the agent's recipe (buildtool.GraderEnv). Its folders (Dir,
	// BuildCache, TempRoot, ConfigDir) and sign-in are not used.
	Agent claude.Invocation
	// Environ is the user's environment, which the agent's allowlist filters (claude.EnvironFor).
	Environ []string
}

// grading is one grade's own folders and environment.
type grading struct {
	Root  string // holds the rest; removed whole (remove)
	Copy  string // the grading copy (not under Root; the caller removes it)
	Cache string // the grade's build cache: Root/cache, a clone of the seed
	Temp  string // the grade's temp root: Root/tmp (TMPDIR, java.io.tmpdir)
	Deps  string // the deps folder the grade reads, the agent's
	// Environ is the grade's whole environment (runner.Spec.Environ): buildtool.GraderEnv.
	Environ []string
	// Made says how the cache was made: buildtool.CloneFile, a cp command, or "prepared" (no seed).
	Made string
}

// prepareGrading makes a grade's folders and environment: Root, created exclusively; the cache, cloned from the seed
// (or prepared fresh without one); the temp root; then the environment. On an error nothing is left: Root is removed.
func prepareGrading(ctx context.Context, in gradingInput) (g grading, err error) {
	for name, p := range map[string]string{"folder": in.Root, "copy": in.Copy} {
		if !filepath.IsAbs(p) {
			return grading{}, fmt.Errorf("the grade's %s %q is not absolute", name, p)
		}
	}
	if info, err := os.Lstat(in.Copy); err != nil || !info.IsDir() {
		return grading{}, fmt.Errorf("the grading copy %s is not a folder (%v)", in.Copy, err)
	}
	if in.Seed != "" {
		if ready, err := seedReady(in.Seed); err != nil {
			return grading{}, err
		} else if !ready {
			return grading{}, fmt.Errorf("the grading seed %s does not exist", in.Seed)
		}
	}
	if err := os.MkdirAll(filepath.Dir(in.Root), 0o700); err != nil {
		return grading{}, fmt.Errorf("the grade's folder: %w", err)
	}
	// Exclusive: a folder already there is another grade's, or a dead one's that Recover has not removed yet. Either
	// way it is not this grade's to use.
	if err := os.Mkdir(in.Root, 0o700); err != nil {
		return grading{}, fmt.Errorf("the grade's folder: %w", err)
	}
	g = grading{Root: in.Root, Copy: in.Copy, Cache: filepath.Join(in.Root, "cache"), Temp: filepath.Join(in.Root, "tmp"), Deps: in.Agent.Deps}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeTree(in.Root))
			g = grading{}
		}
	}()
	profiles := buildtool.SelectRun(in.Agent.Tools, in.Agent.AgentTools)
	if in.Seed != "" {
		if g.Made, err = buildtool.CloneFolder(ctx, in.Seed, g.Cache); err != nil {
			return g, fmt.Errorf("the grade's cache: %w", err)
		}
	} else {
		if err = os.Mkdir(g.Cache, 0o700); err != nil {
			return g, fmt.Errorf("the grade's cache: %w", err)
		}
		if err = buildtool.PrepareRun(ctx, profiles, in.Agent.Deps, g.Cache); err != nil {
			return g, fmt.Errorf("the grade's cache: %w", err)
		}
		g.Made = "prepared"
	}
	if err = os.Mkdir(g.Temp, 0o700); err != nil {
		return g, fmt.Errorf("the grade's temp root: %w", err)
	}
	inv := in.Agent
	allowed := claude.EnvironFor(in.Environ, profiles)
	g.Environ, err = buildtool.GraderEnv(profiles, allowed, buildtool.AgentContext{Environ: in.Environ, Home: inv.Home, Repo: in.Copy,
		BuildCache: g.Cache, Deps: inv.Deps, JavaHome: inv.JavaHome, Venv: inv.Venv, Metadata: inv.ProjectMetadata, ImportRoot: inv.ImportRoot}, g.Temp)
	if err != nil {
		return g, err
	}
	return g, nil
}

// remove removes the grade's folder: its cache, temp root and profile file (not the grading copy). It needs no
// context: it runs on cancellation too.
func (g grading) remove() error {
	return removeTree(g.Root)
}

// profileFile is where WriteProfile writes the grade's sandbox profile: in Root, outside the folders the grade writes
// (Cache, Temp, Copy), so no build can rewrite it between two of the grade's commands.
func (g grading) profileFile() string {
	return filepath.Join(g.Root, "profile.sb")
}

// writeProfile writes the grade's sandbox profile (sandbox.Profile.WriteFile) with p's tag, home, data folder, denied
// paths and loopback, and the grade's own folders: the copy, the cache and the temp root as the writable ones, and the
// agent's deps folder, read-only. The folders exist by now, as WriteFile needs, and the file goes in Root, outside them
// (profileFile). It returns the profile as written, the file and its SHA-256, for the canary and CheckFile (step 3).
func (g grading) writeProfile(p sandbox.Profile) (written sandbox.Profile, file, digest string, err error) {
	p.Copy, p.Cache, p.Temp, p.Deps = g.Copy, g.Cache, g.Temp, g.Deps
	file = g.profileFile()
	digest, err = p.WriteFile(file)
	if err != nil {
		return sandbox.Profile{}, "", "", err
	}
	return p, file, digest, nil
}

// withGrading prepares a grade (prepareGrading), runs grade with it, and removes it whatever happens: on success, on
// an error, on cancellation, and when grade panics. A removal that fails is returned with grade's error.
func withGrading(ctx context.Context, in gradingInput, grade func(g grading) error) (err error) {
	g, err := prepareGrading(ctx, in)
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := g.remove(); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove the grade's folder: %w", rmErr))
		}
	}()
	return grade(g)
}

// removeTree removes root as os.RemoveAll does, and also when what a grade (the agent's code) left in it resists: a
// folder without permissions (chmod 000) or a file or folder with the user's immutable or append-only flag (chflags
// uchg), both within a grade's power over its own folders. It then clears those on what it finds, never through a link,
// and tries again. A missing root is not an error.
//
// Known limit: the clearing pass checks each path (not a link) before it changes it; a process still writing the
// folder could swap a path for a link in between. It runs after the grade's processes ended (step 3 makes sure of
// that for processes that left the process group: the plan's F7).
func removeTree(root string) error {
	if root == "" {
		return nil
	}
	if err := os.RemoveAll(root); err == nil {
		return nil
	}
	unlockTree(root)
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove %s: %w", root, err)
	}
	return nil
}

// unlockTree clears the flags (clearFlags) of p and of what is under it, and gives its folders back their owner's
// permissions, never following a link.
func unlockTree(p string) {
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	clearFlags(p, info)
	if !info.IsDir() {
		return
	}
	if info.Mode().Perm()&0o700 != 0o700 {
		os.Chmod(p, info.Mode().Perm()|0o700)
	}
	entries, _ := os.ReadDir(p)
	for _, e := range entries {
		unlockTree(filepath.Join(p, e.Name()))
	}
}
