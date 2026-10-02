// Package ghx talks to GitHub for Agentium's pull-request cost screen, through the GitHub CLI (gh) the user installed
// and logged in to. It finds the open pull request of a commit, keeps one marked comment on it up to date, and sets a
// commit status. It never installs gh and adds no module.
//
// Where it runs: only in the user's own Agentium process (a command that the user, a hook or an AI calls), with the
// user's environment and gh login, never inside a run or an agent's sandbox. The packages a run executes in must not
// import it (a test checks the import graph), and the agent's sandbox denies ~/.config/gh and drops GH_TOKEN anyway.
//
// What it trusts: the repository identity comes from the user's git remote (RepoOf), and commit IDs, pull request
// numbers and comment IDs are validated before they go into an API path. Everything GitHub returns (titles, comment
// bodies, logins) is data: it is parsed as JSON, compared, and never put on a command line or run. A comment's body is
// only the caller's text (Agentium's redacted report), sent to gh on stdin as a JSON request body, never as an argument.
// Every call runs through runner: an argument list (no shell), its own process group, a timeout.
package ghx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/pigeaca/agentium/internal/runner"
)

// DefaultTimeout bounds one gh call when the client sets none.
const DefaultTimeout = 30 * time.Second

// maxOutput bounds what one gh call may print (a paginated comment list included); more is an error, not a cut.
const maxOutput = 64 << 20

var (
	// ErrNoGH: gh is not installed, or not on PATH.
	ErrNoGH = errors.New("the GitHub CLI (gh) is not installed or not on PATH; install it (https://cli.github.com), then run `gh auth login`")
	// ErrNotLoggedIn: gh has no usable login for github.com (exit code 4, or HTTP 401).
	ErrNotLoggedIn = errors.New("the GitHub CLI (gh) is not logged in to github.com; run `gh auth login`")
	// ErrRateLimited: GitHub refused the call for a rate limit (HTTP 403 or 429 saying so); try again later.
	ErrRateLimited = errors.New("GitHub rate-limited this login; try again later")
	// ErrTimeout: a gh call ran past the client's timeout; its process group was killed.
	ErrTimeout = errors.New("the GitHub CLI (gh) timed out")
)

// Client runs the user's gh. The zero value is not usable: use New.
type Client struct {
	gh      string        // absolute path of gh
	environ []string      // gh's environment (Environ of the user's)
	timeout time.Duration // per call
}

// New finds gh on the PATH of environ (the user's environment, os.Environ()) and returns a client whose calls each
// end after timeout (DefaultTimeout when 0). It does not run gh: a missing login shows on the first call.
func New(environ []string, timeout time.Duration) (Client, error) {
	gh, err := lookPath("gh", environ)
	if err != nil {
		return Client{}, err
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return Client{gh: gh, environ: Environ(environ), timeout: timeout}, nil
}

// lookPath finds name in the absolute folders of environ's PATH (relative entries are skipped: they would resolve
// against wherever Agentium was started).
func lookPath(name string, environ []string) (string, error) {
	path := ""
	for _, kv := range environ {
		if value, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = value
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", ErrNoGH
}

// ghNames are the user's variables gh needs to find its login and reach GitHub: the home and config folders (gh reads
// $GH_CONFIG_DIR, else $XDG_CONFIG_HOME/gh, else ~/.config/gh; its token is there or in the macOS keychain, which gh
// reads through /usr/bin/security, hence PATH), locale, proxies and certificates.
// On Linux, gh's keyring is the Secret Service, reached over the session bus (DBUS_SESSION_BUS_ADDRESS, and
// XDG_RUNTIME_DIR where the bus socket lives): this is the user's own process, so both pass.
var ghNames = []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "TZ",
	"GH_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"}

// ghToken is gh's own, explicit token variable for github.com: a user who logs gh in this way (`GH_TOKEN=... gh`)
// keeps that login here. It is the only credential variable gh receives; runner's credential filter drops every other
// one. GITHUB_TOKEN, which gh also reads, is dropped: it is often exported for other tools or another account, and
// would make the screen post as that account instead of the user's gh login.
const ghToken = "GH_TOKEN"

// ghFixed make gh non-interactive and quiet, and keep it from sending anything but the call: no prompts, pager,
// colors, spinner, update checks or telemetry.
var ghFixed = []string{"GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "NO_COLOR=1", "CLICOLOR=0", "GH_SPINNER_DISABLED=1",
	"GH_NO_UPDATE_NOTIFIER=1", "GH_NO_EXTENSION_UPDATE_NOTIFIER=1", "GH_TELEMETRY=false", "DO_NOT_TRACK=1"}

// Environ is gh's environment, formed from the user's: an allowlist (ghNames and LC_*), which the shared credential
// filter applies to as everywhere, then GH_TOKEN when the user set it, then ghFixed. Everything else is dropped: other
// credentials (GITHUB_TOKEN, ANTHROPIC_API_KEY, SSH_AUTH_SOCK), GIT_*, AGENTIUM_*, and gh settings that would redirect
// or log a call (GH_HOST, GH_REPO, GH_DEBUG, GH_FORCE_TTY, GH_ENTERPRISE_TOKEN).
func Environ(environ []string) []string {
	out := runner.EnvPolicy{Allowlist: true, Names: ghNames, Prefixes: []string{"LC_"}}.Filter(environ)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if name == ghToken {
			out = append(out, kv)
		}
	}
	return append(out, ghFixed...)
}

// api runs `gh api --hostname github.com args...` with body (a JSON request) on stdin when it is not nil, and returns
// stdout. The command is an argument list: nothing passes through a shell.
func (c Client) api(ctx context.Context, body []byte, args ...string) ([]byte, error) {
	if c.gh == "" {
		return nil, errors.New("ghx: the client was not made with New")
	}
	argv := append([]string{c.gh, "api", "--hostname", Host}, args...)
	stdout := &capped{limit: maxOutput}
	var stderr capped
	stderr.limit = 64 << 10
	var stdin io.Reader
	if body != nil {
		stdin = bytes.NewReader(body)
	}
	// "/" as the folder: gh, given full API paths and a host, needs no repository, and never reads the user's.
	result, err := runner.Run(ctx, runner.Spec{Dir: "/", Args: argv, Environ: c.environ, Timeout: c.timeout,
		Output: stdout, Stderr: &stderr, Stdin: stdin})
	endpoint := endpointOf(args)
	switch {
	case err != nil:
		return nil, fmt.Errorf("gh api %s: %w", endpoint, err)
	case result.TimedOut:
		return nil, fmt.Errorf("gh api %s: %w after %s", endpoint, ErrTimeout, c.timeout)
	case result.ExitCode == 4 || strings.Contains(stderr.String(), "(HTTP 401)"):
		return nil, fmt.Errorf("gh api %s: %w", endpoint, ErrNotLoggedIn)
	case rateLimited(stderr.String()):
		return nil, fmt.Errorf("gh api %s: %w", endpoint, ErrRateLimited)
	case result.ExitCode != 0:
		return nil, &CallError{Endpoint: endpoint, ExitCode: result.ExitCode, Stderr: printable(stderr.String(), 400)}
	case stdout.over:
		return nil, fmt.Errorf("gh api %s: output over %d bytes", endpoint, maxOutput)
	}
	return stdout.Bytes(), nil
}

// rateLimited reports whether gh's message is GitHub's refusal for a rate limit, primary or secondary: an HTTP 403 or
// 429 whose message says "rate limit". Other 403s (no access) stay CallErrors.
func rateLimited(stderr string) bool {
	return (strings.Contains(stderr, "(HTTP 403)") || strings.Contains(stderr, "(HTTP 429)")) &&
		strings.Contains(strings.ToLower(stderr), "rate limit")
}

// CallError is a gh call that ran and failed (not a timeout or a missing login). Stderr is gh's message, cut short and
// stripped of control characters, since it may quote what GitHub returned.
type CallError struct {
	Endpoint string
	ExitCode int
	Stderr   string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("gh api %s: exit status %d: %s", e.Endpoint, e.ExitCode, e.Stderr)
}

// endpointOf is the API path among a call's arguments (the one starting with "repos/" or "user"), for errors.
func endpointOf(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "repos/") || a == "user" {
			return a
		}
	}
	return "?"
}

// capped is a buffer that keeps at most limit bytes and remembers whether more came. It never returns a write error,
// so gh is not killed by a broken pipe halfway through a write.
type capped struct {
	bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.Len(); len(p) > room {
		c.over = true
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

// printable is s without control characters (a terminal escape in text GitHub echoes cannot reach the user's
// terminal), on one line, cut to at most n runes.
func printable(s string, n int) string {
	var b strings.Builder
	count := 0
	for _, r := range strings.TrimSpace(s) {
		if count == n {
			b.WriteString("…")
			break
		}
		switch {
		case r == '\n' || r == '\t':
			r = ' '
		case unicode.IsControl(r) || r == unicode.ReplacementChar:
			continue
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// decodeAll decodes every JSON value in data into a list: gh --paginate prints one array per page, or one merged
// array, depending on its version, and both read the same here.
func decodeAll[T any](data []byte) ([]T, error) {
	var all []T
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var page []T
		if err := dec.Decode(&page); errors.Is(err, io.EOF) {
			return all, nil
		} else if err != nil {
			return nil, fmt.Errorf("read gh's JSON: %w", err)
		}
		all = append(all, page...)
	}
}

var shaPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// checkCommit refuses anything but a full lowercase commit ID (SHA-1 or SHA-256), so it cannot change an API path.
func checkCommit(sha string) error {
	if !shaPattern.MatchString(sha) {
		return fmt.Errorf("commit %q is not a full commit ID", sha)
	}
	return nil
}

// Login is the login of the account gh is signed in as on github.com (GET /user): the identity every comment and
// status is posted as, for the caller to show ("commenting as <login>") before it posts. Logins GitHub no longer
// issues (ending in a hyphen, or with "--") are refused: such an account cannot post (fail-closed).
func (c Client) Login(ctx context.Context) (string, error) {
	out, err := c.api(ctx, nil, "user")
	if err != nil {
		return "", err
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(out, &user); err != nil {
		return "", fmt.Errorf("read gh's JSON: %w", err)
	}
	if !ownerPattern.MatchString(user.Login) {
		return "", fmt.Errorf("gh returned no usable login for %s", Host)
	}
	return user.Login, nil
}
