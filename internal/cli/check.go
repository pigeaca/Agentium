package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

const checkUsage = `Usage:
  agentium check add NAME (--ran TEXT | --changed GLOB | --not-changed GLOB)
                         a rule check counts the runs that followed a rule of yours: the agent ran a command
                         containing TEXT (a plain substring, case kept), changed a file that matches GLOB, or
                         changed no file that matches it
  agentium check list
  agentium check rm NAME

Rule checks belong to the project and are kept in the data folder, never in your repository. An experiment's report
counts, for each version, the runs that met each check, read again from every run's stored transcript and change when
the report is made, so experiments that ran before a check existed are counted too. They are counts, never a verdict.
A run whose transcript (--ran) or change (--changed, --not-changed) is missing or unreadable is shown as unread.

Commands are the shell commands the agent ran, less the ones that were denied. Paths are the files the agent's change
touches (a rename counts under both names, a deleted file as changed); GLOB: * within a folder, ** across folders, ?
one character, {a,b} either; a pattern without a slash also matches a file name in any folder.

add, list and rm take --json: one JSON document instead of text; exit codes are 0 success, 1 failure, 2 usage.
`

func runCheck(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, checkUsage)
		return ExitUsage
	}
	commands := map[string]func(context.Context, Env, []string) int{"add": checkAdd, "list": checkList, "rm": checkRemove}
	if run, ok := commands[args[0]]; ok {
		return run(ctx, env, args[1:])
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(env.Stdout, checkUsage)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "agentium check: unknown subcommand %q\n\n%s", args[0], checkUsage)
	return ExitUsage
}

// checkInfo is a rule check as the commands' documents show it.
type checkInfo struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"` // ran | changed | not-changed
	Pattern string `json:"pattern"`
}

func checkInfoOf(c store.RuleCheck) checkInfo {
	return checkInfo{Name: c.Name, Kind: c.Kind, Pattern: c.Pattern}
}

type checkAddDoc struct {
	header
	Check checkInfo `json:"check"`
}

type checkListDoc struct {
	header
	Checks []checkInfo `json:"checks"`
}

type checkRemovedDoc struct {
	header
	Removed string `json:"removed"`
}

func checkAdd(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("check add", flag.ContinueOnError)
	var given [][2]string // each flag given, in order: its name (the check's kind) and value
	for _, f := range [][2]string{{run.CheckRan, "met by a run in which a shell command the agent ran contains `TEXT`"},
		{run.CheckChanged, "met by a run whose change touches a file matching `GLOB`"},
		{run.CheckNotChanged, "met by a run whose change touches no file matching `GLOB`"}} {
		kind := f[0]
		fs.Func(kind, f[1], func(v string) error { given = append(given, [2]string{kind, v}); return nil })
	}
	rest, code, ok := parseArgs(env, fs, args, checkUsage)
	if !ok {
		return code
	}
	name, ok := oneName(env, "check add", rest, checkUsage)
	if !ok {
		return ExitUsage
	}
	if !snapshot.ValidName(name) {
		fmt.Fprintf(env.Stderr, "agentium check add: %q is not a valid name: lowercase letters, digits, '.', '_' and '-', starting with a letter or digit, at most 63 characters\n", name)
		return ExitUsage
	}
	if len(given) != 1 {
		fmt.Fprintf(env.Stderr, "agentium check add: give exactly one of --ran, --changed and --not-changed\n\n%s", checkUsage)
		return ExitUsage
	}
	kind, pattern := given[0][0], given[0][1]
	if pattern == "" {
		fmt.Fprintf(env.Stderr, "agentium check add: --%s needs a value that is not empty\n", kind)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	saved, err := w.db.SaveRuleCheck(ctx, store.RuleCheck{ProjectID: w.project.ID, Name: name, Kind: kind, Pattern: pattern, CreatedAt: env.Now()})
	if errors.Is(err, store.ErrExists) {
		return fail(env, fmt.Errorf("check %q already exists: remove it first (agentium check rm %s)", name, name))
	} else if err != nil {
		return fail(env, err)
	}
	if env.JSON {
		return env.emit(checkAddDoc{header: env.hdr(), Check: checkInfoOf(saved)})
	}
	st := env.style()
	fmt.Fprintf(env.Stdout, "Added check %s: %s\n", term.Sanitize(name), ruleCheckWords(saved))
	fmt.Fprintln(env.Stdout, st.Note("Reports count the runs that met it; agentium check list shows them all."))
	return ExitOK
}

// ruleCheckWords is a check's rule in words.
func ruleCheckWords(c store.RuleCheck) string {
	pattern := term.Sanitize(c.Pattern)
	switch c.Kind {
	case run.CheckRan:
		return fmt.Sprintf("the agent ran a command containing %q", pattern)
	case run.CheckChanged:
		return "the agent changed a file matching " + pattern
	default:
		return "the agent changed no file matching " + pattern
	}
}

func checkList(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("check list", flag.ContinueOnError), args, checkUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 {
		fmt.Fprint(env.Stderr, checkUsage)
		return ExitUsage
	}
	w, err := openProjectFor(ctx, env, true)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	checks, err := w.db.RuleChecks(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	if env.JSON {
		doc := checkListDoc{header: env.hdr(), Checks: []checkInfo{}}
		for _, c := range checks {
			doc.Checks = append(doc.Checks, checkInfoOf(c))
		}
		return env.emit(doc)
	}
	st := env.style()
	if len(checks) == 0 {
		fmt.Fprintf(env.Stdout, "No rule checks yet: %s counts the runs that followed a rule of yours\n", st.Command("agentium check add NAME --ran TEXT"))
		return ExitOK
	}
	table := term.NewTable(st, term.Left("NAME"), term.Left("KIND"), term.Left("PATTERN"))
	for _, c := range checks {
		table.Row(term.Sanitize(c.Name), c.Kind, term.Sanitize(c.Pattern))
	}
	if err := table.Write(env.Stdout); err != nil {
		return fail(env, err)
	}
	return ExitOK
}

func checkRemove(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("check rm", flag.ContinueOnError), args, checkUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		fmt.Fprint(env.Stderr, checkUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	if err := w.db.DeleteRuleCheck(ctx, w.project.ID, rest[0]); err != nil {
		return fail(env, err)
	}
	if env.JSON {
		return env.emit(checkRemovedDoc{header: env.hdr(), Removed: rest[0]})
	}
	fmt.Fprintf(env.Stdout, "Removed check %s\n", term.Sanitize(rest[0]))
	return ExitOK
}
