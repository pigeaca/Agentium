package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/gitx"
)

// memSource is an in-memory repository state; paths ending in "*" are executable.
type memSource map[string]string

func (m memSource) Paths() []string {
	var paths []string
	for p := range m {
		paths = append(paths, strings.TrimSuffix(p, "*"))
	}
	sort.Strings(paths)
	return paths
}
func (m memSource) ReadFile(p string) ([]byte, error) {
	for _, key := range []string{p, p + "*"} {
		if data, ok := m[key]; ok {
			return []byte(data), nil
		}
	}
	return nil, os.ErrNotExist
}
func (m memSource) Executable(p string) bool { _, ok := m[p+"*"]; return ok }
func (m memSource) Describe() string         { return "memory" }

func bareRepo(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.InitBare(context.Background(), bare); err != nil {
		t.Fatal(err)
	}
	return bare
}

var full = memSource{
	"CLAUDE.md":               "# Project\n@AGENTS.md\n",
	"AGENTS.md":               "Rules\n",
	".claude/rules/go.md":     "gofmt\n",
	".claude/hooks/check.sh*": "#!/bin/sh\n",
	"main.go":                 "package main\n", // not context
}

func TestBuildIsDeterministicAndHoldsOnlyContext(t *testing.T) {
	ctx := context.Background()
	bare := bareRepo(t)
	first, manifest, err := Build(ctx, bare, full, "snapshot full", nil)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := Build(ctx, bareRepo(t), full, "snapshot full", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("same content gave commits %s and %s", first, again)
	}
	tree, err := gitx.Run(ctx, "--git-dir", bare, "ls-tree", "-r", "--format=%(objectmode) %(path)", first)
	if err != nil {
		t.Fatal(err)
	}
	want := "100755 .claude/hooks/check.sh\n100644 .claude/rules/go.md\n100644 AGENTS.md\n100644 CLAUDE.md"
	if tree != want {
		t.Errorf("tree:\n%s\nwant:\n%s", tree, want)
	}
	if parents, _ := gitx.Run(ctx, "--git-dir", bare, "log", "--format=%P", "-1", first); parents != "" {
		t.Errorf("snapshot commits must be parentless, got parents %q", parents)
	}
	if !slices.Equal(manifest.Paths(), []string{".claude/hooks/check.sh", ".claude/rules/go.md", "AGENTS.md", "CLAUDE.md"}) {
		t.Errorf("manifest paths = %v", manifest.Paths())
	}
	sum := sha256.Sum256([]byte("Rules\n"))
	for _, f := range manifest.Files {
		if f.Path == "AGENTS.md" && (f.SHA256 != hex.EncodeToString(sum[:]) || f.Kind != "import" || f.StartupBytes != 6) {
			t.Errorf("AGENTS.md entry = %+v", f)
		}
	}
	if manifest.StartupBytes != len("# Project\n@AGENTS.md\n")+len("Rules\n")+len("gofmt\n") {
		t.Errorf("startup bytes = %d", manifest.StartupBytes)
	}
}

func TestDiff(t *testing.T) {
	ctx := context.Background()
	bare := bareRepo(t)
	fullCommit, _, err := Build(ctx, bare, full, "full", nil)
	if err != nil {
		t.Fatal(err)
	}
	minimal := memSource{"CLAUDE.md": "# Project\nKeep it short.\n", "main.go": "package main\n"}
	minimalCommit, _, err := Build(ctx, bare, minimal, "minimal", nil)
	if err != nil {
		t.Fatal(err)
	}
	stat, patch, err := Diff(ctx, bare, fullCommit, minimalCommit)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AGENTS.md", "CLAUDE.md", ".claude/rules/go.md", "4 files changed"} {
		if !strings.Contains(stat, want) {
			t.Errorf("stat lacks %q:\n%s", want, stat)
		}
	}
	if !strings.Contains(patch, "+Keep it short.") || !strings.Contains(patch, "-Rules") {
		t.Errorf("patch:\n%s", patch)
	}
	if stat, patch, err := Diff(ctx, bare, fullCommit, fullCommit); err != nil || stat != "" || patch != "" {
		t.Errorf("self diff = %q %q %v", stat, patch, err)
	}
}

func TestPlanOverlay(t *testing.T) {
	base := memSource{
		"CLAUDE.md":               "# Project\n@AGENTS.md\n@.agents/core.md\n",
		"AGENTS.md":               "Rules\n",
		".agents/core.md":         "Core\n", // imported, but an ordinary repository file
		".claude/rules/go.md":     "gofmt\n",
		".claude/hooks/check.sh*": "#!/bin/sh\n",
		"main.go":                 "package main\n",
		"docs/x.md":               "not context in the base\n",
	}
	cases := []struct {
		name    string
		snap    memSource
		writes  []string
		deletes []string
		err     error
	}{
		{name: "minimal", snap: memSource{"CLAUDE.md": "# Project\nKeep it short.\n"}, writes: []string{"CLAUDE.md"},
			deletes: []string{".claude/hooks/check.sh", ".claude/rules/go.md", "AGENTS.md"}},
		{name: "agents only", snap: memSource{"AGENTS.md": "Other rules\n"}, writes: []string{"AGENTS.md"},
			deletes: []string{".claude/hooks/check.sh", ".claude/rules/go.md", "CLAUDE.md"}},
		{name: "same file imported", snap: memSource{"CLAUDE.md": "@main.go\n", "main.go": "package main\n"},
			writes: []string{"CLAUDE.md", "main.go"}, deletes: []string{".claude/hooks/check.sh", ".claude/rules/go.md", "AGENTS.md"}},
		{name: "changes source code", snap: memSource{"CLAUDE.md": "@main.go\n", "main.go": "package changed\n"}, err: ErrTouchesNonContext},
		{name: "changes a document", snap: memSource{"CLAUDE.md": "@docs/x.md\n", "docs/x.md": "edited guide\n"},
			writes: []string{"CLAUDE.md", "docs/x.md"}, deletes: []string{".claude/hooks/check.sh", ".claude/rules/go.md", "AGENTS.md"}},
		// Taken when docs/x.md did not exist: in this base the import would resolve and load it.
		{name: "import appears in base", snap: memSource{"CLAUDE.md": "@docs/x.md\n"}, err: ErrArmMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			overlay, err := PlanOverlay(base, c.snap)
			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Fatalf("err = %v, want %v", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(overlay.Writes, c.writes) || !slices.Equal(overlay.Deletes, c.deletes) {
				t.Errorf("overlay = %+v, want writes %v deletes %v", overlay, c.writes, c.deletes)
			}
		})
	}
	if _, err := PlanOverlay(base, memSource{"CLAUDE.md": "@main.go\n", "main.go": "package changed\n"}); err == nil || !strings.Contains(err.Error(), "main.go") {
		t.Errorf("the refusal must name the file: %v", err)
	}
}

func TestPlanOverlayWhenTheBaseImportsCodeAndHarnessDiffers(t *testing.T) {
	base := memSource{
		"CLAUDE.md":             "See @package.json and @docs/guide.md\n",
		"package.json":          "{\"scripts\":{\"test\":\"jest\"}}\n",
		"docs/guide.md":         "old guide\n",
		".claude/settings.json": "{\"permissions\":{}}\n",
		".mcp.json":             "{}\n",
	}
	// Imported by the base, but build inputs: changing them would change the build, not only the context.
	base["requirements.txt"] = "django==4\n"
	base["testdata/golden.md"] = "expected output\n"
	for file, content := range map[string]string{"package.json": "{}\n", "requirements.txt": "django==5\n", "testdata/golden.md": "other\n"} {
		if _, err := PlanOverlay(base, memSource{"CLAUDE.md": "@" + file + "\n", file: content}); !errors.Is(err, ErrTouchesNonContext) {
			t.Errorf("imported %s changed: err = %v, want ErrTouchesNonContext", file, err)
		}
	}
	overlay, err := PlanOverlay(base, memSource{"CLAUDE.md": "See @docs/guide.md\n", "docs/guide.md": "new guide\n",
		".claude/settings.json": "{\"permissions\":{\"allow\":[\"Bash\"]}}\n", ".claude/hooks/pre.sh*": "#!/bin/sh\n"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".claude/hooks/pre.sh", ".claude/settings.json", ".mcp.json"}; !slices.Equal(overlay.HarnessChanged, want) {
		t.Errorf("harness changed = %v, want %v", overlay.HarnessChanged, want)
	}
	if !slices.Equal(overlay.Deletes, []string{".mcp.json"}) { // package.json stays: the base only imported it
		t.Errorf("deletes = %v", overlay.Deletes)
	}
}

func TestBuildIncludesDocumentsAndOddPaths(t *testing.T) {
	ctx := context.Background()
	bare := bareRepo(t)
	src := memSource{
		"CLAUDE.md":                          "Read [testing](.agents/testing.md)\n",
		".agents/testing.md":                 "testing\n",
		".claude/skills/r/SKILL.md":          "---\nname: r\ndescription: d\n---\n",
		".claude/skills/r/tab\tand\nline.md": "odd name\n", // must not corrupt the index input
		"main.go":                            "package main\n", "requirements.txt": "django==5\n", "CMakeLists.txt": "project(x)\n",
	}
	commit, manifest, err := Build(ctx, bare, src, "with include", []string{".agents/testing.md", "CLAUDE.md"})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitx.Output(ctx, nil, "--git-dir", bare, "ls-tree", "-r", "-z", "--name-only", commit)
	if err != nil {
		t.Fatal(err)
	}
	want := ".agents/testing.md\x00.claude/skills/r/SKILL.md\x00.claude/skills/r/tab\tand\nline.md\x00CLAUDE.md\x00"
	if string(tree) != want {
		t.Errorf("tree = %q, want %q", tree, want)
	}
	if !slices.Contains(manifest.Paths(), ".agents/testing.md") || manifest.StartupBytes != len("Read [testing](.agents/testing.md)\n")+len("r")+len("d") {
		t.Errorf("manifest = %+v", manifest)
	}
	for _, bad := range []string{"main.go", "missing.md", "requirements.txt", "CMakeLists.txt"} {
		if _, _, err := Build(ctx, bare, src, "bad include", []string{bad}); err == nil || !strings.Contains(err.Error(), "--include "+bad) {
			t.Errorf("--include %s: err = %v", bad, err)
		}
	}
}

func TestUncapturedChangesListsNonContextEdits(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = gitx.Environ(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for name, body := range map[string]string{"CLAUDE.md": "a\n", "main.go": "package main\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "initial")
	for name, body := range map[string]string{"CLAUDE.md": "b\n", "main.go": "package main // edited\n", "new dir/notes.txt": "x\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	changes, err := UncapturedChanges(context.Background(), root, []string{"CLAUDE.md"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main.go", "new dir/notes.txt"}; !slices.Equal(changes, want) {
		t.Errorf("uncaptured = %q, want %q", changes, want)
	}
}

func TestValidName(t *testing.T) {
	for name, ok := range map[string]bool{"baseline": true, "v2.1_min-ctx": true, "0": true, "": false, "Upper": false,
		"-lead": false, "has space": false, "a/b": false, strings.Repeat("a", 63): true, strings.Repeat("a", 64): false,
		"a..b": false, "x.lock": false, "x.": false} {
		if ValidName(name) != ok {
			t.Errorf("ValidName(%q) = %v, want %v", name, !ok, ok)
		}
	}
}
