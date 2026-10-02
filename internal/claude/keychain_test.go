package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
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
			_, fs := sandboxOf(settings)
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

// HOME can be redirected while the account's login keychain stays in its real home folder, where an explicit path
// opens it. The account's folder (AccountHome) is then denied as well, in its real form too, in every list the
// redirected home's is; when it is the same folder as HOME, or unknown, nothing is added. A relative one is refused.
func TestAccountKeychainIsDeniedWhenHomeIsRedirected(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "Library", "Keychains"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "account")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realDir, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "redirected")
	environ := []string{"PATH=/usr/bin", "HOME=" + home}
	account := filepath.Join(link, "Library", "Keychains")
	for _, tools := range [][]string{nil, {"go"}, {"python"}} {
		inv := toolInvocation(t, tools...)
		inv.Home, inv.AccountHome = home, link
		denied := inv.DeniedPaths(environ)
		_, settings := toolCommand(t, inv, environ)
		_, fs := sandboxOf(settings)
		denyRead := toStrings(fs["denyRead"])
		rules := toStrings(settings["permissions"].(map[string]any)["deny"])
		for _, p := range []string{account, filepath.Join(realDir, "Library", "Keychains"), filepath.Join(home, "Library", "Keychains")} {
			if !slices.Contains(denied, p) || !slices.Contains(denyRead, p) || !slices.Contains(rules, "Read(/"+p+"/**)") {
				t.Errorf("%v: %s is not denied (DeniedPaths, denyRead and the Read rule)", tools, p)
			}
		}
		credentials, _ := json.Marshal(settings["sandbox"].(map[string]any)["credentials"])
		if !strings.Contains(string(credentials), `"path":"`+account+`"`) {
			t.Errorf("%v: %s is not a denied credential file", tools, account)
		}
		for _, p := range denied {
			if (strings.HasPrefix(p, link+"/") && p != account) || (strings.HasPrefix(p, realDir+"/") && !strings.HasSuffix(p, "/Library/Keychains")) {
				t.Errorf("%v: %s is denied: only the account's keychains are", tools, p)
			}
		}
	}
	for _, accountHome := range []string{"", home, home + "/"} {
		inv := toolInvocation(t)
		inv.Home, inv.AccountHome = home, accountHome
		with := inv.DeniedPaths(environ)
		inv.AccountHome = ""
		if without := inv.DeniedPaths(environ); !slices.Equal(with, without) {
			t.Errorf("AccountHome %q changes the denied paths: %q", accountHome, with)
		}
	}
	inv := toolInvocation(t)
	inv.AccountHome = "relative/home"
	if _, _, err := inv.Command(environ); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Errorf("a relative AccountHome: %v", err)
	}
}

// The deny holds where it matters, on this Mac's own login keychain, without reading any of it. The run's sandbox
// settings become a sandbox-exec profile the way the isolation spike built one: everything allowed, the security
// server's Mach lookups among it (as Claude Code's profile allows them), and each denyRead path denied as a subpath, as
// Claude Code renders it. An attacker names the keychain file by path, so inside, `security show-keychain-info` on the
// login keychain fails (the client cannot open the file: "Operation not permitted"), its folder cannot be listed, and
// `security list-keychains` still works but no longer names the login keychain. That holds with HOME the account's
// own and with HOME redirected elsewhere (Invocation.AccountHome). Outside, the same probes succeed, so the test sees a
// difference this machine can show. Only exit statuses and whether the search list names the login keychain are
// looked at; show-keychain-info reports lock settings, and no item is searched for or read.
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
	account, err := user.Current()
	if err != nil || !filepath.IsAbs(account.HomeDir) {
		t.Skipf("the account's home folder: %v", err)
	}
	keychains := filepath.Join(account.HomeDir, "Library", "Keychains")
	login := filepath.Join(keychains, "login.keychain-db")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// run is a probe's exit status (-1 when it could not start) and whether its output names the login keychain; the
	// probe's HOME is home.
	run := func(home string, args ...string) (int, bool) {
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "HOME=") }), "HOME="+home)
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
	if code, _ := run(account.HomeDir, "/bin/ls", keychains); code != 0 {
		t.Skipf("this account's keychain folder cannot be listed outside a sandbox (exit %d)", code)
	}
	if code, _ := run(account.HomeDir, security, "show-keychain-info", login); code != 0 {
		t.Skipf("this account's login keychain cannot be opened by path outside a sandbox (exit %d)", code)
	}
	if _, lists := run(account.HomeDir, security, "list-keychains", "-d", "user"); !lists {
		t.Skip("this account's search list has no login keychain")
	}
	// profile is the run's settings for inv as a sandbox-exec profile.
	profile := func(inv Invocation) string {
		_, settings := toolCommand(t, inv, []string{"PATH=/usr/bin:/bin", "HOME=" + inv.Home})
		_, fs := sandboxOf(settings)
		var b strings.Builder
		b.WriteString("(version 1)\n(allow default)\n")
		for _, p := range toStrings(fs["denyRead"]) {
			b.WriteString("(deny file-read* (subpath " + strconv.Quote(p) + "))\n")
		}
		return b.String()
	}
	sandboxed := func(profile, home string, args ...string) (int, bool) {
		return run(home, append([]string{sandboxExec, "-p", profile}, args...)...)
	}
	// A nested sandbox (these tests run inside an agent's) or a profile the system rejects exits 65 before the probe.
	if code, _ := sandboxed("(version 1)\n(allow default)\n", account.HomeDir, "/usr/bin/true"); code != 0 {
		t.Skipf("sandbox-exec cannot run a profile here (exit %d)", code)
	}

	redirected := t.TempDir()
	own := toolInvocation(t)
	own.Home = account.HomeDir
	moved := toolInvocation(t)
	moved.Home, moved.AccountHome = redirected, account.HomeDir
	for _, c := range []struct {
		name string
		inv  Invocation
	}{{"HOME is the account's", own}, {"HOME redirected", moved}} {
		p := profile(c.inv)
		if code, _ := sandboxed(p, c.inv.Home, "/bin/ls", keychains); code == 0 {
			t.Errorf("%s: the agent's shell can list the login keychain's folder", c.name)
		}
		if code, _ := sandboxed(p, c.inv.Home, security, "show-keychain-info", login); code == 0 {
			t.Errorf("%s: the agent's shell can open the login keychain by its path", c.name)
		}
		code, lists := sandboxed(p, c.inv.Home, security, "list-keychains", "-d", "user")
		if code != 0 {
			t.Errorf("%s: security list-keychains exits %d in the sandbox; the probe proves nothing", c.name, code)
		}
		if lists {
			t.Errorf("%s: the agent's shell still has the login keychain in its search list", c.name)
		}
	}
	// Without the account's home folder, a redirected HOME leaves the login keychain open by its path: the case above
	// tests AccountHome, not something else that denies it.
	moved.AccountHome = ""
	if code, _ := sandboxed(profile(moved), redirected, security, "show-keychain-info", login); code != 0 {
		t.Logf("with HOME redirected and no AccountHome the login keychain still does not open by path (exit %d)", code)
	}
}
