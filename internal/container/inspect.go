package container

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ErrMismatch: the daemon's record of the created container differs from what was asked. The container is removed
// and never started.
var ErrMismatch = errors.New("the created container is not the one asked for")

// inspected is the part of `docker container inspect` the check reads. Lists and maps the check wants empty are
// decoded as raw JSON so that anything at all in them (not only the shapes known today) is refused.
type inspected struct {
	Name   string
	Image  string
	Path   string
	Args   []string
	Config struct {
		User       string
		Entrypoint []string
		Cmd        []string
		Env        []string
		Labels     map[string]string
		Tty        bool
		OpenStdin  bool
	}
	HostConfig struct {
		NetworkMode       string
		Runtime           string
		IpcMode           string
		PidMode           string
		UTSMode           string
		UsernsMode        string
		CgroupnsMode      string
		Privileged        bool
		CapAdd            json.RawMessage
		CapDrop           []string
		SecurityOpt       []string
		ReadonlyRootfs    bool
		Binds             json.RawMessage
		VolumesFrom       json.RawMessage
		Links             json.RawMessage
		Devices           json.RawMessage
		DeviceRequests    json.RawMessage
		DeviceCgroupRules json.RawMessage
		PortBindings      json.RawMessage
		PublishAllPorts   bool
		Sysctls           json.RawMessage
		GroupAdd          json.RawMessage
		Tmpfs             map[string]string
		Memory            int64
		MemorySwap        int64
		PidsLimit         *int64
		NanoCpus          int64
		ShmSize           int64
		OomKillDisable    *bool
		AutoRemove        bool
		Init              *bool
		LogConfig         struct{ Type string }
		MaskedPaths       []string
		ReadonlyPaths     []string
		Mounts            []struct {
			Type          string
			Source        string
			Target        string
			ReadOnly      bool
			BindOptions   json.RawMessage
			TmpfsOptions  json.RawMessage
			VolumeOptions *struct {
				Labels       map[string]string
				DriverConfig *struct{ Name string }
			}
		}
	}
	Mounts []struct {
		Type        string
		Name        string
		Destination string
		Driver      string
		RW          bool
	}
	NetworkSettings struct {
		Networks map[string]json.RawMessage
	}
}

// shape is the normalized record the inspect digest is taken over: what was checked, without the instance (its name,
// IDs, run labels and volume names). Two grades in the same shape and image have the same digest.
type shape struct {
	Image          string            `json:"image"`
	User           string            `json:"user"`
	Runtime        string            `json:"runtime"`
	Entrypoint     []string          `json:"entrypoint"`
	Cmd            []string          `json:"cmd"`
	Network        string            `json:"network"`
	IPC            string            `json:"ipc"`
	Cgroupns       string            `json:"cgroupns"`
	CapDrop        []string          `json:"cap_drop"`
	SecurityOpt    []string          `json:"security_opt"`
	ReadonlyRootfs bool              `json:"readonly_rootfs"`
	Tmpfs          map[string]string `json:"tmpfs"`
	Memory         int64             `json:"memory"`
	MemorySwap     int64             `json:"memory_swap"`
	Pids           int64             `json:"pids"`
	NanoCPUs       int64             `json:"nano_cpus"`
	Shm            int64             `json:"shm"`
	Log            string            `json:"log"`
	AutoRemove     bool              `json:"auto_remove"`
	Init           bool              `json:"init"`
	Mounts         []string          `json:"mounts"`
}

// checkInspect compares the daemon's record of the created container (raw: `docker container inspect` output, an
// array of one) with spec, and returns the normalized digest of what it checked. Every difference is named; any one
// refuses the container.
//
// runtime is the daemon's default runtime, the only one accepted. The container's environment must be the image's own,
// exactly: the docker client adds variables to a create on its own (its configuration's proxies, credentials
// included), and every command would inherit them.
func checkInspect(raw []byte, spec Spec, runtime string) (digest string, imageEnv []string, err error) {
	var list []inspected
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", nil, fmt.Errorf("%w: unreadable record: %v", ErrMismatch, err)
	}
	if len(list) != 1 {
		return "", nil, fmt.Errorf("%w: %d records", ErrMismatch, len(list))
	}
	c := list[0]
	h := c.HostConfig
	var bad []string
	want := func(field string, ok bool, have any, expected any) {
		if !ok {
			bad = append(bad, fmt.Sprintf("%s is %v, want %v", field, have, expected))
		}
	}
	empty := func(field string, raw json.RawMessage) {
		s := strings.TrimSpace(string(raw))
		want(field, s == "" || s == "null" || s == "[]" || s == "{}", s, "empty")
	}
	name := Name(spec)
	want("Name", c.Name == "/"+name, c.Name, "/"+name)
	want("Image", c.Image == spec.Image.ID, c.Image, spec.Image.ID)
	want("Config.User", c.Config.User == MainUser, c.Config.User, MainUser)
	if !slices.Equal(c.Config.Env, spec.Image.Env) {
		// Names only: the values may be credentials (a proxy's password), and this error can reach logs.
		bad = append(bad, "Config.Env differs from the image's: "+envDiff(c.Config.Env, spec.Image.Env))
	}
	want("HostConfig.Runtime", runtime != "" && h.Runtime == runtime, h.Runtime, runtime)
	want("Config.Entrypoint", slices.Equal(c.Config.Entrypoint, []string{"sleep"}), c.Config.Entrypoint, "[sleep]")
	want("Config.Cmd", slices.Equal(c.Config.Cmd, []string{spec.deadlineSeconds()}), c.Config.Cmd, "["+spec.deadlineSeconds()+"]")
	want("Path", c.Path == "sleep" && slices.Equal(c.Args, []string{spec.deadlineSeconds()}), c.Path+" "+strings.Join(c.Args, " "), "sleep "+spec.deadlineSeconds())
	want("Config.Tty", !c.Config.Tty, c.Config.Tty, false)
	want("Config.OpenStdin", !c.Config.OpenStdin, c.Config.OpenStdin, false)
	for _, l := range spec.labels() {
		want("Config.Labels["+l[0]+"]", c.Config.Labels[l[0]] == l[1], c.Config.Labels[l[0]], l[1])
	}
	want("HostConfig.NetworkMode", h.NetworkMode == "none", h.NetworkMode, "none")
	networks := make([]string, 0, len(c.NetworkSettings.Networks))
	for n := range c.NetworkSettings.Networks {
		networks = append(networks, n)
	}
	sort.Strings(networks)
	want("NetworkSettings.Networks", slices.Equal(networks, []string{"none"}), networks, "[none]")
	want("HostConfig.IpcMode", h.IpcMode == "private", h.IpcMode, "private")
	want("HostConfig.PidMode", h.PidMode == "", h.PidMode, `""`)
	want("HostConfig.UTSMode", h.UTSMode == "", h.UTSMode, `""`)
	want("HostConfig.UsernsMode", h.UsernsMode == "", h.UsernsMode, `""`)
	want("HostConfig.CgroupnsMode", h.CgroupnsMode == "private", h.CgroupnsMode, "private")
	want("HostConfig.Privileged", !h.Privileged, h.Privileged, false)
	empty("HostConfig.CapAdd", h.CapAdd)
	want("HostConfig.CapDrop", slices.Equal(h.CapDrop, []string{"ALL"}), h.CapDrop, "[ALL]")
	want("HostConfig.SecurityOpt", slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}), h.SecurityOpt, "[no-new-privileges]")
	want("HostConfig.ReadonlyRootfs", h.ReadonlyRootfs, h.ReadonlyRootfs, true)
	want("HostConfig.MaskedPaths", len(h.MaskedPaths) > 0, len(h.MaskedPaths), "the default masked paths")
	want("HostConfig.ReadonlyPaths", len(h.ReadonlyPaths) > 0, len(h.ReadonlyPaths), "the default read-only paths")
	empty("HostConfig.Binds", h.Binds)
	empty("HostConfig.VolumesFrom", h.VolumesFrom)
	empty("HostConfig.Links", h.Links)
	empty("HostConfig.Devices", h.Devices)
	empty("HostConfig.DeviceRequests", h.DeviceRequests)
	empty("HostConfig.DeviceCgroupRules", h.DeviceCgroupRules)
	empty("HostConfig.PortBindings", h.PortBindings)
	want("HostConfig.PublishAllPorts", !h.PublishAllPorts, h.PublishAllPorts, false)
	empty("HostConfig.Sysctls", h.Sysctls)
	empty("HostConfig.GroupAdd", h.GroupAdd)
	wantTmpfs := map[string]string{TmpDir: spec.tmpfs()}
	want("HostConfig.Tmpfs", mapsEqual(h.Tmpfs, wantTmpfs), h.Tmpfs, wantTmpfs)
	want("HostConfig.Memory", h.Memory == spec.Limits.Memory, h.Memory, spec.Limits.Memory)
	want("HostConfig.MemorySwap", h.MemorySwap == spec.Limits.Memory, h.MemorySwap, spec.Limits.Memory)
	pids := int64(0)
	if h.PidsLimit != nil {
		pids = *h.PidsLimit
	}
	want("HostConfig.PidsLimit", pids == spec.Limits.Pids, pids, spec.Limits.Pids)
	want("HostConfig.NanoCpus", h.NanoCpus == int64(spec.Limits.CPUs)*1e9, h.NanoCpus, int64(spec.Limits.CPUs)*1e9)
	want("HostConfig.ShmSize", h.ShmSize == spec.Limits.Shm, h.ShmSize, spec.Limits.Shm)
	want("HostConfig.OomKillDisable", h.OomKillDisable == nil || !*h.OomKillDisable, h.OomKillDisable != nil && *h.OomKillDisable, false)
	want("HostConfig.AutoRemove", h.AutoRemove, h.AutoRemove, true)
	want("HostConfig.Init", h.Init != nil && *h.Init, h.Init != nil && *h.Init, true)
	want("HostConfig.LogConfig.Type", h.LogConfig.Type == "none", h.LogConfig.Type, "none")
	bad = append(bad, checkMounts(c, spec)...)
	if len(bad) > 0 {
		return "", nil, fmt.Errorf("%w: %s", ErrMismatch, strings.Join(bad, "; "))
	}
	norm := shape{Image: c.Image, User: c.Config.User, Runtime: h.Runtime, Entrypoint: c.Config.Entrypoint, Cmd: c.Config.Cmd, Network: h.NetworkMode, IPC: h.IpcMode,
		Cgroupns: h.CgroupnsMode, CapDrop: h.CapDrop, SecurityOpt: h.SecurityOpt, ReadonlyRootfs: h.ReadonlyRootfs, Tmpfs: h.Tmpfs,
		Memory: h.Memory, MemorySwap: h.MemorySwap, Pids: pids, NanoCPUs: h.NanoCpus, Shm: h.ShmSize, Log: h.LogConfig.Type,
		AutoRemove: h.AutoRemove, Init: true, Mounts: []string{"volume " + GradeDir + " rw"}}
	if spec.Deps != "" {
		norm.Mounts = append(norm.Mounts, "volume "+DepsDir+" ro")
	}
	data, err := json.Marshal(norm)
	if err != nil {
		return "", nil, fmt.Errorf("inspect digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), c.Config.Env, nil
}

// checkMounts allows exactly the mounts asked for: an anonymous volume at /grade, read-write and labelled, and the
// deps volume at /deps, read-only, when there is one. A bind of any kind, a tmpfs mount, or a volume the image itself
// declares is refused.
func checkMounts(c inspected, spec Spec) []string {
	var bad []string
	wantAsked := 1
	if spec.Deps != "" {
		wantAsked = 2
	}
	if len(c.HostConfig.Mounts) != wantAsked {
		bad = append(bad, fmt.Sprintf("HostConfig.Mounts has %d entries, want %d", len(c.HostConfig.Mounts), wantAsked))
	}
	for i, m := range c.HostConfig.Mounts {
		field := "HostConfig.Mounts[" + strconv.Itoa(i) + "]"
		if strings.TrimSpace(string(m.BindOptions)) != "" && string(m.BindOptions) != "null" || strings.TrimSpace(string(m.TmpfsOptions)) != "" && string(m.TmpfsOptions) != "null" {
			bad = append(bad, field+" has bind or tmpfs options")
		}
		switch {
		case m.Type == "volume" && m.Target == GradeDir && m.Source == "" && !m.ReadOnly:
			if m.VolumeOptions == nil || m.VolumeOptions.DriverConfig != nil && m.VolumeOptions.DriverConfig.Name != "" {
				bad = append(bad, field+" lacks its labels or names a volume driver")
				continue
			}
			for _, l := range spec.labels() {
				if m.VolumeOptions.Labels[l[0]] != l[1] {
					bad = append(bad, fmt.Sprintf("%s's label %s is %q, want %q", field, l[0], m.VolumeOptions.Labels[l[0]], l[1]))
				}
			}
		case spec.Deps != "" && m.Type == "volume" && m.Target == DepsDir && m.Source == spec.Deps && m.ReadOnly:
		default:
			bad = append(bad, fmt.Sprintf("%s is %s %q at %q (read-only %v), which was not asked for", field, m.Type, m.Source, m.Target, m.ReadOnly))
		}
	}
	var grade, deps int
	for _, m := range c.Mounts {
		switch {
		case m.Type == "volume" && m.Destination == GradeDir && m.RW && m.Name != "" && m.Driver == "local":
			grade++
		case spec.Deps != "" && m.Type == "volume" && m.Destination == DepsDir && !m.RW && m.Name == spec.Deps && m.Driver == "local":
			deps++
		default:
			bad = append(bad, fmt.Sprintf("Mounts has %s %q at %q (rw %v, driver %q), which was not asked for", m.Type, m.Name, m.Destination, m.RW, m.Driver))
		}
	}
	if grade != 1 || deps != wantAsked-1 {
		bad = append(bad, fmt.Sprintf("Mounts has %d grade and %d deps volumes, want 1 and %d", grade, deps, wantAsked-1))
	}
	return bad
}

// envDiff names the variables have adds, drops or changes against want, or says only the order differs; never values.
func envDiff(have, want []string) string {
	index := func(env []string) map[string]string {
		m := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}
	h, w := index(have), index(want)
	var added, dropped, changed []string
	for k, v := range h {
		if wv, ok := w[k]; !ok {
			added = append(added, k)
		} else if wv != v {
			changed = append(changed, k)
		}
	}
	for k := range w {
		if _, ok := h[k]; !ok {
			dropped = append(dropped, k)
		}
	}
	sort.Strings(added)
	sort.Strings(dropped)
	sort.Strings(changed)
	if len(added)+len(dropped)+len(changed) == 0 {
		return "the same variables, in another order or repeated"
	}
	return fmt.Sprintf("added %v, dropped %v, changed %v", added, dropped, changed)
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
