//go:build darwin

package run

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/task"
)

// fakeClaudeName is the name the test binary runs under as the fake Claude Code of TestClaudeInvocationGolden (see
// TestMain): a link to the test binary, so the fake sees exactly the arguments, environment and working folder a run
// gives Claude Code, with nothing a shell would add (PWD, SHLVL).
const fakeClaudeName = "claude-golden-fake"

// goldenEvent is the transcript line the fake prints first: what it was given.
const goldenEvent = "agentium_golden"

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == fakeClaudeName {
		os.Exit(fakeClaude())
	}
	os.Exit(m.Run())
}

// fakeClaude prints what it was given as one transcript event (which Parse ignores), then a fair run's init and result.
func fakeClaude() int {
	cwd, err := os.Getwd() // PWD is not on the allowlist, so this is the folder itself, resolved
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	line, err := json.Marshal(map[string]any{"type": goldenEvent, "args": os.Args[1:], "env": os.Environ(), "cwd": cwd})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(string(line))
	fmt.Println(`{"type":"system","subtype":"init","claude_code_version":"2.1.285","model":"claude-sonnet-5","permissionMode":"acceptEdits","tools":["Bash"],"skills":[],"slash_commands":[]}`)
	fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.25,"num_turns":2,"duration_ms":1000,"modelUsage":{}}`)
	return 0
}

// TestClaudeInvocationGolden pins exactly what a run gives Claude Code, end to end through Once: its arguments, its
// environment (the allowlist), its settings (the sandbox, the denied paths, the credential denials, the permission
// rules), the folder it starts in and its working folders (--add-dir), and the session folder Agentium expects it to
// keep. It was recorded before the agent seam (internal/agent) and must not change with it, nor with any refactor: a
// change here is a change to what Claude Code runs with, a security change for the denied paths and the environment,
// to be checked line by line, never regenerated blindly.
//
// The cases: a task at the root and in a monorepo module (one and two folders deep), each sign-in mode, with and
// without a cost cap, default and explicit model and effort, and the user's own CLAUDE_CONFIG_DIR, CLAUDE_CODE_TMPDIR
// and XDG_RUNTIME_DIR. A run graded in the sandbox gives Claude Code exactly what a run graded on the host does
// (TestClaudeInvocationSameInTheSandbox). Machine-specific parts become placeholders; a path and its resolved form
// (/var and /private/var) collapse into one line, so the golden is the same on every Mac.
func TestClaudeInvocationGolden(t *testing.T) {
	var out strings.Builder
	for _, c := range claudeGoldenCases() {
		fmt.Fprintf(&out, "== %s\n%s", c.name, captureClaude(t, c, ""))
	}
	golden := filepath.Join("testdata", "claude-invocation.golden")
	if *update {
		must(t, os.WriteFile(golden, []byte(out.String()), 0o600))
	}
	want, err := os.ReadFile(golden)
	must(t, err)
	if out.String() != string(want) {
		t.Errorf("what Claude Code is given changed (rerun with -update only if that is intended, and review every line):\n%s", out.String())
	}
}

// A run graded in the sandbox gives Claude Code what one graded on the host does: the grader is Agentium's, after the
// agent.
func TestClaudeInvocationSameInTheSandbox(t *testing.T) {
	needSandbox(t)
	c := claudeGoldenCases()[0]
	host := captureClaude(t, c, task.GraderHost)
	sandboxed := captureClaude(t, c, task.GraderSandbox)
	if sandboxed != host {
		t.Errorf("graded in the sandbox, Claude Code is given:\n%s\nnot what a host-graded run gets:\n%s", sandboxed, host)
	}
}

type claudeGoldenCase struct {
	name          string
	module, decoy string
	signIn        string
	model, effort string
	budget        float64
	environ       []string // added to the user's environment
}

func claudeGoldenCases() []claudeGoldenCase {
	return []claudeGoldenCase{
		{name: "root task, login, claude-sonnet-5, the CLI's default effort, a $1 cap", signIn: claude.SignInLogin, model: "claude-sonnet-5", budget: 1},
		{name: "module task (svc), login, claude-opus-5-5, effort high, a $2.5 cap, the user's own config and temp folders",
			module: "svc", decoy: "decoy", signIn: claude.SignInLogin, model: "claude-opus-5-5", effort: "high", budget: 2.5,
			environ: []string{"CLAUDE_CONFIG_DIR=<HOME>/.claude-work", "CLAUDE_CODE_TMPDIR=/golden/cc-tmp", "XDG_RUNTIME_DIR=/golden/run"}},
		{name: "root task, API key, claude-sonnet-5, effort low, no cap", signIn: claude.SignInAPIKey, model: "claude-sonnet-5", effort: "low"},
		{name: "module task two folders deep (a/svc), token file, claude-sonnet-5, a $0.5 cap",
			module: "a/svc", decoy: "b/svc", signIn: claude.SignInTokenFile, model: "claude-sonnet-5", budget: 0.5},
	}
}

// claudeUser is the user's environment every case starts from: what the allowlist keeps, drops and replaces.
func claudeUser(home string) []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=" + home, "USER=u", "LOGNAME=u", "SHELL=/bin/zsh", "TMPDIR=/golden/tmp",
		"LANG=en_US.UTF-8", "LC_ALL=C", "TERM=xterm", "TZ=UTC", "GOPATH=/golden/go", "GOFLAGS=-mod=mod", "GOCACHE=/golden/gocache",
		"GOPROXY=https://proxy.golang.org", "JAVA_HOME=/golden/jdk", "CARGO_HOME=/golden/cargo", "RUSTC_WRAPPER=sccache",
		"XDG_CONFIG_HOME=/golden/xdg", "GH_CONFIG_DIR=/golden/gh", "NODE_OPTIONS=--max-old-space-size=4096", "HOMEBREW_PREFIX=/opt/homebrew",
		"HTTPS_PROXY=http://proxy:3128", "SSL_CERT_FILE=/golden/certs.pem", "PYTHONPATH=/golden/py", "PIP_INDEX_URL=https://pip.example/simple",
		"VIRTUAL_ENV=/golden/venv", "EDITOR=vim", "ANTHROPIC_API_KEY=parent-key", "CLAUDE_CODE_OAUTH_TOKEN=parent-token",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1", "OPENAI_API_KEY=parent-openai", "CODEX_HOME=/golden/codex", "GITHUB_TOKEN=parent-gh",
		"GIT_DIR=/golden/repo/.git", "AGENTIUM_HOME=/golden/data", "SSH_AUTH_SOCK=/golden/agent.sock", "AWS_SECRET_ACCESS_KEY=parent-aws"}
}

// captureClaude runs case c through Once with the fake Claude Code (grader: "" or a mode) and returns what the fake
// was given, normalized.
func captureClaude(t *testing.T, c claudeGoldenCase, grader string) string {
	t.Helper()
	decoy := c.decoy
	if decoy == "" {
		decoy = "decoy"
	}
	f := newModuleOnce(t, c.module, decoy, "")
	dir := filepath.Dir(f.env.Home)
	// A short folder of the test's own for the runs' temp roots (/tmp holds other runs' roots, which every run denies):
	// /private/tmp/gXXXX/ag-0123456789/claude-<uid> must fit Claude Code's 44 bytes (claude.TempRootFits).
	temps := shortTemp(t)
	t.Cleanup(func() { os.RemoveAll(temps) })
	f.env.Layout.Temp = temps
	fake := filepath.Join(dir, "bin", fakeClaudeName)
	must(t, os.MkdirAll(filepath.Dir(fake), 0o700))
	exe, err := os.Executable()
	must(t, err)
	must(t, os.Symlink(exe, fake))
	f.env.CLI = fake
	f.env.SignIn = c.signIn
	switch c.signIn {
	case claude.SignInAPIKey:
		f.env.Secret = "golden-api-key-not-real"
	case claude.SignInTokenFile:
		f.env.Secret = "golden-token-not-real"
		f.env.TokenFile = filepath.Join(f.env.Home, ".config", "agentium", "claude-oauth-token")
	}
	f.env.Grader = grader
	f.env.DenyExtra = []string{"/golden/predicted/other-run"}
	environ := claudeUser(f.env.Home)
	for _, kv := range c.environ {
		environ = append(environ, strings.ReplaceAll(kv, "<HOME>", f.env.Home))
	}
	f.env.Environ = environ
	f.spec.Model, f.spec.Effort, f.spec.BudgetUSD = c.model, c.effort, c.budget
	f.spec.Keep = true // the start folder stays, so the session folder is named after its real path, as during the run
	rec, err := Once(context.Background(), f.env, f.spec)
	if grader == task.GraderSandbox {
		skipLogBlind(t, err)
	}
	if err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	if rec.Outcome != "ok" { // a fair run (the outcome's value, so no refactor of where it is named touches this test)
		t.Fatalf("%s: outcome %s, notes %v", c.name, rec.Outcome, rec.Notes)
	}
	var given struct {
		Args []string `json:"args"`
		Env  []string `json:"env"`
		CWD  string   `json:"cwd"`
	}
	transcript, err := os.Open(filepath.Join(rec.RecordsDir, "stream.jsonl"))
	must(t, err)
	defer transcript.Close()
	scanner := bufio.NewScanner(transcript)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == goldenEvent {
			must(t, json.Unmarshal(scanner.Bytes(), &given))
		}
	}
	must(t, scanner.Err())
	if given.Args == nil {
		t.Fatalf("%s: the fake Claude Code did not report what it was given", c.name)
	}

	// The placeholders, longest first. The session folder is where Agentium expects Claude Code to keep the run's
	// session: under the active config folder, named after the start folder's real path.
	// The user's id in the names of Claude Code's shared temp folders (claude-<uid>, cc-socks-<uid>).
	uidName := regexp.MustCompile(`\b(claude|cc-socks|cc-daemon)-` + strconv.Itoa(os.Getuid()) + `\b`)
	runTemp := f.env.Layout.RunTemp(f.env.workspaceName())
	replace := map[string]string{runTemp: "<RUNTEMP>", realOf(runTemp): "<RUNTEMP>", temps: "<TEMPS>", realOf(temps): "<TEMPS>",
		dir: "<T>", realOf(dir): "<T>", sessionName(realOf(dir)): "<T-AS-SESSION-NAME>", f.env.ID: "<ID>", exe: "<TEST-BINARY>"}
	normalize := func(s string) string {
		keys := make([]string, 0, len(replace))
		for k := range replace {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return len(keys[i]) > len(keys[j]) || len(keys[i]) == len(keys[j]) && keys[i] < keys[j]
		})
		for _, k := range keys {
			s = strings.ReplaceAll(s, k, replace[k])
		}
		return uidName.ReplaceAllString(s, "${1}-<UID>")
	}

	var b strings.Builder
	start := f.env.Layout.Workspaces + "/" + f.env.workspaceName() + "/repo"
	if c.module != "" {
		start += "/" + c.module
	}
	activeConfig := filepath.Join(f.env.Layout.Workspaces, f.env.workspaceName(), "config")
	if c.signIn == claude.SignInLogin {
		activeConfig = claude.UserConfigDir(environ, f.env.Home)
	}
	fmt.Fprintf(&b, "-- working folder\n%s\n", normalize(given.CWD))
	fmt.Fprintf(&b, "-- session folder\n%s\n", normalize(claude.SessionFolder(activeConfig, start)))
	fmt.Fprintf(&b, "-- args\n")
	var settings string
	for i, a := range given.Args {
		switch {
		case i > 0 && given.Args[i-1] == "--settings":
			settings = a
			fmt.Fprintf(&b, "<the settings below>\n")
		case i > 0 && given.Args[i-1] == "-p":
			fmt.Fprintf(&b, "%s\n", normalize(strconv.Quote(a)))
		default:
			fmt.Fprintf(&b, "%s\n", normalize(a))
		}
	}
	fmt.Fprintf(&b, "-- settings\n%s\n", normalizeSettings(t, settings, normalize))
	fmt.Fprintf(&b, "-- env\n")
	for _, kv := range given.Env {
		fmt.Fprintf(&b, "%s\n", normalize(kv))
	}
	return b.String()
}

// shortTemp makes a folder /tmp/gXXXX (four hex digits) of the test's own.
func shortTemp(t *testing.T) string {
	t.Helper()
	for range 100 {
		suffix := make([]byte, 2)
		_, err := rand.Read(suffix)
		must(t, err)
		p := "/tmp/g" + hex.EncodeToString(suffix)
		if err := os.Mkdir(p, 0o700); err == nil {
			return p
		}
	}
	t.Fatal("no free short folder in /tmp")
	return ""
}

// realOf is p with its symbolic links resolved (p itself when that fails).
func realOf(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// sessionName is a path as Claude Code names a session folder after it (claude.SessionFolder): every character but
// letters and digits replaced by "-".
func sessionName(p string) string {
	return regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(p, "-")
}

// normalizeSettings indents the settings JSON with its strings normalized, and collapses a list's neighbors that
// normalize to the same line (a path and its resolved form).
func normalizeSettings(t *testing.T, raw string, normalize func(string) string) string {
	t.Helper()
	var v any
	must(t, json.Unmarshal([]byte(raw), &v))
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return normalize(x)
		case []any:
			var out []any
			for _, e := range x {
				e = walk(e)
				if s, ok := e.(string); ok && len(out) > 0 {
					if prev, ok := out[len(out)-1].(string); ok && prev == s {
						continue
					}
				}
				out = append(out, e)
			}
			if out == nil {
				out = []any{}
			}
			return out
		case map[string]any:
			for k, e := range x {
				x[k] = walk(e)
			}
			return x
		}
		return v
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // the placeholders' <>
	enc.SetIndent("", "  ")
	must(t, enc.Encode(walk(v)))
	return strings.TrimSuffix(b.String(), "\n")
}
