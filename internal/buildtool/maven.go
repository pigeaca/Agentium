package buildtool

import "path/filepath"

// mavenProfile holds Maven's special cases; the repository's own wrapper (mvnw) is preferred, as the user decided.
//
// Offline (proved in a real session on FasterXML/jackson-core): setup runs the build with network, the wrapper's
// distribution going to MAVEN_USER_HOME and dependencies to a repository, both in the deps folder. The agent gets
// JAVA_HOME resolved on the host (the sandbox cannot look a JDK up), that MAVEN_USER_HOME (read only), and
// MAVEN_ARGS=-o -Dmaven.repo.local=<run cache>/m2 -Dmaven.repo.local.tail=<deps>/m2: Maven reads dependencies from the
// read-only tail and writes nothing outside the run's cache (its per-run repository stays empty).
//
// Agentium's own commands keep their local repository and wrapper downloads in the data folder; the user's whole ~/.m2
// is denied to agents (installed artifacts, settings.xml with credentials, wrapper distributions: the recipe needs
// none of them, the wrapper's come from the deps folder).
func mavenProfile() Profile {
	return Profile{
		Name:   "maven",
		Detect: []string{"pom.xml"},
		TestCommand: func(has func(string) bool) string {
			if has("mvnw") {
				return "./mvnw -q test"
			}
			return "mvn -q test"
		},
		Languages: []string{"java", "kotlin", "scala"},
		Runners:   []string{"mvn", "mvnw"},
		Configs: []string{"pom.xml", ".mvn/maven.config", ".mvn/jvm.config", ".mvn/extensions.xml", ".mvn/settings.xml",
			".mvn/wrapper/maven-wrapper.properties"},
		// mvn, mvnw and ./mvnw, with flags and goals before the phase: `mvn -q test`, `./mvnw clean verify`.
		TestPatterns:  []string{`mvnw?( \S+)* (test|verify|integration-test|surefire:test|failsafe:integration-test)`},
		EnvNames:      []string{"M2_HOME", "MAVEN_HOME"},
		CommandCaches: []CacheVar{{Name: "MAVEN_USER_HOME", Dir: "maven"}},
		CommandVars: func(cache string) []string {
			return []string{"MAVEN_ARGS=-Dmaven.repo.local=" + filepath.Join(cache, "m2")}
		},
		AgentEnv: func(c AgentContext) []string {
			var env []string
			if c.JavaHome != "" {
				env = append(env, "JAVA_HOME="+c.JavaHome)
			}
			if c.Deps != "" && c.BuildCache != "" {
				env = append(env, "MAVEN_USER_HOME="+filepath.Join(c.Deps, "mvnw-home"),
					"MAVEN_ARGS=-o -Dmaven.repo.local="+filepath.Join(c.BuildCache, "m2")+" -Dmaven.repo.local.tail="+filepath.Join(c.Deps, "m2"))
			}
			return env
		},
		// A test goal that selects no test: it resolves and compiles everything the tests need and fetches the test
		// plugin, without running the project's tests.
		Warm: func(has func(string) bool, deps string) []WarmStep {
			mvn := "mvn"
			if has("mvnw") {
				mvn = "./mvnw"
			}
			return []WarmStep{{
				Command: mvn + " -B -q test -Dtest=AgentiumWarmNoSuchTest -Dsurefire.failIfNoSpecifiedTests=false -DfailIfNoTests=false",
				Env:     []string{"MAVEN_USER_HOME=" + filepath.Join(deps, "mvnw-home"), "MAVEN_ARGS=-Dmaven.repo.local=" + filepath.Join(deps, "m2")},
			}}
		},
		UserCaches: func(environ []string, home string) []string {
			return []string{filepath.Join(home, ".m2"), userHome(environ, "MAVEN_USER_HOME", filepath.Join(home, ".m2"))}
		},
	}
}
