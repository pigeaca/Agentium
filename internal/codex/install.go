package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/home"
)

// lockWait bounds LoginStatus's wait for the login's lock.
const lockWait = 30 * time.Second

// loginLock is the lock file of a ChatGPT login home: beside it, not in it. Runs (Command.Exclusive) and LoginStatus
// take it.
func loginLock(codexHome string) string { return filepath.Clean(codexHome) + ".lock" }

var versionPattern = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)

// Version runs the Codex CLI at cli with --version and returns its dotted version. Codex is run outside any repository
// with a home and a CODEX_HOME that do not exist, in a folder of its own that is removed afterwards: without CODEX_HOME
// it would write its helper links into the user's ~/.codex, which Agentium never touches. It gets Agentium's PATH (an
// npm-installed Codex is a Node script); the rest of the user's environment stays out.
func Version(ctx context.Context, cli string) (string, error) {
	out, _, err := runCLI(ctx, cli, "", []string{"--version"})
	if err != nil {
		return "", err
	}
	match := versionPattern.FindStringSubmatch(out)
	if match == nil {
		return "", fmt.Errorf("unrecognized output %q", strings.TrimSpace(out))
	}
	return match[1] + "." + match[2] + "." + match[3], nil
}

// CheckVersion refuses a Codex CLI other than the minor version Agentium verified (SupportedVersion): an older one is
// stale, and a newer one may change a default the isolation rests on.
func CheckVersion(version string) error {
	want := versionPattern.FindStringSubmatch(SupportedVersion)
	got := versionPattern.FindStringSubmatch(version)
	if got == nil {
		return fmt.Errorf("Codex reported no version Agentium can read (%q)", version)
	}
	if got[1] != want[1] || got[2] != want[2] {
		return fmt.Errorf("Codex %s is installed, but this Agentium runs Codex %s.%s only (checked against %s): install that version, or an Agentium that supports yours",
			version, want[1], want[2], SupportedVersion)
	}
	return nil
}

// ErrNotSignedIn is LoginStatus's error when Agentium's Codex home holds no ChatGPT login.
var ErrNotSignedIn = errors.New("Codex is not signed in to ChatGPT in Agentium's own Codex home")

// LoginStatus asks the Codex CLI at cli whether the Codex home codexHome holds a ChatGPT login (`codex login status`),
// and refuses otherwise: no login, a missing home, or an API key's login (runs in login mode force the ChatGPT one).
// Agentium never reads the home's auth.json itself, and never prints what the command says (an API key's login names
// part of the key). The error says how the user signs in. It holds the login's lock (Command.Exclusive) while it asks:
// reading the login may refresh its token, which must not race a run's refresh; a run holding it for longer than
// lockWait is an error, not a wait for the whole run.
func LoginStatus(ctx context.Context, cli, codexHome string) error {
	signIn := fmt.Sprintf("sign in once with `CODEX_HOME=%s codex login` (Agentium never runs it), or set CODEX_API_KEY", codexHome)
	if info, err := os.Stat(codexHome); err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s does not exist; %s", ErrNotSignedIn, codexHome, signIn)
	}
	lockCtx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	unlock, err := home.LockFile(lockCtx, loginLock(codexHome), nil)
	if err != nil {
		return fmt.Errorf("another Codex run is using the ChatGPT login in %s: try again once it ends (%w)", codexHome, err)
	}
	defer unlock()
	out, exit, err := runCLI(ctx, cli, codexHome, []string{"login", "status"})
	switch {
	case err != nil && exit == 0:
		return fmt.Errorf("codex login status: %w", err)
	case exit != 0:
		return fmt.Errorf("%w (%s); %s", ErrNotSignedIn, codexHome, signIn)
	case !strings.Contains(out, "ChatGPT"):
		return fmt.Errorf("%w: %s is signed in another way (an API key?); %s", ErrNotSignedIn, codexHome, signIn)
	}
	return nil
}

// runCLI runs the Codex CLI with args, outside any repository, and returns its output and exit code: standard output
// and standard error together (`login status` writes to standard error, --version to standard output). Its
// environment holds only PATH, a home that does not exist, and CODEX_HOME: codexHome, or (when empty) a folder that does
// not exist either, so the command writes nowhere. Errors are for a command that did not run, timed out or failed;
// exit is its exit code when it ran.
func runCLI(ctx context.Context, cli, codexHome string, args []string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "agentium-codex-")
	if err != nil {
		return "", 0, fmt.Errorf("a folder for the Codex CLI: %w", err)
	}
	defer os.RemoveAll(dir)
	if codexHome == "" {
		codexHome = filepath.Join(dir, "codex-home-absent")
	}
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(dir, "home-absent"), "CODEX_HOME=" + codexHome, "NO_COLOR=1"}
	cmd.WaitDelay = 2 * time.Second // a child that keeps stdout open cannot hold Output past the timeout
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stdout
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), exitErr.ExitCode(), err
	}
	if err != nil {
		return "", 0, fmt.Errorf("%s %s: %w", cli, strings.Join(args, " "), err)
	}
	return stdout.String(), 0, nil
}
