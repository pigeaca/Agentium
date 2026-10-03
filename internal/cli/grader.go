package cli

import (
	"flag"
	"runtime"

	"github.com/pigeaca/agentium/internal/task"
)

// graderFlag is --grader host|sandbox: where the verification commands run (the isolation plan, Part 1). Without it a
// command takes the platform's default (defaultGrader).
type graderFlag struct{ value *string }

func addGraderFlag(fs *flag.FlagSet) graderFlag {
	return graderFlag{fs.String("grader", "", "where the verification runs: sandbox (Agentium's grading sandbox; the default on macOS) or host")}
}

// mode is the grader mode asked for, or env's default; a mistake is a usage error's text.
func (g graderFlag) mode(env Env) (string, error) {
	if g.value == nil || *g.value == "" {
		return defaultGrader(env), nil
	}
	return task.ParseGrader(*g.value)
}

// defaultGrader is the mode a command uses without --grader: Env.DefaultGrader when set (tests pin it), else the
// platform's (task.DefaultGrader: the sandbox on macOS, the host elsewhere).
func defaultGrader(env Env) string {
	if env.DefaultGrader != "" {
		return env.DefaultGrader
	}
	return task.DefaultGrader(runtime.GOOS)
}
