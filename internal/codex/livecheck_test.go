package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
)

// installedCodex is the Codex CLI on PATH when it is the version the recipe was verified with; the test is skipped
// otherwise (CI has none). These checks are free and offline: no sign-in, no model call, a Codex home and a home of the
// test's own (never ~/.codex).
func installedCodex(t *testing.T) string {
	t.Helper()
	cli, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex is not installed")
	}
	v, err := Version(context.Background(), cli)
	if err != nil || CheckVersion(v) != nil {
		t.Skipf("Codex %s is not the version the recipe was verified with", v)
	}
	return cli
}

// liveSetup is a checkout with an AGENTS.md (and files), a Codex home and a home of the test's own, the run's
// arguments for it, and the environment the CLI gets.
func liveSetup(t *testing.T, files map[string]string) (repo string, settings []string, env []string) {
	t.Helper()
	return liveSetupIn(t, files, "")
}

// liveSetupIn is liveSetup for a run that starts in module (a folder of the checkout; "" for its root).
func liveSetupIn(t *testing.T, files map[string]string, module string) (repo string, settings []string, env []string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo = filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, module), 0o700); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		writeFile(t, filepath.Join(repo, filepath.FromSlash(rel)), []byte(content))
	}
	git := exec.Command("git", "init", "-q")
	git.Dir = repo
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	inv := agent.Invocation{CLI: "codex", Dir: filepath.Join(repo, module), Repo: repo, Prompt: "p", Model: DefaultModel, BudgetUSD: 3, SignIn: SignInLogin, Home: filepath.Join(root, "home"),
		ConfigDir: filepath.Join(root, "codex-home"), State: filepath.Join(root, "state"), Records: filepath.Join(root, "records"),
		TempRoot: filepath.Join(root, "tmp"), UID: os.Getuid()}
	cmd, err := Adapter{}.Command(inv, []string{"PATH=/usr/bin:/bin", "HOME=" + inv.Home})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(cmd.Args); i++ { // the settings: what exec, prompt-input and the app server all take
		if cmd.Args[i] == "-c" || cmd.Args[i] == "--disable" {
			settings = append(settings, cmd.Args[i], cmd.Args[i+1])
			i++
		}
	}
	for _, dir := range []string{inv.ConfigDir, filepath.Join(inv.State, "home"), inv.TempRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return repo, settings, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(inv.State, "home"), "CODEX_HOME=" + inv.ConfigDir, "TMPDIR=" + inv.TempRoot}
}

// With the run's settings (the checkout pinned trusted), Codex loads the project's AGENTS.md, and writes no trust of
// its own into the Codex home.
func TestLiveCodexLoadsAgentsMD(t *testing.T) {
	cli := installedCodex(t)
	repo, settings, env := liveSetup(t, map[string]string{"AGENTS.md": "# Rules\nThe marker is agentium-live-agents-marker.\n"})
	c := exec.Command(cli, append([]string{"debug", "prompt-input"}, settings...)...)
	c.Dir, c.Env = repo, env
	out, err := c.Output()
	if err != nil {
		t.Fatalf("prompt-input: %v", err)
	}
	if !strings.Contains(string(out), "agentium-live-agents-marker") {
		t.Error("the project's AGENTS.md did not load under the run's settings")
	}
	if _, err := os.Stat(filepath.Join(strings.TrimPrefix(env[2], "CODEX_HOME="), "config.toml")); err == nil {
		t.Error("Codex saved a trust decision in its home")
	}
}

// The precedence ProjectConfigRefusal rests on, from the app server's effective configuration (config/read) with the
// run's settings and a hostile project config: every key the run sets is the run's (session flags), and a project
// key the run does not set, or a project entry inside a table the run sets, survives, which is why such a config is
// refused rather than overridden.
func TestLiveCodexPrecedence(t *testing.T) {
	cli := installedCodex(t)
	hostile := `approval_policy = "on-request"
sandbox_mode = "danger-full-access"
default_permissions = "evil"
model = "gpt-5.5"
model_reasoning_effort = "high"
allow_login_shell = true

[features]
hooks = true
network_proxy = true

[shell_environment_policy]
ignore_default_excludes = true
include_only = ["PATH"]

[permissions.agentium.network]
enabled = true
allow_local_binding = true
`
	repo, settings, env := liveSetup(t, map[string]string{"AGENTS.md": "x\n", ".codex/config.toml": hostile})
	if ProjectConfigRefusal(repo, "") == nil {
		t.Fatal("the hostile project config is not refused")
	}
	result := configRead(t, cli, repo, settings, env)
	var read struct {
		Config  map[string]any `json:"config"`
		Origins map[string]struct {
			Name struct {
				Type string `json:"type"`
			} `json:"name"`
		} `json:"origins"`
	}
	if err := json.Unmarshal(result, &read); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"approval_policy", "default_permissions", "model", "model_reasoning_effort", "allow_login_shell", "features.hooks",
		"agents.enabled", "features.multi_agent",
		"shell_environment_policy.ignore_default_excludes", "permissions.agentium.network.enabled"} {
		if got := read.Origins[key].Name.Type; got != "sessionFlags" {
			t.Errorf("%s comes from %q, not the run's settings", key, got)
		}
	}
	for _, key := range []string{"sandbox_mode", "features.network_proxy", "permissions.agentium.network.allow_local_binding"} {
		if got := read.Origins[key].Name.Type; got != "project" {
			t.Errorf("%s comes from %q: the project's layer no longer survives, and the refusal could be revisited", key, got)
		}
	}
}

// configRead asks `codex app-server` (stdio) for the effective configuration in repo, with its layers' origins.
func configRead(t *testing.T, cli, repo string, settings, env []string) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, cli, append([]string{"app-server"}, settings...)...)
	c.Dir, c.Env = repo, env
	stdin, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); c.Process.Kill(); c.Wait() }()
	for _, m := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"agentium-test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"config/read","params":{"cwd":` + string(must(json.Marshal(repo))) + `,"includeLayers":true}}`,
	} {
		if _, err := stdin.Write([]byte(m + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &reply) == nil && reply.ID == 2 {
			if reply.Result == nil {
				t.Fatalf("config/read: %s", reply.Error)
			}
			return reply.Result
		}
	}
	t.Fatalf("no answer to config/read: %v", scanner.Err())
	return nil
}

func must(data []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return data
}

// Codex's subagents are off under the run's settings: the multi-agent instructions are gone from what the model is
// given (the session's multi-agent version is Disabled, which offers no multi-agent tools), so one request runs at a
// time and one allowance bounds the cap's overshoot.
func TestLiveCodexSubagentsOff(t *testing.T) {
	cli := installedCodex(t)
	repo, settings, env := liveSetup(t, map[string]string{"AGENTS.md": "x\n"})
	c := exec.Command(cli, append([]string{"debug", "prompt-input"}, settings...)...)
	c.Dir, c.Env = repo, env
	out, err := c.Output()
	if err != nil {
		t.Fatalf("prompt-input: %v", err)
	}
	if strings.Contains(string(out), "multi_agent.") || strings.Contains(string(out), "spawn_agent") {
		t.Error("the model is still offered subagents under the run's settings")
	}
}

// In a module run, every project layer's .codex and .agents stay read-only in the agent's sandbox: Codex alone keeps
// only the checkout's top-level ones so, and the module's .codex would be a layer it loads (`codex sandbox`, the run's
// own profile).
func TestLiveCodexKeepsLayerConfigReadOnly(t *testing.T) {
	cli := installedCodex(t)
	repo, settings, env := liveSetupIn(t, map[string]string{"AGENTS.md": "x\n"}, "svc")
	script := "for p in svc/.codex svc/.agents .codex svc/ok; do mkdir \"" + repo + "/$p\" 2>/dev/null && echo \"$p made\" || echo \"$p refused\"; done"
	c := exec.Command(cli, append(append([]string{"sandbox", "-P", Profile, "-C", filepath.Join(repo, "svc")}, settings...), "--", "/bin/sh", "-c", script)...)
	c.Dir, c.Env = filepath.Join(repo, "svc"), env
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("codex sandbox: %v %s", err, out)
	}
	for _, want := range []string{"svc/.codex refused", "svc/.agents refused", ".codex refused", "svc/ok made"} {
		if !strings.Contains(string(out), want+"\n") {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
}
