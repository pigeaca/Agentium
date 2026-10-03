package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// LogTool is macOS's log command, by absolute path (zsh has a log builtin, and PATH is never trusted).
const LogTool = "/usr/bin/log"

// Denial is one denial the kernel logged under a grade's tag: "Sandbox: <process>(<pid>) deny(1) <operation> <target>",
// then the tag. The kernel merges repeats ("N duplicate reports for ...") and limits the rate, so Repeats is a lower
// bound. Process and Target are what the grade chose (a process name, a path, a service, an address): records keep
// them, but nothing shared (notes, reports) should print them, since a hostile build could encode what it read there.
type Denial struct {
	Process   string    `json:"process"`
	PID       int       `json:"pid"`
	Operation string    `json:"operation"` // file-read-data, file-write-create, mach-lookup, network-outbound, ...
	Target    string    `json:"target,omitempty"`
	Repeats   int       `json:"repeats"`
	At        time.Time `json:"at"`
}

// String is the denial as one line: "<process> <operation> <target>".
func (d Denial) String() string {
	return strings.TrimSpace(d.Process + " " + d.Operation + " " + d.Target)
}

// denialLine matches the kernel's message, after any "N duplicate report(s) for " prefix.
var denialLine = regexp.MustCompile(`^(?:(\d+) duplicate reports? for )?Sandbox: (.*)\((\d+)\) deny\(\d+\) (\S+)(?: (.*))?$`)

// ParseDenials reads `log show --style ndjson` output: one JSON object per line, whose eventMessage is the kernel's
// denial followed by a newline and the tag. Lines that are not denials (the tool's summary, other messages) are
// skipped. A message is matched only when its last line is tag, so a target holding a newline cannot forge another
// grade's denial.
func ParseDenials(out []byte, tag string) []Denial {
	var denials []Denial
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var event struct {
			Message   string `json:"eventMessage"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &event) != nil {
			continue
		}
		body, last, ok := cutLast(event.Message, "\n")
		if !ok || last != tag {
			continue
		}
		m := denialLine.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		d := Denial{Process: m[2], Operation: m[4], Target: m[5], Repeats: 1}
		d.PID, _ = strconv.Atoi(m[3])
		if m[1] != "" {
			d.Repeats, _ = strconv.Atoi(m[1])
		}
		d.At, _ = time.Parse("2006-01-02 15:04:05.000000-0700", event.Timestamp)
		denials = append(denials, d)
	}
	return denials
}

// cutLast splits s around the last sep.
func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// ErrDenialsUnread means a grade's denials could not be read from the unified log: the log tool is missing or failed,
// or the grade's end mark never appeared. A failed grade whose denials are unknown cannot be told from a sandbox
// failure.
var ErrDenialsUnread = errors.New("the grade's sandbox denials could not be read")

// ReadDenials returns the denials the kernel logged under p's tag, once the log shows them all, except those of the
// processes in ignore (the canary's probes, CanaryProbes): the kernel reports denials late, and their log times are
// when it reported them, so only process IDs tell the canary's from the grade's. since is when the grade began (the
// canary's start): the log is read from a second before it. It first runs an end probe under the profile (profileFile, as
// Canary does) that reads profileFile itself, which the profile denies (it lies in the data folder) and which exists,
// so the kernel logs the denial (it logs none for a missing file); then it reads the log until the probe's denial
// appears, told apart by its process ID, waiting up to wait (the kernel reports denials asynchronously). The probe's
// own denial is not returned. Errors wrap ErrDenialsUnread, except cancellation.
func ReadDenials(ctx context.Context, profileFile string, p Profile, since time.Time, wait time.Duration, ignore []int) ([]Denial, error) {
	if _, err := os.Stat(LogTool); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDenialsUnread, err)
	}
	probe := 0
	spec, err := Wrap(runner.Spec{Dir: p.Temp, Args: []string{"/bin/test", "-r", profileFile}, Environ: []string{"PATH=/usr/bin:/bin"},
		Timeout: canaryTimeout, Output: &bytes.Buffer{}, Started: func(pid int) { probe = pid }}, profileFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDenialsUnread, err)
	}
	if _, err := runner.Run(ctx, spec); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: the end probe: %v", ErrDenialsUnread, err)
	}
	start := since.Add(-time.Second).Format("2006-01-02 15:04:05") // whole seconds
	deadline := time.Now().Add(wait)
	for {
		out, err := exec.CommandContext(ctx, LogTool, "show", "--start", start, "--style", "ndjson", "--predicate", LogPredicate(p.Tag)).Output()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w: log show: %v", ErrDenialsUnread, err)
		}
		all := ParseDenials(out, p.Tag)
		if slices.ContainsFunc(all, func(d Denial) bool { return d.PID == probe }) {
			var denials []Denial
			for _, d := range all {
				if d.PID != probe && !slices.Contains(ignore, d.PID) {
					denials = append(denials, d)
				}
			}
			return denials, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: the end probe's denial did not reach the log within %s", ErrDenialsUnread, wait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Noise reports whether d is a denial that tools make in every grade and that changes no result, which Agentium ignores
// (the isolation plan's step 1): every process's /dev/dtracehelper, bash's /dev/tty, the JVM's hsperfdata files in the
// user's temp folder and its configd lookups, the mDNSResponder socket (name lookups, which fail offline anyway), and
// the analytics lookups of tools such as security, and the Go toolchain's telemetry counters
// (~/Library/Application Support/go/telemetry), which every go command writes.
func (d Denial) Noise() bool {
	switch {
	case d.Target == "/dev/dtracehelper", d.Target == "/dev/tty":
		return true
	case strings.Contains(d.Target, "/hsperfdata_"):
		return true
	case strings.Contains(d.Target, "mDNSResponder"):
		return true
	case strings.HasPrefix(d.Operation, "file-write") && strings.Contains(d.Target, "/Library/Application Support/go/telemetry/"):
		return true
	case d.Operation == "mach-lookup" && (strings.HasPrefix(d.Target, "com.apple.SystemConfiguration.") ||
		d.Target == "com.apple.analyticsd" || d.Target == "com.apple.diagnosticd"):
		return true
	}
	return false
}

// Flagged reports whether d is a limit the grading profile imposes but the agent's own sandbox (Claude Code's, from the
// run's settings) does not, so a grade that failed with it cannot be told from a sandbox failure (the isolation plan's
// decision 3: such a failed grade is infrastructure, retried or left out). Noise is never flagged. The agent's sandbox
// imposes the same limits on:
//   - reads, except the grader's own credential stores (graderCredentialFiles), which agents may still read;
//   - writes (file-write*, hard links): the agent writes only its checkout, build cache and temp root;
//   - the network to addresses (an agent has no direct network; local binding is the grade's wider allowance);
//   - signals, process information and sysctl reads (the same rules).
//
// Everything else is flagged: Mach lookups (the agent's sandbox allows the security server, launch services, fonts and
// more), POSIX shared memory and other semaphores, IOKit, Unix sockets, ioctls, and any operation not named here.
func (p Profile) Flagged(d Denial) bool {
	if d.Noise() {
		return false
	}
	op := d.Operation
	switch {
	case strings.HasPrefix(op, "file-read"):
		for _, path := range p.graderOnly() {
			if within(filepath.Clean(d.Target), path) {
				return true
			}
		}
		return false
	case strings.HasPrefix(op, "file-write"), op == "file-link":
		return false
	case strings.HasPrefix(op, "network"):
		return strings.Contains(d.Target, "/") // a Unix socket's path; an address has none
	case strings.HasPrefix(op, "signal"), strings.HasPrefix(op, "process-info"), strings.HasPrefix(op, "sysctl"):
		return false
	}
	return true
}

// graderOnly are the grader's own credential stores, in every form: denied to grades, not yet to agents.
func (p Profile) graderOnly() []string {
	var paths []string
	for _, name := range graderCredentialFiles() {
		paths = append(paths, filepath.Join(p.Home, name))
	}
	return WithForms(paths)
}
