package sandbox

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
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
	} {
		if !d.Noise() {
			t.Errorf("not noise: %+v", d)
		}
	}
	for _, d := range []Denial{
		{Operation: "mach-lookup", Target: "com.apple.SecurityServer"},
		{Operation: "file-read-data", Target: "/Users/u/.ssh/id_ed25519"},
		{Operation: "file-write-data", Target: "/dev/ttys001"},
		{Operation: "network-outbound", Target: "1.1.1.1:443"},
	} {
		if d.Noise() {
			t.Errorf("noise: %+v", d)
		}
	}
}

// Flagged are the limits the grading profile imposes and the agent's own sandbox does not (decision 3): reads of the
// grader's own credential stores, Mach lookups, POSIX IPC, IOKit, Unix sockets and anything unknown. Reads of what
// agents are denied too, writes, the network to addresses, signals, process information and sysctl reads are not.
func TestDenialFlagged(t *testing.T) {
	home := t.TempDir()
	p := Profile{Home: home, Data: "/data"}
	flagged := []Denial{
		{Operation: "file-read-data", Target: filepath.Join(home, ".m2/settings.xml")},
		{Operation: "file-read-metadata", Target: filepath.Join(RealForm(home), ".config/pip/pip.conf")},
		{Operation: "file-read-data", Target: filepath.Join(home, ".cargo/credentials.toml")},
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
		{Operation: "file-read-metadata", Target: filepath.Join(home, ".m2")}, // the folder above the grader's own store
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
	}
	for _, d := range shared {
		if p.Flagged(d) {
			t.Errorf("flagged: %+v", d)
		}
	}
}
