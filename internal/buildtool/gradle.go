package buildtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// read-only cache (GRADLE_RO_DEP_CACHE). Gradle's file-lock service binds a local UDP socket, so Gradle projects alone
// get the sandbox's allowLocalBinding (outbound network stays blocked).
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
		Warm: func(has func(string) bool, deps string) []WarmStep {
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
		PrepareRun: prepareGradleRun,
		StopRun:    stopGradleDaemons,
		UserCaches: gradleCaches,
	}
}

// prepareGradleRun makes the run's GRADLE_USER_HOME: no daemon, and the wrapper's distributions cloned from the deps
// folder when setup fetched them.
func prepareGradleRun(ctx context.Context, deps, buildCache string) error {
	guh := filepath.Join(buildCache, "gradle")
	if err := os.MkdirAll(guh, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(guh, "gradle.properties"), []byte("org.gradle.daemon=false\n"), 0o600); err != nil {
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

// stopGradleDaemons ends the Gradle daemons the run started. They are found through their logs in the run's own
// user home (daemon/<version>/daemon-<pid>.out.log), and only a process whose command line is a Gradle daemon is
// signalled. The agent can write those logs, so it could name another process: the check limits that to a Gradle
// daemon of the same user, which restarts by itself. Nothing in the checkout is run: the agent may have changed gradlew.
func stopGradleDaemons(buildCache string, host Host) error {
	logs, err := filepath.Glob(filepath.Join(buildCache, "gradle", "daemon", "*", "daemon-*.out.log"))
	if err != nil {
		return err
	}
	for _, log := range logs {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(log), "daemon-"), ".out.log")
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 {
			continue
		}
		if strings.Contains(host.Command(pid), "GradleDaemon") {
			host.terminate(pid)
		}
	}
	return nil
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
