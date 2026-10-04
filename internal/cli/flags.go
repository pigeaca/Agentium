package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// parseArgs parses args with fs, allowing flags between positional arguments (the flag package stops at the first
// positional one); everything after "--" is positional. -h and --help print usage to stdout; a bad flag prints the
// error and usage to stderr. When ok is false the command returns code.
func parseArgs(env Env, fs *flag.FlagSet, args []string, usage string) (positional []string, code int, ok bool) {
	fs.SetOutput(io.Discard)
	if env.flagSink != nil {
		env.flagSink(fs)
	}
	for {
		err := fs.Parse(args)
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return nil, ExitOK, false
		}
		if instead := removedHit(fs); instead != "" { // a removed flag names its replacement, without the whole usage
			fmt.Fprintf(env.Stderr, "agentium %s: %s\n", fs.Name(), instead)
			return nil, ExitUsage, false
		}
		if why := badValue(fs); err != nil && why != "" { // a value that says why itself, without the whole usage
			fmt.Fprintf(env.Stderr, "agentium %s: %s\n", fs.Name(), why)
			return nil, ExitUsage, false
		}
		if err != nil {
			fmt.Fprintf(env.Stderr, "agentium %s: %v\n\n%s", fs.Name(), err, usage)
			return nil, ExitUsage, false
		}
		rest := fs.Args()
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), ExitOK, true
		}
		if len(rest) == 0 {
			return positional, ExitOK, true
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// oneName returns the one positional NAME a command takes. Otherwise it prints why under the command's name (fs's),
// then usage, to stderr, and ok is false: the command returns ExitUsage.
func oneName(env Env, command string, rest []string, usage string) (name string, ok bool) {
	switch len(rest) {
	case 1:
		return rest[0], true
	case 0:
		fmt.Fprintf(env.Stderr, "agentium %s: give a NAME\n\n%s", command, usage)
	default:
		quoted := make([]string, len(rest))
		for i, r := range rest {
			quoted[i] = fmt.Sprintf("%q", r)
		}
		fmt.Fprintf(env.Stderr, "agentium %s: expected one NAME, got %d: %s (put each flag's value right after it, and quote a value with spaces)\n\n%s",
			command, len(rest), strings.Join(quoted, ", "), usage)
	}
	return "", false
}

// removedFlag is a flag that no longer exists. It still parses, with or without a value, only to fail: parseArgs then
// reports that it was removed and names its replacement (instead), rather than "flag provided but not defined".
type removedFlag struct {
	name, instead string
	hit           bool
}

// errRemovedFlag stops the parse at a removed flag; parseArgs prints the replacement instead of it.
var errRemovedFlag = errors.New("removed")

func (r *removedFlag) String() string { return "" }

// IsBoolFlag lets the flag appear without a value: a removed --old FILE then leaves FILE positional, and the parse
// fails at the flag either way.
func (r *removedFlag) IsBoolFlag() bool { return true }

func (r *removedFlag) Set(string) error {
	r.hit = true
	return errRemovedFlag
}

// removeFlags registers flags that were removed, each with what replaces it: say "use --allow-local-binding=false".
// They appear in no usage text.
func removeFlags(fs *flag.FlagSet, replacements map[string]string) {
	for name, instead := range replacements {
		fs.Var(&removedFlag{name: name, instead: instead}, name, "removed: "+instead)
	}
}

// removedHit is the message for the removed flag that stopped fs's parse; "" when none did.
func removedHit(fs *flag.FlagSet) string {
	var msg string
	fs.VisitAll(func(f *flag.Flag) {
		if r, ok := f.Value.(*removedFlag); ok && r.hit && msg == "" {
			msg = fmt.Sprintf("--%s was removed: %s", r.name, r.instead)
		}
	})
	return msg
}
