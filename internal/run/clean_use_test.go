package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
)

// stale backdates paths three days and returns a check that each was marked used since.
func stale(t *testing.T, paths ...string) func(when string) {
	t.Helper()
	old := time.Now().Add(-72 * time.Hour)
	for _, p := range paths {
		must(t, os.Chtimes(p, old, old))
	}
	return func(when string) {
		t.Helper()
		for _, p := range paths {
			if age := time.Since(modTime(p)); age > time.Hour {
				t.Errorf("%s: %s is not marked used (%s old)", when, p, age.Round(time.Minute))
			}
		}
	}
}

// A run's setup marks the base's stamp and the deps folder used; so does a validation, at its start and at each of
// its commands (CheckoutCommands' Env), so a long validation keeps them fresh past cleanup's grace.
func TestRunsAndValidationsMarkTheirDependenciesUsed(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, map[string]string{"Cargo.toml": "[package]\nname = \"x\"\nversion = \"0.1.0\"\n"})
	data := t.TempDir()
	layout := home.Layout{Root: data, Cache: filepath.Join(data, "cache"), Deps: filepath.Join(data, "deps")}
	env := Env{Layout: layout, Bare: bare, VerifyTimeout: 10 * time.Second}
	deps := env.depsFolder()
	names := buildtool.NeedsWarming(buildtool.Select([]string{"cargo"}))
	if len(names) == 0 {
		t.Fatal("cargo warms nothing: pick another tool for this test")
	}
	must(t, os.MkdirAll(deps, 0o700))
	stamp := env.stampPath(deps, base, names)
	must(t, os.MkdirAll(filepath.Dir(stamp), 0o700))
	must(t, os.WriteFile(stamp, nil, 0o600)) // warmed already

	check := stale(t, stamp, deps)
	inv := claude.Invocation{Deps: deps, BuildCache: filepath.Join(data, "run-cache")}
	if _, _, err := env.prepareTools(ctx, buildtool.Select([]string{"cargo"}), inv, base, filepath.Join(data, "setup.log"), func(int) {}); err != nil {
		t.Fatal(err)
	}
	check("a run's setup")

	check = stale(t, stamp, deps)
	cc, err := CheckoutCommands(ctx, CommandsEnv{Layout: layout, Bare: bare, Timeout: 10 * time.Second}, base, []string{"cargo test"}, filepath.Join(data, "warm.log"))
	if err != nil {
		t.Fatal(err)
	}
	check("a validation's start")
	check = stale(t, stamp, deps)
	cc.Env(filepath.Join(data, "checkout"))
	check("a validation's command")

	// A run whose base has no stamp (its warm-up failed or waited out) still marks the deps folder.
	must(t, os.Remove(stamp))
	check = stale(t, deps)
	state := env.warmState(deps)
	hold, err := lockFile(ctx, filepath.Join(state, "lock"), nil) // another warm-up
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	env.WarmWait = 100 * time.Millisecond
	if _, _, err := env.prepareTools(ctx, buildtool.Select([]string{"cargo"}), inv, base, filepath.Join(data, "setup.log"), func(int) {}); err == nil {
		t.Fatal("a waited-out warm-up is no error")
	}
	check("a run whose warm-up waited out")
}

// A seed is handed out under a shared lock: while cleanup holds the lock exclusively (between its recheck and its
// rename), prepareSeed waits; once the seed is gone, it makes a new one rather than return one being removed.
func TestPrepareSeedWaitsForCleanupsLock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seed := filepath.Join(dir, "1", "go-99999999-"+baseA)
	fill(t, seed, 10)
	unlock, err := home.LockFile(ctx, seed+".lock", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- prepareSeed(ctx, nil, "", seed, nil) }()
	select {
	case err := <-done:
		t.Fatalf("prepareSeed returned while cleanup held the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	must(t, os.Rename(seed, filepath.Join(dir, "moved-away"))) // cleanup's move into the quarantine
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(seed, "data")); err == nil {
		t.Error("prepareSeed returned the removed seed")
	}
	if !realFolder(seed) {
		t.Error("prepareSeed made no new seed")
	}
}
