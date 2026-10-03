package task

import (
	"fmt"

	"github.com/pigeaca/agentium/internal/sandbox"
)

// Grader modes: where grading and validation run the verification commands (the isolation plan, Part 1). Runs,
// validations and experiment locks record theirs; an empty mode, in what was recorded before modes existed, is host.
const (
	// GraderHost runs them on the host, unsandboxed, as Agentium always did.
	GraderHost = "host"
	// GraderSandbox runs them under Agentium's grading sandbox (internal/sandbox), named by its profile's version: a
	// new version is a new mode, so what was validated under the old one is validated again.
	GraderSandbox = sandbox.Version
)

// GraderOf is a recorded mode as it counts: empty is host.
func GraderOf(mode string) string {
	if mode == "" {
		return GraderHost
	}
	return mode
}

// DefaultGrader is the mode new experiments, runs and validations get without --grader: the sandbox on macOS (goos
// "darwin"), whose sandbox-exec it needs, and the host elsewhere (the isolation plan's decision 1).
func DefaultGrader(goos string) string {
	if goos == "darwin" {
		return GraderSandbox
	}
	return GraderHost
}

// ParseGrader reads --grader: "host" or "sandbox" (the current sandbox version, which it may also name).
func ParseGrader(flag string) (string, error) {
	switch flag {
	case "host":
		return GraderHost, nil
	case "sandbox", GraderSandbox:
		return GraderSandbox, nil
	}
	return "", fmt.Errorf("--grader %q: use host or sandbox", flag)
}

// KnownGrader reports whether this Agentium grades in mode (as GraderOf reads it): host, or the current sandbox
// version. A lock or validation under another version (an older or newer profile) is not comparable with grades now.
func KnownGrader(mode string) bool {
	m := GraderOf(mode)
	return m == GraderHost || m == GraderSandbox
}

// DescribeGrader names a mode for people: "on the host" or "in the sandbox (sandbox-v1)".
func DescribeGrader(mode string) string {
	if m := GraderOf(mode); m != GraderHost {
		return "in the sandbox (" + m + ")"
	}
	return "on the host"
}
