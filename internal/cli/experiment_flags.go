package cli

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
)

// judgeFlag is --judge[=MODEL[:EFFORT]] or --judge-pairs[=MODEL[:EFFORT]]: a boolean flag that may name the judge's
// model and effort. Alone it turns the judge on at its defaults; a true or false value (as strconv.ParseBool reads
// it) sets it, and any other value is the judge's MODEL[:EFFORT], which turns it on. A value must follow "=": the
// flag package reads a separate word as a positional argument, as with every boolean flag.
type judgeFlag struct {
	name          string // the flag's, for its error
	on            bool
	model, effort string // "": the default judge's
	invalid       string // why the last value was refused, under the flag's name (badValue)
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
		*j = judgeFlag{name: j.name, on: on}
		return nil
	}
	model, effort, err := experiment.ParseProfile(s)
	if err != nil {
		// ParseProfile's error starts with the quoted value, which the message already shows after the flag.
		reason := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(err.Error(), strconv.Quote(s)), ":"))
		j.invalid = fmt.Sprintf("--%s=%s: %s", j.name, s, reason)
		return err
	}
	*j = judgeFlag{name: j.name, on: true, model: model, effort: effort}
	return nil
}

// badValue is why a flag value that explains itself (judgeFlag) stopped fs's parse, rather than the flag package's
// "invalid boolean value" wording; "" when none did.
func badValue(fs *flag.FlagSet) string {
	var msg string
	fs.VisitAll(func(f *flag.Flag) {
		if j, ok := f.Value.(*judgeFlag); ok && j.invalid != "" && msg == "" {
			msg = j.invalid
		}
	})
	return msg
}
