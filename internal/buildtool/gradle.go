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
)

// gradleProfile holds Gradle's special cases (Java or Kotlin projects); the repository's own wrapper (gradlew) is
// preferred, as the user decided.
//
// Offline (proved in a real session on junit-pioneer/junit-pioneer): setup warms a Gradle user home in the deps folder
// with network. The agent's GRADLE_USER_HOME is its own, in the run cache, because Gradle writes there; the run's
// preparation clones the wrapper's distribution into it (copy-on-write where the file system can) and writes
// org.gradle.daemon=false to its gradle.properties, which beats the project's. Dependencies come from the deps folder's
// read-only cache (GRADLE_RO_DEP_CACHE). Gradle's file-lock service binds a local UDP socket, so Gradle projects need the
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
				env = append(env, "GRADLE_RO_DEP_CACHE="+filepath.Join(c.Deps, "gradle", "caches"))
			}
			return env
		},
		LocalBinding: true,
		// Two steps: the compile classpaths, then the test task with a filter that selects nothing (it fails for that
		// reason, after resolving the test runtime classpath, and the warm-up tolerates failures).
		PrepareDeps: prepareGradleDeps,
		Warm: func(_, deps string, has func(string) bool) []WarmStep {
			gradle := "gradle"
			if has("gradlew") {
				gradle = "./gradlew"
			}
			env := []string{"GRADLE_USER_HOME=" + filepath.Join(deps, "gradle")}
			return []WarmStep{
				{Command: gradle + " --no-daemon --no-build-cache --console=plain -q testClasses", Env: env},
				{Command: gradle + " --no-daemon --no-build-cache --console=plain -q test --tests AgentiumWarmNoSuchTest || true", Env: env},
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
		// Toolchain JDKs the warm-up downloaded live in the deps folder's Gradle home, not in this run's: point Gradle
		// there, and never let it download one (the sandbox has no network anyway).
		props += "org.gradle.java.installations.paths=" + filepath.Join(deps, "gradle", "jdks") + "\n" +
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
// its pid. So an agent cannot make Agentium kill the user's daemon, another run's, or a process it names, and planted
// logs cannot hide the real daemon.
//
// The root is the run's build cache resolved once, plus "gradle": the build cache is a folder Agentium made before the
// agent started, and the agent cannot replace it (the sandbox lets it write inside the folder, not in its parent), but
// it can replace <buildCache>/gradle, so that is checked not to be a link. When the machine cannot tell
// what a process has open (no lsof), nothing is signalled and the error says so. Nothing in the checkout is run: the
// agent may have changed gradlew.
func stopGradleDaemons(ctx context.Context, buildCache string, host Host) error {
	resolved, err := filepath.EvalSymlinks(buildCache)
	if err != nil {
		return nil // no build cache: nothing ran
	}
	root := filepath.Join(resolved, "gradle")
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		return nil // no Gradle home, or one the agent replaced with a link or a file: no daemon of this run's is there
	}
	pids, err := host.Daemons(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, pid := range pids {
		open, err := host.OpenFiles(ctx, pid)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		wanted := "daemon-" + strconv.Itoa(pid) + ".out.log"
		if slices.ContainsFunc(open, func(f string) bool { return daemonLog(root, f, wanted) }) {
			host.terminate(ctx, pid)
		}
	}
	return errors.Join(errs...)
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

// prepareGradleDeps turns the cache cleanup of the deps folder's Gradle home off, both as a property and as an init
// script (which one a Gradle version honors varies): cleanup deletes cache entries that a build has not used for a
// while, and agents read these files while later warm-ups run.
func prepareGradleDeps(deps string) error {
	guh := filepath.Join(deps, "gradle")
	if err := os.MkdirAll(filepath.Join(guh, "init.d"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(guh, "gradle.properties"), []byte(gradleHomeProps+"org.gradle.cache.cleanup=false\n"), 0o600); err != nil {
		return err
	}
	script := "beforeSettings { settings ->\n    settings.caches { cleanup = Cleanup.DISABLED }\n}\n"
	return os.WriteFile(filepath.Join(guh, "init.d", "agentium-no-cleanup.gradle"), []byte(script), 0o600)
}

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
