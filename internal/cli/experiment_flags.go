package cli

import (
	"flag"
	"fmt"
	"strconv"

	"github.com/pigeaca/agentium/internal/experiment"
)

// judgeFlag is --judge[=MODEL[:EFFORT]] or --judge-pairs[=MODEL[:EFFORT]]: a boolean flag that may name the judge's
// model and effort. Alone it turns the judge on at its defaults; a true or false value (as strconv.ParseBool reads
// it) sets it, and any other value is the judge's MODEL[:EFFORT], which turns it on. A value must follow "=": the
// flag package reads a separate word as a positional argument, as with every boolean flag.
type judgeFlag struct {
	on            bool
	model, effort string // "": the default judge's
}

func (j *judgeFlag) IsBoolFlag() bool { return true }

func (j *judgeFlag) String() string {
	switch {
	case j == nil || !j.on:
		return "false"
	case j.model == "":
		return "true"
	}
	return experiment.Profile(j.model, j.effort)
}

func (j *judgeFlag) Set(s string) error {
	if on, err := strconv.ParseBool(s); err == nil {
		*j = judgeFlag{on: on}
		return nil
	}
	model, effort, err := experiment.ParseProfile(s)
	if err != nil {
		return err
	}
	*j = judgeFlag{on: true, model: model, effort: effort}
	return nil
}

// removedFlags are flags a command no longer has. Each still parses, taking a value as it used to, so the command
// can name its replacement (exit 2) instead of failing on an unknown flag, or on its value read as an argument.
type removedFlags struct {
	replacements map[string]string // the flag's name: what to use instead
	used         []string          // the removed flags given, in order
}

// removeFlags registers each removed flag of replacements on fs. Call report after parsing.
func removeFlags(fs *flag.FlagSet, replacements map[string]string) *removedFlags {
	r := &removedFlags{replacements: replacements}
	for name := range replacements {
		fs.Var(removedValue{name: name, flags: r}, name, "removed: "+replacements[name])
	}
	return r
}

// report prints, under the command's name, each removed flag that was given with its replacement. ok is false when
// there was one: the command returns ExitUsage.
func (r *removedFlags) report(env Env, command string) (ok bool) {
	for _, name := range r.used {
		fmt.Fprintf(env.Stderr, "agentium %s: --%s was removed: %s\n", command, name, r.replacements[name])
	}
	return len(r.used) == 0
}

// removedValue records that its removed flag was given.
type removedValue struct {
	name  string
	flags *removedFlags
}

func (v removedValue) String() string { return "" }

func (v removedValue) Set(string) error {
	v.flags.used = append(v.flags.used, v.name)
	return nil
}
