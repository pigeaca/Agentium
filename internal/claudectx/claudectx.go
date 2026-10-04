// Package claudectx works out which project files Claude Code loads, as experiments run it: from the repository root
// (or a monorepo module's folder, ResolveIn), with project settings only (so CLAUDE.local.md, user files and
// settings.local.json never load).
//
// Rules, from Claude Code's memory, skills and settings docs (checked 2026-09-28, CLI 2.1.281):
//   - CLAUDE.md and .claude/CLAUDE.md at the root load at session start. If neither exists, AGENTS.md loads instead.
//   - @path imports in those files load too, recursively up to five hops, but not inside code spans or blocks. Rules and
//     nested CLAUDE.md files are memory files as well: their imports load when they do.
//   - .claude/rules/**/*.md load at start, unless their frontmatter scopes them with paths:, then on demand.
//   - Skill, subagent and command descriptions load at start; their bodies load on demand.
//   - CLAUDE.md files in subdirectories load on demand, when files there are read.
//   - .claude/settings.json, .claude/hooks and .mcp.json change what runs, not what the model reads.
//
// A session started in a subfolder (a monorepo module's, ResolveIn) loads the CLAUDE.md files of every folder from the
// root down to it (the memory docs: Claude Code reads them recursively up from where it starts). Agentium applies each
// rule above to each of those folders alike: its instruction file (CLAUDE.md or .claude/CLAUDE.md, else AGENTS.md) and
// imports, its .claude/rules, its skill, subagent and command descriptions, and its harness files. That the .claude
// folders above the starting folder load too is Agentium's reading of the docs, not yet checked in a real session (a
// run's own record of the skills and commands Claude Code reports at start shows any difference).
package claudectx

import (
	"bytes"
	"cmp"
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
	// Broken are the Warnings that name an @import of a file that does not exist (also in Warnings; lint reports them
	// as problems of their own).
	Broken []string `json:"broken_imports,omitempty"`
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

// Resolve works out the context of src for a session started at the repository's root.
func Resolve(src source.Source) (Context, error) {
	return ResolveIn(src, "")
}

// ResolveIn works out the context of src for a session started in module, a folder of the repository (slash-separated,
// relative to its root, as store.Task.Module holds it; "" is the root, and then it is Resolve): the instruction files,
// rules, descriptions and harness files of every folder from the root down to the module load as the root's do (see
// the package comment). Paths stay relative to the repository's root.
func ResolveIn(src source.Source, module string) (Context, error) {
	r := &resolver{src: src, seen: map[string]bool{}, folders: Folders(module), module: module}
	if err := r.resolve(); err != nil {
		return Context{}, err
	}
	sort.SliceStable(r.entries, func(i, j int) bool { return kindOrder(r.entries[i].Kind) < kindOrder(r.entries[j].Kind) })
	return Context{Entries: r.entries, Warnings: r.warnings, Broken: r.broken, Linked: r.linked()}, nil
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
	src source.Source
	// folders are where context loads from at start, the root first ("" alone at the root: Folders), and module the
	// folder the session starts in.
	folders   []string
	module    string
	seen      map[string]bool
	entries   []Entry
	warnings  []string
	broken    []string
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

// Folders lists the folders a session started in module loads its context from, the root first: "" and each folder on
// the way down to module ("svc/billing": "", "svc", "svc/billing"). At the root it is "" alone.
func Folders(module string) []string {
	folders := []string{""}
	if module == "" {
		return folders
	}
	parts := strings.Split(module, "/")
	for i := range parts {
		folders = append(folders, strings.Join(parts[:i+1], "/"))
	}
	return folders
}

// prefix is the path prefix of folder's files: "" for the root, else "folder/".
func prefix(folder string) string {
	if folder == "" {
		return ""
	}
	return folder + "/"
}

func (r *resolver) resolve() error {
	// Each folder's instruction files: CLAUDE.md and .claude/CLAUDE.md, else AGENTS.md.
	roots := make([][]string, len(r.folders))
	agentsLoaded := make([]bool, len(r.folders))
	found := false
	for i, folder := range r.folders {
		p := prefix(folder)
		for _, name := range []string{"CLAUDE.md", ".claude/CLAUDE.md"} {
			if source.Has(r.src, p+name) {
				roots[i] = append(roots[i], p+name)
			}
		}
		agentsLoaded[i] = len(roots[i]) == 0 && source.Has(r.src, p+"AGENTS.md")
		if agentsLoaded[i] {
			roots[i] = []string{p + "AGENTS.md"}
		}
		found = found || len(roots[i]) > 0
	}
	switch {
	case !found && r.module == "":
		r.warn("No CLAUDE.md, .claude/CLAUDE.md or AGENTS.md at the repository root: Claude Code loads no project instructions.")
	case !found:
		r.warn("No CLAUDE.md, .claude/CLAUDE.md or AGENTS.md at the repository root or in the folders down to the module %s: Claude Code loads no project instructions.", r.module)
	}
	for i, folder := range r.folders {
		p := prefix(folder)
		agentsViaLink := false
		for _, name := range roots[i] {
			if data, ok := r.read(name); ok {
				r.add(name, KindInstructions, data, len(data), "")
				r.imports([]string{name}, data, true)
				// Source follows symbolic links, so a CLAUDE.md that links to AGENTS.md reads as AGENTS.md's own bytes:
				// Claude Code loads that content through the link, though only CLAUDE.md is recorded.
				if name != p+"AGENTS.md" && source.Has(r.src, p+"AGENTS.md") {
					if agents, ok := r.read(p + "AGENTS.md"); ok && bytes.Equal(agents, data) {
						agentsViaLink = true
					}
				}
			}
		}
		if source.Has(r.src, p+"AGENTS.md") && !agentsLoaded[i] && !agentsViaLink && !r.seen[p+"AGENTS.md"] {
			r.warn("%sAGENTS.md is not loaded by Claude Code: a CLAUDE.md exists and does not import it (@AGENTS.md).", p)
		}
		if source.Has(r.src, p+"CLAUDE.local.md") {
			r.warn("%sCLAUDE.local.md is personal: experiments run with project settings only and never load it.", p)
		}
		if source.Has(r.src, p+".claude/settings.local.json") {
			r.warn("%s.claude/settings.local.json is personal: experiments never load it.", p)
		}
		for _, dir := range []string{".claude/rules", ".claude/skills", ".claude/agents", ".claude/commands"} {
			if source.Has(r.src, p+dir) { // git stores a symbolic link to a directory as one file
				r.warn("%s%s is a symbolic link to a directory: Agentium does not follow it yet, so its files are not in this context.", p, dir)
			}
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
	for _, folder := range r.folders {
		if rel, ok := strings.CutPrefix(p, prefix(folder)); ok && r.classifyIn(p, rel) {
			return
		}
	}
	// An instruction file in any other folder (below the starting folder, or beside it) loads on demand. One in a folder
	// that loads at start but is not loaded there (an AGENTS.md beside a CLAUDE.md) does not load on demand either.
	base := path.Base(p)
	if (base == "CLAUDE.md" || base == "AGENTS.md") && strings.Contains(p, "/") && !strings.HasPrefix(p, ".claude/") &&
		(len(r.folders) == 1 || !slices.Contains(r.folders[1:], path.Dir(p))) {
		if data, ok := r.read(p); ok {
			r.add(p, KindNested, data, 0, "")
			r.importers = append(r.importers, importer{p, data, false})
		}
	}
}

// classifyIn places p, a file of a folder that loads at start, by rel, its path in that folder: the folder's rules,
// skills, subagents, commands and harness files. It reports whether p is one.
func (r *resolver) classifyIn(p, rel string) bool {
	switch {
	case strings.HasPrefix(rel, ".claude/rules/") && strings.HasSuffix(rel, ".md"):
		data, ok := r.read(p)
		if !ok {
			return true
		}
		startup := frontmatterField(data, "paths") == "" && !frontmatterHasKey(data, "paths")
		if startup {
			r.add(p, KindRule, data, len(data), "")
		} else {
			r.add(p, KindScopedRule, data, 0, "")
		}
		r.importers = append(r.importers, importer{p, data, startup})
	case strings.HasPrefix(rel, ".claude/skills/"):
		data, ok := r.read(p)
		if !ok {
			return true
		}
		if path.Base(rel) == "SKILL.md" && strings.Count(rel, "/") == 3 {
			r.add(p, KindSkill, data, descriptionBytes(data), "")
		} else {
			r.add(p, KindSkillFile, data, 0, "")
		}
	case strings.HasPrefix(rel, ".claude/agents/") && strings.HasSuffix(rel, ".md"):
		if data, ok := r.read(p); ok {
			r.add(p, KindSubagent, data, descriptionBytes(data), "")
		}
	case strings.HasPrefix(rel, ".claude/commands/") && strings.HasSuffix(rel, ".md"):
		if data, ok := r.read(p); ok {
			r.add(p, KindCommand, data, descriptionBytes(data), "")
		}
	case IsHarness(rel):
		if data, ok := r.read(p); ok {
			r.add(p, KindHarness, data, 0, "")
		}
	default:
		return false
	}
	return true
}

// IsHarness reports whether Claude Code reads the file at p, a repository path, to decide what runs rather than as
// text for the model: project settings (hooks, env, permissions, helpers), hook scripts and MCP servers. Resolve
// classifies with it, and the pull-request screen refuses changes to these files. The paths are lower case and exact;
// a caller that must also catch variants a case-insensitive file system would load folds the path first.
func IsHarness(p string) bool {
	return p == ".claude/settings.json" || p == ".mcp.json" || strings.HasPrefix(p, ".claude/hooks/")
}

// harnessFields are frontmatter fields of skills, subagents and commands that change what runs rather than what the
// model reads (Claude Code's skills and subagents docs, CLI 2.1): hooks run commands outside the Bash sandbox,
// mcpServers start servers, allowed-tools and permissionMode grant tools without asking, and memory gives a subagent
// a memory folder that outlives the session (from the docs as recalled, unverified: a field Claude Code lacks only
// costs a refusal). They are compared normalized: lower case, without "-", "_" or spaces.
var harnessFields = []string{"hooks", "mcpservers", "allowedtools", "permissionmode", "memory"}

// HarnessFields is what a skill's, subagent's or command's frontmatter says about what runs.
type HarnessFields struct {
	Fields []string // the harness fields declared, at any nesting level, normalized (see harnessFields)
	// Doubt says why the frontmatter cannot be read safely (YAML syntax this reader does not parse at a key, a
	// delimiter some parser might read differently, no closing line); a doubt counts as harness.
	Doubt string
	// Text is the frontmatter, which is what names the commands its hooks run (the whole rest of the file when in
	// doubt); the body is prose for the model.
	Text string
}

// Harness reports whether the frontmatter changes what runs, or may.
func (h HarnessFields) Harness() bool { return len(h.Fields) > 0 || h.Doubt != "" }

// HarnessFrontmatter reads the frontmatter of a skill, subagent or command for harness fields. It fails closed: only
// plain and simply quoted keys in block style are read, and anything else at a key position (flow mappings or
// sequences, explicit keys, tags, anchors, aliases, merge keys, directives, escapes in quoted keys) is a doubt. So is
// a delimiter line a different parser might honor: a "---" that is not alone on its line, a "..." end marker, a
// frontmatter that is not closed, or one after a byte-order mark or blank lines. The body is never read.
func HarnessFrontmatter(data []byte) HarnessFields {
	lines := strings.Split(strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n"), "\n")
	if strings.TrimRight(lines[0], " \t\r") != "---" {
		for _, line := range lines {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				if strings.HasPrefix(trimmed, "---") {
					return HarnessFields{Doubt: "a --- line that might open frontmatter", Text: strings.Join(lines, "\n")}
				}
				break
			}
		}
		return HarnessFields{}
	}
	var h HarnessFields
	keyIndent, contentIndent := -1, -1 // inside a block scalar: its key's column, and its content's once known
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t\r")
		switch {
		case line == "---":
			h.Text = strings.Join(lines[1:i], "\n")
			return h
		case strings.HasPrefix(line, "---") || line == "...":
			h.Doubt = fmt.Sprintf("the line %q might end the frontmatter", line)
			h.Text = strings.Join(lines[1:], "\n")
			return h
		case strings.ContainsAny(lines[i], yamlBreaks) && h.Doubt == "":
			// YAML also breaks lines at a lone CR, NEL, LS and PS, so one line here may hold several keys there.
			h.Doubt = fmt.Sprintf("a line break other than LF or CRLF in the frontmatter line %q", line)
		}
		if keyIndent >= 0 && strings.TrimSpace(line) != "" {
			spaces := len(line) - len(strings.TrimLeft(line, " "))
			switch {
			case spaces <= keyIndent && line[spaces] == '\t':
				h.Doubt = cmp.Or(h.Doubt, fmt.Sprintf("a tab where a block scalar may end, in the frontmatter line %q", line))
			case spaces > keyIndent && contentIndent < 0:
				contentIndent = spaces
			case spaces > keyIndent && spaces < contentIndent:
				h.Doubt = cmp.Or(h.Doubt, fmt.Sprintf("a dedent inside a block scalar, in the frontmatter line %q", line))
			}
			if spaces > keyIndent {
				continue // block scalar text, however it starts (Markdown lists, links, emphasis)
			}
			keyIndent, contentIndent = -1, -1
		}
		if keyIndent < 0 {
			keyIndent = h.scan(line)
		}
	}
	h.Doubt = "the frontmatter has no closing --- line"
	h.Text = strings.Join(lines[1:], "\n")
	return h
}

// yamlBreaks are the characters YAML 1.2 reads as line breaks, besides LF (and the CR of CRLF, already joined).
const yamlBreaks = "\r\u0085\u2028\u2029"

// blockScalar matches a value that starts a block scalar: | or >, with an optional chomping indicator and an optional
// comment. An explicit indentation indicator (a digit) is left out on purpose: it can claim more indentation than the
// text has, so its lines are scanned as keys instead (fail closed).
var blockScalar = regexp.MustCompile(`^[|>][-+]?(?:[ \t]+#.*)?$`)

// scan reads one frontmatter line for a key. When the key's value starts a block scalar, it returns the key's column,
// so the caller reads the more indented lines that follow as text; otherwise it returns -1.
func (h *HarnessFields) scan(line string) int {
	rest := strings.TrimLeft(line, " \t")
	for rest == "-" || strings.HasPrefix(rest, "- ") || strings.HasPrefix(rest, "-\t") { // sequence items
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	if rest == "" || rest[0] == '#' {
		return -1
	}
	doubt := func(why string) {
		if h.Doubt == "" {
			h.Doubt = fmt.Sprintf("%s in the frontmatter line %q", why, line)
		}
	}
	if strings.ContainsRune("{[?!&*%@`|><", rune(rest[0])) {
		doubt("YAML syntax this reader does not parse at a key")
		return -1
	}
	key, value := "", ""
	if quote := rest[0]; quote == '"' || quote == '\'' {
		end := strings.IndexByte(rest[1:], quote)
		if end < 0 {
			doubt("a quoted key or value that spans lines")
			return -1
		}
		inner, after := rest[1:1+end], strings.TrimLeft(rest[2+end:], " \t")
		if strings.Contains(inner, `\`) || (quote == '\'' && strings.HasPrefix(after, "'")) {
			doubt("an escape in a quoted key")
			return -1
		}
		if !strings.HasPrefix(after, ":") {
			return -1 // a quoted value continuing from an earlier line
		}
		key, value = inner, after[1:]
	} else if before, after, ok := strings.Cut(rest, ":"); ok {
		key, value = before, after
	} else {
		return -1 // a value continuing from an earlier line
	}
	key = strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "", "\t", "").Replace(key))
	if slices.Contains(harnessFields, key) && !slices.Contains(h.Fields, key) {
		h.Fields = append(h.Fields, key)
	}
	if blockScalar.MatchString(strings.TrimSpace(value)) {
		return len(line) - len(rest)
	}
	return -1
}

// LoadsByPresence reports whether a file at p is context just by existing (instruction files, .claude, .mcp.json), as
// opposed to a file that loads only when something imports it.
func LoadsByPresence(p string) bool {
	base := path.Base(p)
	return p == ".mcp.json" || strings.HasPrefix(p, ".claude/") || base == "CLAUDE.md" || base == "AGENTS.md"
}

// LoadsByPresenceIn is LoadsByPresence for a session started in module (ResolveIn): the .claude folder and .mcp.json
// of every folder down to the module load by being present too. At the root it is LoadsByPresence.
func LoadsByPresenceIn(p, module string) bool {
	if LoadsByPresence(p) {
		return true
	}
	for _, folder := range Folders(module)[1:] {
		if rel, ok := strings.CutPrefix(p, folder+"/"); ok && (rel == ".mcp.json" || strings.HasPrefix(rel, ".claude/")) {
			return true
		}
	}
	return false
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
				r.broken = append(r.broken, r.warnings[len(r.warnings)-1])
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
	return IsDocumentExt(p) && !InDataFolder(p)
}

// IsDocumentExt is IsDocument by file extension only, wherever the file is.
func IsDocumentExt(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".mdx", ".markdown", ".rst", ".adoc":
		return true
	}
	return false
}

// InDataFolder reports whether p is under a folder of tests, fixtures, vendored or installed code.
func InDataFolder(p string) bool {
	for _, dir := range strings.Split(path.Dir(p), "/") {
		switch strings.ToLower(dir) {
		case "test", "tests", "testdata", "fixtures", "__fixtures__", "__snapshots__", "golden", "node_modules", "vendor":
			return true
		}
	}
	return false
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

// frontmatter returns the YAML frontmatter lines: the file opens with a "---" line (trailing spaces allowed), and the
// frontmatter ends at the first later line that is exactly "---" once trailing spaces are trimmed.
func frontmatter(data []byte) []string {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if strings.TrimRight(lines[0], " \t\r") != "---" {
		return nil
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r") == "---" {
			return lines[1:i]
		}
	}
	return nil
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

// commandPath is a command file's path inside its .claude/commands folder, whichever folder that is in.
func commandPath(p string) string {
	if rest, ok := strings.CutPrefix(p, ".claude/commands/"); ok {
		return rest
	}
	if _, rest, ok := strings.Cut(p, "/.claude/commands/"); ok {
		return rest
	}
	return p
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
		rel := strings.TrimSuffix(commandPath(e.Path), ".md")
		names = append(names, path.Base(rel))
		if strings.Contains(rel, "/") {
			names = append(names, strings.ReplaceAll(rel, "/", ":"))
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}
