package claude

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/buildtool"
)

// userPython is a user's environment with every Python, pip and uv setting that could point the agent elsewhere or
// carry an index credential (PIP_INDEX_URL's token is in the value, which the credential check never reads).
var userPython = append(slices.Clone(parentEnv), "PYTHONPATH=/home/u/other", "PYTHONHOME=/opt/py", "PYTHONDONTWRITEBYTECODE=1",
	"PYTHONSTARTUP=/home/u/.pythonrc", "PIP_INDEX_URL=https://user:s3cret@pypi.example/simple", "PIP_EXTRA_INDEX_URL=https://x",
	"PIP_CACHE_DIR=/home/u/pipc", "UV_INDEX_URL=https://user:s3cret@pypi.example/simple", "UV_CACHE_DIR=/home/u/uvc",
	"UV_PYTHON=/opt/py/bin/python3", "VIRTUAL_ENV=/home/u/proj/.venv", "UV_NO_SYNC=0") // secret-scan: allow

// No project's agent gets the user's PYTHON*, PIP_*, UV_* or VIRTUAL_ENV, whatever its tools; a Python project's agent
// gets the profile's own instead, each once.
func TestPythonVariablesNeverPass(t *testing.T) {
	for _, tools := range [][]string{nil, {"maven"}, {"cargo"}, {"python"}} {
		for _, kv := range EnvironFor(userPython, buildtool.Select(tools)) {
			name, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(name, "PYTHON") || strings.HasPrefix(name, "PIP_") || strings.HasPrefix(name, "UV_") || name == "VIRTUAL_ENV" {
				t.Errorf("%v: the allowlist passes %s", tools, kv)
			}
		}
		inv := toolInvocation(t, tools...)
		env, _ := toolCommand(t, inv, userPython)
		for name, v := range env {
			if slices.ContainsFunc([]string{"s3cret", "/home/u/other", "/opt/py", "/home/u/pipc", "/home/u/uvc", "/home/u/proj", ".pythonrc"},
				func(user string) bool { return strings.Contains(v, user) }) {
				t.Errorf("%v: %s=%q reaches the agent", tools, name, v)
			}
		}
	}
}

// A Python project's run: the venv (in the deps folder, read-only), the checkout on PYTHONPATH and after it the base's
// metadata folder (in the deps folder, read-only), bytecode, uv's cache and hypothesis's database in the run's own
// cache, pip and uv offline; the deps folder's download caches and resolve reports and the user's caches and venv are
// denied, the venv, the metadata and uv's interpreters are not. A metadata folder outside the deps folder, which the
// agent might write, is refused.
func TestPythonRunEnvironmentAndSandbox(t *testing.T) {
	inv := toolInvocation(t, "python")
	inv.Venv = "/data/deps/1/py/0123456789abcdef/venv"
	inv.ProjectMetadata = "/data/deps/1/py-meta/fedcba9876543210"
	env, settings := toolCommand(t, inv, userPython)
	for name, want := range map[string]string{"VIRTUAL_ENV": inv.Venv, "PATH": inv.Venv + "/bin:/usr/bin:/bin", "PYTHONPATH": inv.Dir + ":" + inv.ProjectMetadata,
		"HYPOTHESIS_STORAGE_DIRECTORY": "/work/runs/r1/go-build/hypothesis",
		"PYTHONPYCACHEPREFIX":          "/work/runs/r1/go-build/pycache", "PYTEST_ADDOPTS": "-p no:cacheprovider", "PIP_NO_INDEX": "1",
		"PIP_DISABLE_PIP_VERSION_CHECK": "1", "UV_OFFLINE": "1", "UV_NO_SYNC": "1", "UV_FROZEN": "1", "UV_PYTHON_DOWNLOADS": "never",
		"UV_PROJECT_ENVIRONMENT": inv.Venv, "UV_CACHE_DIR": "/work/runs/r1/go-build/uv"} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	for _, name := range []string{"PYTHONHOME", "PYTHONDONTWRITEBYTECODE", "PYTHONSTARTUP", "PIP_INDEX_URL", "PIP_EXTRA_INDEX_URL",
		"PIP_CACHE_DIR", "UV_INDEX_URL", "UV_PYTHON"} {
		if v, ok := env[name]; ok {
			t.Errorf("%s=%q reaches the agent", name, v)
		}
	}
	network, fs := sandboxOf(settings)
	if network["allowLocalBinding"] != nil || len(network["allowedDomains"].([]any)) != 0 {
		t.Errorf("network %v", network)
	}
	denyRead, denyWrite := fs["denyRead"].([]any), fs["denyWrite"].([]any)
	for _, p := range []string{"/data/deps/1/uv-cache", "/data/deps/1/pip-cache", "/data/deps/1/py-resolve", "/home/u/.cache/uv",
		"/home/u/.cache/pip", "/home/u/Library/Caches/pip", "/home/u/Library/Caches/pypoetry", "/home/u/.cache/pypoetry", "/home/u/uvc",
		"/home/u/pipc", "/home/u/proj/.venv", "/home/u/.npm", "/home/u/Library/pnpm/store"} {
		if !slices.Contains(denyRead, any(p)) {
			t.Errorf("%s is not denied", p)
		}
	}
	for _, p := range denyRead {
		if s := p.(string); strings.HasPrefix(inv.Venv, s) || strings.HasPrefix(inv.ProjectMetadata, s) || s == "/home/u/.local/share/uv" || strings.HasPrefix(s, "/home/u/.local/share/uv/python") {
			t.Errorf("%s is denied: the venv, the metadata or the interpreter", s)
		}
	}
	if !slices.Contains(denyWrite, any("/data/deps/1")) || slices.ContainsFunc(fs["allowWrite"].([]any), func(p any) bool { return strings.HasPrefix(p.(string), "/data/deps") }) {
		t.Errorf("the deps folder, and its venvs, are writable: denyWrite %v, allowWrite %v", denyWrite, fs["allowWrite"])
	}
	// A run that found no venv runs the host's interpreter with the same offline settings, and names no venv.
	inv.Venv = ""
	env, _ = toolCommand(t, inv, userPython)
	if _, ok := env["VIRTUAL_ENV"]; ok || env["PATH"] != "/usr/bin:/bin" || env["PIP_NO_INDEX"] != "1" {
		t.Errorf("without a venv: VIRTUAL_ENV %q, PATH %q", env["VIRTUAL_ENV"], env["PATH"])
	}
	inv.Venv = "venv"
	if _, _, err := inv.Command(userPython); err == nil {
		t.Error("a relative venv is accepted")
	}
	inv.Venv = ""
	for _, meta := range []string{"py-meta/x", "/work/runs/r1/repo/meta", "/data/deps/1", "/data/deps/10/py-meta/x", "/data/deps/1/../2/py-meta/x"} {
		inv.ProjectMetadata = meta
		if _, _, err := inv.Command(userPython); err == nil {
			t.Errorf("the metadata folder %q is accepted", meta)
		}
	}
	inv.ProjectMetadata, inv.Deps = "/data/deps/1/py-meta/x", ""
	if _, _, err := inv.Command(userPython); err == nil {
		t.Error("a metadata folder without a deps folder is accepted")
	}
}

// The Python folders a warm-up creates in the deps folder after an agent started, and the user's caches that do not
// exist yet, are denied where they will lie: by their real form, from the longest existing prefix (a data folder or a
// home reached through a link).
func TestDeniedPathsResolveMissingPythonFolders(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "deps", "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(real, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realDir, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	inv := toolInvocation(t, "python")
	inv.Deps, inv.Home = filepath.Join(link, "deps", "1"), filepath.Join(link, "home")
	_, settings := toolCommand(t, inv, []string{"PATH=/usr/bin", "HOME=" + inv.Home})
	_, fs := sandboxOf(settings)
	denyRead := fs["denyRead"].([]any)
	for _, rel := range []string{"deps/1/uv-cache", "deps/1/pip-cache", "deps/1/py-resolve", "home/.cache/uv", "home/Library/Caches/pip", "home/.npm"} {
		for _, p := range []string{filepath.Join(link, rel), filepath.Join(realDir, rel)} {
			if !slices.Contains(denyRead, any(p)) {
				t.Errorf("%s is not denied", p)
			}
		}
	}
}

// The files where pip, uv, poetry and keyring keep index settings and credentials are denied to every project's agent,
// the Read tool and the sandbox alike: at their defaults, where the user's variables name them (NETRC, PIP_CONFIG_FILE,
// UV_CONFIG_FILE) and the machine's own; each also in its real form when it lies behind a link, missing or not.
func TestPythonCredentialFilesAreDeniedToEveryAgent(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realDir, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(link, "home")
	environ := []string{"PATH=/usr/bin", "HOME=" + home, "NETRC=" + filepath.Join(link, "secrets", "netrc"),
		"PIP_CONFIG_FILE=" + filepath.Join(link, "conf", "pip.conf"), "UV_CONFIG_FILE=/opt/uv/uv.toml"}
	for _, tools := range [][]string{nil, {"python"}, {"cargo"}} {
		inv := toolInvocation(t, tools...)
		inv.Home = home
		denied := inv.DeniedPaths(environ)
		_, settings := toolCommand(t, inv, environ)
		_, fs := sandboxOf(settings)
		denyRead := fs["denyRead"].([]any)
		var rules []string
		for _, r := range settings["permissions"].(map[string]any)["deny"].([]any) {
			rules = append(rules, r.(string))
		}
		var want []string
		for _, rel := range []string{"home/.config/pip", "home/Library/Application Support/pip", "home/.pip", "home/.config/uv/uv.toml",
			"home/.local/share/uv/credentials", "home/.config/pypoetry/auth.toml", "home/Library/Application Support/pypoetry/auth.toml",
			"home/.local/share/python_keyring", "secrets/netrc", "conf/pip.conf"} {
			want = append(want, filepath.Join(link, rel), filepath.Join(realDir, rel))
		}
		want = append(want, "/opt/uv/uv.toml", "/etc/pip.conf", "/private/etc/pip.conf", "/etc/xdg/pip/pip.conf", "/etc/uv/uv.toml",
			"/Library/Application Support/pip/pip.conf")
		for _, p := range want {
			if !slices.Contains(denied, p) || !slices.Contains(denyRead, any(p)) || !slices.Contains(rules, "Read(/"+p+"/**)") {
				t.Errorf("%v: %s is not denied (DeniedPaths, denyRead and the Read rule)", tools, p)
			}
		}
		for _, p := range denied {
			if p == filepath.Join(home, ".local", "share", "uv") || strings.HasPrefix(p, filepath.Join(realDir, "home", ".local", "share", "uv", "python")) {
				t.Errorf("%v: %s is denied: uv's interpreters", tools, p)
			}
		}
	}
}
