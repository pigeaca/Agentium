package claudectx

import (
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
)

// memSource is an in-memory repository state.
type memSource map[string]string

func (m memSource) Paths() []string {
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}
func (m memSource) ReadFile(p string) ([]byte, error) {
	if data, ok := m[p]; ok {
		return []byte(data), nil
	}
	return nil, os.ErrNotExist
}
func (m memSource) Executable(string) bool { return false }
func (m memSource) Describe() string       { return "memory" }

func resolve(t *testing.T, files memSource) Context {
	t.Helper()
	ctx, err := Resolve(files)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func kinds(ctx Context) map[string]string {
	out := map[string]string{}
	for _, e := range ctx.Entries {
		out[e.Path] = e.Kind
	}
	return out
}

func hasWarning(ctx Context, fragment string) bool {
	for _, w := range ctx.Warnings {
		if strings.Contains(w, fragment) {
			return true
		}
	}
	return false
}

func TestClaudeMdWithImportsRulesSkillsAndHarness(t *testing.T) {
	ctx := resolve(t, memSource{
		"CLAUDE.md":                          "# Project\n@AGENTS.md\n@.agents/core.md and see `@not/an/import.md`\n```\n@docs/in-fence.md\n```\nmail me@example.com, ping @alice\n",
		"AGENTS.md":                          "Agents\n",
		".agents/core.md":                    "Core rules\n@../../outside.md\n@~/personal.md\n@missing.md\n",
		".claude/rules/go.md":                "Always gofmt\n",
		".claude/rules/web.md":               "---\npaths: web/**\n---\nUse React\n",
		".claude/skills/review/SKILL.md":     "---\nname: review\ndescription: Review a change\n---\nLong body\n",
		".claude/skills/review/checklist.md": "items\n",
		".claude/agents/reviewer.md":         "---\nname: reviewer\ndescription: Reads code\n---\nbody\n",
		".claude/commands/ship.md":           "Ship it\nsteps\n",
		".claude/settings.json":              "{}",
		".mcp.json":                          "{}",
		"web/CLAUDE.md":                      "web rules\n",
		"main.go":                            "package main\n",
		"docs/in-fence.md":                   "not imported\n",
	})
	want := map[string]string{
		"CLAUDE.md": KindInstructions, "AGENTS.md": KindImport, ".agents/core.md": KindImport,
		".claude/rules/go.md": KindRule, ".claude/rules/web.md": KindScopedRule,
		".claude/skills/review/SKILL.md": KindSkill, ".claude/skills/review/checklist.md": KindSkillFile,
		".claude/agents/reviewer.md": KindSubagent, ".claude/commands/ship.md": KindCommand,
		".claude/settings.json": KindHarness, ".mcp.json": KindHarness, "web/CLAUDE.md": KindNested,
	}
	got := kinds(ctx)
	if len(got) != len(want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	for p, kind := range want {
		if got[p] != kind {
			t.Errorf("%s kind = %q, want %q", p, got[p], kind)
		}
	}
	if ctx.Entries[0].Path != "CLAUDE.md" || ctx.Entries[1].Path != "AGENTS.md" || ctx.Entries[1].Via != "CLAUDE.md" {
		t.Errorf("load order = %+v", ctx.Entries[:3])
	}
	for _, fragment := range []string{"imports ../../outside.md, outside the repository", "imports ~/personal.md", "imports .agents/missing.md, which does not exist"} {
		if !hasWarning(ctx, fragment) {
			t.Errorf("warnings %q lack %q", ctx.Warnings, fragment)
		}
	}
	// Startup: the three instruction files, the unscoped rule, and the descriptions only.
	wantStartup := len("# Project\n@AGENTS.md\n@.agents/core.md and see `@not/an/import.md`\n```\n@docs/in-fence.md\n```\nmail me@example.com, ping @alice\n") +
		len("Agents\n") + len("Core rules\n@../../outside.md\n@~/personal.md\n@missing.md\n") + len("Always gofmt\n") +
		len("review") + len("Review a change") + len("reviewer") + len("Reads code") + len("Ship it")
	if ctx.StartupBytes() != wantStartup {
		t.Errorf("startup bytes = %d, want %d", ctx.StartupBytes(), wantStartup)
	}
}

func TestAgentsMdLoadsOnlyWithoutClaudeMd(t *testing.T) {
	ctx := resolve(t, memSource{"AGENTS.md": "Rules\n@docs/a.md\n", "docs/a.md": "A\n"})
	if got := kinds(ctx); got["AGENTS.md"] != KindInstructions || got["docs/a.md"] != KindImport {
		t.Errorf("AGENTS.md fallback: %v", got)
	}
	ctx = resolve(t, memSource{"CLAUDE.md": "Claude only\n", "AGENTS.md": "Rules\n"})
	if _, loaded := kinds(ctx)["AGENTS.md"]; loaded || !hasWarning(ctx, "AGENTS.md is not loaded") {
		t.Errorf("AGENTS.md should be unloaded with a warning: %v %q", kinds(ctx), ctx.Warnings)
	}
	ctx = resolve(t, memSource{"main.go": "package main\n"})
	if len(ctx.Entries) != 0 || !hasWarning(ctx, "loads no project instructions") {
		t.Errorf("no instructions: %+v", ctx)
	}
}

func TestAgentsMdWarningWithClaudeMdVariants(t *testing.T) {
	const warning = "AGENTS.md is not loaded"
	// A symbolic link is followed by the source, so CLAUDE.md -> AGENTS.md reads as the same bytes.
	if ctx := resolve(t, memSource{"CLAUDE.md": "Rules\n", "AGENTS.md": "Rules\n"}); hasWarning(ctx, warning) {
		t.Errorf("CLAUDE.md linked to AGENTS.md warned: %q", ctx.Warnings)
	}
	if ctx := resolve(t, memSource{"CLAUDE.md": "Other\n", "AGENTS.md": "Rules\n"}); !hasWarning(ctx, warning) {
		t.Errorf("a separate CLAUDE.md lost the warning: %q", ctx.Warnings)
	}
	if ctx := resolve(t, memSource{"CLAUDE.md": "@AGENTS.md\n", "AGENTS.md": "Rules\n"}); hasWarning(ctx, warning) {
		t.Errorf("an importing CLAUDE.md warned: %q", ctx.Warnings)
	}
}

func TestImportDepthCyclesAndPersonalFiles(t *testing.T) {
	ctx := resolve(t, memSource{
		"CLAUDE.md": "@a.md\n", "a.md": "@b.md\n", "b.md": "@c.md\n", "c.md": "@d.md\n", "d.md": "@e.md\n", "e.md": "@f.md\n@CLAUDE.md\nthanks @alice\n", "f.md": "too deep\n",
		"CLAUDE.local.md": "mine\n", ".claude/settings.local.json": "{}", ".claude/skills": "../.agents/skills",
	})
	got := kinds(ctx)
	for _, p := range []string{"a.md", "b.md", "c.md", "d.md", "e.md"} {
		if got[p] != KindImport {
			t.Errorf("%s should be imported (within five hops): %v", p, got)
		}
	}
	if _, loaded := got["f.md"]; loaded || !hasWarning(ctx, "beyond Claude Code's 5-hop limit") {
		t.Errorf("f.md is the sixth hop: %v %q", got, ctx.Warnings)
	}
	if hasWarning(ctx, "alice") {
		t.Errorf("a mention at the sixth hop is not an import: %q", ctx.Warnings)
	}
	if _, loaded := got["CLAUDE.local.md"]; loaded || !hasWarning(ctx, "CLAUDE.local.md is personal") || !hasWarning(ctx, "settings.local.json is personal") {
		t.Errorf("personal files: %v %q", got, ctx.Warnings)
	}
	if !hasWarning(ctx, "e.md imports CLAUDE.md, which imports it back (an import cycle)") {
		t.Errorf("cycle not reported: %q", ctx.Warnings)
	}
	if !hasWarning(ctx, ".claude/skills is a symbolic link to a directory") {
		t.Errorf("directory symlink not reported: %q", ctx.Warnings)
	}
}

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens(0) != 0 || EstimateTokens(1) != 1 || EstimateTokens(4000) != 1000 {
		t.Error("estimate is about four bytes per token")
	}
}

func TestSharedImportIsNotACycle(t *testing.T) {
	ctx := resolve(t, memSource{"CLAUDE.md": "@a.md\n@b.md\n", "a.md": "@shared.md\n", "b.md": "@shared.md\n", "shared.md": "S\n"})
	if len(ctx.Warnings) != 0 || kinds(ctx)["shared.md"] != KindImport || ctx.StartupBytes() != len("@a.md\n@b.md\n")+2*len("@shared.md\n")+len("S\n") {
		t.Errorf("shared import: %+v", ctx)
	}
}

func TestImportsFromRulesAndNestedFiles(t *testing.T) {
	ctx := resolve(t, memSource{
		"CLAUDE.md":             "Root\n",
		".claude/rules/a.md":    "@../../docs/guide.md\n",
		".claude/rules/web.md":  "---\r\npaths: web/**\r\n---\r\n@../../docs/react.md\r\n", // CRLF frontmatter still scopes it
		"web/CLAUDE.md":         "@notes.md\n",
		"docs/guide.md":         "guide\n",
		"docs/react.md":         "react\n",
		"web/notes.md":          "notes\n",
		"docs/shared.md":        "shared\n",
		".claude/rules/both.md": "---\npaths: x/**\n---\n@../../docs/shared.md\n",
		".claude/rules/z.md":    "@../../docs/shared.md\n", // a startup rule wins over the scoped one
	})
	byPath := map[string]Entry{}
	for _, e := range ctx.Entries {
		byPath[e.Path] = e
	}
	for p, want := range map[string]struct {
		via     string
		startup bool
	}{
		"docs/guide.md":  {".claude/rules/a.md", true},
		"docs/react.md":  {".claude/rules/web.md", false},
		"web/notes.md":   {"web/CLAUDE.md", false},
		"docs/shared.md": {".claude/rules/z.md", true},
	} {
		e, ok := byPath[p]
		if !ok || e.Kind != KindImport || e.Via != want.via || (e.StartupBytes > 0) != want.startup {
			t.Errorf("%s = %+v, want import via %s, startup %v", p, e, want.via, want.startup)
		}
	}
	if byPath[".claude/rules/web.md"].Kind != KindScopedRule {
		t.Errorf("CRLF frontmatter not parsed: %+v", byPath[".claude/rules/web.md"])
	}
}

func TestBareImportsAndLinkedFiles(t *testing.T) {
	ctx := resolve(t, memSource{
		"CLAUDE.md": "See @README and @Makefile; ask @alice.\n" +
			"Read [testing](.agents/testing.md#rules), [the guide](docs/guide.md \"Guide\"), [site](https://example.com), [top](#top).\n" +
			"Also [go.mod](go.mod), [run](scripts/run.sh) and [fixture](testdata/case.md).\n" +
			"@docs/guide.md\n```\n[in code](.agents/fenced.md)\n```\n",
		"README":             "readme\n",
		"Makefile":           "test:\n",
		".agents/testing.md": "testing rules\n",
		".agents/fenced.md":  "x\n",
		"docs/guide.md":      "guide\n",
		"go.mod":             "module x\n",
		"scripts/run.sh":     "#!/bin/sh\n",
		"testdata/case.md":   "fixture\n",
	})
	got := kinds(ctx)
	if got["README"] != KindImport || got["Makefile"] != KindImport {
		t.Errorf("bare imports of existing files: %v", got)
	}
	if hasWarning(ctx, "alice") {
		t.Errorf("a mention is not a missing import: %q", ctx.Warnings)
	}
	if want := []string{".agents/testing.md"}; strings.Join(ctx.Linked, ",") != strings.Join(want, ",") {
		t.Errorf("linked = %v, want %v (documents only: not imported files, code, test data, URLs, anchors or fenced links)", ctx.Linked, want)
	}
}

func TestSkillNames(t *testing.T) {
	src := memSource{
		"CLAUDE.md":                       "x\n",
		".claude/skills/review/SKILL.md":  "---\nname: code-review\ndescription: d\n---\n",
		".claude/skills/deploy/SKILL.md":  "no frontmatter\n",
		".claude/skills/review/extra.md":  "not a skill\n",
		".claude/skills/deploy2/SKILL.md": "---\nname: deploy\n---\n",
	}
	got := SkillNames(resolve(t, src), src)
	if want := []string{"code-review", "deploy", "deploy2", "review"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("skill names = %v, want %v", got, want)
	}
}

func TestRuleGlobs(t *testing.T) {
	for _, c := range []struct {
		data string
		want []string
	}{
		{"---\npaths: \"src/**/*.ts\"\n---\nx\n", []string{"src/**/*.ts"}},
		{"---\npaths: [\"*.go\", 'db/*.sql']\n---\n", []string{"*.go", "db/*.sql"}},
		{"---\npaths: [\"src/*.{ts,tsx}\", db/*.sql]\n---\n", []string{"src/*.{ts,tsx}", "db/*.sql"}},
		{"---\nname: x\npaths:\n  - \"a/**\"\n  - b/*.md\ndescription: y\n---\n", []string{"a/**", "b/*.md"}},
		{"---\nname: x\n---\n", nil},
		{"no frontmatter\n", nil},
	} {
		if got := RuleGlobs([]byte(c.data)); !slices.Equal(got, c.want) {
			t.Errorf("%q: %v, want %v", c.data, got, c.want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "pkg/sub/x.go", true},
		{"src/**/*.ts", "src/a/b.ts", true},
		{"src/**/*.ts", "src/b.ts", true},
		{"src/**/*.ts", "lib/b.ts", false},
		{"*.go", "pkg/x.go", true}, // no slash: a name in any folder
		{"db/*.sql", "db/a.sql", true},
		{"db/*.sql", "db/old/a.sql", false},
		{"src/{api,web}/*.ts", "src/web/x.ts", true},
		{"src/{api,web}/*.ts", "src/cli/x.ts", false},
		{"doc?.md", "docs.md", true},
		{"a/**", "a/b/c", true},
	} {
		if got := GlobMatch(c.pattern, c.path); got != c.want {
			t.Errorf("%q vs %q: %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestIsHarnessIsWhatResolveClassifiesAsHarness(t *testing.T) {
	files := memSource{
		".claude/settings.json": "{}", ".mcp.json": "{}", ".claude/hooks/pre.sh": "#!/bin/sh\n", ".claude/hooks/lib/x.py": "",
		".claude/settings.local.json": "{}", ".claude/rules/a.md": "rule\n", "pkg/.mcp.json": "{}", "CLAUDE.md": "hi\n",
	}
	ctx := resolve(t, files)
	for p := range files {
		if got, want := IsHarness(p), kinds(ctx)[p] == KindHarness; got != want {
			t.Errorf("IsHarness(%s) = %v, but Resolve's kind is %q", p, got, kinds(ctx)[p])
		}
	}
	for _, p := range []string{".Claude/settings.json", ".claude/hooks", "pkg/.claude/settings.json"} {
		if IsHarness(p) {
			t.Errorf("IsHarness(%s) = true; the exact paths only (callers fold variants)", p)
		}
	}
}

func TestHarnessFrontmatter(t *testing.T) {
	cases := []struct {
		name, text string
		fields     []string
		doubt      bool
	}{
		{"plain", "---\nname: a\ndescription: b\n---\nhooks: in the body is prose\n", nil, false},
		{"hooks", "---\nname: a\nhooks:\n  PreToolUse:\n    - command: ./x.sh\n---\n", []string{"hooks"}, false},
		{"CRLF", "---\r\nallowed-tools: Bash(curl:*)\r\npermissionMode: bypassPermissions\r\n---\r\n", []string{"allowedtools", "permissionmode"}, false},
		{"quoted key after a BOM", "\ufeff---\n\"mcpServers\": {}\n---\n", []string{"mcpservers"}, false},
		{"single-quoted key", "---\n'hooks': x\n---\n", []string{"hooks"}, false},
		{"tools and model are not harness", "---\ntools: [Read, Grep]\nmodel: opus\n---\n", nil, false},
		{"no frontmatter", "no frontmatter\nhooks: x\n", nil, false},
		{"nested", "---\nnested:\n  allowed_tools: x\n---\n", []string{"allowedtools"}, false},
		{"in a sequence", "---\nlist:\n  - memory: project\n---\n", []string{"memory"}, false},
		{"opening line with a trailing space", "--- \nhooks: x\n---\n", []string{"hooks"}, false},
		{"closing line with a trailing space", "---\nname: a\n--- \nbody\n", nil, false},
		// Fail closed: forms a YAML parser reads as keys, but this reader does not parse.
		{"flow mapping", "---\n{name: a, hooks: {Stop: []}}\n---\n", nil, true},
		{"flow mapping in a sequence", "---\n- {hooks: x}\n---\n", nil, true},
		{"escaped quoted key", "---\n\"permission\\x4dode\": bypassPermissions\n---\n", nil, true},
		{"single-quote escape", "---\n'hoo''ks': x\n---\n", nil, true},
		{"tag", "---\n!!str hooks: x\n---\n", nil, true},
		{"explicit key", "---\n? hooks\n: x\n---\n", nil, true},
		{"merge key", "---\nbase: &a\n  name: x\n<<: *a\n---\n", nil, true},
		{"anchor at a key", "---\n&a hooks: x\n---\n", nil, true},
		{"directive", "---\n%YAML 1.2\n---\n", nil, true},
		{"a line that may end the frontmatter early", "---\nname: a\n---x\nhooks: x\n---\n", nil, true},
		{"document end marker", "---\nname: a\n...\nhooks: x\n---\n", nil, true},
		{"not closed", "---\nname: a\nhooks: x\n", []string{"hooks"}, true},
		{"after blank lines", "\n\n---\nhooks: x\n---\n", nil, true},
		{"indentation indicator larger than the text", "---\ndescription: |5\n  hooks: x\n---\n", []string{"hooks"}, false},
		{"--- followed by text", "---x\nhooks: x\n---\n", nil, true},
		{"lone CR", "---\nname: reviewer\rpermissionMode: bypassPermissions\n---\nbody\n", nil, true},
		{"NEL", "---\nname: reviewer\u0085permissionMode: bypassPermissions\n---\n", nil, true},
		{"line separator", "---\nname: reviewer\u2028permissionMode: bypassPermissions\n---\n", nil, true},
		{"paragraph separator", "---\nname: reviewer\u2029permissionMode: bypassPermissions\n---\n", nil, true},
		{"CR before CRLF", "---\r\nname: reviewer\r\r\n---\r\n", nil, true},
		// Block scalars: their more indented lines are text, however they start.
		{"literal block of Markdown", "---\nname: r\ndescription: |\n  - **reviewing** code\n  * `diffs`\n---\n", nil, false},
		{"folded block with a link", "---\nname: r\ndescription: >-\n  [the role](x.md)\n  more\n\n  after a blank line\n---\n", nil, false},
		{"block with a chomping indicator and a comment", "---\ndescription: |+ # text\n    ? not a key\n---\n", nil, false},
		// An explicit indentation indicator is not trusted: its lines are scanned as keys (fail closed).
		{"block with an indentation indicator", "---\ndescription: |2+ # text\n    ? not a key\n---\n", nil, true},
		{"block in a sequence item", "---\nitems:\n  - description: |\n      * bullet\n  - name: x\n---\n", nil, false},
		{"hooks after a block", "---\ndescription: |\n  - **reviewing** code\nhooks:\n  Stop: []\n---\n", []string{"hooks"}, false},
		{"hooks inside a block is text", "---\ndescription: |\n  hooks: x\n---\n", nil, false},
		{"tab where a block may end", "---\ndescription: |\n  text\n\thooks: x\n---\n", []string{"hooks"}, true},
		{"dedent inside a block", "---\ndescription: |\n    text\n  hooks: x\n---\n", nil, true},
		{"multi-line quoted key", "---\n\"hooks\n  \": x\n---\n", nil, true},
	}
	for _, c := range cases {
		got := HarnessFrontmatter([]byte(c.text))
		if !slices.Equal(got.Fields, c.fields) || (got.Doubt != "") != c.doubt {
			t.Errorf("%s: fields %v, doubt %q; want %v, doubt %v", c.name, got.Fields, got.Doubt, c.fields, c.doubt)
		}
		if got.Harness() != (len(c.fields) > 0 || c.doubt) {
			t.Errorf("%s: Harness() = %v", c.name, got.Harness())
		}
	}
	if got := HarnessFrontmatter([]byte("---\nhooks: {Stop: [sh run.sh]}\n---\nsee other.sh\n")); got.Text != "hooks: {Stop: [sh run.sh]}" {
		t.Errorf("Text = %q, want the frontmatter only", got.Text)
	}
}

func TestFrontmatterDelimiters(t *testing.T) {
	for text, want := range map[string][]string{
		"---\npaths: a\n---\nbody\n":         {"paths: a"},
		"--- \t\npaths: a\n---  \nbody\n":    {"paths: a"},
		"---\npaths: a\n---x\nb: c\n---\n":   {"paths: a", "---x", "b: c"},
		"---x\npaths: a\n---\n":              nil,
		"---\npaths: a\n":                    nil,
		"text\n---\npaths: a\n---\n":         nil,
		"---\r\npaths: a\r\n---\r\nbody\r\n": {"paths: a"},
	} {
		if got := frontmatter([]byte(text)); !slices.Equal(got, want) {
			t.Errorf("frontmatter(%q) = %q, want %q", text, got, want)
		}
	}
}
