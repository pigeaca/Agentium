package container

import (
	"bufio"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrProbe: a probe inside the started container failed, so the grade's code must not run there. Like every check
// here it is infrastructure, never a fail, and the grade is never run in another mode instead.
var ErrProbe = errors.New("the container failed its isolation probes")

// probeScript runs inside the started container as the grade's user, before any of the grade's code, and prints
// sections that checkProbes reads; it only reads and writes probe files, and uses nothing beyond POSIX sh and the
// basic tools every image has (id, ls, cat, grep, readlink). It decides nothing itself: a missing tool shows as a
// failed probe.
const probeScript = `section() { printf '== %s\n' "$1"; }
section id; id -u; id -g
section write
for d in /tmp /grade/work /grade/cache /grade /deps /sys/fs/cgroup /; do
  if [ ! -d "$d" ]; then echo "$d missing"; continue; fi
  if (: > "$d/.agentium-probe") 2>/dev/null; then rm -f "$d/.agentium-probe"; echo "$d writable"; else echo "$d denied"; fi
done
section net; ls -1 /sys/class/net
section route; cat /proc/net/route
section ns; for n in ipc pid cgroup uts; do echo "$n $(readlink /proc/self/ns/$n)"; done
section status; grep -E '^(NoNewPrivs|Seccomp|CapInh|CapPrm|CapEff|CapBnd|CapAmb):' /proc/self/status
section mounts; cat /proc/self/mounts
section devlog; if [ -e /dev/log ]; then echo present; else echo absent; fi
section signal; if kill -0 1 2>/dev/null; then echo allowed; else echo denied; fi
section sockets
for s in /var/run/docker.sock /run/docker.sock /var/run/containerd/containerd.sock /run/containerd/containerd.sock /run/podman/podman.sock /var/run/podman/podman.sock; do
  if [ -e "$s" ]; then echo "$s"; fi
done
section cgroup; cat /proc/self/cgroup
section counters
if cat /sys/fs/cgroup/memory.events /sys/fs/cgroup/pids.events > /dev/null; then echo readable; else echo unreadable; fi
if (echo 0 > /sys/fs/cgroup/pids.max) 2>/dev/null; then echo writable; else echo protected; fi
section end
`

// initNamespaces are the inodes of the kernel's initial namespaces (include/linux/proc_ns.h): a container that shows
// one of these shares that namespace with the VM.
func initNamespaces() map[string]string {
	return map[string]string{
		"ipc":    "ipc:[4026531839]",
		"uts":    "uts:[4026531838]",
		"pid":    "pid:[4026531836]",
		"cgroup": "cgroup:[4026531835]",
	}
}

// sections splits a script's output into its "== name" sections.
func sections(out string) map[string][]string {
	got := map[string][]string{}
	current := ""
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "== "); ok {
			current = name
			got[current] = []string{}
			continue
		}
		if current != "" {
			got[current] = append(got[current], line)
		}
	}
	return got
}

// checkProbes reads probeScript's output and refuses anything but an isolated container: the grade's user, not root;
// /tmp, /grade/work and /grade/cache writable and nothing else (a read-only root, read-only /deps and cgroup files);
// the main process (pid 1, MainUser) beyond its signals;
// only the loopback interface and no route; namespaces of its own; no new privileges, seccomp filtering and no
// capabilities; no /dev/log and no runtime socket; its own cgroup namespace with readable, protected counters.
func checkProbes(out string, deps bool) error {
	s := sections(out)
	var bad []string
	fail := func(format string, args ...any) { bad = append(bad, fmt.Sprintf(format, args...)) }
	if _, ok := s["end"]; !ok {
		return fmt.Errorf("%w: the probe script did not finish", ErrProbe)
	}
	if id := s["id"]; !slices.Equal(id, []string{"65534", "65534"}) {
		fail("user and group are %v, want 65534 65534", id)
	}
	wantWrite := map[string]string{"/tmp": "writable", "/grade/work": "writable", "/grade/cache": "writable", "/grade": "denied",
		"/deps": "missing", "/sys/fs/cgroup": "denied", "/": "denied"}
	if deps {
		wantWrite["/deps"] = "denied"
	}
	seen := map[string]bool{}
	for _, line := range s["write"] {
		dir, verdict, _ := strings.Cut(line, " ")
		seen[dir] = true
		if want, ok := wantWrite[dir]; !ok || verdict != want {
			fail("%s is %s, want %s", dir, verdict, want)
		}
	}
	for dir := range wantWrite {
		if !seen[dir] {
			fail("%s was not probed", dir)
		}
	}
	if net := s["net"]; !slices.Equal(net, []string{"lo"}) {
		fail("network interfaces are %v, want [lo]", net)
	}
	if route := s["route"]; len(route) != 1 {
		fail("the routing table has %d routes, want none", max(0, len(route)-1))
	}
	nsSeen := 0
	inits := initNamespaces()
	for _, line := range s["ns"] {
		kind, inode, _ := strings.Cut(line, " ")
		init, ok := inits[kind]
		if !ok {
			continue
		}
		nsSeen++
		if inode == init || !strings.HasPrefix(inode, kind+":[") {
			fail("the %s namespace is %q, the VM's own or unreadable", kind, inode)
		}
	}
	if nsSeen != len(inits) {
		fail("%d of %d namespaces were read", nsSeen, len(inits))
	}
	status := map[string]string{}
	for _, line := range s["status"] {
		k, v, _ := strings.Cut(line, ":")
		status[k] = strings.TrimSpace(v)
	}
	if status["NoNewPrivs"] != "1" {
		fail("NoNewPrivs is %q, want 1", status["NoNewPrivs"])
	}
	if status["Seccomp"] != "2" {
		fail("Seccomp is %q, want 2 (filtering)", status["Seccomp"])
	}
	for _, c := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		if v, err := strconv.ParseUint(status[c], 16, 64); err != nil || v != 0 {
			fail("%s is %q, want no capabilities", c, status[c])
		}
	}
	bad = append(bad, checkMountTable(s["mounts"], deps)...)
	if devlog := s["devlog"]; !slices.Equal(devlog, []string{"absent"}) {
		fail("/dev/log is %v, want absent", devlog)
	}
	if signal := s["signal"]; !slices.Equal(signal, []string{"denied"}) {
		fail("signalling the main process (pid 1) is %v, want denied", signal)
	}
	if socks := s["sockets"]; len(socks) > 0 {
		fail("runtime sockets are reachable: %v", socks)
	}
	if cg := s["cgroup"]; !slices.Equal(cg, []string{"0::/"}) {
		fail("the cgroup is %v, want its own namespace (0::/)", cg)
	}
	if counters := s["counters"]; !slices.Equal(counters, []string{"readable", "protected"}) {
		fail("the cgroup counters are %v, want readable and protected", counters)
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s", ErrProbe, strings.Join(bad, "; "))
	}
	return nil
}

// checkMountTable reads /proc/self/mounts: the root and /deps read-only, /grade read-write, /tmp a nosuid, nodev
// tmpfs, and the cgroup files read-only.
func checkMountTable(lines []string, deps bool) []string {
	type mount struct{ fstype, opts string }
	table := map[string]mount{}
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		table[f[1]] = mount{f[2], f[3]} // the last mount at a point is the visible one
	}
	opt := func(m mount, o string) bool { return slices.Contains(strings.Split(m.opts, ","), o) }
	var bad []string
	check := func(point string, ok func(mount) bool, want string) {
		m, found := table[point]
		if !found || !ok(m) {
			bad = append(bad, fmt.Sprintf("the mount at %s is %q %q, want %s", point, m.fstype, m.opts, want))
		}
	}
	check("/", func(m mount) bool { return opt(m, "ro") }, "read-only")
	check(GradeDir, func(m mount) bool { return opt(m, "rw") }, "read-write")
	check(TmpDir, func(m mount) bool { return m.fstype == "tmpfs" && opt(m, "rw") && opt(m, "nosuid") && opt(m, "nodev") }, "a nosuid, nodev tmpfs")
	check("/sys/fs/cgroup", func(m mount) bool { return opt(m, "ro") }, "read-only")
	if deps {
		check(DepsDir, func(m mount) bool { return opt(m, "ro") }, "read-only")
	} else if _, found := table[DepsDir]; found {
		bad = append(bad, "a mount at /deps that was not asked for")
	}
	return bad
}

// countersScript prints the container's cgroup counters after a command, run as MainUser so the grade's code cannot
// kill it. They are the kernel's: the grade can read them and cannot write them.
const countersScript = `for f in memory.events pids.events memory.peak; do printf '== %s\n' "$f"; cat "/sys/fs/cgroup/$f" 2>/dev/null || echo unavailable; done; printf '== end\n'`

// Counters are the container's cgroup counters, cumulative since it started.
type Counters struct {
	OOMKills   int64 `json:"oom_kills"`   // memory.events oom_kill
	PidsMax    int64 `json:"pids_max"`    // pids.events max: forks refused at the process limit
	MemoryPeak int64 `json:"memory_peak"` // memory.peak in bytes; 0 where the kernel lacks it
}

// Hit reports whether a resource limit was reached: a failure then is left out, not counted (open decision 7).
func (c Counters) Hit() bool { return c.OOMKills > 0 || c.PidsMax > 0 }

// parseCounters reads countersScript's output. memory.events and pids.events must be there; memory.peak may not.
func parseCounters(out string) (Counters, error) {
	s := sections(out)
	if _, ok := s["end"]; !ok {
		return Counters{}, errors.New("cgroup counters: the script did not finish")
	}
	keyed := func(name, key string) (int64, error) {
		for _, line := range s[name] {
			k, v, ok := strings.Cut(line, " ")
			if ok && k == key {
				return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			}
		}
		return 0, fmt.Errorf("cgroup counters: no %s in %s", key, name)
	}
	var c Counters
	var err error
	if c.OOMKills, err = keyed("memory.events", "oom_kill"); err != nil {
		return Counters{}, err
	}
	if c.PidsMax, err = keyed("pids.events", "max"); err != nil {
		return Counters{}, err
	}
	if peak := s["memory.peak"]; len(peak) == 1 && peak[0] != "unavailable" {
		if c.MemoryPeak, err = strconv.ParseInt(strings.TrimSpace(peak[0]), 10, 64); err != nil {
			return Counters{}, fmt.Errorf("cgroup counters: memory.peak %q", peak[0])
		}
	}
	return c, nil
}
