package run

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/task"
)

// fakeWheelFile writes a project's wheel (a zip): its code and a .dist-info whose METADATA names it, besides a RECORD
// and top_level.txt that must not reach the metadata folder.
func fakeWheelFile(t *testing.T, path, name, version string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	dist := name + "-" + version + ".dist-info/"
	for _, f := range [][2]string{{name + "/__init__.py", "X = 'wheel'\n"}, {dist + "METADATA", "Metadata-Version: 2.4\nName: " + name + "\nVersion: " + version + "\n\nThe README.\n"},
		{dist + "RECORD", name + "/__init__.py,,\n"}, {dist + "top_level.txt", name + "\n"}} {
		w, err := zw.Create(f[0])
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(f[1]))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, buf.String())
}

// fakeUv is a host whose PATH holds only a fake uv and interpreter: uv finds the interpreter, syncs a venv and builds
// the project's wheel (logged to calls), the interpreter reports its version and path.
func fakeUv(t *testing.T) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "bin"), filepath.Join(dir, "calls")
	fakeWheelFile(t, filepath.Join(dir, "x.whl"), "x", "1.0")
	python := filepath.Join(bin, "python3")
	writeFile(t, python, "#!/bin/sh\nprintf '3.12.13\\n%s\\n' '"+python+"'\n")
	writeFile(t, filepath.Join(bin, "uv"), `#!/bin/sh
echo "uv $1 $2 | UV_CACHE_DIR=$UV_CACHE_DIR UV_PROJECT_ENVIRONMENT=$UV_PROJECT_ENVIRONMENT" >> '`+calls+`'
case "$1" in
--version) echo 'uv 0.11.28';;
python) echo '`+python+`';;
sync) /bin/mkdir -p "$UV_PROJECT_ENVIRONMENT/bin" && echo 'home = /x' > "$UV_PROJECT_ENVIRONMENT/pyvenv.cfg" && /bin/ln -sf '`+python+`' "$UV_PROJECT_ENVIRONMENT/bin/python";;
build) if [ -n "$FAKE_BUILD_FAIL" ]; then exit 1; fi; /bin/cp '`+filepath.Join(dir, "x.whl")+`' "$4/";;
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
	bare, base := bareWith(t, map[string]string{"pyproject.toml": "[project]\nname = \"x\"\nrequires-python = \">=3.10\"\n[build-system]\nrequires = [\"flit_core\"]\n",
		"uv.lock": "version = 1\n", "src/x/__init__.py": ""})
	bin, calls := fakeUv(t)
	data := writableTempDir(t)
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
	if content, err := os.ReadFile(stamp); err != nil || json.Unmarshal(content, &inStamp) != nil || inStamp.Venv != warmed.Venv || inStamp.Metadata != warmed.Metadata {
		t.Errorf("the base's stamp %q: %v", content, err)
	}
	// The base's metadata: made in the warm-up (uv build), kept in the deps folder, named by the stamp.
	if !buildtool.MetadataReady(warmed.Metadata) || filepath.Dir(warmed.Metadata) != filepath.Join(deps, "py-meta") ||
		!strings.Contains(string(log), "uv build --wheel | UV_CACHE_DIR="+filepath.Join(deps, "uv-cache")) {
		t.Errorf("metadata %q:\n%s", warmed.Metadata, log)
	}
	if left, _ := os.ReadDir(filepath.Join(data, "cache", "warm")); len(left) != 0 {
		t.Errorf("the throwaway checkout is left: %v", left)
	}

	builds := func() int { data, _ := os.ReadFile(calls); return strings.Count(string(data), "uv build") }
	if again := prepare(); again.Venv != warmed.Venv || again.Metadata != warmed.Metadata || syncs() != 1 || builds() != 1 {
		t.Errorf("a stamped base: venv %q, metadata %q, syncs %d, builds %d", again.Venv, again.Metadata, syncs(), builds())
	}
	// Metadata that is no longer as made (a file planted in it) is not handed to a run: the base is warmed again, and
	// gets it made again, beside the changed folder moved aside.
	if err := os.Chmod(warmed.Metadata, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(warmed.Metadata, "x.py"), "X = 'planted'\n")
	if _, ok := readStamp(stamp, profiles); ok {
		t.Error("a stamp naming changed metadata reads as warmed")
	}
	if again := prepare(); again.Metadata != warmed.Metadata || !buildtool.MetadataReady(again.Metadata) || builds() != 2 {
		t.Errorf("changed metadata: %q, builds %d", again.Metadata, builds())
	}
	if err := os.Chmod(filepath.Dir(warmed.Venv), 0o700); err != nil { // stamped venvs are read-only, their folder too
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(warmed.Venv), "stamp.json")); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStamp(stamp, profiles); ok {
		t.Error("a stamp naming a venv that is not ready reads as warmed")
	}
	if again := prepare(); again.Venv != warmed.Venv || !buildtool.VenvReady(again.Venv) || syncs() != 2 {
		t.Errorf("an unready venv: %q, syncs %d", again.Venv, syncs())
	}
	if _, ok := readStamp(stamp+"-missing", profiles); ok {
		t.Error("a missing stamp reads as warmed")
	}
	if err := os.WriteFile(stamp, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStamp(stamp, profiles); ok {
		t.Error("a stamp cut short reads as warmed")
	}
}

// writableTempDir is a temporary folder that may come to hold read-only venvs: made writable again before removal.
func writableTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Type()&os.ModeSymlink == 0 {
				if info, err := d.Info(); err == nil {
					os.Chmod(p, info.Mode().Perm()|0o200)
				}
			}
			return nil
		})
	})
	return dir
}

// A Python warm-up that cannot find an interpreter is permanent: the run gets no venv and a note, and the base is
// stamped with the note, so a later run repeats it without warming again.
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
	if _, ok := readStamp(env.stampPath(deps, base, []string{"python"}), buildtool.Select([]string{"python"})); !ok {
		t.Error("a permanent failure was not stamped")
	}
	if left, _ := os.ReadDir(filepath.Join(data, "cache", "warm")); len(left) != 0 {
		t.Errorf("throwaway left: %v", left)
	}
	again, notes2, err := env.prepareTools(ctx, buildtool.Select([]string{"python"}), claude.Invocation{Deps: deps, BuildCache: filepath.Join(data, "ws")},
		base, filepath.Join(data, "setup2.log"), func(int) {})
	if err != nil || again.Venv != "" || !slices.Equal(notes2, notes) {
		t.Errorf("a later run: %+v %q %v", again, notes2, err)
	}
	if _, err := os.Stat(filepath.Join(data, "setup2.log")); err == nil {
		t.Error("a later run warmed again")
	}
}

// Grading runs the verification in its copy with the run's venv (CheckoutEnv): PYTHONPATH is the grading copy's import
// root as the base decided it (src/ here, though the agent removed it), never the agent's checkout; bytecode and uv's
// cache go to the data folder, which agents may not read; and the user's Python settings the agent never got are gone.
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
	meta := filepath.Join(data, "deps", "1", "py-meta", "0123456789abcdef")
	user := []string{"PATH=/usr/bin:/bin", "PYTHONHOME=/host/py", "PYTHONOPTIMIZE=2", "PIP_INDEX_URL=https://x", "VIRTUAL_ENV=/host/venv"}
	env := Env{Layout: home.Layout{Cache: filepath.Join(data, "cache")}, VerifyTimeout: 20 * time.Second, Environ: user,
		CommandEnv: []string{"PYTHONPATH=/user/py"}}
	env.checkoutEnv = func(dir string) []string {
		return buildtool.CheckoutEnv(buildtool.Select([]string{"python"}), buildtool.AgentContext{Allowed: env.Environ, Repo: dir,
			BuildCache: env.Layout.Cache, Venv: venv, Metadata: meta, ImportRoot: "src"})
	}
	env.checkoutBase = runner.Environ(buildtool.CheckoutEnviron(buildtool.Select([]string{"python"}), user))
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
	for _, name := range []string{"PYTHONHOME", "PYTHONOPTIMIZE", "PIP_INDEX_URL"} {
		if v, ok := vars[name]; ok {
			t.Errorf("the user's %s=%q reached grading", name, v)
		}
	}
	if h := vars["HYPOTHESIS_STORAGE_DIRECTORY"]; filepath.Dir(h) != filepath.Join(data, "cache", "hypothesis") {
		t.Errorf("grading's hypothesis database %q: not the copy's own in the data folder", h)
	}
	for name, want := range map[string]string{"VIRTUAL_ENV": venv, "PYTHONPATH": filepath.Join(graded, "src") + ":" + meta, "PYTHONPYCACHEPREFIX": filepath.Join(data, "cache", "pycache"),
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

// fakePip is a host whose PATH holds a fake python3 (3.12) that makes venvs and runs a fake pip: --version, a resolve
// that reports one package, installs, and a wheel's build (logged to calls).
func fakePip(t *testing.T) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "bin"), filepath.Join(dir, "calls")
	fakeWheelFile(t, filepath.Join(dir, "pkg.whl"), "pkg", "2.0")
	python := filepath.Join(bin, "python3")
	writeFile(t, python, `#!/bin/sh
echo "python $*" >> '`+calls+`'
case "$1" in -I) printf '3.12.13\n%s\n' '`+python+`'; exit 0;; esac
if [ "$1 $2" = "-m venv" ]; then /bin/mkdir -p "$3/bin" "$3/lib/python3.12/site-packages" && echo 'home = /x' > "$3/pyvenv.cfg" && /bin/ln -s '`+python+`' "$3/bin/python"; exit $?; fi
if [ "$1 $2 $3" = "-m pip --version" ]; then echo "pip 25.0 from /x"; exit 0; fi
if [ "$1 $2 $3 $4 $5 $6" = "-m pip wheel --quiet --no-deps --wheel-dir" ]; then /bin/cp '`+filepath.Join(dir, "pkg.whl")+`' "$7/"; exit $?; fi
if [ "$1 $2 $3" = "-m pip install" ]; then
  while [ $# -gt 0 ]; do
    if [ "$1" = "--report" ]; then echo '{"install":[{"metadata":{"name":"pytest","version":"8.4.0"},"download_info":{"url":"https://x","archive_info":{}}}]}' > "$2"; exit 0; fi
    shift
  done
  exit 0
fi
exit 3
`)
	if err := os.Chmod(python, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// Validation warms the base's venv as a run does (the same deps folder, lock and stamp) and runs the verification with
// it: the venv's interpreter first on PATH, the base's src layout on PYTHONPATH, caches in the data folder, and none of
// the host's VIRTUAL_ENV, PYTHONHOME or pip settings. A run of the same base then reuses that venv without warming.
func TestValidationUsesTheRunsVenv(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, map[string]string{"pyproject.toml": "[project]\nname = \"pkg\"\nrequires-python = \">=3.10\"\n",
		"requirements-test.txt": "pytest\n", "src/pkg/__init__.py": "", "tests/test_pkg.py": ""})
	bin, calls := fakePip(t)
	data := writableTempDir(t)
	layout := home.Layout{Root: data, Deps: filepath.Join(data, "deps"), Cache: filepath.Join(data, "cache"), Artifacts: filepath.Join(data, "artifacts")}
	host := []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=/nonexistent", "VIRTUAL_ENV=/host/venv", "PYTHONHOME=/host/py",
		"PIP_INDEX_URL=https://u:p@index", "PYTHONWARNINGS=ignore"} // secret-scan: allow
	c := CommandsEnv{Layout: layout, Bare: bare, Environ: host, Timeout: 20 * time.Second, Now: time.Now}
	out := filepath.Join(data, "env.txt")
	v := task.Validator{Bare: bare, WorkDir: filepath.Join(data, "checkouts"), LogDir: filepath.Join(data, "logs"), Timeout: 20 * time.Second,
		Cache: layout.Cache, Now: time.Now,
		Checkout: func(ctx context.Context, base string, verify []string, logPath string) (task.CheckoutCommands, error) {
			return CheckoutCommands(ctx, c, base, verify, logPath)
		}}
	// The verification fills its hypothesis database, which goes with the validation's checkout.
	result, err := v.Validate(ctx, task.Spec{Base: base, Verify: []string{"env > " + out + " && command -v python >> " + out +
		` && mkdir -p "$HYPOTHESIS_STORAGE_DIRECTORY/examples"`}}, []task.Arm{{Name: "base"}})
	if err != nil || len(result.Stages) != 1 || !result.Stages[0].OK {
		t.Fatalf("%+v %v", result, err)
	}
	got, _ := os.ReadFile(out)
	vars := map[string]string{}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	for _, line := range lines {
		if name, v, ok := strings.Cut(line, "="); ok {
			vars[name] = v
		}
	}
	deps := filepath.Join(layout.Deps, filepath.Base(filepath.Dir(bare)))
	venv := vars["VIRTUAL_ENV"]
	if !strings.HasPrefix(venv, filepath.Join(deps, "py")+string(filepath.Separator)) || !buildtool.VenvReady(venv) {
		t.Fatalf("VIRTUAL_ENV %q", venv)
	}
	if python := lines[len(lines)-1]; python != filepath.Join(venv, "bin", "python") {
		t.Errorf("python is %q", python)
	}
	meta := strings.TrimPrefix(vars["PYTHONPATH"], filepath.Join(v.WorkDir, "base-base", "src")+":")
	if !buildtool.MetadataReady(meta) || filepath.Dir(meta) != filepath.Join(deps, "py-meta") {
		t.Errorf("PYTHONPATH %q, want the checkout's src, then the base's metadata folder", vars["PYTHONPATH"])
	}
	if h := vars["HYPOTHESIS_STORAGE_DIRECTORY"]; filepath.Dir(h) != filepath.Join(layout.Cache, "hypothesis") {
		t.Errorf("validation's hypothesis database %q", h)
	} else if _, err := os.Stat(h); err == nil {
		t.Errorf("validation's hypothesis database %s outlived its checkout", h)
	}
	for name, want := range map[string]string{"PYTHONPYCACHEPREFIX": filepath.Join(layout.Cache, "pycache"), "PIP_NO_INDEX": "1",
		"UV_PROJECT_ENVIRONMENT": venv} {
		if vars[name] != want {
			t.Errorf("%s = %q, want %q", name, vars[name], want)
		}
	}
	for _, name := range []string{"PYTHONHOME", "PIP_INDEX_URL", "PYTHONWARNINGS"} {
		if v, ok := vars[name]; ok {
			t.Errorf("the host's %s=%q reached validation", name, v)
		}
	}
	// A run of the same base finds it warmed: the same venv, no second resolve.
	resolves := func() int { b, _ := os.ReadFile(calls); return strings.Count(string(b), "--dry-run") }
	env := Env{Layout: layout, Bare: bare, Environ: host, VerifyTimeout: 20 * time.Second}
	warmed, _, err := env.prepareTools(ctx, buildtool.Select([]string{"python"}), claude.Invocation{Deps: deps, BuildCache: filepath.Join(data, "ws")},
		base, filepath.Join(data, "setup.log"), func(int) {})
	if err != nil || warmed.Venv != venv || warmed.Metadata != meta || resolves() != 1 {
		t.Errorf("the run's venv %q and metadata %q (validation's %q, %q), resolves %d: %v", warmed.Venv, warmed.Metadata, venv, meta, resolves(), err)
	}
}

// A transient Python failure leaves the base unstamped, but the other tools' warm-up steps that succeeded do not run
// again on the next run; an empty stamp, with a profile that warms in Go, is not a warmed base.
func TestPythonFailureNeverRewarmsOtherTools(t *testing.T) {
	dir := writableTempDir(t)
	deps, steps := filepath.Join(dir, "deps"), filepath.Join(dir, "steps")
	env := Env{VerifyTimeout: 20 * time.Second, Layout: home.Layout{Cache: filepath.Join(dir, "cache")}, Environ: []string{"PATH=" + filepath.Join(dir, "none")}}
	failing := buildtool.Profile{Name: "python", WarmFunc: func(context.Context, buildtool.WarmInput) (buildtool.Warmed, error) {
		return buildtool.Warmed{Failed: "uv sync failed", Transient: true}, nil
	}}
	profiles := []buildtool.Profile{{Name: "cargo"}, failing}
	warm := []buildtool.WarmStep{{Command: "echo fetched >> " + steps}}
	for i := 0; i < 2; i++ {
		note, err := env.warmTools(context.Background(), t.TempDir(), deps, "c1", profiles, []string{"cargo", "python"}, warm, filepath.Join(dir, "log"), func(int) {})
		if err != nil || !strings.Contains(note, "not stamped") {
			t.Fatalf("run %d: %q %v", i, note, err)
		}
	}
	if got, _ := os.ReadFile(steps); string(got) != "fetched\n" {
		t.Errorf("the other tools' steps ran %q", got)
	}
	stamp := env.stampPath(deps, "c1", []string{"cargo", "python"})
	if _, err := os.Stat(stamp); err == nil {
		t.Error("a transient failure was stamped")
	}
	if err := os.WriteFile(stamp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStamp(stamp, profiles); ok {
		t.Error("an empty stamp reads as warmed with a Go warm-up selected")
	}
	if _, ok := readStamp(stamp, buildtool.Select([]string{"cargo"})); !ok {
		t.Error("an empty stamp of tools without a Go warm-up is warmed, as before")
	}
}

// The steps marker never hides a network failure of the fixed steps: a Gradle skip that looks like one leaves neither
// stamp nor marker, so the next run runs the steps again. A permanent skip, with a Go warm-up that failed transiently,
// is kept in the marker: the next run skips the steps and still stamps the base with the skip's note.
func TestStepsMarkerWaitsForNetworkSkips(t *testing.T) {
	dir := writableTempDir(t)
	deps, log := filepath.Join(dir, "deps"), filepath.Join(dir, "steps")
	env := Env{VerifyTimeout: 20 * time.Second, Layout: home.Layout{Cache: filepath.Join(dir, "cache")}}
	pyFails := true
	python := buildtool.Profile{Name: "python", WarmFunc: func(context.Context, buildtool.WarmInput) (buildtool.Warmed, error) {
		if pyFails {
			return buildtool.Warmed{Failed: "uv sync failed", Transient: true}, nil
		}
		return buildtool.Warmed{Venv: ""}, nil
	}}
	profiles := []buildtool.Profile{{Name: "gradle"}, python}
	names := []string{"gradle", "python"}
	step := func(reason string) []buildtool.WarmStep {
		cmd := "echo ran >> " + log
		if reason != "" {
			cmd += "; printf ':app:conf\\t" + reason + "\\n' > " + buildtool.WarmSkippedFile
		}
		return []buildtool.WarmStep{{Command: cmd}}
	}
	warm := func(base string, steps []buildtool.WarmStep) string {
		t.Helper()
		note, err := env.warmTools(context.Background(), t.TempDir(), deps, base, profiles, names, steps, filepath.Join(dir, "log"), func(int) {})
		if err != nil {
			t.Fatal(err)
		}
		return note
	}
	ran := func() int { b, _ := os.ReadFile(log); return strings.Count(string(b), "ran") }

	// A network skip: not stamped, no marker; the next run (Python now fine, the network back) runs the steps again.
	pyFails = false
	if note := warm("c1", step("Could not GET https://repo/x.pom")); !strings.Contains(note, "network failure") {
		t.Fatalf("note %q", note)
	}
	if _, err := os.Stat(env.stampPath(deps, "c1", names) + ".steps"); err == nil {
		t.Error("a marker after a network skip")
	}
	if note := warm("c1", step("")); note != "" || ran() != 2 {
		t.Errorf("the retry: note %q, steps ran %d times", note, ran())
	}
	if _, ok := readStamp(env.stampPath(deps, "c1", names), profiles); !ok {
		t.Error("not stamped after the retry")
	}

	// A permanent skip while Python fails transiently: the marker keeps the note, and the next run, skipping the steps,
	// stamps the base with it.
	pyFails = true
	warm("c2", step("Cannot choose between variants"))
	pyFails = false
	before := ran()
	if note := warm("c2", step("")); !strings.Contains(note, "1 configuration(s) not warmed: :app:conf") || ran() != before {
		t.Errorf("note %q, steps ran again: %v", note, ran() != before)
	}
}

// A metadata build that fails is retried by the base's next warm-ups (a download of the build backend may have
// failed): each run that warmed still gets the venv, without metadata, and a note; the base is not stamped. The third
// failure in a row is stamped with its note, and later runs of the base build nothing.
func TestPythonMetadataFailureIsRetriedThroughTheRun(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, map[string]string{"pyproject.toml": "[project]\nname = \"x\"\nrequires-python = \">=3.10\"\n[build-system]\nrequires = [\"flit_core\"]\n",
		"uv.lock": "version = 1\n", "src/x/__init__.py": ""})
	bin, calls := fakeUv(t)
	data := writableTempDir(t)
	deps := filepath.Join(data, "deps", "1")
	env := Env{Layout: home.Layout{Cache: filepath.Join(data, "cache")}, Bare: bare, VerifyTimeout: 20 * time.Second,
		Environ: []string{"PATH=" + bin, "HOME=/nonexistent", "FAKE_BUILD_FAIL=1"}, Now: time.Now}
	profiles := buildtool.Select([]string{"python"})
	builds := func() int { data, _ := os.ReadFile(calls); return strings.Count(string(data), "uv build") }
	stamp := env.stampPath(deps, base, []string{"python"})
	for try := 1; try <= 4; try++ {
		warmed, notes, err := env.prepareTools(ctx, profiles, claude.Invocation{Deps: deps, BuildCache: filepath.Join(data, "ws")}, base,
			filepath.Join(data, "setup.log"), func(int) {})
		if err != nil || !buildtool.VenvReady(warmed.Venv) || warmed.Metadata != "" || len(notes) != 1 {
			t.Fatalf("try %d: %+v %q %v", try, warmed, notes, err)
		}
		_, stamped := readStamp(stamp, profiles)
		switch {
		case try < 3 && (stamped || !strings.Contains(notes[0], fmt.Sprintf("(try %d of 3)", try)) || !strings.Contains(notes[0], "not stamped")):
			t.Errorf("try %d: stamped %v, %q", try, stamped, notes)
		case try >= 3 && (!stamped || !strings.Contains(notes[0], "(3 tries)")):
			t.Errorf("try %d: stamped %v, %q", try, stamped, notes)
		}
		if want := min(try, 3); builds() != want {
			t.Errorf("try %d: %d builds, want %d", try, builds(), want)
		}
	}
}

// What Agentium's own commands kept for one checkout in the data folder (Python's hypothesis database) goes with the
// run's checkout and its grading copy; another checkout's stays.
func TestRemoveCheckoutsRemovesTheirHypothesisDatabases(t *testing.T) {
	data := t.TempDir()
	env := Env{Layout: home.Layout{Cache: filepath.Join(data, "cache")}, Environ: []string{"PATH=/usr/bin:/bin"}}
	env = env.withCheckoutTools(buildtool.Select([]string{"python"}), filepath.Join(data, "deps"), buildtool.Warmed{}, "")
	workspace := filepath.Join(data, "ws")
	repo, graded, other := filepath.Join(workspace, "repo"), filepath.Join(data, "records", "r1", "verify"), filepath.Join(data, "other")
	databases := map[string]string{}
	for _, dir := range []string{repo, graded, other} {
		for _, kv := range env.checkoutEnv(dir) {
			if v, ok := strings.CutPrefix(kv, "HYPOTHESIS_STORAGE_DIRECTORY="); ok {
				databases[dir] = v
			}
		}
		if filepath.Dir(databases[dir]) != filepath.Join(data, "cache", "hypothesis") {
			t.Fatalf("%s: database %q", dir, databases[dir])
		}
		if err := os.MkdirAll(filepath.Join(databases[dir], "examples"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env.removeCheckouts(workspace, repo, graded)
	for dir, h := range databases {
		_, err := os.Stat(h)
		if gone := err != nil; gone != (dir != other) {
			t.Errorf("%s: its database removed %v", dir, gone)
		}
	}
	if _, err := os.Stat(graded); err == nil {
		t.Error("the grading copy was left")
	}
}
