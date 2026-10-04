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

var monorepoFiles = map[string]string{
	"README.md":                   "a monorepo",
	"svc/go.mod":                  "module example.com/svc\n",
	"svc/main.go":                 "package main\n",
	"py/pyproject.toml":           "[project]\nname = \"py\"\n",
	"py/src/pkg/__init__.py":      "",
	"mvn/pom.xml":                 "<project/>",
	"mvn/src/main/java/Main.java": "x",
	"gr/settings.gradle":          "",
}

// A module's tools, import root and local-binding need come from its own folder of the base commit; the root of a
// monorepo has none of them, and a project with no module reads the root as ever.
func TestModuleLayoutComesFromTheModulesFolder(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, monorepoFiles)
	for _, c := range []struct {
		module     string
		tools      []string
		importRoot string
	}{{"svc", []string{"go"}, "svc"}, {"py", []string{"python"}, "py/src"}, {"mvn", []string{"maven"}, "mvn"}, {"gr", []string{"gradle"}, "gr"}} {
		l, err := baseLayoutIn(ctx, bare, base, c.module)
		if err != nil || !slices.Equal(l.tools, c.tools) || l.importRoot != c.importRoot {
			t.Errorf("module %s: tools %v, import root %q, %v; want %v, %q", c.module, l.tools, l.importRoot, err, c.tools, c.importRoot)
		}
	}
	if l, err := baseLayoutIn(ctx, bare, base, ""); err != nil || len(l.tools) != 0 || l.importRoot != "" {
		t.Errorf("the root of a monorepo: %+v, %v", l, err)
	}
	if l, err := baseLayout(ctx, bare, base); err != nil || len(l.tools) != 0 {
		t.Errorf("baseLayout: %+v, %v", l, err)
	}
	for module, want := range map[string]bool{"": false, "svc": false, "mvn": false, "gr": true} {
		if needed, err := NeedsLocalBindingIn(ctx, bare, module, []string{base}); err != nil || needed != want {
			t.Errorf("local binding for module %q = %v, %v; want %v", module, needed, err, want)
		}
	}
}

// Warm-up stamps, grading seeds and (through the stamp) the deps' venvs are keyed by module: two modules of one
// repository and base never share one, and the root's keys are what they were before modules.
func TestStampsAndSeedsAreKeyedByModule(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Cache: filepath.Join(data, "cache"), Deps: filepath.Join(data, "deps")}
	bare := filepath.Join(data, "projects", "7", "repo.git")
	root := Env{Layout: layout, Bare: bare}
	a, b := root, root
	a.Module, b.Module = "svc/api", "svc/web"
	deps := root.depsFolder()
	names := []string{"python"}
	stamps := map[string]string{}
	for _, env := range []Env{root, a, b} {
		stamps[env.stampPath(deps, "c1", names)] = env.Module
	}
	if len(stamps) != 3 {
		t.Errorf("modules share a stamp: %v", stamps)
	}
	if want := "python-" + buildtool.WarmVersion(buildtool.Select(names)) + "-c1"; filepath.Base(root.stampPath(deps, "c1", names)) != want {
		t.Errorf("the root's stamp is %s, want %s", filepath.Base(root.stampPath(deps, "c1", names)), want)
	}
	if !strings.Contains(filepath.Base(a.stampPath(deps, "c1", names)), buildtool.ModuleKey("svc/api")) {
		t.Errorf("a module's stamp does not name it: %s", a.stampPath(deps, "c1", names))
	}
	if root.depsFolder() != a.depsFolder() { // dependencies stay per project; the keys inside them differ
		t.Errorf("the deps folder changed: %s, %s", root.depsFolder(), a.depsFolder())
	}

	profiles := buildtool.Select([]string{"go"})
	seeds := map[string]bool{}
	for _, env := range []Env{root, a, b} {
		seed, err := env.gradingSeed(profiles, commitA)
		if err != nil || seeds[seed] || !commitSuffix.MatchString(seed) { // cleanup finds a seed by its commit suffix
			t.Fatalf("seed %s, %v (shared or not a seed)", seed, err)
		}
		seeds[seed] = true
	}
	if seed, _ := root.gradingSeed(profiles, commitA); filepath.Base(seed) != buildtool.SeedKey(profiles)+"-"+commitA {
		t.Errorf("the root's seed changed: %s", seed)
	}
}

// The warm-up runs in the module's folder of its throwaway checkout of the base, and the commands of a run or a
// validation run in the module's folder of theirs.
func TestWarmAndCommandsRunInTheModule(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, monorepoFiles)
	data := t.TempDir()
	seen := filepath.Join(data, "seen")
	var dirs []string
	fake := buildtool.Profile{Name: "fake", Warm: func(dir, deps string, _ func(string) bool) []buildtool.WarmStep {
		dirs = append(dirs, dir)
		return []buildtool.WarmStep{{Command: `pwd > ` + seen + `; ls >> ` + seen}}
	}}
	env := Env{Layout: home.Layout{Cache: filepath.Join(data, "cache")}, Bare: bare, VerifyTimeout: 20 * time.Second, Module: "svc"}
	if note, err := env.warmInThrowaway(ctx, []buildtool.Profile{fake}, filepath.Join(data, "deps"), base, filepath.Join(data, "setup.log"), func(int) {}); err != nil || note != "" {
		t.Fatalf("%q, %v", note, err)
	}
	got, _ := os.ReadFile(seen)
	lines := strings.Fields(string(got))
	if len(lines) == 0 || !strings.HasSuffix(lines[0], "/repo/svc") || !slices.Contains(lines, "go.mod") || slices.Contains(lines, "README.md") {
		t.Errorf("the warm-up ran in %q", got)
	}
	if len(dirs) == 0 || !strings.HasSuffix(dirs[0], "/repo/svc") {
		t.Errorf("the warm steps were planned for %v", dirs)
	}

	checkoutDir := t.TempDir()
	if env.inModule(checkoutDir) != filepath.Join(checkoutDir, "svc") || (Env{}).inModule(checkoutDir) != checkoutDir {
		t.Errorf("inModule: %s, %s", env.inModule(checkoutDir), (Env{}).inModule(checkoutDir))
	}
	if err := os.MkdirAll(filepath.Join(checkoutDir, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	where := filepath.Join(data, "where")
	results, ok, err := env.commands(ctx, env.inModule(checkoutDir), []string{"pwd > " + where}, filepath.Join(data, "c.log"), func(int) {})
	if err != nil || !ok || len(results) != 1 {
		t.Fatalf("%v, %v, %v", results, ok, err)
	}
	if out, _ := os.ReadFile(where); !strings.HasSuffix(strings.TrimSpace(string(out)), "/svc") {
		t.Errorf("the command ran in %q", out)
	}
}
