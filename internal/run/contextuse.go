package run

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/source"
)

// ContextUse is what a run used of its arm's context. Records keep only what the repository itself defines, and
// Claude Code's own subagent types: paths relative to the repository and the project's skill, command and subagent
// names. Local paths, personal names and what the agent said stay out.
type ContextUse struct {
	// Start are the files Claude Code loads whole at session start: the instruction file, its imports and unscoped
	// rules. Skill, subagent and command descriptions load at start too, but not their bodies, so they are not here.
	Start []string `json:"start"`
	// Files are the context files the run used after it started: path-scoped rules and folder instruction files that
	// loaded because the agent worked with matching files, and any on-demand context file or linked document the
	// agent or its subagents read (with the Read tool, or as an argument to cat, sed, grep and similar).
	Files     []string `json:"files,omitempty"`
	Skills    []string `json:"skills,omitempty"`    // the project's skills and commands it invoked with the Skill tool
	Subagents []string `json:"subagents,omitempty"` // the project's and Claude Code's subagent types it started
	// OtherSubagents counts the other subagent types it started, which may be personal: not named.
	OtherSubagents int `json:"other_subagents,omitempty"`
}

// builtinSubagents are Claude Code's own subagent types, which records may name.
var builtinSubagents = []string{"general-purpose", "Explore", "Plan"}

// UseOf works out a run's context use from its transcript's metrics and the context its arm started with (resolved
// from src). dirs are the folders the agent's file tools name paths under: its checkout (as written and resolved),
// and the working folder its transcript reports.
func UseOf(resolved claudectx.Context, src source.Source, m claude.Metrics, dirs ...string) ContextUse {
	return UseOfIn(resolved, src, m, "", dirs...)
}

// UseOfIn is UseOf for a run whose agent started in module (resolved with claudectx.ResolveIn): dirs still begin with
// the checkout's root, so paths stay relative to it, but a command's relative file names are the module folder's, and
// a module's path-scoped rules match paths in the module.
func UseOfIn(resolved claudectx.Context, src source.Source, m claude.Metrics, module string, dirs ...string) ContextUse {
	use := ContextUse{Start: []string{}}
	touched := relativeAll(m.FilePaths, dirs) // files the agent worked with, through any file tool
	read := relativeAll(m.ReadPaths, dirs)
	used := map[string]bool{}
	// Only Bash commands that ran: a denied `cat AGENTS.md` read nothing (claude.Metrics.RanCommands).
	byReading := func(p string) bool { return slices.Contains(read, p) || namedByReader(p, module, m.RanCommands, dirs) }
	for _, e := range resolved.Entries {
		switch e.Kind {
		case claudectx.KindInstructions, claudectx.KindImport, claudectx.KindRule:
			use.Start = append(use.Start, e.Path)
		case claudectx.KindHarness: // changes what runs, not what the model reads
		case claudectx.KindScopedRule:
			data, _ := src.ReadFile(e.Path)
			globs := claudectx.RuleGlobs(data)
			// A rule's patterns name paths in the folder whose .claude holds it (the root's, or a module's).
			folder := ""
			if before, _, ok := strings.Cut(e.Path, "/.claude/rules/"); ok {
				folder = before + "/"
			}
			used[e.Path] = byReading(e.Path) || slices.ContainsFunc(touched, func(p string) bool {
				rel, ok := strings.CutPrefix(p, folder)
				return ok && slices.ContainsFunc(globs, func(g string) bool { return claudectx.GlobMatch(g, rel) })
			})
		case claudectx.KindNested: // loads when the agent works with a file in its folder
			dir := path.Dir(e.Path) + "/"
			used[e.Path] = byReading(e.Path) || slices.ContainsFunc(touched, func(p string) bool { return strings.HasPrefix(p, dir) })
		default:
			used[e.Path] = byReading(e.Path)
		}
	}
	for _, p := range resolved.Linked {
		used[p] = byReading(p)
	}
	for p, ok := range used {
		if ok {
			use.Files = append(use.Files, p)
		}
	}
	project := append(claudectx.SkillNames(resolved, src), claudectx.CommandNames(resolved)...)
	for _, name := range m.SkillCalls {
		if slices.Contains(project, name) && !slices.Contains(use.Skills, name) {
			use.Skills = append(use.Skills, name)
		}
	}
	agents := append(claudectx.SubagentNames(resolved, src), builtinSubagents...)
	for _, kind := range m.SubagentTypes {
		if slices.Contains(agents, kind) {
			use.Subagents = append(use.Subagents, kind)
		} else {
			use.OtherSubagents++
		}
	}
	sort.Strings(use.Start)
	sort.Strings(use.Files)
	sort.Strings(use.Skills)
	sort.Strings(use.Subagents)
	return use
}

// Recovery works out the context use of runs recorded before records kept it, from each run's transcript and its arm's
// starting context: the task's base commit, with the arm's snapshot applied when it has one, both in the bare
// repository Bare. Records is the data folder's records folder, where transcripts are looked for first (a record's own
// folder may name a data folder that has since moved). One value serves one command: it keeps each base and snapshot's
// resolved context, or why it could not be resolved, which every run of a task in that arm shares.
type Recovery struct {
	Bare, Records string
	contexts      map[[3]string]armStart // by base, snapshot and module
}

type armStart struct {
	src      source.Source
	resolved claudectx.Context
	err      error
}

// Recover returns rec's context use: the record's own when it has one, else one worked out from its transcript. A run
// whose transcript is gone gets nil and no error. Errors name the arm, not the run, so a caller can report each once.
func (r *Recovery) Recover(ctx context.Context, rec Record, base, snapshotCommit string) (*ContextUse, error) {
	if rec.ContextUse != nil {
		return rec.ContextUse, nil
	}
	var dirs []string
	if r.Records != "" && rec.ID != "" {
		dirs = append(dirs, filepath.Join(r.Records, rec.ID))
	}
	if rec.RecordsDir != "" {
		dirs = append(dirs, rec.RecordsDir)
	}
	transcript := ""
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "stream.jsonl")); err == nil {
			transcript = filepath.Join(dir, "stream.jsonl")
			break
		}
	}
	if transcript == "" {
		return nil, nil
	}
	m, err := parseFile(transcript)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("a transcript of arm %s: %w", rec.Arm, err)
	}
	start, err := r.start(ctx, base, snapshotCommit, rec.Module)
	if err != nil {
		return nil, fmt.Errorf("arm %s: %w", rec.Arm, err)
	}
	// The transcript names only where the agent started: in a module, the checkout's root is that folder's ancestor.
	root := m.CWD
	if rec.Module != "" {
		root = strings.TrimSuffix(filepath.Clean(m.CWD), string(filepath.Separator)+filepath.FromSlash(rec.Module))
	}
	use := UseOfIn(start.resolved, start.src, m, rec.Module, root, m.CWD)
	return &use, nil
}

// start resolves the context a run started with, in module (where its agent started): base with snapshotCommit applied
// (none: base's own). Failures are kept too, except cancellation, which leaves nothing behind (a cancelled read can make
// a context look smaller than it is).
func (r *Recovery) start(ctx context.Context, base, snapshotCommit, module string) (armStart, error) {
	key := [3]string{base, snapshotCommit, module}
	if s, ok := r.contexts[key]; ok {
		return s, s.err
	}
	s := r.resolve(ctx, base, snapshotCommit, module)
	if err := ctx.Err(); err != nil {
		return armStart{}, err
	}
	if r.contexts == nil {
		r.contexts = map[[3]string]armStart{}
	}
	r.contexts[key] = s
	return s, s.err
}

func (r *Recovery) resolve(ctx context.Context, base, snapshotCommit, module string) armStart {
	src, err := source.Commit(ctx, base, "--git-dir", r.Bare)
	if err != nil {
		return armStart{err: err}
	}
	if snapshotCommit != "" {
		snap, err := source.Commit(ctx, snapshotCommit, "--git-dir", r.Bare)
		if err != nil {
			return armStart{err: err}
		}
		if src, err = snapshot.ApplyIn(src, snap, module); err != nil {
			return armStart{err: err}
		}
	}
	resolved, err := claudectx.ResolveIn(src, module)
	if err != nil {
		return armStart{err: err}
	}
	return armStart{src: src, resolved: resolved}
}

// relativeAll returns the paths under dirs, relative to them with forward slashes, each once.
func relativeAll(paths, dirs []string) []string {
	var out []string
	for _, p := range paths {
		if rel, ok := relativeTo(p, dirs); ok && !slices.Contains(out, rel) {
			out = append(out, rel)
		}
	}
	return out
}

// relativeTo returns p relative to the first of dirs that holds it, with forward slashes.
func relativeTo(p string, dirs []string) (string, bool) {
	p = filepath.Clean(p)
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if rel, ok := strings.CutPrefix(p, filepath.Clean(dir)+string(filepath.Separator)); ok {
			return filepath.ToSlash(rel), true
		}
	}
	return "", false
}

// readers are commands that read the files they are given; searchers among them take a pattern first.
var (
	readers = []string{"cat", "head", "tail", "less", "more", "sed", "awk", "grep", "egrep", "fgrep", "rg", "nl", "bat", "wc", "cut",
		"sort", "uniq", "diff", "cmp", "jq", "view"}
	searchers = []string{"grep", "egrep", "fgrep", "rg"}
)

// namedByReader reports whether a shell command reads p: p is a file argument of a reading command (cat, sed, grep...),
// written as p, ./p (relative to module's folder when module is set: where the agent started) or under one of dirs. Not counted: a search pattern (grep's first operand, unless -e or -f gave
// the pattern), a redirection's target, sed -i (which writes) and sort -o's output.
func namedByReader(p, module string, commands, dirs []string) bool {
	// Relative names are the starting folder's: the root's, or the module's (where a root file has no plain name).
	names := []string{p, "./" + p}
	if module != "" {
		names = nil
		if rel, ok := strings.CutPrefix(p, module+"/"); ok {
			names = []string{rel, "./" + rel}
		}
	}
	for _, dir := range dirs {
		if dir != "" {
			names = append(names, filepath.ToSlash(filepath.Clean(dir))+"/"+p)
		}
	}
	for _, line := range commands {
		for _, args := range simpleCommands(line) {
			for len(args) > 0 && strings.Contains(args[0], "=") && !strings.HasPrefix(args[0], "-") {
				args = args[1:] // VAR=value before the command
			}
			if len(args) == 0 || !slices.Contains(readers, path.Base(args[0])) {
				continue
			}
			name := path.Base(args[0])
			if name == "sed" && slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") }) {
				continue
			}
			patternPending := slices.Contains(searchers, name) && !slices.ContainsFunc(args, func(a string) bool {
				return a == "-e" || a == "-f" || strings.HasPrefix(a, "--regexp") || strings.HasPrefix(a, "--file")
			})
			for i := 1; i < len(args); i++ {
				arg := args[i]
				if before, _, found := strings.Cut(arg, ">"); found { // a redirection: its target is written
					if strings.HasSuffix(arg, ">") {
						i++ // the target is the next argument
					}
					if before != "" && strings.Trim(before, "0123456789") != "" && slices.Contains(names, before) {
						return true // cat docs/a.md>out reads docs/a.md
					}
					continue
				}
				switch {
				case name == "sort" && arg == "-o":
					i++
					continue
				case name == "sort" && (strings.HasPrefix(arg, "-o") || strings.HasPrefix(arg, "--output")):
					continue
				case strings.HasPrefix(arg, "-"):
					continue
				case patternPending:
					patternPending = false
					continue
				}
				if slices.Contains(names, arg) {
					return true
				}
			}
		}
	}
	return false
}

// simpleCommands splits a shell command line into simple commands and their arguments. Single and double quotes group
// and are removed; unquoted |, ;, &, newlines and parentheses end a command. It is not a shell parser, just enough to
// see which files a reading command was given.
func simpleCommands(line string) [][]string {
	var commands [][]string
	var args []string
	var word strings.Builder
	inWord := false
	var quote rune
	endWord := func() {
		if inWord {
			args = append(args, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(args) > 0 {
			commands = append(commands, args)
			args = nil
		}
	}
	for _, c := range line {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				word.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case strings.ContainsRune("|;&\n()", c):
			endCommand()
		case unicode.IsSpace(c):
			endWord()
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	endCommand()
	return commands
}
