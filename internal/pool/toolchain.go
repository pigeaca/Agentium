package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/task"
)

// versionCommand is how a tool reports its version: the first non-empty line of its output.
type versionCommand struct {
	tool string
	args []string
}

// versionCommands are the tools whose versions a validation records, per build-tool profile (buildtool.Profile.Name).
// Gradle itself is not asked: its wrapper pins it per commit, which the task's base fixes; the JDK under it is the
// host's. Maven is asked as well as the JDK, as projects without a wrapper build with the host's.
func versionCommands(profile string) []versionCommand {
	java := versionCommand{"java", []string{"java", "-version"}} // prints to stderr
	switch profile {
	case "go":
		return []versionCommand{{"go", []string{"go", "env", "GOVERSION"}}}
	case "maven":
		return []versionCommand{{"maven", []string{"mvn", "--version"}}, java}
	case "gradle":
		return []versionCommand{java}
	case "cargo":
		return []versionCommand{{"cargo", []string{"cargo", "--version"}}, {"rustc", []string{"rustc", "--version"}}}
	}
	return nil
}

// maxVersion bounds a recorded version's length, in bytes.
const maxVersion = 200

// VersionOutput runs a version command and returns its output, stdout and stderr together; an error when it could not
// run or failed.
type VersionOutput func(ctx context.Context, args []string) (string, error)

// DetectToolchain asks the build tools of the named profiles for their versions, once each. A tool that cannot be run
// or fails is left out (Policy.Stale never counts a missing tool). Only ctx's cancellation is an error.
func DetectToolchain(ctx context.Context, profiles []string, run VersionOutput) (task.Toolchain, error) {
	found := task.Toolchain{}
	for _, profile := range profiles {
		for _, c := range versionCommands(profile) {
			if _, done := found[c.tool]; done {
				continue
			}
			out, err := run(ctx, c.args)
			if ctx.Err() != nil {
				return nil, fmt.Errorf("detect the toolchain: %w", ctx.Err())
			}
			if err != nil {
				continue
			}
			if v := firstLine(out); v != "" {
				found[c.tool] = v
			}
		}
	}
	return found, nil
}

// firstLine is the first non-empty line of out, trimmed, cut to maxVersion bytes on a rune boundary.
func firstLine(out string) string {
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); line != "" {
			return strings.ToValidUTF8(line[:min(len(line), maxVersion)], "") // a rune cut in two is dropped
		}
	}
	return ""
}

// versionTimeout bounds one version command.
const versionTimeout = 30 * time.Second

// HostVersions is a VersionOutput running commands on the host as Agentium's own commands run (runner.Run: its own
// process group, credentials dropped from environ, a timeout), in dir, which should be a folder of Agentium's (not a
// checkout: wrappers and a go.mod there could pick or download other tools). GOTOOLCHAIN=local keeps Go from fetching
// a toolchain to answer.
func HostVersions(dir string, environ []string) VersionOutput {
	return func(ctx context.Context, args []string) (string, error) {
		var out limitedBuffer
		res, err := runner.Run(ctx, runner.Spec{Dir: dir, Args: args, Environ: runner.Environ(environ), Env: []string{"GOTOOLCHAIN=local"},
			Timeout: versionTimeout, Output: &out})
		if err != nil {
			return "", err
		}
		if !res.Passed() {
			return "", errors.New(strings.Join(args, " ") + " failed")
		}
		return out.String(), nil
	}
}

// limitedBuffer keeps the first 64 KiB written to it and drops the rest: a version is a line. The buffer is a field,
// not embedded, so that io.Copy cannot bypass Write through bytes.Buffer's ReadFrom.
type limitedBuffer struct{ buf bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 64<<10 - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
