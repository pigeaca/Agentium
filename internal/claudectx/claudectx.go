// Package claudectx works out which project files Claude Code loads, as experiments run it: from the repository root,
// with project settings only (so CLAUDE.local.md, user files and settings.local.json never load).
//
// Rules, from Claude Code's memory, skills and settings docs (checked 2026-09-28, CLI 2.1.281):
//   - CLAUDE.md and .claude/CLAUDE.md at the root load at session start. If neither exists, AGENTS.md loads instead.
//   - @path imports in those files load too, recursively up to five hops, but not inside code spans or blocks. Rules and
//     nested CLAUDE.md files are memory files as well: their imports load when they do.
//   - .claude/rules/**/*.md load at start, unless their frontmatter scopes them with paths:, then on demand.
//   - Skill, subagent and command descriptions load at start; their bodies load on demand.
//   - CLAUDE.md files in subdirectories load on demand, when files there are read.
//   - .claude/settings.json, .claude/hooks and .mcp.json change what runs, not what the model reads.
package claudectx

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// Kinds of context file.
const (
	KindInstructions = "instructions" // root CLAUDE.md, .claude/CLAUDE.md, or AGENTS.md
	KindImport       = "import"       // reached through @path
	KindRule         = "rule"         // .claude/rules, loaded at start
	KindScopedRule   = "scoped-rule"  // .claude/rules with paths: frontmatter, loaded on demand
	KindSkill        = "skill"        // .claude/skills/*/SKILL.md: description at start, body on demand
	KindSkillFile    = "skill-file"   // other files in a skill folder, read on demand
	KindSubagent     = "subagent"     // .claude/agents/*.md: description at start
	KindCommand      = "command"      // .claude/commands/**/*.md: description at start
	KindNested       = "nested"       // CLAUDE.md or AGENTS.md in a subdirectory, loaded on demand
	KindHarness      = "harness"      // settings, hooks, MCP servers: change what runs, not what the model reads
)

// MaxImportDepth is how many @import hops Claude Code follows.
const MaxImportDepth = 5

// Entry is one file of the context.
type Entry struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Bytes        int    `json:"bytes"`         // whole file
	StartupBytes int    `json:"startup_bytes"` // loaded at session start: the whole file, a description, or 0
	Via          string `json:"via,omitempty"` // the importing file, for imports
}

// Context is what Claude Code would load from a repository state.
type Context struct {
	Entries  []Entry  `json:"entries"`
	Warnings []string `json:"warnings"`
	// Linked are documents (see IsDocument) that context files link to ([text](path)) but do not load: the agent reads
	// them only if it opens them, so they are not context unless a snapshot includes them explicitly.
	Linked []string `json:"linked,omitempty"`
}

// StartupBytes is the total loaded at session start.
func (c Context) StartupBytes() int {
	total := 0
	for _, e := range c.Entries {
		total += e.StartupBytes
	}
	return total
}

// Paths are every context file (startup, on demand and harness), which is what a snapshot captures.
func (c Context) Paths() []string {
	paths := make([]string, 0, len(c.Entries))
	for _, e := range c.Entries {
		paths = append(paths, e.Path)
	}
	sort.Strings(paths)
	return paths
}

// EstimateTokens is a rough estimate (about four bytes per token); runs measure the real first-request size.
func EstimateTokens(bytes int) int {
	return (bytes + 3) / 4
}

// Resolve works out the context of src.
func Resolve(src source.Source) (Context, error) {
	r := &resolver{src: src, seen: map[string]bool{}}
	if err := r.resolve(); err != nil {
		return Context{}, err
	}
	sort.SliceStable(r.entries, func(i, j int) bool { return kindOrder(r.entries[i].Kind) < kindOrder(r.entries[j].Kind) })
	return Context{Entries: r.entries, Warnings: r.warnings, Linked: r.linked()}, nil
}

// kindOrder sorts entries for display: startup instructions in load order first, then the rest by kind.
func kindOrder(kind string) int {
	switch kind {
	case KindInstructions, KindImport:
		return 0
	case KindRule:
		return 1
	case KindSkill:
		return 2
	case KindSubagent:
		return 3
	case KindCommand:
		return 4
	case KindScopedRule:
		return 5
	case KindSkillFile:
		return 6
	case KindNested:
		return 7
	default:
		return 8
	}
}

type resolver struct {
	src       source.Source
	seen      map[string]bool
	entries   []Entry
	warnings  []string
	importers []importer
}

// importer is a rule or nested instruction file whose @imports load with it.
type importer struct {
	path    string
	data    []byte
	startup bool
}

func (r *resolver) warn(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

func (r *resolver) add(p, kind string, data []byte, startup int, via string) {
	r.seen[p] = true
	r.entries = append(r.entries, Entry{Path: p, Kind: kind, Bytes: len(data), StartupBytes: startup, Via: via})
}

func (r *resolver) read(p string) ([]byte, bool) {
	if !source.Has(r.src, p) {
		return nil, false
	}
	data, err := r.src.ReadFile(p)
	if err != nil {
		r.warn("%s could not be read: %v", p, err)
		return nil, false
	}
	return data, true
}

func (r *resolver) resolve() error {
	var roots []string
	for _, name := range []string{"CLAUDE.md", ".claude/CLAUDE.md"} {
		if source.Has(r.src, name) {
			roots = append(roots, name)
		}
	}
	agentsLoaded := len(roots) == 0 && source.Has(r.src, "AGENTS.md")
	if agentsLoaded {
		roots = []string{"AGENTS.md"}
	}
	if len(roots) == 0 {
		r.warn("No CLAUDE.md, .claude/CLAUDE.md or AGENTS.md at the repository root: Claude Code loads no project instructions.")
	}
	for _, name := range roots {
		if data, ok := r.read(name); ok {
			r.add(name, KindInstructions, data, len(data), "")
			r.imports([]string{name}, data, true)
		}
	}
	if source.Has(r.src, "AGENTS.md") && !agentsLoaded && !r.seen["AGENTS.md"] {
		r.warn("AGENTS.md is not loaded by Claude Code: a CLAUDE.md exists and does not import it (@AGENTS.md).")
	}
	if source.Has(r.src, "CLAUDE.local.md") {
		r.warn("CLAUDE.local.md is personal: experiments run with project settings only and never load it.")
	}
	if source.Has(r.src, ".claude/settings.local.json") {
		r.warn(".claude/settings.local.json is personal: experiments never load it.")
	}
	for _, dir := range []string{".claude/rules", ".claude/skills", ".claude/agents", ".claude/commands"} {
		if source.Has(r.src, dir) { // git stores a symbolic link to a directory as one file
			r.warn("%s is a symbolic link to a directory: Agentium does not follow it yet, so its files are not in this context.", dir)
		}
	}
	for _, p := range r.src.Paths() {
		if r.seen[p] {
			continue
		}
		r.classify(p)
	}
	// Startup rules first, so a file that both they and an on-demand file import counts as loaded at start.
	for _, startup := range []bool{true, false} {
		for _, imp := range r.importers {
			if imp.startup == startup {
				r.imports([]string{imp.path}, imp.data, startup)
			}
		}
	}
	return nil
}

// classify places a file that was not reached through the instruction files.
func (r *resolver) classify(p string) {
	switch {
	case strings.HasPrefix(p, ".claude/rules/") && strings.HasSuffix(p, ".md"):
		data, ok := r.read(p)
		if !ok {
			return
		}
		startup := frontmatterField(data, "paths") == "" && !frontmatterHasKey(data, "paths")
		if startup {
			r.add(p, KindRule, data, len(data), "")
		} else {
			r.add(p, KindScopedRule, data, 0, "")
		}
		r.importers = append(r.importers, importer{p, data, startup})
	case strings.HasPrefix(p, ".claude/skills/"):
		data, ok := r.read(p)
		if !ok {
			return
		}
		if path.Base(p) == "SKILL.md" && strings.Count(p, "/") == 3 {
			r.add(p, KindSkill, data, descriptionBytes(data), "")
		} else {
			r.add(p, KindSkillFile, data, 0, "")
		}
	case strings.HasPrefix(p, ".claude/agents/") && strings.HasSuffix(p, ".md"):
		if data, ok := r.read(p); ok {
			r.add(p, KindSubagent, data, descriptionBytes(data), "")
		}
	case strings.HasPrefix(p, ".claude/commands/") && strings.HasSuffix(p, ".md"):
		if data, ok := r.read(p); ok {
			r.add(p, KindCommand, data, descriptionBytes(data), "")
		}
	case p == ".claude/settings.json" || p == ".mcp.json" || strings.HasPrefix(p, ".claude/hooks/"):
		if data, ok := r.read(p); ok {
			r.add(p, KindHarness, data, 0, "")
		}
	case (path.Base(p) == "CLAUDE.md" || path.Base(p) == "AGENTS.md") && strings.Contains(p, "/") && !strings.HasPrefix(p, ".claude/"):
		if data, ok := r.read(p); ok {
			r.add(p, KindNested, data, 0, "")
			r.importers = append(r.importers, importer{p, data, false})
		}
	}
}

// LoadsByPresence reports whether a file at p is context just by existing (instruction files, .claude, .mcp.json), as
// opposed to a file that loads only when something imports it.
func LoadsByPresence(p string) bool {
	base := path.Base(p)
	return p == ".mcp.json" || strings.HasPrefix(p, ".claude/") || base == "CLAUDE.md" || base == "AGENTS.md"
}

// importPattern finds @path tokens: "@" at the start of a line or after whitespace, followed by a path.
var importPattern = regexp.MustCompile(`(?:^|\s)@([^\s` + "`" + `]+)`)

// imports loads the @imports of the last file in chain (the files that led to it, root first); startup says whether
// that file loads at session start, and so its imports do.
func (r *resolver) imports(chain []string, data []byte, startup bool) {
	from, depth := chain[len(chain)-1], len(chain)
	for _, target := range importTargets(string(data)) {
		pathLike := strings.ContainsAny(target, "/.") // @README is a file if it exists; @alice is a mention
		switch {
		case strings.HasPrefix(target, "~/") || path.IsAbs(target):
			r.warn("%s imports %s, outside the repository: personal, and not available in experiments.", from, target)
			continue
		}
		resolved := path.Clean(path.Join(path.Dir(from), target))
		if strings.HasPrefix(resolved, "../") || resolved == ".." {
			r.warn("%s imports %s, outside the repository: not available in experiments.", from, target)
			continue
		}
		if slices.Contains(chain, resolved) {
			r.warn("%s imports %s, which imports it back (an import cycle): loaded once.", from, resolved)
			continue
		}
		if r.seen[resolved] {
			continue // the same file imported from two places loads once
		}
		if !source.Has(r.src, resolved) {
			if pathLike {
				r.warn("%s imports %s, which does not exist.", from, resolved)
			}
			continue
		}
		if depth > MaxImportDepth {
			r.warn("%s imports %s beyond Claude Code's %d-hop limit: it is not loaded.", from, resolved, MaxImportDepth)
			continue
		}
		imported, ok := r.read(resolved)
		if !ok {
			continue // read warned
		}
		startupBytes := 0
		if startup {
			startupBytes = len(imported)
		}
		r.add(resolved, KindImport, imported, startupBytes, from)
		r.imports(append(slices.Clone(chain), resolved), imported, startup)
	}
}

// importTargets lists @ tokens outside fenced code blocks and inline code spans; the caller decides which are files.
func importTargets(text string) []string {
	var targets []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, match := range importPattern.FindAllStringSubmatch(stripCodeSpans(line), -1) {
			if target := strings.TrimRight(match[1], ".,;:)]}!?\"'"); target != "" {
				targets = append(targets, target)
			}
		}
	}
	return targets
}

func stripCodeSpans(line string) string {
	var out strings.Builder
	inSpan := false
	for _, r := range line {
		if r == '`' {
			inSpan = !inSpan
			out.WriteRune(' ')
			continue
		}
		if !inSpan {
			out.WriteRune(r)
		} else {
			out.WriteRune(' ')
		}
	}
	return out.String()
}

// IsDocument reports whether p is prose for readers: a Markdown, reStructuredText or AsciiDoc file outside test-data
// folders. Snapshots may change documents and instruction files, never code, configuration or test inputs, which would
// change what a task builds and tests. Plain .txt is excluded: requirements.txt and CMakeLists.txt are build inputs.
func IsDocument(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".mdx", ".markdown", ".rst", ".adoc":
	default:
		return false
	}
	for _, dir := range strings.Split(path.Dir(p), "/") {
		switch strings.ToLower(dir) {
		case "test", "tests", "testdata", "fixtures", "__fixtures__", "__snapshots__", "golden", "node_modules", "vendor":
			return false
		}
	}
	return true
}

// linkPattern finds Markdown link targets: [text](target) or [text](target "title").
var linkPattern = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// linked lists repository files that context files link to but that are not context themselves.
func (r *resolver) linked() []string {
	found := map[string]bool{}
	for _, e := range r.entries {
		if e.Kind == KindHarness || !strings.HasSuffix(e.Path, ".md") {
			continue
		}
		data, err := r.src.ReadFile(e.Path)
		if err != nil {
			continue
		}
		inFence := false
		for _, line := range strings.Split(string(data), "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, match := range linkPattern.FindAllStringSubmatch(line, -1) {
				target, _, _ := strings.Cut(match[1], "#")
				target, _, _ = strings.Cut(target, "?")
				if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || path.IsAbs(target) {
					continue
				}
				if resolved := path.Clean(path.Join(path.Dir(e.Path), target)); IsDocument(resolved) && source.Has(r.src, resolved) && !r.seen[resolved] {
					found[resolved] = true
				}
			}
		}
	}
	linked := make([]string, 0, len(found))
	for p := range found {
		linked = append(linked, p)
	}
	sort.Strings(linked)
	return linked
}

// frontmatter returns the YAML frontmatter lines, if the file starts with "---".
func frontmatter(data []byte) []string {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return nil
	}
	return strings.Split(text[4:4+end], "\n")
}

func frontmatterField(data []byte, key string) string {
	for _, line := range frontmatter(data) {
		if value, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func frontmatterHasKey(data []byte, key string) bool {
	for _, line := range frontmatter(data) {
		if strings.HasPrefix(line, key+":") {
			return true
		}
	}
	return false
}

// descriptionBytes approximates what loads at start for skills, subagents and commands: name plus description.
func descriptionBytes(data []byte) int {
	name, description := frontmatterField(data, "name"), frontmatterField(data, "description")
	if description == "" { // commands without frontmatter use their first line
		description, _, _ = strings.Cut(strings.TrimSpace(string(data)), "\n")
	}
	return len(name) + len(description)
}

// SkillNames lists the names the context's skills go by: each skill's folder name and its frontmatter name.
func SkillNames(c Context, src source.Source) []string {
	var names []string
	for _, e := range c.Entries {
		if e.Kind != KindSkill {
			continue
		}
		names = append(names, path.Base(path.Dir(e.Path)))
		if data, err := src.ReadFile(e.Path); err == nil {
			if name := frontmatterField(data, "name"); name != "" {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}

// CommandNames lists the names the context's commands go by: a command's file name without .md, and for nested ones
// also "folder:name".
func CommandNames(c Context) []string {
	var names []string
	for _, e := range c.Entries {
		if e.Kind != KindCommand {
			continue
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(e.Path, ".claude/commands/"), ".md")
		names = append(names, path.Base(rel))
		if strings.Contains(rel, "/") {
			names = append(names, strings.ReplaceAll(rel, "/", ":"))
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}
