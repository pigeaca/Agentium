package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// fakeAgent writes a stand-in for Claude Code. It records what it saw (its folder, whether the hidden test was
// present, its config folder), optionally edits the checkout, and prints a stream-json transcript whose result text
// includes the sign-in token, so redaction can be checked.
func fakeAgent(t *testing.T, edit, permissionMode, extraTool string) (cli, seen string) {
	t.Helper()
	dir := t.TempDir()
	seen = filepath.Join(dir, "seen")
	script := `#!/bin/sh
{ pwd; test -e tests/value_test.sh && echo hidden-present || echo hidden-absent; echo "config=$CLAUDE_CONFIG_DIR"; echo "tmp=$CLAUDE_CODE_TMPDIR"; test -d "$CLAUDE_CODE_TMPDIR" && echo tmp-ready; } > ` + seen + `
` + edit + `
cat <<EOF
{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5","permissionMode":"` + permissionMode + `","tools":["Bash","Edit","Read"],"skills":[],"slash_commands":["compact"]}
{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":5,"cache_creation_input_tokens":25000,"cache_read_input_tokens":0,"service_tier":"standard"},"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sh run_tests.sh"}},{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"$(pwd)/value.txt"}}` + extraTool + `]}}
{"type":"result","subtype":"success","is_error":false,"result":"Done. (token: ${CLAUDE_CODE_OAUTH_TOKEN:-none})","total_cost_usd":0.25,"num_turns":4,"duration_ms":30000,"permission_denials":[],"modelUsage":{"claude-sonnet-5":{"inputTokens":10,"outputTokens":500,"cacheReadInputTokens":100,"cacheCreationInputTokens":25000,"costUSD":0.25}}}
EOF
`
	cli = filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli, seen
}

func TestRunOnceGradesWithHiddenTestsAndIsolation(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "run_tests.sh", "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n")
	writeFile(t, repo, "CLAUDE.md", "# Rules\n")
	writeFile(t, repo, "value.txt", "old\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	writeFile(t, repo, "tests/value_test.sh", "grep -q new value.txt\n")
	writeFile(t, repo, "value.txt", "new\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "Make the value new")

	home := t.TempDir()
	data := filepath.Join(t.TempDir(), "data")
	tokenFile := filepath.Join(home, "token")
	writeFile(t, home, "token", "tok-secret-1234567890\n")
	if err := os.Chmod(tokenFile, 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": home, "AGENTIUM_CLAUDE_TOKEN_FILE": tokenFile}
	run := func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{
			Args: args, Stdout: &stdout, Stderr: &stderr, Dir: repo,
			Getenv: func(key string) string { return vars[key] },
			Environ: func() []string {
				return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GITHUB_TOKEN=ghp_notforthechild"}
			},
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Now:      func() time.Time { return time.Now() },
		})
		return cliResult{code, stdout.String(), stderr.String()}
	}
	expect(t, run("init"), ExitOK)
	expect(t, run("task", "import", "--commit", "HEAD", "--name", "value", "--verify", "sh run_tests.sh"), ExitOK)
	before := repoState(t, repo)

	solver, seen := fakeAgent(t, "printf 'new\\n' > value.txt", "acceptEdits", "")
	vars["AGENTIUM_CLAUDE"] = solver
	solved := run("run", "once", "value")
	expect(t, solved, ExitOK, "Starting a real Claude Code run", "sign-in token-file", "outcome      ok; verification passed",
		"changes      1 file(s), +1 -1, 0 commit(s)", "ran the checks: true", "first request 25005 tokens")
	saw, _ := os.ReadFile(seen)
	if !strings.Contains(string(saw), "hidden-absent") || !strings.Contains(string(saw), filepath.Join("workspaces")) ||
		!strings.Contains(string(saw), "config="+filepath.Join(data, "workspaces")) {
		t.Errorf("the agent saw:\n%s", saw)
	}
	records := filepath.Join(data, "records")
	entries, _ := os.ReadDir(records)
	if len(entries) != 1 {
		t.Fatalf("records = %v", entries)
	}
	runDir := filepath.Join(records, entries[0].Name())
	transcript, _ := os.ReadFile(filepath.Join(runDir, "stream.jsonl"))
	if strings.Contains(string(transcript), "tok-secret-1234567890") || !strings.Contains(string(transcript), "[REDACTED]") {
		t.Errorf("the token must be redacted from the transcript:\n%s", transcript)
	}
	diff, _ := os.ReadFile(filepath.Join(runDir, "agent.diff"))
	if !strings.Contains(string(diff), "+new") || strings.Contains(string(diff), "value_test.sh") {
		t.Errorf("the agent's diff (hidden tests are not the agent's work):\n%s", diff)
	}
	// The run's own Claude Code temp root: short, in /tmp, made before the agent started and removed after it.
	tempRoot := ""
	for _, line := range strings.Split(string(saw), "\n") {
		if v, ok := strings.CutPrefix(line, "tmp="); ok {
			tempRoot = v
		}
	}
	if !strings.HasPrefix(tempRoot, "/tmp/ag-") || !strings.Contains(string(saw), "tmp-ready") {
		t.Errorf("the agent's temp root %q (it saw:\n%s)", tempRoot, saw)
	}
	for _, gone := range []string{filepath.Join(runDir, "verify"), filepath.Join(data, "workspaces", entries[0].Name()), tempRoot} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s should be removed after the run", gone)
		}
	}

	idle, _ := fakeAgent(t, "", "acceptEdits", "")
	vars["AGENTIUM_CLAUDE"] = idle
	expect(t, run("run", "once", "value"), ExitOK, "outcome      ok; verification failed", "changes      0 file(s)")

	drifted, _ := fakeAgent(t, "", "default", "")
	vars["AGENTIUM_CLAUDE"] = drifted
	expect(t, run("run", "once", "value"), ExitOK, "outcome      unfair", `unfair: permission mode "default", not "acceptEdits"`)

	peek := `,{"type":"tool_use","id":"t3","name":"Read","input":{"file_path":"` + filepath.Join(data, "agentium.db") + `"}}`
	peeker, _ := fakeAgent(t, "", "acceptEdits", peek)
	vars["AGENTIUM_CLAUDE"] = peeker
	expect(t, run("run", "once", "value"), ExitOK, "outcome      unfair", "1 file tool call(s) reached Agentium's data")

	list := run("run", "list")
	expect(t, list, ExitOK, "value", "yes", "no", "unfair", "$0.25")
	id := strings.Fields(strings.Split(list.stdout, "\n")[1])[0]
	expect(t, run("run", "show", id), ExitOK, "Run "+id, "files        agent.diff", "stream.jsonl", "verify.log")
	shown := run("run", "show", id, "--diff", "--log")
	expect(t, shown, ExitOK, "--- agent.diff", "-old", "+new", "--- setup.log: none (the task has no setup)", "--- verify.log", "$ sh run_tests.sh")
	expect(t, run("run", "show", "nope"), ExitError, "not found")

	expect(t, run("task", "add", "broken-setup", "--base", "HEAD~1", "--instruction", "Anything.", "--setup", "exit 3", "--verify", "true"), ExitOK)
	expect(t, run("run", "once", "broken-setup"), ExitOK, "outcome      infra; verification not run", "note: setup failed")
	expect(t, run("run", "once", "value", "--snapshot", "missing"), ExitError, `snapshot "missing"`)
	expect(t, run("run", "once"), ExitUsage)

	if after := repoState(t, repo); after != before {
		t.Errorf("runs modified the repository:\nbefore %s\nafter  %s", before, after)
	}
}

func TestRunOnceRefusesInstructionFilesAboveTheWorkspace(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "a.txt", "a\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	outer := t.TempDir()
	writeFile(t, outer, "CLAUDE.md", "personal notes\n")
	vars := map[string]string{"AGENTIUM_HOME": filepath.Join(outer, "data"), "HOME": t.TempDir(), "AGENTIUM_CLAUDE": "/bin/false"}
	run := func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{Args: args, Stdout: &stdout, Stderr: &stderr, Dir: repo,
			Getenv: func(key string) string { return vars[key] }, Environ: func() []string { return nil },
			LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now})
		return cliResult{code, stdout.String(), stderr.String()}
	}
	expect(t, run("init"), ExitOK)
	expect(t, run("task", "add", "t", "--base", "HEAD", "--instruction", "Do it.", "--verify", "true"), ExitOK)
	expect(t, run("run", "once", "t"), ExitError, filepath.Join(outer, "CLAUDE.md"), "would load it into every run")
}

// scriptedAgent writes a fake Claude Code that runs body in its checkout, then prints a successful transcript. With
// hang set, it instead waits to be interrupted and reports a result with cost 0.40, as Claude Code does on SIGINT.
func scriptedAgent(t *testing.T, body string, hang bool) string {
	t.Helper()
	stream := `{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5","permissionMode":"acceptEdits","tools":["Bash"],"skills":[],"slash_commands":[]}`
	result := `{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.40,"num_turns":3,"duration_ms":1000,"modelUsage":{}}`
	script := "#!/bin/sh\n"
	if hang {
		script += "trap 'echo '\"'\"'" + result + "'\"'\"'; exit 130' INT\n" + body + "\necho '" + stream + "'\nwhile :; do sleep 0.05; done\n"
	} else {
		script += body + "\necho '" + stream + "'\necho '" + result + "'\n"
	}
	cli := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli
}

type runFixture struct {
	repo, data, home string
	vars             map[string]string
	run              func(ctx context.Context, args ...string) cliResult
	// sleep stands in for Env.Sleep (experiment run --wait); nil returns at once.
	sleep *func(ctx context.Context, d time.Duration) error
	// terminal is Env.Terminal: whether stdout counts as a terminal (false: plain output).
	terminal *bool
	// account is what Env.AccountHome returns: the account's home folder ("" unknown).
	account *string
}

// newRunFixture is a project with one commit that adds a hidden test and the task "value" imported from it, initialized
// in data. The repository, data folder and home are copies of a template built once per test run (see fixtures_test.go).
func newRunFixture(t *testing.T, data string) runFixture {
	t.Helper()
	f := runFixtureAt(t.TempDir(), data, t.TempDir())
	cloneDirs(t, runTemplate(t), []string{f.repo, f.data, f.home})
	return f
}

// runTemplate is the folders (repository, data, home) of newRunFixture's project, built on first use.
func runTemplate(t *testing.T) []string {
	t.Helper()
	return templates["run"].build(t, "run", []string{"repo", "data", "home"}, func(d []string) {
		f := runFixtureAt(d[0], d[1], d[2])
		gitIn(t, f.repo, "init", "-q", "-b", "main")
		writeFile(t, f.repo, "run_tests.sh", "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n")
		writeFile(t, f.repo, "CLAUDE.md", "# Rules\n")
		writeFile(t, f.repo, "value.txt", "old\n")
		gitIn(t, f.repo, "add", "-A")
		gitIn(t, f.repo, "commit", "-q", "-m", "base")
		writeFile(t, f.repo, "tests/value_test.sh", "grep -q new value.txt\n")
		writeFile(t, f.repo, "value.txt", "new\n")
		gitIn(t, f.repo, "add", "-A")
		gitIn(t, f.repo, "commit", "-q", "-m", "Make the value new")
		expect(t, f.run(context.Background(), "init"), ExitOK)
		expect(t, f.run(context.Background(), "task", "import", "--commit", "HEAD", "--name", "value", "--verify", "sh run_tests.sh"), ExitOK)
	})
}

// runFixtureAt is the fixture's runner over the given folders; it creates nothing.
func runFixtureAt(repo, data, home string) runFixture {
	f := runFixture{repo: repo, data: data, home: home}
	f.vars = map[string]string{"AGENTIUM_HOME": data, "HOME": f.home}
	f.sleep = new(func(ctx context.Context, d time.Duration) error)
	f.terminal = new(bool)
	f.account = new(string)
	f.run = func(ctx context.Context, args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(ctx, Env{Args: args, Stdout: &stdout, Stderr: &stderr, Dir: f.repo, Terminal: *f.terminal,
			Getenv: func(key string) string { return f.vars[key] },
			Environ: func() []string {
				environ := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home}
				if v := f.vars["GOCACHE"]; v != "" { // the user's Go build cache
					environ = append(environ, "GOCACHE="+v)
				}
				return environ
			},
			LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now,
			AccountHome: func() string { return *f.account },
			Backoff:     func(int) time.Duration { return 10 * time.Millisecond },
			Sleep: func(ctx context.Context, d time.Duration) error {
				if *f.sleep != nil {
					return (*f.sleep)(ctx, d)
				}
				return nil
			}})
		return cliResult{code, stdout.String(), stderr.String()}
	}
	return f
}

func TestRunGradingResistsTampering(t *testing.T) {
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-grading-must-not-see-this") // in Agentium's own environment // secret-scan: allow
	marker := filepath.Join(t.TempDir(), "filter-ran")
	// The agent rewrites the checks to pass, plants its own version of the hidden test, and sets a git clean filter
	// that would run (outside the sandbox) when grading stages its tree.
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, `printf 'exit 0\n' > run_tests.sh
mkdir -p tests && printf 'true\n' > tests/value_test.sh
git config filter.leak.clean "sh -c 'env > `+marker+`; cat'"
printf '* filter=leak\n' > .gitattributes`, false)
	result := f.run(context.Background(), "run", "once", "value")
	expect(t, result, ExitOK, "outcome      ok; verification failed", "checks       the agent changed run_tests.sh",
		"graded with the starting version: run_tests.sh")
	if _, err := os.Stat(marker); err == nil {
		data, _ := os.ReadFile(marker)
		t.Errorf("the agent's git filter ran during grading (API key visible: %v)", strings.Contains(string(data), "grading-must-not-see-this"))
	}
}

func TestRunInterruptedIsStoredWithItsCost(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	ready := filepath.Join(t.TempDir(), "ready")
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "touch "+ready, true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { // interrupt once the agent is running, not during checkout or setup
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(ready); err == nil {
				break
			}
		}
		cancel()
	}()
	expect(t, f.run(ctx, "run", "once", "value"), ExitError, "context canceled")
	expect(t, f.run(context.Background(), "run", "list"), ExitOK, "cancelled", "$0.40")
}

// An agent that removes its checkout's .git must not erase its run or its spend, and git must not wander into an
// enclosing repository.
func TestRunSurvivesAnAgentRemovingItsGitFolder(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "rm -rf .git", false)
	result := f.run(context.Background(), "run", "once", "value")
	expect(t, result, ExitOK, "outcome      ok; verification failed", "the agent's commits could not be counted")
	expect(t, f.run(context.Background(), "run", "list"), ExitOK, "value", "$0.40")
}

func TestRunKeepSnapshotArmAndSetupOutputs(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nBe brief.\n")
	expect(t, f.run(context.Background(), "context", "snapshot", "brief", "--working-tree"), ExitOK)
	gitIn(t, f.repo, "checkout", "--", "CLAUDE.md")
	expect(t, f.run(context.Background(), "task", "add", "gen", "--base", "HEAD~1", "--instruction", "Anything.",
		"--setup", "echo generated > gen.txt", "--verify", "test -f gen.txt"), ExitOK)
	seen := filepath.Join(t.TempDir(), "claude-md")
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "cat CLAUDE.md > "+seen, false)
	kept := f.run(context.Background(), "run", "once", "gen", "--snapshot", "brief", "--keep")
	// The arm's CLAUDE.md is what the agent saw; neither it nor the setup's output counts as the agent's change.
	expect(t, kept, ExitOK, "Run ", "outcome      ok; verification passed", "changes      0 file(s)")
	if saw, _ := os.ReadFile(seen); string(saw) != "# Rules\nBe brief.\n" {
		t.Errorf("the agent saw CLAUDE.md = %q", saw)
	}
	workspaces, _ := os.ReadDir(filepath.Join(f.data, "workspaces"))
	records, _ := os.ReadDir(filepath.Join(f.data, "records"))
	if len(workspaces) != 1 || len(records) != 1 {
		t.Fatalf("workspaces %v, records %v", workspaces, records)
	}
	if _, err := os.Stat(filepath.Join(f.data, "records", records[0].Name(), "verify", "gen.txt")); err != nil {
		t.Errorf("--keep should keep the grading copy: %v", err)
	}
}

// Setup builds into the agent's own cache (the build tools' agent caches: Go's GOCACHE is the run's go-build folder), so
// a warming step spares the agent a cold build; verification keeps Agentium's cache in the data folder.
func TestRunSetupUsesTheAgentsBuildCache(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	seen := t.TempDir()
	expect(t, f.run(context.Background(), "task", "add", "cache", "--base", "HEAD~1", "--instruction", "Anything.",
		"--setup", `printf %s "$GOCACHE" > `+filepath.Join(seen, "setup"),
		"--verify", `printf %s "$GOCACHE" > `+filepath.Join(seen, "verify")), ExitOK)
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "true", false)
	expect(t, f.run(context.Background(), "run", "once", "cache", "--keep"), ExitOK, "outcome      ok; verification passed")
	workspaces, _ := os.ReadDir(filepath.Join(f.data, "workspaces"))
	if len(workspaces) != 1 {
		t.Fatalf("workspaces %v", workspaces)
	}
	if got, want := readString(t, filepath.Join(seen, "setup")), filepath.Join(f.data, "workspaces", workspaces[0].Name(), "go-build"); got != want {
		t.Errorf("setup's GOCACHE = %q, want the run's own %q", got, want)
	}
	if got, want := readString(t, filepath.Join(seen, "verify")), filepath.Join(f.data, "cache", "go-build"); got != want {
		t.Errorf("verification's GOCACHE = %q, want Agentium's %q", got, want)
	}
}

// HOME (the fixture's home) is not the account's home folder here: the run's sandbox settings deny the login keychain
// in both, so an agent cannot open the account's own by its path.
func TestRunDeniesTheAccountsKeychainWhenHomeIsRedirected(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	*f.account = t.TempDir()
	args := filepath.Join(t.TempDir(), "args")
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, `printf '%s\n' "$@" > `+args, false)
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitOK)
	seen := readString(t, args)
	for _, p := range []string{filepath.Join(*f.account, "Library", "Keychains"), filepath.Join(f.home, "Library", "Keychains")} {
		if !strings.Contains(seen, `"`+p+`"`) || !strings.Contains(seen, "Read(/"+p+"/**)") {
			t.Errorf("the run's settings do not deny %s", p)
		}
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunRefusesAWorkspaceInsideADeniedPath(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	f := newRunFixture(t, filepath.Join(outer, "data"))
	token := filepath.Join(outer, "token") // its folder is denied to runs, and it holds the data folder
	writeFile(t, outer, "token", "tok-abcdef\n")
	if err := os.Chmod(token, 0o600); err != nil {
		t.Fatal(err)
	}
	f.vars["AGENTIUM_CLAUDE_TOKEN_FILE"] = token
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "", false)
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitError, "lies inside", "runs may not read")
	if records, _ := os.ReadDir(filepath.Join(f.data, "records")); len(records) != 0 {
		t.Errorf("a run that never started left records: %v", records)
	}

	// The user's Go build cache is denied too.
	outer = t.TempDir()
	f = newRunFixture(t, filepath.Join(outer, "data"))
	f.vars["GOCACHE"] = outer
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "", false)
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitError, "lies inside "+outer, "runs may not read")
}

// calibratingAgent writes a fake Claude Code that answers the calibration with real-looking tool calls: the sandbox
// command, seq with an output Claude Code saved, and a read of that saved file. It learns the codeword from the
// instruction file itself (not through a tool call, so the check sees it as loaded). mode varies the evidence:
// "redirect" works around the saved output, "denied" fails the read, "sandbox-error" fails the first command,
// "read-claude-md" reads the instruction file with the Read tool.
func calibratingAgent(t *testing.T, tools, skills string, firstRequest int, mode string) string {
	t.Helper()
	saved := "/cfg/projects/-own-session/tool-results/b1.txt"
	use := func(id, name, input string) string {
		return `{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":` + strconv.Itoa(firstRequest) +
			`},"content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]}}`
	}
	result := func(id, text string, isError bool) string {
		return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"` + text + `","is_error":` + strconv.FormatBool(isError) + `}]}}`
	}
	lines := []string{`{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5","permissionMode":"acceptEdits","tools":[` +
		tools + `],"skills":[` + skills + `],"slash_commands":["compact"]}`}
	sandboxOut, sandboxErr := "agentium-sandbox-ok", false
	if mode == "sandbox-error" {
		sandboxOut, sandboxErr = "sandbox-exec: Operation not permitted", true
	}
	lines = append(lines, use("t1", "Bash", `{"command":"printf 'agentium-sandbox-ok\\n'"}`), result("t1", sandboxOut, sandboxErr))
	switch mode {
	case "redirect":
		lines = append(lines, use("t2", "Bash", `{"command":"seq 1 40000 > /tmp/o.txt && sed -n 20000p /tmp/o.txt"}`), result("t2", "20000", false))
	default:
		lines = append(lines, use("t2", "Bash", `{"command":"seq 1 40000"}`),
			result("t2", `<persisted-output>\nOutput too large (223.5KB). Full output saved to: `+saved+`\n</persisted-output>`, false))
		if mode == "denied" {
			lines = append(lines, use("t3", "Read", `{"file_path":"`+saved+`","offset":20000}`), result("t3", "Permission to read denied", true))
		} else {
			lines = append(lines, use("t3", "Read", `{"file_path":"`+saved+`","offset":20000}`), result("t3", "20000: 20000", false))
		}
	}
	if mode == "read-claude-md" {
		lines = append(lines, use("t4", "Read", `{"file_path":"CLAUDE.md"}`), result("t4", "# Rules", false))
	}
	script := "#!/bin/sh\ncode=$(cat CLAUDE.md AGENTS.md 2>/dev/null | sed -n 's/^Calibration codeword: //p' | tail -1)\n[ -n \"$code\" ] || code=NONE\n"
	for _, line := range lines {
		script += "cat <<'EOF'\n" + line + "\nEOF\n"
	}
	script += `echo '{"type":"result","subtype":"success","is_error":false,"result":"SANDBOX=agentium-sandbox-ok LINE=20000 CODEWORD='"$code"'","total_cost_usd":0.02,"num_turns":4,"duration_ms":2000,"modelUsage":{}}'` + "\n"
	cli := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli
}

func TestCalibrationRecordsTheEnvironmentLaterRunsMustMatch(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\n"+strings.Repeat("A longer context line for the trimmed arm.\n", 40))
	expect(t, f.run(context.Background(), "context", "snapshot", "long", "--working-tree"), ExitOK)
	gitIn(t, f.repo, "checkout", "--", "CLAUDE.md")

	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, "")
	cal := f.run(context.Background(), "run", "calibrate", "--snapshot", "long")
	expect(t, cal, ExitOK, "Calibrating 2 arm(s)", "base  ok       ok       ok            ok",
		"long: measured +0 tokens, estimated +430 (measured/estimated 0.00)")
	if strings.Contains(cal.stdout, "review") {
		t.Errorf("skill names must not be printed:\n%s", cal.stdout)
	}
	expect(t, f.run(context.Background(), "run", "list"), ExitOK, "(calibration)")

	// A later run with another tool set is unfair against the calibration; another model gets a note.
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Edit","Read","Monitor"`, `"review"`, 25000, "")
	expect(t, f.run(context.Background(), "run", "once", "value", "--model", "claude-opus-5-5"), ExitOK,
		"Checking the environment against the calibration of base", "note: the calibration used claude-sonnet-5, this run claude-opus-5-5",
		"outcome      unfair", "tools differ (added Monitor; missing none)")
	expect(t, f.run(context.Background(), "run", "once", "value", "--snapshot", "long"), ExitOK, "calibration of long")
}

func TestCalibrationChecksRestOnTheTranscript(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	for mode, want := range map[string]string{
		"redirect":       "base  ok       ok       unverified    ok",
		"denied":         "base  ok       ok       FAILED        ok",
		"sandbox-error":  "base  ok       FAILED   ok            ok",
		"read-claude-md": "base  ok       ok       ok            unverified",
	} {
		f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Read"`, ``, 25000, mode)
		expect(t, f.run(context.Background(), "run", "calibrate"), ExitError, want, "failed arms were not saved")
	}
	// None of those became the baseline.
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Read"`, ``, 25000, "")
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitOK, "arm base is not calibrated")

	// An arm without an instruction file cannot carry the codeword: n/a, and the other arms still count.
	gitIn(t, f.repo, "rm", "-q", "CLAUDE.md")
	expect(t, f.run(context.Background(), "context", "snapshot", "bare", "--working-tree"), ExitOK)
	gitIn(t, f.repo, "checkout", "-q", "HEAD", "--", "CLAUDE.md")
	expect(t, f.run(context.Background(), "run", "calibrate", "--snapshot", "bare"), ExitOK, "bare  ok       ok       ok            n/a")
}

func TestCalibrationKeepsOnlyBundledSkills(t *testing.T) {
	t.Parallel()
	// The base is calibrated at HEAD, which has a project skill the task's older base does not: the calibration
	// stores only the bundled skill, and a run adds its own project skills, so it is not unfair.
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	writeFile(t, f.repo, ".claude/skills/new-skill/SKILL.md", "---\nname: new-skill\ndescription: d\n---\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "add a project skill")
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Read"`, `"bundled","new-skill"`, 25000, "")
	expect(t, f.run(context.Background(), "run", "calibrate"), ExitOK)
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Read"`, `"bundled"`, 25000, "")
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitOK, "outcome      ok")
}

func TestCalibrationWithPersonalSkillsIsNotSaved(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	if err := os.MkdirAll(filepath.Join(f.home, ".claude", "skills", "my-secret-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Read"`, `"my-secret-skill"`, 25000, "")
	cal := f.run(context.Background(), "run", "calibrate")
	expect(t, cal, ExitError, "base  unfair", "unfair: 1 personal skill(s) loaded")
	if strings.Contains(cal.stdout, "my-secret-skill") {
		t.Errorf("a personal skill name was printed:\n%s", cal.stdout)
	}
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Read"`, ``, 25000, "")
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitOK, "arm base is not calibrated")
}

// A pass graded with test-runner configuration the agent changed is flagged (and experiments do not count it as a
// success), unless the task's reference changes that configuration too.
func TestRunFlagsChangedRunnerConfiguration(t *testing.T) {
	t.Parallel()
	repo, data, home := t.TempDir(), filepath.Join(t.TempDir(), "data"), t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "Makefile", "test:\n\tsh run_tests.sh\n")
	writeFile(t, repo, "run_tests.sh", "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n")
	writeFile(t, repo, "value.txt", "old\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	writeFile(t, repo, "tests/value_test.sh", "grep -q new value.txt\n")
	writeFile(t, repo, "value.txt", "new\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "Make the value new")
	writeFile(t, repo, "tests/check_test.sh", "grep -q '^check:' Makefile\n")
	writeFile(t, repo, "Makefile", "test:\n\tsh run_tests.sh\ncheck: test\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "Add a check target")
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": home}
	cli := func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{Args: args, Stdout: &stdout, Stderr: &stderr, Dir: repo, Getenv: func(k string) string { return vars[k] },
			Environ:  func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home} },
			LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now})
		return cliResult{code, stdout.String(), stderr.String()}
	}
	expect(t, cli("init"), ExitOK)
	expect(t, cli("task", "import", "--commit", "HEAD~1", "--name", "value", "--verify", "make test"), ExitOK)
	expect(t, cli("task", "import", "--commit", "HEAD", "--name", "check", "--verify", "make test"), ExitOK)
	vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt; printf 'check: test\\n' >> Makefile", false)
	for _, name := range []string{"value", "check"} {
		expect(t, cli("run", "once", name), ExitOK, "verification passed", "checks       the agent changed Makefile")
	}
	db, err := store.Open(context.Background(), filepath.Join(data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, _ := db.Projects(context.Background())
	runs, err := db.Runs(context.Background(), projects[0].ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs %v, %v", runs, err)
	}
	for _, r := range runs {
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			t.Fatal(err)
		}
		flagged := slices.Equal(rec.Behavior.ConfigChanged, []string{"Makefile"})
		if want := r.TaskName == "value"; flagged != want || experiment.Success(r.Outcome, r.Passed, rec.Behavior.ConfigChanged) == want {
			t.Errorf("task %s: config changed %v, success %v", r.TaskName, rec.Behavior.ConfigChanged, !want)
		}
	}
}

// When Agentium cannot grade a fair attempt (here its own grading repository is damaged, which the fake agent can do
// because it runs unsandboxed), the run is an infrastructure failure, retried in experiments, not the agent's failure.
func TestRunGradingFailureIsInfrastructure(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt; rm -rf '"+f.data+"'/records/*/verify/.git", false)
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitError, "not a git repository")
	expect(t, f.run(context.Background(), "run", "list"), ExitOK, "infra", "$0.40")
	// The start file ends with the finished record, which a runner killed before storing it would be recovered from.
	files, _ := filepath.Glob(filepath.Join(f.data, "records", "*", "started.json"))
	if len(files) != 1 {
		t.Fatalf("start files %v", files)
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), `"finished":true`) || !strings.Contains(string(data), `"outcome":"infra"`) {
		t.Errorf("start file after the run: %s", data)
	}
}

// What the agent leaves unreadable is its own doing: graded without it, the run keeps its outcome.
func TestRunGradesWithoutUnreadableFiles(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt; touch locked; chmod 000 locked", false)
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitOK, "outcome      ok; verification passed",
		"note: graded without what the agent left unreadable: locked")
}

// While a setup command runs, the run's start file names its process group, so a runner killed meanwhile leaves
// enough behind to see that the command still runs before its workspace is removed.
func TestRunRecordsTheRunningCommand(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	dir := t.TempDir()
	marker, release := filepath.Join(dir, "setup-pid"), filepath.Join(dir, "release")
	// The setup waits for the test to release it, so the start file is read while it surely runs; a failed test
	// releases it too.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	expect(t, f.run(context.Background(), "task", "edit", "value", "--setup",
		"echo $$ > "+marker+"; while [ ! -e "+release+" ]; do sleep 0.05; done"), ExitOK)
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt", false)
	done := make(chan cliResult)
	go func() { done <- f.run(context.Background(), "run", "once", "value") }()
	var data []byte
	// The pid is written before the start file learns the process group, and the start file is rewritten in place: a
	// read too early, or of a partial file, retries.
	waitFor(t, "the setup command's process group in the start file", func() bool {
		raw, err := os.ReadFile(marker)
		pid := strings.TrimSpace(string(raw))
		if err != nil || pid == "" {
			return false
		}
		files, _ := filepath.Glob(filepath.Join(f.data, "records", "*", "started.json"))
		if len(files) != 1 {
			return false
		}
		data, _ = os.ReadFile(files[0])
		return strings.Contains(string(data), `"pgid":`+pid)
	})
	if strings.Contains(string(data), `"agent_started":true`) {
		t.Errorf("start file during setup says the agent started: %s", data)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	expect(t, <-done, ExitOK, "verification passed")
}

// Agentium's own commands (setup, validation, grading) build with a cache in the data folder, which runs are denied;
// the agent builds with a cache of its own, in its workspace.
func TestBuildCaches(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	marks := t.TempDir()
	mark := func(name string) string { // records where a command's Go build cache and temporary files went
		return "echo \"$GOCACHE $TMPDIR $GOTMPDIR\" >> " + filepath.Join(marks, name)
	}
	expect(t, f.run(context.Background(), "task", "edit", "value", "--verify", mark("verify")+"; sh run_tests.sh", "--setup", mark("setup")), ExitOK)
	expect(t, f.run(context.Background(), "task", "validate", "value"), ExitOK, "valid")
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "echo \"$GOCACHE\" > "+filepath.Join(marks, "agent")+"; printf 'new\\n' > value.txt", false)
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitOK, "verification passed")
	agent, _ := os.ReadFile(filepath.Join(marks, "agent"))
	agentCache := strings.TrimSpace(string(agent))
	if !strings.HasPrefix(agentCache, filepath.Join(f.data, "workspaces")) || !strings.HasSuffix(agentCache, "go-build") {
		t.Errorf("the agent's GOCACHE = %q, want its workspace's own", agentCache)
	}
	own, tmp := filepath.Join(f.data, "cache", "go-build"), filepath.Join(f.data, "cache", "tmp")
	lines := func(name string) []string {
		data, _ := os.ReadFile(filepath.Join(marks, name))
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	// Validation (without the solution, then with it) and grading use Agentium's own cache and temporary folder; the
	// run's setup warms the agent's cache.
	line := func(cache string) string { return cache + " " + tmp + " " + tmp }
	want := map[string][]string{"setup": {line(own), line(own), line(agentCache)}, "verify": {line(own), line(own), line(own)}}
	for name, w := range want {
		if got := lines(name); !slices.Equal(got, w) {
			t.Errorf("%s ran with (GOCACHE TMPDIR GOTMPDIR) %q, want %q", name, got, w)
		}
	}
	if info, err := os.Stat(tmp); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("Agentium's temporary folder: %v, %v; want it owner-only", info, err)
	}
}
