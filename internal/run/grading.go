package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/sandbox"
)

// The grading environment of sandboxed grading (the isolation plan, Part 1, step 2): what a grade gets besides its
// grading copy. Sandbox mode (sandboxgrade.go) grades runs and validation stages with it; host mode grades on the host,
// with the shared CommandEnv, exactly as before, and never uses a seed.
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
//     What a grade adds to its clone (the hidden tests' builds) goes with the clone;
//   - seeds are for sandboxed grades only. A host-mode grade runs unsandboxed and could write the seed itself, so it is
//     never offered one (step 3 gates seed use on the sandbox mode);
//   - what a grade left running is stopped before its folders go (grading.stop): every process of the grade's own
//     sandbox (found by what its sandbox allows, so a `setsid` child that left the folders is found too) and every
//     process that uses the folders; and the removal never follows a link
//     (removeTree): a grade's process may still swap any entry for a link to the user's files. What cannot be removed is
//     moved aside into the quarantine (removeOrQuarantine), so a hostile grade can never block later runs.
//
// A grade's environment is the agent's recipe (buildtool.GraderEnv), its writable folders the grading copy, the cache
// and the temp root (Grading.WriteProfile).

// Names: the grade's folder in a run's records; the seed folders and the quarantine in the data folder's cache, which
// agents and grades may not read.
const (
	gradingFolder    = "grading"
	seedsFolder      = "grading-seed"
	quarantineFolder = "quarantine"
)

// fullCommit is a resolved commit ID: SHA-1 or SHA-256, in full.
var fullCommit = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// gradingSeed is the seed of the grading caches of runs on base with the selected profiles:
// <cache>/grading-seed/<project>/<buildtool.SeedKey>-<base>. The project is the deps folder's (depsFolder): the bare
// repository's project folder. base must be a full, resolved commit ID: a seed holds what one commit's files make, and
// a branch name or a short ID could name another commit later.
func (env Env) gradingSeed(profiles []buildtool.Profile, base string) (string, error) {
	if env.Layout.Cache == "" {
		return "", errors.New("the data folder's layout names no cache folder for grading seeds")
	}
	if !fullCommit.MatchString(base) {
		return "", fmt.Errorf("grading seed: %q is not a full commit ID", base)
	}
	project := "default"
	if env.Bare != "" {
		project = filepath.Base(filepath.Dir(env.Bare))
	}
	key := buildtool.SeedKey(profiles)
	if m := buildtool.ModuleKey(env.Module); m != "" { // two modules of one repository never share a seed
		key += "-" + m
	}
	return filepath.Join(env.Layout.Cache, seedsFolder, project, key+"-"+base), nil
}

// quarantine is where removeOrQuarantine moves what it cannot remove: in the data folder's cache.
func quarantine(layout home.Layout) string {
	if layout.Cache == "" {
		return ""
	}
	return filepath.Join(layout.Cache, quarantineFolder)
}

// prepareSeed makes the seed folder once, if it is not there yet: the selected profiles' PrepareRun hooks (Gradle's
// user home with no daemon and the wrapper cloned from deps; Python's cache folders), then warm, when set, with the
// folder in the making. warm is trusted code only, run before any grade: the base's own commands (a warm-up, or
// validation's base stage), never anything that holds hidden tests or a reference solution, and never a grade.
//
// It is published whole by a rename, under a lock beside it (seed+".lock"), and never changed after: concurrent runs
// wait for one maker, and a folder that exists is a finished seed. Before the rename, what the warm step left running
// is stopped (the profiles' StopRun, then every process still using the folder), so nothing writes the seed once it is
// published. What a dead maker left (seed+".tmp") is removed under the lock before the next one starts.
//
// Every call that returns a seed marks it used (markUsed): the seed folder's modification time is its last use, which
// cleanup (PlanClean) ages it by. A seed already there is found and marked under a shared lock (seedTaken), so cleanup,
// which takes the lock exclusively and rechecks the mark before it moves a seed away, never takes one being handed out.
func prepareSeed(ctx context.Context, profiles []buildtool.Profile, deps, seed string, warm func(ctx context.Context, dir string) error) (err error) {
	if !filepath.IsAbs(seed) {
		return fmt.Errorf("the grading seed %q is not absolute", seed)
	}
	defer func() {
		if err == nil {
			markUsed(seed)
		}
	}()
	if ready, err := seedTaken(ctx, seed); ready || err != nil {
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
		_, stopErr := stopUsing(profiles, tmp)
		return errors.Join(err, stopErr, removeTree(tmp))
	}
	if _, err := stopUsing(profiles, tmp); err != nil {
		return errors.Join(fmt.Errorf("grading seed: %w", err), removeTree(tmp))
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
	// owner-only, and holds the copy, the cache, the temp root and the sandbox profile file. Its parent is created when
	// missing.
	Root string
	// Seed is the seed the cache is cloned from (gradingSeed, made by prepareSeed). Empty: the cache is made fresh by
	// the profiles' PrepareRun hooks, as a seed is.
	Seed string
	// Copy is the grading copy, which must exist, outside Root and on its volume: it is moved into Root (Root/copy),
	// the grade's working folder there, and goes with Root. Nested in a folder of Agentium's own, it can always be moved
	// into the quarantine whole, whatever the grade does to the copy itself (an access list on it denying deletion
	// blocks a rename of the copy, not of the folder above it). On a failure to prepare, it is moved back.
	Copy string
	// Agent is the run's agent invocation, or one with the same Tools, AgentTools, Home, Deps, JavaHome, Venv,
	// ProjectMetadata and ImportRoot: the grade gets the agent's recipe (buildtool.GraderEnv). Its folders (Dir,
	// BuildCache, TempRoot, ConfigDir) and sign-in are not used.
	Agent claude.Invocation
	// Environ is the user's environment, which the agent's allowlist filters (claude.EnvironFor).
	Environ []string
	// Quarantine is where a grade's folder that cannot be removed is moved (quarantine of the layout); Warn, when set,
	// is told when that happens.
	Quarantine string
	Warn       func(string)
	// Cleaning, when set, is told when the cleanup starts, and Quarantined when it moved the folder into the quarantine.
	Cleaning, Quarantined func()
	// remove, when set, replaces removeTree for the grade's folder (tests make it fail).
	remove func(string) error
	// Keep, when set, is where the copy is moved back to once the grade is done and what it left running is stopped
	// (--keep): the rest of the grade's folder is removed as always. It must not exist; normally it is Copy.
	Keep string
}

// grading is one grade's own folders and environment.
type grading struct {
	Root  string // holds the rest; removed whole (remove)
	Copy  string // the grading copy: Root/copy, moved there from gradingInput.Copy
	Cache string // the grade's build cache: Root/cache, a clone of the seed
	Temp  string // the grade's temp root: Root/tmp (TMPDIR, java.io.tmpdir)
	Deps  string // the deps folder the grade reads, the agent's
	// Environ is the grade's whole environment (runner.Spec.Environ): buildtool.GraderEnv.
	Environ []string
	// Made says how the cache was made: buildtool.CloneFile, a cp command, or "prepared" (no seed).
	Made string
	// profiles are the run's (buildtool.SelectRun): their StopRun ends what the grade's build tools left running.
	profiles []buildtool.Profile
}

// prepareGrading makes a grade's folders and environment: Root, created exclusively; the copy, moved into it; the cache,
// cloned from the seed (or prepared fresh without one); the temp root; then the environment. On an error the copy is
// moved back and Root is removed.
func prepareGrading(ctx context.Context, in gradingInput) (g grading, err error) {
	for name, p := range map[string]string{"folder": in.Root, "copy": in.Copy} {
		if !filepath.IsAbs(p) {
			return grading{}, fmt.Errorf("the grade's %s %q is not absolute", name, p)
		}
	}
	if info, err := os.Lstat(in.Copy); err != nil || !info.IsDir() {
		return grading{}, fmt.Errorf("the grading copy %s is not a folder (%v)", in.Copy, err)
	}
	if within(filepath.Clean(in.Copy), filepath.Clean(in.Root)) || within(filepath.Clean(in.Root), filepath.Clean(in.Copy)) {
		return grading{}, fmt.Errorf("the grading copy %s and the grade's folder %s overlap", in.Copy, in.Root)
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
	g = grading{Root: in.Root, Copy: filepath.Join(in.Root, "copy"), Cache: filepath.Join(in.Root, "cache"), Temp: filepath.Join(in.Root, "tmp"),
		Deps: in.Agent.Deps}
	moved := false
	defer func() {
		if err != nil {
			if moved {
				err = errors.Join(err, os.Rename(g.Copy, in.Copy))
			}
			err = errors.Join(err, removeTree(in.Root))
			g = grading{}
		}
	}()
	if err = os.Rename(in.Copy, g.Copy); err != nil {
		return g, fmt.Errorf("the grading copy: %w", err)
	}
	moved = true
	profiles := buildtool.SelectRun(in.Agent.Tools, in.Agent.AgentTools)
	g.profiles = profiles
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
	g.Environ, err = buildtool.GraderEnv(profiles, allowed, buildtool.AgentContext{Environ: in.Environ, Home: inv.Home, Repo: g.Copy,
		BuildCache: g.Cache, Deps: inv.Deps, JavaHome: inv.JavaHome, Venv: inv.Venv, Metadata: inv.ProjectMetadata, ImportRoot: inv.ImportRoot}, g.Temp)
	if err != nil {
		return g, err
	}
	return g, nil
}

// remove stops what the grade left running (stop), then removes the grade's folder: its copy, cache, temp root and
// profile file. It needs no context: it runs on cancellation too.
func (g grading) remove() error {
	_, err := g.stop()
	return errors.Join(err, removeTree(g.Root))
}

// stop ends what the grade left running and returns what it killed, one line each: first every process of the grade's
// own sandbox (stopSandboxed: one whose sandbox may write the grade's temp root but not the folder above the grade's,
// so a `setsid` child that changed its working folder away and closed every file is found too), while the grade's
// folder is still locked (lock) and the temp root cannot have been moved; then what uses the grade's folders
// (stopUsing: the profiles' StopRun on the cache, and any process working there or holding a file, which also covers a
// host-mode grade's processes); then it makes the folder writable again, for the removal.
func (g grading) stop() ([]string, error) {
	killed, err := stopSandboxed(g.Temp, filepath.Dir(g.Root))
	more, useErr := stopUsing(g.profiles, g.Cache, g.Root)
	var chmodErr error
	if info, statErr := os.Lstat(g.Root); statErr == nil && info.IsDir() {
		chmodErr = os.Chmod(g.Root, 0o700)
	}
	return append(killed, more...), errors.Join(err, useErr, chmodErr)
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

// withGrading prepares a grade (prepareGrading), runs grade with it, then stops what it left running and removes it,
// whatever happens: on success, on an error, on cancellation, and when grade panics. With in.Keep, the copy is moved
// there first, once nothing of the grade runs any more. A folder that cannot be removed
// is moved into in.Quarantine with a warning (removeOrQuarantine); only when that fails too is it an error.
func withGrading(ctx context.Context, in gradingInput, grade func(g grading) error) (err error) {
	g, err := prepareGrading(ctx, in)
	if err != nil {
		return err
	}
	warn := func(string) {}
	if in.Warn != nil {
		warn = in.Warn
	}
	defer func() {
		if in.Cleaning != nil {
			in.Cleaning()
		}
		killed, stopErr := g.stop()
		for _, k := range killed {
			warn("the grade left a process running; it was stopped: " + k)
		}
		if stopErr != nil {
			warn("the grade left processes that could not all be stopped: " + stopErr.Error())
		}
		if in.Keep != "" {
			if err := os.Rename(g.Copy, in.Keep); err != nil {
				warn("the grading copy could not be kept: " + err.Error())
			}
		}
		remove := removeTree
		if in.remove != nil {
			remove = in.remove
		}
		warning, rmErr := quarantineAfter(g.Root, in.Quarantine, remove)
		if warning != "" {
			warn(warning)
			if in.Quarantined != nil {
				in.Quarantined()
			}
		}
		if rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove the grade's folder: %w", rmErr))
		}
	}()
	return grade(g)
}

// stopUsing ends what is left running in folders: the profiles' StopRun on the first (the build cache: Gradle's
// daemons), then every process of the user that still uses any of them (stopProcessesUnder, macOS), which it returns,
// one line each (process ID and command). It needs no context: it runs after a cancellation too, within half a minute.
func stopUsing(profiles []buildtool.Profile, folders ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var errs []error
	if len(folders) > 0 && folders[0] != "" {
		if err := buildtool.StopRun(ctx, profiles, folders[0], buildtool.SystemHost()); err != nil {
			errs = append(errs, err)
		}
	}
	killed, err := stopProcessesUnder(folders)
	if err != nil {
		errs = append(errs, err)
	}
	return killed, errors.Join(errs...)
}

// removeTree removes root, whose contents a grade (the agent's code) wrote and may still be changing. os.RemoveAll comes
// first (it never follows a link); when what the grade left resists it (a folder without permissions, the owner's
// immutable or append-only flag or an access list on a file, a folder or a link), removeTreeAt clears that and removes, entry by entry
// through folder descriptors, never following a link or resolving a path the grade can swap. A missing root is not an
// error. root's parent must be Agentium's own folder, which no grade can write.
func removeTree(root string) error {
	if root == "" {
		return nil
	}
	if err := os.RemoveAll(root); err == nil {
		return nil
	}
	return removeTreeAt(root)
}

// removeOrQuarantine removes root (removeTree); when that fails, it moves root into the quarantine folder dir, under a
// fresh name, and returns a warning instead of an error: what a grade can make unremovable (an access list that denies
// deletion, a tree deeper than the descriptors allow) must never block later runs or recovery. Quarantined folders lie
// in the data folder's cache, out of every agent's and grade's reach; recovery tries to remove them again
// (emptyQuarantine). The error is for a folder that could be neither removed nor moved.
func removeOrQuarantine(root, dir string) (warning string, err error) {
	return quarantineAfter(root, dir, removeTree)
}

// quarantineAfter is removeOrQuarantine with the removal given (tests make it fail).
func quarantineAfter(root, dir string, remove func(string) error) (warning string, err error) {
	rmErr := remove(root)
	if rmErr == nil {
		return "", nil
	}
	if dir == "" {
		return "", rmErr
	}
	moved, err := moveAside(root, dir)
	if err != nil {
		return "", fmt.Errorf("%w; and it could not be moved aside: %v", rmErr, err) // one line: it becomes a warning
	}
	return fmt.Sprintf("%s could not be removed (%v); it was moved to %s: inspect it, then remove it (chmod -RN may be needed)", root, rmErr, moved), nil
}

// moveAside renames root into dir, as <parent>-<name>-<random>: a run's grade folder (<records>/<id>/grading) is named
// after its run.
func moveAside(root, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	moved := filepath.Join(dir, filepath.Base(filepath.Dir(root))+"-"+filepath.Base(root)+"-"+hex.EncodeToString(b))
	if err := os.Rename(root, moved); err != nil {
		return "", err
	}
	return moved, nil
}

// emptyQuarantine tries to remove what the quarantine folder dir holds, and returns a warning for what is still there.
func emptyQuarantine(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var left []string
	for _, e := range entries {
		if err := removeTree(filepath.Join(dir, e.Name())); err != nil {
			left = append(left, e.Name())
		}
	}
	if len(left) == 0 {
		return ""
	}
	return fmt.Sprintf("%d folder(s) in %s could not be removed (a grade left them unremovable): inspect them, then remove them (chmod -RN may be needed)", len(left), dir)
}
