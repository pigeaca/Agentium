package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// toolInvocation is a run in a repository with the named build tools, with the run's own build cache and a deps folder.
func toolInvocation(t *testing.T, tools ...string) Invocation {
	t.Helper()
	inv := invocation(t, SignInLogin, "")
	inv.Tools, inv.BuildCache = tools, "/work/runs/r1/go-build"
	inv.Deps, inv.JavaHome = "/data/deps/1", "/host/jdk"
	inv.AllowLocalBinding = true // the user's opt-in, which only Gradle projects use
	return inv
}

func toolCommand(t *testing.T, inv Invocation, environ []string) (env map[string]string, settings map[string]any) {
	t.Helper()
	args, list, err := inv.Command(environ)
	if err != nil {
		t.Fatal(err)
	}
	env = map[string]string{}
	for _, kv := range list {
		name, value, _ := strings.Cut(kv, "=")
		if _, dup := env[name]; dup {
			t.Errorf("%s is set twice in the agent's environment", name)
		}
		env[name] = value
	}
	if err := json.Unmarshal([]byte(flagValue(args, "--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	return env, settings
}

func sandbox(settings map[string]any) (network, filesystem map[string]any) {
	sb := settings["sandbox"].(map[string]any)
	return sb["network"].(map[string]any), sb["filesystem"].(map[string]any)
}

// The user's environment has every tool's variables: each profile keeps the offline recipe's settings, and the
// user's own never reach the agent over them.
var userTools = append(slices.Clone(parentEnv), "JAVA_HOME=/user/jdk", "MAVEN_OPTS=-Dmaven.repo.local=/home/u/.m2/repository",
	"MAVEN_ARGS=-Dmaven.repo.local=/home/u/.m2/repository", "GRADLE_USER_HOME=/home/u/.gradle", "GRADLE_OPTS=-Xmx8g",
	"CARGO_HOME=/home/u/.cargo", "RUSTC_WRAPPER=sccache", "CARGO_TARGET_DIR=/var/target", "SCCACHE_DIR=/var/sccache")

func TestMavenRunEnvironmentAndSandbox(t *testing.T) {
	inv := toolInvocation(t, "maven")
	env, settings := toolCommand(t, inv, userTools)
	want := map[string]string{"JAVA_HOME": "/host/jdk", "MAVEN_USER_HOME": "/data/deps/1/mvnw-home",
		"MAVEN_ARGS": "-o -Dmaven.repo.local=/work/runs/r1/go-build/m2 -Dmaven.repo.local.tail=/data/deps/1/m2 -Dmaven.build.cache.enabled=false"}
	for name, v := range want {
		if env[name] != v {
			t.Errorf("%s = %q, want %q", name, env[name], v)
		}
	}
	if _, ok := env["MAVEN_OPTS"]; ok {
		t.Error("MAVEN_OPTS reaches the agent: it could point the repository back at the user's")
	}
	network, fs := sandbox(settings)
	if _, ok := network["allowLocalBinding"]; ok {
		t.Error("Maven projects get no local binding")
	}
	if allow, _ := fs["allowWrite"].([]any); len(allow) != 1 || allow[0] != "/work/runs/r1/go-build" {
		t.Errorf("allowWrite = %v: only the run's own cache", fs["allowWrite"])
	}
	if writes, _ := fs["denyWrite"].([]any); !slices.Contains(writes, any("/data/deps/1")) {
		t.Errorf("the deps folder is not stated read-only: %v", writes)
	}
	if deny, _ := fs["denyRead"].([]any); slices.Contains(deny, any("/data/deps/1")) || slices.Contains(deny, any("/data/deps")) {
		t.Error("the deps folder is denied: offline builds could not read it")
	}
	denied := inv.DeniedPaths(userTools)
	// The deps folder's Gradle home and build cache are denied (the dependency caches agents read are not): what
	// warm-ups compiled, with earlier tasks' reference code and hidden tests, must not be readable.
	for _, p := range []string{"/data/deps/1/gradle", "/data/deps/1/build-cache"} {
		if !slices.Contains(denied, p) {
			t.Errorf("%s is not denied", p)
		}
	}
	if slices.Contains(denied, "/data/deps/1/m2") || slices.Contains(denied, "/data/deps/1/gradle-ro") || slices.Contains(denied, "/data/deps/1/gradle-jdks") {
		t.Error("the read-only dependency caches are denied")
	}
	for _, p := range []string{"/home/u/.m2"} {
		if !slices.Contains(denied, p) {
			t.Errorf("%s is not denied", p)
		}
	}
	// Go's special cases stay, as for every project.
	if env["GOFLAGS"] != "-buildvcs=false" {
		t.Errorf("GOFLAGS = %q", env["GOFLAGS"])
	}
}

func TestGradleRunEnvironmentAndSandbox(t *testing.T) {
	inv := toolInvocation(t, "gradle")
	env, settings := toolCommand(t, inv, userTools)
	want := map[string]string{"JAVA_HOME": "/host/jdk", "GRADLE_USER_HOME": "/work/runs/r1/go-build/gradle", "GRADLE_RO_DEP_CACHE": "/data/deps/1/gradle-ro"}
	for name, v := range want {
		if env[name] != v {
			t.Errorf("%s = %q, want %q", name, env[name], v)
		}
	}
	if _, ok := env["GRADLE_OPTS"]; ok {
		t.Error("GRADLE_OPTS reaches the agent")
	}
	network, fs := sandbox(settings)
	if network["allowLocalBinding"] != true {
		t.Errorf("Gradle's file-lock service binds a local socket: %v", network)
	}
	// allowLocalBinding is a grant of its own (bind any local port, inbound, outbound to localhost): opt-in, and a run
	// without it does not start; non-Gradle projects never get it, opt-in or not.
	refused := inv
	refused.AllowLocalBinding = false
	if _, _, err := refused.Command(userTools); err == nil || !strings.Contains(err.Error(), "agentium init --allow-local-binding") ||
		!strings.Contains(err.Error(), "localhost") {
		t.Errorf("a Gradle run without the opt-in: %v", err)
	}
	if err := LocalBindingRefusal([]string{"gradle"}, true); err != nil {
		t.Errorf("with the opt-in: %v", err)
	}
	for _, tools := range [][]string{nil, {"go"}, {"maven"}, {"cargo"}, {"maven", "cargo"}} {
		if err := LocalBindingRefusal(tools, false); err != nil {
			t.Errorf("%v is refused without the opt-in: %v", tools, err)
		}
		_, other := toolCommand(t, toolInvocation(t, tools...), userTools)
		if n, _ := sandbox(other); n["allowLocalBinding"] != nil {
			t.Errorf("%v gets local binding", tools)
		}
	}
	if domains, _ := network["allowedDomains"].([]any); len(domains) != 0 || network["strictAllowlist"] != true {
		t.Errorf("outbound network is not blocked: %v", network)
	}
	if allow, _ := fs["allowWrite"].([]any); len(allow) != 1 {
		t.Errorf("allowWrite = %v", fs["allowWrite"])
	}
	denied := inv.DeniedPaths(userTools)
	for _, p := range []string{"/home/u/.gradle/caches", "/home/u/.gradle/daemon", "/home/u/.gradle/gradle.properties", "/home/u/.gradle/init.d"} {
		if !slices.Contains(denied, p) {
			t.Errorf("%s is not denied", p)
		}
	}
	for _, p := range []string{"/home/u/.gradle", "/home/u/.gradle/wrapper"} {
		if slices.Contains(denied, p) {
			t.Errorf("%s is denied: wrapper distributions must stay readable", p)
		}
	}
}

func TestCargoRunEnvironmentAndSandbox(t *testing.T) {
	inv := toolInvocation(t, "cargo")
	env, settings := toolCommand(t, inv, userTools)
	for name, v := range map[string]string{"CARGO_HOME": "/data/deps/1/cargo", "CARGO_NET_OFFLINE": "true", "RUSTC_WRAPPER": "",
		"CARGO_TARGET_DIR": "/work/runs/r1/repo/target"} { // the checkout's own, whatever the user's config says
		if got, ok := env[name]; !ok || got != v {
			t.Errorf("%s = %q (set %v), want %q: sccache would cache compiled hidden tests", name, got, ok, v)
		}
	}
	if network, _ := sandbox(settings); network["allowLocalBinding"] != nil {
		t.Errorf("Cargo projects get no local binding: %v", network)
	}
	denied := inv.DeniedPaths(userTools)
	for _, p := range []string{"/home/u/.cargo/registry", "/home/u/.cargo/git", "/var/sccache", "/var/target"} {
		if !slices.Contains(denied, p) {
			t.Errorf("%s is not denied", p)
		}
	}
	if slices.Contains(denied, "/home/u/.cargo") || slices.Contains(denied, "/home/u/.cargo/bin") {
		t.Error("~/.cargo/bin must stay readable: cargo and rustc run through it")
	}
}

// A repository without a JVM or Cargo marker is a Go (or other) project as before: the user's wrapper variable passes
// as it did, no local binding, and the user's caches of every tool are still denied.
func TestUnmarkedProjectKeepsItsEnvironment(t *testing.T) {
	inv := invocation(t, SignInLogin, "")
	inv.BuildCache = "/work/runs/r1/go-build"
	env, settings := toolCommand(t, inv, userTools)
	if env["RUSTC_WRAPPER"] != "sccache" || env["JAVA_HOME"] != "/user/jdk" || env["CARGO_HOME"] != "/home/u/.cargo" {
		t.Errorf("legacy variables changed: %v", env)
	}
	for _, name := range []string{"MAVEN_ARGS", "GRADLE_USER_HOME", "GRADLE_RO_DEP_CACHE", "CARGO_NET_OFFLINE", "MAVEN_USER_HOME"} {
		if v, ok := env[name]; ok {
			t.Errorf("%s = %q without its profile", name, v)
		}
	}
	if network, _ := sandbox(settings); network["allowLocalBinding"] != nil {
		t.Error("local binding without Gradle")
	}
	denied := inv.DeniedPaths(userTools)
	for _, p := range []string{"/home/u/.m2", "/home/u/.gradle/caches", "/home/u/.cargo/registry", "/var/sccache"} {
		if !slices.Contains(denied, p) {
			t.Errorf("%s is not denied in a project without that tool", p)
		}
	}
}

func TestToolPathsAreChecked(t *testing.T) {
	for _, mutate := range []func(*Invocation){func(i *Invocation) { i.Deps = "deps" }, func(i *Invocation) { i.JavaHome = "jdk" }} {
		inv := toolInvocation(t, "maven")
		mutate(&inv)
		if _, _, err := inv.Command(userTools); err == nil {
			t.Error("a relative deps folder or JDK is accepted")
		}
	}
}

// A real Go run, with a deps folder: Go's environment is as before, and the deps folder is stated read-only and left
// readable, with no local binding. This pins the one change a Go project's sandbox gets from profiles.
func TestGoRunWithADepsFolder(t *testing.T) {
	inv := toolInvocation(t, "go")
	inv.AllowLocalBinding = false
	env, settings := toolCommand(t, inv, userTools)
	network, fs := sandbox(settings)
	if network["allowLocalBinding"] != nil {
		t.Error("local binding for a Go project")
	}
	writes, _ := fs["denyWrite"].([]any)
	if !slices.Contains(writes, any("/data/deps/1")) {
		t.Errorf("denyWrite lacks the deps folder: %v", writes)
	}
	if env["GOFLAGS"] != "-buildvcs=false" || env["GOCACHE"] != "/work/runs/r1/go-build" || env["RUSTC_WRAPPER"] != "sccache" {
		t.Errorf("a Go project's environment changed: %v", env)
	}
	for _, name := range []string{"MAVEN_ARGS", "GRADLE_USER_HOME", "CARGO_NET_OFFLINE", "CARGO_TARGET_DIR"} {
		if _, ok := env[name]; ok {
			t.Errorf("%s reaches a Go project's agent", name)
		}
	}
}
