package cli

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
)

// A project's settings (store.Settings) are set once with `agentium init` and read by task import, pool update, start
// and task validate. Each of those commands may take some of them as its own hidden flags: a flag given wins for that
// call only, then the stored setting, then the built-in default. A project with no settings behaves as before they
// existed.

// The settings' names, as flags of init and as the commands' overrides.
const (
	settingVerify        = "verify"
	settingSetup         = "setup"
	settingRequireLock   = "require-lock"
	settingJobs          = "jobs"
	settingVerifyTimeout = "verify-timeout"
)

// defaultJobs is how many tasks are validated at once when neither a flag nor the project's setting says.
const defaultJobs = 2

// settingFlags are the settings a command takes as flags (all of them for init; hidden overrides elsewhere).
type settingFlags struct {
	fs            *flag.FlagSet
	verify, setup stringList
	requireLock   bool
	jobs          int
	verifyTimeout time.Duration
	// timeoutName is the verify timeout's flag name: "verify-timeout", or task validate's older "timeout".
	timeoutName string
}

// addSettingFlags registers the named settings as flags of fs; the verify timeout's flag is called timeoutName.
func addSettingFlags(fs *flag.FlagSet, timeoutName string, names ...string) *settingFlags {
	f := &settingFlags{fs: fs, timeoutName: timeoutName}
	for _, name := range names {
		switch name {
		case settingVerify:
			fs.Var(&f.verify, settingVerify, "a verification command for the tasks (repeatable; an empty one: the detected commands)")
		case settingSetup:
			fs.Var(&f.setup, settingSetup, "a command a fresh checkout needs first (repeatable; an empty one: none)")
		case settingRequireLock:
			fs.BoolVar(&f.requireLock, settingRequireLock, false, "set aside Python commits whose base pins no dependencies")
		case settingJobs:
			fs.IntVar(&f.jobs, settingJobs, 0, "how many tasks to validate at once")
		case settingVerifyTimeout:
			fs.DurationVar(&f.verifyTimeout, timeoutName, 0, "time limit for each setup or verification command")
		default:
			panic("unknown setting " + name)
		}
	}
	return f
}

// given reports whether the setting's flag was given.
func (f *settingFlags) given(name string) bool {
	if name == settingVerifyTimeout {
		name = f.timeoutName
	}
	found := false
	f.fs.Visit(func(fl *flag.Flag) { found = found || fl.Name == name })
	return found
}

// any reports whether any setting's flag was given.
func (f *settingFlags) any() bool {
	return slices.ContainsFunc([]string{settingVerify, settingSetup, settingRequireLock, settingJobs, settingVerifyTimeout}, f.given)
}

// check refuses values no command can use; the error names the flag.
func (f *settingFlags) check() error {
	switch {
	case f.given(settingJobs) && f.jobs < 1:
		return errors.New("--jobs must be at least 1")
	case f.given(settingVerifyTimeout) && f.verifyTimeout <= 0:
		return fmt.Errorf("--%s must be more than 0", f.timeoutName)
	}
	return nil
}

// apply returns s with the flags given in place of its values. Empty commands are dropped, so an empty --verify
// (alone) returns to the detected commands and an empty --setup to none.
func (f *settingFlags) apply(s store.Settings) store.Settings {
	if f.given(settingVerify) {
		s.Verify = nonEmpty(f.verify)
	}
	if f.given(settingSetup) {
		s.Setup = nonEmpty(f.setup)
	}
	if f.given(settingRequireLock) {
		s.RequireLock = f.requireLock
	}
	if f.given(settingJobs) {
		s.Jobs = f.jobs
	}
	if f.given(settingVerifyTimeout) {
		s.VerifyTimeout = f.verifyTimeout
	}
	return s
}

// taskCommands are the verify and setup commands a task gets from the flags given. nil means not given, and
// completeTask takes the project's; an empty --setup gives an empty, non-nil list: no setup, whatever the project's is.
func (f *settingFlags) taskCommands() (verify, setup []string) {
	if f.given(settingVerify) {
		verify = nonEmpty(f.verify)
	}
	if f.given(settingSetup) {
		setup = append([]string{}, nonEmpty(f.setup)...)
	}
	return verify, setup
}

func nonEmpty(commands []string) []string {
	var out []string
	for _, c := range commands {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// jobsOf is how many tasks to validate at once under s.
func jobsOf(s store.Settings) int {
	if s.Jobs > 0 {
		return s.Jobs
	}
	return defaultJobs
}

// verifyTimeoutOf is the time limit of each setup or verification command under s.
func verifyTimeoutOf(s store.Settings) time.Duration {
	if s.VerifyTimeout > 0 {
		return s.VerifyTimeout
	}
	return experiment.DefaultVerifyTimeout
}

// settings is the project's stored settings.
func (w *workspace) settings() store.Settings { return w.project.Settings }
