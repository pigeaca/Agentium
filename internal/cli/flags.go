package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
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

// usageError is a mistake in how a command was called that only the services behind it could find: the handler reports
// it as a usage error (exit 2) under the command's name.
type usageError string

func (e usageError) Error() string { return string(e) }
