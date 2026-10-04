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
	"slices"
	"strings"
	"syscall"
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
	t.Cleanup(func() { must(t, d.Close()) })
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
		stats, err := c.CopyIn(ctx, tree, DefaultCopyLimits())
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

// TestRealProxyConfigNeverReachesTheGrade: the docker client copies its configuration's proxies, credentials included,
// into every container it creates. The user's configuration (here one with a fake proxy password) is only read to find
// the endpoint: the create runs with an empty one, so the grade's environment is the image's own.
func TestRealProxyConfigNeverReachesTheGrade(t *testing.T) {
	real, img := realDocker(t)
	data := testData(t, real)
	userConfig := t.TempDir()
	must(t, os.WriteFile(filepath.Join(userConfig, "config.json"),
		[]byte(`{"proxies":{"default":{"httpProxy":"http://user:FAKEPASS@proxy.example:3128","httpsProxy":"http://user:FAKEPASS@proxy.example:3128","noProxy":"*.example"}}}`), 0o600))
	d, err := Open(context.Background(), Options{Environ: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "DOCKER_CONFIG=" + userConfig, "DOCKER_HOST=" + real.host}})
	must(t, err)
	defer func() { must(t, d.Close()) }()
	var out strings.Builder
	err = d.Run(context.Background(), realSpec(data, "proxy", img, d), func(ctx context.Context, c *Container) error {
		if !slices.Equal(c.ImageEnv(), img.Env) {
			return fmt.Errorf("the container's environment %v is not the image's %v", c.ImageEnv(), img.Env)
		}
		_, err := c.Exec(ctx, Command{Command: "env; cat /proc/1/environ | tr '\\0' '\\n'", Timeout: time.Minute, Output: &out})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"FAKEPASS", "proxy.example", "PROXY", "proxy="} {
		if strings.Contains(out.String(), leak) {
			t.Errorf("the grade sees %q:\n%s", leak, out.String())
		}
	}
	if !strings.Contains(out.String(), "GOLANG_VERSION=1.27.1") {
		t.Errorf("the environment was not read:\n%s", out.String())
	}
	// The positive control: a create with the user's configuration does carry the password.
	control := "agentium-" + data + "-proxycontrol"
	docker := func(args ...string) string {
		cmd := exec.Command("docker", append([]string{"--config", userConfig, "--host", real.host}, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("docker %v: %v", args[0], err)
		}
		return string(out)
	}
	docker("create", "--pull", "never", "--name", control, "--label", LabelData+"="+data, "--label", LabelRun+"=proxycontrol", "--label", LabelMode+"="+Mode, img.Ref, "true")
	env := docker("container", "inspect", "--format", "{{json .Config.Env}}", control)
	docker("rm", "--force", "--volumes", control)
	if !strings.Contains(env, "FAKEPASS") {
		t.Errorf("the control did not get the proxies, so the test proves nothing: %s", env)
	}
}

// TestRealInspectBackstopsTheConfig: were the client's configuration ever to carry proxies into a create (here the
// empty folder Open accepted gains a config.json afterwards), the inspect check refuses the container on the daemon's
// own record, naming Config.Env. It also shows that every call uses that folder: without --config, the create would
// get no proxies and this test would fail. The user's own ~/.docker is never touched.
func TestRealInspectBackstopsTheConfig(t *testing.T) {
	real, img := realDocker(t)
	data := testData(t, real)
	config := t.TempDir()
	d, err := Open(context.Background(), Options{Environ: os.Environ(), ConfigDir: config})
	must(t, err)
	defer func() { must(t, d.Close()) }()
	must(t, os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"proxies":{"default":{"httpProxy":"http://user:FAKEPASS@proxy.example:3128"}}}`), 0o600))
	ran := false
	err = d.Run(context.Background(), realSpec(data, "backstop", img, d), func(context.Context, *Container) error { ran = true; return nil })
	if !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), "Config.Env") || strings.Contains(err.Error(), "FAKEPASS") || ran {
		t.Fatalf("a create with proxies: %v (fn ran: %v), want ErrMismatch naming Config.Env, without the password", err, ran)
	}
	t.Logf("refused: %.200s", err)
}

// TestRealGradeCannotStopTheDeadline: the main process (the init and the sleep that holds the deadline) runs as
// MainUser, so the grade's code, as User, can neither stop the sleep (which would outlive the deadline) nor end it or
// the init (which would end its own container mid-command, turning a failure into infrastructure). The deadline then
// still removes the container.
func TestRealGradeCannotStopTheDeadline(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	spec := realSpec(data, "deadline", img, d)
	spec.Deadline = 8 * time.Second
	const attack = `s=""
for p in /proc/[0-9]*; do if [ "$(cat "$p/comm" 2>/dev/null)" = sleep ]; then s=${p#/proc/}; fi; done
[ -n "$s" ] || { echo "no sleep found"; exit 2; }
echo "sleep runs as $(awk '/^Uid:/ {print $2}' /proc/$s/status 2>/dev/null || grep '^Uid:' /proc/$s/status)"
for sig in STOP TERM KILL; do if kill -$sig "$s" 2>/dev/null; then echo "kill -$sig sleep succeeded"; else echo "kill -$sig sleep denied"; fi; done
for sig in STOP TERM KILL; do if kill -$sig 1 2>/dev/null; then echo "kill -$sig 1 succeeded"; else echo "kill -$sig 1 denied"; fi; done
grep '^State:' /proc/$s/status`
	var out strings.Builder
	started := time.Now()
	var tail error
	err := d.Run(context.Background(), spec, func(ctx context.Context, c *Container) error {
		res, err := c.Exec(ctx, Command{Command: attack, Timeout: time.Minute, Output: &out})
		if err != nil || res.ExitCode != 0 {
			return fmt.Errorf("the attack: %+v, %v\n%s", res, err, out.String())
		}
		_, tail = c.Exec(ctx, Command{Command: "sleep 120"})
		return nil
	})
	t.Logf("as the grade's user:\n%s\npast the deadline: %v", out.String(), tail)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "denied"); n != 6 || strings.Contains(out.String(), "succeeded") || !strings.Contains(out.String(), "State:\tS (sleeping)") {
		t.Errorf("%d of 6 signals denied, or the sleep is not sleeping", n)
	}
	if !errors.Is(tail, ErrUnjudgeable) {
		t.Errorf("a command cut by the deadline: %v, want ErrUnjudgeable", tail)
	}
	if took := time.Since(started); took > 60*time.Second {
		t.Errorf("the deadline (8 s) took %s", took)
	}
	assertNothingLeft(t, d, data)
}

// TestRealForkBombIsUnjudgeable: a fork bomb left running after a failing command holds every process slot, so the
// counters cannot be read: the result is ErrUnjudgeable (left out, never retried), and the container is removed.
func TestRealForkBombIsUnjudgeable(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	spec := realSpec(data, "forkbomb", img, d)
	spec.Limits.Pids = 64
	const bomb = `nohup python3 -c '
import os, time
while True:
    try:
        if os.fork() == 0:
            time.sleep(600)
            os._exit(0)
    except OSError:
        time.sleep(0.001)
' > /dev/null 2>&1 &
while read n < /sys/fs/cgroup/pids.current && [ "$n" -lt 64 ]; do :; done
exit 1`
	var execErr error
	err := d.Run(context.Background(), spec, func(ctx context.Context, c *Container) error {
		_, execErr = c.Exec(ctx, Command{Command: bomb, Timeout: time.Minute})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(execErr, ErrUnjudgeable) {
		t.Errorf("after a fork bomb: %v, want ErrUnjudgeable", execErr)
	}
	t.Logf("after a fork bomb: %v", execErr)
	assertNothingLeft(t, d, data)
}

// TestRealGradeCannotKillTheCounters: a killer the grade leaves running kills every process it can, over and over.
// The counters' read runs as MainUser, so it survives, and the failing command's result is judged (with its counters),
// not lost.
func TestRealGradeCannotKillTheCounters(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	const killer = `nohup sh -c 'read me rest < /proc/self/stat
while :; do for p in /proc/[0-9]*; do q=${p#/proc/}; [ "$q" = "$me" ] || kill -9 "$q" 2> /dev/null; done; done' > /dev/null 2>&1 &
sleep 1
exit 1`
	err := d.Run(context.Background(), realSpec(data, "killer", img, d), func(ctx context.Context, c *Container) error {
		res, err := c.Exec(ctx, Command{Command: killer, Timeout: time.Minute})
		if err != nil {
			return fmt.Errorf("the counters did not survive the killer: %w", err)
		}
		if res.ExitCode == 0 {
			return fmt.Errorf("the failing command passed: %+v", res)
		}
		time.Sleep(time.Second)
		if _, err := c.Counters(ctx); err != nil {
			return fmt.Errorf("a later read: %w", err)
		}
		t.Logf("judged: exit %d, counters %+v", res.ExitCode, res.Counters)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNothingLeft(t, d, data)
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
	if _, _, err := checkInspect(raw, spec, d.Engine().DefaultRuntime); err != nil {
		t.Fatal(err)
	}
	var skeleton bytes.Buffer
	must(t, skeletonTar(&skeleton))
	must(t, c.check(d.call(ctx, []string{"cp", "-", c.name + ":" + GradeDir}, &skeleton, controlTimeout)))
	_, err = d.output(ctx, "start", c.name)
	must(t, err)
	probes, err := d.output(ctx, c.execArgs(User, false, nil, "/", "sh", "-c", probeScript)...)
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

// TestRealClientFailureIsUnjudgeable: a docker client that fails after the command ran (here a wrapper around the real
// client that reports an error, or exits 1 silently, once the real exec is done) is told from the command's own exit:
// the daemon's record of the exec disagrees, or the client spoke, so the result is ErrUnjudgeable and the container is
// removed. A genuine exit 1, with output on stderr (joined to stdout inside the container), is judged as exit 1.
func TestRealClientFailureIsUnjudgeable(t *testing.T) {
	_, img := realDocker(t)
	real, err := exec.LookPath("docker")
	must(t, err)
	wrapper := filepath.Join(t.TempDir(), "docker")
	must(t, os.WriteFile(wrapper, []byte(`#!/bin/sh
for a in "$@"; do
  case "$a" in
  *'#client-fails'*) '`+real+`' "$@" > /dev/null 2>&1; echo 'error during connect: simulated' >&2; exit 1 ;;
  *'#client-silent'*) '`+real+`' "$@" > /dev/null 2>&1; exit 1 ;;
  esac
done
exec '`+real+`' "$@"
`), 0o755))
	d, err := Open(context.Background(), Options{Bin: wrapper, Environ: os.Environ()})
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	data := testData(t, d)
	var out strings.Builder
	var judged Result
	var failed error
	err = d.Run(context.Background(), realSpec(data, "client", img, d), func(ctx context.Context, c *Container) error {
		var err error
		if judged, err = c.Exec(ctx, Command{Command: "echo to-stderr >&2; exit 1", Timeout: time.Minute, Output: &out}); err != nil {
			return fmt.Errorf("a genuine exit 1: %w", err)
		}
		_, failed = c.Exec(ctx, Command{Command: "true #client-fails", Timeout: time.Minute})
		return nil
	})
	must(t, err)
	if judged.ExitCode != 1 || !strings.Contains(out.String(), "to-stderr") {
		t.Errorf("a genuine exit 1: %+v, output %q", judged, out.String())
	}
	if !errors.Is(failed, ErrUnjudgeable) {
		t.Errorf("a client that failed after a passing command: %v, want ErrUnjudgeable", failed)
	}
	t.Logf("client failed: %v", failed)
	err = d.Run(context.Background(), realSpec(data, "silent", img, d), func(ctx context.Context, c *Container) error {
		_, err := c.Exec(ctx, Command{Command: "exit 3 #client-silent", Timeout: time.Minute})
		return err
	})
	if !errors.Is(err, ErrUnjudgeable) {
		t.Errorf("a client that exited 1 where the command exited 3: %v, want ErrUnjudgeable", err)
	}
	t.Logf("client silent: %v", err)
	assertNothingLeft(t, d, data)
}

// TestRealCopyInFolderSwappedForPipe: a folder swapped for a pipe between its header and its listing is refused at
// once (the open never blocks), as ErrUnjudgeable, and nothing is left.
func TestRealCopyInFolderSwappedForPipe(t *testing.T) {
	d, img := realDocker(t)
	data := testData(t, d)
	tree := t.TempDir()
	writeTree(t, tree, map[string]string{"a/inner.go": "package a\n", "b.go": "package b\n"})
	d.hooks.openDir = func(name string) {
		if name == "a" {
			must(t, os.Rename(filepath.Join(tree, "a"), filepath.Join(tree, "moved")))
			must(t, syscall.Mkfifo(filepath.Join(tree, "a"), 0o600))
		}
	}
	t.Cleanup(func() { d.hooks = walkHooks{} })
	start := time.Now()
	err := d.Run(context.Background(), realSpec(data, "swap", img, d), func(ctx context.Context, c *Container) error {
		_, err := c.CopyIn(ctx, tree, CopyLimits{Bytes: 1 << 20, Entries: 100, Timeout: 30 * time.Second})
		return err
	})
	if !errors.Is(err, ErrUnjudgeable) {
		t.Fatalf("%v, want ErrUnjudgeable", err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("the copy-in took %s", took)
	}
	t.Logf("swapped: %v", err)
	assertNothingLeft(t, d, data)
}
