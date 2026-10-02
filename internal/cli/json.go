package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/home"
)

// The --json contract (docs/guide.md, "Scripting and automation"): one JSON document on stdout, a top-level "schema"
// version and "command", snake_case fields, errors as {"error": {"message", "code"}} with the exit code, no styling.

// JSONSchema is the version of the --json documents. It rises only for a change that breaks readers (a removed or
// renamed field, a changed meaning); added fields do not raise it.
const JSONSchema = 1

// header starts every document; documents embed it, so its fields come first and flat.
type header struct {
	Schema  int    `json:"schema"`
	Command string `json:"command"`
}

func hdr(command string) header { return header{Schema: JSONSchema, Command: command} }

// hdr is the header of the running command's documents, for handlers shared by several commands (task add and import).
func (env Env) hdr() header {
	if env.json == nil {
		return hdr("")
	}
	return hdr(env.json.command)
}

// jsonState is shared by the copies of one command's Env: where the document goes and whether one was written.
type jsonState struct {
	out     io.Writer
	command string // "task list": what the handler's documents are headed with when it does not know its own name
	written bool
	// err is the last error reported through fail: the message of an error document, so an earlier warning on stderr
	// cannot take its place.
	err string
	// roots are the repository's folders the command learned of (openProject, init, lint): free text names them <repo>.
	roots []string
}

// noteRoot records the repository folder so that redact can hide it, wherever it is.
func (env Env) noteRoot(root string) {
	if env.json != nil && root != "" {
		env.json.roots = append(env.json.roots, root)
	}
}

// errorDoc is a failed command's document.
type errorDoc struct {
	header
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// jsonCommands lists the subcommands (or, with "", the whole command) that take --json in part 1.
var jsonCommands = map[string][]string{
	"init":    {""},
	"start":   {""},
	"context": {"show", "snapshot", "list", "diff", "lint"},
	"task":    {"list", "show", "mine", "validate", "import", "add", "edit", "rm"},
	"run":     {"once", "show", "list"},
}

// splitJSONFlag removes the bare --json flag (before any "--") from args and reports whether the command takes it.
// A command that does not take --json is left as it is, so its flag parser answers as before; so is a help request.
func splitJSONFlag(command string, args []string) (rest []string, want bool) {
	subs, ok := jsonCommands[command]
	if !ok {
		return args, false
	}
	if subs[0] != "" && (len(args) == 0 || !slices.Contains(subs, args[0])) {
		return args, false
	}
	end := slices.Index(args, "--")
	if end < 0 {
		end = len(args)
	}
	found, help := false, false
	for _, a := range args[:end] {
		switch a {
		case "--json", "-json":
			found = true
		case "-h", "--help", "-help": // a bare "help" is a subcommand, which never reaches here (or a name, which is not help)
			help = true
		}
	}
	if !found {
		return args, false
	}
	for i := 0; i < end; i++ {
		if args[i] != "--json" && args[i] != "-json" {
			rest = append(rest, args[i])
		}
	}
	rest = append(rest, args[end:]...)
	return rest, !help
}

// runJSON runs a command in JSON mode: human output is discarded, the handler emits its document, and a failure that
// did not emit one becomes an error document built from what the handler wrote to stderr.
func runJSON(ctx context.Context, env Env, command string, args []string) int {
	state := &jsonState{out: env.Stdout, command: commandName(command, args)}
	realStderr := env.Stderr
	var stderr bytes.Buffer
	env.JSON, env.Plain, env.Terminal, env.StdinTerminal = true, true, false, false
	env.json, env.Stdout, env.Stderr = state, io.Discard, &stderr
	code := dispatch(ctx, env, command, args)
	name := state.command
	switch {
	case !state.written && code != ExitOK:
		env.emitError(name, code, errorMessage(env, stderr.String(), code))
	case !state.written:
		env.emitTo(hdr(name))
	case stderr.Len() > 0:
		_, _ = io.Copy(realStderr, &stderr) // warnings of a command that did produce its document
	}
	return code
}

// commandName is "task list" for a command that has subcommands, else the command.
func commandName(command string, args []string) string {
	if subs := jsonCommands[command]; subs[0] != "" && len(args) > 0 {
		return command + " " + args[0]
	}
	return command
}

// errorMessage is a failed command's message: the error fail recorded, else the last "agentium ..." line of stderr (a
// usage error's own sentence; the usage text and earlier warnings are not part of it). Paths are redacted.
func errorMessage(env Env, stderr string, code int) string {
	text := ""
	if env.json != nil {
		text = env.json.err
	}
	if text == "" {
		stderr, _, _ = strings.Cut(strings.TrimSpace(stderr), "\n\nUsage:")
		lines := strings.Split(strings.TrimSpace(stderr), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.HasPrefix(lines[i], "agentium") {
				text = lines[i]
				break
			}
		}
		if text == "" && !strings.HasPrefix(strings.TrimSpace(stderr), "Usage:") && len(lines) > 0 {
			text = lines[len(lines)-1]
		}
		if text == "" {
			text = "invalid arguments: run the command with -h for its usage"
		}
	}
	text = env.redact(strings.TrimPrefix(strings.TrimSpace(text), "agentium: "))
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", "; ")), " ")
	if text == "" {
		text = fmt.Sprintf("the command failed (exit %d)", code)
	}
	return text
}

// redact hides, in free text (messages, logs, warnings, notes), the data folder as <data>, the repository as <repo> and
// the home folder as ~. It matches whole path names only: after a start, quote, space, = ( [ { or a backtick (or file://), and before
// anything that does not go on a name (letters, digits, _ -, and a . with a name character after it), so /root does not touch
// root.md or /Users/alice, and ends cleanly before a backtick, a bracket or a sentence's full stop. Content that is the
// user's own (diffs, patches, logs, instructions, file names) is never passed through it. Documents hold no paths
// elsewhere; this is the backstop for text that quotes one.
func (env Env) redact(text string) string {
	type spelling struct{ path, repl string }
	var all []spelling
	add := func(path, repl string) {
		if path = strings.TrimRight(path, "/"); len(path) < 2 {
			return
		}
		all = append(all, spelling{path, repl})
		if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
			all = append(all, spelling{real, repl})
		}
	}
	if layout, err := home.Resolve(env.Getenv); err == nil {
		add(layout.Root, "<data>")
	}
	add(env.Dir, "<repo>")
	if env.json != nil {
		for _, root := range env.json.roots {
			add(root, "<repo>")
		}
	}
	add(env.Getenv("HOME"), "~")
	slices.SortStableFunc(all, func(a, b spelling) int { return len(b.path) - len(a.path) }) // nested folders first
	for _, sp := range all {
		text = replacePath(text, sp.path, sp.repl)
	}
	return text
}

// redactAll is redact for each text of a list.
func (env Env) redactAll(texts []string) []string {
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = env.redact(t)
	}
	return out
}

// continuesName reports whether the byte at i goes on a path name: a letter, digit, "_" or "-", or a "." that a name
// character follows (so "/root." ends a sentence, and "/root.md" is another name).
func continuesName(text string, i int) bool {
	isName := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
	}
	switch {
	case i >= len(text):
		return false
	case text[i] == '.':
		return i+1 < len(text) && isName(text[i+1])
	}
	return isName(text[i])
}

// replacePath replaces path in text where it is a whole path name (see redact).
func replacePath(text, path, repl string) string {
	var out strings.Builder
	for {
		i := strings.Index(text, path)
		if i < 0 {
			break
		}
		end := i + len(path)
		startOK := i == 0 || strings.IndexByte(" \t\r\n\"'=(`[{", text[i-1]) >= 0 || strings.HasSuffix(text[:i], "file://")
		endOK := !continuesName(text, end)
		if startOK && endOK {
			out.WriteString(text[:i] + repl)
		} else {
			out.WriteString(text[:end])
		}
		text = text[end:]
	}
	out.WriteString(text)
	return out.String()
}

func (env Env) emitError(command string, code int, message string) {
	env.emitTo(errorDoc{header: hdr(command), Error: errorBody{Message: message, Code: code}})
}

// emit writes the command's document (indented, one trailing newline) and returns ExitOK; a document that cannot be
// encoded is a runtime failure reported to stderr, which runJSON turns into an error document.
func (env Env) emit(doc any) int {
	if err := env.emitTo(doc); err != nil {
		return fail(env, fmt.Errorf("encode the JSON document: %w", err))
	}
	return ExitOK
}

// emitCode is emit for a command whose result also sets the exit code.
func (env Env) emitCode(doc any, code int) int {
	if env.emit(doc) != ExitOK {
		return ExitError
	}
	return code
}

func (env Env) emitTo(doc any) error {
	if env.json == nil {
		return fmt.Errorf("no JSON output")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	env.json.written = true
	_, err := env.json.out.Write(buf.Bytes())
	return err
}

// list returns s, or an empty non-nil list: JSON lists are never null.
func list[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
