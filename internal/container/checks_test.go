package container

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// inspectWith returns the real record (testdata/inspect.json) with one change applied to its only element.
func inspectWith(t *testing.T, change func(c map[string]any)) []byte {
	t.Helper()
	var list []map[string]any
	must(t, json.Unmarshal([]byte(readFixture(t, "inspect.json")), &list))
	change(list[0])
	data, err := json.Marshal(list)
	must(t, err)
	return data
}

func obj(m map[string]any, key string) map[string]any { return m[key].(map[string]any) }

// TestInspectAcceptsTheRealRecord: the daemon's own record of the shape passes, and its digest leaves out the
// instance (name, IDs, run labels): two grades of one shape share it.
func TestInspectAcceptsTheRealRecord(t *testing.T) {
	spec := fixtureSpec()
	digest, env, err := checkInspect([]byte(readFixture(t, "inspect.json")), spec, "runc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(env) == 0 {
		t.Errorf("digest %q, env %v", digest, env)
	}
	other := spec
	other.Run = "another"
	raw := inspectWith(t, func(c map[string]any) {
		c["Name"] = "/" + Name(other)
		c["Id"] = strings.Repeat("e", 64)
		obj(obj(c, "Config"), "Labels")[LabelRun] = "another"
		obj(obj(c, "HostConfig")["Mounts"].([]any)[0].(map[string]any), "VolumeOptions")["Labels"].(map[string]any)[LabelRun] = "another"
		c["Mounts"].([]any)[0].(map[string]any)["Name"] = strings.Repeat("w", 64)
	})
	digest2, _, err := checkInspect(raw, other, "runc")
	if err != nil || digest2 != digest {
		t.Errorf("another run of the same shape: %q, %v; want %q", digest2, err, digest)
	}
	bigger := spec
	bigger.Limits.Pids = 8192
	raw = inspectWith(t, func(c map[string]any) { obj(c, "HostConfig")["PidsLimit"] = 8192 })
	if digest3, _, err := checkInspect(raw, bigger, "runc"); err != nil || digest3 == digest {
		t.Errorf("another shape has the same digest: %v", err)
	}
}

// TestInspectMismatchRefused: every way the daemon's record can differ from what was asked is refused, naming the
// field.
func TestInspectMismatchRefused(t *testing.T) {
	host := func(key string, v any) func(map[string]any) {
		return func(c map[string]any) { obj(c, "HostConfig")[key] = v }
	}
	config := func(key string, v any) func(map[string]any) {
		return func(c map[string]any) { obj(c, "Config")[key] = v }
	}
	bind := map[string]any{"Type": "bind", "Source": "/Users", "Target": "/host", "ReadOnly": true}
	cases := map[string]struct {
		change func(map[string]any)
		field  string
	}{
		"network bridge":         {host("NetworkMode", "bridge"), "HostConfig.NetworkMode"},
		"network default":        {host("NetworkMode", "default"), "HostConfig.NetworkMode"},
		"network host":           {host("NetworkMode", "host"), "HostConfig.NetworkMode"},
		"another network joined": {func(c map[string]any) { obj(obj(c, "NetworkSettings"), "Networks")["bridge"] = map[string]any{} }, "NetworkSettings.Networks"},
		"ipc host":               {host("IpcMode", "host"), "HostConfig.IpcMode"},
		"ipc shareable":          {host("IpcMode", "shareable"), "HostConfig.IpcMode"},
		"pid host":               {host("PidMode", "host"), "HostConfig.PidMode"},
		"uts host":               {host("UTSMode", "host"), "HostConfig.UTSMode"},
		"userns host":            {host("UsernsMode", "host"), "HostConfig.UsernsMode"},
		"cgroupns host":          {host("CgroupnsMode", "host"), "HostConfig.CgroupnsMode"},
		"privileged":             {host("Privileged", true), "HostConfig.Privileged"},
		"a capability added":     {host("CapAdd", []string{"NET_RAW"}), "HostConfig.CapAdd"},
		"capabilities kept":      {host("CapDrop", nil), "HostConfig.CapDrop"},
		"one capability dropped": {host("CapDrop", []string{"NET_RAW"}), "HostConfig.CapDrop"},
		"new privileges allowed": {host("SecurityOpt", nil), "HostConfig.SecurityOpt"},
		"seccomp unconfined":     {host("SecurityOpt", []string{"no-new-privileges", "seccomp=unconfined"}), "HostConfig.SecurityOpt"},
		"apparmor unconfined":    {host("SecurityOpt", []string{"no-new-privileges", "apparmor=unconfined"}), "HostConfig.SecurityOpt"},
		"writable root":          {host("ReadonlyRootfs", false), "HostConfig.ReadonlyRootfs"},
		"paths unmasked":         {host("MaskedPaths", []string{}), "HostConfig.MaskedPaths"},
		"proc writable":          {host("ReadonlyPaths", nil), "HostConfig.ReadonlyPaths"},
		"a bind":                 {host("Binds", []string{"/Users:/host:ro"}), "HostConfig.Binds"},
		"the docker socket":      {host("Binds", []string{"/var/run/docker.sock:/var/run/docker.sock"}), "HostConfig.Binds"},
		"volumes from":           {host("VolumesFrom", []string{"other"}), "HostConfig.VolumesFrom"},
		"links":                  {host("Links", []string{"/other:/me/other"}), "HostConfig.Links"},
		"a device":               {host("Devices", []any{map[string]any{"PathOnHost": "/dev/kvm", "PathInContainer": "/dev/kvm", "CgroupPermissions": "rwm"}}), "HostConfig.Devices"},
		"a device request":       {host("DeviceRequests", []any{map[string]any{"Driver": "nvidia"}}), "HostConfig.DeviceRequests"},
		"a device cgroup rule":   {host("DeviceCgroupRules", []string{"c 1:3 rwm"}), "HostConfig.DeviceCgroupRules"},
		"a published port":       {host("PortBindings", map[string]any{"80/tcp": []any{map[string]any{"HostPort": "8080"}}}), "HostConfig.PortBindings"},
		"publish all":            {host("PublishAllPorts", true), "HostConfig.PublishAllPorts"},
		"sysctls":                {host("Sysctls", map[string]string{"net.ipv4.ip_forward": "1"}), "HostConfig.Sysctls"},
		"extra groups":           {host("GroupAdd", []string{"docker"}), "HostConfig.GroupAdd"},
		"another tmpfs":          {host("Tmpfs", map[string]string{"/tmp": fixtureSpec().tmpfs(), "/run": "rw"}), "HostConfig.Tmpfs"},
		"tmpfs unsized":          {host("Tmpfs", map[string]string{"/tmp": "rw,exec,nosuid,nodev"}), "HostConfig.Tmpfs"},
		"no tmpfs":               {host("Tmpfs", nil), "HostConfig.Tmpfs"},
		"memory":                 {host("Memory", 0), "HostConfig.Memory"},
		"swap":                   {host("MemorySwap", -1), "HostConfig.MemorySwap"},
		"no process limit":       {host("PidsLimit", nil), "HostConfig.PidsLimit"},
		"unlimited processes":    {host("PidsLimit", -1), "HostConfig.PidsLimit"},
		"cpus":                   {host("NanoCpus", 0), "HostConfig.NanoCpus"},
		"shm":                    {host("ShmSize", 64<<20), "HostConfig.ShmSize"},
		"oom killer off":         {host("OomKillDisable", true), "HostConfig.OomKillDisable"},
		"kept after it ends":     {host("AutoRemove", false), "HostConfig.AutoRemove"},
		"no init":                {host("Init", false), "HostConfig.Init"},
		"logs kept":              {host("LogConfig", map[string]any{"Type": "json-file"}), "HostConfig.LogConfig.Type"},
		"root user":              {config("User", ""), "Config.User"},
		"the grade's user":       {config("User", User), "Config.User"},
		"proxies added": {func(c map[string]any) {
			obj(c, "Config")["Env"] = append([]any{"HTTP_PROXY=http://user:FAKEPASS@proxy.example:3128", "http_proxy=http://user:FAKEPASS@proxy.example:3128"}, obj(c, "Config")["Env"].([]any)...)
		}, "Config.Env"},
		"a variable added":    {func(c map[string]any) { obj(c, "Config")["Env"] = append(obj(c, "Config")["Env"].([]any), "X=1") }, "Config.Env"},
		"a variable changed":  {func(c map[string]any) { obj(c, "Config")["Env"].([]any)[0] = "PATH=/tmp/evil:/usr/bin" }, "Config.Env"},
		"no environment":      {config("Env", nil), "Config.Env"},
		"another runtime":     {host("Runtime", "runsc"), "HostConfig.Runtime"},
		"no runtime":          {host("Runtime", ""), "HostConfig.Runtime"},
		"user 0":              {config("User", "0:0"), "Config.User"},
		"an entrypoint":       {config("Entrypoint", []string{"/entrypoint.sh"}), "Config.Entrypoint"},
		"another deadline":    {config("Cmd", []string{"86400"}), "Config.Cmd"},
		"process":             {func(c map[string]any) { c["Path"] = "sh" }, "Path"},
		"a terminal":          {config("Tty", true), "Config.Tty"},
		"stdin open":          {config("OpenStdin", true), "Config.OpenStdin"},
		"labels missing":      {config("Labels", nil), "Config.Labels[agentium.data]"},
		"another run's label": {func(c map[string]any) { obj(obj(c, "Config"), "Labels")[LabelRun] = "other" }, "Config.Labels[agentium.run]"},
		"another image":       {func(c map[string]any) { c["Image"] = "sha256:" + strings.Repeat("0", 64) }, "Image"},
		"another name":        {func(c map[string]any) { c["Name"] = "/agentium-test-other-grade" }, "Name"},
		"a bind asked":        {func(c map[string]any) { h := obj(c, "HostConfig"); h["Mounts"] = append(h["Mounts"].([]any), bind) }, "HostConfig.Mounts"},
		"the grade volume read-only": {func(c map[string]any) {
			obj(c, "HostConfig")["Mounts"].([]any)[0].(map[string]any)["ReadOnly"] = true
		}, "HostConfig.Mounts[0]"},
		"the grade volume unlabelled": {func(c map[string]any) {
			delete(obj(c, "HostConfig")["Mounts"].([]any)[0].(map[string]any), "VolumeOptions")
		}, "HostConfig.Mounts[0]"},
		"the grade volume through a driver": {func(c map[string]any) {
			obj(obj(c, "HostConfig")["Mounts"].([]any)[0].(map[string]any), "VolumeOptions")["DriverConfig"] = map[string]any{"Name": "sshfs"}
		}, "HostConfig.Mounts[0]"},
		"the grade volume with bind options": {func(c map[string]any) {
			obj(c, "HostConfig")["Mounts"].([]any)[0].(map[string]any)["BindOptions"] = map[string]any{"Propagation": "rshared"}
		}, "HostConfig.Mounts[0]"},
		"a bind mounted": {func(c map[string]any) {
			c["Mounts"] = append(c["Mounts"].([]any), map[string]any{"Type": "bind", "Source": "/Users", "Destination": "/host", "RW": false})
		}, "Mounts has bind"},
		"an image volume": {func(c map[string]any) {
			c["Mounts"] = append(c["Mounts"].([]any), map[string]any{"Type": "volume", "Name": "x", "Destination": "/var/lib/data", "RW": true, "Driver": "local"})
		}, "Mounts has volume"},
		"the grade volume missing": {func(c map[string]any) { c["Mounts"] = []any{} }, "Mounts has 0 grade"},
		"the grade volume remote": {func(c map[string]any) {
			c["Mounts"].([]any)[0].(map[string]any)["Driver"] = "sshfs"
		}, "Mounts has volume"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := checkInspect(inspectWith(t, c.change), fixtureSpec(), "runc")
			if !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), c.field) {
				t.Fatalf("%v, want ErrMismatch naming %s", err, c.field)
			}
			if strings.Contains(err.Error(), "FAKEPASS") || strings.Contains(err.Error(), "/tmp/evil") {
				t.Errorf("the error carries a variable's value: %v", err)
			}
		})
	}
	_, _, err := checkInspect(inspectWith(t, func(c map[string]any) {
		env := obj(c, "Config")["Env"].([]any)
		obj(c, "Config")["Env"] = append([]any{"HTTP_PROXY=http://user:FAKEPASS@proxy.example:3128"}, append(env[1:], "PATH=/x")...)
	}), fixtureSpec(), "runc")
	if err == nil || !strings.Contains(err.Error(), "added [HTTP_PROXY], dropped [], changed [PATH]") {
		t.Errorf("the environment's difference by name: %v", err)
	}
	if _, _, err := checkInspect([]byte(readFixture(t, "inspect.json")), fixtureSpec(), "runsc"); !errors.Is(err, ErrMismatch) {
		t.Errorf("a daemon whose default runtime is another: %v", err)
	}
	if _, _, err := checkInspect([]byte(readFixture(t, "inspect.json")), fixtureSpec(), ""); !errors.Is(err, ErrMismatch) {
		t.Errorf("no default runtime known: %v", err)
	}
	for _, raw := range []string{"", "{}", "[]", "[{},{}]", "not json"} {
		if _, _, err := checkInspect([]byte(raw), fixtureSpec(), "runc"); !errors.Is(err, ErrMismatch) {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

// TestInspectDepsVolume: with a deps volume, exactly it is allowed, read-only, at /deps.
func TestInspectDepsVolume(t *testing.T) {
	spec := fixtureSpec()
	spec.Deps = "agentium-deps-test"
	deps := func(ro bool, src string) func(map[string]any) {
		return func(c map[string]any) {
			h := obj(c, "HostConfig")
			h["Mounts"] = append(h["Mounts"].([]any), map[string]any{"Type": "volume", "Source": src, "Target": DepsDir, "ReadOnly": ro})
			c["Mounts"] = append(c["Mounts"].([]any), map[string]any{"Type": "volume", "Name": src, "Destination": DepsDir, "RW": !ro, "Driver": "local"})
		}
	}
	if _, _, err := checkInspect(inspectWith(t, deps(true, spec.Deps)), spec, "runc"); err != nil {
		t.Errorf("the deps volume read-only: %v", err)
	}
	for name, change := range map[string]func(map[string]any){
		"writable":       deps(false, spec.Deps),
		"another volume": deps(true, "other"),
		"absent":         func(map[string]any) {},
	} {
		if _, _, err := checkInspect(inspectWith(t, change), spec, "runc"); !errors.Is(err, ErrMismatch) {
			t.Errorf("deps %s: %v", name, err)
		}
	}
}

// TestProbesRefuseAnythingButIsolation: the real probes pass, and each sign of a weaker container is refused.
func TestProbesRefuseAnythingButIsolation(t *testing.T) {
	good := readFixture(t, "probe.txt")
	if err := checkProbes(good, false); err != nil {
		t.Fatal(err)
	}
	replace := func(old, new string) string {
		if !strings.Contains(good, old) {
			t.Fatalf("the fixture lacks %q", old)
		}
		return strings.Replace(good, old, new, 1)
	}
	cases := map[string]string{
		"root":                    replace("== id\n65534\n65534\n", "== id\n0\n0\n"),
		"work not writable":       replace("/grade/work writable", "/grade/work denied"),
		"cache missing":           replace("/grade/cache writable", "/grade/cache missing"),
		"tmp not writable":        replace("/tmp writable", "/tmp denied"),
		"volume root writable":    replace("/grade denied", "/grade writable"),
		"root writable":           replace("/ denied", "/ writable"),
		"cgroup writable":         replace("/sys/fs/cgroup denied", "/sys/fs/cgroup writable"),
		"deps unexpected":         replace("/deps missing", "/deps denied"),
		"a probe missing":         replace("/tmp writable\n", ""),
		"an interface":            replace("== net\nlo\n", "== net\neth0\nlo\n"),
		"a route":                 replace("== ns\n", "eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n== ns\n"),
		"the VM's IPC namespace":  replace("ipc ipc:[4026532327]", "ipc ipc:[4026531839]"),
		"the VM's PID namespace":  replace("pid pid:[4026532328]", "pid pid:[4026531836]"),
		"the VM's cgroups":        replace("cgroup cgroup:[4026532329]", "cgroup cgroup:[4026531835]"),
		"the VM's hostname space": replace("uts uts:[4026532326]", "uts uts:[4026531838]"),
		"a namespace unread":      replace("uts uts:[4026532326]\n", ""),
		"new privileges":          replace("NoNewPrivs:\t1", "NoNewPrivs:\t0"),
		"no seccomp":              replace("Seccomp:\t2", "Seccomp:\t0"),
		"a capability":            replace("CapEff:\t0000000000000000", "CapEff:\t0000000000002000"),
		"bounding set":            replace("CapBnd:\t0000000000000000", "CapBnd:\t00000000a80425fb"),
		"capability unreadable":   replace("CapPrm:\t0000000000000000\n", ""),
		"root fs writable":        replace("overlay / overlay ro,", "overlay / overlay rw,"),
		"tmp not tmpfs":           replace("tmpfs /tmp tmpfs rw,nosuid,nodev,", "/dev/root /tmp ext4 rw,nosuid,nodev,"),
		"tmp allows devices":      replace("tmpfs /tmp tmpfs rw,nosuid,nodev,", "tmpfs /tmp tmpfs rw,nosuid,"),
		"grade read-only":         replace("/dev/root /grade ext4 rw,", "/dev/root /grade ext4 ro,"),
		"cgroup fs writable":      replace("cgroup /sys/fs/cgroup cgroup2 ro,", "cgroup /sys/fs/cgroup cgroup2 rw,"),
		"deps mounted":            replace("== devlog", "/dev/root /deps ext4 ro 0 0\n== devlog"),
		"dev log":                 replace("== devlog\nabsent", "== devlog\npresent"),
		"main process signalled":  replace("== signal\ndenied", "== signal\nallowed"),
		"signal unprobed":         replace("== signal\ndenied\n", ""),
		"docker socket":           replace("== sockets\n", "== sockets\n/var/run/docker.sock\n"),
		"the VM's cgroup":         replace("== cgroup\n0::/\n", "== cgroup\n0::/docker/abc\n"),
		"counters unreadable":     replace("readable\nprotected", "unreadable\nprotected"),
		"counters writable":       replace("readable\nprotected", "readable\nwritable"),
		"unfinished":              replace("== end\n", ""),
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkProbes(out, false); !errors.Is(err, ErrProbe) {
				t.Fatalf("%v, want ErrProbe", err)
			}
		})
	}
	// With a deps volume, /deps must be there, read-only and not writable.
	withDeps := strings.Replace(strings.Replace(good, "/deps missing", "/deps denied", 1), "== devlog", "/dev/root /deps ext4 ro,relatime 0 0\n== devlog", 1)
	if err := checkProbes(withDeps, true); err != nil {
		t.Errorf("with deps: %v", err)
	}
	if err := checkProbes(strings.Replace(withDeps, "/deps denied", "/deps writable", 1), true); !errors.Is(err, ErrProbe) {
		t.Errorf("writable deps: %v", err)
	}
	if err := checkProbes(good, true); !errors.Is(err, ErrProbe) {
		t.Errorf("deps asked and missing: %v", err)
	}
}

// TestCounters: the counters are read from their own files (memory.events' "max" is not pids.events'), memory.peak
// is optional, and a missing counter is an error, never a zero.
func TestCounters(t *testing.T) {
	c, err := parseCounters(goodCounters)
	if err != nil || c != (Counters{MemoryPeak: 104857600}) || c.Hit() {
		t.Errorf("good: %+v, %v", c, err)
	}
	hit := "== memory.events\nlow 0\nhigh 0\nmax 7\noom 1\noom_kill 1\n== pids.events\nmax 3\n== memory.peak\nunavailable\n== end\n"
	if c, err := parseCounters(hit); err != nil || c != (Counters{OOMKills: 1, PidsMax: 3}) || !c.Hit() {
		t.Errorf("hit: %+v, %v", c, err)
	}
	if c, _ := parseCounters(strings.Replace(hit, "max 3", "max 0", 1)); c.PidsMax != 0 || c.OOMKills != 1 {
		t.Errorf("memory.events max read as pids: %+v", c)
	}
	for name, out := range map[string]string{
		"no memory.events": "== memory.events\nunavailable\n== pids.events\nmax 0\n== end\n",
		"no pids.events":   "== memory.events\noom_kill 0\n== pids.events\nunavailable\n== end\n",
		"unfinished":       "== memory.events\noom_kill 0\n== pids.events\nmax 0\n",
		"garbage":          "== memory.events\noom_kill x\n== pids.events\nmax 0\n== end\n",
		"bad peak":         "== memory.events\noom_kill 0\n== pids.events\nmax 0\n== memory.peak\nx\n== end\n",
	} {
		if _, err := parseCounters(out); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
