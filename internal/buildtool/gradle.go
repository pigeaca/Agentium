package buildtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// gradleProfile holds Gradle's special cases (Java or Kotlin projects); the repository's own wrapper (gradlew) is
// preferred, as the user decided.
//
// Offline (proved in a real session on junit-pioneer/junit-pioneer): setup warms a Gradle user home in the deps folder
// with network. The agent's GRADLE_USER_HOME is its own, in the run cache, because Gradle writes there; the run's
// preparation clones the wrapper's distribution into it (copy-on-write where the file system can) and writes
// org.gradle.daemon=false to its gradle.properties, which beats the project's. Dependencies come from the deps folder's
// read-only cache (GRADLE_RO_DEP_CACHE, gradleRO), which holds nothing but modules-2: agents are denied the deps
// folder's whole Gradle home (DepsDenied), whose other caches record what warm-ups compiled (see prepareGradleDeps for
// the layout). Gradle's file-lock service binds a local UDP socket, so Gradle projects need the
// sandbox's allowLocalBinding, which grants more than binding: any local port, inbound, and outbound to localhost
// (claude.LocalBindingRefusal). It is opt-in per project (agentium init --allow-local-binding); without it, agent runs
// on a Gradle project do not start.
//
// A daemon outlived its run's process group in the spike, with an 8 GB heap: so no daemon, and whatever remains is
// stopped when the run ends (stopGradleDaemons). Agentium's own commands keep their Gradle user home in the data folder.
//
// The user's caches, daemon registry, init scripts and gradle.properties are denied to agents; the wrapper's
// distributions under ~/.gradle/wrapper stay readable.
func gradleProfile() Profile {
	return Profile{
		Name:   "gradle",
		Detect: []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"},
		TestCommand: func(has func(string) bool) string {
			if has("gradlew") {
				return "./gradlew test"
			}
			return "gradle test"
		},
		Languages: []string{"java", "kotlin", "scala"},
		Runners:   []string{"gradle", "gradlew"},
		Configs: []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "gradle.properties",
			"gradle/libs.versions.toml", "gradle/wrapper/gradle-wrapper.properties"},
		// gradle and gradlew with options before the task: `./gradlew test`, `gradle :app:test --tests X`, `gradlew build`.
		TestPatterns:  []string{`gradlew?( \S+)* (\S*:)?(test|check|build|integrationTest)`},
		EnvNames:      []string{"GRADLE_HOME"},
		CommandCaches: []CacheVar{{Name: "GRADLE_USER_HOME", Dir: "gradle"}},
		AgentEnv: func(c AgentContext) []string {
			var env []string
			if c.JavaHome != "" {
				env = append(env, "JAVA_HOME="+c.JavaHome)
			}
			if c.BuildCache != "" {
				env = append(env, "GRADLE_USER_HOME="+filepath.Join(c.BuildCache, "gradle"))
			}
			if c.Deps != "" {
				env = append(env, "GRADLE_RO_DEP_CACHE="+filepath.Join(c.Deps, gradleRO))
			}
			return env
		},
		LocalBinding: true,
		// Three steps: the compile classpaths, the test task with a filter that selects nothing (it fails for that
		// reason, after resolving the test runtime classpath), then every other resolvable configuration of every
		// project (Checkstyle, JaCoCo, ...: the agent may run tasks that `test` does not reach), through an init script
		// given to this step only. The warm-up tolerates failures.
		PrepareDeps: prepareGradleDeps,
		WarmRecipe:  noCleanupScript + resolveAllScript + gradleLayout,
		Warm: func(_, deps string, has func(string) bool) []WarmStep {
			gradle := "gradle"
			if has("gradlew") {
				gradle = "./gradlew"
			}
			env := []string{"GRADLE_USER_HOME=" + filepath.Join(deps, "gradle")}
			return []WarmStep{
				{Command: gradle + " --no-daemon --no-build-cache --console=plain -q testClasses", Env: env},
				{Command: gradle + " --no-daemon --no-build-cache --console=plain -q test --tests AgentiumWarmNoSuchTest || true", Env: env},
				{Command: gradle + " --no-daemon --no-build-cache --console=plain -q -I '" + filepath.Join(deps, "gradle", resolveAllScriptName) + "' " + resolveAllTask + " || true", Env: env},
			}
		},
		PrepareRun:      prepareGradleRun,
		StopRun:         stopGradleDaemons,
		PrepareCommands: prepareGradleCommands,
		UserCaches:      gradleCaches,
	}
}

// gradleHomeProps are the settings of every Gradle user home Agentium writes (the run's, the deps folder's, the one for
// its own commands):
//   - no daemon: a daemon outlives its command with a heap of gigabytes;
//   - no build cache: compiled classes would land in a place later runs share (the deps folder, or the data folder's
//     cache), and a later task's base holds an earlier task's reference code and hidden tests. Turning it off changes no
//     test result, only speed, and it beats a project's own org.gradle.caching=true;
//   - Kotlin compiles in the Gradle process: a Kotlin compile daemon would outlive Agentium's own, unsandboxed commands,
//     listen on localhost, and (with local binding allowed) could compile an agent's code outside the sandbox.
const gradleHomeProps = "org.gradle.daemon=false\norg.gradle.caching=false\nkotlin.compiler.execution.strategy=in-process\n"

// prepareGradleRun makes the run's GRADLE_USER_HOME: no daemon, and the wrapper's distributions cloned from the deps
// folder when setup fetched them.
func prepareGradleRun(ctx context.Context, deps, buildCache string) error {
	guh := filepath.Join(buildCache, "gradle")
	if err := os.MkdirAll(guh, 0o700); err != nil {
		return err
	}
	props := gradleHomeProps
	if deps != "" {
		// Toolchain JDKs the warm-up downloaded live in the deps folder (gradleJDKs, outside its Gradle home, which the
		// agent may not read), not in this run's home: point Gradle there, and never let it download one (the sandbox
		// has no network anyway).
		props += "org.gradle.java.installations.paths=" + filepath.Join(deps, gradleJDKs) + "\n" +
			"org.gradle.java.installations.auto-download=false\n"
	}
	if err := os.WriteFile(filepath.Join(guh, "gradle.properties"), []byte(props), 0o600); err != nil {
		return err
	}
	// Offline whatever the build says: a dynamic version (1.+, latest.release) would otherwise try the network to find
	// the newest one, and fail slowly; offline, Gradle uses what the read-only cache holds.
	if err := os.MkdirAll(filepath.Join(guh, "init.d"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(guh, "init.d", "agentium-offline.gradle"), []byte("gradle.startParameter.offline = true\n"), 0o600); err != nil {
		return err
	}
	if deps == "" {
		return nil
	}
	wrapper := filepath.Join(deps, "gradle", "wrapper")
	if _, err := os.Stat(wrapper); errors.Is(err, os.ErrNotExist) {
		return nil // a project without a wrapper, or a warm-up that fetched none
	} else if err != nil {
		return err
	}
	if err := cloneTree(ctx, wrapper, filepath.Join(guh, "wrapper")); err != nil {
		return fmt.Errorf("clone the wrapper distribution: %w", err)
	}
	return nil
}

// stopGradleDaemons ends the Gradle daemons the run started. It starts from the machine's own process list (the user's
// GradleDaemon processes), never from files the agent can write, and signals a process only when it holds open the log
// that daemon would keep in the run's own Gradle home: <buildCache>/gradle/daemon/<version>/daemon-<pid>.out.log, with
// its pid.
//
// buildCache is the run's build cache RESOLVED ONCE, by the caller, before the agent started: the sandbox lets the
// agent write the build cache path itself, so afterwards it could move the folder away and put a link to another run's
// folder, or to the user's Gradle home, in its place. So this touches no file-system state at stop time: lsof reports
// real paths, and they are compared textually with the pre-resolved root. A daemon the agent moved elsewhere (its own
// -Dgradle.user.home, or a renamed log) is not found, like a daemon that left the process group with setsid: a known
// limitation, not a way to kill anything. When the machine cannot tell what a process has open (no lsof), nothing is
// signalled and the error says so. Nothing in the checkout is run: the agent may have changed gradlew.
func stopGradleDaemons(ctx context.Context, buildCache string, host Host) error {
	root := filepath.Join(buildCache, "gradle")
	pids, err := host.Daemons(ctx)
	if err != nil {
		return err
	}
	var errs []error
	handled := map[int]bool{}
	for _, pid := range pids {
		holds := func(ctx context.Context) (bool, error) {
			open, err := host.OpenFiles(ctx, pid)
			wanted := "daemon-" + strconv.Itoa(pid) + ".out.log"
			// A log with other hard links is not the daemon's own (belt and braces: the sandbox already stops the agent
			// linking files outside its folders); an unknown count (0) is accepted.
			return slices.ContainsFunc(open, func(f OpenFile) bool { return f.Links <= 1 && daemonLog(root, f.Name, wanted) }), err
		}
		if ok, err := holds(ctx); err != nil {
			errs = append(errs, err)
		} else if ok {
			handled[pid] = true
			// The match is repeated just before SIGKILL, with a context of its own: the pid may have been reused.
			host.terminate(ctx, pid, func() bool {
				again, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				ok, err := holds(again)
				return ok && err == nil
			})
		}
	}
	if left := unmatchedDaemons(root, handled, host); len(left) > 0 {
		errs = append(errs, fmt.Errorf("the run's Gradle home names running process(es) %v as daemons that could not be matched to an open log (a renamed or deleted log, a case mismatch): a Gradle daemon may have been left running", left))
	}
	return errors.Join(errs...)
}

// unmatchedDaemons lists pids named by daemon logs in the run's Gradle home that are still running but were not
// handled: only a note for the record, nothing is signalled on this evidence (the logs are the agent's to write).
func unmatchedDaemons(root string, handled map[int]bool, host Host) []int {
	logs, _ := filepath.Glob(filepath.Join(root, "daemon", "*", "daemon-*.out.log"))
	slices.Sort(logs)
	var left []int
	for _, log := range logs[:min(len(logs), 64)] {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(log), "daemon-"), ".out.log")
		if pid, err := strconv.Atoi(name); err == nil && pid > 1 && !handled[pid] && host.Signal(pid, 0) == nil && !slices.Contains(left, pid) {
			left = append(left, pid)
		}
	}
	return left
}

// daemonLog reports whether file is <root>/daemon/<version>/<name>.
func daemonLog(root, file, name string) bool {
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 3 && parts[0] == "daemon" && parts[1] != ".." && parts[2] == name
}

// prepareGradleDeps lays out the deps folder's Gradle home (gradleLayout), then turns its cache cleanup off, both as a
// property and as an init script (which one a Gradle version honors varies): cleanup deletes cache entries that a build
// has not used for a while, and agents read these files while later warm-ups run. It runs under the warm-up lock.
func prepareGradleDeps(deps string) error {
	guh := filepath.Join(deps, "gradle")
	if err := os.MkdirAll(filepath.Join(guh, "init.d"), 0o700); err != nil {
		return err
	}
	for _, s := range gradleShared {
		if err := linkOutside(filepath.Join(guh, s.inHome), filepath.Join(deps, s.outside)); err != nil {
			return fmt.Errorf("lay out the Gradle home: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(guh, "gradle.properties"), []byte(gradleHomeProps+"org.gradle.cache.cleanup=false\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(guh, "init.d", "agentium-no-cleanup.gradle"), []byte(noCleanupScript), 0o600); err != nil {
		return err
	}
	os.Remove(filepath.Join(guh, "init.d", "agentium-resolve-all.gradle")) // an earlier recipe's place: it must not load in every step
	return os.WriteFile(filepath.Join(guh, resolveAllScriptName), []byte(resolveAllScript), 0o600)
}

// gradleRO is the deps folder's read-only dependency cache for agents (GRADLE_RO_DEP_CACHE), and gradleJDKs its
// toolchain JDKs (org.gradle.java.installations.paths): both outside its Gradle home, deps/gradle, which agents may not
// read at all (DepsDenied).
const (
	gradleRO   = "gradle-ro"
	gradleJDKs = "gradle-jdks"
)

// gradleShared are the folders of the deps Gradle home that agents need (inHome, relative to it), each kept outside the
// home (outside, relative to the deps folder) with a symbolic link in its place, so warm-ups write them where Gradle
// expects and agents read them where nothing else is.
//
// Why links from the home out, and not the other way round: the sandbox matches a file's real path, so a link from
// gradle-ro into the denied home would be denied too; a copy refreshed after each warm-up would cost time and space per
// warm-up (a full copy where the file system cannot clone) and old copies could not be removed while a build reads
// them. With the links, gradle-ro holds only modules-2 by construction, whatever a later warm-up adds to the home (a
// new Gradle version's caches/<version>, transforms, jars), and stays current with no refresh. Gradle asks that a
// read-only cache not change while builds use it; warm-ups only add files to modules-2, with cleanup off, as they did
// when agents read caches/modules-2 itself.
var gradleShared = []struct{ inHome, outside string }{
	{filepath.Join("caches", "modules-2"), filepath.Join(gradleRO, "modules-2")},
	{"jdks", gradleJDKs},
}

// gradleLayout states the layout in the warm-up's recipe (WarmRecipe): a deps folder laid out before it is laid out
// again, since only a warm-up, under its lock, makes the links, and the stamps of the earlier recipe are not found.
const gradleLayout = "layout: caches/modules-2 -> ../../gradle-ro/modules-2, jdks -> ../gradle-jdks\n"

// linkOutside makes link a relative symbolic link to the folder outside, creating the folder when missing. A folder at
// link (an earlier layout) is moved to outside in one rename, so an agent of an earlier Agentium reading through link
// finds it again once the link is made. It refuses, changing nothing, when both are folders (which one is current is
// unknown: remove one), or when either is something else: only Agentium writes the deps folder, so these mean it was
// changed by hand. It runs under the warm-up lock; a crash between the rename and the link leaves a state the next call
// completes.
func linkOutside(link, outside string) error {
	target, err := filepath.Rel(filepath.Dir(link), outside)
	if err != nil {
		return err
	}
	isDir := func(path string) (bool, error) {
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return false, nil
		case err != nil:
			return false, err
		case !info.IsDir():
			return false, fmt.Errorf("%s is not a folder", path)
		}
		return true, nil
	}
	info, err := os.Lstat(link)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		if got, err := os.Readlink(link); err == nil && got == target {
			if ok, err := isDir(outside); ok || err != nil {
				return err
			}
		}
		if err := os.Remove(link); err != nil { // another target, or a dangling link: made again below
			return err
		}
	case info.IsDir():
		if ok, err := isDir(outside); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("%s and %s are both folders: remove one (the deps folder holds only downloads, fetched again by the next warm-up)", link, outside)
		}
		if err := os.MkdirAll(filepath.Dir(outside), 0o700); err != nil {
			return err
		}
		if err := os.Rename(link, outside); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s is not a folder or a link", link)
	}
	if _, err := isDir(outside); err != nil {
		return err
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// noCleanupScript turns the cache cleanup off. settings.caches is Gradle 8.0+; older versions honor the property
// (gradleHomeProps) and skip this.
const noCleanupScript = `beforeSettings { settings ->
    if (settings.metaClass.respondsTo(settings, "getCaches")) {
        settings.caches { cleanup = Cleanup.DISABLED }
    }
}
`

// resolveAllTask is the task resolveAllScript adds to every project of the warm-up's build.
const resolveAllTask = "agentiumResolveAll"

// resolveAllScriptName is the script's file in the deps folder's Gradle home. It is not in init.d: only the warm-up
// step that needs it loads it (-I), so the other steps run as they always did, and no agent's home reads it.
const resolveAllScriptName = "agentium-resolve-all.gradle"

// resolveAllScript is an init script that adds resolveAllTask to every project: it resolves every resolvable
// configuration, which downloads the artifacts into the read-only cache. Without it a build's other tasks fail offline
// in the agent's sandbox (junit-pioneer: ":checkstyle ... No cached version of com.puppycrawl.tools:checkstyle ...").
// A configuration that cannot be resolved here (one that needs attributes only its consumer sets, a failing
// repository) is skipped so it does not stop the others, and listed with its reason in WarmSkippedFile at the
// build's root (read by the run's setup). The configurations are taken at configuration time, not through
// Task.project at execution time; the task still declares itself incompatible with the configuration cache, where
// Gradle knows that call, since resolving at execution is not something it can store.
const resolveAllScript = `allprojects {
    def confs = project.configurations
    def projectPath = project.path
    def skipped = new File(rootProject.projectDir, "` + WarmSkippedFile + `")
    tasks.register("agentiumResolveAll") {
        if (it.respondsTo("notCompatibleWithConfigurationCache", String)) {
            it.notCompatibleWithConfigurationCache("resolves every configuration of the project")
        }
        doLast {
            confs.matching { it.canBeResolved }.all { conf ->
                try {
                    conf.resolve()
                } catch (Exception e) {
                    def reasons = []
                    for (Throwable t = e; t != null && reasons.size() < 6; t = t.cause) {
                        reasons << String.valueOf(t.message).replaceAll("\\s+", " ")
                    }
                    println "agentium: skipped ${projectPath}:${conf.name}"
                    skipped << "${projectPath}:${conf.name}\t${reasons.join(' | ')}\n"
                }
            }
        }
    }
}
`

// prepareGradleCommands gives Agentium's own Gradle commands (validation, grading) a user home under the cache folder
// that never starts a daemon: a daemon would outlive the command with a heap of gigabytes.
func prepareGradleCommands(cache string) error {
	guh := filepath.Join(cache, "gradle")
	if err := os.MkdirAll(guh, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(guh, "gradle.properties"), []byte(gradleHomeProps), 0o600)
}

// gradleCaches are the user's Gradle folders that hold compiled outputs (caches, with the build cache), daemon state,
// scripts and credentials (gradle.properties), under GRADLE_USER_HOME or ~/.gradle. wrapper/ is left readable.
func gradleCaches(environ []string, home string) []string {
	// Both the default folder and GRADLE_USER_HOME: an earlier build may have used either.
	homes := []string{filepath.Join(home, ".gradle")}
	if guh := userHome(environ, "GRADLE_USER_HOME", homes[0]); guh != homes[0] {
		homes = append(homes, guh)
	}
	var paths []string
	for _, guh := range homes {
		for _, name := range []string{"caches", "daemon", "native", "jdks", "init.d", "init.gradle", "init.gradle.kts", "gradle.properties",
			"build-scan-data", ".tmp", "workers", "notifications"} {
			paths = append(paths, filepath.Join(guh, name))
		}
	}
	return paths
}
