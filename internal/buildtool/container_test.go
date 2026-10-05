package buildtool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/container"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A deps volume is seeded from the host's platform-independent caches only: the modules go.sum names from the user's
// module cache, Maven's repository and wrapper, Gradle's modules-2, distributions and p2-data, and Cargo's registry;
// never a build's output, a venv, or the rest of the user's caches. Every top folder is made for the warm-ups.
func TestContainerSeeds(t *testing.T) {
	deps, repo, home := t.TempDir(), t.TempDir(), t.TempDir()
	modcache := filepath.Join(home, "go", "pkg", "mod")
	write(t, repo, map[string]string{"go.sum": "github.com/BurntSushi/toml v1.4.0 h1:x=\ngithub.com/BurntSushi/toml v1.4.0/go.mod h1:y=\n" +
		"golang.org/x/mod v0.20.0/go.mod h1:z=\n../evil v1 h1:x=\n"})
	write(t, modcache, map[string]string{
		"cache/download/github.com/!burnt!sushi/toml/@v/v1.4.0.zip": "zip",
		"cache/download/github.com/!burnt!sushi/toml/@v/v1.4.0.mod": "mod",
		"cache/download/github.com/!burnt!sushi/toml/@v/v1.3.0.zip": "older: not named",
		"cache/download/github.com/!burnt!sushi/toml/@v/list":       "v1.4.0\n",
		"cache/download/golang.org/x/mod/@v/v0.20.0.mod":            "mod",
		"cache/download/example.com/unrelated/@v/v1.0.0.zip":        "not named",
		"github.com/!burnt!sushi/toml@v1.4.0/decode.go":             "extracted: not seeded",
	})
	write(t, deps, map[string]string{
		"m2/org/x/x.jar": "jar", "mvnw-home/wrapper/dists/m/maven.zip": "zip",
		"gradle-ro/modules-2/files-2.1/a.jar": "jar", "gradle/wrapper/dists/gradle-8.5-bin/x/gradle-8.5.zip": "zip",
		"gradle/caches/p2-data/bundle.jar": "p2", "gradle/caches/8.5/compiled.bin": "compiled: not seeded", "gradle/jdks/macos-jdk/bin/java": "macOS: not seeded",
		"cargo/registry/index/x": "i", "cargo/git/db/y": "g", "cargo/bin/tool": "not seeded",
		"py/abc/venv/bin/python": "a macOS venv: not seeded",
	})
	seeds, entries := ContainerSeeds(Select([]string{"go", "maven", "gradle", "cargo", "python"}), deps, repo, []string{"HOME=" + home}, home)
	var buf bytes.Buffer
	_, written, err := container.SeedTar(context.Background(), &buf, seeds, entries, container.SeedLimits())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"gradle/caches/modules-2", "gradle/gradle.properties", "gradle/init.d/agentium-no-cleanup.gradle", "gradle/" + resolveAllScriptName,
		"gomod/cache/download/github.com/!burnt!sushi/toml/@v/list", "gomod/cache/download/github.com/!burnt!sushi/toml/@v/v1.4.0.mod",
		"gomod/cache/download/github.com/!burnt!sushi/toml/@v/v1.4.0.zip", "gomod/cache/download/golang.org/x/mod/@v/v0.20.0.mod",
		"m2/org/x/x.jar", "mvnw-home/wrapper/dists/m/maven.zip",
		"gradle-ro/modules-2/files-2.1/a.jar", "gradle/wrapper/dists/gradle-8.5-bin/x/gradle-8.5.zip", "gradle/caches/p2-data/bundle.jar",
		"cargo/registry/index/x", "cargo/git/db/y",
	}
	if !slices.Equal(written, want) {
		t.Errorf("seeded:\n%q\nwant\n%q", written, want)
	}
	var tops []string
	for _, e := range entries {
		if e.Dir {
			tops = append(tops, e.Name)
		}
	}
	if !slices.Equal(tops, ContainerTop()) {
		t.Errorf("top folders %v", tops)
	}
	// A project of one tool seeds only its own caches (and Go's, which every project has).
	seeds, _ = ContainerSeeds(Select([]string{"cargo"}), deps, t.TempDir(), []string{"HOME=" + home}, home)
	if len(seeds) != 2 || seeds[0].To != "cargo/registry" {
		t.Errorf("cargo only: %+v", seeds)
	}
}

func TestGoModCache(t *testing.T) {
	home := t.TempDir()
	for _, tt := range []struct {
		env  []string
		want string
	}{
		{nil, filepath.Join(home, "go", "pkg", "mod")},
		{[]string{"GOPATH=/opt/gp:/other"}, "/opt/gp/pkg/mod"},
		{[]string{"GOPATH=/opt/gp", "GOMODCACHE=/cache/mod"}, "/cache/mod"},
		{[]string{"GOMODCACHE=relative"}, filepath.Join(home, "go", "pkg", "mod")},
	} {
		if got := goModCache(tt.env, home); got != tt.want {
			t.Errorf("%v: %s, want %s", tt.env, got, tt.want)
		}
	}
	for in, want := range map[string]string{"github.com/BurntSushi/toml": "github.com/!burnt!sushi/toml", "v1.0.0-RC1": "v1.0.0-!r!c1"} {
		if got, ok := escapeModule(in); !ok || got != want {
			t.Errorf("escape %s: %s", in, got)
		}
	}
	for _, bad := range []string{"../x", "/abs", "a//b", "a/./b", ""} {
		if _, ok := escapeModule(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The warm-up's steps: Go unpacks offline; Python always warms here, keyed by its inputs, and says when it cannot;
// Gradle only when the host's warm-up failed, as the host's steps with /deps for the deps folder.
func TestContainerWarmSteps(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"go.mod": "module m\n", "pyproject.toml": "[project]\nname='x'\n", "uv.lock": "version = 1\n", "gradlew": "#!/bin/sh\n"})
	w := ContainerWarmSteps(Select([]string{"go", "python", "gradle", "maven", "cargo"}), dir, false)
	var cmds []string
	for _, s := range w.Steps {
		cmds = append(cmds, s.Command)
		if !slices.Contains(s.Env, "HOME=/grade/cache/home") {
			t.Errorf("%s: no HOME in %v", s.Command, s.Env)
		}
	}
	if !slices.Equal(cmds, []string{"go mod download", "uv sync --frozen --no-install-project --no-install-workspace --no-install-local"}) {
		t.Errorf("steps %q", cmds)
	}
	if !slices.Contains(w.Steps[0].Env, "GOPROXY=off") || !slices.Contains(w.Steps[0].Env, "GOMODCACHE=/deps/gomod") {
		t.Errorf("go env %v", w.Steps[0].Env)
	}
	if !strings.HasPrefix(w.Venv, "/deps/py/") || !strings.HasSuffix(w.Venv, "/venv") || !slices.Contains(w.Steps[1].Env, "UV_PROJECT_ENVIRONMENT="+w.Venv) ||
		!slices.Contains(w.Steps[1].Env, "UV_PYTHON_DOWNLOADS=never") {
		t.Errorf("python: venv %s, env %v", w.Venv, w.Steps[1].Env)
	}
	// Another lock is another venv.
	write(t, dir, map[string]string{"uv.lock": "version = 2\n"})
	if w2 := ContainerWarmSteps(Select([]string{"python"}), dir, false); w2.Venv == w.Venv {
		t.Error("the venv's key ignores uv.lock")
	}
	os.Remove(filepath.Join(dir, "go.mod"))
	if w := ContainerWarmSteps(Select([]string{"gradle"}), dir, false); len(w.Steps) != 0 {
		t.Errorf("gradle after the host's warm-up: %+v", w.Steps)
	}
	failed := ContainerWarmSteps(Select([]string{"gradle"}), dir, true)
	if len(failed.Steps) != 3 || !strings.HasPrefix(failed.Steps[0].Command, "./gradlew ") || !slices.Contains(failed.Steps[0].Env, "GRADLE_USER_HOME=/deps/gradle") ||
		!strings.Contains(failed.Steps[2].Command, "/deps/gradle/"+resolveAllScriptName) {
		t.Errorf("gradle after a failed host warm-up: %+v", failed.Steps)
	}
	os.Remove(filepath.Join(dir, "uv.lock"))
	if w := ContainerWarmSteps(Select([]string{"python"}), dir, false); len(w.Steps) != 0 || len(w.Notes) != 1 || w.Venv != "" {
		t.Errorf("unlocked python: %+v", w)
	}
}
