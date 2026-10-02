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
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

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
// sets its own. Agentium's own commands in a checkout (the task's setup, grading, validation) use the same venv
// (CheckoutEnv), with their caches in the data folder, and lose the same variables (CheckoutDrop).
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
		AgentEnv:     pythonEnv,
		CheckoutEnv:  pythonEnv,
		CheckoutDrop: pythonVar,
		WarmRecipe:   pythonRecipe,
		WarmFunc:     warmPython,
		PrepareRun:   preparePythonRun,
		UserCaches:   pythonCaches,
	}
}

// pythonRecipe is the warm-up's recipe, part of WarmVersion and of every venv's key: changing what warmPython does
// changes it, so bases and venvs made by an earlier recipe are made again.
const pythonRecipe = "python-2: interpreter by `uv python find --system --no-project <requires-python>` (UV_PYTHON_DOWNLOADS=never) " +
	"or python3 on PATH; venv <deps>/py/<key>/venv, synced, read-only, its manifest in the stamp; uv: uv sync --frozen " +
	"--no-install-project --no-install-workspace --no-install-local --no-install-package <project>; pip: venv, pip>=22.2, then " +
	"pip install --dry-run --ignore-installed --report of the requirement files and .[test|tests|testing|dev|test-dependencies], " +
	"the set less local packages and the project's own name pinned and installed with --no-deps; setuptools' dynamic files " +
	"keyed\n"

// pythonPrivate are the deps folder's Python folders agents may not read (DepsDenied): uv's and pip's download caches
// and the resolve reports. Agents read only <deps>/py.
var pythonPrivate = []string{"uv-cache", "pip-cache", "py-resolve"}

// pythonVar names the variables of the user's environment that never reach a Python project's tests, the agent's or
// Agentium's own: PYTHON*, PIP_*, UV_* and VIRTUAL_ENV (pythonEnv sets the ones it needs).
func pythonVar(name string) bool {
	return strings.HasPrefix(name, "PYTHON") || strings.HasPrefix(name, "PIP_") || strings.HasPrefix(name, "UV_") || name == "VIRTUAL_ENV"
}

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
		env = append(env, "PYTHONPATH="+filepath.Join(c.Repo, c.ImportRoot))
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

// ImportRoot is where a Python project's code imports from, relative to its checkout, read from the base commit's file
// paths (slash-separated, as git lists them): "src" when src/ holds a package (src/<name>/__init__.py) or a module
// (src/<name>.py), the src layout; else "", the checkout itself. It is decided once per run from the base, before the
// agent starts, so a src/ folder the agent adds or removes changes neither its own PYTHONPATH nor grading's.
func ImportRoot(paths []string) string {
	for _, p := range paths {
		rest, ok := strings.CutPrefix(p, "src/")
		if !ok {
			continue
		}
		parts := strings.Split(rest, "/")
		if (len(parts) == 1 && strings.HasSuffix(parts[0], ".py")) || (len(parts) == 2 && parts[1] == "__init__.py") {
			return "src"
		}
	}
	return ""
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
	return append(paths, pythonCredentials(env, home)...)
}

// pythonCredentials are the files where pip, uv, poetry and keyring keep index settings and credentials (an index URL
// with a token, uv's and poetry's stored logins, keyring's file backend), for the user (defaults on macOS and Linux,
// XDG_CONFIG_HOME, XDG_DATA_HOME, and the variables naming them) and, when they exist, for the machine. Agents run pip
// and uv offline and need none of them; pip and uv 0.11 skip a config they cannot read. uv's credentials folder is
// denied, not the folder above it, which holds the interpreters venvs link to. The machine's own configs are listed
// whether they exist or not: denying a missing one is harmless (the agent cannot create it), and the list then does
// not depend on the machine.
func pythonCredentials(env map[string]string, home string) []string {
	config, data := filepath.Join(home, ".config"), filepath.Join(home, ".local", "share")
	appSupport := filepath.Join(home, "Library", "Application Support")
	paths := []string{filepath.Join(config, "pip"), filepath.Join(appSupport, "pip"), filepath.Join(home, ".pip"),
		filepath.Join(config, "uv", "uv.toml"), filepath.Join(data, "uv", "credentials"),
		filepath.Join(config, "pypoetry", "auth.toml"), filepath.Join(appSupport, "pypoetry", "auth.toml"),
		filepath.Join(data, "python_keyring")}
	if x := env["XDG_CONFIG_HOME"]; filepath.IsAbs(x) {
		paths = append(paths, filepath.Join(x, "pip"), filepath.Join(x, "uv", "uv.toml"), filepath.Join(x, "pypoetry", "auth.toml"))
	}
	if x := env["XDG_DATA_HOME"]; filepath.IsAbs(x) {
		paths = append(paths, filepath.Join(x, "uv", "credentials"), filepath.Join(x, "python_keyring"))
	}
	if x := env["POETRY_CONFIG_DIR"]; filepath.IsAbs(x) {
		paths = append(paths, filepath.Join(x, "auth.toml"))
	}
	for _, name := range []string{"PIP_CONFIG_FILE", "UV_CONFIG_FILE", "UV_CREDENTIALS_DIR", "NETRC"} {
		if v := env[name]; filepath.IsAbs(v) && !inside(home, filepath.Clean(v)) {
			paths = append(paths, filepath.Clean(v))
		}
	}
	// /etc is /private/etc on macOS: both forms are listed, so the list is the same on every machine.
	for _, f := range []string{"/etc/pip.conf", "/etc/xdg/pip/pip.conf", "/etc/uv/uv.toml"} {
		paths = append(paths, f, "/private"+f)
	}
	return append(paths, "/Library/Application Support/pip/pip.conf")
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
	// Manifest is a hash of the venv's layout (venvManifest), checked by VenvReady: a package or a .pth added to or
	// removed from site-packages, a .pth rewritten, a script added to bin/ after the stamp (by a host-side command: the
	// venv is read-only, and agents cannot write the deps folder) makes it rebuilt. It does not hash every file, so an
	// edit inside an installed package goes unseen.
	Manifest string `json:"manifest"`
}

const venvStampName = "stamp.json"

// VenvReady reports whether venv is a complete venv of the deps folder whose interpreter still exists and whose layout
// is as stamped: its stamp is there, of the current recipe, bin/python resolves, and its manifest matches (packages
// and .pth files added, removed or, for .pth files, rewritten; not edits inside a package: see venvStamp.Manifest).
// A run's stamp naming a venv that is not ready is warmed again, which rebuilds the venv.
func VenvReady(venv string) bool {
	_, ok := venvIntact(venv)
	return ok
}

// venvIntact is VenvReady with the venv's stamp.
func venvIntact(venv string) (venvStamp, bool) {
	if venv == "" {
		return venvStamp{}, false
	}
	stamp, ok := readVenvStamp(filepath.Dir(venv))
	if !ok || !venvMade(venv) {
		return venvStamp{}, false
	}
	if m, err := venvManifest(venv); err != nil || m != stamp.Manifest {
		return venvStamp{}, false
	}
	return stamp, true
}

// venvManifest hashes the venv's layout: pyvenv.cfg, the names in bin/, in each site-packages folder every top-level
// name with its type and size, and the content of every .pth file (which runs code at start). It reads no package's
// files, so it is cheap enough to check on every run, and catches what is added, removed or swapped at the top, not an
// edit deeper down.
func venvManifest(venv string) (string, error) {
	h := sha256.New()
	cfg, err := os.ReadFile(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		return "", err
	}
	fmt.Fprintf(h, "cfg\x00%s\x00", cfg)
	list := func(dir string, withPth bool) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries { // sorted by name
			info, err := e.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00%s\x00%d\x00", e.Name(), info.Mode().Type(), info.Size())
			if withPth && strings.HasSuffix(e.Name(), ".pth") && info.Mode().IsRegular() {
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					return err
				}
				fmt.Fprintf(h, "pth\x00%s\x00", data)
			}
		}
		return nil
	}
	if err := list(filepath.Join(venv, "bin"), false); err != nil {
		return "", err
	}
	sites, _ := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages"))
	slices.Sort(sites)
	for _, site := range sites {
		fmt.Fprintf(h, "site\x00%s\x00", filepath.Base(filepath.Dir(site)))
		if err := list(site, true); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// setWritable makes every file and folder under root (links excluded) writable by its owner, or not writable by
// anyone: a stamped venv is made read-only, so a host-side command (the task's setup, grading, which are not
// sandboxed) cannot change it by accident; a rebuild makes it writable again before removing it.
func setWritable(root string, writable bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() &^ 0o222
		if writable {
			mode = info.Mode().Perm() | 0o200
		}
		return os.Chmod(p, mode)
	})
}

// pyInputs are what decides a venv's contents, read from the warm-up checkout.
type pyInputs struct {
	manager  string   // uv (uv.lock) or pip
	files    []string // relative paths hashed into the key
	reqFiles []string // pip: the requirement files given to the resolve
	project  string   // pip: "." or ".[extra]" when the checkout is an installable project (its dependencies); "" if not
	spec     string   // requires-python, "" when unstated
	names    []string // the project's own names (pyproject's [project] or [tool.poetry] name, setup.cfg's), normalized
	notes    []string
}

// warmPython builds the base's venv, or finds it built: see pythonProfile. It runs under the warm-up lock (the run's),
// which every warm-up of the project's deps folder takes, so no two build one venv; a venv never changes after its
// stamp, while agents read it. Nothing of the project is built or imported beyond what pip needs to read its
// dependencies, and no test runs (a later base holds earlier tasks' hidden tests as ordinary tests).
func warmPython(ctx context.Context, in WarmInput) (Warmed, error) {
	// What the repository's files make impossible is a note (the run goes on without a venv), not an error.
	// Failures the repository or the host causes (no interpreter, no uv, an input Agentium cannot key) are permanent:
	// the base is stamped with the note. Those a download may cause are transient, and warmed again by the next run.
	permanent := func(why string) (Warmed, error) { return Warmed{Failed: why}, nil }
	transient := func(why string, err error) (Warmed, error) { return Warmed{Failed: why, Transient: true}, err }
	inputs, err := readPyInputs(in.Dir)
	if err != nil {
		return permanent(err.Error())
	}
	path := vars(in.Environ)["PATH"]
	uv := lookPath("uv", path)
	if inputs.manager == "uv" && uv == "" {
		return permanent("uv.lock found, but no uv on PATH: install uv (Agentium installs no tools)")
	}
	interp, version, failed, err := findInterpreter(ctx, in, uv, inputs.spec)
	if err != nil {
		return Warmed{}, err
	}
	if failed != "" {
		return permanent(failed)
	}
	tool := ""
	if inputs.manager == "uv" {
		out, ok, err := pyProbe(ctx, in, []string{uv, "--version"}, nil)
		if err != nil || !ok {
			return transient("uv --version failed: see setup.log", err)
		}
		tool = strings.TrimSpace(out)
	}
	key, err := venvKey(in.Dir, inputs, tool, interp, version)
	if err != nil {
		return permanent(err.Error())
	}
	root := filepath.Join(in.Deps, "py", key)
	venv := filepath.Join(root, "venv")
	if stamp, ok := venvIntact(venv); ok {
		return Warmed{Venv: venv, Notes: stamp.Notes}, nil
	}
	// Not intact: it is rebuilt from nothing, in place (a venv's scripts name its folder, so it cannot be built elsewhere
	// and moved). Unstamped (a warm-up that died or failed), no run was ever handed it, so it is removed. Stamped (its
	// manifest no longer matches, or its interpreter is gone), runs of other bases may be using it right now, so it is
	// renamed aside, whole, to <key>.bad-<time> beside it and left there: Agentium cannot tell when no run uses it any
	// more, so the user removes it (`chmod -R u+w` first: it is read-only). The setup log names it.
	if _, err := os.Lstat(filepath.Join(root, venvStampName)); err == nil {
		aside := fmt.Sprintf("%s.bad-%s", root, in.Now.UTC().Format("20060102T150405Z"))
		for n := 2; fileExists(aside); n++ { // another rebuild in the same second
			aside = fmt.Sprintf("%s.bad-%s-%d", root, in.Now.UTC().Format("20060102T150405Z"), n)
		}
		if err := os.Rename(root, aside); err != nil {
			return Warmed{}, fmt.Errorf("move a changed venv aside: %w", err)
		}
		fmt.Fprintf(in.Log, "[agentium] the venv %s no longer matches its stamp: moved aside to %s (remove it when no run uses it) and rebuilt\n", venv, aside)
	} else {
		if err := setWritable(root, true); err != nil {
			return Warmed{}, fmt.Errorf("remove an unfinished venv: %w", err)
		}
		if err := os.RemoveAll(root); err != nil {
			return Warmed{}, fmt.Errorf("remove an unfinished venv: %w", err)
		}
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
		args := []string{uv, "sync", "--frozen", "--no-install-project", "--no-install-workspace", "--no-install-local", "--python", interp}
		for _, name := range inputs.names { // a released copy of the project, pulled in by a dependency
			args = append(args, "--no-install-package", name)
		}
		ok, err := pyRun(ctx, in, args, env)
		if err != nil || !ok {
			return transient("uv sync failed: see setup.log", err)
		}
	default:
		pinned, notes, failed, retry, err := pipVenv(ctx, in, inputs, interp, venv, key, env)
		if err != nil || failed != "" {
			return Warmed{Failed: failed, Transient: retry}, err
		}
		stamp.Pinned, stamp.Notes = pinned, append(stamp.Notes, notes...)
		stamp.Resolved = in.Now.Format("2006-01-02")
	}
	if !venvMade(venv) {
		return transient("the venv was not made (no bin/python or pyvenv.cfg): see setup.log", nil)
	}
	// Read-only, on disk, then stamped: a crash before the stamp leaves an unstamped venv, rebuilt by the next warm-up.
	if err := setWritable(venv, false); err != nil {
		return Warmed{}, err
	}
	if stamp.Manifest, err = venvManifest(venv); err != nil {
		return Warmed{}, err
	}
	syscall.Sync()
	if err := writeVenvStamp(root, stamp); err != nil {
		return Warmed{}, err
	}
	if err := setWritable(root, false); err != nil {
		return Warmed{}, err
	}
	return Warmed{Venv: venv, Notes: stamp.Notes}, nil
}

// MissingRunners notes the test runners the verification commands use that are not installed in the venv (pytest,
// tox, nox), asked through importlib.metadata, which imports nothing of the project: the verification would fail for
// every arm. environ is the base environment of the check (its PATH is not used: the venv's python is named).
func MissingRunners(ctx context.Context, venv string, verify []string, environ []string) []string {
	if venv == "" {
		return nil
	}
	var notes []string
	for _, runner := range []string{"pytest", "tox", "nox"} {
		word := regexp.MustCompile(`\b` + runner + `\b`)
		if !slices.ContainsFunc(verify, word.MatchString) {
			continue
		}
		check, cancel := context.WithTimeout(ctx, time.Minute)
		cmd := exec.CommandContext(check, filepath.Join(venv, "bin", "python"), "-I", "-c",
			"import importlib.metadata as m, sys; m.version(sys.argv[1])", runner)
		cmd.Env = CheckoutEnviron([]Profile{pythonProfile()}, environ)
		err := cmd.Run()
		cancel()
		if err != nil && ctx.Err() == nil {
			notes = append(notes, fmt.Sprintf("%s is not installed in the venv, and the verification runs it: add it to the project's "+
				"test dependencies (uv.lock's default groups, a test extra or requirement file)", runner))
		}
	}
	return notes
}

// fileExists reports whether path exists (a link counts, whatever it points at).
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
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
func pipVenv(ctx context.Context, in WarmInput, inputs pyInputs, interp, venv, key string, env []string) (pinned, notes []string, failed string, retry bool, err error) {
	if ok, err := pyRun(ctx, in, []string{interp, "-m", "venv", venv}, env); err != nil || !ok {
		return nil, nil, "python -m venv failed: see setup.log", true, err
	}
	if len(inputs.reqFiles) == 0 && inputs.project == "" {
		return nil, []string{"no requirement files and no project dependencies: the venv holds only pip"}, "", false, nil
	}
	private := filepath.Join(in.Deps, "py-resolve")
	if err := os.MkdirAll(private, 0o700); err != nil {
		return nil, nil, "", false, err
	}
	report := filepath.Join(private, key+".json")
	python := filepath.Join(venv, "bin", "python")
	// --report needs pip 22.2: an older interpreter's bundled pip (3.9's 21.x) is upgraded in the fresh venv first
	// (the warm-up has network).
	out, ok, err := pyProbe(ctx, in, []string{python, "-m", "pip", "--version"}, env[len(in.Env):]) // env is in.Env and its overrides
	if err != nil || !ok {
		return nil, nil, "pip --version failed in the venv: see setup.log", true, err
	}
	pipVersion := "" // "pip 21.1.3 from ..."
	if fields := strings.Fields(out); len(fields) >= 2 {
		pipVersion = fields[1]
	}
	if v, ok := release(pipVersion); !ok || compare(v, []int{22, 2}) < 0 {
		if ok, err := pyRun(ctx, in, []string{python, "-m", "pip", "install", "--quiet", "pip>=22.2"}, env); err != nil || !ok {
			return nil, nil, "upgrading pip to 22.2 (for --report) failed: see setup.log", true, err
		}
	}
	args := []string{python, "-m", "pip", "install", "--quiet", "--dry-run", "--ignore-installed", "--report", report}
	for _, f := range inputs.reqFiles {
		args = append(args, "-r", f)
	}
	if inputs.project != "" {
		args = append(args, inputs.project)
	}
	if ok, err := pyRun(ctx, in, args, env); err != nil || !ok {
		return nil, nil, "pip's resolve failed: see setup.log", true, err
	}
	data, err := os.ReadFile(report)
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("pip's report: %w", err)
	}
	pinned, skipped, own, err := pinnedFromReport(data, in.Dir, inputs.names)
	if err != nil {
		return nil, nil, err.Error(), false, nil
	}
	if len(skipped) > 0 {
		notes = append(notes, "local packages not installed (the checkout is on PYTHONPATH): "+strings.Join(skipped, ", "))
	}
	if len(own) > 0 {
		notes = append(notes, "a released copy of the project ("+strings.Join(own, ", ")+"), which a dependency asks for, is not installed: "+
			"it could hold a later task's reference solution; the checkout is on PYTHONPATH")
	}
	if len(pinned) > 0 {
		list := filepath.Join(private, key+"-pinned.txt")
		if err := os.WriteFile(list, []byte(strings.Join(pinned, "\n")+"\n"), 0o600); err != nil {
			return nil, nil, "", false, err
		}
		if ok, err := pyRun(ctx, in, []string{python, "-m", "pip", "install", "--quiet", "--no-deps", "-r", list}, env); err != nil || !ok {
			return nil, nil, "pip install of the pinned set failed: see setup.log", true, err
		}
	}
	if !pinnedByFiles(in.Dir, inputs.files, pinned) {
		notes = append(notes, fmt.Sprintf("no lock file: dependencies resolved at warm-up on %s (%d packages, pinned in the venv's stamp)",
			in.Now.Format("2006-01-02"), len(pinned)))
	}
	return pinned, notes, "", false, nil
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
// put the warm-up checkout's code into the venv. Those other than the project itself (the folder dir) are named. A
// package with one of the project's own names (projectNames, normalized; the folder dir's entry adds its own) is
// dropped too and listed in own: a test dependency that depends on the project pulls its released copy, which may hold
// a later task's reference solution. Every value is checked, so a line can be nothing but a requirement (no option, no
// second line).
func pinnedFromReport(data []byte, dir string, projectNames []string) (pinned, skipped, own []string, err error) {
	var r pipReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, nil, nil, fmt.Errorf("pip's report: %w", err)
	}
	names := slices.Clone(projectNames)
	for _, item := range r.Install {
		if d := item.DownloadInfo; len(d.DirInfo) > 0 && string(d.DirInfo) != "null" && sameFolder(d.URL, dir) {
			names = append(names, normalizeName(item.Metadata.Name))
		}
	}
	for _, item := range r.Install {
		name, version, d := item.Metadata.Name, item.Metadata.Version, item.DownloadInfo
		if !pyName.MatchString(name) {
			return nil, nil, nil, fmt.Errorf("pip's report names a package %q", name)
		}
		url := d.URL
		switch {
		case len(d.DirInfo) > 0 && string(d.DirInfo) != "null":
			if !sameFolder(d.URL, dir) {
				skipped = append(skipped, name)
			}
			continue
		case slices.Contains(names, normalizeName(name)):
			own = append(own, name)
			continue
		case d.VCSInfo != nil:
			url = d.VCSInfo.VCS + "+" + url + "@" + d.VCSInfo.CommitID
		case !item.IsDirect:
			if !pyVersion.MatchString(version) {
				return nil, nil, nil, fmt.Errorf("pip's report gives %s the version %q", name, version)
			}
			pinned = append(pinned, name+"=="+version)
			continue
		}
		if url == "" || strings.ContainsAny(url, " \t\r\n#") {
			return nil, nil, nil, fmt.Errorf("pip's report gives %s the address %q", name, url)
		}
		pinned = append(pinned, name+" @ "+url)
	}
	slices.Sort(pinned)
	return pinned, skipped, own, nil
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

// isRequirementFile tells the requirement files among a pip venv's inputs (those given and those they include, and
// setuptools' dynamic files, which are requirement files too) from the project's own files.
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
		for _, table := range []string{"project", "tool.poetry"} {
			if name := tomlString(pyproject, table, "name"); name != "" {
				in.names = append(in.names, normalizeName(name))
			}
		}
	}
	if has("setup.cfg") {
		if data, err := readInside(dir, "setup.cfg"); err == nil {
			if name := iniString(data, "metadata", "name"); name != "" && !slices.Contains(in.names, normalizeName(name)) {
				in.names = append(in.names, normalizeName(name))
			}
		}
	}
	in.names = slices.DeleteFunc(in.names, func(n string) bool { return !pyName.MatchString(n) })
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
	// Files setuptools reads dependencies from (dynamic = file = ...) decide the venv too.
	dynamic, err := dynamicFiles(dir, pyproject)
	if err != nil {
		return in, err
	}
	for _, f := range dynamic {
		if !slices.Contains(in.files, f) {
			in.files = append(in.files, f)
		}
	}
	in.files = append(in.files, included...)
	if tomlTable(pyproject, "project") || tomlTable(pyproject, "build-system") || has("setup.py") || (has("setup.cfg") && fileHasLine(dir, "setup.cfg", "[metadata]")) {
		in.project = "."
		for _, extra := range []string{"test", "tests", "testing", "dev", "test-dependencies"} {
			if tomlKey(pyproject, "project.optional-dependencies", extra) {
				in.project = ".[" + extra + "]"
				break
			}
		}
	}
	if has("setup.py") {
		in.notes = append(in.notes, "setup.py may read files the venv's key does not cover: a change to them alone reuses the venv")
	}
	if tomlTable(pyproject, "dependency-groups") {
		in.notes = append(in.notes, "dependency groups ([dependency-groups]) are not installed without uv.lock")
	}
	return in, nil
}

// dynamicFile is a `file = "x"` or `file = ["x", "y"]` setting, as setuptools' dynamic metadata names its files.
var dynamicFile = regexp.MustCompile(`file\s*=\s*(\[[^\]]*\]|"[^"]*"|'[^']*')`)

// dynamicFiles lists the files pyproject.toml's [tool.setuptools.dynamic] tables read dependencies from (dependencies,
// optional-dependencies), relative to the checkout; one outside it fails closed.
func dynamicFiles(dir string, pyproject []byte) ([]string, error) {
	var files []string
	for table, lines := range tomlSections(pyproject) {
		if table != "tool.setuptools.dynamic" && !strings.HasPrefix(table, "tool.setuptools.dynamic.") {
			continue
		}
		for _, line := range lines {
			for _, m := range dynamicFile.FindAllStringSubmatch(line, -1) {
				for _, q := range regexp.MustCompile(`"([^"]*)"|'([^']*)'`).FindAllStringSubmatch(m[1], -1) {
					f := filepath.ToSlash(filepath.Clean(q[1] + q[2]))
					if !filepath.IsLocal(f) {
						return nil, fmt.Errorf("pyproject.toml reads dependencies from %s, outside the repository", q[1]+q[2])
					}
					if _, err := readInside(dir, f); err != nil {
						return nil, err
					}
					if !slices.Contains(files, f) {
						files = append(files, f)
					}
				}
			}
		}
	}
	slices.Sort(files)
	return files, nil
}

// iniString is a setup.cfg section's key's value (one line), "" when absent.
func iniString(data []byte, section, key string) string {
	current := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && current == section && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
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

// writeVenvStamp writes the stamp whole or not at all, and durably (WriteFileSynced).
func writeVenvStamp(root string, s venvStamp) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileSynced(filepath.Join(root, venvStampName), append(data, '\n'), 0o600)
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
