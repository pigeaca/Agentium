package buildtool

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// clickPackage is click's pyproject.toml with its build system: a package whose metadata a build makes.
const clickPackage = clickPyproject + "\n[build-system]\nrequires = [\"flit_core<4\"]\nbuild-backend = \"flit_core.buildapi\"\n"

// metadataFiles lists what a metadata folder holds, relative to it.
func metadataFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); rel != "." {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// uv: the base's metadata is made by `uv build --wheel` in the warm-up checkout, into a scratch folder in the denied
// py-resolve folder (removed after), with every backend's git-version override; the metadata folder in
// <deps>/py-meta/<key> holds one .dist-info with METADATA's headers and nothing else (no RECORD, top_level.txt,
// entry_points.txt, WHEEL, license or code from the wheel; no long description), all read-only. Another warm-up of
// the base finds the same folder; a base with other metadata gets its own, and the first is left as it was.
func TestWarmPythonMakesTheProjectsMetadata(t *testing.T) {
	f := newFakePython(t, "3.12.13", true, "")
	deps, repo := depsDir(t), t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPackage, "uv.lock": "version = 1\n", "src/click/__init__.py": ""})
	ctx := context.Background()
	w, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(repo, deps))
	if err != nil || w.Failed != "" || len(w.Notes) != 0 || !VenvReady(w.Venv) {
		t.Fatalf("%+v %v\n%s", w, err, f.log(t))
	}
	if filepath.Dir(w.Metadata) != filepath.Join(deps, "py-meta") || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(filepath.Base(w.Metadata)) || !MetadataReady(w.Metadata) {
		t.Fatalf("metadata %q (ready %v)", w.Metadata, MetadataReady(w.Metadata))
	}
	if got := metadataFiles(t, w.Metadata); !slices.Equal(got, []string{"fake_project-1.2.3.dist-info", "fake_project-1.2.3.dist-info/METADATA"}) {
		t.Errorf("the metadata folder holds %q", got)
	}
	data, err := os.ReadFile(filepath.Join(w.Metadata, "fake_project-1.2.3.dist-info", "METADATA"))
	if err != nil || string(data) != fakeMetadataHeaders {
		t.Errorf("METADATA %q, want %q (%v)", data, fakeMetadataHeaders, err)
	}
	// Read-only: neither a host-side command by accident nor anything else rewrites it without a chmod.
	for _, p := range []string{w.Metadata, filepath.Join(w.Metadata, "fake_project-1.2.3.dist-info"), filepath.Join(w.Metadata, "fake_project-1.2.3.dist-info", "METADATA")} {
		if info, err := os.Lstat(p); err != nil || info.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s is writable: %v %v", p, info, err)
		}
	}
	if os.Geteuid() != 0 {
		if err := os.WriteFile(filepath.Join(w.Metadata, "planted.pth"), []byte("import os\n"), 0o644); err == nil {
			t.Error("a file could be written into the metadata folder")
		}
	}
	log := f.log(t)
	build := regexp.MustCompile(`uv build --wheel --out-dir (\S+) --python ` + regexp.QuoteMeta(f.interp) + ` \|`).FindStringSubmatch(log)
	if build == nil || filepath.Dir(build[1]) != filepath.Join(deps, "py-resolve") {
		t.Fatalf("no uv build into py-resolve:\n%s", log)
	}
	if !strings.Contains(log, "build into "+build[1]+" | SETUPTOOLS_SCM_PRETEND_VERSION=0.0.0 PDM_BUILD_SCM_VERSION=0.0.0") {
		t.Errorf("the build ran without the git-version overrides:\n%s", log)
	}
	if left, _ := filepath.Glob(filepath.Join(deps, "py-resolve", "wheel-*")); len(left) != 0 {
		t.Errorf("the wheel is left: %q", left)
	}
	if strings.Contains(log, "pip wheel") {
		t.Errorf("pip built a uv project:\n%s", log)
	}

	// The base again (a later run, or validation): the same folder, the venv not synced again.
	syncs := strings.Count(log, "uv sync")
	again, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(repo, deps))
	if err != nil || again.Metadata != w.Metadata || again.Venv != w.Venv || strings.Count(f.log(t), "uv sync") != syncs {
		t.Errorf("again: %+v %v", again, err)
	}
	// A base whose metadata differs (another version): another folder, beside the first, which stays as it was.
	fakeWheel(t, filepath.Join(f.dir, "other.whl"), "1.3.0")
	other, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(repo, deps, "FAKE_WHEEL=other"))
	if err != nil || other.Metadata == "" || other.Metadata == w.Metadata || !MetadataReady(other.Metadata) || !MetadataReady(w.Metadata) {
		t.Errorf("other metadata: %+v %v", other, err)
	}
	if got := metadataFiles(t, other.Metadata); !slices.Equal(got, []string{"fake_project-1.3.0.dist-info", "fake_project-1.3.0.dist-info/METADATA"}) {
		t.Errorf("the other folder holds %q", got)
	}
}

// A metadata folder is ready only as made: read-only, one .dist-info holding one regular METADATA, whose name and
// content give the folder's name. Anything else (a file or a module planted, METADATA rewritten or made a link, the
// folder writable, a warm-up that died before it was finished) is not handed to a run: the next warm-up moves a changed
// folder aside (a run may have it on its PYTHONPATH), removes unfinished ones, and makes it again.
func TestMetadataFolderReadiness(t *testing.T) {
	f := newFakePython(t, "3.12.13", true, "")
	deps, repo := depsDir(t), t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPackage, "uv.lock": "version = 1\n"})
	ctx := context.Background()
	w, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(repo, deps))
	if err != nil || !MetadataReady(w.Metadata) {
		t.Fatal(w, err)
	}
	distInfo := filepath.Join(w.Metadata, "fake_project-1.2.3.dist-info")
	metadata := filepath.Join(distInfo, "METADATA")
	asides := 0
	for name, damage := range map[string]func(){
		"a module planted beside": func() { writeFiles(t, w.Metadata, map[string]string{"fake_project/__init__.py": "X = 1\n"}) },
		"a RECORD planted":        func() { writeFiles(t, distInfo, map[string]string{"RECORD": "x,,\n"}) },
		"a top_level.txt planted": func() { writeFiles(t, distInfo, map[string]string{"top_level.txt": "x\n"}) },
		"METADATA rewritten":      func() { os.WriteFile(metadata, []byte("Name: fake.project\nVersion: 9\n"), 0o444) },
		"METADATA a link": func() {
			target := filepath.Join(t.TempDir(), "METADATA")
			os.WriteFile(target, []byte(fakeMetadataHeaders), 0o444)
			os.Remove(metadata)
			os.Symlink(target, metadata)
		},
		"the folder writable": func() {},
		"METADATA removed":    func() { os.Remove(metadata) },
	} {
		if err := setWritable(w.Metadata, true); err != nil {
			t.Fatal(err)
		}
		damage()
		if name != "the folder writable" {
			setWritable(w.Metadata, false)
		}
		if MetadataReady(w.Metadata) {
			t.Errorf("%s: still ready", name)
		}
		again, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(repo, deps))
		if err != nil || again.Metadata != w.Metadata || !MetadataReady(again.Metadata) {
			t.Errorf("%s: %+v %v", name, again, err)
		}
		found, _ := filepath.Glob(w.Metadata + ".bad-*")
		if len(found) != asides+1 {
			t.Errorf("%s: not moved aside (%q)", name, found)
		}
		asides = len(found)
	}
	// Leftovers of a warm-up that died while making a folder are removed by the next one.
	leftover := filepath.Join(deps, "py-meta", ".new-123")
	writeFiles(t, leftover, map[string]string{"x.dist-info/METADATA": "Name: x\n"})
	os.Chmod(filepath.Join(leftover, "x.dist-info"), 0o555)
	if _, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(repo, deps)); err != nil {
		t.Fatal(err)
	}
	if fileExists(leftover) {
		t.Error("an unfinished folder was left")
	}
	for _, missing := range []string{"", filepath.Join(deps, "py-meta", "0000000000000000"), filepath.Join(deps, "py-meta")} {
		if MetadataReady(missing) {
			t.Errorf("%q is ready", missing)
		}
	}
}

// A version computed from git: the build runs with the backends' overrides set to 0.0.0 (a checkout of one commit
// has neither history nor tags), and the run gets a note when the metadata says so; a project whose own static
// version is 0.0.0 gets none.
func TestProjectMetadataVersionFromGit(t *testing.T) {
	f := newFakePython(t, "3.12.13", true, "")
	fakeWheel(t, filepath.Join(f.dir, "scm-0.0.0.whl"), "0.0.0")
	hatch := "[project]\nname = \"attrs\"\ndynamic = [\"version\"]\n[build-system]\nrequires = [\"hatchling\", \"hatch-vcs\"]\n" +
		"build-backend = \"hatchling.build\"\n[tool.hatch.version]\nsource = \"vcs\"\n"
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": hatch, "uv.lock": "version = 1\n"})
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, depsDir(t), "FAKE_SCM=1"))
	if err != nil || w.Metadata == "" || !slices.Equal(w.Notes, []string{"the project's version could not be read statically (a version computed from git needs history, " +
		"which a checkout of one commit lacks): its metadata says 0.0.0"}) {
		t.Fatalf("%+v %v\n%s", w, err, f.log(t))
	}
	if got := metadataFiles(t, w.Metadata); !slices.Contains(got, "fake_project-0.0.0.dist-info/METADATA") {
		t.Errorf("the folder holds %q", got)
	}
	static := t.TempDir()
	writeFiles(t, static, map[string]string{"pyproject.toml": strings.Replace(hatch, "dynamic = [\"version\"]", "version = \"0.0.0\"", 1), "uv.lock": "version = 1\n"})
	if w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(static, depsDir(t), "FAKE_SCM=1")); err != nil || w.Metadata == "" || len(w.Notes) != 0 {
		t.Errorf("a static 0.0.0: %+v %v", w, err)
	}
	// setup.cfg's literal version is static too; one read from the code (attr:) is not.
	for cfg, note := range map[string]bool{"version = 0.0.0\n": false, "version = attr: pkg.__version__\n": true} {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"setup.cfg": "[metadata]\nname = pkg\n" + cfg, "setup.py": "", "uv.lock": "version = 1\n"})
		if w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(dir, depsDir(t), "FAKE_SCM=1")); err != nil || w.Metadata == "" || (len(w.Notes) == 1) != note {
			t.Errorf("setup.cfg %q: %+v %v", cfg, w, err)
		}
	}
}

// pip: the venv's own pip builds the wheel (`pip wheel --no-deps`), in the warm-up checkout, into py-resolve.
func TestProjectMetadataWithPip(t *testing.T) {
	report := `{"install":[{"metadata":{"name":"coverage","version":"7.16.2"},"download_info":{"url":"https://x","archive_info":{}}}]}`
	f := newFakePython(t, "3.12.13", false, report)
	deps, repo := depsDir(t), t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": "[project]\nname = \"more-itertools\"\nversion = \"11.1.0\"\n", "requirements-test.txt": "coverage==7.16.2\n"})
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), f.input(repo, deps))
	if err != nil || w.Failed != "" || !MetadataReady(w.Metadata) {
		t.Fatalf("%+v %v\n%s", w, err, f.log(t))
	}
	if want := "python -m pip wheel --quiet --no-deps --wheel-dir " + filepath.Join(deps, "py-resolve", "wheel-"); !strings.Contains(f.log(t), want) ||
		!strings.Contains(f.log(t), ". | VIRTUAL_ENV="+w.Venv+" PIP_CACHE_DIR="+filepath.Join(deps, "pip-cache")) {
		t.Errorf("no %q with the venv's settings in\n%s", want, f.log(t))
	}
}

// What cannot be made never fails the venv: the warm-up's failure is transient (a download of the build backend may
// have failed), with the venv ready, which the run that warmed it uses without metadata (only tests that read the
// project's metadata fail), and the base stays unstamped. A project that is no package has none, and no note: a uv
// project without a build system, or with `[tool.uv] package = false`, and a pip folder of requirements.
func TestProjectMetadataFailuresAreTransient(t *testing.T) {
	f := newFakePython(t, "3.12.13", true, "")
	writeWheel(t, filepath.Join(f.dir, "nometa.whl"), map[string]string{"x/__init__.py": "", "x-1.dist-info/RECORD": ""})
	writeWheel(t, filepath.Join(f.dir, "badversion.whl"), map[string]string{"x-1.dist-info/METADATA": "Name: x\nVersion: 1-2\n"})
	writeWheel(t, filepath.Join(f.dir, "misnamed.whl"), map[string]string{"y-1.dist-info/METADATA": "Name: x\nVersion: 1\n"})
	writeWheel(t, filepath.Join(f.dir, "otherversion.whl"), map[string]string{"x-2.dist-info/METADATA": "Name: x\nVersion: 1\n"})
	ctx := context.Background()
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPackage, "uv.lock": "version = 1\n"})
	for extra, why := range map[string]string{"FAKE_BUILD_FAIL=1": "the wheel's build failed: see setup.log",
		"FAKE_WHEEL=nometa": "the wheel has no METADATA", "FAKE_WHEEL=badversion": `version "1-2"`, "FAKE_WHEEL=none": "the build made 0 wheels, not one",
		"FAKE_WHEEL=misnamed": `the wheel's folder "y-1.dist-info" does not name x 1`, "FAKE_WHEEL=otherversion": `"x-2.dist-info" does not name x 1`} {
		w, err := WarmFuncs(ctx, Select([]string{"python"}), f.input(repo, depsDir(t), extra))
		if err != nil || !w.Transient || !VenvReady(w.Venv) || w.Metadata != "" ||
			!strings.Contains(w.Failed, "the project's metadata could not be made") || !strings.Contains(w.Failed, why) {
			t.Errorf("%s: %+v %v", extra, w, err)
		}
	}
	for name, files := range map[string]map[string]string{
		"no build system":   {"pyproject.toml": clickPyproject, "uv.lock": "version = 1\n"},
		"package = false":   {"pyproject.toml": clickPackage + "[tool.uv]\npackage = false\n", "uv.lock": "version = 1\n"},
		"requirements only": {"requirements.txt": ""},
	} {
		g := newFakePython(t, "3.12.13", true, `{"install":[]}`)
		dir := t.TempDir()
		writeFiles(t, dir, files)
		w, err := WarmFuncs(ctx, Select([]string{"python"}), g.input(dir, depsDir(t)))
		if err != nil || w.Failed != "" || w.Metadata != "" || slices.ContainsFunc(w.Notes, func(n string) bool { return strings.Contains(n, "metadata") }) ||
			strings.Contains(g.log(t), "build into") {
			t.Errorf("%s: %+v %v\n%s", name, w, err, g.log(t))
		}
	}
}

// METADATA keeps its headers, continuation lines included, and loses the long description (the body and a
// Description header); a name or version that cannot name a .dist-info folder, or a line that is no header, is refused.
func TestMetadataHeaders(t *testing.T) {
	name, version, headers, err := metadataHeaders([]byte(fakeMetadata))
	if err != nil || name != "Fake.Project" || version != "1.2.3" || string(headers) != fakeMetadataHeaders {
		t.Errorf("%q %q %q %v", name, version, headers, err)
	}
	for _, bad := range []string{"Name: x\n", "Version: 1\n", "Name: x\nVersion: 1-2\n", "Name: -x\nVersion: 1\n", "Name: x/y\nVersion: 1\n",
		"Name: x\nVersion: 1\nnot a header\n", "Name: x\nVersion: 1\n: no key\n", "Name: x\nVersion: 1 2\n", ""} {
		if _, _, _, err := metadataHeaders([]byte(bad)); err == nil {
			t.Errorf("%q is accepted", bad)
		}
	}
	if _, _, headers, err := metadataHeaders([]byte("Name: x\nVersion: 1\nDESCRIPTION: long\n  more\nSummary: s\n")); err != nil || string(headers) != "Name: x\nVersion: 1\nSummary: s\n" {
		t.Errorf("a Description header in capitals: %q %v", headers, err)
	}
}

// With a real interpreter, the environment the profile gives (the checkout first on PYTHONPATH, then the metadata
// folder): importlib.metadata finds the project's version, and the project imports from the checkout. Without the
// project in the checkout, it does not import at all: the metadata folder holds no module.
func TestProjectMetadataWithARealInterpreter(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	deps := depsDir(t)
	meta, err := installMetadata(WarmInput{Deps: deps, Log: &strings.Builder{}}, "my_pkg-9.9.dist-info", []byte("Metadata-Version: 2.1\nName: my-pkg\nVersion: 9.9\n"))
	if err != nil {
		t.Fatal(err)
	}
	probe := "import importlib.metadata as m; print(m.version('my-pkg'))\ntry:\n import my_pkg; print(my_pkg.__file__)\nexcept ImportError as e: print(type(e).__name__)\n"
	run := func(repo string) []string {
		t.Helper()
		cmd := exec.Command(python, "-c", probe)
		var environ []string
		for _, kv := range os.Environ() {
			if !pythonVar(strings.SplitN(kv, "=", 2)[0]) {
				environ = append(environ, kv)
			}
		}
		cmd.Env = append(environ, AgentEnv(Select([]string{"python"}), AgentContext{Allowed: environ, Repo: repo, Metadata: meta})...)
		cmd.Dir = t.TempDir()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"my_pkg/__init__.py": ""})
	if got := run(repo); len(got) != 2 || got[0] != "9.9" || !strings.HasPrefix(got[1], repo) && !strings.Contains(got[1], filepath.Base(repo)+"/my_pkg/__init__.py") {
		t.Errorf("with the package in the checkout: %q", got)
	}
	if got := run(t.TempDir()); len(got) != 2 || got[0] != "9.9" || got[1] != "ModuleNotFoundError" {
		t.Errorf("without it: %q", got)
	}
	// A dotted name, in the wheel's own folder name: found by that name on every Python, 3.9 included.
	zope, err := installMetadata(WarmInput{Deps: deps, Log: &strings.Builder{}}, "zope.interface-7.2.dist-info", []byte("Metadata-Version: 2.1\nName: zope.interface\nVersion: 7.2\n"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", "import importlib.metadata as m; print(m.version('zope.interface'))")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONPATH=" + zope}
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "7.2" {
		t.Errorf("zope.interface: %q %v", out, err)
	}
}

// A failure is retried by the next warm-ups of the base, counted in the warm-up state: the third in a row is a note,
// stamped with the base (a build that always fails is not rebuilt by every run), and the count starts again, so
// removing the base's stamp gives it as many tries. A success clears the count. Another base counts on its own.
func TestProjectMetadataTriesAreCounted(t *testing.T) {
	f := newFakePython(t, "3.12.13", true, "")
	deps, state, repo := depsDir(t), t.TempDir(), t.TempDir()
	writeFiles(t, repo, map[string]string{"pyproject.toml": clickPackage, "uv.lock": "version = 1\n"})
	warm := func(base string, extra ...string) Warmed {
		t.Helper()
		in := f.input(repo, deps, extra...)
		in.Base, in.State = base, state
		w, err := WarmFuncs(context.Background(), Select([]string{"python"}), in)
		if err != nil || !VenvReady(w.Venv) {
			t.Fatalf("%+v %v", w, err)
		}
		return w
	}
	for try := 1; try <= 2; try++ {
		if w := warm("c1", "FAKE_BUILD_FAIL=1"); !w.Transient || !strings.Contains(w.Failed, fmt.Sprintf("(try %d of 3)", try)) {
			t.Errorf("try %d: %+v", try, w)
		}
	}
	if w := warm("c2", "FAKE_BUILD_FAIL=1"); !strings.Contains(w.Failed, "(try 1 of 3)") {
		t.Errorf("another base: %+v", w)
	}
	w := warm("c1", "FAKE_BUILD_FAIL=1")
	if w.Failed != "" || w.Transient || w.Metadata != "" || len(w.Notes) != 1 || !strings.Contains(w.Notes[0], "could not be made (the wheel's build failed: see setup.log)") ||
		!strings.HasSuffix(w.Notes[0], "(3 tries)") {
		t.Errorf("the third try: %+v", w)
	}
	if fileExists(filepath.Join(state, "c1.metadata-tries")) {
		t.Error("the count was kept after the note")
	}
	if w := warm("c1", "FAKE_BUILD_FAIL=1"); !strings.Contains(w.Failed, "(try 1 of 3)") {
		t.Errorf("after a stamp removed by hand: %+v", w)
	}
	if w := warm("c1"); w.Failed != "" || !MetadataReady(w.Metadata) || fileExists(filepath.Join(state, "c1.metadata-tries")) {
		t.Errorf("a success: %+v", w)
	}
	// A wheel a killed warm-up left in py-resolve is swept by the next one.
	stale := filepath.Join(deps, "py-resolve", "wheel-123")
	writeFiles(t, stale, map[string]string{"x-1-py3-none-any.whl": "code"})
	warm("c3")
	if fileExists(stale) {
		t.Error("a killed warm-up's wheel was left")
	}
}

// A dotted name (zope.interface): the folder keeps the wheel's own .dist-info name, which importlib.metadata finds by
// the dotted name on every Python (3.9 does not normalize it to zope_interface).
func TestProjectMetadataKeepsTheWheelsFolderName(t *testing.T) {
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"setup.py": "from setuptools import setup; setup()\n", "requirements.txt": ""})
	g := newFakePython(t, "3.12.13", false, `{"install":[]}`)
	writeWheel(t, filepath.Join(g.dir, "zope.whl"), map[string]string{"zope.interface-7.2.dist-info/METADATA": "Metadata-Version: 2.1\nName: zope.interface\nVersion: 7.2\n"})
	w, err := WarmFuncs(context.Background(), Select([]string{"python"}), g.input(repo, depsDir(t), "FAKE_WHEEL=zope"))
	if err != nil || w.Failed != "" || !MetadataReady(w.Metadata) {
		t.Fatalf("%+v %v\n%s", w, err, g.log(t))
	}
	if got := metadataFiles(t, w.Metadata); !slices.Equal(got, []string{"zope.interface-7.2.dist-info", "zope.interface-7.2.dist-info/METADATA"}) {
		t.Errorf("the folder holds %q", got)
	}
}
