package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claude"
	discover "github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/runner"
)

// TestPythonProfileGolden pins what a Python project's runs depend on from its profile: the agent's environment and
// denied paths (with and without a venv, src layout or not), the environment of Agentium's own commands in a checkout
// (setup, grading), discovery's test commands, the files grading treats as checks, and test-runner detection. The
// user's environment carries every Python, pip and uv setting that must not reach the agent. Like the Go golden file, a
// change to it is a security change, to be checked line by line.
func TestPythonProfileGolden(t *testing.T) {
	var out bytes.Buffer
	scratch := t.TempDir()

	user := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/golden/home", "USER=u", "SHELL=/bin/zsh", "TMPDIR=/golden/tmp", "LANG=en_US.UTF-8",
		"PYTHONPATH=/golden/py", "PYTHONHOME=/golden/pyhome", "PYTHONDONTWRITEBYTECODE=1", "PYTHONSTARTUP=/golden/home/.pythonrc",
		"PIP_INDEX_URL=https://u:p@index.example/simple", "PIP_CACHE_DIR=/golden/pipc", "PIP_REQUIRE_VIRTUALENV=1", // secret-scan: allow
		"UV_INDEX_URL=https://u:p@index.example/simple", "UV_CACHE_DIR=/golden/uvc", "UV_PYTHON=/golden/py/bin/python3", // secret-scan: allow
		"VIRTUAL_ENV=/golden/home/proj/.venv", "MYPYPATH=/golden/home/stubs", "POETRY_CACHE_DIR=/golden/poetry", "XDG_CACHE_HOME=/golden/home/xdg",
		"ANTHROPIC_API_KEY=parent-key", "GITHUB_TOKEN=parent-gh", "AGENTIUM_HOME=/golden/data"}
	base := claude.Invocation{CLI: "/golden/bin/claude", Dir: "/golden/data/workspaces/r1/repo", Prompt: "Fix the parser.",
		Model: "claude-sonnet-5", Effort: "medium", BudgetUSD: 3, Home: "/golden/home", SignIn: claude.SignInLogin,
		Deny: []string{"/golden/data/projects", "/golden/data/records", "/golden/repo"}, TempRoot: "/golden/t/ag-0123456789", UID: 4242,
		Tools: []string{"python"}, BuildCache: "/golden/data/workspaces/r1/go-build", Deps: "/golden/data/deps/1"}
	venv := "/golden/data/deps/1/py/0123456789abcdef/venv"
	meta := "/golden/data/deps/1/py-meta/fedcba9876543210" // the base's metadata-only .dist-info, after the checkout
	for _, c := range []struct {
		name string
		inv  func(claude.Invocation) claude.Invocation
	}{
		{"a venv", func(inv claude.Invocation) claude.Invocation { inv.Venv = venv; return inv }},
		{"a venv, the src layout (decided from the base), the project's metadata", func(inv claude.Invocation) claude.Invocation {
			inv.Venv, inv.ImportRoot = venv, buildtool.ImportRoot([]string{"pyproject.toml", "src/click/__init__.py"})
			inv.ProjectMetadata = meta
			return inv
		}},
		{"no venv (a warm-up that failed)", func(inv claude.Invocation) claude.Invocation { return inv }},
	} {
		inv := c.inv(base)
		_, env, err := inv.Command(user)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		fmt.Fprintf(&out, "== agent: %s\n-- env\n", c.name)
		for _, kv := range env {
			fmt.Fprintf(&out, "%s\n", kv)
		}
		fmt.Fprintf(&out, "-- denied paths\n")
		for _, p := range inv.DeniedPaths(user) {
			fmt.Fprintf(&out, "%s\n", p)
		}
	}

	// Agentium's own commands: their caches (CommandEnvFor), then what a checkout adds (CheckoutEnv) for grading's copy.
	fmt.Fprintf(&out, "== commands: CommandEnvFor\n")
	for _, kv := range buildtool.CommandEnvFor(buildtool.Select([]string{"python"}), "/golden/data/cache") {
		fmt.Fprintf(&out, "%s\n", kv)
	}
	fmt.Fprintf(&out, "== commands: CheckoutEnv (grading's copy, the src layout, the project's metadata)\n")
	for _, kv := range buildtool.CheckoutEnv(buildtool.Select([]string{"python"}), buildtool.AgentContext{Allowed: user, Environ: user,
		Home: "/golden/home", Repo: "/golden/data/workspaces/r1/graded", BuildCache: "/golden/data/cache", Deps: base.Deps, Venv: venv,
		Metadata: meta, ImportRoot: "src"}) {
		fmt.Fprintf(&out, "%s\n", kv)
	}
	// The base environment of setup, grading and validation: the user's, less what the agent never gets either.
	fmt.Fprintf(&out, "== commands: CheckoutEnviron (their base environment)\n")
	for _, kv := range runner.Environ(buildtool.CheckoutEnviron(buildtool.Select([]string{"python"}), append(slices.Clone(user),
		"PYTHONOPTIMIZE=2", "PYTHONWARNINGS=error", "PYTHONHASHSEED=0", "PYTHONSAFEPATH=1", "PIP_NO_DEPS=1", "UV_EXCLUDE_NEWER=2020-01-01"))) {
		fmt.Fprintf(&out, "%s\n", kv)
	}

	// Discovery's proposed test commands.
	noClaude := discover.Env{Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "", errors.New("not found") }}
	for _, d := range []struct {
		name  string
		files map[string]string
	}{
		{"uv (click)", map[string]string{"pyproject.toml": "[dependency-groups]\ntests = [\"pytest\"]\n", "uv.lock": "[[package]]\nname = \"pytest\"\n"}},
		{"pip with pytest configured", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n", "requirements-dev.txt": "pytest\n"}},
		{"unittest only (more-itertools)", map[string]string{"pyproject.toml": "[project]\nname = \"more-itertools\"\n", "requirements/testing.txt": "coverage\n"}},
	} {
		info, err := discover.Discover(context.Background(), goldenRepo(t, d.files), noClaude)
		if err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
		fmt.Fprintf(&out, "== discovery: %s\n%q\n", d.name, info.TestCommands)
	}

	// The files grading treats as the verification's scripts and test-runner configuration.
	files := fakeSource{".python-version", "conftest.py", "docs/requirements.txt", "noxfile.py", "pyproject.toml", "pytest.ini",
		"requirements-dev.txt", "requirements.txt", "requirements/testing.txt", "setup.cfg", "setup.py", "src/click/core.py", "tests/conftest.py",
		"tox.ini", "uv.lock"}
	sort.Strings(files)
	for _, verify := range [][]string{{"uv run pytest"}, {"python3 -m pytest -q tests/test_parser.py"}, {"python -m unittest tests.test_recipes"},
		{"tox -e py312"}, {"nox -s tests"}, {"python3 scripts/check.py"}, {"make test"}} {
		scripts, configs := checkFiles(verify, "", files)
		sort.Strings(configs)
		fmt.Fprintf(&out, "== checkFiles %q\nscripts %q\nconfigs %q\n", verify, scripts, configs)
	}

	fmt.Fprintf(&out, "== ranTests\n")
	for _, c := range []string{"uv run pytest tests/test_formatting.py", "python -m pytest", "python3.12 -m unittest tests.test_recipes", "pytest -x",
		".venv/bin/pytest", "tox -e py312", "nox -s tests", "uv run ruff check", "uv sync", "pip install -e .", "python -c 'import click'",
		"cat pytest.ini"} {
		fmt.Fprintf(&out, "%v %s\n", ranTests([]string{c}), c)
	}

	got := out.String()
	if strings.Contains(got, scratch) {
		t.Fatalf("a temporary path is left in the output:\n%s", got)
	}
	path := filepath.Join("testdata", "python-profile.golden")
	if *update {
		writeFile(t, path, got)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("a Python project's runs changed (compare with %s; run with -update only after checking every line):\n%s", path, lineDiff(string(want), got))
	}
}
