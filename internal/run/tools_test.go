package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
)

func TestDepsFolderIsPerProjectOutsideTheDeniedFolders(t *testing.T) {
	env := Env{Layout: home.Layout{Root: "/data", Deps: "/data/deps", Cache: "/data/cache"}, Bare: "/data/projects/7/repo.git"}
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
	if (Env{Layout: home.Layout{Deps: "/data/deps"}}).depsFolder() != "" {
		t.Error("a deps folder without a cache folder for the warm-up state")
	}
	if got := (Env{Layout: home.Layout{Deps: "/data/deps", Cache: "/data/cache"}}).depsFolder(); got != "/data/deps/default" {
		t.Errorf("without a bare repository: %s", got)
	}
}

// Setup warms the dependencies once per base commit and tool set, with the step's own variables added to Agentium's
// command environment; a failing step is a note, not a failed run, and leaves no stamp so the next run tries again.
func TestWarmToolsStampsAndNotes(t *testing.T) {
	dir := t.TempDir()
	deps, log := filepath.Join(dir, "deps"), filepath.Join(dir, "calls")
	checkout := t.TempDir()
	env := Env{CommandEnv: []string{"BASE=1"}, VerifyTimeout: 20 * time.Second, Layout: home.Layout{Cache: filepath.Join(dir, "cache")}}
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
	// Lock and stamps live where agents cannot read (the data folder's cache), not in the deps folder they read.
	if _, err := os.Stat(env.stampPath(deps, "c1", []string{"cargo"})); err != nil || !strings.HasPrefix(env.stampPath(deps, "c1", []string{"cargo"}), filepath.Join(dir, "cache")) {
		t.Errorf("stamp: %v", err)
	}
	if entries, _ := os.ReadDir(deps); len(entries) != 0 {
		t.Errorf("warm-up state in the deps folder agents read: %v", entries)
	}
	if info, err := os.Stat(deps); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("deps folder: %v %v", info, err)
	}
}

// A stamp from before warm-up versions (named tools-base) never matches, so a changed recipe warms existing bases again.
func TestWarmStampsCarryTheRecipeVersion(t *testing.T) {
	dir := t.TempDir()
	deps, log := filepath.Join(dir, "deps"), filepath.Join(dir, "calls")
	env := Env{VerifyTimeout: 20 * time.Second, Layout: home.Layout{Cache: filepath.Join(dir, "cache")}}
	steps := []buildtool.WarmStep{{Command: "echo ran >> " + log}}
	tools := []string{"gradle"}
	state := env.warmState(deps)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "gradle-c1"), nil, 0o600); err != nil { // the old format
		t.Fatal(err)
	}
	if _, err := os.Stat(env.stampPath(deps, "c1", tools)); err == nil {
		t.Fatal("an old-format stamp counts")
	}
	for range 2 {
		if _, err := env.warmTools(context.Background(), t.TempDir(), deps, "c1", buildtool.Select(tools), tools, steps, filepath.Join(dir, "setup.log"), func(int) {}); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(log); string(data) != "ran\n" {
		t.Errorf("calls %q: one warm-up despite the old stamp, none after the new one", data)
	}
	if want := "gradle-" + buildtool.WarmVersion(buildtool.Select(tools)) + "-c1"; filepath.Base(env.stampPath(deps, "c1", tools)) != want {
		t.Errorf("stamp %s, want %s", env.stampPath(deps, "c1", tools), want)
	}
}

// Configurations a step could not warm are a note; a network-looking reason leaves no stamp so the next run retries.
func TestWarmSkippedConfigurationsAreANote(t *testing.T) {
	for _, c := range []struct {
		reason  string
		stamped bool
	}{{"Could not resolve all files for configuration", true}, {"Could not GET 'https://repo.example/x.pom' | Connection reset", false}} {
		dir := t.TempDir()
		deps, checkout := filepath.Join(dir, "deps"), t.TempDir()
		env := Env{VerifyTimeout: 20 * time.Second, Layout: home.Layout{Cache: filepath.Join(dir, "cache")}}
		list := ":demoTestsRuntimeClasspath\t" + c.reason + "\n:other\t" + c.reason + "\n"
		if err := os.WriteFile(filepath.Join(checkout, buildtool.WarmSkippedFile), []byte(list), 0o600); err != nil { // as the step would
			t.Fatal(err)
		}
		steps := []buildtool.WarmStep{{Command: "true"}}
		note, err := env.warmTools(context.Background(), checkout, deps, "c1", buildtool.Select([]string{"gradle"}), []string{"gradle"}, steps, filepath.Join(dir, "setup.log"), func(int) {})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(note, "2 configuration(s) not warmed: :demoTestsRuntimeClasspath, :other") {
			t.Errorf("note %q", note)
		}
		if _, err := os.Stat(env.stampPath(deps, "c1", []string{"gradle"})); (err == nil) != c.stamped {
			t.Errorf("reason %q: stamped %v, want %v", c.reason, err == nil, c.stamped)
		}
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
	unlock, err := lockFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := lockFile(ctx, path, nil); err == nil {
		t.Fatal("a second holder got the lock")
	}
	unlock()
	again, err := lockFile(context.Background(), path, nil)
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
		scripts, configs := checkFiles([]string{verify}, "", files)
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

// A warm-up that waits for another one gives up after the bound with errWarmWait (the run ends as an infrastructure failure) and does not touch the stamp; one
// already stamped skips even the checkout (the bare repository here does not exist, so a checkout would fail).
func TestWarmWaitIsBoundedAndStampedSkipsTheCheckout(t *testing.T) {
	dir := t.TempDir()
	deps := filepath.Join(dir, "deps", "1")
	env := Env{Layout: home.Layout{Cache: filepath.Join(dir, "cache")}, Bare: filepath.Join(dir, "missing.git"), WarmWait: 300 * time.Millisecond, VerifyTimeout: 10 * time.Second}
	state := env.warmState(deps)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(state, filepath.Dir(deps)) {
		t.Fatalf("the warm-up state %s lies in the folder agents read", state)
	}
	hold, err := lockFile(context.Background(), filepath.Join(state, "lock"), nil) // another warm-up
	if err != nil {
		t.Fatal(err)
	}
	steps := []buildtool.WarmStep{{Command: "touch " + filepath.Join(dir, "ran")}}
	start := time.Now()
	note, err := env.warmTools(context.Background(), dir, deps, "c1", buildtool.Select([]string{"cargo"}), []string{"cargo"}, steps, filepath.Join(dir, "log"), func(int) {})
	if !errors.Is(err, errWarmWait) || note != "" || time.Since(start) > 10*time.Second {
		t.Errorf("a bounded wait: %q, %v, after %v", note, err, time.Since(start))
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("the warm-up ran without the lock")
	}
	hold()
	// A cancelled run is an error, not a note.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hold, _ = lockFile(context.Background(), filepath.Join(state, "lock"), nil)
	if _, err := env.warmTools(ctx, dir, deps, "c1", buildtool.Select([]string{"cargo"}), []string{"cargo"}, steps, filepath.Join(dir, "log"), func(int) {}); err == nil {
		t.Error("a cancelled wait is not an error")
	}
	hold()
	if err := os.WriteFile(env.stampPath(deps, "c1", []string{"cargo"}), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if note, err := env.warmInThrowaway(context.Background(), buildtool.Select([]string{"cargo"}), deps, "c1", filepath.Join(dir, "log"), func(int) {}); err != nil || note != "" {
		t.Errorf("a stamped warm-up: %q, %v", note, err)
	}
}

// A run whose warm-up wait timed out clones nothing and is not counted against its arm: prepareTools returns errWarmWait
// before the Gradle home gets the wrapper distribution (Once then ends the run as an infrastructure failure).
func TestWaitedOutWarmUpClonesNothing(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, map[string]string{"build.gradle": ""})
	data := t.TempDir()
	deps, run := filepath.Join(data, "deps", "1"), filepath.Join(data, "ws", "go-build")
	if err := os.MkdirAll(filepath.Join(deps, "gradle", "wrapper", "dists", "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := Env{Layout: home.Layout{Cache: filepath.Join(data, "cache")}, Bare: bare, WarmWait: 200 * time.Millisecond, VerifyTimeout: 10 * time.Second}
	if err := os.MkdirAll(env.warmState(deps), 0o700); err != nil {
		t.Fatal(err)
	}
	hold, err := lockFile(ctx, filepath.Join(env.warmState(deps), "lock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	inv := claude.Invocation{Deps: deps, BuildCache: run}
	_, _, err = env.prepareTools(ctx, buildtool.Select([]string{"gradle"}), inv, base, filepath.Join(data, "log"), func(int) {})
	if !errors.Is(err, errWarmWait) {
		t.Fatalf("prepareTools: %v", err)
	}
	if _, err := os.Stat(filepath.Join(run, "gradle")); err == nil {
		t.Error("the run's Gradle home was made, wrapper clone included, for a run that cannot be fair")
	}
}

// A warm-up that timed out waiting finds the other one finished meanwhile and warmed this base: it goes on, and a base
// not warmed is still the error.
func TestWarmWaitRechecksTheStamp(t *testing.T) {
	dir := t.TempDir()
	deps := filepath.Join(dir, "deps", "1")
	env := Env{Layout: home.Layout{Cache: filepath.Join(dir, "cache")}, WarmWait: 150 * time.Millisecond, VerifyTimeout: 10 * time.Second}
	if err := os.MkdirAll(env.warmState(deps), 0o700); err != nil {
		t.Fatal(err)
	}
	hold, err := lockFile(context.Background(), filepath.Join(env.warmState(deps), "lock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	call := func() error {
		_, err := env.warmTools(context.Background(), dir, deps, "c1", buildtool.Select([]string{"cargo"}), []string{"cargo"}, nil, filepath.Join(dir, "log"), func(int) {})
		return err
	}
	if err := call(); !errors.Is(err, errWarmWait) {
		t.Fatalf("not warmed: %v", err)
	}
	if err := os.WriteFile(env.stampPath(deps, "c1", []string{"cargo"}), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := call(); err != nil {
		t.Errorf("warmed meanwhile: %v", err)
	}
}

// Every run refuses to start on a build configuration it cannot read safely, whatever the project's language: the
// denials it feeds apply to all projects.
func TestDeniedPathsFailClosedOnAnUnreadableBuildConfig(t *testing.T) {
	userHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(userHome, ".cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, ".cargo", "config.toml"), make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	env := Env{Layout: home.Layout{Root: "/data", Workspaces: "/data/workspaces"}, ProjectRoot: t.TempDir(), Home: userHome}
	if _, err := env.denied(context.Background(), "/data/workspaces/r1"); err == nil || !strings.Contains(err.Error(), "regular file under 1 MiB") {
		t.Errorf("an oversized config: %v", err)
	}
	if err := env.CheckBuildConfigs(context.Background()); err == nil {
		t.Error("an experiment's pre-lock check accepts it")
	}
}

// A Python base with a Go module in a subfolder: Go is not among its tools (no `go test ./...` for mined tasks), but
// its agent keeps Go's side, the run's GOCACHE above all, without which go falls back to the user's denied cache and
// fails in the sandbox. A Python base without Go gets none of it.
func TestNestedGoModuleKeepsGoForTheAgent(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		files map[string]string
		goOn  bool
	}{
		{"a Go module in a subfolder", map[string]string{"pyproject.toml": "[project]\nname = \"p\"\n", "tools/foo/go.mod": "module foo\n", "tools/foo/main.go": "package main\n"}, true},
		{"no Go", map[string]string{"pyproject.toml": "[project]\nname = \"p\"\n", "p/__init__.py": "\n"}, false},
	} {
		bare, base := bareWith(t, c.files)
		l, err := baseLayout(ctx, bare, base)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(l.tools, []string{"python"}) || slices.Contains(l.agentTools, "go") != c.goOn {
			t.Errorf("%s: tools %q, agent tools %q", c.name, l.tools, l.agentTools)
		}
		inv := claude.Invocation{CLI: "/c", Dir: "/w/repo", Prompt: "p", Model: "m", Home: "/home/u", SignIn: claude.SignInLogin,
			TempRoot: "/t/ag-1", UID: 1, Tools: l.tools, AgentTools: l.agentTools, BuildCache: "/w/go-build"}
		_, env, err := inv.Command([]string{"PATH=/bin", "HOME=/home/u", "GOCACHE=/home/u/gocache"})
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(env, "GOCACHE=/w/go-build"); got != c.goOn {
			t.Errorf("%s: the run's GOCACHE %v, want %v: %q", c.name, got, c.goOn, env)
		}
		if slices.Contains(env, "GOCACHE=/home/u/gocache") {
			t.Errorf("%s: the user's GOCACHE reached the agent", c.name)
		}
	}
}
