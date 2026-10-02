package sandbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/buildtool"
)

// Version names the grading profile's rules. Records and locks name it (the grader mode): a change to the rules that
// can turn a grade's result is a new version, so tasks validated under the old one are validated again.
const Version = "sandbox-v1"

// Profile is one grade's seatbelt profile: deny by default, modeled on Claude Code 2.1.285's own profile (its process,
// sysctl and device rules) but narrower, and written by Agentium so it is versioned and reviewable (the isolation
// plan, step 0). What it allows:
//   - processes: exec and fork; process information, signals and task ports only within the same sandbox;
//   - Mach services: name lookups, logging and notifications only (machServices): no security server, so no keychain;
//   - network: none, or loopback only (Loopback);
//   - reads: everything except the data folder, Denied and the credential stores, each in every form (Forms); then the
//     writable folders and Deps are readable again, and whatever denied path lies inside them is denied again
//     (Deps' private folders among them: buildtool.DepsDenied);
//   - writes: only Copy, Cache, Temp, /dev/null, and the file descriptors the grade already holds open for writing
//     (/dev/fd/<n>: its standard output and error, which /dev/stdout and /dev/stderr resolve to);
//   - POSIX IPC: semaphores only under Python multiprocessing's prefix (semaphorePrefix); no shared memory, except
//     reading the system's notification state (notifyMemory).
//
// Every deny, the default and the explicit ones, carries Tag as its message, so each denial the kernel logs names the
// grade (LogPredicate).
//
// Each grade renders its own profile, with its own Tag, file, cache and temp root: two grades never share one.
type Profile struct {
	// Tag is unique to the grade. The profile's default deny carries it, `(deny default (with message "<tag>"))`, so
	// the grade's denials can be told apart in the unified log (LogPredicate). NewTag makes one.
	Tag string
	// Home is the user's home folder, and AccountHome the account's home in the user database when HOME is redirected
	// (see CredentialPaths); Environ is the user's environment (MovedCredentials).
	Home        string
	AccountHome string
	Environ     []string
	// Data is Agentium's data folder, denied whole (hidden tests, other runs, the shared caches, the database).
	Data string
	// Denied are further paths the grade may not read or write: the run's denied paths (the user's repository and its
	// worktrees, the user's build caches, other runs' temp roots). Each is denied in every form.
	Denied []string
	// Copy is the grading copy, Cache the grade's own build cache, Temp its temp root: the only folders it writes.
	// They must exist when the profile is written (WriteFile), so their real forms are known. Cache may be empty.
	Copy  string
	Cache string
	Temp  string
	// Deps is the project's warmed dependencies, read-only; its private folders (buildtool.DepsDenied) stay denied.
	// Empty: none.
	Deps string
	// Loopback lets the grade bind, accept and connect on localhost (any port), which Gradle and tests that start a
	// local server need. Known limit: seatbelt's "localhost" in these rules is every address of this machine, not only
	// loopback, and no narrower rule exists (the isolation plan's step 0 tried the alternatives). So a process can
	// listen on the wildcard address or the machine's network address and accept connections from the network, and
	// can connect to any service listening on any of the machine's addresses. Outbound connections to other hosts stay
	// refused: a hostile build can serve the network but not reach it directly; the firewall and NAT are the guard.
	// Claude Code's allowLocalBinding allows the same and more.
	//
	// That reaches every service listening on this machine, not only the grade's own: a database, a dev server, other
	// grades running at the same time, and, where they run, a local HTTP or SOCKS proxy (which is internet access), a
	// browser's remote debugging port, or Docker's TCP socket. Unix sockets (Docker's default, the ssh agent's) stay
	// denied.
	Loopback bool
}

// tagPattern is what a tag may hold: it is written into the profile and matched in log predicates, so no quoting.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// NewTag returns a fresh grade tag: "agentium-" and 16 random hex digits.
func NewTag() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("sandbox tag: %w", err)
	}
	return "agentium-" + hex.EncodeToString(b), nil
}

// LogPredicate selects a grade's denials in the unified log (`/usr/bin/log show --predicate` or `log stream`, no
// administrator rights needed): the kernel writes "Sandbox: <process>(<pid>) deny(1) <operation> <path>" and then the
// profile's message, the tag. Repeats are merged and rate-limited, so counts are lower bounds.
func LogPredicate(tag string) string {
	return `eventMessage ENDSWITH "` + tag + `"`
}

// machServices are the Mach services a grade may look up: user and group names (opendirectoryd), the per-user temp
// folder (dirhelper), logging and notifications. Claude Code 2.1.285's profile also allows the security server
// (com.apple.SecurityServer, com.apple.securityd.xpc: the keychain), launch services, fonts, audio, power and
// distributed notifications; none of the step 0 fixtures (Go, Maven, Gradle, Cargo, pytest) needed them.
func machServices() []string {
	return []string{"com.apple.bsd.dirhelper", "com.apple.logd", "com.apple.system.logger", "com.apple.system.notification_center",
		"com.apple.system.opendirectoryd.libinfo", "com.apple.system.opendirectoryd.membership"}
}

// sysctlNames and sysctlPrefixes are the kernel values a grade may read: Claude Code 2.1.285's list (hardware,
// kernel version and limits, process lists, CPU features), which runtimes probe at start.
func sysctlNames() []string {
	return []string{"hw.activecpu", "hw.busfrequency_compat", "hw.byteorder", "hw.cacheconfig", "hw.cachelinesize_compat",
		"hw.cpufamily", "hw.cpufrequency", "hw.cpufrequency_compat", "hw.cputype", "hw.l1dcachesize_compat",
		"hw.l1icachesize_compat", "hw.l2cachesize_compat", "hw.l3cachesize_compat", "hw.logicalcpu", "hw.logicalcpu_max",
		"hw.machine", "hw.memsize", "hw.ncpu", "hw.nperflevels", "hw.packages", "hw.pagesize_compat", "hw.pagesize",
		"hw.physicalcpu", "hw.physicalcpu_max", "hw.tbfrequency_compat", "hw.vectorunit", "kern.argmax", "kern.bootargs",
		"kern.hostname", "kern.maxfiles", "kern.maxfilesperproc", "kern.maxproc", "kern.ngroups", "kern.osproductversion",
		"kern.osrelease", "kern.ostype", "kern.osvariant_status", "kern.osversion", "kern.secure_kernel",
		"kern.tcsm_available", "kern.tcsm_enable", "kern.usrstack64", "kern.version", "kern.willshutdown",
		"machdep.cpu.brand_string", "machdep.ptrauth_enabled", "security.mac.lockdown_mode_state", "sysctl.proc_cputype",
		"vm.loadavg"}
}

func sysctlPrefixes() []string {
	return []string{"hw.optional.arm", "hw.optional.arm.", "hw.optional.armv8_", "hw.perflevel", "kern.proc.all",
		"kern.proc.pgrp.", "kern.proc.pid.", "machdep.cpu.", "net.routetable."}
}

// ioctlDevices are the devices a grade may control. Unlike Claude Code's list, /dev/tty is not among them: a grade
// never needs the terminal, and an ioctl there (TIOCSTI) could type into the user's shell.
func ioctlDevices() []string {
	return []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/dtracehelper"}
}

// writableDevices are the devices a grade may write besides its folders (not /dev/tty, as above). /dev/stdout and
// /dev/stderr are not listed: the sandbox matches the path they resolve to, /dev/fd/<n>, which fdRule allows.
func writableDevices() []string {
	return []string{"/dev/null"}
}

// fdRule lets the grade write through /dev/fd/<n> (what /dev/stdout, /dev/stderr and `tee /dev/stderr` open): the
// kernel reopens a descriptor the process already holds, and refuses to open one held read-only for writing, so this
// reaches no file the grade could not write already. A build's `>/dev/stderr` would fail the grade without it.
const fdRule = `(allow file-write-data (regex #"^/dev/fd/[0-9]+$"))` + "\n"

// semaphorePrefix is the only name prefix of POSIX semaphores a grade may use: Python multiprocessing's (its locks,
// queues and pools; the names are /mp-<random>). Named POSIX shared memory is denied: an object outlives the grade and
// any later process can open it by name (an agent's sandbox allows that), so a hostile build could leave the hidden
// tests there. No step 0 fixture needs it (Python's shared_memory module does: /psm_<random>, refused).
//
// Known limit: semaphores under the prefix are a channel out of the grade. A build can create /mp-<name> semaphores
// and encode data in their counts (about 15 bits each, up to kern.posix.sem.max, 10,000 by default: some 10-20 KB),
// and they last until reboot; any later process that knows the names reads them back, an agent's included. Since the
// hostile build and a later agent can be the same model, it needs no guessing, only the model colluding with itself
// across runs. Python multiprocessing needs the prefix, so it stays; container mode (Part 2) closes the channel.
const semaphorePrefix = "/mp-"

// notifyMemory is the shared memory notifyd publishes its state in, which every process's notification calls read
// (one denial per process otherwise). notifyd creates it; the grade may only read it, so it carries nothing out.
const notifyMemory = "apple.shm.notification_center"

// systemFolders are folders no writable folder may be or hold (defense in depth: the other checks cover the home
// folder, the data folder, the deps and the credential stores).
func systemFolders() []string {
	return []string{"/tmp", "/private/tmp", "/private", "/var", "/private/var", "/etc", "/usr", "/bin", "/sbin", "/opt",
		"/Library", "/System", "/Applications", "/Users"}
}

// header is the fixed part of every profile, after the default deny.
const header = `
; Processes: Claude Code 2.1.285's rules. Signals, task ports and process information reach only this sandbox.
(allow process-exec)
(allow process-fork)
(allow process-info* (target same-sandbox))
(allow signal (target same-sandbox))
(allow mach-priv-task-port (target same-sandbox))
(allow user-preference-read)
(allow iokit-get-properties)
(allow system-socket (require-all (socket-domain AF_SYSTEM) (socket-protocol 2)))
`

// Render returns the profile's text. It checks the profile (see check) and resolves each path's real form, which needs
// the file system; nothing is written.
func (p Profile) Render() (string, error) {
	if err := p.check(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("(version 1)\n")
	fmt.Fprintf(&b, "; Agentium grading sandbox %s: deny by default; each denial is logged with this grade's tag.\n", Version)
	b.WriteString("(deny default (with message " + quote(p.Tag) + "))\n")
	b.WriteString(header)
	b.WriteString("\n; POSIX IPC: semaphores under Python multiprocessing's prefix only; no shared memory but the system's\n" +
		"; notification state, read-only.\n" +
		"(allow ipc-posix-sem (ipc-posix-name-prefix " + quote(semaphorePrefix) + "))\n" +
		"(allow ipc-posix-shm-read-data (ipc-posix-name " + quote(notifyMemory) + "))\n")

	b.WriteString("\n; Mach services: no security server (the keychain), launch services, fonts, audio or power.\n(allow mach-lookup")
	for _, name := range machServices() {
		b.WriteString("\n  (global-name " + quote(name) + ")")
	}
	b.WriteString(")\n\n(allow sysctl-read")
	for _, name := range sysctlNames() {
		b.WriteString("\n  (sysctl-name " + quote(name) + ")")
	}
	for _, prefix := range sysctlPrefixes() {
		b.WriteString("\n  (sysctl-name-prefix " + quote(prefix) + ")")
	}
	b.WriteString(")\n(allow sysctl-write (sysctl-name \"kern.tcsm_enable\"))\n")

	b.WriteString("\n; Devices: not the terminal.\n")
	rule(&b, "allow file-ioctl", "literal", ioctlDevices())
	b.WriteString("(allow file-ioctl file-read-data file-write-data (require-all (literal \"/dev/null\") (vnode-type CHARACTER-DEVICE)))\n")

	if p.Loopback {
		b.WriteString("\n; Network: this machine only. \"localhost\" matches every address of the machine, so a process can also\n" +
			"; listen on its network address (Profile.Loopback); outbound connections reach no other host.\n" +
			"(allow network-bind (local ip \"localhost:*\"))\n" +
			"(allow network-inbound (local ip \"localhost:*\"))\n" +
			"(allow network-outbound (remote ip \"localhost:*\"))\n")
	} else {
		b.WriteString("\n; Network: none.\n")
	}

	denied := p.denied()
	readable := p.readable()
	writable := p.writable()
	b.WriteString("\n; Reads: everything but the data folder, the run's denied paths and the credential stores; then the grade's\n" +
		"; folders and the dependencies; then what is denied inside those again.\n(allow file-read*)\n")
	tagged(&b, "deny file-read*", "subpath", denied, p.Tag)
	rule(&b, "allow file-read*", "subpath", readable)
	tagged(&b, "deny file-read*", "subpath", inside(denied, readable), p.Tag)
	rule(&b, "allow file-read-metadata", "literal", ancestors(readable, denied))

	b.WriteString("\n; Writes: the grading copy, the grade's cache and temp root, and the output devices only.\n")
	rule(&b, "allow file-write*", "subpath", writable)
	rule(&b, "allow file-write*", "literal", writableDevices())
	b.WriteString(fdRule)
	tagged(&b, "deny file-write*", "subpath", inside(denied, writable), p.Tag)
	return b.String(), nil
}

// rule writes (<action> (<filter> "<path>") ...) for paths; nothing when there are none. Paths were checked by check.
func rule(b *strings.Builder, action, filter string, paths []string) {
	tagged(b, action, filter, paths, "")
}

// tagged is rule with the grade's tag as the rule's message (when tag is set): only the default deny carries one
// otherwise, so a denial by an explicit rule (a credential store, the data folder) would reach the log untagged.
func tagged(b *strings.Builder, action, filter string, paths []string, tag string) {
	if len(paths) == 0 {
		return
	}
	b.WriteString("(" + action)
	for _, path := range paths {
		b.WriteString("\n  (" + filter + " " + quote(path) + ")")
	}
	if tag != "" {
		b.WriteString("\n  (with message " + quote(tag) + ")")
	}
	b.WriteString(")\n")
}

// quote is s as a profile string literal: backslashes and double quotes escaped. check refuses control characters.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// denied is every path the grade may not read, in every form: the data folder, Denied, the credential stores, and
// Deps' private folders.
func (p Profile) denied() []string {
	paths := []string{p.Data}
	paths = append(paths, p.Denied...)
	paths = append(paths, p.credentials()...)
	if p.Deps != "" {
		paths = append(paths, buildtool.DepsDenied(p.Deps)...)
	}
	return WithForms(paths)
}

// credentials are the credential stores the grade may not read: every sandbox's (CredentialPaths, MovedCredentials)
// and the grader's own additions (graderCredentialFiles).
func (p Profile) credentials() []string {
	paths := CredentialPaths(p.Home, p.AccountHome)
	paths = append(paths, MovedCredentials(p.Environ, p.Home)...)
	for _, name := range graderCredentialFiles() {
		paths = append(paths, filepath.Join(p.Home, name))
	}
	return paths
}

// writable are the folders the grade writes, in every form.
func (p Profile) writable() []string {
	return WithForms(nonEmpty(p.Copy, p.Cache, p.Temp))
}

// readable are the folders the grade reads again inside the denied ones: its writable folders and Deps.
func (p Profile) readable() []string {
	return WithForms(nonEmpty(p.Copy, p.Cache, p.Temp, p.Deps))
}

func nonEmpty(paths ...string) []string {
	return slices.DeleteFunc(paths, func(p string) bool { return p == "" })
}

// inside are the paths of denied that lie strictly inside one of roots: denied again after roots are allowed, since
// the last matching rule wins.
func inside(denied, roots []string) []string {
	var out []string
	for _, d := range denied {
		if slices.ContainsFunc(roots, func(root string) bool { return d != root && within(d, root) }) {
			out = append(out, d)
		}
	}
	return out
}

// ancestors are the folders above roots that lie in a denied path, each once: their own metadata (a literal, not
// their contents) is readable again, so tools that look up the folders above the working directory still work.
func ancestors(roots, denied []string) []string {
	var out []string
	for _, root := range roots {
		for dir := filepath.Dir(root); ; dir = filepath.Dir(dir) {
			if !slices.Contains(out, dir) && slices.ContainsFunc(denied, func(d string) bool { return within(dir, d) }) {
				out = append(out, dir)
			}
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	return out
}

// within reports whether p is root or lies inside it (both clean).
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// check refuses a profile that would not hold: a bad tag; a relative, empty or unprintable path (a moved credential
// store's among them); a writable folder
// that is or holds the home folder, the data folder, Deps, a credential store or a system folder (systemFolders), or
// lies inside Deps or a credential store (the build could write them); an empty denied path. Everything is compared in every form.
func (p Profile) check() error {
	if !tagPattern.MatchString(p.Tag) {
		return fmt.Errorf("sandbox tag %q: want letters, digits, dots, dashes and underscores, up to 64", p.Tag)
	}
	required := map[string]string{"home folder": p.Home, "data folder": p.Data, "grading copy": p.Copy, "temp root": p.Temp}
	for _, name := range []string{"home folder", "data folder", "grading copy", "temp root"} {
		if required[name] == "" {
			return fmt.Errorf("a sandbox profile needs its %s", name)
		}
	}
	// The moved credential stores come from the environment: one that cannot be written into a profile refuses it,
	// rather than leaving the store readable.
	all := append([]string{p.Home, p.AccountHome, p.Data, p.Copy, p.Cache, p.Temp, p.Deps}, p.Denied...)
	for _, path := range p.Denied {
		if path == "" {
			return errors.New("an empty sandbox denied path") // it would render as the current folder, denying nothing
		}
	}
	for _, path := range append(all, MovedCredentials(p.Environ, p.Home)...) {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("sandbox path %q is not absolute", path)
		}
		if strings.ContainsFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("sandbox path %q holds a control character", path)
		}
	}
	guarded := WithForms(append(append(nonEmpty(p.Home, p.AccountHome, p.Data, p.Deps), p.credentials()...), systemFolders()...))
	stores := WithForms(append(nonEmpty(p.Deps), p.credentials()...))
	for _, root := range p.writable() {
		for _, g := range guarded {
			if within(g, root) {
				return fmt.Errorf("the sandbox's writable folder %s holds %s", root, g)
			}
		}
		for _, s := range stores {
			if within(root, s) {
				return fmt.Errorf("the sandbox's writable folder %s lies inside %s", root, s)
			}
		}
	}
	return nil
}

// WriteFile renders the profile and writes it to path, a new file readable by its owner only, and returns the text's
// SHA-256 (hex). The grade's folders must exist by now, so their real forms are the ones written. path must lie outside
// every folder the profile lets the grade write: each of the grade's commands starts sandbox-exec on the file again,
// and a build that could rewrite it would run the next command unsandboxed. An existing file is never replaced.
func (p Profile) WriteFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("sandbox profile file %q is not absolute", path)
	}
	for _, dir := range nonEmpty(p.Copy, p.Cache, p.Temp) {
		info, err := os.Stat(dir)
		if err != nil {
			return "", fmt.Errorf("sandbox folder: %w", err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("sandbox folder %s is not a folder", dir)
		}
	}
	text, err := p.Render()
	if err != nil {
		return "", err
	}
	for _, form := range Forms(path) {
		for _, root := range p.writable() {
			if within(form, root) {
				return "", fmt.Errorf("sandbox profile file %s lies in the folder %s the sandbox writes", path, root)
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("sandbox profile file: %w", err)
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return "", fmt.Errorf("write sandbox profile %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write sandbox profile %s: %w", path, err)
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), nil
}
