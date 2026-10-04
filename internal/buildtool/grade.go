package buildtool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// A sandboxed grade (the isolation plan, Part 1) builds and tests with the agent's recipe, not with the environment of
// Agentium's own host commands (CommandEnv): the agent and its grader see the same toolchain, dependencies (the deps
// folder, read-only) and settings, which the isolation plan's step 0 proved offline under the grading sandbox for Go,
// Maven, Gradle, Cargo and Python. Its writable folders are its own: the grading copy, a cache cloned from a seed
// (SeedKey) and a temp root.

// GraderEnv is the whole environment of a sandboxed grade's commands (runner.Spec.Environ). It is the agent's recipe
// for the same run, with the grade's own folders in place of the agent's. In order, a later entry replacing an
// earlier one of the same name:
//   - allowed: the agent's allowlisted environment (sandbox.EnvironFor of the user's), without the variables set below;
//   - the selected profiles' AgentEnv, with c.Repo the grading copy and c.BuildCache the grade's own cache (Go's
//     GOFLAGS, the JVM tools' JAVA_HOME and offline settings, Cargo's offline home, Python's venv);
//   - their AgentCacheEnv in c.BuildCache (Go's GOCACHE);
//   - TMPDIR and the profiles' TempVars (Go's GOTMPDIR) set to temp, the grade's temp root: the agent's TMPDIR is Claude
//     Code's own folder in the run's temp root, which the grade does not get;
//   - their GradeEnv: what the grading sandbox needs besides (the JVM's temp folder, Gradle's bind address, Go offline).
//
// Claude Code's own variables and the run's sign-in are never in it: they are not on the allowlist, and nothing here
// adds them. Each name appears once. c.Repo, c.BuildCache and temp must be absolute; temp may hold no white space or
// control character, since JAVA_TOOL_OPTIONS splits its value on white space and cannot quote it.
func GraderEnv(selected []Profile, allowed []string, c AgentContext, temp string) ([]string, error) {
	for name, p := range map[string]string{"grading copy": c.Repo, "grading cache": c.BuildCache, "temp root": temp} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("the grade's %s %q is not absolute", name, p)
		}
	}
	if strings.ContainsFunc(temp, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return nil, fmt.Errorf("the grade's temp root %q holds white space or a control character, which JAVA_TOOL_OPTIONS cannot carry", temp)
	}
	c.Allowed = allowed // Python's venv goes first on the allowlisted PATH
	set := AgentEnv(selected, c)
	set = append(set, AgentCacheEnv(selected, c.BuildCache)...)
	set = append(set, "TMPDIR="+temp)
	for _, p := range selected {
		if !p.agentSide() {
			continue
		}
		for _, name := range p.TempVars {
			set = append(set, name+"="+temp)
		}
	}
	for _, p := range selected {
		if p.GradeEnv != nil && p.agentSide() {
			set = append(set, p.GradeEnv(temp)...)
		}
	}
	replaced := map[string]bool{}
	for _, kv := range set {
		name, _, _ := strings.Cut(kv, "=")
		replaced[name] = true
	}
	var env []string
	for _, kv := range allowed {
		if name, _, _ := strings.Cut(kv, "="); !replaced[name] {
			env = append(env, kv)
		}
	}
	return lastWins(append(env, set...)), nil
}

// lastWins keeps one entry per name: the last one's value, at the first one's place.
func lastWins(environ []string) []string {
	at := map[string]int{}
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if i, ok := at[name]; ok {
			out[i] = kv
			continue
		}
		at[name] = len(out)
		out = append(out, kv)
	}
	return out
}

// jvmGradeEnv points the JVM's temporary files at the grade's temp root. On macOS the JVM takes java.io.tmpdir from
// the user's /var/folders/<x>/T, not from TMPDIR, and the grading sandbox denies writes there (jackson-core: 5 errors
// without it in the isolation plan's step 0). It carries no -Djava.net.preferIPv4Stack: that flag breaks Gradle's
// bind address (gradleGradeEnv). The user's own JAVA_TOOL_OPTIONS is not on the agent's allowlist, so nothing is lost.
func jvmGradeEnv(temp string) []string {
	return []string{"JAVA_TOOL_OPTIONS=-Djava.io.tmpdir=" + temp}
}

// gradleGradeEnv is the JVM's temp folder, and Gradle's daemon bind address on IPv6 loopback. Gradle's worker daemons
// (Checkstyle, forked compilers) connect back to the build over the address it announces, 127.0.0.1 by default, which a
// JVM opens as ::ffff:127.0.0.1: no seatbelt rule matches that address, so under the grading sandbox's loopback rules
// Gradle does not even start. With ::1 the build listens on and announces an address the localhost rules match
// (Gradle 9.7.1 reads the variable; earlier versions were not checked). Proved in the isolation plan's step 0.
func gradleGradeEnv(temp string) []string {
	return append(jvmGradeEnv(temp), "GRADLE_DAEMON_BIND_ADDRESS=::1")
}

// seedRecipe versions what a grading cache's seed holds besides the profiles' hooks: change it when the seed's making
// changes, so seeds made the old way are made again (SeedKey).
const seedRecipe = "grading-seed-1: the selected profiles' PrepareRun, then the caller's trusted warm step, published by rename"

// SeedKey names the seed of the grading caches for runs with the selected profiles (one seed per base commit as
// well; the caller adds it): the profiles that shape a grade's cache, joined by "+", and a short hash of what does
// (their agent side, their warm-up recipes, PrepareRun's version in seedRecipe). Two tool sets never share a seed.
func SeedKey(selected []Profile) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n", seedRecipe, WarmVersion(selected))
	var names []string
	for _, p := range selected {
		fmt.Fprintf(h, "%s %t %s\n", p.Name, p.agentSide(), p.WarmRecipe)
		names = append(names, p.Name)
	}
	return strings.Join(names, "+") + "-" + hex.EncodeToString(h.Sum(nil))[:8]
}
