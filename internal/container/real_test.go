package container

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// These tests need a real, local Docker daemon with the pinned Go image already present; they skip, with the reason,
// when there is none. They never pull. Every container and volume they make is named agentium-test<hex>-... and
// labelled agentium.data=test<hex>, and each test checks that nothing with its label is left.

// goImage is the golang:1.27 index digest the spike pulled (docs/research/2026-10-04-container-spike.md).
const goImage = "golang@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190"

// realDocker opens the user's docker daemon and finds the Go image, or skips.
func realDocker(t *testing.T) (*Docker, Image) {
	t.Helper()
	if testing.Short() {
		t.Skip("real-daemon test skipped in -short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker client on PATH")
	}
	ctx := context.Background()
	d, err := Open(ctx, Options{Environ: os.Environ()})
	if err != nil {
		t.Skipf("no usable local docker daemon: %v", err)
	}
	img, err := d.Image(ctx, goImage)
	if err != nil {
		t.Skipf("the pinned Go image is not usable here (never pulled by tests): %v", err)
	}
	return d, img
}

// testData is a fresh data ID for one test, "test" and 6 hex digits, so concurrent test runs never share leftovers.
// The cleanup fails the test if anything with its label is left, after removing it.
func testData(t *testing.T, d *Docker) string {
	t.Helper()
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	data := "test" + hex.EncodeToString(b)
	t.Cleanup(func() {
		ctx := context.Background()
		left, err := d.Leftovers(ctx, data)
		if err != nil {
			t.Errorf("list leftovers: %v", err)
			return
		}
		if len(left) > 0 {
			t.Errorf("left behind: %+v", left)
			runs := map[string]bool{}
			for _, l := range left {
				runs[l.Run] = true
			}
			for run := range runs {
				if err := d.RemoveRun(ctx, data, run); err != nil {
					t.Errorf("remove leftovers of %s: %v", run, err)
				}
			}
		}
		// Also by name and label straight from docker, independent of Leftovers' own filters.
		for _, args := range [][]string{
			{"ps", "--all", "--quiet", "--filter", "name=agentium-" + data},
			{"ps", "--all", "--quiet", "--filter", "label=" + LabelData + "=" + data},
			{"volume", "ls", "--quiet", "--filter", "label=" + LabelData + "=" + data},
		} {
			out, err := d.output(ctx, args...)
			if err != nil || strings.TrimSpace(string(out)) != "" {
				t.Errorf("docker %v: %q, %v", args, out, err)
			}
		}
	})
	return data
}

func realSpec(data, run string, img Image, d *Docker) Spec {
	return Spec{Data: data, Run: run, Role: "grade", Image: img, Limits: DefaultLimits(d.Engine().NCPU), Deadline: 10 * time.Minute}
}

// goEnv is a Go grade's environment inside the container: caches under /grade/cache, offline.
var goEnv = []string{"HOME=/grade/cache/home", "GOCACHE=/grade/cache/go-build", "GOPATH=/grade/cache/gopath", "GOPROXY=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod"}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// treeDigest is a digest of every path, mode, link target and content under root, to show the host copy unchanged.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	must(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s %v %d\n", p, info.Mode(), info.ModTime().UnixNano())
		if info.Mode()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			fmt.Fprintln(h, target)
		} else if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(data)
		}
		return nil
	}))
	return hex.EncodeToString(h.Sum(nil))
}

// TestRealGoTestPasses: the whole shape on a real daemon. The container is checked and probed, the grading copy goes
// in as a tar stream, and a Go test (with an httptest server on the container's own loopback) runs and passes, offline,
// as the grade's user. A link the tree holds to a host file points nowhere inside, and the host copy is unchanged.
func TestRealGoTestPasses(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	secret := filepath.Join(t.TempDir(), "host-secret")
	must(t, os.WriteFile(secret, []byte("host only"), 0o600))
	tree := t.TempDir()
	writeTree(t, tree, map[string]string{
		"go.mod":         "module example.com/m\n\ngo 1.27\n",
		"sub/m.go":       "package sub\n\nfunc Add(a, b int) int { return a + b }\n",
		"sub/m_test.go":  "package sub\n\nimport (\n\t\"io\"\n\t\"net/http\"\n\t\"net/http/httptest\"\n\t\"testing\"\n)\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"add\")\n\t}\n\tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, \"pong\") }))\n\tdefer srv.Close()\n\tresp, err := http.Get(srv.URL)\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\tresp.Body.Close()\n}\n",
		"scripts/run.sh": "#!/bin/sh\necho script-ran\n",
	})
	must(t, os.Chmod(filepath.Join(tree, "scripts/run.sh"), 0o755))
	must(t, os.Symlink(secret, filepath.Join(tree, "leak")))
	before := treeDigest(t, tree)
	var out strings.Builder
	var digest string
	err := d.Run(context.Background(), realSpec(data, "gotest", img, d), func(ctx context.Context, c *Container) error {
		digest = c.InspectDigest()
		stats, err := c.CopyIn(ctx, tree, DefaultCopyLimit)
		if err != nil {
			return err
		}
		if stats.Entries != 7 {
			return fmt.Errorf("copied %d entries, want 7 (2 folders, 4 files, 1 link)", stats.Entries)
		}
		for _, cmd := range []Command{
			{Command: "go test -count=1 -v ./...", Env: goEnv},
			{Command: "./run.sh", Dir: "scripts"},
			{Command: `test -L leak && ! cat leak && [ "$(readlink leak)" = "` + secret + `" ]`},
			{Command: "id -u; ls -ld /grade/work /grade/cache"},
		} {
			res, err := c.Exec(ctx, Command{Command: cmd.Command, Dir: cmd.Dir, Env: cmd.Env, Timeout: 5 * time.Minute, Output: &out})
			if err != nil {
				return err
			}
			if res.ExitCode != 0 || res.Counters.Hit() {
				return fmt.Errorf("%q: %+v", cmd.Command, res)
			}
		}
		return nil
	})
	t.Logf("output:\n%s", out.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--- PASS: TestAdd", "ok  \texample.com/m/sub", "script-ran", "65534", "nobody nogroup"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(out.String(), "host only") {
		t.Error("the host file behind a link reached the container")
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("inspect digest %q", digest)
	}
	if treeDigest(t, tree) != before {
		t.Error("the host copy changed")
	}
}

// TestRealLimitsShowInCounters: an exec'd process killed by the OOM killer, and forks refused at the process limit,
// both show in the counters read after the command, and the container keeps running.
func TestRealLimitsShowInCounters(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	spec := realSpec(data, "limits", img, d)
	spec.Limits = Limits{Memory: 128 << 20, Pids: 64, CPUs: 1, Tmp: 16 << 20, Shm: 16 << 20}
	err := d.Run(context.Background(), spec, func(ctx context.Context, c *Container) error {
		res, err := c.Exec(ctx, Command{Command: "tail /dev/zero", Timeout: time.Minute})
		if err != nil {
			return err
		}
		if res.ExitCode != 137 || res.Counters.OOMKills < 1 || !res.Counters.Hit() {
			return fmt.Errorf("after an OOM: %+v", res)
		}
		var out strings.Builder
		res, err = c.Exec(ctx, Command{Command: `i=0; while [ $i -lt 100 ]; do sleep 2 & i=$((i+1)); done; wait; true`, Timeout: time.Minute, Output: &out})
		if err != nil {
			return err
		}
		if res.Counters.PidsMax < 1 {
			return fmt.Errorf("after forks past the limit: %+v\n%s", res, out.String())
		}
		t.Logf("counters: %+v", res.Counters)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRealSideChannelsClosed: what sandbox-v1 leaves open is closed (isolation decisions 7 and 8). Grade A leaves a
// /mp- POSIX semaphore, a POSIX shm object, SysV shared memory and a message queue, and a listener on 0.0.0.0; A
// itself sees them all (the positive control). Grade B, running beside A, and grade C, after A, see none of them and
// cannot connect to A's listener; there is no /dev/log, and every connect out fails.
func TestRealSideChannelsClosed(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	marker := "agentiumprobe" + data
	const port = "47123"
	leave := `set -e
python3 -c 'import _multiprocessing; s = _multiprocessing.SemLock(1, 1, 1, "/mp-` + marker + `", False); print("sem left")'
echo hidden > /dev/shm/` + marker + `
ipcmk -M 4096 && ipcmk -Q
nohup python3 -c 'import socket, time
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("0.0.0.0", ` + port + `)); s.listen(); time.sleep(600)' > /dev/null 2>&1 &
sleep 1`
	control := `ls /dev/shm; ipcs -m | grep -c '^0x'; ipcs -q | grep -c '^0x'
python3 -c 'import socket; socket.create_connection(("127.0.0.1", ` + port + `), 2).close(); print("control connect ok")'`
	// look prints what another grade finds: it must find nothing.
	look := `ls -A /dev/shm | grep -c ` + marker + ` || true
echo "shm $(ipcs -m | grep -c '^0x')"; echo "msg $(ipcs -q | grep -c '^0x')"
if [ -e /dev/log ]; then echo "devlog present"; else echo "devlog absent"; fi
logger --socket-errors=on probe 2>/dev/null && echo "logger ok" || echo "logger failed"
python3 - <<'EOF'
import socket
for host, port in [("127.0.0.1", ` + port + `), ("::1", ` + port + `), ("192.0.2.1", 80), ("192.168.5.2", 80), ("10.0.2.2", 80), ("1.1.1.1", 443)]:
    try:
        socket.create_connection((host, port), 2).close()
        print("connect", host, "ok")
    except OSError as e:
        print("connect", host, "failed", e.errno)
try:
    socket.getaddrinfo("example.com", 443)
    print("dns ok")
except OSError:
    print("dns failed")
EOF`
	var aOut, bOut, cOut strings.Builder
	ctx := context.Background()
	err := d.Run(ctx, realSpec(data, "sidea", img, d), func(ctx context.Context, a *Container) error {
		for _, cmd := range []string{leave, control} {
			res, err := a.Exec(ctx, Command{Command: cmd, Timeout: time.Minute, Output: &aOut})
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("grade A: %q exited %d", cmd, res.ExitCode)
			}
		}
		return d.Run(ctx, realSpec(data, "sideb", img, d), func(ctx context.Context, b *Container) error {
			_, err := b.Exec(ctx, Command{Command: look, Timeout: time.Minute, Output: &bOut})
			return err
		})
	})
	if err != nil {
		t.Fatalf("%v\nA:\n%s\nB:\n%s", err, aOut.String(), bOut.String())
	}
	err = d.Run(ctx, realSpec(data, "sidec", img, d), func(ctx context.Context, c *Container) error {
		_, err := c.Exec(ctx, Command{Command: look, Timeout: time.Minute, Output: &cOut})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("A:\n%s\nB:\n%s\nC:\n%s", aOut.String(), bOut.String(), cOut.String())
	for _, want := range []string{"sem left", "sem.mp-" + marker, "\n" + marker + "\n", "control connect ok"} {
		if !strings.Contains(aOut.String(), want) {
			t.Errorf("the positive control in A lacks %q", want)
		}
	}
	if !strings.Contains(aOut.String(), "\n1\n1\n") {
		t.Error("A does not see its own SysV shm and message queue")
	}
	for name, out := range map[string]string{"B (concurrent)": bOut.String(), "C (later)": cOut.String()} {
		for _, want := range []string{"0\nshm 0\nmsg 0\n", "devlog absent", "logger failed", "connect 127.0.0.1 failed 111", "connect ::1 failed 111",
			"connect 192.0.2.1 failed 101", "connect 192.168.5.2 failed 101", "connect 10.0.2.2 failed 101", "connect 1.1.1.1 failed 101", "dns failed"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
}

// TestRealCancelLeavesNothing: a cancel during a command kills the container (the docker client's death alone would
// leave the command running), and nothing of the grade is left.
func TestRealCancelLeavesNothing(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	err := d.Run(ctx, realSpec(data, "cancel", img, d), func(ctx context.Context, c *Container) error {
		time.AfterFunc(time.Second, cancel)
		_, err := c.Exec(ctx, Command{Command: "sleep 300"})
		if _, again := c.Exec(context.Background(), Command{Command: "true"}); !errors.Is(again, ErrGone) {
			t.Errorf("an exec after the cancel: %v, want ErrGone", again)
		}
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after a cancel: %v", err)
	}
	if took := time.Since(started); took > time.Minute {
		t.Errorf("the cancel took %s", took)
	}
	assertNothingLeft(t, d, data)
}

// TestRealTimeoutAndDeadline: a command's timeout kills the container, and so does the deadline (the main process
// ends and --rm removes it) when nothing else does; either way nothing is left.
func TestRealTimeoutAndDeadline(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	err := d.Run(context.Background(), realSpec(data, "timeout", img, d), func(ctx context.Context, c *Container) error {
		res, err := c.Exec(ctx, Command{Command: "sleep 300", Timeout: 2 * time.Second})
		if err != nil {
			return err
		}
		if !res.TimedOut || res.ExitCode != -1 {
			return fmt.Errorf("after a timeout: %+v", res)
		}
		if _, err := c.Exec(ctx, Command{Command: "true"}); !errors.Is(err, ErrGone) {
			return fmt.Errorf("an exec after the timeout: %v, want ErrGone", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNothingLeft(t, d, data)
	spec := realSpec(data, "deadline", img, d)
	spec.Deadline = 3 * time.Second
	err = d.Run(context.Background(), spec, func(ctx context.Context, c *Container) error {
		_, err := c.Exec(ctx, Command{Command: "sleep 300"})
		return err
	})
	if err == nil {
		t.Fatal("a command past the deadline counted as a result")
	}
	t.Logf("past the deadline: %v", err)
	assertNothingLeft(t, d, data)
}

// TestRealCreatedNotStartedFoundByLabel: a container created but never started (Agentium died between create and
// start) outlives --rm and the deadline; its labels find it and its volume, and RemoveRun removes both.
func TestRealCreatedNotStartedFoundByLabel(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	spec := realSpec(data, "orphan", img, d)
	if _, err := d.output(context.Background(), createArgs(spec)...); err != nil {
		t.Fatal(err)
	}
	left, err := d.Leftovers(context.Background(), data)
	must(t, err)
	if len(left) != 2 || left[0] != (Leftover{Kind: "container", Name: Name(spec), Run: "orphan", State: "created"}) || left[1].Kind != "volume" || left[1].Run != "orphan" {
		t.Fatalf("leftovers: %+v", left)
	}
	must(t, d.RemoveRun(context.Background(), data, "orphan"))
	assertNothingLeft(t, d, data)
}

func assertNothingLeft(t *testing.T, d *Docker, data string) {
	t.Helper()
	left, err := d.Leftovers(context.Background(), data)
	must(t, err)
	if len(left) > 0 {
		t.Errorf("left behind: %+v", left)
	}
}

// TestRealFixtures checks the daemon's real record and the probes' real output against the checks, and with -update
// rewrites testdata/inspect.json and testdata/probe.txt from them (normalized: the test's data ID becomes "test", and
// the container's and volume's IDs and the image's layer paths become placeholders), which the unit tests mutate.
func TestRealFixtures(t *testing.T) {
	d, img := realDocker(t)
	if img.ID != fixtureImageID || d.Engine().NCPU < 4 {
		t.Skip("the fixtures are for the pinned Go image on a daemon with at least 4 CPUs")
	}
	data := testData(t, d)
	spec := fixtureSpec()
	spec.Data = data
	ctx := context.Background()
	c := &Container{d: d, spec: spec, name: Name(spec), mayExist: true}
	defer func() { must(t, c.kill(ctx)) }()
	_, err := d.output(ctx, createArgs(spec)...)
	must(t, err)
	raw, err := d.output(ctx, "container", "inspect", c.name)
	must(t, err)
	if _, _, err := checkInspect(raw, spec); err != nil {
		t.Fatal(err)
	}
	var skeleton bytes.Buffer
	must(t, skeletonTar(&skeleton))
	must(t, c.check(d.call(ctx, []string{"cp", "-", c.name + ":" + GradeDir}, &skeleton, controlTimeout)))
	_, err = d.output(ctx, "start", c.name)
	must(t, err)
	probes, err := d.output(ctx, c.execArgs(false, nil, "", "sh", "-c", probeScript)...)
	must(t, err)
	if err := checkProbes(string(probes), false); err != nil {
		t.Fatal(err)
	}
	if !*update {
		return
	}
	var rec []struct {
		ID     string `json:"Id"`
		Mounts []struct{ Name string }
	}
	must(t, json.Unmarshal(raw, &rec))
	normalize := func(s string) string {
		s = strings.ReplaceAll(s, rec[0].ID, strings.Repeat("c", 64))
		s = strings.ReplaceAll(s, rec[0].ID[:12], strings.Repeat("c", 12)) // the hostname
		s = strings.ReplaceAll(s, rec[0].Mounts[0].Name, strings.Repeat("v", 64))
		s = layerPath.ReplaceAllString(s, "/var/lib/docker/overlay2/<layer>")
		s = trailingSpace.ReplaceAllString(s, "") // the kernel pads the routing table's header
		return strings.ReplaceAll(s, data, "test")
	}
	must(t, os.WriteFile(filepath.Join("testdata", "inspect.json"), []byte(normalize(string(raw))), 0o644))
	must(t, os.WriteFile(filepath.Join("testdata", "probe.txt"), []byte(normalize(string(probes))), 0o644))
}

var (
	layerPath     = regexp.MustCompile(`/var/lib/docker/overlay2/[A-Za-z0-9/]+`)
	trailingSpace = regexp.MustCompile(`(?m)[ \t]+$`)
)
