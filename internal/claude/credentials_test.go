package claude

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pigeaca/agentium/internal/sandbox"
)

// gh's config, which can hold a plain-text token, is denied where the user's environment moves it (GH_CONFIG_DIR,
// $XDG_CONFIG_HOME/gh), as ~/.config/gh is, in every project: in DeniedPaths and the sandbox's denyRead, each also in
// its real form when it lies behind a link, missing or not. A variable naming the home folder denies nothing of it.
func TestMovedGhConfigIsDenied(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "xdg"), 0o700); err != nil {
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
	environ := []string{"PATH=/usr/bin", "HOME=/home/u", "GH_CONFIG_DIR=" + filepath.Join(link, "ghconf"), "XDG_CONFIG_HOME=" + filepath.Join(link, "xdg")}
	for _, tools := range [][]string{nil, {"python"}, {"maven"}} {
		inv := toolInvocation(t, tools...)
		denied := inv.DeniedPaths(environ)
		_, settings := toolCommand(t, inv, environ)
		_, fs := sandboxOf(settings)
		denyRead := fs["denyRead"].([]any)
		for _, p := range []string{filepath.Join(link, "ghconf"), filepath.Join(realDir, "ghconf"), filepath.Join(link, "xdg", "gh"),
			filepath.Join(realDir, "xdg", "gh"), "/home/u/.config/gh"} {
			if !slices.Contains(denied, p) || !slices.Contains(denyRead, any(p)) {
				t.Errorf("%v: %s is not denied", tools, p)
			}
		}
	}
	for _, v := range []string{"GH_CONFIG_DIR=/home/u", "GH_CONFIG_DIR=/home", "GH_CONFIG_DIR=relative/gh"} {
		got := sandbox.MovedCredentials([]string{v}, "/home/u")
		if len(got) != 0 {
			t.Errorf("%s denies %q", v, got)
		}
	}
}
