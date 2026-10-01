package claudectx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// CodexDocMaxBytes is Codex's project_doc_max_bytes default: it reads only the first 32 KiB of AGENTS.md files and
// does not follow @imports (see the feasibility study, section 4).
const CodexDocMaxBytes = 32 * 1024

// Lint is the free check of a context: no agent runs, no network.
type Lint struct {
	StartupBytes int // what Claude Code loads at session start: information for the size change, never a problem
	// Problems are broken @imports and AGENTS.md files over CodexDocMaxBytes.
	Problems []string
	// Warnings are the rest of what `context show` warns about.
	Warnings []string
	// Files is every file of the context, to tell whether an edited file belongs to it.
	Files []string
}

// LintContext checks the context Claude Code would load from src.
func LintContext(src source.Source) (Lint, error) {
	resolved, err := Resolve(src)
	if err != nil {
		return Lint{}, err
	}
	l := Lint{StartupBytes: resolved.StartupBytes(), Problems: slices.Clone(resolved.Broken)}
	// Codex reads every AGENTS.md from the root down, loaded by Claude Code or not, so look at the files, not the entries.
	for _, p := range src.Paths() {
		if path.Base(p) != "AGENTS.md" {
			continue
		}
		if data, err := src.ReadFile(p); err == nil && len(data) > CodexDocMaxBytes {
			l.Problems = append(l.Problems, fmt.Sprintf("%s is %.1f KiB: Codex reads only its first %d KiB (project_doc_max_bytes).",
				p, float64(len(data))/1024, CodexDocMaxBytes/1024))
		}
	}
	for _, w := range resolved.Warnings {
		if !slices.Contains(resolved.Broken, w) {
			l.Warnings = append(l.Warnings, w)
		}
	}
	for _, e := range resolved.Entries {
		if e.Kind != KindHarness { // settings and hooks change what runs, not what the model reads
			l.Files = append(l.Files, e.Path)
		}
	}
	return l, nil
}

// HookPayload is the part of a Claude Code PostToolUse payload that lint reads.
type HookPayload struct {
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

// MaxHookPayload bounds a payload the hook reads: a Write payload carries the whole file.
const MaxHookPayload = 16 << 20

// ParseHookPayload decodes one payload (the JSON Claude Code writes to a hook's stdin). relevant is false for a tool
// that does not edit a file or a payload without a file (the hook then prints nothing); err is for input that is not
// a payload.
func ParseHookPayload(data []byte) (p HookPayload, relevant bool, err error) {
	if err := json.Unmarshal(data, &p); err != nil {
		return p, false, fmt.Errorf("not a JSON hook payload: %w", err)
	}
	switch p.ToolName {
	case "", "Edit", "Write", "MultiEdit":
	default:
		return p, false, nil
	}
	return p, p.ToolInput.FilePath != "", nil
}

// Contains reports whether the repository-relative file (slash-separated) is one of the lint's context files.
func (l Lint) Contains(rel string) bool {
	return slices.Contains(l.Files, path.Clean(filepath.ToSlash(rel)))
}

// Reaches is Contains, widened to the files an edit can change the context through: files that load by presence (an
// AGENTS.md Claude Code does not load still gets a lint warning), and the target of a context file that is a symbolic
// link (CLAUDE.md -> AGENTS.md). root is the working tree.
func (l Lint) Reaches(root, rel string) bool {
	rel = path.Clean(filepath.ToSlash(rel))
	if l.Contains(rel) || LoadsByPresence(rel) {
		return true
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	for _, f := range l.Files {
		full := filepath.Join(root, filepath.FromSlash(f))
		if info, err := os.Lstat(full); err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if target, err := filepath.EvalSymlinks(full); err == nil {
			if r, err := filepath.Rel(realRoot, target); err == nil && filepath.ToSlash(r) == rel {
				return true
			}
		}
	}
	return false
}

// HookSettings is the object to merge into the user's own ~/.claude/settings.json: a PostToolUse hook on the edit
// tools that runs command. Agentium never writes it. The timeout (seconds) keeps a stuck check from holding up edits.
func HookSettings(command string) ([]byte, error) {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type matcher struct {
		Matcher string `json:"matcher"`
		Hooks   []hook `json:"hooks"`
	}
	var snippet struct {
		Hooks struct {
			PostToolUse []matcher `json:"PostToolUse"`
		} `json:"hooks"`
	}
	snippet.Hooks.PostToolUse = []matcher{{Matcher: "Edit|Write|MultiEdit", Hooks: []hook{{Type: "command", Command: command, Timeout: 5}}}}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snippet); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// HookOutput is the JSON a PostToolUse hook prints so Claude Code shows text to the user: systemMessage is a universal
// hook output field, "warning message shown to the user" (Claude Code hooks reference, JSON output).
func HookOutput(text string) string {
	out, _ := json.Marshal(map[string]string{"systemMessage": strings.TrimRight(text, "\n")})
	return string(out)
}
