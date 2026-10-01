package run

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/home"
)

func TestDepsFolderIsPerProjectOutsideTheDeniedFolders(t *testing.T) {
	env := Env{Layout: home.Layout{Root: "/data", Deps: "/data/deps"}, Bare: "/data/projects/7/repo.git"}
	got := env.depsFolder()
	if got != "/data/deps/7" {
		t.Errorf("deps folder %s", got)
	}
	// The agent must read it: it is none of the folders every run is denied.
	denied, err := Env{Layout: home.Layout{Root: "/data", Database: "/data/agentium.db", Artifacts: "/data/artifacts", Records: "/data/records",
		Workspaces: "/data/workspaces", Cache: "/data/cache", Temp: "/tmp"}, ProjectRoot: t.TempDir()}.denied(context.Background(), "/data/workspaces/r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range denied {
		if within(got, p) {
			t.Errorf("the deps folder %s lies inside the denied path %s", got, p)
		}
	}
	if (Env{Layout: home.Layout{Root: "/data"}}).depsFolder() != "" {
		t.Error("a layout without a deps folder names one")
	}
	if got := (Env{Layout: home.Layout{Deps: "/data/deps"}}).depsFolder(); got != "/data/deps/default" {
		t.Errorf("without a bare repository: %s", got)
	}
}

// Setup warms the dependencies once per base commit and tool set, with the step's own variables added to Agentium's
// command environment; a failing step is a note, not a failed run, and leaves no stamp so the next run tries again.
func TestWarmToolsStampsAndNotes(t *testing.T) {
	dir := t.TempDir()
	deps, log := filepath.Join(dir, "deps"), filepath.Join(dir, "calls")
	checkout := t.TempDir()
	env := Env{CommandEnv: []string{"BASE=1"}, VerifyTimeout: 20 * time.Second}
	steps := []buildtool.WarmStep{{Command: `echo "$BASE $MARK" >> ` + log, Env: []string{"MARK=a", "BASE=2"}}}
	ctx := context.Background()
	warm := func(base string, steps []buildtool.WarmStep) string {
		t.Helper()
		note, err := env.warmTools(ctx, checkout, deps, base, []string{"cargo"}, steps, filepath.Join(dir, "setup.log"), func(int) {})
		if err != nil {
			t.Fatal(err)
		}
		return note
	}
	if note := warm("c1", steps); note != "" {
		t.Errorf("note %q", note)
	}
	warm("c1", steps) // stamped
	if data, _ := os.ReadFile(log); string(data) != "2 a\n" {
		t.Errorf("calls %q: one warm-up, the step's variables after the command environment's", data)
	}
	warm("c2", steps) // another base commit
	if data, _ := os.ReadFile(log); string(data) != "2 a\n2 a\n" {
		t.Errorf("calls %q after a new base commit", data)
	}
	failing := []buildtool.WarmStep{{Command: "exit 3"}, {Command: `echo later >> ` + log}}
	note := warm("c3", failing)
	if !strings.Contains(note, "exit 3") || !strings.Contains(note, "setup.log") {
		t.Errorf("note %q", note)
	}
	if data, _ := os.ReadFile(log); !strings.HasSuffix(string(data), "later\n") {
		t.Error("a step after a failing one did not run")
	}
	if warm("c3", failing) == "" {
		t.Error("a failed warm-up was stamped as done")
	}
	if _, err := os.Stat(filepath.Join(deps, "stamps", "cargo-c1")); err != nil {
		t.Errorf("stamp: %v", err)
	}
	if info, err := os.Stat(deps); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("deps folder: %v %v", info, err)
	}
}

// Two warm-ups never write one dependency cache together, and waiting ends with the run's context.
func TestDepsLockIsExclusiveAndCancellable(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	unlock, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := lockFile(ctx, path); err == nil {
		t.Fatal("a second holder got the lock")
	}
	unlock()
	again, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}

// Grading's check files and the test-runner pattern know each tool's runners and configuration.
func TestGradingKnowsMavenGradleAndCargo(t *testing.T) {
	files := fakeSource{".mvn/wrapper/maven-wrapper.properties", "Cargo.lock", "Cargo.toml", "build.gradle.kts", "gradle.properties", "gradlew", "mvnw",
		"pom.xml", "settings.gradle.kts"}
	slices.Sort(files)
	for verify, want := range map[string][]string{
		"./mvnw -q test":      {".mvn/wrapper/maven-wrapper.properties", "pom.xml"},
		"mvn -q test":         {".mvn/wrapper/maven-wrapper.properties", "pom.xml"},
		"./gradlew test":      {"build.gradle.kts", "gradle.properties", "settings.gradle.kts"},
		"cargo nextest run":   {"Cargo.lock", "Cargo.toml"},
		"./mvnw verify":       {".mvn/wrapper/maven-wrapper.properties", "pom.xml"},
		"gradle :app:test":    {"build.gradle.kts", "gradle.properties", "settings.gradle.kts"},
		"cargo test --lib":    {"Cargo.lock", "Cargo.toml"},
		"go test ./...":       nil,
		"./gradlew --version": {"build.gradle.kts", "gradle.properties", "settings.gradle.kts"},
	} {
		scripts, configs := checkFiles([]string{verify}, files)
		slices.Sort(configs)
		if !slices.Equal(configs, want) {
			t.Errorf("%q: configuration %q, want %q", verify, configs, want)
		}
		wrapper := strings.TrimPrefix(strings.Fields(verify)[0], "./")
		if (wrapper == "mvnw" || wrapper == "gradlew") && !slices.Contains(scripts, wrapper) {
			t.Errorf("%q: the wrapper script %s is not a check script: %q", verify, wrapper, scripts)
		}
	}
	for _, c := range []string{"./mvnw -q test", "mvn verify", "./gradlew :app:test", "cargo nextest run", "cargo test"} {
		if !ranTests([]string{c}) {
			t.Errorf("%q is not seen as running tests", c)
		}
	}
}
