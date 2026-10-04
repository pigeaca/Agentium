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
	// GraderContainer runs them in a throwaway container (the containers plan). Until that plan's step 4 lands it is
	// named but not graded in: KnownGrader and ParseGrader refuse it as an unknown mode, and no site may read it as
	// the sandbox or the host. Every site that tells modes apart names the mode it means.
	GraderContainer = "container-v1"
)

// GradesOn names the modes this Agentium grades in, for the refusal of any other (UnknownGrader and callers that
// add context): one text, so the places that refuse cannot drift. Step 4 of the containers plan extends it.
const GradesOn = "on the host or in " + GraderSandbox

// UnknownGrader is the refusal of a mode this Agentium does not grade in (container-v1 included, until the containers
// plan's step 4): the same words at every entry point.
func UnknownGrader(mode string) error {
	return fmt.Errorf("grader %s: this Agentium grades %s", mode, GradesOn)
}

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
	switch m := GraderOf(mode); m {
	case GraderHost:
		return "on the host"
	case GraderSandbox:
		return "in the sandbox (" + m + ")"
	case GraderContainer:
		return "in a container (" + m + ")"
	default:
		return "in an unknown grader mode (" + m + ")"
	}
}
