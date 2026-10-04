package buildtool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func hasFiles(files ...string) func(string) bool {
	return func(name string) bool { return slices.Contains(files, name) }
}

// Each tool is detected by its own files at the root, proposes its wrapper's command when the repository has one, and
// is selected for a run only when detected (Go's profile is always on, as before profiles).
func TestDetectionAndTestCommands(t *testing.T) {
	for _, c := range []struct {
		name  string
		files []string
		want  []string
		tools []string
	}{
		{"maven wrapper", []string{"pom.xml", "mvnw", ".mvn/wrapper/maven-wrapper.properties"}, []string{"./mvnw -q test"}, []string{"maven"}},
		{"maven without a wrapper", []string{"pom.xml"}, []string{"mvn -q test"}, []string{"maven"}},
		{"gradle wrapper, kotlin scripts", []string{"build.gradle.kts", "settings.gradle.kts", "gradlew"}, []string{"./gradlew test"}, []string{"gradle"}},
		{"gradle without a wrapper", []string{"build.gradle"}, []string{"gradle test"}, []string{"gradle"}},
		{"gradle settings alone", []string{"settings.gradle"}, []string{"gradle test"}, []string{"gradle"}},
		{"cargo", []string{"Cargo.toml", "Cargo.lock"}, []string{"cargo test"}, []string{"cargo"}},
		{"a wrapper alone proves nothing", []string{"mvnw", "gradlew"}, nil, nil},
		{"two tools, in table order", []string{"pom.xml", "Cargo.toml", "go.mod"}, []string{"go test ./...", "mvn -q test", "cargo test"}, []string{"go", "maven", "cargo"}},
	} {
		if got := TestCommands(hasFiles(c.files...)); !slices.Equal(got, c.want) {
			t.Errorf("%s: test commands %q, want %q", c.name, got, c.want)
		}
		if got := DetectedNames(hasFiles(c.files...)); !slices.Equal(got, c.tools) {
			t.Errorf("%s: detected %q, want %q", c.name, got, c.tools)
		}
	}
	names := func(ps []Profile) (out []string) {
		for _, p := range ps {
			out = append(out, p.Name)
		}
		return out
	}
	if got := names(Select(nil)); !slices.Equal(got, []string{"go"}) {
		t.Errorf("a repository with no marker still gets Go's profile: %q", got)
	}
	if got := names(Select([]string{"cargo", "gradle"})); !slices.Equal(got, []string{"go", "gradle", "cargo"}) {
		t.Errorf("selection: %q", got)
	}
}

// Grading's check-file map knows each tool's configuration, by the words of the runners (wrappers too).
func TestRunnerConfigsPerTool(t *testing.T) {
	configs := RunnerConfigs()
	for word, want := range map[string][]string{
		"mvn": {"pom.xml", ".mvn/maven.config"}, "mvnw": {"pom.xml", ".mvn/wrapper/maven-wrapper.properties"},
		"gradle":  {"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "gradle.properties"},
		"gradlew": {"build.gradle.kts", "gradle/wrapper/gradle-wrapper.properties"},
		"cargo":   {"Cargo.toml", "Cargo.lock", "rust-toolchain.toml", ".cargo/config.toml"},
	} {
		for _, f := range want {
			if !slices.Contains(configs[word], f) {
				t.Errorf("%s's configuration lacks %s: %q", word, f, configs[word])
			}
		}
	}
}

// The test-runner pattern, joined as run does, covers the wrappers, `mvn verify` and `cargo nextest`, and not commands
// that only build or print.
func TestTestPatterns(t *testing.T) {
	re := regexp.MustCompile(`\b(` + strings.Join(TestPatterns(), "|") + `)\b`)
	for _, c := range []string{"mvn test", "mvn -q test", "./mvnw -q test", "mvnw test", "mvn verify", "./mvnw -B clean verify", "mvn -pl core -am test",
		"mvn surefire:test", "./gradlew test", "gradle test", "./gradlew :app:test --tests Foo", "gradle --offline test", "./gradlew check",
		"./gradlew build", "cargo test", "cargo test --offline --test test_buf", "cargo nextest run", "go test ./...", "cd x && ./mvnw -q test"} {
		if !re.MatchString(c) {
			t.Errorf("%q is not seen as running tests", c)
		}
	}
	for _, c := range []string{"mvn dependency:tree", "./mvnw -q compile", "mvn package -DskipTests", "gradle tasks", "./gradlew assemble", "cargo build",
		"cargo fetch", "cargo fmt", "cat mvn-test.txt", "echo contest"} {
		if re.MatchString(c) {
			t.Errorf("%q is seen as running tests", c)
		}
	}
}

func env(t *testing.T, kvs []string) map[string]string {
	t.Helper()
	return vars(kvs)
}

// withoutGo drops what Go's always-on profile sets.
func withoutGo(kvs []string) []string {
	return slices.DeleteFunc(slices.Clone(kvs), func(kv string) bool { return strings.HasPrefix(kv, "GOFLAGS=") })
}

// Each profile's agent environment: what it sets, clears and leaves out. Without a deps folder or a build cache the
// tool's offline settings are left out, not set to empty folders.
func TestAgentEnvPerTool(t *testing.T) {
	ctx := AgentContext{Allowed: []string{"PATH=/usr/bin", "JAVA_HOME=/user/jdk", "RUSTC_WRAPPER=sccache"}, Environ: []string{"RUSTC_WRAPPER=sccache"},
		Home: "/home/u", Repo: "/data/workspaces/r1/repo", BuildCache: "/data/workspaces/r1/go-build", Deps: "/data/deps/1", JavaHome: "/host/jdk"}
	maven := env(t, AgentEnv(Select([]string{"maven"}), ctx))
	for name, want := range map[string]string{"JAVA_HOME": "/host/jdk", "MAVEN_USER_HOME": "/data/deps/1/mvnw-home",
		"MAVEN_ARGS": "-o -Dmaven.repo.local=/data/workspaces/r1/go-build/m2 -Dmaven.repo.local.tail=/data/deps/1/m2 -Dmaven.build.cache.enabled=false"} {
		if maven[name] != want {
			t.Errorf("maven: %s = %q, want %q", name, maven[name], want)
		}
	}
	gradle := env(t, AgentEnv(Select([]string{"gradle"}), ctx))
	for name, want := range map[string]string{"JAVA_HOME": "/host/jdk", "GRADLE_USER_HOME": "/data/workspaces/r1/go-build/gradle",
		"GRADLE_RO_DEP_CACHE": "/data/deps/1/gradle-ro"} {
		if gradle[name] != want {
			t.Errorf("gradle: %s = %q, want %q", name, gradle[name], want)
		}
	}
	cargo := env(t, AgentEnv(Select([]string{"cargo"}), ctx))
	for name, want := range map[string]string{"CARGO_HOME": "/data/deps/1/cargo", "CARGO_NET_OFFLINE": "true", "RUSTC_WRAPPER": "", "RUSTC_WORKSPACE_WRAPPER": "",
		"CARGO_TARGET_DIR": "/data/workspaces/r1/repo/target", "CARGO_BUILD_BUILD_DIR": "/data/workspaces/r1/repo/target"} {
		if v, ok := cargo[name]; !ok || v != want {
			t.Errorf("cargo: %s = %q (set %v), want %q", name, v, ok, want)
		}
	}
	if _, ok := cargo["JAVA_HOME"]; ok {
		t.Error("cargo sets JAVA_HOME")
	}
	// Nothing resolved, nothing prepared: the profiles set nothing they cannot back.
	bare := AgentContext{Allowed: ctx.Allowed, Environ: ctx.Environ, Home: "/home/u"}
	if got := withoutGo(AgentEnv(Select([]string{"maven", "gradle"}), bare)); len(got) != 0 {
		t.Errorf("maven and gradle without a build cache, deps or JDK set %q", got)
	}
	if got := withoutGo(AgentEnv(Select([]string{"cargo"}), bare)); !slices.Equal(got, []string{"RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER="}) {
		t.Errorf("cargo without deps: %q", got)
	}
}

// Agentium's own commands: each tool's caches go into the data folder, and every rustc wrapper is cleared so no
// compiled hidden test is cached outside it. Go's environment (CommandEnv) does not change with the selection.
func TestCommandEnvPerTool(t *testing.T) {
	base := CommandEnv("/data/cache", "/data/cache/tmp")
	if !slices.Contains(base, "GOCACHE=/data/cache/go-build") || slices.ContainsFunc(base, func(kv string) bool { return strings.HasPrefix(kv, "GRADLE") || strings.HasPrefix(kv, "RUSTC") }) {
		t.Errorf("the global command environment: %q", base)
	}
	got := env(t, CommandEnvFor(Select([]string{"maven", "gradle", "cargo"}), "/data/cache"))
	for name, want := range map[string]string{"MAVEN_USER_HOME": "/data/cache/maven", "MAVEN_ARGS": "-Dmaven.repo.local=/data/cache/m2 -Dmaven.build.cache.enabled=false",
		"GRADLE_USER_HOME": "/data/cache/gradle", "RUSTC_WRAPPER": "", "RUSTC_WORKSPACE_WRAPPER": "", "CARGO_BUILD_RUSTC_WRAPPER": "",
		"CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER": "",
		"CARGO_TARGET_DIR":                    "target"} {
		if v, ok := got[name]; !ok || v != want {
			t.Errorf("%s = %q (set %v), want %q", name, v, ok, want)
		}
	}
	if len(CommandEnvFor(Select(nil), "/data/cache")) != 0 {
		t.Error("Go's profile adds to the global environment twice")
	}
}

// The user's caches are denied whatever the repository, under the home folder or where the user's environment puts them.
func TestUserCachesPerTool(t *testing.T) {
	got := UserCaches([]string{"CARGO_HOME=/opt/cargo", "SCCACHE_DIR=/var/sccache", "CARGO_TARGET_DIR=/var/target", "GRADLE_USER_HOME=/opt/gradle",
		"MAVEN_USER_HOME=/opt/m2", "XDG_CACHE_HOME=/home/u/xdg"}, "/home/u")
	for _, want := range []string{"/home/u/.m2", "/opt/m2", "/home/u/.gradle/caches", "/home/u/.gradle/gradle.properties", "/opt/gradle/caches",
		"/opt/gradle/daemon", "/home/u/.cargo/registry", "/opt/cargo/registry", "/opt/cargo/credentials.toml", "/var/sccache", "/var/target",
		"/home/u/xdg/sccache", "/home/u/Library/Caches/Mozilla.sccache", "/home/u/.cache/sccache", "/home/u/Library/Caches/go-build"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not denied", want)
		}
	}
	// The recipes need these readable: wrapper distributions, and the toolchain's proxies.
	for _, readable := range []string{"/home/u/.gradle", "/home/u/.gradle/wrapper", "/opt/gradle/wrapper", "/home/u/.cargo", "/home/u/.cargo/bin"} {
		if slices.Contains(got, readable) {
			t.Errorf("%s must stay readable", readable)
		}
	}
	for _, p := range UserCaches([]string{"CARGO_HOME=rel", "SCCACHE_DIR=rel", "GRADLE_USER_HOME=rel"}, "/home/u") {
		if !filepath.IsAbs(p) {
			t.Errorf("%q is not absolute", p)
		}
	}
}

func TestLocalBindingOnlyForGradle(t *testing.T) {
	for name, want := range map[string]bool{"maven": false, "cargo": false, "gradle": true} {
		if got := LocalBinding(Select([]string{name})); got != want {
			t.Errorf("%s: local binding %v", name, got)
		}
	}
	if LocalBinding(Select(nil)) {
		t.Error("Go needs no local sockets")
	}
}

// repoWith makes a checkout with the named files (and the test class, when given, under src/test/java).
func repoWith(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The warm-up commands name the wrapper when there is one, put the fetched files in the deps folder, and Maven's runs
// one real test class of the base when there is one.
func TestWarmSteps(t *testing.T) {
	steps := WarmSteps(Select([]string{"maven", "gradle", "cargo"}), repoWith(t, "mvnw", "gradlew", "src/test/java/org/x/FooTest.java"), "/data/deps/1")
	var commands []string
	for _, s := range steps {
		commands = append(commands, s.Command)
		if len(s.Env) == 0 || !strings.Contains(strings.Join(s.Env, " "), "/data/deps/1/") {
			t.Errorf("%q does not write into the deps folder: %q", s.Command, s.Env)
		}
	}
	joined := strings.Join(commands, "\n")
	for _, want := range []string{"./mvnw -B -q test -Dtest=FooTest ", "./gradlew --no-daemon", "cargo fetch"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warm-up %q in %q", want, commands)
		}
	}
	plain := WarmSteps(Select([]string{"maven", "gradle"}), repoWith(t), "/d")
	if !strings.Contains(plain[0].Command, "mvn -B") || strings.Contains(plain[0].Command, "./mvnw") || strings.Contains(plain[1].Command, "./gradlew") ||
		!strings.Contains(plain[0].Command, "AgentiumWarmNoSuchTest") {
		t.Errorf("without wrappers or tests: %q", plain)
	}
	// Gradle: compile classpaths, the test runtime, then every other resolvable configuration, each its own step (a failure
	// of one must not skip the next), all without the daemon and the build cache.
	var gradle []string
	for _, s := range WarmSteps(Select([]string{"gradle"}), repoWith(t, "gradlew"), "/d") {
		if !strings.Contains(s.Command, "--no-daemon --no-build-cache") {
			t.Errorf("a Gradle warm-up that may use the daemon or the build cache: %q", s.Command)
		}
		gradle = append(gradle, s.Command)
	}
	if len(gradle) != 3 || !strings.HasSuffix(gradle[0], "testClasses") || !strings.Contains(gradle[1], "AgentiumWarmNoSuchTest") ||
		!strings.HasSuffix(gradle[2], "-q -I '/d/gradle/agentium-resolve-all.gradle' agentiumResolveAll || true") || strings.Contains(gradle[0]+gradle[1], "-I") {
		t.Errorf("Gradle warm-up steps: %q", gradle)
	}
	if got := NeedsWarming(Select([]string{"cargo", "maven"})); !slices.Equal(got, []string{"maven", "cargo"}) {
		t.Errorf("warmed tools: %q", got)
	}
}

// The deps folder's Gradle home never cleans its cache up: agents read it while later warm-ups run.
func TestGradleDepsHomeKeepsItsFiles(t *testing.T) {
	deps := t.TempDir()
	if err := PrepareDeps(Select([]string{"gradle", "maven"}), deps); err != nil {
		t.Fatal(err)
	}
	props, _ := os.ReadFile(filepath.Join(deps, "gradle", "gradle.properties"))
	script, _ := os.ReadFile(filepath.Join(deps, "gradle", "init.d", "agentium-no-cleanup.gradle"))
	if !strings.Contains(string(props), "org.gradle.cache.cleanup=false") || !strings.Contains(string(script), "Cleanup.DISABLED") || !strings.Contains(string(script), "respondsTo(settings, \"getCaches\")") {
		t.Errorf("cleanup is not disabled: %q %q", props, script)
	}
}

// The warm-up's task exists through an init script in the deps folder's Gradle home only: it resolves every resolvable
// configuration of every project, tolerating one that cannot be resolved, and the agent's home has no such script.
func TestGradleDepsHomeResolvesEveryConfiguration(t *testing.T) {
	deps, cache := t.TempDir(), t.TempDir()
	if err := PrepareDeps(Select([]string{"gradle"}), deps); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(deps, "gradle", resolveAllScriptName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"allprojects", `register("agentiumResolveAll")`, "canBeResolved", "conf.resolve()", "catch (Exception e)", "notCompatibleWithConfigurationCache", "def confs = project.configurations", WarmSkippedFile} {
		if !strings.Contains(string(script), want) {
			t.Errorf("init script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(string(script), "project.configurations.matching") || strings.Contains(string(script), "doLast {\n            project.") {
		t.Error("the script uses Task.project at execution time")
	}
	// Not in init.d (every step would load it), and not in the run's home.
	if _, err := os.Stat(filepath.Join(deps, "gradle", "init.d", "agentium-resolve-all.gradle")); err == nil {
		t.Error("the script loads in every warm-up step")
	}
	if err := PrepareRun(context.Background(), Select([]string{"gradle"}), "", cache); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "gradle", "init.d", "agentium-resolve-all.gradle")); err == nil {
		t.Error("the run's Gradle home resolves configurations")
	}
}

// A changed warm-up recipe changes its version (the stamps' name), and an unchanged one does not.
func TestWarmVersion(t *testing.T) {
	gradle := Select([]string{"gradle"})
	if WarmVersion(gradle) != WarmVersion(Select([]string{"gradle"})) || WarmVersion(gradle) == WarmVersion(Select([]string{"cargo"})) {
		t.Error("versions are not stable per tool set")
	}
	changed := slices.Clone(gradle)
	changed[0].WarmRecipe += "x"
	if WarmVersion(changed) == WarmVersion(gradle) {
		t.Error("a changed recipe keeps its version")
	}
}

// What a warm-up skipped is read from the checkout; network-looking reasons are flagged.
func TestReadWarmSkipped(t *testing.T) {
	dir := t.TempDir()
	if names, transient := ReadWarmSkipped(dir); names != nil || transient {
		t.Errorf("no file: %v %v", names, transient)
	}
	os.WriteFile(filepath.Join(dir, WarmSkippedFile), []byte(":a\tNo attributes\n\n:b:c\tread timed out\n"), 0o600)
	if names, transient := ReadWarmSkipped(dir); !slices.Equal(names, []string{":a", ":b:c"}) || !transient {
		t.Errorf("got %v %v", names, transient)
	}
}

// The run's Gradle home gets the distribution as an independent copy and no daemon.
func TestPrepareGradleRun(t *testing.T) {
	deps, cache := t.TempDir(), t.TempDir()
	dist := filepath.Join(deps, "gradle", "wrapper", "dists", "gradle-9.7.1-bin", "abc", "gradle-9.7.1", "bin")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "gradle"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRun(context.Background(), Select([]string{"gradle"}), deps, cache); err != nil {
		t.Fatal(err)
	}
	props, _ := os.ReadFile(filepath.Join(cache, "gradle", "gradle.properties"))
	if want := gradleHomeProps + "org.gradle.java.installations.auto-download=false\n"; string(props) != want { // no JDKs provisioned
		t.Errorf("gradle.properties = %q, want %q", props, want)
	}
	if init, _ := os.ReadFile(filepath.Join(cache, "gradle", "init.d", "agentium-offline.gradle")); !strings.Contains(string(init), "startParameter.offline = true") {
		t.Errorf("no offline init script: %q", init)
	}
	copied := filepath.Join(cache, "gradle", "wrapper", "dists", "gradle-9.7.1-bin", "abc", "gradle-9.7.1", "bin", "gradle")
	info, err := os.Stat(copied)
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the distribution was not cloned with its modes: %v %v", info, err)
	}
	if err := os.WriteFile(copied, []byte("changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if orig, _ := os.ReadFile(filepath.Join(dist, "gradle")); string(orig) != "#!/bin/sh\n" {
		t.Error("writing in the run's copy changed the deps folder")
	}
	// No wrapper fetched, no deps folder: only the properties.
	bare := t.TempDir()
	if err := PrepareRun(context.Background(), Select([]string{"gradle"}), "", bare); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bare, "gradle", "wrapper")); err == nil {
		t.Error("a wrapper appeared from nowhere")
	}
	// Maven and Cargo prepare nothing.
	if err := PrepareRun(context.Background(), Select([]string{"maven", "cargo"}), deps, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// stopHost is a fake machine: the user's Gradle daemons with the files they hold open, and signals that end them.
type stopHost struct {
	daemons []int // the user's GradleDaemon processes
	open    map[int][]string
	links   map[string]int // link counts by file name (0: unknown)
	alive   map[int]bool
	ignores map[int]bool // processes that survive SIGTERM
	signals []string
}

func (h *stopHost) host() Host {
	return Host{
		Daemons: func(context.Context) ([]int, error) { return h.daemons, nil },
		OpenFiles: func(_ context.Context, pid int) ([]OpenFile, error) {
			if h.open == nil {
				return nil, errors.New("no lsof")
			}
			var files []OpenFile
			for _, name := range h.open[pid] {
				files = append(files, OpenFile{Name: name, Links: h.links[name]})
			}
			return files, nil
		},
		Signal: func(pid int, sig syscall.Signal) error {
			if !h.alive[pid] {
				return syscall.ESRCH
			}
			if sig != 0 {
				h.signals = append(h.signals, strconv.Itoa(pid)+":"+sig.String())
			}
			if sig == syscall.SIGKILL || (sig == syscall.SIGTERM && !h.ignores[pid]) {
				h.alive[pid] = false
			}
			return nil
		},
		Grace: 150 * time.Millisecond,
	}
}

func mustReal(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// gradleHome makes <cache>/gradle/daemon/9.7.1 and returns the build cache and a function giving a daemon's real log path.
func gradleHome(t *testing.T) (cache string, log func(pid int) string) {
	t.Helper()
	cache = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cache, "gradle", "daemon", "9.7.1"), 0o700); err != nil {
		t.Fatal(err)
	}
	real := mustReal(t, cache) // lsof reports real paths (/private/var on macOS)
	return cache, func(pid int) string {
		return filepath.Join(real, "gradle", "daemon", "9.7.1", "daemon-"+strconv.Itoa(pid)+".out.log")
	}
}

// The run's daemons are found from the machine's process list, by the log each holds open in the run's own Gradle home;
// nothing the agent writes names a process. The user's own daemon, another run's, and a daemon whose "log" the agent
// pointed elsewhere are left alone. Nothing from the checkout runs. A fake machine stands for the host.
func TestStopGradleDaemons(t *testing.T) {
	cache, log := gradleHome(t)
	_, otherLog := gradleHome(t) // another run's Gradle home
	h := &stopHost{
		daemons: []int{4242, 5000, 6000, 7000},
		open: map[int][]string{
			4242: {log(4242), "/jdk/lib/modules"},                      // the run's own daemon: has its log open
			5000: {"/home/u/.gradle/daemon/9.7.1/daemon-5000.out.log"}, // the user's own daemon
			6000: {otherLog(6000)},                                     // another run's daemon
			7000: {log(7001)},                                          // holds a log of another pid's name: not its own
		},
		alive: map[int]bool{4242: true, 5000: true, 6000: true, 7000: true},
	}
	// A planted log naming the user's daemon is never a reason to signal it: at most a note.
	if err := os.WriteFile(filepath.Join(cache, "gradle", "daemon", "9.7.1", "daemon-5000.out.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), mustReal(t, cache), h.host()); err == nil || !strings.Contains(err.Error(), "[5000]") {
		t.Fatalf("the planted log should only be noted: %v", err)
	}
	if h.alive[4242] || !h.alive[5000] || !h.alive[6000] || !h.alive[7000] {
		t.Errorf("alive: %v (only the run's own daemon must stop)", h.alive)
	}
	if !slices.Equal(h.signals, []string{"4242:terminated"}) {
		t.Errorf("signals %v", h.signals)
	}
	// A daemon that ignores SIGTERM is killed after the grace period.
	h = &stopHost{daemons: []int{4242}, open: map[int][]string{4242: {log(4242)}}, alive: map[int]bool{4242: true}, ignores: map[int]bool{4242: true}}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), mustReal(t, cache), h.host()); err != nil {
		t.Fatal(err)
	}
	if h.alive[4242] || !slices.Equal(h.signals, []string{"4242:terminated", "4242:killed"}) {
		t.Errorf("a stubborn daemon: alive %v, signals %v", h.alive, h.signals)
	}
	// A machine that cannot tell what a process has open (no lsof) is not trusted: nothing is signalled, and it is said.
	h = &stopHost{daemons: []int{4242}, alive: map[int]bool{4242: true}}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), mustReal(t, cache), h.host()); err == nil || len(h.signals) != 0 {
		t.Errorf("without lsof: error %v, signals %v", err, h.signals)
	}
	// No daemons, no error.
	if err := StopRun(context.Background(), Select([]string{"gradle"}), t.TempDir(), (&stopHost{}).host()); err != nil {
		t.Errorf("a run without daemons: %v", err)
	}
}

// The sandbox lets the agent write the build cache path itself, so mid-run it can move the folder away and put a link to
// another run's folder (or the user's Gradle home) in its place. StopRun gets the path resolved BEFORE the agent started
// and touches no file-system state, so a daemon holding a log in the linked-to folder is not signalled, and one holding
// a log under the real path still is.
func TestStopGradleDaemonsIgnoresAReplacedBuildCache(t *testing.T) {
	cache, log := gradleHome(t)
	resolved := mustReal(t, cache) // what Once resolves right after making the folder
	otherCache, otherLog := gradleHome(t)
	// The agent: mv <ws>/go-build into the checkout, then ln -s <another run's> <ws>/go-build.
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(cache, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherCache, cache); err != nil {
		t.Fatal(err)
	}
	h := &stopHost{daemons: []int{4242, 5000}, open: map[int][]string{4242: {otherLog(4242)}, 5000: {log(5000)}},
		alive: map[int]bool{4242: true, 5000: true}}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), resolved, h.host()); err != nil {
		t.Fatal(err)
	}
	if !h.alive[4242] {
		t.Error("a daemon of the folder the agent linked to was signalled")
	}
	if h.alive[5000] || !slices.Equal(h.signals, []string{"5000:terminated"}) {
		t.Errorf("the run's own daemon (its log under the pre-resolved path): alive %v, signals %v", h.alive, h.signals)
	}
	// The user's own Gradle home, even named like the run's (".../gradle"), is just another path.
	h = &stopHost{daemons: []int{6000}, open: map[int][]string{6000: {"/home/u/.gradle/daemon/9.7.1/daemon-6000.out.log"}}, alive: map[int]bool{6000: true}}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), resolved, h.host()); err != nil || !h.alive[6000] {
		t.Errorf("the user's daemon: %v, alive %v", err, h.alive)
	}
}

// A pid reused between the first check and SIGKILL is not killed: the match is repeated before it.
func TestStopGradleDaemonsRechecksBeforeKilling(t *testing.T) {
	cache, log := gradleHome(t)
	h := &stopHost{daemons: []int{4242}, open: map[int][]string{4242: {log(4242)}}, alive: map[int]bool{4242: true}, ignores: map[int]bool{4242: true}}
	host := h.host()
	calls := 0
	host.OpenFiles = func(_ context.Context, pid int) ([]OpenFile, error) {
		calls++
		if calls == 1 {
			return []OpenFile{{Name: log(pid), Links: 1}}, nil
		}
		return []OpenFile{{Name: "/usr/lib/other", Links: 1}}, nil // the pid is somebody else's now
	}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), mustReal(t, cache), host); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.signals, []string{"4242:terminated"}) || calls != 2 {
		t.Errorf("signals %v after %d checks: SIGKILL must wait for a second match", h.signals, calls)
	}
}

// ps and lsof come from fixed absolute paths, never from PATH.
func TestToolsAreAbsolute(t *testing.T) {
	if got := tool("lsof", "/nonexistent/a", "/nonexistent/b"); got != "/nonexistent/lsof" {
		t.Errorf("a missing tool: %s", got)
	}
	if got := tool("sh", "/nonexistent/a", "/bin/sh"); got != "/bin/sh" {
		t.Errorf("an existing tool: %s", got)
	}
	if ps := tool("ps", "/bin/ps", "/usr/bin/ps"); !filepath.IsAbs(ps) {
		t.Errorf("ps: %s", ps)
	}
}

// A cancelled context still ends a daemon that ignores SIGTERM, at once.
func TestStopGradleDaemonsWhenCancelled(t *testing.T) {
	cache, log := gradleHome(t)
	h := &stopHost{daemons: []int{100}, open: map[int][]string{100: {log(100)}}, alive: map[int]bool{100: true}, ignores: map[int]bool{100: true}}
	host := h.host()
	host.Grace = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := StopRun(ctx, Select([]string{"gradle"}), mustReal(t, cache), host); err != nil || h.alive[100] || time.Since(start) > 5*time.Second {
		t.Errorf("a cancelled stop: %v, alive %v, after %v", err, h.alive, time.Since(start))
	}
}

// Agentium's own Gradle commands never start a daemon.
func TestGradleCommandsGetNoDaemon(t *testing.T) {
	cache := t.TempDir()
	if err := PrepareCommands(cache); err != nil {
		t.Fatal(err)
	}
	if props, _ := os.ReadFile(filepath.Join(cache, "gradle", "gradle.properties")); string(props) != gradleHomeProps {
		t.Errorf("gradle.properties = %q", props)
	}
}

func TestResolveJavaHome(t *testing.T) {
	jdk := func(root string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "bin", "java"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return root
	}
	dir := t.TempDir()
	user, found, onPath := jdk(filepath.Join(dir, "user")), jdk(filepath.Join(dir, "found")), jdk(filepath.Join(dir, "path"))
	run := func(context.Context, string, ...string) (string, error) { return found + "\n", nil }
	if got := ResolveJavaHome(context.Background(), []string{"JAVA_HOME=" + user}, run); got != user {
		t.Errorf("the user's JAVA_HOME: %s", got)
	}
	if got := ResolveJavaHome(context.Background(), []string{"JAVA_HOME=" + filepath.Join(dir, "gone"), "PATH=/nonexistent"}, run); got == filepath.Join(dir, "gone") {
		t.Errorf("a JAVA_HOME without a JDK is used: %s", got)
	}
	// On PATH, through a link, as Homebrew and SDKMAN lay JDKs out.
	link := filepath.Join(dir, "links")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(onPath, "bin", "java"), filepath.Join(link, "java")); err != nil {
		t.Fatal(err)
	}
	resolvedPath, _ := filepath.EvalSymlinks(onPath)
	if got := ResolveJavaHome(context.Background(), []string{"PATH=" + link}, func(context.Context, string, ...string) (string, error) { return "", os.ErrNotExist }); got != resolvedPath {
		t.Errorf("java on PATH: %s, want %s", got, resolvedPath)
	}
	if got := ResolveJavaHome(context.Background(), []string{"PATH=/nonexistent"}, func(context.Context, string, ...string) (string, error) { return "", os.ErrNotExist }); got != "" {
		t.Errorf("no JDK anywhere: %q", got)
	}
}

// A target directory set in a Cargo config or the environment cannot take grading's compiled hidden tests to a folder
// agents read: the user's are denied however the setting is written, in the user's Cargo home, the folders above the
// home, and the user's repository and the folders above it. Parsing only ever denies more, never less.
func TestCargoTargetDirectories(t *testing.T) {
	home := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []struct{ config, want string }{
		{"[net]\ngit-fetch-with-cli = true\n\n[build]\njobs = 4\ntarget-dir = \"/shared/target\" # fast disk\n", "/shared/target"},
		{"[build] # where output goes\ntarget-dir = '/lit/target'\n", "/lit/target"},
		{"build.target-dir = \"/dotted/target\"\n", "/dotted/target"},
		{"[build]\n\"target-dir\" = \"/quoted/key\"\n", "/quoted/key"},
		{"[ build ]\ntarget-dir=\"/tight/target\"\n", "/tight/target"},
		{"build = { jobs = 2, target-dir = \"/inline/target\" }\n", "/inline/target"},
		{"[build]\ntarget-dir = 'out/target'\n", "out/target"},
		{"[env]\ntarget-dir = \"/other/table\"\n", "/other/table"}, // over-denied on purpose
	} {
		write(filepath.Join(home, ".cargo", "config.toml"), c.config)
		want := c.want
		if !filepath.IsAbs(want) {
			want = filepath.Join(home, want)
		}
		if got := UserCaches(nil, home); !slices.Contains(got, want) {
			t.Errorf("case %d: %s is not denied: %q", i, want, got)
		}
	}
	// build-dir (cargo 1.95 puts test executables there), in any of the forms, and the cache home's default build/.
	for i, c := range []struct{ config, want string }{
		{"[build]\nbuild-dir = \"/builds/out\"\n", "/builds/out"},
		{"build.build-dir = '/dotted/builds' # x\n", "/dotted/builds"},
		{"[build]\nbuild-dir = \"{cargo-cache-home}/build/{workspace-path-hash}\"\n", filepath.Join(home, ".cargo", "build")},
	} {
		write(filepath.Join(home, ".cargo", "config.toml"), c.config)
		if got := UserCaches(nil, home); !slices.Contains(got, c.want) {
			t.Errorf("build-dir case %d: %s is not denied: %q", i, c.want, got)
		}
	}
	if got := UserCaches([]string{"CARGO_BUILD_BUILD_DIR=/env/builds"}, home); !slices.Contains(got, "/env/builds") || !slices.Contains(got, filepath.Join(home, ".cargo", "build")) {
		t.Errorf("the environment's build dir and the cargo cache home's build/: %q", got)
	}
	// Garbage is not a reason to deny less.
	write(filepath.Join(home, ".cargo", "config.toml"), "[build\ntarget-dir = \"/after/garbage\"\n\x00\x01")
	if got := UserCaches(nil, home); !slices.Contains(got, "/after/garbage") {
		t.Errorf("a broken config: %q", got)
	}
	os.Remove(filepath.Join(home, ".cargo", "config.toml"))
	// CARGO_HOME elsewhere, the older file name, and the folders above the home.
	cargoHome := filepath.Join(t.TempDir(), "ch")
	write(filepath.Join(cargoHome, "config"), "[build]\ntarget-dir = \"/oldname/target\"\n")
	write(filepath.Join(filepath.Dir(home), ".cargo", "config.toml"), "[build]\ntarget-dir = \"/above/target\"\n")
	got := UserCaches([]string{"CARGO_HOME=" + cargoHome, "CARGO_BUILD_TARGET_DIR=/env/target"}, home)
	for _, want := range []string{"/oldname/target", "/env/target", "/above/target"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not denied: %q", want, got)
		}
	}
	// The user's repository: its own .cargo/config.toml and those above it; relative paths start at the config's folder.
	repo := filepath.Join(home, "work", "app")
	write(filepath.Join(repo, ".cargo", "config.toml"), "[build]\ntarget-dir = \"build-out\"\n")
	write(filepath.Join(home, "work", ".cargo", "config.toml"), "[build]\ntarget-dir = \"/work/shared\"\n")
	projectGot, _ := ProjectCaches(nil, home, []string{repo})
	for _, want := range []string{filepath.Join(repo, "build-out"), "/work/shared"} {
		if !slices.Contains(projectGot, want) {
			t.Errorf("the repository's config: %s is not denied: %q", want, projectGot)
		}
	}
	if got, _ := ProjectCaches(nil, home, []string{"", "relative"}); slices.Contains(got, "relative") {
		t.Error("a repository path that is not absolute reads configs from the working folder")
	}
}

// Nothing compiled lands where later runs share it: every Gradle home Agentium writes has the build cache off (beating
// the project's own org.gradle.caching=true), no daemon and Kotlin compiling in process; the warm-ups of both tools
// turn the build cache off on their command lines; agents are denied the deps folder's build caches.
func TestBuildCachesAreOffAndDenied(t *testing.T) {
	deps, cache, run := t.TempDir(), t.TempDir(), t.TempDir()
	if err := PrepareDeps(Select([]string{"gradle"}), deps); err != nil {
		t.Fatal(err)
	}
	if err := PrepareCommands(cache); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRun(context.Background(), Select([]string{"gradle"}), deps, run); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{"deps": filepath.Join(deps, "gradle", "gradle.properties"),
		"commands": filepath.Join(cache, "gradle", "gradle.properties"), "run": filepath.Join(run, "gradle", "gradle.properties")} {
		props, _ := os.ReadFile(file)
		for _, want := range []string{"org.gradle.caching=false", "org.gradle.daemon=false", "kotlin.compiler.execution.strategy=in-process"} {
			if !strings.Contains(string(props), want) {
				t.Errorf("%s Gradle home: no %s in %q", name, want, props)
			}
		}
	}
	for _, step := range WarmSteps(Select([]string{"gradle", "maven"}), repoWith(t, "gradlew"), deps) {
		switch {
		case strings.Contains(step.Command, "gradlew"):
			if !strings.Contains(step.Command, "--no-build-cache") {
				t.Errorf("a Gradle warm-up with the build cache on: %q", step.Command)
			}
		case !strings.Contains(step.Command, "-Dmaven.build.cache.enabled=false"):
			t.Errorf("a Maven warm-up with the build cache extension on: %q", step.Command)
		}
	}
	// Python's download caches and resolve reports follow (see TestPythonDepsDeniedWhole).
	if denied := DepsDenied(deps); !slices.Equal(denied[:2], []string{filepath.Join(deps, "gradle"), filepath.Join(deps, "build-cache")}) {
		t.Errorf("denied %q: the whole Gradle home and the build cache", denied)
	}
}

// What a warm-up writes to the deps folder's Gradle home is denied to agents as a whole (a caches/<version> folder made
// by a warm-up after an agent started included), and what they read lies outside it: GRADLE_RO_DEP_CACHE names a folder
// holding only modules-2, and the toolchain JDKs have a folder of their own. Warm-ups write both through links in
// the home.
func TestGradleDepsLayout(t *testing.T) {
	deps, run := t.TempDir(), t.TempDir()
	if err := PrepareDeps(Select([]string{"gradle"}), deps); err != nil {
		t.Fatal(err)
	}
	guh := filepath.Join(deps, "gradle")
	for link, want := range map[string]string{filepath.Join(guh, "caches", "modules-2"): "../../gradle-ro/modules-2", filepath.Join(guh, "jdks"): "../gradle-jdks"} {
		if got, err := os.Readlink(link); err != nil || got != filepath.FromSlash(want) {
			t.Errorf("%s -> %q (%v), want %s", link, got, err, want)
		}
	}
	// A warm-up writes through the links; a later one, with another Gradle version, adds its own caches.
	for _, f := range []string{filepath.Join("caches", "modules-2", "files-2.1", "a.jar"), filepath.Join("jdks", "jdk-21", "release"),
		filepath.Join("jdks", "temurin-21", "jdk-21", "bin", "java"), filepath.Join("jdks", "temurin-21", "jdk-21", "provisioned.ok"),
		filepath.Join("caches", "9.9", "javaCompile", "classAnalysis.bin")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(guh, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(guh, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(deps, gradleRO)); len(entries) != 1 || entries[0].Name() != "modules-2" || !entries[0].IsDir() {
		t.Errorf("the read-only cache holds %v, want only the modules-2 folder", entries)
	}
	for _, f := range []string{filepath.Join(gradleRO, "modules-2", "files-2.1", "a.jar"), filepath.Join(gradleJDKs, "jdk-21", "release")} {
		if _, err := os.Stat(filepath.Join(deps, f)); err != nil {
			t.Errorf("a warm-up's file is not where agents read it: %v", err)
		}
	}
	agent := env(t, AgentEnv(Select([]string{"gradle"}), AgentContext{Deps: deps, BuildCache: run}))
	if agent["GRADLE_RO_DEP_CACHE"] != filepath.Join(deps, gradleRO) {
		t.Errorf("GRADLE_RO_DEP_CACHE = %q", agent["GRADLE_RO_DEP_CACHE"])
	}
	if err := PrepareRun(context.Background(), Select([]string{"gradle"}), deps, run); err != nil {
		t.Fatal(err)
	}
	props, _ := os.ReadFile(filepath.Join(run, "gradle", "gradle.properties"))
	if !strings.Contains(string(props), "org.gradle.java.installations.paths="+filepath.Join(deps, gradleJDKs, "temurin-21", "jdk-21")+"\n") {
		t.Errorf("the run's JDKs: %q (the JDK a warm-up provisioned through the link)", props)
	}
	// Agents read only those two folders: neither is under a denied path, and everything in the Gradle home is.
	denied := DepsDenied(deps)
	under := func(path string) bool {
		return slices.ContainsFunc(denied, func(d string) bool { return path == d || strings.HasPrefix(path, d+string(filepath.Separator)) })
	}
	for _, f := range []string{agent["GRADLE_RO_DEP_CACHE"], filepath.Join(deps, gradleJDKs)} {
		if under(f) {
			t.Errorf("%s is denied", f)
		}
	}
	err := filepath.WalkDir(deps, func(path string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(deps, path)
		if top := strings.Split(rel, string(filepath.Separator))[0]; top != "." && top != gradleRO && top != gradleJDKs && !under(path) {
			t.Errorf("%s is readable", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Laying out again, as each warm-up does, changes nothing.
	if err := PrepareDeps(Select([]string{"gradle"}), deps); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(guh, "caches", "modules-2", "files-2.1", "a.jar")); err != nil {
		t.Errorf("laid out again: %v", err)
	}
}

// A deps folder from before the layout: its modules-2 and jdks folders move out in one rename each, with their files,
// and the links take their place. A crash after a rename is completed by the next call; an unexpected state is refused.
func TestGradleDepsLayoutMovesAnEarlierOne(t *testing.T) {
	deps := t.TempDir()
	guh := filepath.Join(deps, "gradle")
	for _, f := range []string{filepath.Join("caches", "modules-2", "files-2.1", "a.jar"), filepath.Join("jdks", "jdk-21", "release")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(guh, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(guh, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareDeps(Select([]string{"gradle"}), deps); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(gradleRO, "modules-2", "files-2.1", "a.jar"), filepath.Join(gradleJDKs, "jdk-21", "release")} {
		if _, err := os.Stat(filepath.Join(deps, f)); err != nil {
			t.Errorf("not moved: %v", err)
		}
	}
	if info, err := os.Lstat(filepath.Join(guh, "caches", "modules-2")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("no link in modules-2's place: %v %v", info, err)
	}

	// A crash between the rename and the link; then a link to somewhere else: both are made right.
	link := filepath.Join(guh, "caches", "modules-2")
	must(t, os.Remove(link))
	if err := linkOutside(link, filepath.Join(deps, gradleRO, "modules-2")); err != nil {
		t.Fatal(err)
	}
	must(t, os.Remove(link))
	must(t, os.Symlink(filepath.Join(deps, "elsewhere"), link))
	if err := linkOutside(link, filepath.Join(deps, gradleRO, "modules-2")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != filepath.Join("..", "..", gradleRO, "modules-2") {
		t.Errorf("link %q", got)
	}
	if _, err := os.Stat(filepath.Join(link, "files-2.1", "a.jar")); err != nil {
		t.Errorf("files lost: %v", err)
	}

	// Both a folder in the home and one outside: which is current is unknown, so nothing changes.
	must(t, os.Remove(link))
	must(t, os.MkdirAll(filepath.Join(link, "files-2.1"), 0o755))
	if err := PrepareDeps(Select([]string{"gradle"}), deps); err == nil || !strings.Contains(err.Error(), "both folders") ||
		!strings.Contains(err.Error(), "remove one by hand") {
		t.Errorf("both folders, and what to do: %v", err)
	}
	if _, err := os.Stat(filepath.Join(deps, gradleRO, "modules-2", "files-2.1", "a.jar")); err != nil {
		t.Errorf("the outside folder changed: %v", err)
	}
	// Something other than a folder outside (a link back into the home, say) is refused, and so is a file in the home.
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, "gradle", "caches"), 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, os.Symlink(filepath.Join(other, "gradle"), filepath.Join(other, "gradle-jdks")))
	if err := linkOutside(filepath.Join(other, "gradle", "jdks"), filepath.Join(other, "gradle-jdks")); err == nil {
		t.Error("an outside folder that is a link is accepted")
	}
	must(t, os.WriteFile(filepath.Join(other, "gradle", "caches", "modules-2"), nil, 0o644))
	if err := linkOutside(filepath.Join(other, "gradle", "caches", "modules-2"), filepath.Join(other, gradleRO, "modules-2")); err == nil {
		t.Error("a file in the link's place is accepted")
	}
}

// After the migration from an earlier layout, readers keep finding modules-2's files, through GRADLE_RO_DEP_CACHE and
// through the home's link, while later warm-ups lay the folder out again. (A reader of the earlier layout during the
// migration itself can miss caches/modules-2 between the rename and the link: no test can rule that window out.)
func TestGradleDepsLayoutKeepsModulesForReaders(t *testing.T) {
	deps := t.TempDir()
	guh := filepath.Join(deps, "gradle")
	earlier := filepath.Join(guh, "caches", "modules-2", "files-2.1", "a.jar")
	must(t, os.MkdirAll(filepath.Dir(earlier), 0o755))
	must(t, os.WriteFile(earlier, []byte("x"), 0o644))
	if err := PrepareDeps(Select([]string{"gradle"}), deps); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(guh, "caches", "modules-2")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("not migrated: %v %v", info, err)
	}
	readable := []string{filepath.Join(deps, gradleRO, "modules-2", "files-2.1", "a.jar"), earlier}
	stop, missing := make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(missing)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, f := range readable {
				if _, err := os.ReadFile(f); err != nil {
					missing <- err
					return
				}
			}
		}
	}()
	for range 200 {
		if err := PrepareDeps(Select([]string{"gradle"}), deps); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	if err := <-missing; err != nil {
		t.Errorf("a reader lost modules-2: %v", err)
	}
}

// The run's Gradle home lists each provisioned JDK's Java home (Gradle takes each entry of
// org.gradle.java.installations.paths as one installation): Gradle's layouts on macOS and Linux, and a JDK unpacked
// straight into its folder. Unfinished, linked or misshapen entries are not listed.
func TestProvisionedJDKs(t *testing.T) {
	dir := t.TempDir()
	file := func(rel string) {
		t.Helper()
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755))
		must(t, os.WriteFile(filepath.Join(dir, rel), nil, 0o755))
	}
	if got, err := provisionedJDKs(filepath.Join(dir, "missing")); err != nil || got != nil {
		t.Errorf("a missing folder: %q %v", got, err)
	}
	file("eclipse_adoptium-21-aarch64-os_x/jdk-21.0.6+7/provisioned.ok") // macOS
	file("eclipse_adoptium-21-aarch64-os_x/jdk-21.0.6+7/Contents/Home/bin/java")
	file("eclipse_adoptium-17-x64-linux/jdk-17.0.9+9/provisioned.ok") // Linux
	file("eclipse_adoptium-17-x64-linux/jdk-17.0.9+9/bin/java")
	file("azul_zulu-11-x64-linux/.ready") // unpacked into its folder, the newer marker
	file("azul_zulu-11-x64-linux/bin/java")
	file("amazon_corretto-8-x64-linux/.ready") // the newer marker next to the JDK's folder
	file("amazon_corretto-8-x64-linux/jdk8u402/bin/java")
	file("unfinished-22/jdk-22/bin/java") // no marker: may still be unpacking
	file("no-java-23/jdk-23/provisioned.ok")
	file("no-java-23/jdk-23/bin/javac")
	file("with,comma/jdk-24/provisioned.ok")
	file("with,comma/jdk-24/bin/java")
	file("OpenJDK21U-jdk_aarch64_mac_hotspot_21.0.6_7.tar.gz") // the archive and its lock
	file("OpenJDK21U-jdk_aarch64_mac_hotspot_21.0.6_7.tar.gz.lock")
	// Links never lead out of the folder: a whole JDK, a JDK's folder, its Contents or its bin.
	outside := t.TempDir()
	for _, rel := range []string{"jdk/provisioned.ok", "jdk/bin/java", "jdk/Contents/Home/bin/java", "bin/java"} {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(outside, rel)), 0o755))
		must(t, os.WriteFile(filepath.Join(outside, rel), nil, 0o755))
	}
	must(t, os.Symlink(filepath.Join(outside, "jdk"), filepath.Join(dir, "linked-whole")))
	file("linked-jdk/.ready")
	must(t, os.Symlink(filepath.Join(outside, "jdk"), filepath.Join(dir, "linked-jdk", "jdk-25")))
	file("linked-contents/jdk-26/provisioned.ok")
	must(t, os.Symlink(filepath.Join(outside, "jdk", "Contents"), filepath.Join(dir, "linked-contents", "jdk-26", "Contents")))
	file("linked-bin/jdk-27/provisioned.ok")
	must(t, os.Symlink(filepath.Join(outside, "bin"), filepath.Join(dir, "linked-bin", "jdk-27", "bin")))
	file("linked-java/jdk-28/provisioned.ok")
	must(t, os.MkdirAll(filepath.Join(dir, "linked-java", "jdk-28", "bin"), 0o755))
	must(t, os.Symlink(filepath.Join(outside, "bin", "java"), filepath.Join(dir, "linked-java", "jdk-28", "bin", "java")))

	got, err := provisionedJDKs(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "amazon_corretto-8-x64-linux", "jdk8u402"),
		filepath.Join(dir, "azul_zulu-11-x64-linux"),
		filepath.Join(dir, "eclipse_adoptium-17-x64-linux", "jdk-17.0.9+9"),
		filepath.Join(dir, "eclipse_adoptium-21-aarch64-os_x", "jdk-21.0.6+7", "Contents", "Home"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("provisionedJDKs = %q, want %q", got, want)
	}

	// The run's home lists them, comma-separated.
	deps, cache := t.TempDir(), t.TempDir()
	must(t, os.Rename(dir, filepath.Join(deps, gradleJDKs)))
	if err := PrepareRun(context.Background(), Select([]string{"gradle"}), deps, cache); err != nil {
		t.Fatal(err)
	}
	props, _ := os.ReadFile(filepath.Join(cache, "gradle", "gradle.properties"))
	for i := range want {
		want[i] = filepath.Join(deps, gradleJDKs, strings.TrimPrefix(want[i], dir))
	}
	if line := "org.gradle.java.installations.paths=" + strings.Join(want, ",") + "\n"; !strings.Contains(string(props), line) {
		t.Errorf("gradle.properties = %q, want the line %q", props, line)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Only a plain Java class name goes into the Maven warm-up's command line.
func TestMavenWarmUpNamesOnlyJavaIdentifiers(t *testing.T) {
	for file, want := range map[string]string{"src/test/java/a/FooTest.java": "-Dtest=FooTest ", "src/test/java/a/Foo$(touch x)Test.java": "AgentiumWarmNoSuchTest",
		"src/test/java/a/Foo;rm -rf ~Test.java": "AgentiumWarmNoSuchTest", "src/test/java/a/Bar-Test.java": "AgentiumWarmNoSuchTest"} {
		steps := WarmSteps(Select([]string{"maven"}), repoWith(t, file), "/d")
		if len(steps) != 1 || !strings.Contains(steps[0].Command, want) {
			t.Errorf("%s: %q", file, steps)
		}
	}
}

// A Cargo config that cannot be read within bounds stops a Cargo run (fail closed: skipping it would deny too little);
// ordinary and missing configs are fine.
func TestCheckConfigsFailsClosed(t *testing.T) {
	home, root := t.TempDir(), filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(filepath.Join(root, ".cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckConfigs(nil, home, []string{root}); err != nil {
		t.Fatalf("no configs: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cargo", "config.toml"), []byte("[build]\njobs = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckConfigs(nil, home, []string{root}); err != nil {
		t.Fatalf("an ordinary config: %v", err)
	}
	// Over the bound.
	big := filepath.Join(root, ".cargo", "config.toml")
	if err := os.WriteFile(big, make([]byte, maxConfigBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckConfigs(nil, home, []string{root}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("an oversized config: %v", err)
	}
	if got, err := ProjectCaches(nil, home, []string{root}); err == nil || len(got) != 0 {
		t.Errorf("an oversized config is read or skipped: %q, %v", got, err)
	} else if !strings.Contains(err.Error(), "regular file under 1 MiB") {
		t.Errorf("the message: %v", err)
	}
	// Not a regular file: a FIFO would block a reader forever, so it is refused without being opened.
	if err := os.Remove(big); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(big, 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- CheckConfigs(nil, home, []string{root}) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("a FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO config blocks")
	}
	if _, err := readConfig("/dev/zero"); err == nil {
		t.Error("/dev/zero is read")
	}
}

// A log with another hard link is not the daemon's own; one link, or an unknown count, is.
func TestStopGradleDaemonsRequiresASingleLink(t *testing.T) {
	cache, log := gradleHome(t)
	h := &stopHost{daemons: []int{4242, 5000, 6000}, open: map[int][]string{4242: {log(4242)}, 5000: {log(5000)}, 6000: {log(6000)}},
		links: map[string]int{log(4242): 2, log(5000): 1}, alive: map[int]bool{4242: true, 5000: true, 6000: true}}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), mustReal(t, cache), h.host()); err != nil {
		t.Fatal(err)
	}
	if !h.alive[4242] || h.alive[5000] || h.alive[6000] {
		t.Errorf("alive %v: a log with two links must not match; one link and an unknown count do", h.alive)
	}
}

// A daemon named by a log of the run's Gradle home that is running but could not be matched (a renamed log, a case
// mismatch) is reported, so the record says one may have been left; nothing is signalled on the strength of that log.
func TestStopGradleDaemonsNotesAnUnmatchedDaemon(t *testing.T) {
	cache, log := gradleHome(t)
	if err := os.WriteFile(log(4242), nil, 0o600); err != nil { // log() gives the real path; the folder is the cache's
		t.Fatal(err)
	}
	h := &stopHost{daemons: []int{4242}, open: map[int][]string{4242: {"/private/other/place/daemon-4242.out.log"}}, alive: map[int]bool{4242: true}}
	err := StopRun(context.Background(), Select([]string{"gradle"}), mustReal(t, cache), h.host())
	if err == nil || !strings.Contains(err.Error(), "may have been left running") || len(h.signals) != 0 {
		t.Errorf("an unmatched daemon: %v, signals %v", err, h.signals)
	}
	// A log of a process that has ended is the ordinary case: no note.
	h = &stopHost{open: map[int][]string{}, alive: map[int]bool{}}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), mustReal(t, cache), h.host()); err != nil {
		t.Errorf("a finished daemon's log: %v", err)
	}
}

// Cargo's build-dir templates: {workspace-root} is the repository (and each worktree), {cargo-cache-home} each Cargo
// home, and what is left (a hash) is cut off, denying the folder that holds every such directory. An environment value
// is resolved the same way, not skipped.
func TestCargoBuildDirTemplates(t *testing.T) {
	roots, homes := []string{"/work/app", "/work/app-wt"}, []string{"/home/u/.cargo", "/opt/ch"}
	for _, c := range []struct {
		value string
		want  []string
	}{
		{"{workspace-root}/../.build/{workspace-path-hash}", []string{"/work/.build", "/work/.build"}},
		{"{workspace-root}/target", []string{"/work/app/target", "/work/app-wt/target"}},
		{"{cargo-cache-home}/build/{workspace-path-hash}", []string{"/home/u/.cargo/build", "/opt/ch/build"}},
		{"/abs/{workspace-path-hash}/out", []string{"/abs"}},
		{"{unknown}/x", nil},
		{"/plain/dir", []string{"/plain/dir"}},
	} {
		got := resolveBuildDir(c.value, "/cfg", roots, homes)
		if !slices.Equal(got, c.want) && !(c.value == "{workspace-root}/../.build/{workspace-path-hash}" && slices.Contains(got, "/work/.build")) {
			t.Errorf("%s: %q, want %q", c.value, got, c.want)
		}
	}
	if got := resolveBuildDir("{workspace-root}/x", "", nil, nil); got != nil {
		t.Errorf("a template with no known root: %q", got)
	}
	if got := resolveBuildDir("relative/{h}", "/cfg", nil, nil); !slices.Equal(got, []string{"/cfg/relative"}) {
		t.Errorf("a relative value starts at the config's folder: %q", got)
	}
	// Through the configs and the environment.
	home := t.TempDir()
	repo := filepath.Join(home, "work", "app")
	if err := os.MkdirAll(filepath.Join(repo, ".cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cargo", "config.toml"), []byte("[build]\nbuild-dir = \"{workspace-root}/../.build/{workspace-path-hash}\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ProjectCaches([]string{"CARGO_BUILD_BUILD_DIR={cargo-cache-home}/b/{workspace-path-hash}"}, home, []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(home, "work", ".build"), filepath.Join(home, ".cargo", "b")} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not denied: %q", want, got)
		}
	}
}

// A module's keys differ from the root's and from every other module's; the root's are what they were before modules.
func TestModuleKeysAreSeparate(t *testing.T) {
	if ModuleKey("") != "" {
		t.Error("the root has a module key")
	}
	a, b := ModuleKey("services/api"), ModuleKey("services/web")
	if a == "" || a == b || strings.ContainsAny(a, "/-") || ModuleKey("services/api") != a {
		t.Errorf("module keys %q, %q", a, b)
	}
	if moduleVenvKey("0123456789abcdef", "") != "0123456789abcdef" {
		t.Error("the root's venv key changed")
	}
	if k1, k2 := moduleVenvKey("0123456789abcdef", "a"), moduleVenvKey("0123456789abcdef", "b"); k1 == k2 || k1 == "0123456789abcdef" || len(k1) != 16 {
		t.Errorf("venv keys %q, %q", k1, k2)
	}
}

func TestInModule(t *testing.T) {
	paths := []string{"README.md", "svc/go.mod", "svc/src/a.py", "svcx/go.mod", "svc2/b.py"}
	if got := InModule(paths, ""); !slices.Equal(got, paths) {
		t.Errorf("no module: %v", got)
	}
	if got := InModule(paths, "svc"); !slices.Equal(got, []string{"go.mod", "src/a.py"}) {
		t.Errorf("svc: %v", got)
	}
}
