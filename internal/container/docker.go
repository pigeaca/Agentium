package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// Errors the usability check returns; each makes the mode unusable, and none is ever answered by grading another way.
var (
	// ErrUnavailable: no docker client, or no daemon answers.
	ErrUnavailable = errors.New("docker is not available")
	// ErrRemote: the daemon is not on this machine (its endpoint is not a Unix socket).
	ErrRemote = errors.New("the docker daemon is not local")
	// ErrUnsupported: the daemon is not Linux, lacks seccomp or cgroup v2, or its API is too old.
	ErrUnsupported = errors.New("the docker daemon cannot isolate a grade")
	// ErrTooSmall: the daemon has less memory or fewer CPUs than one grade's limits.
	ErrTooSmall = errors.New("the docker daemon is too small for a grade's limits")
	// ErrImageMissing: the image is not present by its digest. Agentium never pulls on its own.
	ErrImageMissing = errors.New("image not present")
)

// MinAPIVersion is the oldest daemon API the driver accepts: Docker 20.10's, the first with cgroup v2 support and
// create --pull. The shape was settled on 1.47 (engine 27.4).
const MinAPIVersion = "1.41"

// clientEnvNames is the docker client's environment allowlist: nothing else from Agentium's environment reaches it.
// DOCKER_HOST and DOCKER_CONTEXT are only used to find the endpoint; later calls pin it with --host instead.
var clientEnvNames = []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"}

// ClientEnv is environ filtered to what the docker client may see (credentials are dropped whatever their name).
func ClientEnv(environ []string) []string {
	out := runner.EnvPolicy{Allowlist: true, Names: clientEnvNames}.Filter(environ)
	if out == nil {
		out = []string{}
	}
	return out
}

// Options says how to reach docker.
type Options struct {
	Bin     string   // the docker client; "docker" (looked up in PATH) when empty
	Environ []string // Agentium's environment; ClientEnv filters it
}

// Docker is a client pinned to one local daemon. Open makes it; its zero value is not usable.
type Docker struct {
	bin     string
	environ []string // ClientEnv, without DOCKER_HOST and DOCKER_CONTEXT: --host pins the endpoint
	host    string   // the endpoint, unix://...; never recorded or printed (it holds a home path)
	engine  Engine
}

// Engine is what a record keeps about the daemon: never its endpoint, name or paths.
type Engine struct {
	Version       string `json:"version"`
	APIVersion    string `json:"api_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	CgroupVersion string `json:"cgroup_version"`
	NCPU          int    `json:"ncpu"`
	MemTotal      int64  `json:"mem_total"`
	Rootless      bool   `json:"rootless,omitempty"`
}

// controlTimeout bounds every docker call that is not a grade's command, so a hung daemon cannot hang a grade.
const controlTimeout = 2 * time.Minute

// Open finds the daemon the user's docker client would use, refuses it unless it is local, and checks that it can
// isolate a grade: Linux, seccomp, cgroup v2 and an API at least MinAPIVersion. Every later call goes to the same
// endpoint (--host), whatever the user's context becomes meanwhile.
func Open(ctx context.Context, opts Options) (*Docker, error) {
	bin := opts.Bin
	if bin == "" {
		bin = "docker"
	}
	env := ClientEnv(opts.Environ)
	d := &Docker{bin: bin, environ: env}
	out, err := d.output(ctx, "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}")
	if err != nil {
		return nil, fmt.Errorf("%w: find the endpoint: %v", ErrUnavailable, err)
	}
	var host string
	if err := json.Unmarshal(bytes.TrimSpace(out), &host); err != nil {
		return nil, fmt.Errorf("%w: read the endpoint: %v", ErrUnavailable, err)
	}
	if err := CheckLocal(host); err != nil {
		return nil, err
	}
	d.host = host
	d.environ = dropNames(env, "DOCKER_HOST", "DOCKER_CONTEXT")
	if d.engine, err = d.readEngine(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// CheckLocal refuses an endpoint that is not a Unix socket: tcp://, ssh:// and every other scheme can reach another
// machine, and the hidden tests would go with the grade. The endpoint itself is never named in the error.
func CheckLocal(endpoint string) error {
	path, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok || path == "" || !strings.HasPrefix(path, "/") {
		scheme, _, found := strings.Cut(endpoint, "://")
		if !found {
			scheme = "none"
		}
		return fmt.Errorf("%w: its endpoint is %s://, and only a Unix socket on this machine is accepted (decision 9)", ErrRemote, scheme)
	}
	return nil
}

func dropNames(env []string, names ...string) []string {
	out := []string{}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !containsString(names, name) {
			out = append(out, kv)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// readEngine reads `docker version` and `docker info` and refuses a daemon that cannot isolate a grade.
func (d *Docker) readEngine(ctx context.Context) (Engine, error) {
	out, err := d.output(ctx, "version", "--format", "{{json .Server}}")
	if err != nil {
		return Engine{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var v struct {
		Version    string
		APIVersion string `json:"ApiVersion"`
		Os         string
		Arch       string
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &v); err != nil || v.Version == "" {
		return Engine{}, fmt.Errorf("%w: no server version", ErrUnavailable)
	}
	out, err = d.output(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return Engine{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var info struct {
		ServerErrors    []string
		ServerVersion   string
		OSType          string
		NCPU            int
		MemTotal        int64
		CgroupVersion   string
		SecurityOptions []string
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &info); err != nil {
		return Engine{}, fmt.Errorf("%w: read docker info: %v", ErrUnavailable, err)
	}
	if len(info.ServerErrors) > 0 || info.ServerVersion == "" {
		return Engine{}, fmt.Errorf("%w: docker info has no server", ErrUnavailable)
	}
	e := Engine{Version: v.Version, APIVersion: v.APIVersion, OS: v.Os, Arch: v.Arch, CgroupVersion: info.CgroupVersion, NCPU: info.NCPU, MemTotal: info.MemTotal}
	seccomp := false
	for _, opt := range info.SecurityOptions {
		fields := securityFields(opt)
		switch fields["name"] {
		case "seccomp":
			seccomp = fields["profile"] != "unconfined"
		case "rootless":
			e.Rootless = true
		}
	}
	switch {
	case e.OS != "linux" || info.OSType != "linux":
		return e, fmt.Errorf("%w: it runs %q, not Linux", ErrUnsupported, e.OS)
	case !seccomp:
		return e, fmt.Errorf("%w: seccomp is off", ErrUnsupported)
	case e.CgroupVersion != "2":
		return e, fmt.Errorf("%w: cgroup v%s, not v2 (the counters a grade is judged by need v2)", ErrUnsupported, e.CgroupVersion)
	case !apiAtLeast(e.APIVersion, MinAPIVersion):
		return e, fmt.Errorf("%w: API %q is older than %s", ErrUnsupported, e.APIVersion, MinAPIVersion)
	}
	return e, nil
}

// securityFields splits one of docker info's SecurityOptions ("name=seccomp,profile=builtin") into its fields.
func securityFields(opt string) map[string]string {
	fields := map[string]string{}
	for _, part := range strings.Split(opt, ",") {
		k, v, _ := strings.Cut(part, "=")
		fields[k] = v
	}
	return fields
}

// apiAtLeast compares two "major.minor" API versions; an unreadable one is too old.
func apiAtLeast(have, want string) bool {
	hm, hn, ok1 := majorMinor(have)
	wm, wn, ok2 := majorMinor(want)
	if !ok1 || !ok2 {
		return false
	}
	return hm > wm || hm == wm && hn >= wn
}

func majorMinor(v string) (int, int, bool) {
	a, b, ok := strings.Cut(v, ".")
	if !ok {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(a)
	minor, err2 := strconv.Atoi(b)
	return major, minor, err1 == nil && err2 == nil
}

// Engine is what the daemon reported when it was opened.
func (d *Docker) Engine() Engine { return d.engine }

// Fits refuses limits the daemon cannot give one grade: less memory than the limit, or fewer CPUs (open decision 8).
// Several concurrent grades can still exceed the daemon together; the caller warns about that.
func (d *Docker) Fits(l Limits) error {
	if err := l.validate(); err != nil {
		return err
	}
	if d.engine.MemTotal < l.Memory {
		return fmt.Errorf("%w: it has %d MiB of memory and a grade's limit is %d MiB", ErrTooSmall, d.engine.MemTotal>>20, l.Memory>>20)
	}
	if d.engine.NCPU < l.CPUs {
		return fmt.Errorf("%w: it has %d CPUs and a grade's limit is %d", ErrTooSmall, d.engine.NCPU, l.CPUs)
	}
	return nil
}

// Image is an image found by its pinned reference.
type Image struct {
	Ref  string   // the pinned reference: <repository>@sha256:<64 hex>, or an image ID, sha256:<64 hex>
	ID   string   // the image ID (sha256:...), which the inspect check compares with the container's
	Env  []string // the image's environment (Config.Env), which commands inherit; PATH among it
	Arch string
}

var (
	digestRef = regexp.MustCompile(`^[a-z0-9]+(?:[._/-][a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*@sha256:[0-9a-f]{64}$`)
	imageID   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Image finds ref on the daemon. ref must be pinned (a digest reference or an image ID), never a tag, so the daemon
// cannot substitute another image. A missing image is ErrImageMissing: Image never pulls, and neither does Run
// (create --pull never). The image must be Linux on the daemon's architecture.
func (d *Docker) Image(ctx context.Context, ref string) (Image, error) {
	if !digestRef.MatchString(ref) && !imageID.MatchString(ref) {
		return Image{}, fmt.Errorf("image %q: not pinned by digest", ref)
	}
	out, stderr, res, err := d.call(ctx, []string{"image", "inspect", "--format", "{{json .}}", ref}, nil, controlTimeout)
	if err != nil {
		return Image{}, fmt.Errorf("image %q: %w", ref, err)
	}
	if res.ExitCode != 0 {
		if strings.Contains(stderr, "No such image") {
			return Image{}, fmt.Errorf("%w: %s (pull it with consent first; Agentium never pulls on its own)", ErrImageMissing, ref)
		}
		return Image{}, fmt.Errorf("image %q: %w: %s", ref, ErrUnavailable, firstLine(stderr))
	}
	var img struct {
		ID           string `json:"Id"`
		RepoDigests  []string
		Os           string
		Architecture string
		Config       struct{ Env []string }
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &img); err != nil {
		return Image{}, fmt.Errorf("image %q: read its record: %w", ref, err)
	}
	switch {
	case imageID.MatchString(ref) && img.ID != ref:
		return Image{}, fmt.Errorf("image %q: the daemon answered with %q", ref, img.ID)
	case digestRef.MatchString(ref) && !containsString(img.RepoDigests, ref):
		return Image{}, fmt.Errorf("%w: %s (the daemon's image of that name has another digest)", ErrImageMissing, ref)
	case !imageID.MatchString(img.ID):
		return Image{}, fmt.Errorf("image %q: unreadable ID %q", ref, img.ID)
	case img.Os != "linux" || img.Architecture != d.engine.Arch:
		return Image{}, fmt.Errorf("image %q is %s/%s, and the daemon runs %s/%s", ref, img.Os, img.Architecture, d.engine.OS, d.engine.Arch)
	}
	return Image{Ref: ref, ID: img.ID, Env: img.Config.Env, Arch: img.Architecture}, nil
}

// Usable is the whole usability check before a command uses the mode: Open, Fits and every image present by digest.
func Usable(ctx context.Context, opts Options, limits Limits, refs ...string) (*Docker, []Image, error) {
	d, err := Open(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	if err := d.Fits(limits); err != nil {
		return nil, nil, err
	}
	images := make([]Image, 0, len(refs))
	for _, ref := range refs {
		img, err := d.Image(ctx, ref)
		if err != nil {
			return nil, nil, err
		}
		images = append(images, img)
	}
	return d, images, nil
}

// args is a docker call's full argv after the binary: the pinned endpoint first, once Open has found it.
func (d *Docker) args(args []string) []string {
	if d.host == "" {
		return args
	}
	return append([]string{"--host", d.host}, args...)
}

// call runs docker with args (argv only, never a shell) and returns its stdout and stderr; a non-zero exit is in the
// result, not an error. stderr has the endpoint replaced, since it holds a home path.
func (d *Docker) call(ctx context.Context, args []string, stdin io.Reader, timeout time.Duration) ([]byte, string, runner.Result, error) {
	var stdout bytes.Buffer
	stderr := &capped{max: 64 << 10}
	res, err := runner.Run(ctx, runner.Spec{Args: append([]string{d.bin}, d.args(args)...), Environ: d.environ, Timeout: timeout,
		Output: &stdout, Stderr: stderr, Stdin: stdin})
	errText := d.redact(stderr.String())
	if err != nil {
		return nil, errText, res, fmt.Errorf("docker %s: %w", args[0], err)
	}
	if res.TimedOut {
		return nil, errText, res, fmt.Errorf("docker %s: timed out after %s", args[0], timeout)
	}
	return stdout.Bytes(), errText, res, nil
}

// output is call for a command that must succeed.
func (d *Docker) output(ctx context.Context, args ...string) ([]byte, error) {
	out, stderr, res, err := d.call(ctx, args, nil, controlTimeout)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker %s: exit %d: %s", args[0], res.ExitCode, firstLine(stderr))
	}
	return out, nil
}

func (d *Docker) redact(s string) string {
	if d.host != "" {
		s = strings.ReplaceAll(s, d.host, "<docker endpoint>")
		s = strings.ReplaceAll(s, strings.TrimPrefix(d.host, "unix://"), "<docker endpoint>")
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "no message"
	}
	return s
}

// capped keeps the first max bytes written to it and drops the rest, so a chatty failure cannot grow memory.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }
