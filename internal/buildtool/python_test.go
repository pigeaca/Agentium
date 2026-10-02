package buildtool

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"maps"
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
// layout) on PYTHONPATH, then the base's metadata folder, bytecode, uv's cache and hypothesis's database in the run's
// own cache, no pytest cache, pip and uv offline, uv in the venv without syncing. Without a venv the host's interpreter
// runs with the same settings; without a build cache no bytecode is written. Agentium's own commands give each
// checkout its own hypothesis database in the data folder.
func TestPythonEnv(t *testing.T) {
	repo := t.TempDir()
	meta := "/data/deps/1/py-meta/0123456789abcdef"
	ctx := AgentContext{Allowed: []string{"PATH=/usr/bin:/bin", "PYTHONPATH=/user/py"}, Home: "/home/u", Repo: repo,
		BuildCache: "/data/workspaces/r1/go-build", Deps: "/data/deps/1", Venv: "/data/deps/1/py/k/venv", Metadata: meta}
	got := AgentEnv(Select([]string{"python"}), ctx)
	want := []string{"GOFLAGS=-buildvcs=false", "VIRTUAL_ENV=/data/deps/1/py/k/venv", "PATH=/data/deps/1/py/k/venv/bin:/usr/bin:/bin",
		"PYTHONPATH=" + repo + ":" + meta, "PYTHONPYCACHEPREFIX=/data/workspaces/r1/go-build/pycache",
		"HYPOTHESIS_STORAGE_DIRECTORY=/data/workspaces/r1/go-build/hypothesis", "PYTEST_ADDOPTS=-p no:cacheprovider",
		"PIP_NO_INDEX=1", "PIP_DISABLE_PIP_VERSION_CHECK=1", "UV_OFFLINE=1", "UV_NO_SYNC=1", "UV_FROZEN=1", "UV_PYTHON_DOWNLOADS=never",
		"UV_PROJECT_ENVIRONMENT=/data/deps/1/py/k/venv", "UV_CACHE_DIR=/data/workspaces/r1/go-build/uv"}
	if !slices.Equal(got, want) {
		t.Errorf("agent env\n got %q\nwant %q", got, want)
	}
	// Agentium's own commands in a checkout: the same, with the data folder's cache, and a hypothesis database of the
	// checkout's own there (a grading never replays what another arm's grading found).
	checkout := CheckoutEnv(Select([]string{"python"}), AgentContext{Allowed: ctx.Allowed, Repo: repo, BuildCache: "/data/cache", Venv: ctx.Venv, Metadata: meta})
	if !slices.Contains(checkout, "PYTHONPYCACHEPREFIX=/data/cache/pycache") || !slices.Contains(checkout, "UV_CACHE_DIR=/data/cache/uv") ||
		!slices.Contains(checkout, "VIRTUAL_ENV="+ctx.Venv) || !slices.Contains(checkout, "PYTHONPATH="+repo+":"+meta) {
		t.Errorf("checkout env %q", checkout)
	}
	other := env(t, CheckoutEnv(Select([]string{"python"}), AgentContext{Repo: t.TempDir(), BuildCache: "/data/cache"}))
	if h := env(t, checkout)["HYPOTHESIS_STORAGE_DIRECTORY"]; !strings.HasPrefix(h, "/data/cache/hypothesis/") || h == other["HYPOTHESIS_STORAGE_DIRECTORY"] ||
		!strings.HasPrefix(other["HYPOTHESIS_STORAGE_DIRECTORY"], "/data/cache/hypothesis/") {
		t.Errorf("hypothesis's databases: %q and %q", h, other["HYPOTHESIS_STORAGE_DIRECTORY"])
	}
	if got := CheckoutEnv(Select([]string{"go", "maven", "gradle", "cargo"}), ctx); len(got) != 0 {
		t.Errorf("other tools set %q in their commands' checkouts", got)
	}

	// The src layout, decided from the base commit's paths (ImportRoot), not from the checkout on disk: a src/ folder the
	// agent adds later changes nothing.
	src := ctx
	src.ImportRoot = "src"
	if e := env(t, AgentEnv(Select([]string{"python"}), src)); e["PYTHONPATH"] != filepath.Join(repo, "src")+":"+meta {
		t.Errorf("src layout: PYTHONPATH=%q", e["PYTHONPATH"])
	}
	writeFiles(t, repo, map[string]string{"src/added/__init__.py": ""})
	if e := env(t, AgentEnv(Select([]string{"python"}), ctx)); e["PYTHONPATH"] != repo+":"+meta {
		t.Errorf("a src/ added in the checkout moved PYTHONPATH to %q", e["PYTHONPATH"])
	}
	for _, c := range []struct {
		paths []string
		want  string
	}{
		{[]string{"pyproject.toml", "src/click/__init__.py", "tests/test_x.py"}, "src"},
		{[]string{"src/mod.py"}, "src"},
		{[]string{"src/README.md", "src/data/x.json", "pkg/__init__.py"}, ""},
		{[]string{"src/a/b/__init__.py", "x/src/m.py"}, ""},
		{[]string{"src"}, ""}, // a link named src
	} {
		if got := ImportRoot(c.paths); got != c.want {
			t.Errorf("ImportRoot(%q) = %q, want %q", c.paths, got, c.want)
		}
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
		return strings.HasPrefix(kv, "PYTHONPYCACHEPREFIX=") || strings.HasPrefix(kv, "UV_CACHE_DIR=") || strings.HasPrefix(kv, "PYTHONPATH=") ||
			strings.HasPrefix(kv, "HYPOTHESIS_STORAGE_DIRECTORY=")
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
	for _, name := range []string{"pycache", "uv", "hypothesis"} {
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
// makes venvs, its pip (which writes report as its resolve's report, records what it installs, and builds a wheel),
// and, with uv, a uv that finds that interpreter, syncs a venv and builds a wheel. A build copies <dir>/<name>.whl into
// its output folder: $FAKE_WHEEL, else "default" (fakeWheel), or with FAKE_SCM "scm-" and the pretended version, so a
// build without the override fails; FAKE_BUILD_FAIL fails it, and FAKE_WHEEL=none succeeds with no wheel. Every call is logged to calls.
type fakePython struct {
	dir, bin, calls, interp string
}

// fakeMetadata is the METADATA of the fake's default wheel: headers with a continuation line, a Description header and
// a long description, which must not reach the metadata folder.
const fakeMetadata = "Metadata-Version: 2.4\r\nName: Fake.Project\r\nVersion: 1.2.3\r\nSummary: A fake\r\nLicense: MIT\r\n" +
	"        with a second line\r\nDescription: an old-style long\r\n        description\r\nRequires-Dist: dep>=1\r\n\r\n# Fake\r\n\r\nThe README, and a changelog.\r\n"

// fakeMetadataHeaders is what the metadata folder keeps of fakeMetadata.
const fakeMetadataHeaders = "Metadata-Version: 2.4\nName: Fake.Project\nVersion: 1.2.3\nSummary: A fake\nLicense: MIT\n" +
	"        with a second line\nRequires-Dist: dep>=1\n"

// writeWheel writes a wheel (a zip) holding files.
func writeWheel(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := slices.Sorted(maps.Keys(files))
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeWheel is a project's wheel of a version as a build makes it: its code, and a .dist-info with METADATA
// (fakeMetadata), RECORD, top_level.txt, entry_points.txt, WHEEL and a license, of which only METADATA's headers may
// reach the metadata folder.
func fakeWheel(t *testing.T, path, version string) {
	t.Helper()
	dist := "fake_project-" + version + ".dist-info/"
	writeWheel(t, path, map[string]string{"fake_project/__init__.py": "X = 'wheel'\n",
		dist + "METADATA": strings.Replace(fakeMetadata, "Version: 1.2.3", "Version: "+version, 1), dist + "RECORD": "fake_project/__init__.py,,\n",
		dist + "top_level.txt": "fake_project\n", dist + "WHEEL": "Wheel-Version: 1.0\n",
		dist + "entry_points.txt": "[console_scripts]\nfake = fake_project:main\n", dist + "licenses/LICENSE": "MIT\n"})
}

func newFakePython(t *testing.T, version string, uv bool, report string) fakePython {
	t.Helper()
	dir := t.TempDir()
	f := fakePython{dir: dir, bin: filepath.Join(dir, "bin"), calls: filepath.Join(dir, "calls.log")}
	f.interp = filepath.Join(f.bin, "python3")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeWheel(t, filepath.Join(dir, "default.whl"), "1.2.3")
	build := `build() {
  echo "build into $1 | SETUPTOOLS_SCM_PRETEND_VERSION=$SETUPTOOLS_SCM_PRETEND_VERSION PDM_BUILD_SCM_VERSION=$PDM_BUILD_SCM_VERSION" >> '` + f.calls + `'
  if [ -n "$FAKE_BUILD_FAIL" ]; then exit 1; fi
  src="${FAKE_WHEEL:-default}"; if [ -n "$FAKE_SCM" ]; then src="scm-$SETUPTOOLS_SCM_PRETEND_VERSION"; fi
  if [ "$src" = none ]; then exit 0; fi
  /bin/cp '` + dir + `'/"$src.whl" "$1/"; exit $?
}
`
	python := `#!/bin/sh
` + build + `echo "python $* | VIRTUAL_ENV=$VIRTUAL_ENV PIP_CACHE_DIR=$PIP_CACHE_DIR PYTHONPATH=$PYTHONPATH" >> '` + f.calls + `'
if [ "$1 $2 $3" = "-m pip wheel" ]; then
  while [ $# -gt 0 ]; do if [ "$1" = "--wheel-dir" ]; then build "$2"; fi; shift; done; exit 3
fi
case "$1" in
-I) if [ -n "$4" ] && [ "$4" = "$FAKE_MISSING" ]; then exit 1; fi; printf '%s\n%s\n' '` + version + `' '` + f.interp + `'; exit 0;;
--version) echo 'Python ` + version + `'; exit 0;;
esac
if [ "$1 $2 $3" = "-m pip --version" ]; then echo "pip ${FAKE_PIP:-25.0} from /x (python 3)"; exit 0; fi
if [ "$1 $2" = "-m venv" ]; then
  /bin/mkdir -p "$3/bin" && echo 'home = /x' > "$3/pyvenv.cfg" && /bin/ln -s '` + f.interp + `' "$3/bin/python"; exit $?
fi
if [ "$1 $2 $3" = "-m pip install" ]; then
  report=""; list=""
  while [ $# -gt 0 ]; do
    case "$1" in --report) report="$2"; shift;; -r) list="$2"; shift;; "pip>=22.2") echo "upgraded pip" >> '` + f.calls + `'; exit 0;; esac; shift
  done
  if [ -n "$report" ]; then /bin/cat '` + filepath.Join(dir, "report.json") + `' > "$report"; exit $?; fi
  echo "installed:" >> '` + f.calls + `'; /bin/cat "$list" >> '` + f.calls + `'; exit 0
fi
exit 3
`
	writeExec(t, f.interp, python)
	if uv {
		writeExec(t, filepath.Join(f.bin, "uv"), `#!/bin/sh
`+build+`echo "uv $* | UV_CACHE_DIR=$UV_CACHE_DIR UV_PROJECT_ENVIRONMENT=$UV_PROJECT_ENVIRONMENT UV_PYTHON_DOWNLOADS=$UV_PYTHON_DOWNLOADS UV_PYTHON=$UV_PYTHON VIRTUAL_ENV=$VIRTUAL_ENV" >> '`+f.calls+`'
case "$1" in
--version) echo "uv ${FAKE_UV_VERSION:-0.11.28}"; exit 0;;
python) echo '`+f.interp+`'; exit 0;;
build) while [ $# -gt 0 ]; do if [ "$1" = "--out-dir" ]; then build "$2"; fi; shift; done; exit 3;;
sync) /bin/mkdir -p "$UV_PROJECT_ENVIRONMENT/bin" "$UV_PROJECT_ENVIRONMENT/lib/python3.12/site-packages/dep" && echo 'x = 1' > "$UV_PROJECT_ENVIRONMENT/lib/python3.12/site-packages/dep/__init__.py" && echo 'home = /x' > "$UV_PROJECT_ENVIRONMENT/pyvenv.cfg" && /bin/ln -sf '`+f.interp+`' "$UV_PROJECT_ENVIRONMENT/bin/python"; exit $?;;
esac
exit 3
`)
	}
	return f
}

// depsDir is a deps folder for a test: stamped venvs are read-only, so it is made writable again before it is removed.
func depsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { setWritable(dir, true) })
	return dir
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
	deps, repo := depsDir(t), t.TempDir()
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
		"uv sync --frozen --no-install-project --no-install-workspace --no-install-local --python " + f.interp + " --no-install-package click | UV_CACHE_DIR=" + filepath.Join(deps, "uv-cache") +
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
// not count; a stamped venv whose interpreter is gone is not ready; nor is one whose imports changed after its stamp (a
// .pth or a package added, a package removed: the manifest). A stamped venv is read-only, and a rebuild copes with it:
// an unstamped one is removed, a stamped one (a run may be using it) is moved aside whole and rebuilt.
func TestWarmPythonRebuildsAnUnstampedVenv(t *testing.T) {
	f := newFakePython(t, "3.12.13", true, "")
	deps, repo := depsDir(t), t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPyproject, "uv.lock": "version = 1\n"})
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, deps))
	if err != nil || w.Venv == "" {
		t.Fatal(w, err)
	}
	stamp := filepath.Join(filepath.Dir(w.Venv), venvStampName)
	leftover := filepath.Join(w.Venv, "lib", "half-installed")
	site := filepath.Join(w.Venv, "lib", "python3.12", "site-packages")
	// Read-only once stamped, the stamp's folder too: a host-side command cannot change it by accident.
	for _, p := range []string{w.Venv, site, filepath.Join(site, "dep", "__init__.py"), stamp, filepath.Dir(stamp)} {
		if info, err := os.Lstat(p); err != nil || info.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s is writable after the stamp: %v %v", p, info, err)
		}
	}
	if err := os.WriteFile(filepath.Join(site, "evil.pth"), []byte("import os\n"), 0o644); err == nil {
		t.Error("a .pth could be written into a stamped venv")
	}
	asidesBefore := 0
	for name, damage := range map[string]func(){
		"a .pth planted":    func() { os.WriteFile(filepath.Join(site, "evil.pth"), []byte("import os\n"), 0o644) },
		"a package added":   func() { os.MkdirAll(filepath.Join(site, "planted"), 0o755) },
		"a package removed": func() { os.RemoveAll(filepath.Join(site, "dep")) },
		"a .pth rewritten":  func() { os.WriteFile(filepath.Join(site, "_virtualenv.pth"), []byte("import x\n"), 0o644) },
		"no stamp":          func() { os.Remove(stamp) },
		"an older recipe":   func() { os.WriteFile(stamp, []byte(`{"recipe":"python-0"}`), 0o600) },
		"a corrupt stamp":   func() { os.WriteFile(stamp, []byte(`{`), 0o600) },
		"no pyvenv.cfg":     func() { os.Remove(filepath.Join(w.Venv, "pyvenv.cfg")) },
		// A crash while moving it aside leaves its folder writable: made again, never handed out half-guarded.
		"its folder writable":      func() {},
		"site-packages writable":   func() {},
		"the venv folder writable": func() {},
		"a dangling python": func() {
			os.Remove(filepath.Join(w.Venv, "bin", "python"))
			os.Symlink("/nonexistent/python", filepath.Join(w.Venv, "bin", "python"))
		},
	} {
		if err := setWritable(filepath.Dir(w.Venv), true); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, filepath.Dir(leftover), map[string]string{"half-installed": "x"})
		damage()
		// The venv's folder read-only again, as a stamped one is: moving it aside must cope (macOS asks for write
		// permission on a folder that is renamed).
		// Everything read-only again, as warmPython left it (so only the damage tells), then the modes some cases open.
		if err := setWritable(filepath.Dir(w.Venv), false); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "its folder writable":
			os.Chmod(filepath.Dir(w.Venv), 0o755)
		case "site-packages writable":
			os.Chmod(site, 0o755)
		case "the venv folder writable":
			os.Chmod(w.Venv, 0o755)
		}
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
		// A stamped venv (any case but "no stamp") may be in use by a run: moved aside whole, never removed in place.
		asides, _ := filepath.Glob(filepath.Dir(w.Venv) + ".bad-*")
		if wantAside := name != "no stamp"; wantAside != (len(asides) > asidesBefore) {
			t.Errorf("%s: moved aside %v (folders %q)", name, len(asides) > asidesBefore, asides)
		}
		asidesBefore = len(asides)
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
	deps, repo := depsDir(t), t.TempDir()
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
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, depsDir(t)))
	if err != nil || w.Failed != "" || len(w.Notes) != 0 {
		t.Errorf("%+v %v\n%s", w, err, f.log(t))
	}
}

// Warm-ups that cannot be done are failures with a reason (the base is not stamped, the run goes on), never a download
// and never an error that stops the run: no interpreter meets requires-python, uv.lock without uv, a requirement file
// including one outside the repository, a report that is not pip's.
func TestWarmPythonFailures(t *testing.T) {
	ctx := context.Background()
	old := newFakePython(t, "3.9.6", false, "")
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPyproject})
	w, err := WarmFuncs(ctx, Select([]string{"python"}), old.input(repo, depsDir(t)))
	if err != nil || !strings.Contains(w.Failed, `meets requires-python ">=3.10"`) || w.Venv != "" {
		t.Errorf("an old interpreter: %+v %v", w, err)
	}
	locked := t.TempDir()
	writeFiles(t, locked, map[string]string{"pyproject.toml": clickPyproject, "uv.lock": ""})
	if w, err := WarmFuncs(ctx, Select([]string{"python"}), old.input(locked, depsDir(t))); err != nil || !strings.Contains(w.Failed, "no uv on PATH") {
		t.Errorf("uv.lock without uv: %+v %v", w, err)
	}
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"requirements.txt": "-r ../../shared.txt\n"})
	if w, err := WarmFuncs(ctx, Select([]string{"python"}), old.input(outside, depsDir(t))); err != nil || !strings.Contains(w.Failed, "outside the repository") {
		t.Errorf("an include outside the repository: %+v %v", w, err)
	}
	// A failed resolve leaves no stamp.
	failing := newFakePython(t, "3.12.13", false, "not json")
	deps := depsDir(t)
	writeFiles(t, outside, map[string]string{"requirements.txt": "x\n"})
	if w, err := WarmFuncs(ctx, Select([]string{"python"}), failing.input(outside, deps)); err != nil || !strings.Contains(w.Failed, "pip's report") {
		t.Errorf("a report that is not JSON: %+v %v", w, err)
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
		if pinned, _, _, err := pinnedFromReport([]byte(report), "/repo", nil); err == nil {
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

// The files where pip, uv, poetry and keyring keep index settings and credentials are denied in every project, at
// their defaults, under XDG folders and where variables name them, and the machine's own. uv's interpreters, beside its
// credentials, stay readable.
func TestPythonCredentialFilesAreDenied(t *testing.T) {
	home := "/home/u"
	got := UserCaches([]string{"XDG_CONFIG_HOME=/x/config", "XDG_DATA_HOME=/x/data", "PIP_CONFIG_FILE=/etc/custom/pip.conf",
		"UV_CONFIG_FILE=/opt/uv.toml", "NETRC=/secrets/netrc", "POETRY_CONFIG_DIR=/x/poetry", "UV_CREDENTIALS_DIR=/x/uvcreds"}, home)
	for _, want := range []string{"/home/u/.config/pip", "/home/u/Library/Application Support/pip", "/home/u/.pip",
		"/home/u/.config/uv/uv.toml", "/home/u/.local/share/uv/credentials", "/home/u/.config/pypoetry/auth.toml",
		"/home/u/Library/Application Support/pypoetry/auth.toml", "/home/u/.local/share/python_keyring", "/x/config/pip",
		"/x/config/uv/uv.toml", "/x/config/pypoetry/auth.toml", "/x/data/uv/credentials", "/x/data/python_keyring",
		"/x/poetry/auth.toml", "/etc/custom/pip.conf", "/opt/uv.toml", "/secrets/netrc", "/x/uvcreds"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not denied", want)
		}
	}
	for _, never := range []string{"/home/u/.local/share/uv", "/home/u/.local/share/uv/python", "/home/u/.config"} {
		if slices.Contains(got, never) {
			t.Errorf("%s is denied", never)
		}
	}
	// Machine-wide configs, whether they exist or not (the list does not depend on the machine).
	for _, want := range []string{"/etc/pip.conf", "/etc/xdg/pip/pip.conf", "/Library/Application Support/pip/pip.conf", "/etc/uv/uv.toml"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not denied", want)
		}
	}
	// A variable naming the home folder (or above) denies nothing of it.
	if slices.Contains(pythonCredentials(map[string]string{"NETRC": "/home/u"}, home), "/home/u") {
		t.Error("NETRC=/home/u denies the home folder")
	}
}

// Agentium's own commands in a Python checkout lose the user's Python, pip and uv settings, as the agent does; other
// tools' commands keep the environment as it is.
func TestCheckoutEnvironDropsPythonSettings(t *testing.T) {
	user := []string{"PATH=/bin", "HOME=/h", "PYTHONHOME=/py", "PYTHONOPTIMIZE=2", "PYTHONWARNINGS=ignore", "PYTHONHASHSEED=1",
		"PYTHONSAFEPATH=1", "PIP_INDEX_URL=https://x", "UV_INDEX=y", "VIRTUAL_ENV=/v", "VIRTUAL_ENV_PROMPT=p", "GOFLAGS=-x"}
	if got := CheckoutEnviron(Select([]string{"python"}), user); !slices.Equal(got, []string{"PATH=/bin", "HOME=/h", "VIRTUAL_ENV_PROMPT=p", "GOFLAGS=-x"}) {
		t.Errorf("python: %q", got)
	}
	if got := CheckoutEnviron(Select([]string{"maven", "cargo"}), user); !slices.Equal(got, user) {
		t.Errorf("other tools: %q", got)
	}
}

// Permanent failures (no interpreter, uv.lock without uv, an input that cannot be keyed) are not transient, so the
// base is stamped with the note; failures a download may cause are transient. An old pip is upgraded for --report.
func TestWarmPythonFailureKindsAndOldPip(t *testing.T) {
	ctx := context.Background()
	old := newFakePython(t, "3.9.6", false, "")
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPyproject, "uv.lock": ""})
	if w, _ := WarmFuncs(ctx, Select([]string{"python"}), old.input(repo, depsDir(t))); w.Failed == "" || w.Transient {
		t.Errorf("uv.lock without uv: %+v", w)
	}
	os.Remove(filepath.Join(repo, "uv.lock"))
	if w, _ := WarmFuncs(ctx, Select([]string{"python"}), old.input(repo, depsDir(t))); w.Failed == "" || w.Transient {
		t.Errorf("no interpreter: %+v", w)
	}
	// pip 21 in the fresh venv: upgraded first; a resolve that fails is transient.
	report := `{"install":[{"metadata":{"name":"ruff","version":"0.16.10"},"download_info":{"url":"https://y","archive_info":{}}}]}`
	f := newFakePython(t, "3.12.13", false, report)
	pip := t.TempDir()
	writeFiles(t, pip, map[string]string{"requirements.txt": "ruff\n"})
	w, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(pip, depsDir(t), "FAKE_PIP=21.1.3"))
	if err != nil || w.Failed != "" || !strings.Contains(f.log(t), "upgraded pip") {
		t.Errorf("an old pip: %+v %v\n%s", w, err, f.log(t))
	}
	g := newFakePython(t, "3.12.13", false, report)
	if w, _ := WarmFuncs(ctx, Select([]string{"python"}), g.input(pip, depsDir(t))); w.Failed != "" || strings.Contains(g.log(t), "upgraded pip") {
		t.Errorf("a current pip was upgraded: %+v", w)
	}
	broken := newFakePython(t, "3.12.13", false, report)
	os.Remove(filepath.Join(filepath.Dir(broken.calls), "report.json")) // the resolve's cat fails
	if w, _ := WarmFuncs(ctx, Select([]string{"python"}), broken.input(pip, depsDir(t))); w.Failed == "" || !w.Transient {
		t.Errorf("a failed resolve: %+v", w)
	}
}

// A test dependency that depends on the project pulls its released copy: it is never pinned nor installed, with a
// note; the project's names come from pyproject.toml, setup.cfg and the report's own entry.
func TestWarmPythonNeverInstallsTheProjectsReleasedCopy(t *testing.T) {
	report := `{"install":[
 {"metadata":{"name":"my-lib","version":"1.0"},"is_direct":true,"download_info":{"url":"file:///REPO","dir_info":{}}},
 {"metadata":{"name":"pytest-mylib","version":"2.0"},"download_info":{"url":"https://x","archive_info":{}}},
 {"metadata":{"name":"My_Lib","version":"0.9"},"download_info":{"url":"https://y","archive_info":{}}},
 {"metadata":{"name":"other-name","version":"3"},"download_info":{"url":"https://z","archive_info":{}}}]}`
	repo := t.TempDir()
	report = strings.Replace(report, "/REPO", repo, 1)
	pinned, skipped, own, err := pinnedFromReport([]byte(report), repo, []string{"other-name"})
	if err != nil || !slices.Equal(pinned, []string{"pytest-mylib==2.0"}) || len(skipped) != 0 || !slices.Equal(own, []string{"My_Lib", "other-name"}) {
		t.Errorf("pinned %q, skipped %q, own %q, %v", pinned, skipped, own, err)
	}
	f := newFakePython(t, "3.12.13", false, strings.Replace(report, repo, "/elsewhere", 1))
	writeFiles(t, repo, map[string]string{"setup.cfg": "[metadata]\nname = my.lib\n", "requirements-test.txt": "pytest-mylib\n"})
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, depsDir(t)))
	if err != nil || w.Failed != "" || !slices.ContainsFunc(w.Notes, func(n string) bool { return strings.Contains(n, "a released copy of the project (My_Lib)") }) ||
		strings.Contains(f.log(t), "My_Lib==") {
		t.Errorf("%+v %v\n%s", w, err, f.log(t))
	}
}

// pip's inputs: the dev extra when there is no test extra; setuptools' dynamic files are part of the key; a project
// with setup.py is warned that its key may miss inputs; a poetry-only pyproject is still a project to resolve.
func TestPythonPipInputs(t *testing.T) {
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{
		"pyproject.toml": "[build-system]\nrequires = [\"setuptools\"]\n[project]\nname = \"p\"\ndynamic = [\"dependencies\"]\n" +
			"[project.optional-dependencies]\ndocs = [\"x\"]\ndev = [\"pytest\"]\n" +
			"[tool.setuptools.dynamic]\ndependencies = {file = [\"requirements/base.in\"]}\n" +
			"[tool.setuptools.dynamic.optional-dependencies]\nextra = { file = \"extra.txt\" }\n",
		"requirements/base.in": "click\n", "extra.txt": "rich\n", "setup.py": "from setuptools import setup; setup()\n"})
	in, err := readPyInputs(repo)
	if err != nil || in.project != ".[dev]" || !slices.Contains(in.files, "requirements/base.in") || !slices.Contains(in.files, "extra.txt") ||
		!slices.ContainsFunc(in.notes, func(n string) bool { return strings.Contains(n, "setup.py may read files") }) ||
		!slices.Equal(in.names, []string{"p"}) {
		t.Errorf("%+v %v", in, err)
	}
	k1, _ := venvKey(repo, in, "", "/py", "3.12.0")
	writeFiles(t, repo, map[string]string{"requirements/base.in": "click>=8\n"})
	if k2, _ := venvKey(repo, in, "", "/py", "3.12.0"); k1 == k2 {
		t.Error("a dynamic dependencies file changed, and the key did not")
	}
	writeFiles(t, repo, map[string]string{"pyproject.toml": "[tool.setuptools.dynamic]\ndependencies = {file = [\"../outside.txt\"]}\n"})
	if _, err := readPyInputs(repo); err == nil {
		t.Error("a dynamic file outside the repository")
	}
	poetry := t.TempDir()
	writeFiles(t, poetry, map[string]string{"pyproject.toml": "[tool.poetry]\nname = \"Poetic\"\n[build-system]\nrequires = [\"poetry-core\"]\n"})
	if in, err := readPyInputs(poetry); err != nil || in.project != "." || !slices.Equal(in.names, []string{"poetic"}) {
		t.Errorf("poetry: %+v %v", in, err)
	}
}

// A runner the verification uses but the venv lacks is a note; one it has, or one the verification does not use, is not.
func TestMissingRunners(t *testing.T) {
	f := newFakePython(t, "3.12.13", true, "")
	deps, repo := depsDir(t), t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPyproject, "uv.lock": "version = 1\n"})
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, deps))
	if err != nil || w.Venv == "" {
		t.Fatal(w, err)
	}
	ctx := context.Background()
	if got := MissingRunners(ctx, w.Venv, []string{"uv run pytest -q"}, []string{"FAKE_MISSING=pytest"}); len(got) != 1 || !strings.Contains(got[0], "pytest is not installed") {
		t.Errorf("missing pytest: %q", got)
	}
	if got := MissingRunners(ctx, w.Venv, []string{"uv run pytest -q"}, nil); len(got) != 0 {
		t.Errorf("installed pytest: %q", got)
	}
	if got := MissingRunners(ctx, w.Venv, []string{"python -m unittest"}, []string{"FAKE_MISSING=pytest"}); len(got) != 0 {
		t.Errorf("unittest needs no runner: %q", got)
	}
}

// Stamps are written whole, through a synced temporary file renamed over the old one, and nothing is left beside them.
func TestWriteFileSynced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stamp")
	for _, data := range []string{`{"venv":"/a"}`, `{"venv":"/b"}`, ""} {
		if err := WriteFileSynced(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != data {
			t.Errorf("%q, %v; want %q", got, err, data)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left beside the stamp: %v", entries)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	if err := WriteFileSynced(filepath.Join(dir, "missing", "stamp"), nil, 0o600); err == nil {
		t.Error("a stamp in a missing folder")
	}
}
