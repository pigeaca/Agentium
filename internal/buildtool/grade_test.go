package buildtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// graderFixture is a run's folders as a grade sees them, and the agent's allowlisted environment with the user's own
// settings for every tool, which the grade's recipe must replace where it sets its own.
func graderFixture() (AgentContext, []string, string) {
	c := AgentContext{Home: "/Users/u", Repo: "/data/records/r1/verify", BuildCache: "/data/records/r1/grading/cache",
		Deps: "/data/deps/7", JavaHome: "/jdk/Contents/Home", Venv: "/data/deps/7/py/k1/venv", Metadata: "/data/deps/7/py-meta/k1/click-8.2.dist-info",
		ImportRoot: "src"}
	allowed := []string{"PATH=/usr/bin:/bin", "HOME=/Users/u", "TMPDIR=/var/folders/xy/T/", "LANG=en_US.UTF-8",
		"GOFLAGS=-mod=mod", "GOPROXY=https://proxy.example", "GOCACHE=/Users/u/Library/Caches/go-build", "GOTMPDIR=/Users/u/gotmp",
		"JAVA_HOME=/user/jdk", "CARGO_HOME=/Users/u/.cargo"}
	return c, allowed, "/data/records/r1/grading/tmp"
}

// envMap maps an environment's names to values, failing on a name given twice: a grade's environment has each once.
func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, kv := range env {
		name, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("entry %q has no =", kv)
		}
		if _, dup := m[name]; dup {
			t.Errorf("%s is given twice in %q", name, env)
		}
		m[name] = v
	}
	return m
}

// The grader's environment is the agent's recipe per profile, with the grade's own cache, copy and temp root, plus the
// isolation plan's two additions (the JVM's temp folder for Maven and Gradle; Gradle's IPv6 loopback bind address,
// with no IPv4-only flag) and Go offline. Each profile's own settings beat the user's.
func TestGraderEnvPerProfile(t *testing.T) {
	c, allowed, temp := graderFixture()
	for _, tc := range []struct {
		tools []string
		kept  []string
		want  map[string]string
		// absent are names that must not be set at all; prefix checks a value's start (PATH).
		absent []string
		prefix map[string]string
	}{
		{tools: []string{"go"}, want: map[string]string{"GOCACHE": c.BuildCache, "GOFLAGS": "-mod=mod -buildvcs=false", "GOPROXY": "off",
			"GOTMPDIR": temp, "TMPDIR": temp, "HOME": "/Users/u", "PATH": "/usr/bin:/bin", "LANG": "en_US.UTF-8"},
			absent: []string{"JAVA_TOOL_OPTIONS", "GRADLE_DAEMON_BIND_ADDRESS", "VIRTUAL_ENV", "CLAUDE_CODE_TMPDIR"}},
		{tools: []string{"maven"}, want: map[string]string{"JAVA_HOME": c.JavaHome, "MAVEN_USER_HOME": "/data/deps/7/mvnw-home",
			"MAVEN_ARGS":        "-o -Dmaven.repo.local=" + c.BuildCache + "/m2 -Dmaven.repo.local.tail=/data/deps/7/m2 -Dmaven.build.cache.enabled=false",
			"JAVA_TOOL_OPTIONS": "-Djava.io.tmpdir=" + temp, "TMPDIR": temp,
			// Go's side is off in a Maven project without Go files: the user's values pass as the agent's would.
			"GOCACHE": "/Users/u/Library/Caches/go-build", "GOPROXY": "https://proxy.example"},
			absent: []string{"GRADLE_DAEMON_BIND_ADDRESS"}},
		{tools: []string{"gradle"}, want: map[string]string{"JAVA_HOME": c.JavaHome, "GRADLE_USER_HOME": c.BuildCache + "/gradle",
			"GRADLE_RO_DEP_CACHE": "/data/deps/7/" + gradleRO, "JAVA_TOOL_OPTIONS": "-Djava.io.tmpdir=" + temp,
			"GRADLE_DAEMON_BIND_ADDRESS": "::1", "TMPDIR": temp}},
		{tools: []string{"gradle", "go"}, kept: []string{"go"}, want: map[string]string{"GRADLE_DAEMON_BIND_ADDRESS": "::1",
			"JAVA_TOOL_OPTIONS": "-Djava.io.tmpdir=" + temp, "GOCACHE": c.BuildCache, "GOPROXY": "off"}},
		{tools: []string{"cargo"}, want: map[string]string{"CARGO_TARGET_DIR": c.Repo + "/target", "CARGO_BUILD_BUILD_DIR": c.Repo + "/target",
			"CARGO_HOME": "/data/deps/7/cargo", "CARGO_NET_OFFLINE": "true", "RUSTC_WRAPPER": "", "RUSTC_WORKSPACE_WRAPPER": "", "TMPDIR": temp},
			absent: []string{"JAVA_TOOL_OPTIONS", "GRADLE_DAEMON_BIND_ADDRESS"}},
		{tools: []string{"python"}, want: map[string]string{"VIRTUAL_ENV": c.Venv, "PYTHONPATH": c.Repo + "/src:" + c.Metadata,
			"MYPYPATH": c.Repo + "/src", "PYTHONPYCACHEPREFIX": c.BuildCache + "/pycache", "HYPOTHESIS_STORAGE_DIRECTORY": c.BuildCache + "/hypothesis",
			"UV_CACHE_DIR": c.BuildCache + "/uv", "UV_OFFLINE": "1", "PIP_NO_INDEX": "1", "TMPDIR": temp},
			absent: []string{"JAVA_TOOL_OPTIONS", "GRADLE_DAEMON_BIND_ADDRESS"}, prefix: map[string]string{"PATH": c.Venv + "/bin:/usr/bin:/bin"}},
	} {
		t.Run(strings.Join(tc.tools, "+"), func(t *testing.T) {
			env, err := GraderEnv(SelectRun(tc.tools, tc.kept), allowed, c, temp)
			if err != nil {
				t.Fatal(err)
			}
			got := envMap(t, env)
			for name, want := range tc.want {
				if v, ok := got[name]; !ok || v != want {
					t.Errorf("%s = %q (set %t), want %q", name, v, ok, want)
				}
			}
			for _, name := range tc.absent {
				if v, ok := got[name]; ok {
					t.Errorf("%s = %q, want it unset", name, v)
				}
			}
			for name, prefix := range tc.prefix {
				if !strings.HasPrefix(got[name], prefix) {
					t.Errorf("%s = %q, want it to start with %q", name, got[name], prefix)
				}
			}
			// The IPv4-only flag breaks Gradle's ::1 bind address ("Unsupported address type").
			if strings.Contains(got["JAVA_TOOL_OPTIONS"], "preferIPv4Stack") {
				t.Errorf("JAVA_TOOL_OPTIONS = %q", got["JAVA_TOOL_OPTIONS"])
			}
		})
	}
}

// Whatever the allowed environment carries under the names the grade sets, the grade's own values win, once each: a
// user's (or a planted) JAVA_TOOL_OPTIONS with the IPv4-only flag, TMPDIR, GOCACHE or GRADLE_DAEMON_BIND_ADDRESS.
func TestGraderEnvReplacesTheAllowedValues(t *testing.T) {
	c, _, temp := graderFixture()
	allowed := []string{"PATH=/bin", "JAVA_TOOL_OPTIONS=-Djava.net.preferIPv4Stack=true", "TMPDIR=/var/folders/T", "TMPDIR=/again",
		"GOCACHE=/shared", "GRADLE_DAEMON_BIND_ADDRESS=127.0.0.1", "GRADLE_USER_HOME=/Users/u/.gradle"}
	env, err := GraderEnv(SelectRun([]string{"gradle", "go"}, []string{"go"}), allowed, c, temp)
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(t, env)
	for name, want := range map[string]string{"JAVA_TOOL_OPTIONS": "-Djava.io.tmpdir=" + temp, "TMPDIR": temp, "GOCACHE": c.BuildCache,
		"GRADLE_DAEMON_BIND_ADDRESS": "::1", "GRADLE_USER_HOME": c.BuildCache + "/gradle", "PATH": "/bin"} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
}

// A grade's folders must be absolute, and the temp root must fit in JAVA_TOOL_OPTIONS, which splits on white space.
func TestGraderEnvRefuses(t *testing.T) {
	c, allowed, temp := graderFixture()
	profiles := SelectRun([]string{"maven"}, nil)
	for name, change := range map[string]func(*AgentContext, *string){
		"a relative temp root":     func(_ *AgentContext, tmp *string) { *tmp = "tmp" },
		"a temp root with a space": func(_ *AgentContext, tmp *string) { *tmp = "/data/my runs/tmp" },
		"a temp root with a tab":   func(_ *AgentContext, tmp *string) { *tmp = "/data/x\ty" },
		"no cache":                 func(c *AgentContext, _ *string) { c.BuildCache = "" },
		"a relative copy":          func(c *AgentContext, _ *string) { c.Repo = "verify" },
	} {
		cc, tmp := c, temp
		change(&cc, &tmp)
		if _, err := GraderEnv(profiles, allowed, cc, tmp); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Seeds are per tool set and per agent side: Go's agent side on or off changes what a grade's cache holds (GOCACHE).
func TestSeedKey(t *testing.T) {
	keys := map[string]string{}
	for name, profiles := range map[string][]Profile{
		"go":            Select([]string{"go"}),
		"maven":         SelectRun([]string{"maven"}, nil),
		"maven with go": SelectRun([]string{"maven"}, []string{"go"}),
		"gradle":        SelectRun([]string{"gradle"}, nil),
		"python":        SelectRun([]string{"python"}, nil),
	} {
		key := SeedKey(profiles)
		if key != SeedKey(profiles) {
			t.Errorf("%s: the key is not stable", name)
		}
		if strings.ContainsAny(key, `/\ `) {
			t.Errorf("%s: key %q is not one file name", name, key)
		}
		for other, k := range keys {
			if k == key {
				t.Errorf("%s and %s share the key %s", name, other, key)
			}
		}
		keys[name] = key
	}
	// A repository with no tool detected gets Go's agent side, as a Go module does: the same cache, the same seed.
	if SeedKey(Select(nil)) != keys["go"] {
		t.Error("no tool detected and Go get different seeds")
	}
}

// copyTree says which cp copied, and the copy is independent of its source.
func TestCopyTreeSaysHow(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(filepath.Join(src, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "f"), []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	how, err := copyTree(t.Context(), src, dst)
	if err != nil || !strings.HasPrefix(how, "cp -R") {
		t.Fatalf("copyTree = %q, %v", how, err)
	}
	if err := os.WriteFile(filepath.Join(dst, "a", "f"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(src, "a", "f")); string(data) != "seed" {
		t.Errorf("the source changed: %q", data)
	}
	// A destination someone else made is refused and never removed, by copyTree or by CloneFolder's fallback.
	theirs := filepath.Join(t.TempDir(), "theirs")
	must(t, os.MkdirAll(theirs, 0o700))
	must(t, os.WriteFile(filepath.Join(theirs, "f"), []byte("theirs"), 0o600))
	if _, err := copyTree(t.Context(), src, theirs); err == nil {
		t.Error("copyTree copied into an existing folder")
	}
	if data, _ := os.ReadFile(filepath.Join(theirs, "f")); string(data) != "theirs" {
		t.Errorf("an existing folder changed: %q", data)
	}
	// A copy that fails removes only what it made.
	if _, err := copyTree(t.Context(), filepath.Join(src, "missing"), filepath.Join(t.TempDir(), "failed")); err == nil {
		t.Error("copied a missing folder")
	}
}
