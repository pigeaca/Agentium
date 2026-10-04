package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
)

// gradeFixture is a data folder with a run's grading copy and a deps folder, and an Env over it.
type gradeFixture struct {
	dir    string
	env    Env
	copy   string // <records>/r1/verify
	root   string // <records>/r1/grading
	deps   string
	seed   string // the seed path for Go's profile and base commitA (not made)
	golang []buildtool.Profile
}

func newGradeFixture(t *testing.T) gradeFixture {
	t.Helper()
	dir := t.TempDir()
	layout, err := home.Resolve(func(key string) string {
		return map[string]string{"AGENTIUM_HOME": filepath.Join(dir, "data")}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	layout.Temp = t.TempDir()
	f := gradeFixture{dir: dir, env: Env{Layout: layout, Bare: filepath.Join(layout.Root, "projects", "7", "repo.git")},
		copy: filepath.Join(layout.Records, "r1", "verify"), root: filepath.Join(layout.Records, "r1", gradingFolder),
		deps: filepath.Join(layout.Deps, "7"), golang: buildtool.Select([]string{"go"})}
	f.seed = f.seedFor(t, f.golang, commitA)
	for _, d := range []string{f.copy, f.deps} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.copy, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// Full commit IDs, as a seed needs.
const (
	commitA = "0123456789abcdef0123456789abcdef01234567"
	commitB = "89abcdef0123456789abcdef0123456789abcdef"
	commitC = "fedcba9876543210fedcba9876543210fedcba98"
)

// seedFor is the seed path of the fixture's project for profiles and base.
func (f gradeFixture) seedFor(t *testing.T, profiles []buildtool.Profile, base string) string {
	t.Helper()
	seed, err := f.env.gradingSeed(profiles, base)
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

// newCopy makes a fresh grading copy beside root (prepareGrading moves its copy into the grade's folder).
func (f gradeFixture) newCopy(root string) string {
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		panic(err)
	}
	copy, err := os.MkdirTemp(filepath.Dir(root), "verify-")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(copy, "main.go"), []byte("package main\n"), 0o600); err != nil {
		panic(err)
	}
	return copy
}

// input is a grade of a fresh copy (newCopy) for the agent tools, cloned from seed ("" for none), in root.
func (f gradeFixture) input(root, seed string, tools ...string) gradingInput {
	return gradingInput{Root: root, Seed: seed, Copy: f.newCopy(root), Environ: []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(f.dir, "home")},
		Agent: agent.Invocation{Tools: tools, Home: filepath.Join(f.dir, "home"), Deps: f.deps}}
}

// tree reads every regular file under root (relative path: content).
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		got[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The seed lives in the data folder's cache, per project, tool set and base commit: a seed warmed on one base never
// serves another base's grades, whose earlier tasks' solutions it may hold.
func TestGradingSeedIsPerProjectToolsAndBase(t *testing.T) {
	f := newGradeFixture(t)
	if !strings.HasPrefix(f.seed, filepath.Join(f.env.Layout.Cache, seedsFolder, "7")+string(filepath.Separator)) || !strings.HasSuffix(f.seed, "-"+commitA) {
		t.Errorf("seed = %s", f.seed)
	}
	other8 := gradeFixture{env: Env{Layout: f.env.Layout, Bare: filepath.Join(f.env.Layout.Root, "projects", "8", "repo.git")}}
	seeds := map[string]bool{f.seed: true}
	for _, other := range []string{f.seedFor(t, f.golang, commitB), f.seedFor(t, buildtool.SelectRun([]string{"gradle"}, nil), commitA),
		other8.seedFor(t, f.golang, commitA), f.seedFor(t, f.golang, strings.Repeat("ab", 32))} {
		if seeds[other] {
			t.Errorf("two seeds share %s", other)
		}
		seeds[other] = true
	}
	if _, err := (Env{}).gradingSeed(f.golang, commitA); err == nil {
		t.Error("a layout without a cache folder names a seed")
	}
	// Only a full, resolved commit ID names a seed: a branch, a short or upper-case ID, or a path never does.
	for _, base := range []string{"", "main", "HEAD", "0123456", commitA[:39], commitA + "0", strings.ToUpper(commitA), "../" + commitA[3:],
		commitA[:20] + "/" + commitA[21:], strings.Repeat("g", 40)} {
		if seed, err := f.env.gradingSeed(f.golang, base); err == nil {
			t.Errorf("base %q names the seed %s", base, seed)
		}
	}
}

// A seed is made once, by the profiles' hooks and the trusted warm step, and published whole: concurrent makers wait
// for one; a dead maker's leftover is removed first; a failed warm step leaves no seed; and a published seed is never
// written again.
func TestPrepareSeed(t *testing.T) {
	f := newGradeFixture(t)
	gradle := buildtool.SelectRun([]string{"gradle"}, nil)
	seed := f.seedFor(t, gradle, commitA)
	// A dead maker's partial seed.
	if err := os.MkdirAll(filepath.Join(seed+".tmp", "half"), 0o700); err != nil {
		t.Fatal(err)
	}
	var warmed atomic.Int32
	warm := func(_ context.Context, dir string) error {
		warmed.Add(1)
		if filepath.Base(dir) != filepath.Base(seed)+".tmp" {
			return fmt.Errorf("warmed %s, not the seed in the making", dir)
		}
		return os.WriteFile(filepath.Join(dir, "warm-entry"), []byte("built from the base"), 0o600)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = prepareSeed(context.Background(), gradle, f.deps, seed, warm) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if warmed.Load() != 1 {
		t.Errorf("warmed %d times, want once", warmed.Load())
	}
	got := tree(t, seed)
	if got["warm-entry"] != "built from the base" || !strings.Contains(got["gradle/gradle.properties"], "org.gradle.daemon=false") ||
		got["gradle/init.d/agentium-offline.gradle"] == "" {
		t.Errorf("seed = %v", got)
	}
	if _, err := os.Lstat(seed + ".tmp"); err == nil {
		t.Error("the leftover was not removed")
	}
	// Published: never made again, never written.
	if err := prepareSeed(context.Background(), gradle, f.deps, seed, func(context.Context, string) error {
		t.Error("a published seed was warmed again")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A failed warm step publishes nothing and leaves nothing.
	other := f.seedFor(t, f.golang, commitB)
	if err := prepareSeed(context.Background(), f.golang, f.deps, other, func(_ context.Context, dir string) error {
		os.WriteFile(filepath.Join(dir, "partial"), nil, 0o600)
		return errors.New("the base did not build")
	}); err == nil {
		t.Error("a failed warm step made a seed")
	}
	for _, p := range []string{other, other + ".tmp"} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was left", p)
		}
	}
	// Something at the seed's place that is not a folder is never taken for a seed.
	link := f.seedFor(t, f.golang, commitC)
	if err := os.Symlink(f.copy, link); err != nil {
		t.Fatal(err)
	}
	if err := prepareSeed(context.Background(), f.golang, f.deps, link, nil); err == nil {
		t.Error("a link was taken for a seed")
	}
	if _, err := prepareGrading(context.Background(), f.input(f.root, link)); err == nil {
		t.Error("a grade cloned a link")
	}
}

// A grade's cache is a clone of the seed (one clonefile call on macOS), its own: what the grade writes there never
// reaches the seed, so the next grade starts from the seed as it was. Its temp root is its own too, and its environment
// names them.
func TestPrepareGradingClonesTheSeed(t *testing.T) {
	f := newGradeFixture(t)
	if err := prepareSeed(context.Background(), f.golang, f.deps, f.seed, func(_ context.Context, dir string) error {
		return os.WriteFile(filepath.Join(dir, "entry"), []byte("trusted"), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	g, err := prepareGrading(context.Background(), f.input(f.root, f.seed, "go"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" && g.Made != buildtool.CloneFile {
		t.Errorf("made by %q", g.Made)
	}
	if g.Cache != filepath.Join(f.root, "cache") || g.Temp != filepath.Join(f.root, "tmp") || g.Copy != filepath.Join(f.root, "copy") || g.Deps != f.deps {
		t.Errorf("grading = %+v", g)
	}
	env := envMapOf(g.Environ)
	if env["GOCACHE"] != g.Cache || env["TMPDIR"] != g.Temp || env["GOTMPDIR"] != g.Temp || env["GOPROXY"] != "off" {
		t.Errorf("environment = %v", g.Environ)
	}
	if info, err := os.Lstat(g.Temp); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("temp root: %v, %v", info, err)
	}
	// The grade (the agent's code) poisons its cache.
	if err := os.WriteFile(filepath.Join(g.Cache, "entry"), []byte("poisoned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.Cache, "planted"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, f.seed); len(got) != 1 || got["entry"] != "trusted" {
		t.Errorf("the seed after a grade: %v", got)
	}
	if err := g.remove(); err != nil {
		t.Fatal(err)
	}
	next, err := prepareGrading(context.Background(), f.input(f.root, f.seed, "go"))
	if err != nil {
		t.Fatal(err)
	}
	defer next.remove()
	if got := tree(t, next.Cache); len(got) != 1 || got["entry"] != "trusted" {
		t.Errorf("the next grade's cache: %v", got)
	}
}

func envMapOf(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		name, v, _ := strings.Cut(kv, "=")
		m[name] = v
	}
	return m
}

// Without a seed the cache is made as a seed would be: the profiles' PrepareRun (Gradle's user home with no daemon and
// no build cache).
func TestPrepareGradingWithoutASeed(t *testing.T) {
	f := newGradeFixture(t)
	g, err := prepareGrading(context.Background(), f.input(f.root, "", "gradle"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.remove()
	if g.Made != "prepared" || !strings.Contains(tree(t, g.Cache)["gradle/gradle.properties"], "org.gradle.caching=false") {
		t.Errorf("made %q: %v", g.Made, tree(t, g.Cache))
	}
}

// Two grades at once never share a writable cache: grades of two runs get their own clones of one seed, and a second
// grade in the same folder is refused, never given the first one's.
func TestConcurrentGradesGetTheirOwnCaches(t *testing.T) {
	f := newGradeFixture(t)
	if err := prepareSeed(context.Background(), f.golang, f.deps, f.seed, func(_ context.Context, dir string) error {
		return os.WriteFile(filepath.Join(dir, "entry"), []byte("trusted"), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	const n = 6
	grades := make([]grading, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			in := f.input(filepath.Join(f.env.Layout.Records, fmt.Sprintf("r%d", i+10), gradingFolder), f.seed, "go")
			if grades[i], errs[i] = prepareGrading(context.Background(), in); errs[i] != nil {
				return
			}
			errs[i] = os.WriteFile(filepath.Join(grades[i].Cache, "entry"), []byte(fmt.Sprintf("grade %d", i)), 0o600)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	caches := map[string]bool{}
	for i, g := range grades {
		if caches[g.Cache] || caches[g.Temp] {
			t.Errorf("grade %d shares a folder: %+v", i, g)
		}
		caches[g.Cache], caches[g.Temp] = true, true
		if got := tree(t, g.Cache)["entry"]; got != fmt.Sprintf("grade %d", i) {
			t.Errorf("grade %d reads %q", i, got)
		}
		g.remove()
	}
	if got := tree(t, f.seed)["entry"]; got != "trusted" {
		t.Errorf("the seed reads %q", got)
	}

	// The same folder twice at once: one grade gets it, the other is refused.
	var made atomic.Int32
	results := make([]grading, n)
	for i := range n {
		wg.Go(func() {
			if g, err := prepareGrading(context.Background(), f.input(f.root, f.seed, "go")); err == nil {
				made.Add(1)
				results[i] = g
			}
		})
	}
	wg.Wait()
	if made.Load() != 1 {
		t.Errorf("%d grades got one folder", made.Load())
	}
	for _, g := range results {
		if g.Root != "" {
			g.remove()
		}
	}
}

// withGrading removes the grade's folder whatever happens: on success, on an error, when cancelled, and on a panic.
// It also removes what a hostile grade left to resist removal: a folder without permissions, and (macOS) files and
// folders with the user's immutable flag.
func TestWithGradingRemovesTheGrade(t *testing.T) {
	f := newGradeFixture(t)
	if err := prepareSeed(context.Background(), f.golang, f.deps, f.seed, nil); err != nil {
		t.Fatal(err)
	}
	hostile := func(g grading) {
		locked := filepath.Join(g.Cache, "locked")
		must(t, os.MkdirAll(filepath.Join(locked, "deep"), 0o700))
		must(t, os.WriteFile(filepath.Join(locked, "deep", "f"), nil, 0o600))
		must(t, os.Chmod(filepath.Join(locked, "deep"), 0))
		must(t, os.Chmod(locked, 0))
		if runtime.GOOS == "darwin" {
			immutable := filepath.Join(g.Temp, "immutable")
			must(t, os.WriteFile(immutable, nil, 0o600))
			must(t, setFlags(immutable, ufImmutable))
			must(t, setFlags(g.Temp, ufAppend)) // on the folder: no entry can be removed
			// A link to the seed: removal never follows it.
			must(t, os.Symlink(f.seed, filepath.Join(g.Cache, "to-seed")))
		}
	}
	for name, grade := range map[string]func(context.Context, context.CancelFunc, grading) error{
		"success": func(_ context.Context, _ context.CancelFunc, g grading) error { hostile(g); return nil },
		"error": func(_ context.Context, _ context.CancelFunc, g grading) error {
			hostile(g)
			return errors.New("verification failed to start")
		},
		"cancel": func(ctx context.Context, cancel context.CancelFunc, g grading) error {
			hostile(g)
			cancel() // the user's ^C mid-grade
			<-ctx.Done()
			return ctx.Err()
		},
		"panic": func(_ context.Context, _ context.CancelFunc, g grading) error {
			hostile(g)
			panic("a bug in the grade")
		},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil && name != "panic" {
					t.Errorf("%s: panic %v", name, r)
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := withGrading(ctx, f.input(f.root, f.seed, "go"), func(g grading) error { return grade(ctx, cancel, g) })
			if strings.Contains(fmt.Sprint(err), "remove the grade's folder") {
				t.Errorf("%s: %v", name, err)
			}
		}()
		if _, err := os.Lstat(f.root); err == nil {
			t.Errorf("%s: the grade's folder was left", name)
			os.RemoveAll(f.root)
		}
		if _, err := os.Stat(f.seed); err != nil {
			t.Errorf("%s: the seed went too: %v", name, err)
		}
	}
}

// A grade that cannot be prepared leaves nothing, and never touches a folder already there.
func TestPrepareGradingFailsClean(t *testing.T) {
	f := newGradeFixture(t)
	if _, err := prepareGrading(context.Background(), f.input(f.root, f.seed, "go")); err == nil {
		t.Error("a missing seed was accepted")
	}
	if _, err := os.Lstat(f.root); err == nil {
		t.Error("a failed grade left its folder")
	}
	in := f.input(f.root, "", "go")
	in.Copy = filepath.Join(f.dir, "missing")
	if _, err := prepareGrading(context.Background(), in); err == nil {
		t.Error("a missing copy was accepted")
	}
	must(t, os.MkdirAll(f.root, 0o700))
	must(t, os.WriteFile(filepath.Join(f.root, "theirs"), []byte("another grade's"), 0o600))
	if _, err := prepareGrading(context.Background(), f.input(f.root, "", "go")); err == nil {
		t.Error("an existing grade's folder was reused")
	}
	if data, _ := os.ReadFile(filepath.Join(f.root, "theirs")); string(data) != "another grade's" {
		t.Error("an existing grade's folder was touched")
	}
	// A temp root JAVA_TOOL_OPTIONS cannot carry: the environment fails after the folders exist, and they go.
	spaced := filepath.Join(f.dir, "with space", gradingFolder)
	in = f.input(spaced, "", "maven")
	if _, err := prepareGrading(context.Background(), in); err == nil {
		t.Error("a temp root with a space was accepted")
	}
	if _, err := os.Lstat(spaced); err == nil {
		t.Error("a failed grade left its folder")
	}
}

// The grade's environment is the agent's recipe: what claude.Invocation.Command gives the agent for the same run (the
// copy as its checkout, the grade's cache as its build cache), less Claude Code's own variables and the sign-in, with
// the grade's temp root and the grader's additions. Credentials, GIT_* and AGENTIUM_* never reach it.
func TestGradingEnvIsTheAgentsRecipe(t *testing.T) {
	f := newGradeFixture(t)
	environ := []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(f.dir, "home"), "TMPDIR=/var/folders/xy/T/", "LANG=C",
		"GOFLAGS=-mod=mod", "GOPROXY=https://proxy.example", "GITHUB_TOKEN=ghp_secret", "AWS_SECRET_ACCESS_KEY=secret",
		"CLAUDE_CODE_OAUTH_TOKEN=secret", "ANTHROPIC_API_KEY=sk-secret", "GIT_DIR=/elsewhere", "AGENTIUM_HOME=/elsewhere",
		"PYTHONPATH=/evil", "PIP_INDEX_URL=https://user:token@index.example", "JAVA_TOOL_OPTIONS=-Djava.net.preferIPv4Stack=true",
		"GRADLE_USER_HOME=/Users/u/.gradle", "CARGO_HOME=/Users/u/.cargo"}
	graderOnly := map[string]bool{"TMPDIR": true, "GOTMPDIR": true, "JAVA_TOOL_OPTIONS": true, "GRADLE_DAEMON_BIND_ADDRESS": true, "GOPROXY": true}
	agentOnly := func(name string) bool {
		return strings.HasPrefix(name, "CLAUDE_") || strings.HasPrefix(name, "ANTHROPIC_") || name == "DISABLE_AUTOUPDATER" ||
			name == "ENABLE_CLAUDEAI_MCP_SERVERS"
	}
	venv := filepath.Join(f.deps, "py", "k1", "venv")
	meta := filepath.Join(f.deps, "py-meta", "k1", "x-1.0.dist-info")
	for _, tc := range []struct {
		tools, kept []string
		additions   map[string]string
	}{
		{tools: []string{"go"}, additions: map[string]string{"GOPROXY": "off", "GOTMPDIR": "<temp>"}},
		{tools: []string{"maven"}, additions: map[string]string{"JAVA_TOOL_OPTIONS": "-Djava.io.tmpdir=<temp>"}},
		{tools: []string{"gradle"}, additions: map[string]string{"JAVA_TOOL_OPTIONS": "-Djava.io.tmpdir=<temp>", "GRADLE_DAEMON_BIND_ADDRESS": "::1"}},
		{tools: []string{"cargo"}},
		{tools: []string{"python"}},
		{tools: []string{"python"}, kept: []string{"go"}, additions: map[string]string{"GOPROXY": "off", "GOTMPDIR": "<temp>"}},
	} {
		t.Run(strings.Join(append(tc.tools, tc.kept...), "+"), func(t *testing.T) {
			root := filepath.Join(f.env.Layout.Records, "r-"+strings.Join(append(tc.tools, tc.kept...), "-"), gradingFolder)
			in := gradingInput{Root: root, Copy: f.newCopy(root), Environ: environ, Agent: agent.Invocation{Tools: tc.tools, AgentTools: tc.kept,
				Home: filepath.Join(f.dir, "home"), Deps: f.deps, JavaHome: "/jdk/Contents/Home", Venv: venv, ProjectMetadata: meta, ImportRoot: "src"}}
			g, err := prepareGrading(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			defer g.remove()
			inv := in.Agent
			inv.CLI, inv.Dir, inv.Prompt, inv.Model, inv.BuildCache = "/bin/claude", g.Copy, "fix it", "claude-sonnet-5", g.Cache
			inv.SignIn, inv.Secret, inv.ConfigDir, inv.AllowLocalBinding = claude.SignInAPIKey, "sk-secret", filepath.Join(f.dir, "config"), true
			_, agentEnv, err := claude.Adapter{}.Command(inv, environ)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{}
			for name, v := range envMapOf(agentEnv) {
				if !agentOnly(name) && !graderOnly[name] {
					want[name] = v
				}
			}
			want["TMPDIR"] = g.Temp
			for name, v := range tc.additions {
				want[name] = strings.ReplaceAll(v, "<temp>", g.Temp)
			}
			got := envMapOf(g.Environ)
			if len(got) != len(g.Environ) {
				t.Errorf("a name is given twice: %q", g.Environ)
			}
			for name, v := range want {
				if got[name] != v {
					t.Errorf("%s = %q, want the agent's %q", name, got[name], v)
				}
			}
			for name, v := range got {
				if _, ok := want[name]; !ok {
					t.Errorf("%s = %q is the grader's alone", name, v)
				}
				if strings.Contains(v, "secret") || strings.Contains(v, "token") || v == "/elsewhere" || v == "/evil" {
					t.Errorf("%s = %q leaks the user's environment", name, v)
				}
			}
			if slices.ContainsFunc(g.Environ, func(kv string) bool { return strings.Contains(kv, "preferIPv4Stack") }) {
				t.Errorf("the IPv4-only flag is in %q", g.Environ)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
