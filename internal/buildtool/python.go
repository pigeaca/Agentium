package buildtool

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/runner"
)

// pythonProfile holds Python's special cases (pytest or unittest; uv or pip).
//
// Offline (proved in real sessions on pallets/click with uv and more-itertools with pip; see the Python and TypeScript
// plan, step 1): the run's warm-up finds an interpreter on the host that meets requires-python (never downloading one)
// and builds a venv of the project's dependencies only, never the project itself, at <deps>/py/<key>/venv (warmPython).
// The agent gets that venv (VIRTUAL_ENV, its bin first on PATH, uv pointed at it with no sync), its checkout on
// PYTHONPATH (src/ when it holds the code), bytecode in its run's own cache (PYTHONPYCACHEPREFIX) and no pytest cache,
// and pip and uv offline. The sandbox keeps the venv read-only, so one venv serves every run of every base with the same
// dependency inputs.
//
// Why the project is never installed: an editable install points at the throwaway warm-up checkout (which agents may
// not read: the import fails, or with the folder readable the agent's tests would silently import the base's code, not
// its edits), and a regular one would put a later base's code, holding earlier tasks' reference solutions, into a venv
// that bases share. The checkout on PYTHONPATH, which precedes site-packages, imports the agent's own code.
//
// Environment: the allowlist passes no PYTHON*, PIP_*, UV_* or VIRTUAL_ENV for any project (a user's PIP_INDEX_URL may
// carry a token; PYTHONPATH or UV_CACHE_DIR could point the agent at other code or at the user's caches); this profile
// sets its own. Agentium's own commands (the task's setup, grading) use the same venv (CheckoutEnv) with their caches in
// the data folder; validation, which runs before any warm-up, gets only those caches.
func pythonProfile() Profile {
	return Profile{
		Name:   "python",
		Detect: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "uv.lock"},
		// Discovery reads the files to tell a pytest project (internal/project): a marker alone does not say which runner
		// the tests use, so this proposes nothing.
		TestCommand: func(func(string) bool) string { return "" },
		Languages:   []string{"python"},
		Runners:     []string{"pytest", "unittest", "tox", "nox"},
		// Patterns (a "*" matches within one folder) as checkFiles reads them: requirement files are many.
		Configs: []string{"pyproject.toml", "uv.lock", "requirements*.txt", "requirements/*.txt", "setup.cfg", "setup.py", "tox.ini",
			"pytest.ini", "conftest.py", "noxfile.py", ".python-version"},
		// pytest however started (`uv run pytest`, `.venv/bin/pytest`), `python -m pytest|unittest`, tox and nox.
		TestPatterns: []string{`pytest`, `python[0-9.]* -m (pytest|unittest)`, `tox`, `nox`},
		// Agentium's own commands: what uv, pip and Python cache stays in the data folder (macOS's pip ignores
		// XDG_CACHE_HOME), and so does bytecode, which would otherwise land beside the checkout's sources.
		CommandCaches: []CacheVar{{Name: "UV_CACHE_DIR", Dir: "uv"}, {Name: "PIP_CACHE_DIR", Dir: "pip"},
			{Name: "PYTHONPYCACHEPREFIX", Dir: "pycache"}},
		AgentEnv:    pythonEnv,
		CheckoutEnv: pythonEnv,
		WarmRecipe:  pythonRecipe,
		WarmFunc:    warmPython,
		PrepareRun:  preparePythonRun,
		UserCaches:  pythonCaches,
	}
}

// pythonRecipe is the warm-up's recipe, part of WarmVersion and of every venv's key: changing what warmPython does
// changes it, so bases and venvs made by an earlier recipe are made again.
const pythonRecipe = "python-1: interpreter by `uv python find --system --no-project <requires-python>` (UV_PYTHON_DOWNLOADS=never) " +
	"or python3 on PATH; venv <deps>/py/<key>/venv, stamped; uv: uv sync --frozen --no-install-project --no-install-workspace " +
	"--no-install-local; pip: venv, then pip install --dry-run --ignore-installed --report, the set less local packages pinned " +
	"and installed with --no-deps\n"

// pythonPrivate are the deps folder's Python folders agents may not read (DepsDenied): uv's and pip's download caches
// and the resolve reports. Agents read only <deps>/py.
var pythonPrivate = []string{"uv-cache", "pip-cache", "py-resolve"}

// pythonEnv is the environment of a command that tests a Python project in c.Repo: the agent's (c.BuildCache the run's
// own cache) or one of Agentium's own commands (c.BuildCache the data folder's cache root). With a venv (c.Venv) it is
// active and first on PATH, and uv runs in it without syncing. Without one (no deps folder, or a warm-up that failed)
// the host's interpreter runs, with the same offline and cache settings.
func pythonEnv(c AgentContext) []string {
	var env []string
	if c.Venv != "" {
		path := filepath.Join(c.Venv, "bin")
		if p := vars(c.Allowed)["PATH"]; p != "" {
			path += string(os.PathListSeparator) + p
		}
		env = append(env, "VIRTUAL_ENV="+c.Venv, "PATH="+path)
	}
	if c.Repo != "" {
		env = append(env, "PYTHONPATH="+importRoot(c.Repo))
	}
	if c.BuildCache != "" {
		env = append(env, "PYTHONPYCACHEPREFIX="+filepath.Join(c.BuildCache, "pycache"))
	} else {
		env = append(env, "PYTHONDONTWRITEBYTECODE=1")
	}
	// No .pytest_cache in the checkout; pip and uv never reach an index (the sandbox has no network anyway, and uv run
	// would otherwise try to build and install the project).
	env = append(env, "PYTEST_ADDOPTS=-p no:cacheprovider", "PIP_NO_INDEX=1", "PIP_DISABLE_PIP_VERSION_CHECK=1",
		"UV_OFFLINE=1", "UV_NO_SYNC=1", "UV_FROZEN=1", "UV_PYTHON_DOWNLOADS=never")
	if c.Venv != "" {
		env = append(env, "UV_PROJECT_ENVIRONMENT="+c.Venv)
	}
	if c.BuildCache != "" {
		env = append(env, "UV_CACHE_DIR="+filepath.Join(c.BuildCache, "uv"))
	}
	return env
}

// importRoot is the folder the project's code imports from: <repo>/src when it is a real folder holding a package or a
// module (the src layout), else the repository itself. Links are not followed.
func importRoot(repo string) string {
	src := filepath.Join(repo, "src")
	if info, err := os.Lstat(src); err != nil || !info.IsDir() {
		return repo
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return repo
	}
	for _, e := range entries {
		switch {
		case e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".py"):
			return src
		case e.IsDir():
			if info, err := os.Lstat(filepath.Join(src, e.Name(), "__init__.py")); err == nil && info.Mode().IsRegular() {
				return src
			}
		}
	}
	return repo
}

// preparePythonRun makes the run's own bytecode and uv folders, which the agent's environment names.
func preparePythonRun(_ context.Context, _, buildCache string) error {
	if buildCache == "" {
		return nil
	}
	for _, name := range []string{"pycache", "uv"} {
		if err := os.MkdirAll(filepath.Join(buildCache, name), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// pythonCaches are the user's Python caches, which may hold the project's own builds, sdists included (click's sdist
// ships its tests): uv's, pip's and poetry's, at their defaults (macOS and Linux), under XDG_CACHE_HOME, and where the
// user's variables put them; and the user's active venv (VIRTUAL_ENV), which may hold the project installed from a
// later commit. Never denied: the interpreters uv manages (~/.local/share/uv/python), which venvs link to.
func pythonCaches(environ []string, home string) []string {
	env := vars(environ)
	paths := []string{filepath.Join(home, ".cache", "uv"), filepath.Join(home, ".cache", "pip"), filepath.Join(home, "Library", "Caches", "pip"),
		filepath.Join(home, ".cache", "pypoetry"), filepath.Join(home, "Library", "Caches", "pypoetry")}
	if xdg := env["XDG_CACHE_HOME"]; filepath.IsAbs(xdg) {
		paths = append(paths, filepath.Join(xdg, "uv"), filepath.Join(xdg, "pip"), filepath.Join(xdg, "pypoetry"))
	}
	for _, name := range []string{"UV_CACHE_DIR", "PIP_CACHE_DIR", "POETRY_CACHE_DIR"} {
		if v := env[name]; filepath.IsAbs(v) {
			paths = append(paths, v)
		}
	}
	// A venv, not the home folder or a folder above it (a misconfigured variable would deny everything).
	if v := filepath.Clean(env["VIRTUAL_ENV"]); filepath.IsAbs(v) && !inside(home, v) {
		paths = append(paths, v)
	}
	return paths
}

// inside reports whether p is root or inside it.
func inside(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && filepath.IsLocal(rel)
}

// venvStamp is the file in <deps>/py/<key> that says its venv is complete, written last: a venv without it is a
// warm-up that died or failed, removed and built again. It records what the venv was built from.
type venvStamp struct {
	Recipe      string   `json:"recipe"`
	Manager     string   `json:"manager"` // uv or pip
	Tool        string   `json:"tool,omitempty"`
	Interpreter string   `json:"interpreter"`
	Version     string   `json:"version"`
	Inputs      []string `json:"inputs"`
	Pinned      []string `json:"pinned,omitempty"` // pip without a lock file: what the resolve chose, installed as is
	Resolved    string   `json:"resolved,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

const venvStampName = "stamp.json"

// VenvReady reports whether venv is a complete venv of the deps folder whose interpreter still exists: its stamp is
// there, of the current recipe, and bin/python resolves. A run's stamp naming a venv that is not ready is warmed again.
func VenvReady(venv string) bool {
	if venv == "" {
		return false
	}
	if _, ok := readVenvStamp(filepath.Dir(venv)); !ok {
		return false
	}
	return venvMade(venv)
}

// pyInputs are what decides a venv's contents, read from the warm-up checkout.
type pyInputs struct {
	manager  string   // uv (uv.lock) or pip
	files    []string // relative paths hashed into the key
	reqFiles []string // pip: the requirement files given to the resolve
	project  string   // pip: "." or ".[extra]" when the checkout is an installable project (its dependencies); "" if not
	spec     string   // requires-python, "" when unstated
	notes    []string
}

// warmPython builds the base's venv, or finds it built: see pythonProfile. It runs under the warm-up lock (the run's),
// which every warm-up of the project's deps folder takes, so no two build one venv; a venv never changes after its
// stamp, while agents read it. Nothing of the project is built or imported beyond what pip needs to read its
// dependencies, and no test runs (a later base holds earlier tasks' hidden tests as ordinary tests).
func warmPython(ctx context.Context, in WarmInput) (Warmed, error) {
	// What the repository's files make impossible is a note (the run goes on without a venv), not an error.
	inputs, err := readPyInputs(in.Dir)
	if err != nil {
		return Warmed{Failed: err.Error()}, nil
	}
	path := vars(in.Environ)["PATH"]
	uv := lookPath("uv", path)
	if inputs.manager == "uv" && uv == "" {
		return Warmed{Failed: "uv.lock found, but no uv on PATH: install uv (Agentium installs no tools)"}, nil
	}
	interp, version, failed, err := findInterpreter(ctx, in, uv, inputs.spec)
	if err != nil || failed != "" {
		return Warmed{Failed: failed}, err
	}
	tool := ""
	if inputs.manager == "uv" {
		out, ok, err := pyProbe(ctx, in, []string{uv, "--version"}, nil)
		if err != nil {
			return Warmed{}, err
		}
		if !ok {
			return Warmed{Failed: "uv --version failed: see setup.log"}, nil
		}
		tool = strings.TrimSpace(out)
	}
	key, err := venvKey(in.Dir, inputs, tool, interp, version)
	if err != nil {
		return Warmed{Failed: err.Error()}, nil
	}
	root := filepath.Join(in.Deps, "py", key)
	venv := filepath.Join(root, "venv")
	if stamp, ok := readVenvStamp(root); ok && venvMade(venv) {
		return Warmed{Venv: venv, Notes: stamp.Notes}, nil
	}
	// Unstamped: a warm-up that died or failed. Nothing reads it (only stamped venvs reach runs), so it is rebuilt from
	// nothing, in place: a venv's scripts name its folder, so it cannot be built elsewhere and moved.
	if err := os.RemoveAll(root); err != nil {
		return Warmed{}, fmt.Errorf("remove an unfinished venv: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Warmed{}, err
	}
	stamp := venvStamp{Recipe: pythonRecipe, Manager: inputs.manager, Tool: tool, Interpreter: interp, Version: version,
		Inputs: inputs.files, Notes: inputs.notes}
	// The user's own settings must not move the venv or what goes into it; their index settings stay (private packages).
	env := append(slices.Clone(in.Env), "VIRTUAL_ENV="+venv, "PYTHONPATH=", "PYTHONHOME=", "PYTHONNOUSERSITE=1", "PIP_USER=0",
		"PIP_TARGET=", "PIP_PREFIX=", "PIP_ROOT=", "PIP_NO_INPUT=1", "PIP_DISABLE_PIP_VERSION_CHECK=1",
		"PIP_CACHE_DIR="+filepath.Join(in.Deps, "pip-cache"), "UV_CACHE_DIR="+filepath.Join(in.Deps, "uv-cache"),
		"UV_PROJECT_ENVIRONMENT="+venv, "UV_PYTHON="+interp, "UV_PYTHON_DOWNLOADS=never")
	switch inputs.manager {
	case "uv":
		ok, err := pyRun(ctx, in, []string{uv, "sync", "--frozen", "--no-install-project", "--no-install-workspace", "--no-install-local",
			"--python", interp}, env)
		if err != nil || !ok {
			return Warmed{Failed: "uv sync failed: see setup.log"}, err
		}
	default:
		pinned, notes, failed, err := pipVenv(ctx, in, inputs, interp, venv, key, env)
		if err != nil || failed != "" {
			return Warmed{Failed: failed}, err
		}
		stamp.Pinned, stamp.Notes = pinned, append(stamp.Notes, notes...)
		stamp.Resolved = in.Now.Format("2006-01-02")
	}
	if !venvMade(venv) {
		return Warmed{Failed: "the venv was not made (no bin/python or pyvenv.cfg): see setup.log"}, nil
	}
	if err := writeVenvStamp(root, stamp); err != nil {
		return Warmed{}, err
	}
	return Warmed{Venv: venv, Notes: stamp.Notes}, nil
}

// venvMade is VenvReady before the stamp is written: the venv's own files.
func venvMade(venv string) bool {
	if info, err := os.Lstat(filepath.Join(venv, "pyvenv.cfg")); err != nil || !info.Mode().IsRegular() {
		return false
	}
	info, err := os.Stat(filepath.Join(venv, "bin", "python")) // through the link to the interpreter
	return err == nil && info.Mode().IsRegular()
}

// pipVenv makes a venv with pip, without a lock file (the user's decision): one resolve of the requirement files and
// the project's dependencies (pip's report), then the resolved set less every local package (the project itself, a
// path dependency) pinned and installed with --no-deps. The pinned set goes into the stamp, and every run of it gets a
// note, unless the requirement files already pin every package (they are a lock).
func pipVenv(ctx context.Context, in WarmInput, inputs pyInputs, interp, venv, key string, env []string) (pinned, notes []string, failed string, err error) {
	if ok, err := pyRun(ctx, in, []string{interp, "-m", "venv", venv}, env); err != nil || !ok {
		return nil, nil, "python -m venv failed: see setup.log", err
	}
	if len(inputs.reqFiles) == 0 && inputs.project == "" {
		return nil, []string{"no requirement files and no project dependencies: the venv holds only pip"}, "", nil
	}
	private := filepath.Join(in.Deps, "py-resolve")
	if err := os.MkdirAll(private, 0o700); err != nil {
		return nil, nil, "", err
	}
	report := filepath.Join(private, key+".json")
	python := filepath.Join(venv, "bin", "python")
	args := []string{python, "-m", "pip", "install", "--quiet", "--dry-run", "--ignore-installed", "--report", report}
	for _, f := range inputs.reqFiles {
		args = append(args, "-r", f)
	}
	if inputs.project != "" {
		args = append(args, inputs.project)
	}
	if ok, err := pyRun(ctx, in, args, env); err != nil || !ok {
		return nil, nil, "pip's resolve failed: see setup.log", err
	}
	data, err := os.ReadFile(report)
	if err != nil {
		return nil, nil, "", fmt.Errorf("pip's report: %w", err)
	}
	pinned, skipped, err := pinnedFromReport(data, in.Dir)
	if err != nil {
		return nil, nil, err.Error(), nil
	}
	if len(skipped) > 0 {
		notes = append(notes, "local packages not installed (the checkout is on PYTHONPATH): "+strings.Join(skipped, ", "))
	}
	if len(pinned) > 0 {
		list := filepath.Join(private, key+"-pinned.txt")
		if err := os.WriteFile(list, []byte(strings.Join(pinned, "\n")+"\n"), 0o600); err != nil {
			return nil, nil, "", err
		}
		if ok, err := pyRun(ctx, in, []string{python, "-m", "pip", "install", "--quiet", "--no-deps", "-r", list}, env); err != nil || !ok {
			return nil, nil, "pip install of the pinned set failed: see setup.log", err
		}
	}
	if !pinnedByFiles(in.Dir, inputs.files, pinned) {
		notes = append(notes, fmt.Sprintf("no lock file: dependencies resolved at warm-up on %s (%d packages, pinned in the venv's stamp)",
			in.Now.Format("2006-01-02"), len(pinned)))
	}
	return pinned, notes, "", nil
}

// pipReport is the part of pip's installation report (--report) the pinned set is made from.
type pipReport struct {
	Install []struct {
		Metadata struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"metadata"`
		IsDirect     bool `json:"is_direct"`
		DownloadInfo struct {
			URL     string          `json:"url"`
			DirInfo json.RawMessage `json:"dir_info"`
			VCSInfo *struct {
				VCS      string `json:"vcs"`
				CommitID string `json:"commit_id"`
			} `json:"vcs_info"`
		} `json:"download_info"`
	} `json:"install"`
}

var (
	pyName    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	pyVersion = regexp.MustCompile(`^[A-Za-z0-9.+!_-]+$`)
)

// pinnedFromReport turns pip's report into requirement lines: name==version from an index, name @ url for a direct
// archive or a VCS commit. Local folders (dir_info: the project, a path dependency) are skipped: installing them would
// put the warm-up checkout's code into the venv. Those other than the project itself (the folder dir) are named. Every
// value is checked, so a line can be nothing but a requirement (no option, no second line).
func pinnedFromReport(data []byte, dir string) (pinned, skipped []string, err error) {
	var r pipReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, nil, fmt.Errorf("pip's report: %w", err)
	}
	for _, item := range r.Install {
		name, version, d := item.Metadata.Name, item.Metadata.Version, item.DownloadInfo
		if !pyName.MatchString(name) {
			return nil, nil, fmt.Errorf("pip's report names a package %q", name)
		}
		url := d.URL
		switch {
		case len(d.DirInfo) > 0 && string(d.DirInfo) != "null":
			if !sameFolder(d.URL, dir) {
				skipped = append(skipped, name)
			}
			continue
		case d.VCSInfo != nil:
			url = d.VCSInfo.VCS + "+" + url + "@" + d.VCSInfo.CommitID
		case !item.IsDirect:
			if !pyVersion.MatchString(version) {
				return nil, nil, fmt.Errorf("pip's report gives %s the version %q", name, version)
			}
			pinned = append(pinned, name+"=="+version)
			continue
		}
		if url == "" || strings.ContainsAny(url, " \t\r\n#") {
			return nil, nil, fmt.Errorf("pip's report gives %s the address %q", name, url)
		}
		pinned = append(pinned, name+" @ "+url)
	}
	slices.Sort(pinned)
	return pinned, skipped, nil
}

// sameFolder reports whether a file: URL names the folder dir (either may be reached through links).
func sameFolder(fileURL, dir string) bool {
	u, err := url.Parse(fileURL)
	if err != nil || u.Scheme != "file" || dir == "" {
		return false
	}
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return real(u.Path) == real(dir)
}

// pinLine is a requirement pinned to one version: name==version (or ===), no wildcard.
var pinLine = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*(\[[^\]]*\])?\s*===?\s*[A-Za-z0-9.+!_-]+\s*(;.*)?(#.*)?$`)

// pinnedByFiles reports whether the requirement files among files pin every package of the resolved set: then they are
// a lock (pip-tools' output, say), and the resolve chose nothing of its own.
func pinnedByFiles(dir string, files, resolved []string) bool {
	pins := map[string]bool{}
	for _, f := range files {
		if !isRequirementFile(f) {
			continue
		}
		data, err := readInside(dir, f)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(data), "\n") {
			if m := pinLine.FindStringSubmatch(line); m != nil {
				pins[normalizeName(m[1])] = true
			}
		}
	}
	for _, r := range resolved {
		name, _, _ := strings.Cut(r, "==")
		name, _, _ = strings.Cut(name, " @ ")
		if !pins[normalizeName(name)] {
			return false
		}
	}
	return true
}

// normalizeName is a package name as PEP 503 compares them.
func normalizeName(name string) string {
	return strings.ToLower(regexp.MustCompile(`[-_.]+`).ReplaceAllString(strings.TrimSpace(name), "-"))
}

// testRequirementFile names the requirement files that hold test dependencies, besides requirements.txt.
var testRequirementFile = regexp.MustCompile(`(?i)^(requirements[-_.]?(test|tests|testing|dev)|(test|tests|testing|dev)[-_]requirements|requirements/(test|tests|testing|dev))\.txt$`)

// isRequirementFile tells the requirement files among a pip venv's inputs (those given and those they include) from the
// project's own files.
func isRequirementFile(f string) bool {
	return f != "pyproject.toml" && f != "setup.cfg" && f != "setup.py"
}

// readPyInputs reads what decides the venv from the warm-up checkout: uv.lock makes it uv's (the lock and
// pyproject.toml); otherwise pip's, from requirements.txt and the test requirement files (and every file they include
// with -r or -c), and the project's own dependencies, with its test extra when it declares one.
func readPyInputs(dir string) (pyInputs, error) {
	has := func(name string) bool {
		info, err := os.Lstat(filepath.Join(dir, name))
		return err == nil && (info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0)
	}
	var in pyInputs
	var pyproject []byte
	if has("pyproject.toml") {
		var err error
		if pyproject, err = readInside(dir, "pyproject.toml"); err != nil {
			return in, err
		}
		in.spec = tomlString(pyproject, "project", "requires-python")
	}
	if has("uv.lock") {
		in.manager, in.files = "uv", []string{"uv.lock"}
		if has("pyproject.toml") {
			in.files = append(in.files, "pyproject.toml")
		}
		return in, nil
	}
	in.manager = "pip"
	for _, name := range []string{"pyproject.toml", "setup.cfg", "setup.py"} {
		if has(name) {
			in.files = append(in.files, name)
		}
	}
	var candidates []string
	if has("requirements.txt") {
		candidates = append(candidates, "requirements.txt")
	}
	var found []string
	for _, pattern := range []string{"*.txt", "requirements/*.txt"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			if rel, err := filepath.Rel(dir, m); err == nil && testRequirementFile.MatchString(filepath.ToSlash(rel)) {
				found = append(found, filepath.ToSlash(rel))
			}
		}
	}
	slices.Sort(found)
	in.reqFiles = append(candidates, found...)
	included, err := includedFiles(dir, in.reqFiles)
	if err != nil {
		return in, err
	}
	in.files = append(in.files, included...)
	if tomlTable(pyproject, "project") || has("setup.py") || (has("setup.cfg") && fileHasLine(dir, "setup.cfg", "[metadata]")) {
		in.project = "."
		for _, extra := range []string{"test", "tests", "testing"} {
			if tomlKey(pyproject, "project.optional-dependencies", extra) {
				in.project = ".[" + extra + "]"
				break
			}
		}
	}
	if tomlTable(pyproject, "dependency-groups") {
		in.notes = append(in.notes, "dependency groups ([dependency-groups]) are not installed without uv.lock")
	}
	return in, nil
}

// requirementInclude is a requirement file's line that reads another file: -r, -c and their long forms.
var requirementInclude = regexp.MustCompile(`^\s*(?:-r|-c|--requirement|--constraint)(?:\s*=\s*|\s+|)(\S+)`)

// includedFiles is files and every file they include (-r, -c), each once, relative to dir. An include that leaves the
// checkout, or a URL, is not followed: the key then misses its content, so it fails closed.
func includedFiles(dir string, files []string) ([]string, error) {
	var out []string
	queue := slices.Clone(files)
	for len(queue) > 0 && len(out) < 64 {
		f := queue[0]
		queue = queue[1:]
		if slices.Contains(out, f) {
			continue
		}
		out = append(out, f)
		data, err := readInside(dir, f)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			m := requirementInclude.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			next := filepath.ToSlash(filepath.Join(filepath.Dir(f), m[1]))
			if strings.Contains(m[1], "://") || filepath.IsAbs(m[1]) || !filepath.IsLocal(next) {
				return nil, fmt.Errorf("%s includes %s, outside the repository: Agentium cannot tell when it changes", f, m[1])
			}
			if !slices.Contains(out, next) && !slices.Contains(queue, next) {
				queue = append(queue, next)
			}
		}
	}
	if len(queue) > 0 {
		return nil, errors.New("requirement files include more than 64 files")
	}
	return out, nil
}

// readInside reads the file rel of the checkout dir, refusing one whose real path leaves the checkout (a link out of
// it) or that is not a regular file.
func readInside(dir, rel string) ([]byte, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(filepath.Join(dir, rel))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	if r, err := filepath.Rel(root, real); err != nil || !filepath.IsLocal(r) {
		return nil, fmt.Errorf("%s leads outside the repository", rel)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return nil, fmt.Errorf("%s is not a regular file under 64 MiB", rel)
	}
	return os.ReadFile(real)
}

func fileHasLine(dir, rel, line string) bool {
	data, err := readInside(dir, rel)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// tomlSections splits a TOML file into its tables by header line ("" before the first). It does not parse TOML: it is
// enough for top-level keys of the tables pyproject.toml declares (requires-python, an extra's name), and it reads a
// key written in another form (an inline table, a dotted key) as absent.
func tomlSections(data []byte) map[string][]string {
	sections := map[string][]string{}
	current := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") && !strings.HasPrefix(line, "[[") {
			if end := strings.Index(line, "]"); end > 0 {
				current = strings.TrimSpace(line[1:end])
				sections[current] = append(sections[current], "")
				continue
			}
		}
		sections[current] = append(sections[current], line)
	}
	return sections
}

func tomlTable(data []byte, table string) bool {
	_, ok := tomlSections(data)[table]
	return ok
}

var tomlKeyLine = regexp.MustCompile(`^["']?([A-Za-z0-9_.-]+)["']?\s*=\s*(.*)$`)

func tomlKey(data []byte, table, key string) bool {
	for _, line := range tomlSections(data)[table] {
		if m := tomlKeyLine.FindStringSubmatch(line); m != nil && m[1] == key {
			return true
		}
	}
	return false
}

// tomlString is a table's key's value when it is a one-line string.
func tomlString(data []byte, table, key string) string {
	for _, line := range tomlSections(data)[table] {
		if m := tomlKeyLine.FindStringSubmatch(line); m != nil && m[1] == key {
			v := strings.TrimSpace(m[2])
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
				if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
					return strings.TrimSpace(v[1 : end+1])
				}
			}
		}
	}
	return ""
}

// venvKey names a venv by everything that decides its contents: the recipe, the package manager and its version, the
// interpreter (its real path and version), and each input file's content. Bases with the same inputs share a venv.
func venvKey(dir string, in pyInputs, tool, interp, version string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00", pythonRecipe, in.manager, tool, interp, version, in.project,
		strings.Join(in.reqFiles, "\x01"))
	for _, f := range in.files {
		data, err := readInside(dir, f)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(h, "%s\x00%x\x00", f, sum)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

func readVenvStamp(root string) (venvStamp, bool) {
	data, err := os.ReadFile(filepath.Join(root, venvStampName))
	if err != nil {
		return venvStamp{}, false
	}
	var s venvStamp
	if json.Unmarshal(data, &s) != nil || s.Recipe != pythonRecipe {
		return venvStamp{}, false
	}
	return s, true
}

// writeVenvStamp writes the stamp whole or not at all (a temporary file renamed over it).
func writeVenvStamp(root string, s venvStamp) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(root, venvStampName+".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(root, venvStampName))
}

// findInterpreter finds an interpreter on the host that meets spec (requires-python): uv's choice among the host's
// interpreters, uv's own managed ones and PATH's, never a virtual environment and never a download; without uv, the
// first python3 (then python3.N, newest first) on PATH that meets it. It returns the base interpreter's real path (a
// venv's link then stays on one patch version) and its version; failed explains none.
func findInterpreter(ctx context.Context, in WarmInput, uv, spec string) (interp, version, failed string, err error) {
	want := "requires-python " + strconv.Quote(spec)
	if spec == "" {
		want = "any version"
	}
	none := "no Python interpreter on this machine meets " + want + ": install one (for example `uv python install`); Agentium downloads none"
	if uv != "" {
		request := spec
		if request == "" {
			request = "any"
		}
		out, ok, err := pyProbe(ctx, in, []string{uv, "python", "find", "--system", "--no-project", request},
			[]string{"UV_PYTHON_DOWNLOADS=never", "UV_PYTHON=", "VIRTUAL_ENV="})
		if err != nil || !ok {
			return "", "", none, err
		}
		found := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
		interp, version, err := probeInterpreter(ctx, in, found)
		if err != nil || interp == "" {
			return "", "", none, err
		}
		return interp, version, "", nil
	}
	path := vars(in.Environ)["PATH"]
	names := []string{"python3"}
	for minor := 14; minor >= 8; minor-- {
		names = append(names, fmt.Sprintf("python3.%d", minor))
	}
	for _, name := range names {
		p := lookPath(name, path)
		if p == "" {
			continue
		}
		interp, version, err := probeInterpreter(ctx, in, p)
		if err != nil {
			return "", "", "", err
		}
		if interp == "" {
			continue
		}
		ok, err := satisfies(version, spec)
		if err != nil {
			return "", "", fmt.Sprintf("requires-python %q cannot be checked without uv (%v): install uv", spec, err), nil
		}
		if ok {
			return interp, version, "", nil
		}
	}
	return "", "", none, nil
}

// probeInterpreter asks an interpreter (isolated from the user's PYTHON* settings) for its version and its base
// interpreter's real path; "" when it cannot tell.
func probeInterpreter(ctx context.Context, in WarmInput, python string) (interp, version string, err error) {
	if !filepath.IsAbs(python) {
		return "", "", nil
	}
	out, ok, err := pyProbe(ctx, in, []string{python, "-I", "-c",
		"import os,sys;print('%d.%d.%d'%sys.version_info[:3]);print(os.path.realpath(getattr(sys,'_base_executable',None) or sys.executable))"}, nil)
	if err != nil || !ok {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(lines[0]) || !filepath.IsAbs(lines[1]) {
		return "", "", nil
	}
	return strings.TrimSpace(lines[1]), lines[0], nil
}

// pyRun runs a warm-up command in the checkout, its output in the log.
func pyRun(ctx context.Context, in WarmInput, args, env []string) (bool, error) {
	fmt.Fprintf(in.Log, "$ %s\n", strings.Join(args, " "))
	res, err := runner.Run(ctx, runner.Spec{Dir: in.Dir, Args: args, Environ: runner.Environ(in.Environ), Env: env, Timeout: in.Timeout,
		Output: in.Log, Started: in.Started})
	return err == nil && res.Passed(), err
}

// pyProbe runs a short command and returns its standard output (its errors go to the log).
func pyProbe(ctx context.Context, in WarmInput, args, extra []string) (string, bool, error) {
	var out bytes.Buffer
	fmt.Fprintf(in.Log, "$ %s\n", strings.Join(args, " "))
	res, err := runner.Run(ctx, runner.Spec{Dir: in.Dir, Args: args, Environ: runner.Environ(in.Environ),
		Env: append(slices.Clone(in.Env), extra...), Timeout: in.Timeout, Output: &out, Stderr: in.Log, Started: in.Started})
	return out.String(), err == nil && res.Passed(), err
}

// lookPath finds an executable on path (a PATH value); "" when there is none. Only absolute folders count: a relative
// one would depend on the folder the command runs in.
func lookPath(name, path string) string {
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// satisfies checks a version (x.y.z) against a requires-python specifier set: comparisons, == and != with a trailing
// .*, and ~=. Anything else (a pre-release, ===, an unknown operator) is an error, not a guess.
func satisfies(version, spec string) (bool, error) {
	v, ok := release(version)
	if !ok {
		return false, fmt.Errorf("version %q", version)
	}
	for _, clause := range strings.Split(spec, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		var op string
		for _, o := range []string{"~=", "==", "!=", "<=", ">=", "<", ">"} {
			if strings.HasPrefix(clause, o) {
				op = o
				break
			}
		}
		if op == "" || strings.HasPrefix(clause, "===") {
			return false, fmt.Errorf("specifier %q", clause)
		}
		target := strings.TrimSpace(strings.TrimPrefix(clause, op))
		wildcard := strings.HasSuffix(target, ".*")
		t, ok := release(strings.TrimSuffix(target, ".*"))
		if !ok || (wildcard && op != "==" && op != "!=") || (op == "~=" && len(t) < 2) {
			return false, fmt.Errorf("specifier %q", clause)
		}
		var met bool
		switch {
		case wildcard:
			met = prefixOf(t, v) == (op == "==")
		case op == "~=":
			met = compare(v, t) >= 0 && prefixOf(t[:len(t)-1], v)
		case op == "==":
			met = compare(v, t) == 0
		case op == "!=":
			met = compare(v, t) != 0
		case op == "<=":
			met = compare(v, t) <= 0
		case op == ">=":
			met = compare(v, t) >= 0
		case op == "<":
			met = compare(v, t) < 0
		case op == ">":
			met = compare(v, t) > 0
		}
		if !met {
			return false, nil
		}
	}
	return true, nil
}

func release(s string) ([]int, bool) {
	var out []int
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}

// compare compares release numbers, the shorter padded with zeros.
func compare(a, b []int) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// prefixOf reports whether v starts with p's numbers (v padded with zeros).
func prefixOf(p, v []int) bool {
	for i, n := range p {
		x := 0
		if i < len(v) {
			x = v[i]
		}
		if x != n {
			return false
		}
	}
	return true
}
