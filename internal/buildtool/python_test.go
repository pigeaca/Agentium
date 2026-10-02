package buildtool

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// A Python project is detected by its own files at the root; discovery, not the profile, proposes its test command
// (it reads the files), and the profile names its language, runners and configuration (requirement files by pattern).
func TestPythonDetection(t *testing.T) {
	for _, files := range [][]string{{"pyproject.toml"}, {"setup.py"}, {"setup.cfg"}, {"requirements.txt"}, {"uv.lock"}} {
		if got := DetectedNames(hasFiles(files...)); !slices.Equal(got, []string{"python"}) {
			t.Errorf("%q: detected %q", files, got)
		}
		if got := TestCommands(hasFiles(files...)); len(got) != 0 {
			t.Errorf("%q: the profile proposed %q", files, got)
		}
	}
	for _, files := range [][]string{{"requirements/dev.txt"}, {"src/pkg/__init__.py"}, {"tox.ini"}, {"package.json"}} {
		if got := DetectedNames(hasFiles(files...)); len(got) != 0 {
			t.Errorf("%q: detected %q", files, got)
		}
	}
	p := Select([]string{"python"})[1]
	if p.Name != "python" || !slices.Equal(p.Languages, []string{"python"}) || len(NeedsWarming(Select([]string{"python"}))) != 1 {
		t.Errorf("profile %q, languages %q", p.Name, p.Languages)
	}
	configs := RunnerConfigs()
	for _, word := range []string{"pytest", "tox", "nox", "unittest"} {
		for _, f := range []string{"pyproject.toml", "uv.lock", "requirements*.txt", "requirements/*.txt", "setup.cfg", "setup.py", "tox.ini", "pytest.ini", "conftest.py"} {
			if !slices.Contains(configs[word], f) {
				t.Errorf("%s's configuration lacks %s", word, f)
			}
		}
	}
}

// Python's runners count as running tests however they are started; installing, linting and reading do not.
func TestPythonTestPatterns(t *testing.T) {
	re := regexp.MustCompile(`\b(` + strings.Join(TestPatterns(), "|") + `)\b`)
	for _, c := range []string{"pytest", "pytest -q tests/test_parser.py", "uv run pytest tests/test_formatting.py", "python -m pytest",
		"python3 -m pytest -x", "python3.12 -m unittest tests.test_recipes", ".venv/bin/pytest", "uv run python -m unittest", "tox -e py312",
		"nox -s tests", "cd src && python -m pytest ../tests"} {
		if !re.MatchString(c) {
			t.Errorf("%q is not seen as running tests", c)
		}
	}
	for _, c := range []string{"python -m pip list", "uv run ruff check", "python3 setup.py build", "uv sync", "cat requirements.txt",
		"python -c 'import click'", "echo contest", "toxic", "pip install pytesting"} {
		if re.MatchString(c) {
			t.Errorf("%q is seen as running tests", c)
		}
	}
}

// The agent's environment for a Python project: the venv active and first on PATH, the checkout (src/ for the src
// layout) on PYTHONPATH, bytecode and uv's cache in the run's own cache, no pytest cache, pip and uv offline, uv in the
// venv without syncing. Without a venv the host's interpreter runs with the same settings; without a build cache no
// bytecode is written.
func TestPythonEnv(t *testing.T) {
	repo := t.TempDir()
	ctx := AgentContext{Allowed: []string{"PATH=/usr/bin:/bin", "PYTHONPATH=/user/py"}, Home: "/home/u", Repo: repo,
		BuildCache: "/data/workspaces/r1/go-build", Deps: "/data/deps/1", Venv: "/data/deps/1/py/k/venv"}
	got := AgentEnv(Select([]string{"python"}), ctx)
	want := []string{"GOFLAGS=-buildvcs=false", "VIRTUAL_ENV=/data/deps/1/py/k/venv", "PATH=/data/deps/1/py/k/venv/bin:/usr/bin:/bin",
		"PYTHONPATH=" + repo, "PYTHONPYCACHEPREFIX=/data/workspaces/r1/go-build/pycache", "PYTEST_ADDOPTS=-p no:cacheprovider",
		"PIP_NO_INDEX=1", "PIP_DISABLE_PIP_VERSION_CHECK=1", "UV_OFFLINE=1", "UV_NO_SYNC=1", "UV_FROZEN=1", "UV_PYTHON_DOWNLOADS=never",
		"UV_PROJECT_ENVIRONMENT=/data/deps/1/py/k/venv", "UV_CACHE_DIR=/data/workspaces/r1/go-build/uv"}
	if !slices.Equal(got, want) {
		t.Errorf("agent env\n got %q\nwant %q", got, want)
	}
	// Agentium's own commands in a checkout: the same, with the data folder's cache.
	if got := CheckoutEnv(Select([]string{"python"}), AgentContext{Allowed: ctx.Allowed, Repo: repo, BuildCache: "/data/cache", Venv: ctx.Venv}); !slices.Contains(got, "PYTHONPYCACHEPREFIX=/data/cache/pycache") ||
		!slices.Contains(got, "UV_CACHE_DIR=/data/cache/uv") || !slices.Contains(got, "VIRTUAL_ENV="+ctx.Venv) {
		t.Errorf("checkout env %q", got)
	}
	if got := CheckoutEnv(Select([]string{"go", "maven", "gradle", "cargo"}), ctx); len(got) != 0 {
		t.Errorf("other tools set %q in their commands' checkouts", got)
	}

	// The src layout: a package (or a module) under src/.
	writeFiles(t, repo, map[string]string{"src/click/__init__.py": ""})
	if e := env(t, AgentEnv(Select([]string{"python"}), ctx)); e["PYTHONPATH"] != filepath.Join(repo, "src") {
		t.Errorf("src layout: PYTHONPATH=%q", e["PYTHONPATH"])
	}
	other := t.TempDir()
	writeFiles(t, other, map[string]string{"src/README.md": "", "src/data/x.json": "{}", "pkg/__init__.py": ""})
	if got := importRoot(other); got != other {
		t.Errorf("src without code: %q", got)
	}
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(repo, "src"), filepath.Join(linked, "src")); err != nil {
		t.Fatal(err)
	}
	if got := importRoot(linked); got != linked {
		t.Errorf("a linked src/ is followed: %q", got)
	}

	noVenv := ctx
	noVenv.Venv = ""
	e := env(t, AgentEnv(Select([]string{"python"}), noVenv))
	for _, name := range []string{"VIRTUAL_ENV", "PATH", "UV_PROJECT_ENVIRONMENT"} {
		if v, ok := e[name]; ok {
			t.Errorf("without a venv %s=%q", name, v)
		}
	}
	if e["PIP_NO_INDEX"] != "1" || e["UV_OFFLINE"] != "1" || e["PYTHONPATH"] == "" {
		t.Errorf("without a venv the offline settings went too: %q", e)
	}
	bare := AgentEnv(Select([]string{"python"}), AgentContext{Allowed: ctx.Allowed})
	if !slices.Contains(bare, "PYTHONDONTWRITEBYTECODE=1") || slices.ContainsFunc(bare, func(kv string) bool {
		return strings.HasPrefix(kv, "PYTHONPYCACHEPREFIX=") || strings.HasPrefix(kv, "UV_CACHE_DIR=") || strings.HasPrefix(kv, "PYTHONPATH=")
	}) {
		t.Errorf("no build cache, no checkout: %q", bare)
	}
	// Agentium's own commands keep uv's, pip's and Python's caches in the data folder.
	if got := CommandEnvFor(Select([]string{"python"}), "/data/cache"); !slices.Equal(got, []string{"UV_CACHE_DIR=/data/cache/uv",
		"PIP_CACHE_DIR=/data/cache/pip", "PYTHONPYCACHEPREFIX=/data/cache/pycache"}) {
		t.Errorf("command env %q", got)
	}
	run := t.TempDir()
	if err := PrepareRun(context.Background(), Select([]string{"python"}), "", run); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pycache", "uv"} {
		if info, err := os.Stat(filepath.Join(run, name)); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("the run's %s folder: %v %v", name, info, err)
		}
	}
}

// The user's Python and npm/pnpm caches are denied in every project, at their defaults and where variables put them;
// so is the user's active venv, but never the home folder, and never uv's managed interpreters.
func TestPythonAndNodeUserCaches(t *testing.T) {
	home := "/home/u"
	got := UserCaches([]string{"XDG_CACHE_HOME=/x/cache", "UV_CACHE_DIR=/uvc", "PIP_CACHE_DIR=/pipc", "POETRY_CACHE_DIR=relative",
		"VIRTUAL_ENV=/home/u/proj/.venv", "npm_config_cache=/npmc", "PNPM_HOME=/home/u/Library/pnpm", "XDG_DATA_HOME=/x/data"}, home)
	for _, want := range []string{"/home/u/.cache/uv", "/home/u/.cache/pip", "/home/u/Library/Caches/pip", "/home/u/.cache/pypoetry",
		"/home/u/Library/Caches/pypoetry", "/x/cache/uv", "/x/cache/pip", "/x/cache/pypoetry", "/uvc", "/pipc", "/home/u/proj/.venv",
		"/home/u/.npm", "/home/u/Library/pnpm/store", "/home/u/.local/share/pnpm/store", "/npmc", "/x/data/pnpm/store"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not denied", want)
		}
	}
	for _, never := range []string{"relative", "/home/u/.local/share/uv/python", "/home/u/.local/share/uv", "/home/u/Library/pnpm", home} {
		if slices.Contains(got, never) {
			t.Errorf("%s is denied", never)
		}
	}
	for _, venv := range []string{"/home/u", "/home", "/", "/home/u/"} {
		if slices.Contains(pythonCaches([]string{"VIRTUAL_ENV=" + venv}, home), filepath.Clean(venv)) {
			t.Errorf("VIRTUAL_ENV=%s denies the home folder or one above it", venv)
		}
	}
}

// Agents read only the venvs of the deps folder: uv's and pip's download caches and the resolve reports are denied,
// each whole, so what a later warm-up adds there is denied too.
func TestPythonDepsDeniedWhole(t *testing.T) {
	got := DepsDenied("/data/deps/1")
	for _, want := range []string{"/data/deps/1/uv-cache", "/data/deps/1/pip-cache", "/data/deps/1/py-resolve"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not denied: %q", want, got)
		}
	}
	for _, p := range got {
		if strings.ContainsAny(p, "*?[") || strings.HasPrefix(p, "/data/deps/1/py/") || p == "/data/deps/1/py" {
			t.Errorf("denied %q: a pattern, or the venvs", p)
		}
	}
}

// fakePython is a host with fake tools in bin (and nothing else on PATH): an interpreter that reports version and
// makes venvs, its pip (which writes report as its resolve's report and records what it installs), and, with uv, a
// uv that finds that interpreter and syncs a venv. Every call is logged to calls.
type fakePython struct {
	bin, calls, interp string
}

func newFakePython(t *testing.T, version string, uv bool, report string) fakePython {
	t.Helper()
	dir := t.TempDir()
	f := fakePython{bin: filepath.Join(dir, "bin"), calls: filepath.Join(dir, "calls.log")}
	f.interp = filepath.Join(f.bin, "python3")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	python := `#!/bin/sh
echo "python $* | VIRTUAL_ENV=$VIRTUAL_ENV PIP_CACHE_DIR=$PIP_CACHE_DIR PYTHONPATH=$PYTHONPATH" >> '` + f.calls + `'
case "$1" in
-I) printf '%s\n%s\n' '` + version + `' '` + f.interp + `'; exit 0;;
--version) echo 'Python ` + version + `'; exit 0;;
esac
if [ "$1 $2" = "-m venv" ]; then
  /bin/mkdir -p "$3/bin" && echo 'home = /x' > "$3/pyvenv.cfg" && /bin/ln -s '` + f.interp + `' "$3/bin/python"; exit $?
fi
if [ "$1 $2 $3" = "-m pip install" ]; then
  report=""; list=""
  while [ $# -gt 0 ]; do
    case "$1" in --report) report="$2"; shift;; -r) list="$2"; shift;; esac; shift
  done
  if [ -n "$report" ]; then /bin/cat '` + filepath.Join(dir, "report.json") + `' > "$report"; exit $?; fi
  echo "installed:" >> '` + f.calls + `'; /bin/cat "$list" >> '` + f.calls + `'; exit 0
fi
exit 3
`
	writeExec(t, f.interp, python)
	if uv {
		writeExec(t, filepath.Join(f.bin, "uv"), `#!/bin/sh
echo "uv $* | UV_CACHE_DIR=$UV_CACHE_DIR UV_PROJECT_ENVIRONMENT=$UV_PROJECT_ENVIRONMENT UV_PYTHON_DOWNLOADS=$UV_PYTHON_DOWNLOADS UV_PYTHON=$UV_PYTHON VIRTUAL_ENV=$VIRTUAL_ENV" >> '`+f.calls+`'
case "$1" in
--version) echo "uv ${FAKE_UV_VERSION:-0.11.28}"; exit 0;;
python) echo '`+f.interp+`'; exit 0;;
sync) /bin/mkdir -p "$UV_PROJECT_ENVIRONMENT/bin" && echo 'home = /x' > "$UV_PROJECT_ENVIRONMENT/pyvenv.cfg" && /bin/ln -sf '`+f.interp+`' "$UV_PROJECT_ENVIRONMENT/bin/python"; exit $?;;
esac
exit 3
`)
	}
	return f
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (f fakePython) input(dir, deps string, extra ...string) WarmInput {
	return WarmInput{Dir: dir, Deps: deps, Environ: append([]string{"PATH=" + f.bin, "HOME=/nonexistent"}, extra...),
		Env: []string{"UV_CACHE_DIR=/data/cache/uv"}, Log: &bytes.Buffer{}, Timeout: time.Minute,
		Now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
}

func (f fakePython) log(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(f.calls)
	return string(data)
}

func readStamp(t *testing.T, venv string) venvStamp {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(venv), venvStampName))
	if err != nil {
		t.Fatal(err)
	}
	var s venvStamp
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

const clickPyproject = "[project]\nname = \"click\"\nrequires-python = \">=3.10\" # comment\n\n[dependency-groups]\ntests = [\"pytest\"]\n"

// uv: the interpreter uv finds (never downloading one) and a venv per inputs at <deps>/py/<key>/venv, synced with
// --frozen and nothing local installed, uv's downloads in the deps folder's uv-cache, stamped with what it was built
// from. The same inputs reuse it without running uv sync; a changed lock, interpreter or uv version gives another.
func TestWarmPythonWithUv(t *testing.T) {
	f := newFakePython(t, "3.12.13", true, "")
	deps, repo := t.TempDir(), t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPyproject, "uv.lock": "version = 1\n", "src/click/__init__.py": ""})
	ctx := context.Background()
	w, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(repo, deps))
	if err != nil || w.Failed != "" {
		t.Fatalf("%+v %v\n%s", w, err, f.log(t))
	}
	if filepath.Dir(filepath.Dir(w.Venv)) != filepath.Join(deps, "py") || filepath.Base(w.Venv) != "venv" || !VenvReady(w.Venv) {
		t.Fatalf("venv %q (ready %v)", w.Venv, VenvReady(w.Venv))
	}
	log := f.log(t)
	for _, want := range []string{"uv python find --system --no-project >=3.10 | ", "UV_PYTHON_DOWNLOADS=never",
		"uv sync --frozen --no-install-project --no-install-workspace --no-install-local --python " + f.interp + " | UV_CACHE_DIR=" + filepath.Join(deps, "uv-cache") +
			" UV_PROJECT_ENVIRONMENT=" + w.Venv + " UV_PYTHON_DOWNLOADS=never UV_PYTHON=" + f.interp + " VIRTUAL_ENV=" + w.Venv} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in\n%s", want, log)
		}
	}
	if strings.Contains(log, "pip install") {
		t.Errorf("pip ran for a uv project:\n%s", log)
	}
	s := readStamp(t, w.Venv)
	if s.Manager != "uv" || s.Tool != "uv 0.11.28" || s.Interpreter != f.interp || s.Version != "3.12.13" || !slices.Equal(s.Inputs, []string{"uv.lock", "pyproject.toml"}) || s.Recipe != pythonRecipe {
		t.Errorf("stamp %+v", s)
	}

	// The same inputs: the same venv, and uv sync does not run again.
	before := strings.Count(f.log(t), "uv sync")
	again, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(repo, deps))
	if err != nil || again.Venv != w.Venv || strings.Count(f.log(t), "uv sync") != before {
		t.Errorf("a second warm-up: %+v %v, syncs %d -> %d", again, err, before, strings.Count(f.log(t), "uv sync"))
	}
	// Another base with the same inputs shares it.
	other := t.TempDir()
	writeFiles(t, other, map[string]string{"pyproject.toml": clickPyproject, "uv.lock": "version = 1\n", "src/click/core.py": "changed code\n"})
	if shared, _ := WarmFuncs(ctx, Select([]string{"python"}), f.input(other, deps)); shared.Venv != w.Venv {
		t.Errorf("same inputs, another venv: %q", shared.Venv)
	}

	// The key changes with each input.
	changed := map[string]func() WarmInput{
		"the lock": func() WarmInput {
			d := t.TempDir()
			writeFiles(t, d, map[string]string{"pyproject.toml": clickPyproject, "uv.lock": "version = 1\n# another\n"})
			return f.input(d, deps)
		},
		"pyproject.toml": func() WarmInput {
			d := t.TempDir()
			writeFiles(t, d, map[string]string{"pyproject.toml": clickPyproject + "\n[tool.uv]\ndefault-groups = []\n", "uv.lock": "version = 1\n"})
			return f.input(d, deps)
		},
		"uv's version": func() WarmInput { return f.input(repo, deps, "FAKE_UV_VERSION=0.12.0") },
	}
	for name, in := range changed {
		got, err := WarmFuncs(ctx, Select([]string{"python"}), in())
		if err != nil || got.Venv == "" || got.Venv == w.Venv {
			t.Errorf("%s changed, venv %q (was %q): %v", name, got.Venv, w.Venv, err)
		}
	}
	g := newFakePython(t, "3.13.1", true, "")
	if got, _ := WarmFuncs(ctx, Select([]string{"python"}), g.input(repo, deps)); got.Venv == "" || got.Venv == w.Venv {
		t.Errorf("another interpreter, venv %q", got.Venv)
	}
}

// A venv without its stamp (a warm-up that died, or failed) is removed and built again; a stamp of another recipe does
// not count; a stamped venv whose interpreter is gone is not ready.
func TestWarmPythonRebuildsAnUnstampedVenv(t *testing.T) {
	f := newFakePython(t, "3.12.13", true, "")
	deps, repo := t.TempDir(), t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPyproject, "uv.lock": "version = 1\n"})
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, deps))
	if err != nil || w.Venv == "" {
		t.Fatal(w, err)
	}
	stamp := filepath.Join(filepath.Dir(w.Venv), venvStampName)
	leftover := filepath.Join(w.Venv, "lib", "half-installed")
	writeFiles(t, filepath.Dir(leftover), map[string]string{"half-installed": "x"})
	for name, damage := range map[string]func(){
		"no stamp":        func() { os.Remove(stamp) },
		"an older recipe": func() { os.WriteFile(stamp, []byte(`{"recipe":"python-0"}`), 0o600) },
		"a corrupt stamp": func() { os.WriteFile(stamp, []byte(`{`), 0o600) },
		"no pyvenv.cfg":   func() { os.Remove(filepath.Join(w.Venv, "pyvenv.cfg")) },
		"a dangling python": func() {
			os.Remove(filepath.Join(w.Venv, "bin", "python"))
			os.Symlink("/nonexistent/python", filepath.Join(w.Venv, "bin", "python"))
		},
	} {
		writeFiles(t, filepath.Dir(leftover), map[string]string{"half-installed": "x"})
		damage()
		if VenvReady(w.Venv) {
			t.Errorf("%s: still ready", name)
		}
		syncs := strings.Count(f.log(t), "uv sync")
		again, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, deps))
		if err != nil || again.Venv != w.Venv || !VenvReady(again.Venv) {
			t.Errorf("%s: %+v %v", name, again, err)
		}
		if strings.Count(f.log(t), "uv sync") != syncs+1 {
			t.Errorf("%s: not rebuilt", name)
		}
		if _, err := os.Stat(leftover); err == nil {
			t.Errorf("%s: the unfinished venv's files were kept", name)
		}
		if s := readStamp(t, again.Venv); s.Recipe != pythonRecipe {
			t.Errorf("%s: stamp %+v", name, s)
		}
	}
}

// pip without a lock file: a venv from the interpreter, one resolve of the requirement files (and those they include)
// and the project with its test extra, then the resolved set less local packages pinned, installed with --no-deps, and
// kept in the stamp; every run gets a note with the resolve's date. The report stays in the denied py-resolve folder.
func TestWarmPythonWithPip(t *testing.T) {
	report := `{"version":"1","install":[
 {"metadata":{"name":"more-itertools","version":"10.9.0"},"is_direct":true,"download_info":{"url":"file:///w/repo","dir_info":{}}},
 {"metadata":{"name":"subpkg","version":"0.1"},"is_direct":true,"download_info":{"url":"file:///elsewhere/subpkg","dir_info":{"editable":true}}},
 {"metadata":{"name":"coverage","version":"7.16.2"},"is_direct":false,"download_info":{"url":"https://files/coverage.whl","archive_info":{}}},
 {"metadata":{"name":"ruff","version":"0.16.10"},"is_direct":false,"download_info":{"url":"https://files/ruff.whl","archive_info":{}}},
 {"metadata":{"name":"helper","version":"1.0"},"is_direct":true,"download_info":{"url":"https://github.com/o/helper","vcs_info":{"vcs":"git","commit_id":"abc123"}}}]}`
	deps, repo := t.TempDir(), t.TempDir()
	// The project is the folder being warmed (the warm-up checkout); a path dependency is another folder.
	report = strings.Replace(report, "file:///w/repo", "file://"+repo, 1)
	f := newFakePython(t, "3.12.13", false, report)
	writeFiles(t, repo, map[string]string{
		"pyproject.toml":           "[project]\nname = \"more-itertools\"\nrequires-python = \">=3.10\"\n\n[project.optional-dependencies]\ntests = [\"coverage\"]\n",
		"requirements/testing.txt": "-r base.txt\ncoverage\nruff\n",
		"requirements/base.txt":    "helper @ git+https://github.com/o/helper\n",
		"requirements/docs.txt":    "sphinx\n",
		"requirements.txt":         "",
	})
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, deps, "PIP_INDEX_URL=https://user:secret@index"))
	if err != nil || w.Failed != "" || !VenvReady(w.Venv) {
		t.Fatalf("%+v %v\n%s", w, err, f.log(t))
	}
	log := f.log(t)
	report2 := filepath.Join(deps, "py-resolve", filepath.Base(filepath.Dir(w.Venv))+".json")
	for _, want := range []string{
		"python -m venv " + w.Venv,
		"python -m pip install --quiet --dry-run --ignore-installed --report " + report2 + " -r requirements.txt -r requirements/testing.txt .[tests] | VIRTUAL_ENV=" + w.Venv + " PIP_CACHE_DIR=" + filepath.Join(deps, "pip-cache") + " PYTHONPATH=",
		"python -m pip install --quiet --no-deps -r " + filepath.Join(deps, "py-resolve"),
		"installed:\ncoverage==7.16.2\nhelper @ git+https://github.com/o/helper@abc123\nruff==0.16.10\n",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in\n%s", want, log)
		}
	}
	if strings.Contains(log, "sphinx") || strings.Contains(log, "docs.txt") || strings.Contains(log, "more-itertools==") {
		t.Errorf("the docs requirements or the project itself went in:\n%s", log)
	}
	s := readStamp(t, w.Venv)
	if s.Manager != "pip" || s.Resolved != "2026-10-02" || !slices.Equal(s.Pinned, []string{"coverage==7.16.2", "helper @ git+https://github.com/o/helper@abc123", "ruff==0.16.10"}) ||
		!slices.Equal(s.Inputs, []string{"pyproject.toml", "requirements.txt", "requirements/testing.txt", "requirements/base.txt"}) {
		t.Errorf("stamp %+v", s)
	}
	wantNotes := []string{"local packages not installed (the checkout is on PYTHONPATH): subpkg",
		"no lock file: dependencies resolved at warm-up on 2026-10-02 (3 packages, pinned in the venv's stamp)"}
	if !slices.Equal(w.Notes, wantNotes) || !slices.Equal(s.Notes, wantNotes) {
		t.Errorf("notes %q, stamp %q", w.Notes, s.Notes)
	}
	// A reused venv repeats its notes, with the first resolve's date.
	again, _ := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, deps))
	if !slices.Equal(again.Notes, wantNotes) || strings.Count(f.log(t), "--dry-run") != 1 {
		t.Errorf("reuse: %+v", again)
	}
	// An included file is an input: changing it gives another venv.
	writeFiles(t, repo, map[string]string{"requirements/base.txt": "helper @ git+https://github.com/o/helper@v2\n"})
	if moved, _ := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, deps)); moved.Venv == w.Venv {
		t.Error("an included requirement file changed, and the venv did not")
	}
}

// Requirement files that pin every package of the resolve are a lock: no note about resolving.
func TestWarmPythonPinnedRequirementsAreALock(t *testing.T) {
	report := `{"install":[{"metadata":{"name":"Coverage","version":"7.16.2"},"download_info":{"url":"https://x","archive_info":{}}},
 {"metadata":{"name":"ruff","version":"0.16.10"},"download_info":{"url":"https://y","archive_info":{}}}]}`
	f := newFakePython(t, "3.12.13", false, report)
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"requirements-dev.txt": "coverage==7.16.2 ; python_version >= '3.8'\nRuff==0.16.10  # linter\n"})
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, t.TempDir()))
	if err != nil || w.Failed != "" || len(w.Notes) != 0 {
		t.Errorf("%+v %v\n%s", w, err, f.log(t))
	}
}

// Warm-ups that cannot be done are failures with a reason (the base is not stamped), never a download: no interpreter
// meets requires-python, uv.lock without uv, a requirement file including one outside the repository.
func TestWarmPythonFailures(t *testing.T) {
	ctx := context.Background()
	old := newFakePython(t, "3.9.6", false, "")
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPyproject})
	w, err := WarmFuncs(ctx, Select([]string{"python"}), old.input(repo, t.TempDir()))
	if err != nil || !strings.Contains(w.Failed, `meets requires-python ">=3.10"`) || w.Venv != "" {
		t.Errorf("an old interpreter: %+v %v", w, err)
	}
	locked := t.TempDir()
	writeFiles(t, locked, map[string]string{"pyproject.toml": clickPyproject, "uv.lock": ""})
	if w, err := WarmFuncs(ctx, Select([]string{"python"}), old.input(locked, t.TempDir())); err != nil || !strings.Contains(w.Failed, "no uv on PATH") {
		t.Errorf("uv.lock without uv: %+v %v", w, err)
	}
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"requirements.txt": "-r ../../shared.txt\n"})
	if _, err := WarmFuncs(ctx, Select([]string{"python"}), old.input(outside, t.TempDir())); err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("an include outside the repository: %v", err)
	}
	// A failed resolve leaves no stamp.
	failing := newFakePython(t, "3.12.13", false, "not json")
	deps := t.TempDir()
	writeFiles(t, outside, map[string]string{"requirements.txt": "x\n"})
	if _, err := WarmFuncs(ctx, Select([]string{"python"}), failing.input(outside, deps)); err == nil {
		t.Error("a report that is not JSON")
	}
	stamps, _ := filepath.Glob(filepath.Join(deps, "py", "*", venvStampName))
	if len(stamps) != 0 {
		t.Errorf("stamped after a failure: %q", stamps)
	}
}

func TestSatisfies(t *testing.T) {
	for _, c := range []struct {
		version, spec string
		want          bool
	}{
		{"3.12.13", ">=3.10", true}, {"3.9.6", ">=3.10", false}, {"3.12.13", "", true}, {"3.12.13", ">=3.8, <4", true},
		{"3.12.13", "<3.12", false}, {"3.12.0", "==3.12", true}, {"3.12.13", "==3.12.*", true}, {"3.13.0", "==3.12.*", false},
		{"3.12.13", "!=3.12.*", false}, {"3.11.2", "!=3.12.*", true}, {"3.11.2", "~=3.10", true}, {"4.0.0", "~=3.10", false},
		{"3.10.9", "~=3.10.2", true}, {"3.11.0", "~=3.10.2", false}, {"3.12.13", ">3.12", true}, {"3.12.0", ">3.12", false},
		{"3.12.13", "<=3.12.13", true},
	} {
		if got, err := satisfies(c.version, c.spec); err != nil || got != c.want {
			t.Errorf("satisfies(%q, %q) = %v, %v", c.version, c.spec, got, err)
		}
	}
	for _, spec := range []string{">=3.10.0rc1", "===3.12.13", "~=3", "<3.*", "3.12"} {
		if _, err := satisfies("3.12.13", spec); err == nil {
			t.Errorf("%q is checked, not refused", spec)
		}
	}
}

// The report's values become requirement lines only when they are plainly a name, a version, an address.
func TestPinnedFromReportRefusesOddValues(t *testing.T) {
	for _, report := range []string{
		`{"install":[{"metadata":{"name":"-r /etc/passwd","version":"1"},"download_info":{"url":"https://x"}}]}`,
		`{"install":[{"metadata":{"name":"ok","version":"1\n-e ."},"download_info":{"url":"https://x"}}]}`,
		`{"install":[{"metadata":{"name":"ok","version":"1"},"is_direct":true,"download_info":{"url":"https://x y"}}]}`,
		`{"install":[{"metadata":{"name":"ok","version":"1"},"is_direct":true,"download_info":{"url":""}}]}`,
	} {
		if pinned, _, err := pinnedFromReport([]byte(report), "/repo"); err == nil {
			t.Errorf("%s gave %q", report, pinned)
		}
	}
}

// requires-python is read from [project] only; extras and tables by name.
func TestPyprojectReading(t *testing.T) {
	data := []byte("[tool.x]\nrequires-python = \"<1\"\n[project]  # the project\nname = 'x'\nrequires-python = '>=3.11'\n" +
		"[project.optional-dependencies]\n\"testing\" = [\"pytest\"]\n")
	if got := tomlString(data, "project", "requires-python"); got != ">=3.11" {
		t.Errorf("requires-python %q", got)
	}
	if !tomlKey(data, "project.optional-dependencies", "testing") || tomlKey(data, "project.optional-dependencies", "test") || tomlTable(data, "dependency-groups") {
		t.Error("extras or tables misread")
	}
}
