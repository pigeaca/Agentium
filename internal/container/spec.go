package container

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// Mode is the grader mode whose containers this package makes: task.GraderContainer (a test keeps them equal; the
// package does not import task, which grading will import it from).
const Mode = "container-v1"

// The labels on every container and anonymous volume Run creates. Recovery and clean find leftovers by them.
const (
	LabelData = "agentium.data" // the data folder's ID (DataID)
	LabelRun  = "agentium.run"  // the run's ID
	LabelMode = "agentium.mode" // Mode
)

// User is the grade's user and group: nobody, which every image's /etc/passwd lists (a missing user gives Java
// user.home=/). Every command of the grade, and the copy-in, runs as User. Nothing in a grade runs as root.
const User = "65534:65534"

// MainUser runs the container's main process (the init and the sleep that holds the deadline) and the counters'
// reads: a user the grade is not, so the grade's code can neither stop nor end them (kill needs the same user or
// CAP_KILL, and every capability is dropped). It owns nothing in the container.
const MainUser = "65533:65533"

// The grade's folders inside the container.
const (
	GradeDir = "/grade"       // the anonymous volume; root-owned, holding only work/ and cache/
	WorkDir  = "/grade/work"  // the grading copy
	CacheDir = "/grade/cache" // the grade's own caches and HOME
	DepsDir  = "/deps"        // the dependencies volume, read-only
	TmpDir   = "/tmp"         // a sized tmpfs
)

// Limits are one grade's resource limits.
type Limits struct {
	Memory int64 // bytes; the swap limit is the same, so no swap
	Pids   int64
	CPUs   int
	Tmp    int64 // the /tmp tmpfs's size in bytes (its files count against Memory)
	Shm    int64 // /dev/shm's size in bytes
}

// DefaultLimits are open decision 8's: 4 GiB of memory, 4,096 processes and min(4, the daemon's) CPUs, with a 1 GiB
// /tmp and a 256 MiB /dev/shm.
func DefaultLimits(daemonCPUs int) Limits {
	return Limits{Memory: 4 << 30, Pids: 4096, CPUs: max(1, min(4, daemonCPUs)), Tmp: 1 << 30, Shm: 256 << 20}
}

func (l Limits) validate() error {
	if l.Memory < 64<<20 || l.Pids < 16 || l.CPUs < 1 || l.Tmp < 1<<20 || l.Shm < 1<<20 {
		return fmt.Errorf("container limits too small or unset: %+v", l)
	}
	return nil
}

// Spec is one grade's container.
type Spec struct {
	Data     string // the data folder's ID (DataID): lowercase letters and digits, at most 16
	Run      string // the run's ID: letters, digits, '_', '.', '-', at most 64, starting with a letter or digit
	Role     string // what the container is for ("grade", "validate"): lowercase letters, at most 16
	Image    Image  // found by Docker.Image
	Limits   Limits
	Deadline time.Duration // how long the main process lives: every command's timeout plus a margin; at most a day
	Deps     string        // a volume mounted read-only at /deps, or none; it must already exist

	// warm makes the container a deps warm-up's (Warm), never a grade's: the default network, and the deps volume
	// mounted read-write. Only Warm sets it, and Run refuses it.
	warm bool
}

var (
	dataPattern = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	runPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	rolePattern = regexp.MustCompile(`^[a-z]{1,16}$`)
	volPattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

// DataID is the data folder's ID in names and labels: the first 8 hex digits of its resolved path's SHA-256, so two
// data folders on one daemon never share containers, and the path itself never reaches Docker.
func DataID(dataFolder string) string {
	sum := sha256.Sum256([]byte(dataFolder))
	return hex.EncodeToString(sum[:])[:8]
}

// Name is the grade's container name, agentium-<data>-<run>-<role>. The caller records it before Run creates the
// container, so recovery knows what to remove if Agentium dies in between.
func Name(s Spec) string { return "agentium-" + s.Data + "-" + s.Run + "-" + s.Role }

func (s Spec) validate() error {
	switch {
	case !dataPattern.MatchString(s.Data):
		return fmt.Errorf("container spec: data ID %q", s.Data)
	case !runPattern.MatchString(s.Run):
		return fmt.Errorf("container spec: run ID %q", s.Run)
	case !rolePattern.MatchString(s.Role):
		return fmt.Errorf("container spec: role %q", s.Role)
	case !imageID.MatchString(s.Image.ID) || !digestRef.MatchString(s.Image.Ref) && !imageID.MatchString(s.Image.Ref):
		return fmt.Errorf("container spec: image not found by digest (%q)", s.Image.Ref)
	case s.Deadline < time.Second || s.Deadline > 24*time.Hour:
		return fmt.Errorf("container spec: deadline %s", s.Deadline)
	case s.Deps != "" && !volPattern.MatchString(s.Deps):
		return fmt.Errorf("container spec: deps volume %q", s.Deps)
	}
	return s.Limits.validate()
}

// deadlineSeconds is the main process's sleep, rounded up to whole seconds.
func (s Spec) deadlineSeconds() string {
	return strconv.FormatInt(int64(math.Ceil(s.Deadline.Seconds())), 10)
}

func (s Spec) tmpfs() string {
	return "rw,exec,nosuid,nodev,size=" + strconv.FormatInt(s.Limits.Tmp, 10)
}

// labels are the container's labels, also put on its anonymous volume.
func (s Spec) labels() [][2]string {
	return [][2]string{{LabelData, s.Data}, {LabelRun, s.Run}, {LabelMode, Mode}}
}

// createArgs is the grade's docker create: the shape settled by the spike. Each flag is checked again in the
// daemon's record (checkInspect) before the container starts.
func createArgs(s Spec) []string {
	args := []string{"create", "--pull", "never", "--name", Name(s)}
	for _, l := range s.labels() {
		args = append(args, "--label", l[0]+"="+l[1])
	}
	grade := "type=volume,dst=" + GradeDir
	for _, l := range s.labels() {
		grade += ",volume-label=" + l[0] + "=" + l[1]
	}
	mem := strconv.FormatInt(s.Limits.Memory, 10)
	network, depsMode := "none", ",readonly"
	if s.warm {
		network, depsMode = "bridge", "" // a warm-up's: it fetches, from a trusted commit, into the volume
	}
	args = append(args,
		"--rm", "--init",
		"--network", network, "--ipc", "private",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--user", MainUser,
		"--memory", mem, "--memory-swap", mem,
		"--pids-limit", strconv.FormatInt(s.Limits.Pids, 10),
		"--cpus", strconv.Itoa(s.Limits.CPUs),
		"--shm-size", strconv.FormatInt(s.Limits.Shm, 10),
		"--log-driver", "none",
		"--tmpfs", TmpDir+":"+s.tmpfs(),
		"--mount", grade)
	if s.Deps != "" {
		args = append(args, "--mount", "type=volume,src="+s.Deps+",dst="+DepsDir+depsMode)
	}
	return append(args, "--entrypoint", "sleep", s.Image.Ref, s.deadlineSeconds())
}
