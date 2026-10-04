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

// denialHead is the start of the kernel's message, after any "N duplicate report(s) for " prefix; denialSep is what
// follows the process name: "(<pid>) deny(<n>) <operation>", then a space and the target.
var (
	denialHead = regexp.MustCompile(`^(?:(\d+) duplicate reports? for )?Sandbox: `)
	denialSep  = regexp.MustCompile(`\((\d+)\) deny\(\d+\) ([A-Za-z0-9*_-]+)`)
)

// Unparsed is the operation of a denial whose message could be split more than one way (ParseDenials).
const Unparsed = "unparsed"

// maxLogBytes bounds what one `log show` of a grade may return: a grade chooses how many denials it causes, and how
// long their targets are.
const maxLogBytes = 16 << 20

// ParseDenials reads `log show --style ndjson` output: one JSON object per line, whose eventMessage is the kernel's
// denial followed by a newline and the tag. Only the kernel's own events count (processID 0): no process of the user's
// can log one. The message is matched only when its last line is tag, so a target holding a newline cannot forge
// another grade's denial; such a target is kept whole, newlines included. Lines that are not denials (the tool's
// summary, other messages) are skipped.
//
// The process name and the target are the grade's choice, so the message is read from the left: the
// "(<pid>) deny(<n>) <operation>" after the name splits it. When the message holds that text more than once (a chosen
// name or target forging a split), which split is the kernel's is unknown: the denial is kept as Unparsed, with no
// process ID (-1), so it can neither pass for Agentium's own probes nor hide as a limit the agent's sandbox shares
// (Flagged counts it).
func ParseDenials(out []byte, tag string) []Denial {
	var denials []Denial
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), maxLogBytes)
	for sc.Scan() {
		var event struct {
			Message   string `json:"eventMessage"`
			Timestamp string `json:"timestamp"`
			ProcessID *int   `json:"processID"`
		}
		if json.Unmarshal(sc.Bytes(), &event) != nil || event.ProcessID == nil || *event.ProcessID != 0 {
			continue
		}
		if d, ok := parseDenial(event.Message, tag); ok {
			d.At, _ = time.Parse("2006-01-02 15:04:05.000000-0700", event.Timestamp)
			denials = append(denials, d)
		}
	}
	return denials
}

// parseDenial reads one kernel message (see ParseDenials).
func parseDenial(message, tag string) (Denial, bool) {
	body, last, ok := cutLast(message, "\n")
	if !ok || last != tag {
		return Denial{}, false
	}
	head := denialHead.FindStringSubmatch(body)
	if head == nil {
		return Denial{}, false
	}
	rest := body[len(head[0]):]
	seps := denialSep.FindAllStringSubmatchIndex(rest, -1)
	if seps == nil {
		return Denial{}, false
	}
	repeats := 1
	if head[1] != "" {
		repeats, _ = strconv.Atoi(head[1])
	}
	if len(seps) > 1 { // the chosen name (or target) holds a split of its own: which one is the kernel's is unknown
		return Denial{Operation: Unparsed, Target: rest, PID: -1, Repeats: repeats}, true
	}
	m := seps[0]
	d := Denial{Process: rest[:m[0]], Operation: rest[m[4]:m[5]], Repeats: repeats}
	d.PID, _ = strconv.Atoi(rest[m[2]:m[3]])
	switch target := rest[m[1]:]; {
	case target == "":
	case target[0] == ' ':
		d.Target = target[1:]
	default:
		return Denial{}, false // the operation ran on into other text
	}
	return d, true
}

// cutLast splits s around the last sep.
func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// ErrDenialsUnread means a grade's denials could not be read from the unified log: the log tool is missing or failed,
// returned too much, or the end probe's denial never appeared.
var ErrDenialsUnread = errors.New("the grade's sandbox denials could not be read")

// showLog runs `log show --style ndjson` for tag's events from start on, reading at most maxLogBytes.
func showLog(ctx context.Context, start time.Time, tag string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, LogTool, "show", "--start", start.Format("2006-01-02 15:04:05"), "--style", "ndjson", "--predicate", LogPredicate(tag))
	var out capped
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if out.over {
			return nil, fmt.Errorf("%w: log show returned more than %d bytes", ErrDenialsUnread, maxLogBytes)
		}
		return nil, err
	}
	if out.over {
		return nil, fmt.Errorf("%w: log show returned more than %d bytes", ErrDenialsUnread, maxLogBytes)
	}
	return out.Bytes(), nil
}

// capped is a buffer that keeps at most maxLogBytes and then fails the write, which stops the writing command.
type capped struct {
	bytes.Buffer
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.Len()+len(p) > maxLogBytes {
		c.over = true
		return 0, errors.New("too much output")
	}
	return c.Buffer.Write(p)
}

// logSource reads the log's denials under a tag from a start time (showLog, then ParseDenials); tests replace it.
type logSource func(ctx context.Context, start time.Time, tag string) ([]Denial, error)

func systemLog(ctx context.Context, start time.Time, tag string) ([]Denial, error) {
	out, err := showLog(ctx, start, tag)
	if err != nil {
		return nil, err
	}
	return ParseDenials(out, tag), nil
}

// waitForProbe reads the log (source) under tag from start on, every 250 ms, until it holds the denial of the process
// probe (a process of Agentium's own, named name, whose one denial marks that everything before it has been logged),
// for at most wait, and returns everything read with it. The kernel reports denials asynchronously and late, so a
// read without the probe's denial may lack others too.
func waitForProbe(ctx context.Context, source logSource, start time.Time, tag string, probe int, name string, wait time.Duration) ([]Denial, error) {
	deadline := time.Now().Add(wait)
	for {
		all, err := source(ctx, start, tag)
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, ErrDenialsUnread):
			return nil, err
		case err != nil:
			return nil, fmt.Errorf("%w: log show: %v", ErrDenialsUnread, err)
		}
		if slices.ContainsFunc(all, func(d Denial) bool { return d.PID == probe && d.Process == name }) {
			return all, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: the probe's denial did not reach the unified log within %s", ErrDenialsUnread, wait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ReadDenials returns the denials the kernel logged under p's tag, once the log shows them all, except those of the
// processes in ignore (the canary's probes, CanaryProbes): the kernel reports denials late, and their log times are
// when it reported them, so only process IDs tell the canary's from the grade's. since is when the grade began (the
// canary's start): the log is read from a second before it. It first runs an end probe under the profile
// (profileFile, as Canary does) that reads profileFile itself, which the profile denies (it lies in the data folder)
// and which exists, so the kernel logs the denial (it logs none for a missing file); then it reads the log until the
// probe's denial appears (waitForProbe), up to wait. The probe's own denial is not returned. Errors wrap
// ErrDenialsUnread, except cancellation.
func ReadDenials(ctx context.Context, profileFile string, p Profile, since time.Time, wait time.Duration, ignore []int) ([]Denial, error) {
	if _, err := os.Stat(LogTool); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDenialsUnread, err)
	}
	probe, err := runProbe(ctx, profileFile, p.Temp, "/bin/test", "-r", profileFile)
	if err != nil {
		return nil, err
	}
	return collectDenials(ctx, systemLog, since, p.Tag, probe, "test", wait, ignore)
}

// collectDenials waits for the end probe's denial (waitForProbe), then returns every denial read but the probe's and
// those of the processes in ignore.
func collectDenials(ctx context.Context, source logSource, since time.Time, tag string, probe int, name string, wait time.Duration, ignore []int) ([]Denial, error) {
	all, err := waitForProbe(ctx, source, since.Add(-time.Second), tag, probe, name, wait)
	if err != nil {
		return nil, err
	}
	var denials []Denial
	for _, d := range all {
		if !(d.PID == probe && d.Process == name) && !slices.Contains(ignore, d.PID) {
			denials = append(denials, d)
		}
	}
	return denials, nil
}

// runProbe runs args under the profile in profileFile (sandbox-exec -f), from dir, and returns its process ID: a
// probe that a denial is expected of, so its exit code does not matter.
func runProbe(ctx context.Context, profileFile, dir string, args ...string) (int, error) {
	pid := 0
	spec, err := Wrap(runner.Spec{Dir: dir, Args: args, Environ: []string{"PATH=/usr/bin:/bin"}, Timeout: canaryTimeout, Output: &bytes.Buffer{},
		Started: func(p int) { pid = p }}, profileFile)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrDenialsUnread, err)
	}
	if _, err := runner.Run(ctx, spec); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("%w: the probe: %v", ErrDenialsUnread, err)
	}
	return pid, nil
}

// LogReadable checks that this account can read the kernel's sandbox denials from the unified log (`log show`; on some
// accounts or configurations it returns none): a probe under a profile of its own, which denies it one existing file,
// must have its denial logged within wait. Without it a grade's denials are never known, and decision 3 (a failed grade
// with denials the agent's sandbox does not impose is infrastructure) could not apply. Errors wrap ErrDenialsUnread,
// except cancellation.
func LogReadable(ctx context.Context, wait time.Duration) error {
	if _, err := os.Stat(LogTool); err != nil {
		return fmt.Errorf("%w: %v", ErrDenialsUnread, err)
	}
	dir, err := os.MkdirTemp("", "agentium-log-")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDenialsUnread, err)
	}
	defer os.RemoveAll(dir)
	tag, err := NewTag()
	if err != nil {
		return err
	}
	denied := filepath.Join(RealForm(dir), "denied")
	profileFile := filepath.Join(dir, "probe.sb")
	if err := os.WriteFile(denied, nil, 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrDenialsUnread, err)
	}
	profile := "(version 1)\n(allow default)\n(deny file-read* (literal " + quote(denied) + ") (with message " + quote(tag) + "))\n"
	if err := os.WriteFile(profileFile, []byte(profile), 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrDenialsUnread, err)
	}
	start := time.Now()
	probe, err := runProbe(ctx, profileFile, dir, "/bin/test", "-r", denied)
	if err != nil {
		return err
	}
	_, err = waitForProbe(ctx, systemLog, start.Add(-time.Second), tag, probe, "test", wait)
	return err
}

// Noise reports whether d is a denial that tools make in every grade and that changes no result, which Agentium ignores
// (the isolation plan's step 1): every process's /dev/dtracehelper, bash's /dev/tty, the JVM's hsperfdata files in the
// user's temp folder and its configd lookups, the mDNSResponder socket (name lookups, which fail offline anyway), and
// the analytics lookups of tools such as security, and the Go toolchain's telemetry counters
// (~/Library/Application Support/go/telemetry), which every go command writes, and the distributed-notifications and
// Spotlight-metadata lookups Cargo/rustc make (with the trade-offs noted below).
func (d Denial) Noise() bool {
	switch {
	case d.Operation == Unparsed: // its text is the grade's choice
		return false
	case d.Target == "/dev/dtracehelper", d.Target == "/dev/tty":
		return true
	case strings.Contains(d.Target, "/hsperfdata_"):
		return true
	case strings.Contains(d.Target, "mDNSResponder"):
		return true
	case strings.HasPrefix(d.Operation, "file-write") && strings.Contains(d.Target, "/Library/Application Support/go/telemetry/"):
		return true
	case d.Operation == "mach-lookup" && (strings.HasPrefix(d.Target, "com.apple.SystemConfiguration.") ||
		d.Target == "com.apple.analyticsd" || d.Target == "com.apple.diagnosticd" ||
		// The distributed-notifications and Spotlight-metadata daemons, which every real Cargo/rustc grade of the
		// Java/Rust pilot looked up (isolation step 4) and passed with the lookups denied. The profile still denies
		// them; noise only stops the flag. launchd names distributed notifications' per-user and system instances
		// (com.apple.distributed_notifications@Uv3, @0v3, @1v3) and Spotlight has a family (mds, mds.index,
		// mds.xpcs), so both match by prefix. Two trade-offs, both rare and accepted: Claude Code's agent sandbox
		// allows distributed notifications (machServices), so a failed grade blocked on that lookup, which decision 3
		// left out as infra-sandbox, now counts as the agent's failure (it matters only where the candidate makes a
		// lookup the reference does not); and noise is left out of DenialCount, so an attempt to post data through
		// distributed notifications is still denied but no longer visible in the records.
		strings.HasPrefix(d.Target, "com.apple.distributed_notifications") ||
		strings.HasPrefix(d.Target, "com.apple.metadata.mds")):
		return true
	}
	return false
}

// Flagged reports whether d is a limit the grading profile imposes but the agent's own sandbox (Claude Code's, from the
// run's settings) does not, so a grade that failed with it cannot be told from a sandbox failure (the isolation plan's
// decision 3: such a failed grade is infrastructure, left out with no retry). Noise is never flagged. The agent's sandbox
// imposes the same limits on:
//   - reads (the credential stores grading denies are denied to agents too: CredentialFiles and the build tools' user
//     caches, which a test holds together with graderCredentialFiles);
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
