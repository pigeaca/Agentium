package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
)

// parentEnv is a user's environment with credentials and settings that must not reach a run: Codex's and OpenAI's
// own among them.
var parentEnv = []string{"PATH=/usr/bin:/bin", "HOME=/hm/u", "LANG=en_US.UTF-8", "SHELL=/bin/zsh", "TMPDIR=/var/folders/u/T",
	"ANTHROPIC_API_KEY=parent-anthropic", "OPENAI_API_KEY=parent-openai", "CODEX_API_KEY=parent-codex", "CODEX_HOME=/hm/u/.codex-work",
	"CODEX_SANDBOX=seatbelt", "CLAUDE_CONFIG_DIR=/hm/u/.claude-work", "GITHUB_TOKEN=parent-gh", "GIT_DIR=/hm/u/repo/.git",
	"AGENTIUM_HOME=/data", "SSH_AUTH_SOCK=/tmp/agent.sock", "AWS_SECRET_ACCESS_KEY=parent-aws", "DECOY_PASSWORD=parent-pw", "EDITOR=vim"}

// invocation is a Codex run as Once makes one: in its workspace under /data, with the shared login home or a key.
func invocation(signIn, secret string) agent.Invocation {
	inv := agent.Invocation{CLI: "/bin/codex", Dir: "/data/workspaces/r1/repo", Prompt: "Fix the parser.", Model: "gpt-6.1-sol",
		BudgetUSD: 3, SignIn: signIn, Secret: secret, Home: "/hm/u", ConfigDir: "/data/codex", State: "/data/workspaces/r1/agent",
		Records: "/data/records/r1", TempRoot: "/tmp/ag-0123456789", UID: 501, BuildCache: "/data/workspaces/r1/go-build",
		Deny: []string{"/data/projects", "/data/records", "/data/cache", "/users/repo"}}
	if signIn == SignInAPIKey {
		inv.ConfigDir = "/data/workspaces/r1/codex-home"
	}
	return inv
}

func command(t *testing.T, inv agent.Invocation) (agent.Command, map[string]string, map[string]string) {
	t.Helper()
	cmd, err := Adapter{}.Command(inv, parentEnv)
	if err != nil {
		t.Fatal(err)
	}
	overrides := map[string]string{}
	for i, a := range cmd.Args {
		if i > 0 && cmd.Args[i-1] == "-c" {
			key, value, _ := strings.Cut(a, "=")
			overrides[key] = value
		}
	}
	env := map[string]string{}
	for _, kv := range cmd.Env {
		name, value, _ := strings.Cut(kv, "=")
		if _, dup := env[name]; dup {
			t.Errorf("%s is set twice", name)
		}
		env[name] = value
	}
	return cmd, overrides, env
}

// The recipe the spike verified: exec --json with the prompt on stdin, the user's configuration ignored, every
// setting a -c override, the effort always passed, unbounded retries off, the final message into the records.
func TestCommandIsTheVerifiedRecipe(t *testing.T) {
	cmd, overrides, _ := command(t, invocation(SignInLogin, ""))
	if !slices.Equal(cmd.Args[:5], []string{"exec", "--json", "-m", "gpt-6.1-sol", "--ignore-user-config"}) || !slices.Contains(cmd.Args, "--ignore-rules") {
		t.Errorf("starts %q", cmd.Args[:5])
	}
	tail := cmd.Args[len(cmd.Args)-8:]
	if !slices.Equal(tail, []string{"--disable", "unbounded_connection_retries", "--ignore-rules", "-C", "/data/workspaces/r1/repo", "-o", "/data/records/r1/last-message.txt", "-"}) {
		t.Errorf("ends %q", tail)
	}
	if cmd.Stdin != "Fix the parser." || slices.Contains(cmd.Args, "Fix the parser.") {
		t.Errorf("the prompt goes on stdin only: stdin %q", cmd.Stdin)
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, "dangerously") || strings.Contains(a, "danger-full-access") {
			t.Errorf("a dangerous flag: %s", a)
		}
	}
	for key, want := range map[string]string{
		"model":                  `"gpt-6.1-sol"`, // as a setting too: it outranks a trusted project's model
		"model_reasoning_effort": `"low"`,         // the catalog's default, passed: the rollout records it only then
		"approval_policy":        `"never"`, "default_permissions": `"agentium"`, "forced_login_method": `"chatgpt"`,
		"history.persistence": `"none"`, "web_search": `"disabled"`, "check_for_update_on_startup": "false",
		"sqlite_home": `"/data/workspaces/r1/agent/sqlite"`, "log_dir": `"/data/workspaces/r1/agent/log"`,
		"allow_login_shell": "false",
		"shell_environment_policy.ignore_default_excludes": "false",
		"shell_environment_policy.exclude":                 `["CODEX_*","OPENAI_*"]`,
		"shell_environment_policy.set":                     `{"HOME"="/hm/u","TMPDIR"="/tmp/ag-0123456789","TMPPREFIX"="/tmp/ag-0123456789/zsh","ZDOTDIR"="/tmp/ag-0123456789/zdotdir"}`,
		"permissions.agentium.network":                     "{enabled=false}",
	} {
		if overrides[key] != want {
			t.Errorf("-c %s=%s, want %s", key, overrides[key], want)
		}
	}
	for _, off := range []string{"apps", "plugins", "memories", "hooks", "daemon_auto_start", "fast_mode", "unbounded_connection_retries", "image_generation", "computer_use"} {
		if !strings.Contains(overrides["features"], off+"=false") {
			t.Errorf("feature %s is not off: %s", off, overrides["features"])
		}
	}
	// Trust pinned (trusted: decision 7) in the table form, for the start folder (and the checkout); never the dotted form, which is ignored.
	if !strings.HasPrefix(overrides["projects"], `{"/data/workspaces/r1/repo"={trust_level="trusted"}`) {
		t.Errorf("projects %s", overrides["projects"])
	}
	for key := range overrides {
		if strings.HasPrefix(key, "projects.") || strings.HasPrefix(key, "include_permissions_instructions") || strings.HasPrefix(key, "skills.") {
			t.Errorf("-c %s: not in the recipe", key)
		}
	}
}

// The permission profile: the disk readable, the checkout, build cache and temp root writable, and every path the
// agent may not read denied (which denies writes too).
func TestPermissionProfile(t *testing.T) {
	inv := invocation(SignInLogin, "")
	_, overrides, _ := command(t, inv)
	table := overrides["permissions.agentium.filesystem"]
	if !strings.HasPrefix(table, `{":root"="read","/data/workspaces/r1/repo"="write","/data/workspaces/r1/go-build"="write","/tmp/ag-0123456789"="write"`) {
		t.Errorf("the profile starts %.200s", table)
	}
	for _, p := range (Adapter{}).DeniedPaths(inv, parentEnv) {
		if !strings.Contains(table, `"`+p+`"="deny"`) {
			t.Errorf("%s is not denied in the profile", p)
		}
	}
	if strings.Count(table, `="write"`) != 3+1 { // /tmp/ag-... also in its /private/tmp form
		t.Errorf("writable: %s", table)
	}
}

// Codex's own data, the user's skills, the shared login home, the run's state, and Agentium's other data are denied
// (that every path a Claude Code run denies is too: TestDeniedPathsCoverClaudeCodes).
func TestDeniedPathsCoverCodexs(t *testing.T) {
	inv := invocation(SignInLogin, "")
	denied := (Adapter{}).DeniedPaths(inv, parentEnv)
	for _, p := range []string{"/hm/u/.codex", "/hm/u/.codex-work", "/hm/u/.agents", "/hm/u/.agentium", "/data/codex",
		"/data/workspaces/r1/agent", "/hm/u/.claude", "/hm/u/.claude-work", "/hm/u/.claude.json", "/hm/u/.config/agentium",
		"/data/records", "/users/repo", "/tmp/claude-501"} {
		if !slices.ContainsFunc(denied, func(d string) bool { return within(p, d) }) {
			t.Errorf("%s is not denied", p)
		}
	}
}

// With the data folder at ~/.agentium (the default), the checkout lies in it: ~/.agentium itself is not denied (the
// run could not read its own checkout), but its parts are, the shared login home by name.
func TestDeniedPathsWithTheDataFolderAtHome(t *testing.T) {
	inv := invocation(SignInLogin, "")
	inv.Dir, inv.ConfigDir, inv.State = "/hm/u/.agentium/workspaces/r1/repo", "/hm/u/.agentium/codex", "/hm/u/.agentium/workspaces/r1/agent"
	inv.BuildCache, inv.Records, inv.Deny = "/hm/u/.agentium/workspaces/r1/go-build", "/hm/u/.agentium/records/r1", []string{"/hm/u/.agentium/records"}
	denied := (Adapter{}).DeniedPaths(inv, parentEnv)
	if slices.Contains(denied, "/hm/u/.agentium") {
		t.Error("the data folder that holds the checkout is denied whole")
	}
	for _, p := range []string{"/hm/u/.agentium/codex", "/hm/u/.agentium/records", "/hm/u/.agentium/workspaces/r1/agent"} {
		if !slices.Contains(denied, p) {
			t.Errorf("%s is not denied", p)
		}
	}
	if _, err := (Adapter{}).Command(inv, parentEnv); err != nil {
		t.Errorf("a run in the data folder at home: %v", err)
	}
}

// The environment: the allowlist, a run-local HOME, the run's temp root, CODEX_HOME, and with an API key the key, the
// only credential. No credential of the parent's, no Codex or OpenAI variable of its own, no GIT_* or AGENTIUM_*.
func TestEnvironment(t *testing.T) {
	_, overrides, env := command(t, invocation(SignInLogin, ""))
	want := map[string]string{"HOME": "/data/workspaces/r1/agent/home", "TMPDIR": "/tmp/ag-0123456789", "TMPPREFIX": "/tmp/ag-0123456789/zsh",
		"CODEX_HOME": "/data/codex", "PATH": "/usr/bin:/bin", "LANG": "en_US.UTF-8", "SHELL": "/bin/zsh"}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("%s=%q, want %q", name, env[name], value)
		}
	}
	for name := range env {
		if strings.HasPrefix(name, "OPENAI_") || strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "AGENTIUM_") ||
			strings.HasPrefix(name, "CLAUDE_") || strings.HasPrefix(name, "ANTHROPIC_") || name == "EDITOR" ||
			(strings.HasPrefix(name, "CODEX_") && name != "CODEX_HOME") {
			t.Errorf("%s reached Codex", name)
		}
	}
	if strings.Contains(overrides["shell_environment_policy.set"], "CODEX") {
		t.Errorf("the agent's shells are given a Codex variable: %s", overrides["shell_environment_policy.set"])
	}
	_, _, env = command(t, invocation(SignInAPIKey, "codex-key-not-real"))
	if env["CODEX_API_KEY"] != "codex-key-not-real" || env["CODEX_HOME"] != "/data/workspaces/r1/codex-home" {
		t.Errorf("API key mode: CODEX_API_KEY %q, CODEX_HOME %q", env["CODEX_API_KEY"], env["CODEX_HOME"])
	}
	for _, kv := range mapList(env) {
		if strings.Contains(kv, "parent-") {
			t.Errorf("a parent's credential reached Codex: %s", kv)
		}
	}
}

func mapList(env map[string]string) []string {
	var out []string
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// Login mode shares one home and takes turns; an API key run has a fresh home of its own and runs in parallel.
func TestLoginRunsTakeTurns(t *testing.T) {
	login, _, _ := command(t, invocation(SignInLogin, ""))
	if login.Exclusive != "/data/codex.lock" || slices.Contains(login.Dirs, "/data/codex") {
		t.Errorf("login: lock %q, dirs %q (the shared home is never made by a run)", login.Exclusive, login.Dirs)
	}
	key, _, _ := command(t, invocation(SignInAPIKey, "codex-key-not-real"))
	if key.Exclusive != "" || !slices.Contains(key.Dirs, "/data/workspaces/r1/codex-home") {
		t.Errorf("API key: lock %q, dirs %q", key.Exclusive, key.Dirs)
	}
	if !slices.Contains(key.Args, `forced_login_method="api"`) {
		t.Errorf("API key mode does not force the API sign-in: %q", key.Args)
	}
	if key.Watch == nil || login.Watch == nil {
		t.Error("a capped run has no watcher")
	}
	uncapped := invocation(SignInLogin, "")
	uncapped.BudgetUSD = 0
	if cmd, _, _ := command(t, uncapped); cmd.Watch != nil {
		t.Error("an uncapped run has a watcher")
	}
}

// What a run refuses before it starts.
func TestCommandRefusals(t *testing.T) {
	for name, change := range map[string]func(*agent.Invocation){
		"a login with a secret":           func(inv *agent.Invocation) { inv.Secret = "x" },
		"an unknown sign-in":              func(inv *agent.Invocation) { inv.SignIn = "token-file" },
		"a relative path":                 func(inv *agent.Invocation) { inv.Deny = append(inv.Deny, "data") },
		"no state folder":                 func(inv *agent.Invocation) { inv.State = "" },
		"no temp root":                    func(inv *agent.Invocation) { inv.TempRoot = "" },
		"a Gradle project":                func(inv *agent.Invocation) { inv.Tools, inv.AllowLocalBinding = []string{"gradle"}, true },
		"a cap below the allowance":       func(inv *agent.Invocation) { inv.BudgetUSD = 1 },
		"a capped run on an unpriced one": func(inv *agent.Invocation) { inv.Model, inv.Effort = "gpt-unknown", "high" },
		"an unknown model's default":      func(inv *agent.Invocation) { inv.Model, inv.BudgetUSD = "gpt-unknown", 0 },
		"a folder outside the checkout":   func(inv *agent.Invocation) { inv.Repo = "/elsewhere" },
	} {
		inv := invocation(SignInLogin, "")
		change(&inv)
		if _, err := (Adapter{}).Command(inv, parentEnv); err == nil {
			t.Errorf("%s was not refused", name)
		}
	}
	key := invocation(SignInAPIKey, "")
	if _, err := (Adapter{}).Command(key, parentEnv); err == nil {
		t.Error("an API key sign-in without the key was not refused")
	}
}

// A module run starts in the module's folder; the whole checkout is writable and pinned trusted.
func TestModuleRun(t *testing.T) {
	inv := invocation(SignInLogin, "")
	inv.Repo, inv.Dir = "/data/workspaces/r1/repo", "/data/workspaces/r1/repo/svc"
	cmd, overrides, _ := command(t, inv)
	if i := slices.Index(cmd.Args, "-C"); i < 0 || cmd.Args[i+1] != "/data/workspaces/r1/repo/svc" {
		t.Errorf("args %q", cmd.Args)
	}
	if !strings.Contains(overrides["permissions.agentium.filesystem"], `"/data/workspaces/r1/repo"="write"`) {
		t.Error("the checkout is not writable")
	}
	for _, p := range []string{"/data/workspaces/r1/repo/svc", "/data/workspaces/r1/repo"} {
		if !strings.Contains(overrides["projects"], `"`+p+`"={trust_level="trusted"}`) {
			t.Errorf("%s is not pinned trusted: %s", p, overrides["projects"])
		}
	}
}

// Every value built from a path is a TOML basic string: Codex takes a value that is not valid TOML as a literal string.
func TestTOMLStrings(t *testing.T) {
	for in, want := range map[string]string{`/a b/c`: `"/a b/c"`, `/a"b`: `"/a\"b"`, `/a\b`: `"/a\\b"`, "/a\nb": `"/a\u000ab"`, "/ü": `"/ü"`} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
	if got := tomlTable([][2]string{{":root", "read"}, {"/x", "deny"}}); got != `":root"="read","/x"="deny"` {
		t.Errorf("table %s", got)
	}
}

// The allowance is one full-context request: the whole window as uncached input, plus the largest output.
func TestAllowance(t *testing.T) {
	usd, ok := Allowance("gpt-6.1-sol")
	if !ok || usd < 1.79 || usd > 1.81 { // 258,400 × $2/M + 128,000 × $10/M = $1.7968
		t.Errorf("allowance $%.4f, %v", usd, ok)
	}
	if _, ok := Allowance("gpt-unknown"); ok {
		t.Error("an unknown model has an allowance")
	}
	if e, err := Effort("gpt-6.1-sol", ""); err != nil || e != "low" {
		t.Errorf("default effort %q, %v", e, err)
	}
	if filepath.Base(zshPrefix("/tmp/x")) != "zsh" {
		t.Error("TMPPREFIX")
	}
}

// The user's ~/.zshenv never runs in the agent's shells: `zsh -c` reads $ZDOTDIR/.zshenv even without a login shell,
// after Codex's environment filter, so the shells get an empty ZDOTDIR of the run's own. Checked offline with the
// shells' own settings (shell_environment_policy.set): a .zshenv that exports a marker leaves none; without ZDOTDIR it
// would.
func TestTheUsersZshenvNeverRuns(t *testing.T) {
	if _, err := os.Stat("/bin/zsh"); err != nil {
		t.Skip("no /bin/zsh")
	}
	home, temp := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte("export AGENTIUM_ZSHENV_MARKER=sourced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inv := invocation(SignInLogin, "")
	inv.Home, inv.TempRoot = home, temp
	cmd, overrides, _ := command(t, inv)
	set := map[string]string{}
	for _, entry := range strings.Split(strings.Trim(overrides["shell_environment_policy.set"], "{}"), ",") {
		key, value, _ := strings.Cut(entry, "=")
		set[strings.Trim(key, `"`)] = strings.Trim(value, `"`)
	}
	if set["ZDOTDIR"] == "" || !strings.HasPrefix(set["ZDOTDIR"], temp) || !slices.Contains(cmd.Dirs, set["ZDOTDIR"]) {
		t.Fatalf("ZDOTDIR %q is not an empty folder of the run's own (dirs %q)", set["ZDOTDIR"], cmd.Dirs)
	}
	for _, dir := range cmd.Dirs {
		if strings.HasPrefix(dir, temp) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	shell := func(env map[string]string) string {
		t.Helper()
		c := exec.Command("/bin/zsh", "-c", "env")
		c.Env = []string{"PATH=/usr/bin:/bin"}
		for k, v := range env {
			c.Env = append(c.Env, k+"="+v)
		}
		out, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if out := shell(set); strings.Contains(out, "AGENTIUM_ZSHENV_MARKER") {
		t.Errorf("the user's .zshenv ran in the agent's shell:\n%s", out)
	}
	delete(set, "ZDOTDIR")
	if out := shell(set); !strings.Contains(out, "AGENTIUM_ZSHENV_MARKER=sourced") {
		t.Error("the check proves nothing: without ZDOTDIR the .zshenv did not run either")
	}
}
