package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/task"
)

// fakeUv is a host whose PATH holds only a fake uv and interpreter: uv finds the interpreter and syncs a venv (logged
// to calls), the interpreter reports its version and path.
func fakeUv(t *testing.T) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "bin"), filepath.Join(dir, "calls")
	python := filepath.Join(bin, "python3")
	writeFile(t, python, "#!/bin/sh\nprintf '3.12.13\\n%s\\n' '"+python+"'\n")
	writeFile(t, filepath.Join(bin, "uv"), `#!/bin/sh
echo "uv $1 $2 | UV_CACHE_DIR=$UV_CACHE_DIR UV_PROJECT_ENVIRONMENT=$UV_PROJECT_ENVIRONMENT" >> '`+calls+`'
case "$1" in
--version) echo 'uv 0.11.28';;
python) echo '`+python+`';;
sync) /bin/mkdir -p "$UV_PROJECT_ENVIRONMENT/bin" && echo 'home = /x' > "$UV_PROJECT_ENVIRONMENT/pyvenv.cfg" && /bin/ln -sf '`+python+`' "$UV_PROJECT_ENVIRONMENT/bin/python";;
*) exit 3;;
esac
`)
	for _, f := range []string{python, filepath.Join(bin, "uv")} {
		if err := os.Chmod(f, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, calls
}

// A Python project's run setup warms its venv in a throwaway checkout of the base, under the lock: the base's stamp
// names the venv, which the run gets with the stamp's notes, and the run's own pycache and uv folders exist. A later
// run of the base reuses it without uv sync; once the venv is no longer ready (its stamp removed) the base is warmed
// again, never handed a half-built venv.
func TestPythonWarmUpThroughTheRun(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, map[string]string{"pyproject.toml": "[project]\nname = \"x\"\nrequires-python = \">=3.10\"\n",
		"uv.lock": "version = 1\n", "src/x/__init__.py": ""})
	bin, calls := fakeUv(t)
	data := t.TempDir()
	deps := filepath.Join(data, "deps", "1")
	env := Env{Layout: home.Layout{Cache: filepath.Join(data, "cache")}, Bare: bare, VerifyTimeout: 20 * time.Second,
		Environ: []string{"PATH=" + bin, "HOME=/nonexistent"}, CommandEnv: []string{"UV_CACHE_DIR=" + filepath.Join(data, "cache", "uv")},
		Now: func() time.Time { return time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC) }}
	profiles := buildtool.Select([]string{"python"})
	prepare := func() buildtool.Warmed {
		t.Helper()
		run := filepath.Join(data, "ws", "go-build")
		warmed, notes, err := env.prepareTools(ctx, profiles, claude.Invocation{Deps: deps, BuildCache: run}, base, filepath.Join(data, "setup.log"), func(int) {})
		if err != nil || len(notes) != 0 {
			t.Fatalf("prepareTools: %+v %q %v", warmed, notes, err)
		}
		for _, name := range []string{"pycache", "uv"} {
			if info, err := os.Stat(filepath.Join(run, name)); err != nil || !info.IsDir() {
				t.Errorf("the run's %s folder: %v", name, err)
			}
		}
		return warmed
	}
	syncs := func() int { data, _ := os.ReadFile(calls); return strings.Count(string(data), "uv sync") }
	warmed := prepare()
	if !buildtool.VenvReady(warmed.Venv) || !strings.HasPrefix(warmed.Venv, filepath.Join(deps, "py")+string(filepath.Separator)) || syncs() != 1 {
		t.Fatalf("venv %q, syncs %d", warmed.Venv, syncs())
	}
	log, _ := os.ReadFile(calls)
	if !strings.Contains(string(log), "uv sync --frozen | UV_CACHE_DIR="+filepath.Join(deps, "uv-cache")+" UV_PROJECT_ENVIRONMENT="+warmed.Venv) {
		t.Errorf("uv sync's environment:\n%s", log)
	}
	stamp := env.stampPath(deps, base, []string{"python"})
	var inStamp buildtool.Warmed
	if content, err := os.ReadFile(stamp); err != nil || json.Unmarshal(content, &inStamp) != nil || inStamp.Venv != warmed.Venv {
		t.Errorf("the base's stamp %q: %v", content, err)
	}
	if left, _ := os.ReadDir(filepath.Join(data, "cache", "warm")); len(left) != 0 {
		t.Errorf("the throwaway checkout is left: %v", left)
	}

	if again := prepare(); again.Venv != warmed.Venv || syncs() != 1 {
		t.Errorf("a stamped base: venv %q, syncs %d", again.Venv, syncs())
	}
	if err := os.Remove(filepath.Join(filepath.Dir(warmed.Venv), "stamp.json")); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStamp(stamp); ok {
		t.Error("a stamp naming a venv that is not ready reads as warmed")
	}
	if again := prepare(); again.Venv != warmed.Venv || !buildtool.VenvReady(again.Venv) || syncs() != 2 {
		t.Errorf("an unready venv: %q, syncs %d", again.Venv, syncs())
	}
	if _, ok := readStamp(stamp + "-missing"); ok {
		t.Error("a missing stamp reads as warmed")
	}
	if err := os.WriteFile(stamp, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStamp(stamp); ok {
		t.Error("a stamp cut short reads as warmed")
	}
}

// A Python warm-up that cannot find an interpreter is a note, leaves the base unstamped, and the run gets no venv.
func TestPythonWarmUpFailureIsANote(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, map[string]string{"requirements.txt": "click\n"})
	data := t.TempDir()
	deps := filepath.Join(data, "deps", "1")
	env := Env{Layout: home.Layout{Cache: filepath.Join(data, "cache")}, Bare: bare, VerifyTimeout: 20 * time.Second,
		Environ: []string{"PATH=" + filepath.Join(data, "empty-bin")}}
	warmed, notes, err := env.prepareTools(ctx, buildtool.Select([]string{"python"}), claude.Invocation{Deps: deps, BuildCache: filepath.Join(data, "ws")},
		base, filepath.Join(data, "setup.log"), func(int) {})
	if err != nil || warmed.Venv != "" || len(notes) != 1 || !strings.Contains(notes[0], "no Python interpreter on this machine") {
		t.Errorf("%+v %q %v", warmed, notes, err)
	}
	if _, err := os.Stat(env.stampPath(deps, base, []string{"python"})); err == nil {
		t.Error("a failed warm-up was stamped")
	}
}

// Grading runs the verification in its copy with the run's venv (CheckoutEnv): PYTHONPATH is the grading copy, never
// the agent's checkout, and bytecode and uv's cache go to the data folder, which agents may not read.
func TestGradingUsesTheVenvInItsCopy(t *testing.T) {
	ctx := context.Background()
	repo := goldenRepo(t, map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n", "x/__init__.py": ""})
	head, err := gitx.Run(ctx, "-C", repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	graded, records := filepath.Join(data, "graded"), filepath.Join(data, "records")
	if err := checkout.New(ctx, repo, head, graded); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(records, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(data, "env.txt")
	venv := filepath.Join(data, "deps", "1", "py", "k", "venv")
	env := Env{Layout: home.Layout{Cache: filepath.Join(data, "cache")}, VerifyTimeout: 20 * time.Second, Environ: []string{"PATH=/usr/bin:/bin"},
		CommandEnv: []string{"PYTHONPATH=/user/py"}}
	env.checkoutEnv = func(dir string) []string {
		return buildtool.CheckoutEnv(buildtool.Select([]string{"python"}), buildtool.AgentContext{Allowed: env.Environ, Repo: dir,
			BuildCache: env.Layout.Cache, Venv: venv})
	}
	rec := Record{ContextHead: head, RecordsDir: records}
	spec := Spec{Task: task.Spec{Verify: []string{"env > " + out}}}
	if err := env.grade(ctx, spec, repo, graded, &rec, func(int) {}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	vars := map[string]string{}
	for _, line := range strings.Split(string(got), "\n") {
		if name, v, ok := strings.Cut(line, "="); ok {
			vars[name] = v
		}
	}
	for name, want := range map[string]string{"VIRTUAL_ENV": venv, "PYTHONPATH": graded, "PYTHONPYCACHEPREFIX": filepath.Join(data, "cache", "pycache"),
		"UV_CACHE_DIR": filepath.Join(data, "cache", "uv"), "UV_PROJECT_ENVIRONMENT": venv, "PIP_NO_INDEX": "1"} {
		if vars[name] != want {
			t.Errorf("grading's %s = %q, want %q", name, vars[name], want)
		}
	}
	if !strings.HasPrefix(vars["PATH"], filepath.Join(venv, "bin")+":") {
		t.Errorf("grading's PATH %q", vars["PATH"])
	}
	if rec.Passed == nil || !*rec.Passed {
		t.Errorf("graded %v", rec.Passed)
	}
}
