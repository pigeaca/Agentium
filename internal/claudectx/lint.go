package claudectx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// MaxStartupBytes is Agentium's cap on what loads at session start (32 KiB, about 8,000 tokens): a context past it
// costs every request and is the likeliest thing to trim. It is Agentium's rule of thumb, not a Claude Code limit.
const MaxStartupBytes = 32 * 1024

// Lint is the free check of a context: no agent runs, no network.
type Lint struct {
	StartupBytes int
	// Problems are broken @imports and a context over MaxStartupBytes.
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
	if l.StartupBytes > MaxStartupBytes {
		l.Problems = append(l.Problems, fmt.Sprintf("the context loaded at session start is %.1f KiB, over the %d KiB cap.",
			float64(l.StartupBytes)/1024, MaxStartupBytes/1024))
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

// maxHookPayload bounds stdin: a Write payload carries the whole file.
const maxHookPayload = 16 << 20

// ParseHookPayload reads one payload from r. ok is false for a tool that does not edit a file (the hook then prints
// nothing); err is for input that is not a payload.
func ParseHookPayload(data []byte) (p HookPayload, ok bool, err error) {
	if len(data) > maxHookPayload {
		return p, false, fmt.Errorf("payload over %d MiB", maxHookPayload>>20)
	}
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

// HookSettings is the settings snippet for the user's own ~/.claude/settings.json: a PostToolUse hook on the edit
// tools. Agentium never writes it.
func HookSettings() ([]byte, error) {
	type command struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Matcher string    `json:"matcher"`
		Hooks   []command `json:"hooks"`
	}
	var snippet struct {
		Hooks struct {
			PostToolUse []matcher `json:"PostToolUse"`
		} `json:"hooks"`
	}
	snippet.Hooks.PostToolUse = []matcher{{Matcher: "Edit|Write|MultiEdit", Hooks: []command{{Type: "command", Command: "agentium context lint --hook"}}}}
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
