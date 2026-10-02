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
	for {
		err := fs.Parse(args)
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return nil, ExitOK, false
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
