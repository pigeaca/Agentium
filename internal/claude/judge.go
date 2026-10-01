package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// Judgement is one headless judge call: Claude Code with no tools, a fixed system prompt instead of its own, and a JSON
// schema for the answer. The prompt goes to stdin. Like runs, it loads project settings only (here: none, since Dir is
// empty), no MCP servers and no claude.ai connectors, keeps no session, and gets only the allowlisted environment plus
// the sign-in's own secret. Having no tools, it needs no sandbox or denied paths: it cannot read or run anything.
type Judgement struct {
	CLI          string // path to the claude executable
	Dir          string // an empty folder of the call's own, where Claude Code starts
	Model        string
	Effort       string // empty: the CLI's default
	SystemPrompt string
	Schema       string  // the JSON schema of the answer
	BudgetUSD    float64 // --max-budget-usd for this call; 0: none
	SignIn       string  // SignInAPIKey, SignInTokenFile or SignInLogin, as for runs
	Secret       string  // the API key or token for SignInAPIKey and SignInTokenFile; never logged or stored
	ConfigDir    string  // a fresh, empty CLAUDE_CONFIG_DIR for SignInAPIKey and SignInTokenFile
	Home         string  // the user's home folder
}

// Command returns the arguments and environment of the call. environ is the parent's environment (os.Environ()),
// filtered through the same allowlist as runs (Environ).
func (j Judgement) Command(environ []string) (args, env []string, err error) {
	if j.CLI == "" || j.Dir == "" || j.Model == "" || j.SystemPrompt == "" || j.Schema == "" || j.Home == "" {
		return nil, nil, errors.New("a judge call needs the CLI, a folder, a model, a system prompt, a schema and the home folder")
	}
	for _, p := range []string{j.Dir, j.Home, j.ConfigDir} {
		if p != "" && !filepath.IsAbs(p) {
			return nil, nil, fmt.Errorf("path %q is not absolute", p)
		}
	}
	args = []string{"-p", "--model", j.Model, "--tools", "", "--system-prompt", j.SystemPrompt, "--json-schema", j.Schema,
		"--output-format", "json", "--no-session-persistence",
		"--setting-sources", "project", // no user-level skills, settings or memory
		"--strict-mcp-config",                                                        // only MCP servers given here (none)
		"--settings", `{"autoMemoryEnabled":false,"disableClaudeAiConnectors":true}`} // as runs' settings, beside the variables below
	if j.Effort != "" {
		args = append(args, "--effort", j.Effort)
	}
	if j.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(j.BudgetUSD, 'f', -1, 64))
	}
	env = append(Environ(environ), "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "DISABLE_AUTOUPDATER=1", "ENABLE_CLAUDEAI_MCP_SERVERS=false")
	// The sign-in as Invocation.Command sets it for runs: a fresh config folder cannot use a subscription login.
	switch j.SignIn {
	case SignInLogin:
		if j.Secret != "" {
			return nil, nil, errors.New("sign-in login takes no secret")
		}
		userConfig := UserConfigDir(environ, j.Home)
		if !filepath.IsAbs(userConfig) { // relative, it would resolve in the empty call folder: a fresh, signed-out config
			return nil, nil, fmt.Errorf("CLAUDE_CONFIG_DIR %q is not absolute", userConfig)
		}
		if userConfig != filepath.Join(j.Home, ".claude") {
			env = append(env, "CLAUDE_CONFIG_DIR="+userConfig)
		}
	case SignInAPIKey, SignInTokenFile:
		if j.Secret == "" || j.ConfigDir == "" {
			return nil, nil, fmt.Errorf("sign-in %s needs a secret and a fresh config folder", j.SignIn)
		}
		name := "ANTHROPIC_API_KEY"
		if j.SignIn == SignInTokenFile {
			name = "CLAUDE_CODE_OAUTH_TOKEN"
		}
		env = append(env, "CLAUDE_CONFIG_DIR="+j.ConfigDir, name+"="+j.Secret)
	default:
		return nil, nil, fmt.Errorf("unknown sign-in mode %q", j.SignIn)
	}
	return args, env, nil
}

// RunJudgement makes the call with prompt on stdin, writing Claude Code's stdout (one JSON result) and stderr to the
// given files. They are files, not pipes, so a background process holding one open cannot hold the call past its end;
// keep them outside Dir, which stays empty. On timeout or cancel the process group is interrupted, then killed after
// grace.
func RunJudgement(ctx context.Context, j Judgement, prompt string, environ []string, stdout, stderr *os.File, timeout, grace time.Duration) (runner.Result, error) {
	args, env, err := j.Command(environ)
	if err != nil {
		return runner.Result{}, err
	}
	return runner.Run(ctx, runner.Spec{Dir: j.Dir, Args: append([]string{j.CLI}, args...), Environ: env,
		Stdin: strings.NewReader(prompt), Timeout: timeout, Grace: grace, Output: stdout, Stderr: stderr})
}
