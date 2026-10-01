package claudectx

import (
	"strings"
	"testing"
)

func TestLintContextSeparatesProblemsFromWarnings(t *testing.T) {
	t.Parallel()
	l, err := LintContext(memSource{
		"CLAUDE.md":              "@docs/missing.md\n" + strings.Repeat("x", MaxStartupBytes),
		"CLAUDE.local.md":        "personal",
		".claude/settings.json":  "{}",
		".claude/rules/style.md": "Be brief.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Problems) != 2 || !strings.Contains(l.Problems[0], "docs/missing.md") || !strings.Contains(l.Problems[1], "32 KiB cap") {
		t.Errorf("problems: %q", l.Problems)
	}
	if len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "CLAUDE.local.md") {
		t.Errorf("a broken import must not repeat in warnings: %q", l.Warnings)
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
