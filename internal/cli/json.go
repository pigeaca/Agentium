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
		case "-h", "--help", "-help", "help":
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

// errorMessage turns what a failed command wrote to stderr into one message: the first paragraph (a usage text
// follows it), the data and home folders shown as <data> and ~.
func errorMessage(env Env, stderr string, code int) string {
	text := strings.TrimSpace(stderr)
	if strings.HasPrefix(text, "Usage:") {
		text = "invalid arguments: run the command with -h for its usage"
	}
	text, _, _ = strings.Cut(text, "\n\n")
	text = env.redact(strings.TrimPrefix(text, "agentium: "))
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", "; ")), " ")
	if text == "" {
		text = fmt.Sprintf("the command failed (exit %d)", code)
	}
	return text
}

// redact is the backstop of the documents' own care not to hold paths: the data folder, the working folder and the home
// folder, in that order (they nest), are shown as <data>, <repo> and ~. A path of another spelling (a resolved
// symbolic link) is not recognized, which is why documents are built without paths in the first place.
func (env Env) redact(text string) string {
	for _, r := range [][2]string{{env.Getenv("AGENTIUM_HOME"), "<data>"}, {env.Dir, "<repo>"}, {env.Getenv("HOME"), "~"}} {
		if len(r[0]) < 2 {
			continue
		}
		spellings := []string{r[0]}
		if real, err := filepath.EvalSymlinks(r[0]); err == nil && real != r[0] {
			spellings = []string{real, r[0]} // the resolved one first: it can contain the other (/private/var/x holds /var/x's tail)
		}
		for _, spelling := range spellings {
			text = strings.ReplaceAll(text, strings.TrimSuffix(spelling, "/"), r[1])
		}
	}
	return text
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
	_, err := io.WriteString(env.json.out, env.redact(buf.String()))
	return err
}

// list returns s, or an empty non-nil list: JSON lists are never null.
func list[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
