package claude

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The macOS keychains are denied to every agent, whatever the project's build tools and the sign-in mode: Claude
// Code's sandbox lets the agent's shell reach the security server, so the login keychain's folder (and the System
// keychain's) must be unreadable there. Each is in DeniedPaths, the sandbox's denyRead, the Read tool's rules and the
// sandbox's denied credential files, the login keychain's folder also in its real form when the home folder lies
// behind a link.
func TestKeychainsAreDeniedToEveryAgent(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "home", "Library", "Keychains"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realDir, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(link, "home")
	login := filepath.Join(home, "Library", "Keychains")
	environ := []string{"PATH=/usr/bin", "HOME=" + home}
	for _, tools := range [][]string{nil, {"go"}, {"maven"}, {"gradle"}, {"cargo"}, {"python"}} {
		for _, mode := range []string{SignInLogin, SignInAPIKey, SignInTokenFile} {
			inv := toolInvocation(t, tools...)
			inv.Home, inv.SignIn = home, mode
			if mode != SignInLogin {
				inv.Secret, inv.ConfigDir = "S", "/work/runs/r1/config"
			}
			denied := inv.DeniedPaths(environ)
			_, settings := toolCommand(t, inv, environ)
			_, fs := sandbox(settings)
			denyRead := toStrings(fs["denyRead"])
			rules := toStrings(settings["permissions"].(map[string]any)["deny"])
			for _, p := range []string{login, filepath.Join(realDir, "home", "Library", "Keychains"), "/Library/Keychains"} {
				if !slices.Contains(denied, p) || !slices.Contains(denyRead, p) || !slices.Contains(rules, "Read(/"+p+"/**)") {
					t.Errorf("%v, %s: %s is not denied (DeniedPaths, denyRead and the Read rule)", tools, mode, p)
				}
			}
			var files []string
			for _, f := range settings["sandbox"].(map[string]any)["credentials"].(map[string]any)["files"].([]any) {
				if f := f.(map[string]any); f["mode"] == "deny" {
					files = append(files, f["path"].(string))
				}
			}
			for _, p := range []string{login, "/Library/Keychains"} {
				if !slices.Contains(files, p) {
					t.Errorf("%v, %s: %s is not a denied credential file", tools, mode, p)
				}
			}
		}
	}
}

// The deny holds where it matters, on this Mac's own login keychain, without reading any of it. The run's sandbox
// settings become a sandbox-exec profile the way the isolation spike built one: everything allowed, the security
// server's Mach lookups among it (as Claude Code's profile allows them), and each denyRead path denied as a subpath, as
// Claude Code renders it. Inside, the login keychain's folder cannot be listed, and `security list-keychains` no longer
// names the login keychain; outside, both do, so the test sees a difference this machine can show. Only exit statuses
// and whether the search list names the login keychain are looked at; no item is searched for or read.
func TestKeychainDenyHoldsInASandbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the macOS sandbox and keychain")
	}
	if testing.Short() {
		t.Skip("starts sandboxed processes")
	}
	sandboxExec, security := "/usr/bin/sandbox-exec", "/usr/bin/security"
	for _, tool := range []string{sandboxExec, security} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("%s: %v", tool, err)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	keychains := filepath.Join(home, "Library", "Keychains")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// run is a probe's exit status (-1 when it could not start) and whether its output names the login keychain.
	run := func(args ...string) (int, bool) {
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdout, cmd.Stderr = &out, nil
		err := cmd.Run()
		lists := strings.Contains(out.String(), "/Library/Keychains/login.keychain")
		out.Reset() // never kept or logged
		if err == nil {
			return 0, lists
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), lists
		}
		return -1, lists
	}
	if code, _ := run("/bin/ls", keychains); code != 0 {
		t.Skipf("this account's keychain folder cannot be listed outside a sandbox (exit %d)", code)
	}
	if _, lists := run(security, "list-keychains", "-d", "user"); !lists {
		t.Skip("this account's search list has no login keychain")
	}

	inv := toolInvocation(t)
	inv.Home = home
	environ := []string{"PATH=/usr/bin:/bin", "HOME=" + home}
	_, settings := toolCommand(t, inv, environ)
	_, fs := sandbox(settings)
	var profile strings.Builder
	profile.WriteString("(version 1)\n(allow default)\n")
	for _, p := range toStrings(fs["denyRead"]) {
		profile.WriteString("(deny file-read* (subpath " + strconv.Quote(p) + "))\n")
	}
	sandboxed := func(args ...string) (int, bool) {
		return run(append([]string{sandboxExec, "-p", profile.String()}, args...)...)
	}
	// A nested sandbox (these tests run inside an agent's) or a profile the system rejects exits 65 before the probe.
	if code, _ := sandboxed("/usr/bin/true"); code != 0 {
		t.Skipf("sandbox-exec cannot run a profile here (exit %d)", code)
	}
	if code, _ := sandboxed("/bin/ls", keychains); code == 0 {
		t.Error("the agent's shell can list the login keychain's folder")
	}
	code, lists := sandboxed(security, "list-keychains", "-d", "user")
	if lists {
		t.Errorf("the agent's shell still has the login keychain in its search list (exit %d)", code)
	}
}
