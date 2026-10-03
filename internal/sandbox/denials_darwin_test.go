package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A grade's denials are read from the unified log by its tag, once its end mark is there: what its commands were
// denied (a credential read, the security server's lookup), never the canary's or the end probe's own, and nothing
// another grade was denied.
func TestReadDenials(t *testing.T) {
	needSandbox(t)
	needLog(t)
	g := newGrade(t, nil)
	other := newGrade(t, nil)
	since := time.Now()
	probes, err := CanaryProbes(context.Background(), g.file, g.digest, g.profile)
	if err != nil || len(probes) != 4 {
		t.Fatalf("the canary's probes %v: %v", probes, err)
	}
	g.sh("cat " + strconv.Quote(g.path("home/.ssh/id_test")) + " >/dev/null 2>&1; /usr/bin/security find-generic-password -s agentium-made-up >/dev/null 2>&1")
	other.sh("cat " + strconv.Quote(other.path("home/.aws/credentials")) + " >/dev/null 2>&1")
	began := time.Now()
	denials, err := ReadDenials(context.Background(), g.file, g.profile, since, 20*time.Second, probes)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read in %s: %v", time.Since(began), denials)
	var read, lookup bool
	for _, d := range denials {
		switch {
		case d.Operation == "file-read-data" && d.Target == RealForm(g.path("home/.ssh/id_test")):
			read = true
			if g.profile.Flagged(d) {
				t.Errorf("a credential read the agent's sandbox denies too is flagged: %v", d)
			}
		case d.Operation == "mach-lookup" && d.Target == "com.apple.SecurityServer":
			lookup = true
			if !g.profile.Flagged(d) {
				t.Errorf("the security server's lookup, which the agent's sandbox allows, is not flagged: %v", d)
			}
		case d.Target == RealForm(g.file), d.Target == RealForm(g.profile.Data):
			t.Errorf("the end probe's or the canary's own denial: %v", d)
		case d.Target == RealForm(other.path("home/.aws/credentials")):
			t.Errorf("another grade's denial: %v", d)
		}
	}
	if !read || !lookup {
		t.Errorf("denials %v: credential read %v, security server %v", denials, read, lookup)
	}
}

// Usable passes where sandbox-exec applies a profile, as it does for these tests, and fails nested inside another
// sandbox (Agentium started from an agent's shell): a grade there would be infrastructure after the agent spent.
func TestUsable(t *testing.T) {
	needSandbox(t)
	needLog(t)
	if err := Usable(context.Background()); err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(Exec, "-p", "(version 1)(allow default)", bin, "-")
	cmd.Env = []string{"PATH=/usr/bin:/bin", helperVar + "=usable"}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(string(out), "inside another sandbox") {
		t.Errorf("nested: %v %s", err, out)
	}
}

// needLog skips unless this account's unified log shows the kernel's sandbox denials (LogReadable): not every account
// or CI runner does, and there Usable refuses sandbox mode, so Usable refuses sandbox mode there.
func needLog(t *testing.T) {
	t.Helper()
	if err := LogReadable(context.Background(), 15*time.Second); err != nil {
		t.Skipf("the unified log does not show sandbox denials here: %v", err)
	}
}
