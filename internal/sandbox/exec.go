package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// Exec is macOS's sandbox-exec, by absolute path: never one found on PATH.
const Exec = "/usr/bin/sandbox-exec"

// ErrUnavailable means the sandbox could not be shown to work: sandbox-exec is missing, refuses the profile, runs
// nested inside another sandbox (Agentium inside an agent's), or a canary probe did not behave as the profile says.
// A grade that meets it is an infrastructure outcome (retried or left out), never a fail, and never graded on the host
// instead.
var ErrUnavailable = errors.New("the grading sandbox is unavailable")

// Wrap returns spec run under the profile in profileFile (an absolute path, written by Profile.WriteFile):
// `sandbox-exec -f <profileFile> /bin/sh -c <Command>`, or sandbox-exec followed by Args. Everything else is kept:
// sandbox-exec applies the profile and then executes the command in its own process, so the process ID (Started) and
// its process group are the command's, and runner stops the group as before. Its exit code passes through, except
// that sandbox-exec itself exits 65 when it cannot apply the profile, which a command can also return: the canary
// tells the two apart before a grade's commands run.
func Wrap(spec runner.Spec, profileFile string) (runner.Spec, error) {
	if !filepath.IsAbs(profileFile) {
		return runner.Spec{}, fmt.Errorf("sandbox profile file %q is not absolute", profileFile)
	}
	argv := spec.Args
	if len(argv) == 0 {
		if spec.Command == "" {
			return runner.Spec{}, errors.New("a sandboxed command needs a command or arguments")
		}
		argv = []string{"/bin/sh", "-c", spec.Command}
	}
	spec.Args = append([]string{Exec, "-f", profileFile}, argv...)
	spec.Command = ""
	return spec, nil
}

// canaryTimeout bounds each canary probe: they start a few small system tools.
const canaryTimeout = 30 * time.Second

// Canary checks, before a grade's commands, that profileFile (p's profile, as WriteFile wrote it) really sandboxes:
// under it /usr/bin/true succeeds, the temp root can be written, the data folder cannot be listed and cannot be
// written. The data folder is listed outside the sandbox first, so a denial is told from a missing folder. A failed
// probe returns an error wrapping ErrUnavailable; cancellation returns ctx's error. Probes run with a minimal
// environment and their output is kept only for the error message.
func Canary(ctx context.Context, profileFile string, p Profile) error {
	if _, err := os.Stat(Exec); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if _, err := os.ReadDir(p.Data); err != nil {
		return fmt.Errorf("%w: the canary's denied folder: %v", ErrUnavailable, err)
	}
	probe := func(args ...string) (int, string, error) {
		var out bytes.Buffer
		spec, err := Wrap(runner.Spec{Dir: p.Temp, Args: args, Environ: []string{"PATH=/usr/bin:/bin"}, Timeout: canaryTimeout, Output: &out}, profileFile)
		if err != nil {
			return 0, "", err
		}
		result, err := runner.Run(ctx, spec)
		if err != nil {
			if ctx.Err() != nil {
				return 0, "", ctx.Err()
			}
			return 0, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if result.TimedOut {
			return 0, "", fmt.Errorf("%w: the canary's %s timed out", ErrUnavailable, args[0])
		}
		return result.ExitCode, strings.TrimSpace(out.String()), nil
	}
	if code, out, err := probe("/usr/bin/true"); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("%w: sandbox-exec exited %d (%s)", ErrUnavailable, code, out)
	}
	mark := filepath.Join(p.Temp, ".agentium-canary-"+p.Tag)
	code, out, err := probe("/usr/bin/touch", mark)
	if err != nil {
		return err
	}
	_, statErr := os.Lstat(mark)
	os.Remove(mark)
	if code != 0 || statErr != nil {
		return fmt.Errorf("%w: the canary cannot write the temp root (exit %d: %s)", ErrUnavailable, code, out)
	}
	if code, _, err := probe("/bin/ls", p.Data); err != nil {
		return err
	} else if code == 0 {
		return fmt.Errorf("%w: the canary can list the data folder %s", ErrUnavailable, p.Data)
	}
	outside := filepath.Join(p.Data, ".agentium-canary-"+p.Tag)
	if _, err := os.Lstat(outside); err == nil {
		return fmt.Errorf("%w: %s exists before the canary writes it", ErrUnavailable, outside)
	}
	code, _, err = probe("/usr/bin/touch", outside)
	_, statErr = os.Lstat(outside)
	if statErr == nil {
		os.Remove(outside)
	}
	if err != nil {
		return err
	}
	if code == 0 || statErr == nil {
		return fmt.Errorf("%w: the canary can write the data folder %s", ErrUnavailable, p.Data)
	}
	return nil
}
