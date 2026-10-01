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
				{Command: gradle + " --no-daemon --console=plain -q testClasses", Env: env},
				{Command: gradle + " --no-daemon --console=plain -q test --tests AgentiumWarmNoSuchTest || true", Env: env},
			}
		},
		PrepareRun:      prepareGradleRun,
		StopRun:         stopGradleDaemons,
		PrepareCommands: prepareGradleCommands,
		UserCaches:      gradleCaches,
	}
}

// prepareGradleRun makes the run's GRADLE_USER_HOME: no daemon, and the wrapper's distributions cloned from the deps
// folder when setup fetched them.
func prepareGradleRun(ctx context.Context, deps, buildCache string) error {
	guh := filepath.Join(buildCache, "gradle")
	if err := os.MkdirAll(guh, 0o700); err != nil {
		return err
	}
	props := "org.gradle.daemon=false\n"
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

// maxDaemonLogs caps how many daemon logs a stop reads: a run has a few daemons, and the agent can write more.
const maxDaemonLogs = 16

// stopGradleDaemons ends the Gradle daemons the run started. They are found through their logs in the run's own
// user home (daemon/<version>/daemon-<pid>.out.log), which the agent can write: so a log counts only when it is a
// plain file that really lies inside the run's Gradle home (no link out of it), and a process is signalled only when
// its command line is a Gradle daemon AND it has that very log open (a daemon keeps its log open while it runs). A
// planted log naming the user's own daemon, another run's, or any other process therefore kills nothing. When the
// machine cannot tell (no lsof), nothing is signalled and the error says so. Nothing in the checkout is run: the agent
// may have changed gradlew.
func stopGradleDaemons(ctx context.Context, buildCache string, host Host) error {
	root, err := filepath.EvalSymlinks(filepath.Join(buildCache, "gradle"))
	if err != nil {
		return nil // no Gradle home: no daemon
	}
	logs, err := filepath.Glob(filepath.Join(root, "daemon", "*", "daemon-*.out.log"))
	if err != nil {
		return err
	}
	slices.Sort(logs)
	var errs []error
	for _, log := range logs[:min(len(logs), maxDaemonLogs)] {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(log), "daemon-"), ".out.log")
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 {
			continue
		}
		real, err := filepath.EvalSymlinks(log)
		info, lerr := os.Lstat(log)
		if err != nil || lerr != nil || !info.Mode().IsRegular() || !strings.HasPrefix(real, root+string(filepath.Separator)) {
			continue
		}
		if !strings.Contains(host.Command(ctx, pid), "GradleDaemon") {
			continue
		}
		open, err := host.OpenFiles(ctx, pid)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if slices.Contains(open, real) {
			host.terminate(ctx, pid)
		}
	}
	return errors.Join(errs...)
}

// prepareGradleDeps turns the cache cleanup of the deps folder's Gradle home off, both as a property and as an init
// script (which one a Gradle version honors varies): cleanup deletes cache entries that a build has not used for a
// while, and agents read these files while later warm-ups run.
func prepareGradleDeps(deps string) error {
	guh := filepath.Join(deps, "gradle")
	if err := os.MkdirAll(filepath.Join(guh, "init.d"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(guh, "gradle.properties"), []byte("org.gradle.daemon=false\norg.gradle.cache.cleanup=false\n"), 0o600); err != nil {
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
	return os.WriteFile(filepath.Join(guh, "gradle.properties"), []byte("org.gradle.daemon=false\n"), 0o600)
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
