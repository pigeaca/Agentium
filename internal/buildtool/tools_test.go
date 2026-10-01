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
		"MAVEN_ARGS": "-o -Dmaven.repo.local=/data/workspaces/r1/go-build/m2 -Dmaven.repo.local.tail=/data/deps/1/m2"} {
		if maven[name] != want {
			t.Errorf("maven: %s = %q, want %q", name, maven[name], want)
		}
	}
	gradle := env(t, AgentEnv(Select([]string{"gradle"}), ctx))
	for name, want := range map[string]string{"JAVA_HOME": "/host/jdk", "GRADLE_USER_HOME": "/data/workspaces/r1/go-build/gradle",
		"GRADLE_RO_DEP_CACHE": "/data/deps/1/gradle/caches"} {
		if gradle[name] != want {
			t.Errorf("gradle: %s = %q, want %q", name, gradle[name], want)
		}
	}
	cargo := env(t, AgentEnv(Select([]string{"cargo"}), ctx))
	for name, want := range map[string]string{"CARGO_HOME": "/data/deps/1/cargo", "CARGO_NET_OFFLINE": "true", "RUSTC_WRAPPER": "", "RUSTC_WORKSPACE_WRAPPER": "",
		"CARGO_TARGET_DIR": "/data/workspaces/r1/repo/target"} {
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
	for name, want := range map[string]string{"MAVEN_USER_HOME": "/data/cache/maven", "MAVEN_ARGS": "-Dmaven.repo.local=/data/cache/m2",
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
	if !strings.Contains(string(props), "org.gradle.cache.cleanup=false") || !strings.Contains(string(script), "Cleanup.DISABLED") {
		t.Errorf("cleanup is not disabled: %q %q", props, script)
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
	if want := "org.gradle.daemon=false\norg.gradle.java.installations.paths=" + filepath.Join(deps, "gradle", "jdks") + "\norg.gradle.java.installations.auto-download=false\n"; string(props) != want {
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

// stopHost is a fake machine: processes with command lines and open files, and signals that end them.
type stopHost struct {
	commands map[int]string
	open     map[int][]string
	alive    map[int]bool
	ignores  map[int]bool // processes that survive SIGTERM
	signals  []string
}

func (h *stopHost) host() Host {
	return Host{
		Command: func(_ context.Context, pid int) string { return h.commands[pid] },
		OpenFiles: func(_ context.Context, pid int) ([]string, error) {
			if h.open == nil {
				return nil, errors.New("no lsof")
			}
			return h.open[pid], nil
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

const daemonCommand = "/jdk/bin/java -Xmx8g org.gradle.launcher.daemon.bootstrap.GradleDaemon 9.7.1"

// Daemons the run left are stopped through the logs of its own Gradle home, and only when the process is a Gradle daemon
// that has that very log open. The agent can write the logs, so planted ones naming other processes (the user's own
// daemon, another run's, anything) kill nothing. Nothing from the checkout runs. A fake machine stands for the host.
func TestStopGradleDaemons(t *testing.T) {
	cache, other := t.TempDir(), t.TempDir()
	logs := filepath.Join(cache, "gradle", "daemon", "9.7.1")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	realCache, _ := filepath.EvalSymlinks(cache) // lsof reports real paths (/private/var on macOS)
	log := func(pid int) string { return filepath.Join(logs, "daemon-"+strconv.Itoa(pid)+".out.log") }
	realLog := func(pid int) string {
		return filepath.Join(realCache, "gradle", "daemon", "9.7.1", "daemon-"+strconv.Itoa(pid)+".out.log")
	}
	// The user's own daemon, with its own log elsewhere.
	userLog := filepath.Join(other, "daemon-5000.out.log")
	for _, f := range []string{log(4242), log(5000), log(777), log(6000), log(1), filepath.Join(logs, "daemon-x.out.log"), userLog} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Planted: a link to the user's daemon's real log, under the run's own Gradle home.
	if err := os.Remove(log(6000)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(userLog, log(6000)); err != nil {
		t.Fatal(err)
	}
	h := &stopHost{
		commands: map[int]string{4242: daemonCommand, 5000: daemonCommand, 6000: daemonCommand, 777: "/usr/bin/vim notes.txt"},
		open: map[int][]string{4242: {realLog(4242)}, // the run's own daemon: has its log open
			5000: {userLog},              // the user's daemon, named by a planted log in the run's folder: holds another file
			6000: {mustReal(t, userLog)}, // named through a link out of the run's folder
			777:  {realLog(777)}},        // not a daemon at all
		alive: map[int]bool{4242: true, 5000: true, 6000: true, 777: true},
	}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), cache, h.host()); err != nil {
		t.Fatal(err)
	}
	if h.alive[4242] || !h.alive[5000] || !h.alive[6000] || !h.alive[777] {
		t.Errorf("alive: %v (only the run's own daemon must stop)", h.alive)
	}
	if !slices.Equal(h.signals, []string{"4242:terminated"}) {
		t.Errorf("signals %v", h.signals)
	}
	// A daemon that ignores SIGTERM is killed after the grace period.
	h = &stopHost{commands: map[int]string{4242: daemonCommand}, open: map[int][]string{4242: {realLog(4242)}},
		alive: map[int]bool{4242: true}, ignores: map[int]bool{4242: true}}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), cache, h.host()); err != nil {
		t.Fatal(err)
	}
	if h.alive[4242] || !slices.Equal(h.signals, []string{"4242:terminated", "4242:killed"}) {
		t.Errorf("a stubborn daemon: alive %v, signals %v", h.alive, h.signals)
	}
	// A machine that cannot tell what a process has open (no lsof) is not trusted: nothing is signalled, and it is said.
	h = &stopHost{commands: map[int]string{4242: daemonCommand}, alive: map[int]bool{4242: true}}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), cache, h.host()); err == nil || len(h.signals) != 0 {
		t.Errorf("without lsof: error %v, signals %v", err, h.signals)
	}
	// No Gradle home, no error.
	if err := StopRun(context.Background(), Select([]string{"gradle"}), t.TempDir(), h.host()); err != nil {
		t.Errorf("a run without daemons: %v", err)
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

// The agent can plant as many logs as it likes: only the first few are read, and a cancelled context still ends a
// daemon that ignores SIGTERM at once.
func TestStopGradleDaemonsIsBounded(t *testing.T) {
	cache := t.TempDir()
	logs := filepath.Join(cache, "gradle", "daemon", "9")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	realLogs := filepath.Join(mustReal(t, cache), "gradle", "daemon", "9")
	h := &stopHost{commands: map[int]string{}, open: map[int][]string{}, alive: map[int]bool{}}
	for pid := 100; pid < 140; pid++ { // 40 daemons' logs, in pid order
		if err := os.WriteFile(filepath.Join(logs, "daemon-"+strconv.Itoa(pid)+".out.log"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		h.commands[pid], h.alive[pid] = daemonCommand, true
		h.open[pid] = []string{filepath.Join(realLogs, "daemon-"+strconv.Itoa(pid)+".out.log")}
	}
	if err := StopRun(context.Background(), Select([]string{"gradle"}), cache, h.host()); err != nil {
		t.Fatal(err)
	}
	if len(h.signals) != maxDaemonLogs {
		t.Errorf("%d daemons signalled, want the first %d", len(h.signals), maxDaemonLogs)
	}
	h2 := &stopHost{commands: map[int]string{100: daemonCommand}, open: map[int][]string{100: {filepath.Join(realLogs, "daemon-100.out.log")}},
		alive: map[int]bool{100: true}, ignores: map[int]bool{100: true}}
	host := h2.host()
	host.Grace = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := StopRun(ctx, Select([]string{"gradle"}), cache, host); err != nil || h2.alive[100] || time.Since(start) > 5*time.Second {
		t.Errorf("a cancelled stop: %v, alive %v, after %v", err, h2.alive, time.Since(start))
	}
}

// Agentium's own Gradle commands never start a daemon.
func TestGradleCommandsGetNoDaemon(t *testing.T) {
	cache := t.TempDir()
	if err := PrepareCommands(cache); err != nil {
		t.Fatal(err)
	}
	if props, _ := os.ReadFile(filepath.Join(cache, "gradle", "gradle.properties")); string(props) != "org.gradle.daemon=false\n" {
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

// A target directory set in the user's Cargo config or environment cannot take grading's compiled hidden tests to a
// folder agents read: the user's are denied, and Agentium's own commands and the agent set CARGO_TARGET_DIR themselves.
func TestCargoTargetDirectories(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[net]\ngit-fetch-with-cli = true\n\n[build]\njobs = 4\ntarget-dir = \"/shared/target\" # fast disk\n\n[env]\ntarget-dir = \"/not/this\"\n"
	if err := os.WriteFile(filepath.Join(home, ".cargo", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got := UserCaches([]string{"CARGO_BUILD_TARGET_DIR=/env/target"}, home)
	for _, want := range []string{"/shared/target", "/env/target"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not denied: %q", want, got)
		}
	}
	if slices.Contains(got, "/not/this") {
		t.Error("a target-dir outside [build] is denied")
	}
	// Relative to the folder above .cargo, as Cargo reads it.
	if err := os.WriteFile(filepath.Join(home, ".cargo", "config.toml"), []byte("[build]\ntarget-dir = 'out/target'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := UserCaches(nil, home); !slices.Contains(got, filepath.Join(home, "out", "target")) {
		t.Errorf("a relative target-dir: %q", got)
	}
}
