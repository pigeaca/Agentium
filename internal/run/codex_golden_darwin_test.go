//go:build darwin

package run

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/codex"
)

// fakeCodexName is the name the test binary runs under as the fake Codex of the tests below: a link to the test
// binary, as the fake Claude Code of TestClaudeInvocationGolden, so the fake sees exactly the arguments, environment,
// working folder and standard input a run gives Codex.
const fakeCodexName = "codex-golden-fake"

// spendForever, in a run's prompt, makes the fake Codex spend $0.40 a request until it is interrupted.
const spendForever = "AGENTIUM-SPEND-FOREVER"

// leaveAChild, in a run's prompt, makes the fake Codex leave a process in a session of its own (as unified exec's
// commands run), in the checkout, which outlives Codex and its process group: the stream names its ID.
const leaveAChild = "AGENTIUM-LEAVE-A-CHILD"

// fakeThread is the fake Codex's thread.
const fakeThread = "00000000-0000-7000-8000-00000000f00d"

func init() {
	if filepath.Base(os.Args[0]) == fakeCodexName {
		os.Exit(fakeCodex())
	}
}

// fakeCodex answers --version and login status as Codex 0.160.0 signed in to ChatGPT. As exec, it prints what it was
// given as one stream event (which Parse ignores), starts a thread, writes the session's rollout in CODEX_HOME's
// sessions folder as Codex does (session_meta, turn_context, a request), a shell snapshot, the final message (-o), and
// completes the turn. With spendForever in its prompt it keeps spending until SIGINT, then records the abort and exits
// 1, as Codex does.
func fakeCodex() int {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "--version":
		fmt.Println("codex-cli 0.160.0")
		return 0
	case len(args) > 1 && args[0] == "login" && args[1] == "status":
		fmt.Fprintln(os.Stderr, "Logged in using ChatGPT") // on standard error, as Codex 0.160.0 prints it
		return 0
	}
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, syscall.SIGINT)
	stdin, _ := io.ReadAll(os.Stdin)
	cwd, _ := os.Getwd()
	given, _ := json.Marshal(map[string]any{"type": goldenEvent, "args": args, "env": os.Environ(), "cwd": cwd, "stdin": string(stdin)})
	fmt.Println(string(given))
	fmt.Printf(`{"type":"thread.started","thread_id":%q}`+"\n", fakeThread)
	fmt.Println(`{"type":"turn.started"}`)
	value := func(flag string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	effort := ""
	for i, a := range args {
		if i > 0 && args[i-1] == "-c" && strings.HasPrefix(a, "model_reasoning_effort=") {
			effort, _ = strconv.Unquote(strings.TrimPrefix(a, "model_reasoning_effort="))
		}
	}
	home := os.Getenv("CODEX_HOME")
	rollout := filepath.Join(home, "sessions", "2026", "10", "04", "rollout-2026-10-04T15-00-00-"+fakeThread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	write := func(line string) {
		f, err := os.OpenFile(rollout, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			f.WriteString(line + "\n")
			f.Close()
		}
	}
	write(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"cli_version":"0.160.0","timestamp":%q,"creator_user_id":"user-private","creator_account_id":"account-private"}}`,
		fakeThread, cwd, time.Now().UTC().Format("2006-01-02T15:04:05.000Z")))
	write(fmt.Sprintf(`{"type":"turn_context","payload":{"model":%q,"effort":%q,"approval_policy":"never","sandbox_policy":{"type":"workspace-write","network_access":false},"active_permission_profile":{"id":"agentium"}}}`,
		value("-m"), effort))
	usage := func(input, output int64) string {
		return fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":0}`, input, output)
	}
	if strings.Contains(string(stdin), leaveAChild) {
		child := exec.Command("/bin/sleep", "600")
		child.Dir, child.SysProcAttr = cwd, &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err == nil {
			fmt.Printf(`{"type":"item.completed","item":{"id":"item_9","type":"command_execution","command":"nohup sleep 600 & # pid %d","aggregated_output":"","exit_code":0,"status":"completed"}}`+"\n", child.Process.Pid)
		}
	}
	_ = os.MkdirAll(filepath.Join(home, "shell_snapshots"), 0o700)
	_ = os.WriteFile(filepath.Join(home, "shell_snapshots", fakeThread+".1.sh"), []byte("export FROM_THE_SHELL=1\n"), 0o600)
	if strings.Contains(string(stdin), spendForever) {
		fmt.Println(`{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"sleep 900","status":"in_progress"}}`)
		for {
			write(`{"type":"token_usage_record","payload":{"usage":` + usage(200_000, 0) + `}}`)
			select {
			case <-interrupted:
				write(`{"type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted","duration_ms":1000}}`)
				return 1
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	write(`{"type":"token_usage_record","payload":{"usage":` + usage(10_000, 100) + `}}`)
	write(`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":3.0,"window_minutes":300,"resets_at":1791143360},"plan_type":"plan-private"}}}`)
	write(`{"type":"event_msg","payload":{"type":"task_complete","duration_ms":1000}}`)
	if last := value("-o"); last != "" {
		_ = os.WriteFile(last, []byte("done\n"), 0o600)
	}
	fmt.Println(`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"done"}}`)
	fmt.Println(`{"type":"turn.completed","usage":` + usage(10_000, 100) + `}`)
	return 0
}

// TestCodexInvocationGolden pins exactly what a run gives Codex, end to end through Once: its arguments (every -c
// override, the permission profile's paths one per line, the trust pinned), its environment (the allowlist, the
// run-local HOME and temp root, CODEX_HOME, the key in API key mode only), its standard input (the prompt), the folder
// it starts in, and what the run leaves in the Codex home (its session moved out, its shell snapshot gone, the sign-in
// untouched). A change here is a change to what Codex runs with, a security change for the denied paths and the
// environment, to be checked line by line, never regenerated blindly.
func TestCodexInvocationGolden(t *testing.T) {
	var out strings.Builder
	for _, c := range codexGoldenCases() {
		fmt.Fprintf(&out, "== %s\n%s", c.name, captureCodex(t, c))
	}
	golden := filepath.Join("testdata", "codex-invocation.golden")
	if *update {
		must(t, os.WriteFile(golden, []byte(out.String()), 0o600))
	}
	want, err := os.ReadFile(golden)
	must(t, err)
	if out.String() != string(want) {
		t.Errorf("what Codex is given changed (rerun with -update only if that is intended, and review every line):\n%s", out.String())
	}
}

type codexGoldenCase struct {
	name, module, decoy, signIn, model, effort string
	budget                                     float64
	environ                                    []string // added to the user's environment
}

func codexGoldenCases() []codexGoldenCase {
	return []codexGoldenCase{
		{name: "root task, ChatGPT login, gpt-6.1-sol at its default effort, a $3 cap", signIn: codex.SignInLogin, model: "gpt-6.1-sol", budget: 3},
		{name: "module task (svc), API key, gpt-6.1-sol at effort high, a $2.5 cap, the user's own CODEX_HOME and CLAUDE_CONFIG_DIR",
			module: "svc", decoy: "decoy", signIn: codex.SignInAPIKey, model: "gpt-6.1-sol", effort: "high", budget: 2.5,
			environ: []string{"CLAUDE_CONFIG_DIR=<HOME>/.claude-work", "CODEX_API_KEY=parent-codex-key", "DECOY_PASSWORD=parent-password"}},
	}
}

// codexOnce is a run fixture (newModuleOnce) with the fake Codex as its agent, its data folder's Codex home signed in
// (a sentinel sign-in that must stay untouched) and the user's own ~/.codex (another one).
type codexOnce struct {
	moduleOnce
	dir, temps, exe string
	sentinels       map[string]string // path: content
}

func newCodexOnce(t *testing.T, module, decoy, signIn string, files ...map[string]string) codexOnce {
	t.Helper()
	if decoy == "" {
		decoy = "decoy"
	}
	var base map[string]string
	if len(files) > 0 {
		base = files[0]
	}
	f := codexOnce{moduleOnce: newModuleOnceWith(t, module, decoy, "", base)}
	f.dir = filepath.Dir(f.env.Home)
	f.temps = shortTemp(t)
	t.Cleanup(func() { os.RemoveAll(f.temps) })
	f.env.Layout.Temp = f.temps
	fake := filepath.Join(f.dir, "bin", fakeCodexName)
	must(t, os.MkdirAll(filepath.Dir(fake), 0o700))
	exe, err := os.Executable()
	must(t, err)
	f.exe = exe
	must(t, os.Symlink(exe, fake))
	f.env.CLI, f.env.Agent, f.env.SignIn = fake, codex.Adapter{}, signIn
	if signIn == codex.SignInAPIKey {
		f.env.Secret = "golden-codex-key-not-real"
	}
	f.sentinels = map[string]string{filepath.Join(f.env.Home, ".codex", "auth.json"): `{"user's own":"never opened"}`}
	if signIn == codex.SignInLogin {
		f.sentinels[filepath.Join(f.env.Layout.CodexHome(), "auth.json")] = `{"Agentium's login":"never opened"}`
	}
	for p, content := range f.sentinels {
		must(t, os.MkdirAll(filepath.Dir(p), 0o700))
		must(t, os.WriteFile(p, []byte(content), 0o600))
	}
	f.env.Environ = claudeUser(f.env.Home)
	f.spec.Model, f.spec.BudgetUSD = "gpt-6.1-sol", 3
	return f
}

// checkSentinels fails when a sign-in sentinel changed, or when anything appeared in the user's ~/.codex.
func (f codexOnce) checkSentinels(t *testing.T) {
	t.Helper()
	for p, content := range f.sentinels {
		if data, err := os.ReadFile(p); err != nil || string(data) != content {
			t.Errorf("%s changed: %q, %v", p, data, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(f.env.Home, ".codex"))
	if err != nil || len(entries) != 1 {
		t.Errorf("the user's ~/.codex holds %d entries, %v", len(entries), err)
	}
}

// captureCodex runs case c through Once with the fake Codex and returns what the fake was given, normalized.
func captureCodex(t *testing.T, c codexGoldenCase) string {
	t.Helper()
	f := newCodexOnce(t, c.module, c.decoy, c.signIn)
	for _, kv := range c.environ {
		f.env.Environ = append(f.env.Environ, strings.ReplaceAll(kv, "<HOME>", f.env.Home))
	}
	f.env.DenyExtra = []string{"/golden/predicted/other-run"}
	f.spec.Model, f.spec.Effort, f.spec.BudgetUSD = c.model, c.effort, c.budget
	f.spec.Keep = true
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	if rec.Outcome != "ok" || rec.Agent != "codex" || rec.CostSource != CostPricedByAgentium || rec.PriceTable != "2026-10-04" ||
		rec.Metrics.Rollouts != 1 || rec.Metrics.CostUSD <= 0 || rec.Metrics.Effort == "" {
		t.Fatalf("%s: outcome %s, record %+v, notes %v", c.name, rec.Outcome, rec.Metrics, rec.Notes)
	}
	f.checkSentinels(t)
	var given struct {
		Args  []string `json:"args"`
		Env   []string `json:"env"`
		CWD   string   `json:"cwd"`
		Stdin string   `json:"stdin"`
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
		t.Fatalf("%s: the fake Codex did not report what it was given", c.name)
	}

	uidName := regexp.MustCompile(`\b(claude|cc-socks|cc-daemon)-` + strconv.Itoa(os.Getuid()) + `\b`)
	runTemp := f.env.Layout.RunTemp(f.env.workspaceName())
	replace := map[string]string{runTemp: "<RUNTEMP>", realOf(runTemp): "<RUNTEMP>", f.temps: "<TEMPS>", realOf(f.temps): "<TEMPS>",
		f.dir: "<T>", realOf(f.dir): "<T>", f.env.ID: "<ID>", f.exe: "<TEST-BINARY>"}
	keys := make([]string, 0, len(replace))
	for k := range replace {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return len(keys[i]) > len(keys[j]) || len(keys[i]) == len(keys[j]) && keys[i] < keys[j]
	})
	normalize := func(s string) string {
		for _, k := range keys {
			s = strings.ReplaceAll(s, k, replace[k])
		}
		return uidName.ReplaceAllString(s, "${1}-<UID>")
	}
	var b strings.Builder
	lines := func(list []string) { // a path and its resolved form, side by side, collapse into one line
		prev := ""
		for _, l := range list {
			if l = normalize(l); l != prev {
				fmt.Fprintf(&b, "%s\n", l)
			}
			prev = l
		}
	}
	fmt.Fprintf(&b, "-- working folder\n%s\n", normalize(given.CWD))
	fmt.Fprintf(&b, "-- args\n")
	for i, a := range given.Args {
		key, value, _ := strings.Cut(a, "=")
		if i == 0 || given.Args[i-1] != "-c" || !strings.HasPrefix(value, "{") || !strings.Contains(key, "filesystem") && key != "projects" {
			fmt.Fprintf(&b, "%s\n", normalize(a))
			continue
		}
		// The permission profile and the trusted projects: one entry per line.
		fmt.Fprintf(&b, "%s={\n", key)
		var entries []string
		for _, e := range splitTable(strings.TrimSuffix(strings.TrimPrefix(value, "{"), "}")) {
			entries = append(entries, "  "+e)
		}
		lines(entries)
		fmt.Fprintf(&b, "}\n")
	}
	fmt.Fprintf(&b, "-- stdin\n%s\n", normalize(strconv.Quote(given.Stdin)))
	fmt.Fprintf(&b, "-- env\n")
	lines(given.Env)
	fmt.Fprintf(&b, "-- the Codex home after the run (CODEX_HOME)\n")
	codexHome := f.env.Layout.CodexHome()
	if c.signIn == codex.SignInAPIKey {
		codexHome = filepath.Join(f.env.Layout.Workspaces, f.env.workspaceName(), "codex-home")
	}
	lines(filesIn(t, codexHome))
	fmt.Fprintf(&b, "-- the run's records (without the grading copy)\n")
	var records []string
	for _, f := range filesIn(t, rec.RecordsDir) {
		if !strings.HasPrefix(f, "verify/") {
			records = append(records, f)
		}
	}
	lines(records)
	return b.String()
}

// splitTable splits an inline table's body at the commas between its entries (none is inside a quoted key here but a
// path's; the entries' values are bare words, strings or tables).
func splitTable(body string) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\\' && quoted:
			i++
		case c == '"':
			quoted = !quoted
		case !quoted && c == '{':
			depth++
		case !quoted && c == '}':
			depth--
		case !quoted && depth == 0 && c == ',':
			out = append(out, body[start:i])
			start = i + 1
		}
	}
	return append(out, body[start:])
}

// filesIn lists the regular files under dir, relative and sorted; nothing for a missing folder.
func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// A Codex run that keeps spending is stopped by Agentium's cap while one more full-context request still fits under
// it: capped, its spend read from the rollout (never above the cap), and its session gathered.
func TestCodexRunStopsAtTheCap(t *testing.T) {
	f := newCodexOnce(t, "", "decoy", codex.SignInLogin)
	f.spec.Instruction = "Spend: " + spendForever
	f.spec.BudgetUSD = 3 // the allowance is $1.80: stopped once $1.20 is spent, after the third $0.40 request
	f.env.Grace = 10 * time.Second
	start := time.Now()
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	spent := rec.Spend().AgentUSD
	allowance, _ := codex.Allowance("gpt-6.1-sol")
	if rec.Outcome != agent.OutcomeCapped || spent+allowance <= f.spec.BudgetUSD || spent > f.spec.BudgetUSD || time.Since(start) > time.Minute {
		t.Errorf("outcome %s, spent $%.2f of a $%.2f cap (allowance $%.2f) after %v, notes %v", rec.Outcome, spent, f.spec.BudgetUSD, allowance, time.Since(start), rec.Notes)
	}
	if rec.Metrics.Rollouts != 1 || rec.Metrics.DurationMS != 1000 {
		t.Errorf("the interrupted session's rollout: %+v", rec.Metrics)
	}
	if entries := filesIn(t, filepath.Join(f.env.Layout.CodexHome(), "sessions")); len(entries) != 0 {
		t.Errorf("left in the shared home: %q", entries)
	}
	f.checkSentinels(t)
}

// What a Codex run refuses before anything is spent: a Gradle project (whatever its local-binding opt-in), and a
// judge.
func TestCodexRunRefusals(t *testing.T) {
	f := newCodexOnce(t, "", "decoy", codex.SignInLogin)
	gradle := newModuleOnceWith(t, "", "decoy", "", map[string]string{"build.gradle": "plugins {}\n"})
	gradle.env.CLI, gradle.env.Agent, gradle.env.AllowLocalBinding, gradle.spec.Model = f.env.CLI, codex.Adapter{}, true, "gpt-6.1-sol"
	if _, err := Once(context.Background(), gradle.env, gradle.spec); err == nil || !strings.Contains(err.Error(), "Gradle") {
		t.Errorf("a Gradle project: %v", err)
	}
	f.spec.Task.Grading = "judge"
	if _, err := Once(context.Background(), f.env, f.spec); err == nil || !strings.Contains(err.Error(), "judge") {
		t.Errorf("a judge-graded task: %v", err)
	}
}

// What Codex's commands leave running (each command runs in a session of its own, outside the process group the
// runner kills) is stopped once Codex ends: a child in its own session, in the checkout, is gone after Once, and the
// record says so.
func TestCodexRunSweepsLeftoverProcesses(t *testing.T) {
	f := newCodexOnce(t, "", "decoy", codex.SignInLogin)
	f.spec.Instruction = "Leave one: " + leaveAChild
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := os.ReadFile(filepath.Join(rec.RecordsDir, "stream.jsonl"))
	must(t, err)
	match := regexp.MustCompile(`# pid (\d+)`).FindSubmatch(stream)
	if match == nil {
		t.Fatalf("the fake left no child: %s", stream)
	}
	pid, _ := strconv.Atoi(string(match[1]))
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	gone := false
	for range 40 {
		if err := syscall.Kill(pid, 0); err != nil {
			gone = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gone {
		t.Errorf("process %d, which Codex's command left in the checkout, outlived the run (notes %q)", pid, rec.Notes)
	}
	if !strings.Contains(strings.Join(rec.Notes, " "), "stopped 1 process(es) Codex's commands left running") {
		t.Errorf("the record does not say what was stopped: %q", rec.Notes)
	}
}

// A trusted checkout's Codex configuration is checked before the agent starts: one that could change the run's
// sandbox, network, environment or hooks is refused with nothing spent and no transcript; one with only the allowed
// settings runs, and the run's own model and effort are what Codex is given.
func TestCodexRunRefusesAProjectConfig(t *testing.T) {
	for name, config := range map[string]string{
		"network":     "[permissions.agentium.network]\nenabled = true\n",
		"the sandbox": "sandbox_mode = \"danger-full-access\"\n",
		"hooks":       "[features]\nhooks = true\n",
	} {
		f := newCodexOnce(t, "", "decoy", codex.SignInLogin, map[string]string{".codex/config.toml": config})
		rec, err := Once(context.Background(), f.env, f.spec)
		if err == nil || !strings.Contains(err.Error(), "Codex merges a trusted project's configuration") {
			t.Errorf("%s: %v", name, err)
		}
		if _, statErr := os.Stat(filepath.Join(rec.RecordsDir, "stream.jsonl")); statErr == nil || rec.Spend().AgentUSD != 0 {
			t.Errorf("%s: the agent started", name)
		}
	}
	f := newCodexOnce(t, "", "decoy", codex.SignInLogin, map[string]string{".codex/config.toml": "model = \"gpt-5.5\"\nmodel_reasoning_effort = \"high\"\n"})
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil || rec.Outcome != agent.OutcomeOK || rec.Metrics.Model != "gpt-6.1-sol" || rec.Metrics.Effort != "low" {
		t.Errorf("allowed settings: %v, outcome %s, model %s, effort %s, notes %q", err, rec.Outcome, rec.Metrics.Model, rec.Metrics.Effort, rec.Notes)
	}
}
