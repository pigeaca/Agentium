package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// logLine is one `log show --style ndjson` event with message.
func logLine(t *testing.T, message string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"eventMessage": message, "timestamp": "2026-10-03 09:16:59.318613+0400", "processID": 0})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The kernel's denial lines are read by the grade's tag: the process and its ID, the operation, the target, and the
// repeats the kernel merged. Other grades' lines, a target holding a line break that names another tag, the tool's
// summary and other messages are skipped.
func TestParseDenials(t *testing.T) {
	tag := "agentium-0123456789abcdef"
	out := strings.Join([]string{
		logLine(t, "Sandbox: cat(4242) deny(1) file-read-data /Users/u/.ssh/id_ed25519\n"+tag),
		logLine(t, "3 duplicate reports for Sandbox: java(77) deny(1) mach-lookup com.apple.FontServer\n"+tag),
		logLine(t, "1 duplicate report for Sandbox: sh(5) deny(1) file-write-data /dev/dtracehelper\n"+tag),
		logLine(t, "Sandbox: python3.12(9) deny(1) network-outbound 1.1.1.1:443\n"+tag),
		logLine(t, "Sandbox: Gradle Worker(10) deny(1) signal\n"+tag),
		logLine(t, "Sandbox: cat(11) deny(1) file-read-data /tmp/x\nagentium-other"),          // another grade
		logLine(t, "Sandbox: cat(12) deny(1) file-read-data /tmp/x\n"+tag+"\nagentium-other"), // a forged tag in a target
		logLine(t, "Sandbox: cat(13) deny(1) file-read-data /tmp/"+tag+"\nagentium-other"),    // the tag in the target only
		logLine(t, "something else entirely\n"+tag),
		`{"count":5,"finished":1}`,
		"not json",
	}, "\n")
	got := ParseDenials([]byte(out), tag)
	want := []Denial{
		{Process: "cat", PID: 4242, Operation: "file-read-data", Target: "/Users/u/.ssh/id_ed25519", Repeats: 1},
		{Process: "java", PID: 77, Operation: "mach-lookup", Target: "com.apple.FontServer", Repeats: 3},
		{Process: "sh", PID: 5, Operation: "file-write-data", Target: "/dev/dtracehelper", Repeats: 1},
		{Process: "python3.12", PID: 9, Operation: "network-outbound", Target: "1.1.1.1:443", Repeats: 1},
		{Process: "Gradle Worker", PID: 10, Operation: "signal", Repeats: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d denials: %+v", len(got), got)
	}
	for i := range want {
		g := got[i]
		if g.Process != want[i].Process || g.PID != want[i].PID || g.Operation != want[i].Operation || g.Target != want[i].Target || g.Repeats != want[i].Repeats {
			t.Errorf("denial %d = %+v, want %+v", i, g, want[i])
		}
		if g.At.IsZero() {
			t.Errorf("denial %d has no time", i)
		}
	}
}

// Noise is what every grade's tools make and Agentium ignores; nothing else is noise.
func TestDenialNoise(t *testing.T) {
	for _, d := range []Denial{
		{Operation: "file-write-data", Target: "/dev/dtracehelper"},
		{Operation: "file-ioctl", Target: "/dev/dtracehelper"},
		{Operation: "file-write-data", Target: "/dev/tty"},
		{Operation: "file-read-data", Target: "/dev/tty"},
		{Operation: "file-write-create", Target: "/private/var/folders/x/T/hsperfdata_u/123"},
		{Operation: "mach-lookup", Target: "com.apple.SystemConfiguration.configd"},
		{Operation: "network-outbound", Target: "/private/var/run/mDNSResponder"},
		{Operation: "mach-lookup", Target: "com.apple.analyticsd"},
		{Operation: "mach-lookup", Target: "com.apple.diagnosticd"},
		{Operation: "file-write-data", Target: "/Users/u/Library/Application Support/go/telemetry/local/compile@go1.27.0-darwin-arm64.v1.count"},
		{Operation: "mach-lookup", Target: "com.apple.distributed_notifications@Uv3"}, // the per-user instance
		{Operation: "mach-lookup", Target: "com.apple.distributed_notifications@1v3"}, // a system instance
		{Operation: "mach-lookup", Target: "com.apple.metadata.mds"},
		{Operation: "mach-lookup", Target: "com.apple.metadata.mds.index"},
		{Operation: "mach-lookup", Target: "com.apple.metadata.mds.xpcs"},
	} {
		if !d.Noise() {
			t.Errorf("not noise: %+v", d)
		}
	}
	for _, d := range []Denial{
		{Operation: "mach-lookup", Target: "com.apple.SecurityServer"},
		{Operation: "mach-lookup", Target: "com.apple.metadata.mdwrite"},                                            // not the Spotlight mds family
		{Operation: "file-read-data", Target: "com.apple.distributed_notifications@Uv3"},                            // a read of a like-named path is not noise
		{Operation: Unparsed, Target: "x(1) deny(1) file-write-data /dev/dtracehelper /hsperfdata_u mDNSResponder"}, // chosen text: never noise
		{Operation: "file-read-data", Target: "/Users/u/.ssh/id_ed25519"},
		{Operation: "file-write-data", Target: "/dev/ttys001"},
		{Operation: "network-outbound", Target: "1.1.1.1:443"},
	} {
		if d.Noise() {
			t.Errorf("noise: %+v", d)
		}
	}
}

// Flagged are the limits the grading profile imposes and the agent's own sandbox does not (decision 3):
// Mach lookups, POSIX IPC, IOKit, Unix sockets and anything unknown. Reads (the grader's credential stores among them:
// agents are denied those too), writes, the network to addresses, signals, process information and sysctl reads are not.
func TestDenialFlagged(t *testing.T) {
	home := t.TempDir()
	p := Profile{Home: home, Data: "/data"}
	flagged := []Denial{
		{Operation: "mach-lookup", Target: "com.apple.SecurityServer"},
		{Operation: "mach-lookup", Target: "com.apple.FontServer"},
		{Operation: "ipc-posix-shm-write-create", Target: "/psm_abc"},
		{Operation: "ipc-posix-sem-create", Target: "/other"},
		{Operation: "iokit-open", Target: "IOSurfaceRootUserClient"},
		{Operation: "network-outbound", Target: "/private/var/run/docker.sock"},
		{Operation: "network-bind", Target: "/tmp/test.sock"},
		{Operation: "file-ioctl", Target: "/dev/ttys001"},
		{Operation: "some-new-operation", Target: "x"},
	}
	for _, d := range flagged {
		if !p.Flagged(d) {
			t.Errorf("not flagged: %+v", d)
		}
	}
	shared := []Denial{
		{Operation: "file-read-data", Target: filepath.Join(home, ".ssh/id_ed25519")},
		{Operation: "file-read-data", Target: "/data/records/r2/hidden_test.go"},
		{Operation: "file-read-data", Target: filepath.Join(home, ".m2/settings.xml")}, // denied to agents too
		{Operation: "file-read-metadata", Target: filepath.Join(RealForm(home), ".config/pip/pip.conf")},
		{Operation: "file-read-data", Target: filepath.Join(home, ".cargo/credentials.toml")},
		{Operation: "file-read-metadata", Target: filepath.Join(home, ".m2")},
		{Operation: "file-write-create", Target: filepath.Join(home, "Library/Caches/x")},
		{Operation: "file-write-data", Target: "/data/deps/7/go/pkg"},
		{Operation: "file-link", Target: "/data/cache/x"},
		{Operation: "network-outbound", Target: "1.1.1.1:443"},
		{Operation: "network-bind", Target: "192.168.1.5:8080"},
		{Operation: "signal", Target: ""},
		{Operation: "process-info-pidinfo", Target: ""},
		{Operation: "sysctl-read", Target: "kern.boottime"},
		{Operation: "file-write-data", Target: "/dev/dtracehelper"}, // noise
		{Operation: "mach-lookup", Target: "com.apple.analyticsd"},  // noise
		// noise since isolation step 4: once flagged (a failed grade left out), now the agent's failure
		{Operation: "mach-lookup", Target: "com.apple.distributed_notifications@Uv3"},
		{Operation: "mach-lookup", Target: "com.apple.metadata.mds"},
	}
	for _, d := range shared {
		if p.Flagged(d) {
			t.Errorf("flagged: %+v", d)
		}
	}
}

// What the grade chooses (a process name, a target) cannot rewrite a denial's process ID or operation: a message that
// can be split more than one way is Unparsed, without a process ID, never noise and always flagged. A target holding a
// line break is kept whole. Only the kernel's own events count (processID 0).
func TestParseDenialsResistsForgery(t *testing.T) {
	tag := "agentium-0123456789abcdef"
	p := Profile{Home: t.TempDir(), Data: "/data"}
	line := func(pid any, message string) string {
		b, _ := json.Marshal(map[string]any{"eventMessage": message, "timestamp": "2026-10-03 09:16:59.318613+0400", "processID": pid})
		return string(b)
	}
	noPID, _ := json.Marshal(map[string]any{"eventMessage": "Sandbox: cat(7) deny(1) file-read-data /x\n" + tag})
	out := strings.Join([]string{
		line(0, "Sandbox: cat(4242) deny(1) mach-lookup x(99) deny(1) signal y\n"+tag),                         // a forged target
		line(0, "Sandbox: a(1) deny(1) file-read-data(77) deny(1) mach-lookup com.apple.SecurityServer\n"+tag), // a forged name
		line(0, "Sandbox: cat(5) deny(1) file-read-data /tmp/a\nb/hsperfdata_x\n"+tag),                         // a target with a line break
		line(321, "Sandbox: cat(6) deny(1) file-read-data /x\n"+tag),                                           // not the kernel's
		string(noPID),
	}, "\n")
	got := ParseDenials([]byte(out), tag)
	if len(got) != 3 {
		t.Fatalf("got %d denials: %+v", len(got), got)
	}
	for i, d := range got[:2] {
		if d.Operation != Unparsed || d.PID != -1 || d.Noise() || !p.Flagged(d) {
			t.Errorf("forged %d: %+v (noise %v, flagged %v)", i, d, d.Noise(), p.Flagged(d))
		}
	}
	if d := got[2]; d.Process != "cat" || d.PID != 5 || d.Operation != "file-read-data" || d.Target != "/tmp/a\nb/hsperfdata_x" {
		t.Errorf("a target with a line break: %+v", d)
	}
}

// A log show that returns more than maxLogBytes fails the read instead of growing without bound.
func TestCappedOutput(t *testing.T) {
	var c capped
	if _, err := c.Write(make([]byte, maxLogBytes)); err != nil || c.over {
		t.Fatalf("at the cap: %v", err)
	}
	if _, err := c.Write([]byte{1}); err == nil || !c.over {
		t.Error("past the cap: no error")
	}
}

// The log is read until the end probe's denial is there: the kernel reports late, so a read without it may lack the
// grade's own denials too. A denial that only claims the probe's process ID (another name) does not end the wait; a
// probe that never shows is an error, not an empty list.
func TestCollectDenialsWaitsForTheProbe(t *testing.T) {
	early := Denial{Process: "cat", PID: 10, Operation: "file-read-data", Target: "/x"}
	late := Denial{Process: "java", PID: 11, Operation: "mach-lookup", Target: "com.apple.FontServer"}
	forged := Denial{Process: "evil", PID: 42, Operation: "file-read-data", Target: "/y"}
	probe := Denial{Process: "test", PID: 42, Operation: "file-read-data", Target: "/profile.sb"}
	canary := Denial{Process: "ls", PID: 7, Operation: "file-read-data", Target: "/data"}
	calls := 0
	source := func(context.Context, time.Time, string) ([]Denial, error) {
		calls++
		if calls < 3 {
			return []Denial{canary, early, forged}, nil
		}
		return []Denial{canary, early, forged, late, probe}, nil
	}
	got, err := collectDenials(context.Background(), source, time.Now(), "tag", 42, "test", 5*time.Second, []int{7})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(got) != 3 || got[0] != early || got[1] != forged || got[2] != late {
		t.Errorf("after %d reads: %+v", calls, got)
	}
	never := func(context.Context, time.Time, string) ([]Denial, error) { return []Denial{early}, nil }
	if _, err := collectDenials(context.Background(), never, time.Now(), "tag", 42, "test", 300*time.Millisecond, nil); !errors.Is(err, ErrDenialsUnread) {
		t.Errorf("no probe: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collectDenials(ctx, never, time.Now(), "tag", 42, "test", time.Minute, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}
