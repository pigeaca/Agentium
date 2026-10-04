package project

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func paths(modules []Module) []string {
	var out []string
	for _, m := range modules {
		out = append(out, m.Path)
	}
	return out
}

// Modules are folders holding a build file, up to four levels deep; a module's own subfolders are not searched (nested
// modules fold into their parent); hidden and dependency folders are skipped; the root's own build file is no module.
func TestFindModules(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a/go.mod", "a/sub/go.mod", "b/c/d/e/pom.xml", "b/c/d/e/f/pom.xml", "b/c/d/e/f/g/pom.xml", "p/pyproject.toml",
		".hidden/go.mod", "node_modules/x/package.json", "vendor/y/go.mod", "docs/readme.md", "x/y/z/w/v/go.mod"} {
		write(t, root, f)
	}
	modules, total := FindModules(root)
	if want := []string{"a", "b/c/d/e", "p"}; !slices.Equal(paths(modules), want) || total != 3 {
		t.Errorf("modules %v (total %d), want %v", paths(modules), total, want)
	}
	if modules[0].Tools[0] != "go" || modules[1].Tools[0] != "maven" || modules[2].Tools[0] != "python" {
		t.Errorf("tools: %+v", modules)
	}
}

func TestFindModulesShowsAtMostTwenty(t *testing.T) {
	root := t.TempDir()
	for i := range 27 {
		write(t, root, fmt.Sprintf("svc%02d/go.mod", i))
	}
	modules, total := FindModules(root)
	if len(modules) != MaxModules || total != 27 || modules[0].Path != "svc00" || modules[19].Path != "svc19" {
		t.Errorf("%d shown, total %d: %v", len(modules), total, paths(modules))
	}
}

func TestValidateModule(t *testing.T) {
	root := repo(t, map[string]string{"svc/go.mod": "module x\n", "docs/readme.md": "x"})
	if err := os.Symlink(filepath.Join(root, "svc"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"svc": "svc", "svc/": "svc", "./svc": "svc", "svc/../svc": "svc", "": "", "  ": ""} {
		if got, err := ValidateModule(t.Context(), root, in); err != nil || got != want {
			t.Errorf("ValidateModule(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, why := range map[string]string{"..": "inside the repository", "../x": "inside the repository", "svc/../..": "inside the repository",
		"/svc": "inside the repository", root: "inside the repository", ".": "root", "docs": "no build file", "docs/readme.md": "not a folder",
		"missing": "no such folder", "link": "link", "svc/go.mod": "not a folder"} {
		if got, err := ValidateModule(t.Context(), root, in); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("ValidateModule(%q) = %q, %v; want an error with %q", in, got, err, why)
		}
	}
}

// Discovery lists modules only when the root has no build file, and a module's discovery proposes the module's test
// commands (with the warning about none following them).
func TestDiscoverModulesAndWithModule(t *testing.T) {
	dir := repo(t, map[string]string{"svc/go.mod": "module x\n", "py/pyproject.toml": "[tool.pytest.ini_options]\n", "docs/a.md": "x"})
	info, err := Discover(t.Context(), dir, testEnv(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths(info.Modules), []string{"py", "svc"}) || info.ModulesTotal != 2 || len(info.TestCommands) != 0 ||
		!slices.Contains(info.Warnings, noTestCommands) {
		t.Fatalf("root discovery: %+v", info)
	}
	mod := info.WithModule("svc")
	if mod.Module != "svc" || !slices.Equal(mod.TestCommands, []string{"go test ./..."}) || slices.Contains(mod.Warnings, noTestCommands) {
		t.Errorf("module discovery: %+v", mod)
	}
	if !slices.Contains(info.Warnings, noTestCommands) {
		t.Error("WithModule changed the warnings of the info it was called on")
	}
	if empty := info.WithModule("docs"); len(empty.TestCommands) != 0 || !slices.Contains(empty.Warnings, noTestCommands) {
		t.Errorf("a folder with no tests: %+v", empty)
	}
	if same := info.WithModule(""); same.Module != "" || !slices.Equal(same.TestCommands, info.TestCommands) {
		t.Errorf("no module changed the discovery: %+v", same)
	}

	plain := repo(t, map[string]string{"go.mod": "module x\n", "svc/go.mod": "module y\n"})
	if info, err := Discover(t.Context(), plain, testEnv(nil, "")); err != nil || len(info.Modules) != 0 || info.ModulesTotal != 0 {
		t.Errorf("a root build file lists modules: %+v, %v", info.Modules, err)
	}
}

// The module must be a folder HEAD commits, spelled as the tree spells it: an untracked folder, a gitignored one, a folder
// only a case-insensitive file system finds under another spelling, and one reached through a committed link are no
// module, whatever the working tree shows.
func TestValidateModuleChecksTheCommit(t *testing.T) {
	root := repo(t, map[string]string{"svc/go.mod": "module x\n", ".gitignore": "ignored/\n", "real/inner/go.mod": "module y\n"})
	write(t, root, "untracked/go.mod")
	write(t, root, "ignored/go.mod")
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "alias")
	git(t, root, "commit", "-q", "-m", "link")
	// Another spelling: on a case-insensitive file system the folder is found by Lstat, but HEAD has no such path.
	_, caseErr := os.Lstat(filepath.Join(root, "SVC"))
	for in, why := range map[string]string{"untracked": "HEAD has no such folder", "ignored": "HEAD has no such folder", "alias/inner": "link"} {
		if got, err := ValidateModule(t.Context(), root, in); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("ValidateModule(%q) = %q, %v; want an error with %q", in, got, err, why)
		}
	}
	if caseErr == nil { // case-insensitive file system
		if got, err := ValidateModule(t.Context(), root, "SVC"); err == nil || !strings.Contains(err.Error(), "HEAD has no such folder") {
			t.Errorf("ValidateModule(SVC) = %q, %v", got, err)
		}
	}
	if got, err := ValidateModule(t.Context(), root, "svc"); err != nil || got != "svc" {
		t.Errorf("a committed module: %q, %v", got, err)
	}
	// What HEAD commits is what counts, not what the folder holds now: a module deleted from the working tree is refused
	// for that, and a module whose build file is committed but whose folder is a link in the tree is refused as a link.
	if err := os.RemoveAll(filepath.Join(root, "svc")); err != nil {
		t.Fatal(err)
	}
	if got, err := ValidateModule(t.Context(), root, "svc"); err == nil {
		t.Errorf("a module deleted from the working tree: %q", got)
	}
}
