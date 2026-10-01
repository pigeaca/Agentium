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
	"github.com/pigeaca/agentium/internal/gitx"
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
		note, err := env.warmTools(ctx, checkout, deps, base, buildtool.Select([]string{"cargo"}), []string{"cargo"}, steps, filepath.Join(dir, "setup.log"), func(int) {})
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

// bareWith commits the files in a repository and fetches that commit into a bare one, as Agentium keeps tasks' bases.
func bareWith(t *testing.T, files map[string]string) (bare, commit string) {
	t.Helper()
	ctx := context.Background()
	user := goldenRepo(t, files)
	out, err := gitx.Run(ctx, "-C", user, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	bare = filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := gitx.FetchCommit(ctx, user, out, gitx.SourceRef(out), "--git-dir", bare); err != nil {
		t.Fatal(err)
	}
	return bare, out
}

// A run's tools come from the task's base commit: an arm's snapshot that adds a build file changes nothing about the
// tools, the sandbox or the warm-up (the run asks about the base, not the overlaid checkout).
func TestToolsComeFromTheBaseCommit(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, map[string]string{"pom.xml": "<project/>", "src/Main.java": "x"})
	user := goldenRepo(t, map[string]string{"pom.xml": "<project/>", "settings.gradle": "", "src/Main.java": "x"})
	snap, _ := gitx.Run(ctx, "-C", user, "rev-parse", "HEAD")
	if err := gitx.FetchCommit(ctx, user, snap, gitx.SourceRef(snap), "--git-dir", bare); err != nil {
		t.Fatal(err)
	}
	if got, err := toolsAtBase(ctx, bare, base); err != nil || !slices.Equal(got, []string{"maven"}) {
		t.Errorf("tools at the base: %v, %v", got, err)
	}
	if got, err := toolsAtBase(ctx, bare, snap); err != nil || !slices.Equal(got, []string{"maven", "gradle"}) {
		t.Errorf("tools at the snapshot's commit: %v, %v (the check must tell them apart)", got, err)
	}
	if needed, err := NeedsLocalBinding(ctx, bare, []string{base}); err != nil || needed {
		t.Errorf("a Maven base needs local binding: %v, %v", needed, err)
	}
	if needed, err := NeedsLocalBinding(ctx, bare, []string{base, snap}); err != nil || !needed {
		t.Errorf("a Gradle commit among the bases: %v, %v", needed, err)
	}
	if _, err := toolsAtBase(ctx, bare, "0000000000000000000000000000000000000000"); err == nil {
		t.Error("an unknown commit is not an error")
	}
}

// The warm-up runs in a throwaway checkout of the base, in the data folder's cache: the run's own checkout never sees
// what it builds, and the throwaway is gone afterwards.
func TestWarmRunsInAThrowawayCheckoutOfTheBase(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, map[string]string{"pom.xml": "<project/>"})
	data := t.TempDir()
	seen := filepath.Join(data, "seen")
	fake := buildtool.Profile{Name: "fake", Warm: func(dir, deps string, _ func(string) bool) []buildtool.WarmStep {
		return []buildtool.WarmStep{{Command: `pwd > ` + seen + `; ls >> ` + seen + `; touch target-from-warmup`}}
	}}
	env := Env{Layout: home.Layout{Cache: filepath.Join(data, "cache")}, Bare: bare, VerifyTimeout: 20 * time.Second}
	if note, err := env.warmInThrowaway(ctx, []buildtool.Profile{fake}, filepath.Join(data, "deps"), base, filepath.Join(data, "setup.log"), func(int) {}); err != nil || note != "" {
		t.Fatalf("%q, %v", note, err)
	}
	got, _ := os.ReadFile(seen)
	lines := strings.Fields(string(got))
	if len(lines) < 2 || !strings.Contains(lines[0], "/cache/warm/") {
		t.Fatalf("the warm-up ran in %q", got)
	}
	if !slices.Contains(lines, "pom.xml") {
		t.Errorf("the base commit was not checked out: %q", got)
	}
	if left, _ := os.ReadDir(filepath.Join(data, "cache", "warm")); len(left) != 0 {
		t.Errorf("the throwaway checkout is left: %v", left)
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
