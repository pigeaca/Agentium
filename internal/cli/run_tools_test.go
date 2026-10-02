package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repository's build tool shapes its runs end to end, with fakes for the tool and the agent (nothing is fetched): setup
// warms the dependencies into the project's deps folder once, the agent's environment and sandbox follow the tool's
// recipe, and grading keeps its caches in the data folder.
func TestRunsFollowTheRepositorysBuildTool(t *testing.T) {
	for _, c := range []struct {
		tool, marker string
		// agent are the variables the agent must see, with the run's workspace as <ws> and the project's deps folder as <deps>.
		agent []string
		// warm are the fake tool's calls: its variable and where it must point, <deps> and <cache> as above.
		warmVar, warmWant, verifyWant string
		calls                         int
		binding                       bool
		// extra are more files the base commit holds; absent are variable names the agent must not get.
		extra, absent []string
		// setupCache is where the task's setup builds Go ("" not checked): the agent's own cache when Go's agent side is
		// on, so setup warms its builds, else the data folder's cache.
		setupCache string
	}{
		{tool: "gradle", marker: "build.gradle", agent: []string{"GRADLE_USER_HOME=<ws>/go-build/gradle", "GRADLE_RO_DEP_CACHE=<deps>/gradle-ro"},
			warmVar: "GRADLE_USER_HOME", warmWant: "<deps>/gradle", verifyWant: "<cache>/gradle", calls: 3, binding: true},
		{tool: "mvn", marker: "pom.xml", agent: []string{"MAVEN_USER_HOME=<deps>/mvnw-home",
			"MAVEN_ARGS=-o -Dmaven.repo.local=<ws>/go-build/m2 -Dmaven.repo.local.tail=<deps>/m2 -Dmaven.build.cache.enabled=false"},
			warmVar: "MAVEN_ARGS", warmWant: "-Dmaven.repo.local=<deps>/m2", verifyWant: "-Dmaven.repo.local=<cache>/m2", calls: 1},
		// Without a Go file in the base, the agent gets none of Go's variables.
		{tool: "cargo", marker: "Cargo.toml", agent: []string{"CARGO_HOME=<deps>/cargo", "CARGO_NET_OFFLINE=true"},
			warmVar: "CARGO_HOME", warmWant: "<deps>/cargo", verifyWant: "", calls: 1, absent: []string{"GOCACHE", "GOFLAGS"},
			setupCache: "<cache>/go-build"},
		// A Go module in a subfolder (not a detected tool) keeps them: the run's own GOCACHE above all, without which go
		// would fall back to the user's cache, which the sandbox denies.
		{tool: "cargo", marker: "Cargo.toml", extra: []string{"tools/foo/go.mod"},
			agent:   []string{"CARGO_HOME=<deps>/cargo", "GOCACHE=<ws>/go-build", "GOFLAGS=-buildvcs=false"},
			warmVar: "CARGO_HOME", warmWant: "<deps>/cargo", verifyWant: "", calls: 1, setupCache: "<ws>/go-build"},
	} {
		t.Run(strings.Join(append([]string{c.tool}, c.extra...), "+"), func(t *testing.T) { // not parallel: PATH is the test process's own
			f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
			bin, out := t.TempDir(), t.TempDir()
			fake := "#!/bin/sh\necho \"$" + c.warmVar + " $*\" >> " + filepath.Join(out, "tool") + "\nmkdir -p \"$GRADLE_USER_HOME/wrapper/dists/d\" 2>/dev/null && touch \"$GRADLE_USER_HOME/wrapper/dists/d/ok\"\nexit 0\n"
			if c.tool != "gradle" {
				fake = "#!/bin/sh\necho \"$" + c.warmVar + " $*\" >> " + filepath.Join(out, "tool") + "\nexit 0\n"
			}
			if err := os.WriteFile(filepath.Join(bin, c.tool), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			writeFile(t, f.repo, c.marker, "\n")
			for _, extra := range c.extra {
				writeFile(t, f.repo, extra, "module foo\n")
			}
			gitIn(t, f.repo, "add", "-A")
			gitIn(t, f.repo, "commit", "-q", "-m", "build file")
			verify := `printf '%s' "$GRADLE_USER_HOME $MAVEN_ARGS" > ` + filepath.Join(out, "verify") + `; sh run_tests.sh`
			setup := `printf '%s' "$GOCACHE" > ` + filepath.Join(out, "setup")
			expect(t, f.run(context.Background(), "task", "add", "tool", "--base", "HEAD", "--instruction", "Anything.", "--verify", verify,
				"--setup", setup), ExitOK)
			f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, `printf '%s\n' "$@" > `+filepath.Join(out, "args")+`
env > `+filepath.Join(out, "env")+`
cat "$GRADLE_USER_HOME/gradle.properties" > `+filepath.Join(out, "props")+` 2>/dev/null
ls "$GRADLE_USER_HOME/wrapper/dists" > `+filepath.Join(out, "dists")+` 2>/dev/null
true`, false)
			if c.binding {
				// A Gradle project's agent runs need the user's opt-in for local binding: nothing starts without it, and init
				// keeps the choice until it is changed.
				expect(t, f.run(context.Background(), "init"), ExitOK, "agentium init --allow-local-binding")
				expect(t, f.run(context.Background(), "run", "once", "tool"), 1, "agentium init --allow-local-binding", "localhost")
				if _, err := os.Stat(filepath.Join(out, "tool")); err == nil {
					t.Error("the build tool ran for a run that was refused")
				}
				expect(t, f.run(context.Background(), "init", "--allow-local-binding", "--no-allow-local-binding"), ExitUsage,
					"--no-allow-local-binding was removed: use --allow-local-binding=false")
				expect(t, f.run(context.Background(), "init", "--allow-local-binding"), ExitOK, "agents may bind local ports")
				expect(t, f.run(context.Background(), "init"), ExitOK, "agents may bind local ports")
			} else {
				if r := f.run(context.Background(), "init"); strings.Contains(r.stdout, "local ports") {
					t.Errorf("init mentions local ports for a project without Gradle:\n%s", r.stdout)
				}
			}
			expect(t, f.run(context.Background(), "run", "once", "tool", "--keep"), ExitOK, "outcome      ok; verification passed")

			workspaces, _ := os.ReadDir(filepath.Join(f.data, "workspaces"))
			deps, _ := os.ReadDir(filepath.Join(f.data, "deps"))
			if len(workspaces) != 1 || len(deps) != 1 {
				t.Fatalf("workspaces %v, deps %v", workspaces, deps)
			}
			fill := func(s string) string {
				return strings.NewReplacer("<ws>", filepath.Join(f.data, "workspaces", workspaces[0].Name()),
					"<deps>", filepath.Join(f.data, "deps", deps[0].Name()), "<cache>", filepath.Join(f.data, "cache")).Replace(s)
			}
			calls := strings.Split(strings.TrimSpace(readString(t, filepath.Join(out, "tool"))), "\n")
			if len(calls) != c.calls {
				t.Fatalf("the tool was called %d times: %q", len(calls), calls)
			}
			for _, call := range calls {
				if !strings.HasPrefix(call, fill(c.warmWant)) {
					t.Errorf("warm-up call %q, want its %s to be %s", call, c.warmVar, fill(c.warmWant))
				}
			}
			agentEnv := "\n" + readString(t, filepath.Join(out, "env"))
			for _, want := range c.agent {
				if !strings.Contains(agentEnv, "\n"+fill(want)+"\n") {
					t.Errorf("the agent's environment lacks %s", fill(want))
				}
			}
			if got := readString(t, filepath.Join(out, "setup")); c.setupCache != "" && got != fill(c.setupCache) {
				t.Errorf("setup built Go into %q, want %s", got, fill(c.setupCache))
			}
			for _, name := range c.absent {
				if strings.Contains(agentEnv, "\n"+name+"=") {
					t.Errorf("the agent's environment has %s", name)
				}
			}
			if c.tool == "gradle" {
				if got := readString(t, filepath.Join(out, "props")); !strings.HasPrefix(got, "org.gradle.daemon=false\n") {
					t.Errorf("the run's gradle.properties = %q", got)
				}
				if got := strings.TrimSpace(readString(t, filepath.Join(out, "dists"))); got != "d" {
					t.Errorf("the wrapper distribution was not cloned into the run's Gradle home: %q", got)
				}
			}
			if got := strings.Contains(readString(t, filepath.Join(out, "args")), `"allowLocalBinding":true`); got != c.binding {
				t.Errorf("sandbox local binding = %v, want %v", got, c.binding)
			}
			if got := strings.TrimSpace(readString(t, filepath.Join(out, "verify"))); c.verifyWant != "" && !strings.Contains(got, fill(c.verifyWant)) {
				t.Errorf("grading ran with %q, want %s", got, fill(c.verifyWant))
			}
			// A second run of the same base finds the dependencies already warmed.
			expect(t, f.run(context.Background(), "run", "once", "tool"), ExitOK, "outcome      ok; verification passed")
			if again := strings.Split(strings.TrimSpace(readString(t, filepath.Join(out, "tool"))), "\n"); len(again) != c.calls {
				t.Errorf("the second run warmed again: %q", again)
			}
			if c.binding {
				expect(t, f.run(context.Background(), "init", "--allow-local-binding=false"), ExitOK, "refuse to start")
				expect(t, f.run(context.Background(), "run", "once", "tool"), 1, "agentium init --allow-local-binding")
			}
		})
	}
}

// Validation's warm-up of the base's build tools runs within the call's verify timeout: the project's setting, or
// task validate's own --timeout, which wins for that call. A fake cargo whose fetch takes 2 seconds is stopped under a
// 500ms setting and finishes under a 1m override.
func TestValidationWarmUpFollowsTheTimeout(t *testing.T) { // not parallel: PATH is the test process's own
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	bin, out := t.TempDir(), t.TempDir()
	done := filepath.Join(out, "warmed")
	if err := os.WriteFile(filepath.Join(bin, "cargo"), []byte("#!/bin/sh\n[ \"$1\" = fetch ] || exit 0\nsleep 2\ntouch "+done+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeFile(t, f.repo, "Cargo.toml", "\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "build file")
	ctx := context.Background()
	expect(t, f.run(ctx, "task", "add", "tool", "--base", "HEAD", "--instruction", "Anything.", "--verify", "true"), ExitOK)
	expect(t, f.run(ctx, "init", "--verify-timeout", "500ms"), ExitOK, "verify timeout  500ms")
	expect(t, f.run(ctx, "task", "validate", "tool"), ExitOK, "dependency warm-up failed (cargo fetch)")
	if _, err := os.Stat(done); err == nil {
		t.Fatal("the warm-up finished under a 500ms verify timeout")
	}
	if r := f.run(ctx, "task", "validate", "tool", "--timeout", "1m"); r.code != ExitOK || strings.Contains(r.stdout, "warm-up failed") {
		t.Errorf("with --timeout 1m: %+v", r)
	}
	if _, err := os.Stat(done); err != nil {
		t.Error("the warm-up did not finish under task validate --timeout 1m")
	}
}
