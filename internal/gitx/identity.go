package gitx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// localeVariables are the variables that set the locale git matches text in, in the order the C library reads them.
var localeVariables = []string{"LC_ALL", "LC_CTYPE", "LANG"}

// Identity tells one git, as Agentium runs it, from another: the program found on PATH, its version and build
// options, and the locale variables it runs under. An answer of git that Agentium keeps between commands
// (task.GapsCache) is kept for one identity: a search that ignores case can match differently under another build of
// git or another locale, so what one found says nothing sure about the other.
func Identity(ctx context.Context) (string, error) {
	program, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git's identity: %w", err)
	}
	version, err := Run(ctx, "version", "--build-options")
	if err != nil {
		return "", fmt.Errorf("git's identity: %w", err)
	}
	parts := []string{program, version}
	for _, name := range localeVariables { // git gets them from this process: Environ keeps them
		parts = append(parts, name+"="+os.Getenv(name))
	}
	return strings.Join(parts, "\n"), nil
}
