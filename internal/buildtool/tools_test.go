package buildtool

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
		Home: "/home/u", BuildCache: "/data/workspaces/r1/go-build", Deps: "/data/deps/1", JavaHome: "/host/jdk"}
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
	for name, want := range map[string]string{"CARGO_HOME": "/data/deps/1/cargo", "CARGO_NET_OFFLINE": "true", "RUSTC_WRAPPER": "", "RUSTC_WORKSPACE_WRAPPER": ""} {
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
		"CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER": ""} {
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

// The warm-up commands name the wrapper when there is one, and put the fetched files in the deps folder.
func TestWarmSteps(t *testing.T) {
	steps := WarmSteps(Select([]string{"maven", "gradle", "cargo"}), hasFiles("mvnw", "gradlew"), "/data/deps/1")
	var commands []string
	for _, s := range steps {
		commands = append(commands, s.Command)
		if len(s.Env) == 0 || !strings.Contains(strings.Join(s.Env, " "), "/data/deps/1/") {
			t.Errorf("%q does not write into the deps folder: %q", s.Command, s.Env)
		}
	}
	joined := strings.Join(commands, "\n")
	for _, want := range []string{"./mvnw -B -q test", "./gradlew --no-daemon", "cargo fetch"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warm-up %q in %q", want, commands)
		}
	}
	plain := WarmSteps(Select([]string{"maven", "gradle"}), hasFiles(), "/d")
	if !strings.Contains(plain[0].Command, "mvn -B") || strings.Contains(plain[0].Command, "./mvnw") || strings.Contains(plain[1].Command, "./gradlew") {
		t.Errorf("without wrappers: %q", plain)
	}
	if got := NeedsWarming(Select([]string{"cargo", "maven"})); !slices.Equal(got, []string{"maven", "cargo"}) {
		t.Errorf("warmed tools: %q", got)
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
	if string(props) != "org.gradle.daemon=false\n" {
		t.Errorf("gradle.properties = %q", props)
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

// Daemons the run left are stopped through the logs of its own Gradle home, and only when the process really is a
// Gradle daemon; nothing from the checkout runs. A fake host stands for the machine.
func TestStopGradleDaemons(t *testing.T) {
	cache := t.TempDir()
	logs := filepath.Join(cache, "gradle", "daemon", "9.7.1")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"daemon-4242.out.log", "daemon-777.out.log", "daemon-1.out.log", "daemon-x.out.log", "registry.bin"} {
		if err := os.WriteFile(filepath.Join(logs, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	alive := map[int]bool{4242: true, 777: true}
	var signals []string
	host := Host{
		Command: func(pid int) string {
			switch pid {
			case 4242:
				return "/jdk/bin/java -Xmx8g org.gradle.launcher.daemon.bootstrap.GradleDaemon 9.7.1"
			case 777:
				return "/usr/bin/vim notes.txt" // not a daemon: the agent named it in a log
			}
			return ""
		},
		Signal: func(pid int, sig syscall.Signal) error {
			if !alive[pid] {
				return syscall.ESRCH
			}
			signals = append(signals, sig.String())
			if sig == syscall.SIGTERM {
				alive[pid] = false // the daemon ends on SIGTERM
			}
			return nil
		},
		Grace: time.Second,
	}
	if err := StopRun(Select([]string{"gradle"}), cache, host); err != nil {
		t.Fatal(err)
	}
	if alive[4242] || !alive[777] {
		t.Errorf("alive after the stop: %v (the daemon must be stopped, the other process left alone)", alive)
	}
	if len(signals) != 1 || signals[0] != "terminated" { // SIGTERM; the next probe finds it gone
		t.Errorf("signals: %v", signals)
	}
	// A daemon that ignores SIGTERM is killed after the grace period.
	stubborn := map[int]bool{4242: true}
	var killed bool
	host.Grace = 100 * time.Millisecond
	host.Signal = func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			killed = true
			stubborn[pid] = false
		}
		if !stubborn[pid] {
			return syscall.ESRCH
		}
		return nil
	}
	if err := StopRun(Select([]string{"gradle"}), cache, host); err != nil {
		t.Fatal(err)
	}
	if !killed {
		t.Error("a daemon that ignores SIGTERM is not killed")
	}
	// No Gradle home, no error.
	if err := StopRun(Select([]string{"gradle"}), t.TempDir(), host); err != nil {
		t.Errorf("a run without daemons: %v", err)
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
	run := func(string, ...string) (string, error) { return found + "\n", nil }
	if got := ResolveJavaHome([]string{"JAVA_HOME=" + user}, run); got != user {
		t.Errorf("the user's JAVA_HOME: %s", got)
	}
	if got := ResolveJavaHome([]string{"JAVA_HOME=" + filepath.Join(dir, "gone"), "PATH=/nonexistent"}, run); got == filepath.Join(dir, "gone") {
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
	if got := ResolveJavaHome([]string{"PATH=" + link}, func(string, ...string) (string, error) { return "", os.ErrNotExist }); got != resolvedPath {
		t.Errorf("java on PATH: %s, want %s", got, resolvedPath)
	}
	if got := ResolveJavaHome([]string{"PATH=/nonexistent"}, func(string, ...string) (string, error) { return "", os.ErrNotExist }); got != "" {
		t.Errorf("no JDK anywhere: %q", got)
	}
}
