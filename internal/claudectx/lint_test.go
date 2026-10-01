package claudectx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintContextSeparatesProblemsFromWarnings(t *testing.T) {
	t.Parallel()
	l, err := LintContext(memSource{
		"CLAUDE.md":              "@docs/missing.md\n" + strings.Repeat("x", 3*CodexDocMaxBytes),
		"AGENTS.md":              strings.Repeat("x", CodexDocMaxBytes+1),
		"pkg/AGENTS.md":          "small",
		"CLAUDE.local.md":        "personal",
		".claude/settings.json":  "{}",
		".claude/rules/style.md": "Be brief.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A large CLAUDE.md is information (the size change), not a problem; only AGENTS.md has Codex's limit.
	if len(l.Problems) != 2 || !strings.Contains(l.Problems[0], "docs/missing.md") ||
		!strings.Contains(l.Problems[1], "the AGENTS.md files Codex loads for the repository root total 32.0 KiB; Codex reads only the first 32 KiB (AGENTS.md)") || strings.Contains(l.Problems[1], "pkg/") {
		t.Errorf("problems: %q", l.Problems)
	}
	if len(l.Warnings) != 2 || !strings.Contains(strings.Join(l.Warnings, "\n"), "CLAUDE.local.md") || strings.Contains(strings.Join(l.Warnings, "\n"), "missing.md") {
		t.Errorf("a broken import must not repeat in warnings: %q", l.Warnings)
	}
	if l.Contains("AGENTS.md") || !l.Reaches(t.TempDir(), "AGENTS.md") {
		t.Errorf("an AGENTS.md that is not loaded is not in Files, but an edit of it is still worth a lint")
	}
	if !l.Contains(".claude/rules/style.md") || !l.Contains("./CLAUDE.md") || l.Contains(".claude/settings.json") || l.Contains("main.go") {
		t.Errorf("context files: %q", l.Files)
	}
}

func TestParseHookPayload(t *testing.T) {
	t.Parallel()
	p, ok, err := ParseHookPayload([]byte(`{"tool_name":"Write","cwd":"/r","tool_input":{"file_path":"/r/CLAUDE.md","content":"x"}}`))
	if err != nil || !ok || p.ToolInput.FilePath != "/r/CLAUDE.md" || p.Cwd != "/r" {
		t.Errorf("edit payload: %+v %v %v", p, ok, err)
	}
	if _, ok, err := ParseHookPayload([]byte(`{"tool_name":"Read","tool_input":{"file_path":"/r/CLAUDE.md"}}`)); ok || err != nil {
		t.Errorf("a Read is not an edit: %v %v", ok, err)
	}
	if _, _, err := ParseHookPayload([]byte(`{`)); err == nil {
		t.Error("malformed JSON must be an error")
	}
}

func TestReachesFollowsASymlinkedContextFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for p, body := range map[string]string{"docs/rules.md": "Be brief.\n", "src/main.go": "package main\n"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".claude/rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../docs/rules.md", filepath.Join(root, ".claude/rules/style.md")); err != nil {
		t.Fatal(err)
	}
	l := Lint{Files: []string{".claude/rules/style.md"}}
	if !l.Reaches(root, "docs/rules.md") {
		t.Error("editing the target of a symlinked rule changes the context")
	}
	if l.Reaches(root, "src/main.go") {
		t.Error("an unrelated file is not context")
	}
}

func TestCodexChainsCountFilesFromTheRootDown(t *testing.T) {
	t.Parallel()
	half := strings.Repeat("x", 20*1024)
	l, err := LintContext(memSource{
		"AGENTS.md":                  half,
		"api/AGENTS.md":              half,
		"api/v1/AGENTS.md":           half, // the chain was already cut off above: not repeated
		"web/AGENTS.md":              "small",
		"vendor/lib/AGENTS.md":       strings.Repeat("x", 3*CodexDocMaxBytes),
		"testdata/fixture/AGENTS.md": strings.Repeat("x", 3*CodexDocMaxBytes),
		"tools/AGENTS.md":            half, // a sibling chain: root + tools is over as well
	})
	if err != nil {
		t.Fatal(err)
	}
	var chains []string
	for _, p := range l.Problems {
		if strings.Contains(p, "Codex loads for") {
			chains = append(chains, p)
		}
	}
	if len(chains) != 2 || !strings.Contains(chains[0], "for api total 40.0 KiB") || !strings.Contains(chains[0], "(AGENTS.md, api/AGENTS.md)") ||
		!strings.Contains(chains[1], "for tools total 40.0 KiB") {
		t.Errorf("chains: %q", chains)
	}
}
