// Package run executes one agent run on a task and grades it. The agent works in a workspace prepared as the arm: a
// checkout of the task's base holding only that commit, the arm's context, the task's setup, then a context commit the
// agent's changes are measured from. Claude Code runs isolated (internal/claude) and denied everything else in the
// data folder and the user's repository. Grading happens afterwards on a copy in the run's records, which the agent
// could never read: the hidden tests are added there and the verification commands run.
package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/task"
)

// Spec is what to run.
type Spec struct {
	TaskName    string
	Instruction string
	Task        task.Spec // base, solution, hidden tests, reference, setup, verification
	Arm         task.Arm
	Model       string
	Effort      string
	BudgetUSD   float64
	Timeout     time.Duration // the agent's run
	Keep        bool          // keep the workspace and the verification copy
}

// Env is what a run needs from Agentium and the machine.
type Env struct {
	ID            string // from NewID
	Layout        home.Layout
	Bare          string // the project's bare repository
	ProjectRoot   string // the user's repository: the agent may not read it
	CLI           string // the claude executable
	Home          string
	Environ       []string // the parent's environment; the run gets an allowlisted part
	SignIn        string   // claude.SignInAPIKey, SignInTokenFile or SignInLogin
	Secret        string   // for API key and token sign-in; redacted from every record
	TokenFile     string
	VerifyTimeout time.Duration // each setup or verification command
	Grace         time.Duration // between SIGINT and SIGKILL when the agent is stopped
	Progress      io.Writer
	Now           func() time.Time
}

// Record is a finished run.
type Record struct {
	ID          string         `json:"id"`
	Task        string         `json:"task"`
	Arm         string         `json:"arm"`
	Snapshot    string         `json:"snapshot,omitempty"`
	Model       string         `json:"model"`
	SignIn      string         `json:"sign_in"`
	Outcome     string         `json:"outcome"`          // claude.Outcome*
	Passed      *bool          `json:"passed,omitempty"` // the verification with hidden tests; nil when it did not run
	Drift       []string       `json:"drift,omitempty"`
	Notes       []string       `json:"notes,omitempty"`
	Metrics     claude.Metrics `json:"metrics"`
	Behavior    Behavior       `json:"behavior"`
	Setup       []task.Command `json:"setup,omitempty"`
	Verify      []task.Command `json:"verify,omitempty"`
	ExitCode    int            `json:"exit_code"`
	Started     time.Time      `json:"started"`
	Finished    time.Time      `json:"finished"`
	RecordsDir  string         `json:"records"`
	ContextHead string         `json:"context_commit,omitempty"`
}

// Behavior is what the agent did, beyond passing or failing.
type Behavior struct {
	FilesChanged int  `json:"files_changed"`
	LinesAdded   int  `json:"lines_added"`
	LinesRemoved int  `json:"lines_removed"`
	TestsChanged bool `json:"tests_changed"` // changed a test file
	RanTests     bool `json:"ran_tests"`     // ran a test runner
	RanChecks    bool `json:"ran_checks"`    // ran one of the task's verification commands
	Commits      int  `json:"commits"`       // commits on top of the context commit
	BashCommands int  `json:"bash_commands"`
	Denials      int  `json:"denials"`
	OutsideReads int  `json:"outside_reads"` // file tool calls on Agentium's data, the user's repository or Claude's data
}

// NewID makes a run id: a UTC timestamp and a random suffix, so ids sort by start time.
func NewID(now time.Time) (string, error) {
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("run id: %w", err)
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix), nil
}

// suffix tells the agent how to work in the run's checkout.
const suffix = "\n\nYou are working in this task's own checkout of the repository. Make the change here, in the working " +
	"tree. Do not commit, push, open a pull request, or create branches or worktrees. When you are done, reply with a " +
	"short summary of what you changed and how you verified it."

// Once runs spec once and grades it. The error is for runs that could not be carried out (bad setup of Agentium
// itself, cancellation); an agent's failure is a Record.
func Once(ctx context.Context, env Env, spec Spec) (Record, error) {
	rec := Record{ID: env.ID, Task: spec.TaskName, Arm: spec.Arm.Name, Snapshot: spec.Arm.Snapshot, Model: spec.Model,
		SignIn: env.SignIn, Started: env.Now().UTC(), RecordsDir: filepath.Join(env.Layout.Records, env.ID)}
	finish := func(err error) (Record, error) {
		rec.Finished = env.Now().UTC()
		return rec, err
	}
	workspace := filepath.Join(env.Layout.Workspaces, env.ID)
	repo := filepath.Join(workspace, "repo")
	if found := instructionFilesAbove(repo); len(found) > 0 {
		return finish(fmt.Errorf("%s: Claude Code would load it into every run from above the workspace; move it, or set AGENTIUM_HOME elsewhere", strings.Join(found, ", ")))
	}
	if err := os.MkdirAll(rec.RecordsDir, 0o700); err != nil {
		return finish(fmt.Errorf("run records: %w", err))
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return finish(fmt.Errorf("run workspace: %w", err))
	}
	if !spec.Keep {
		defer os.RemoveAll(workspace)
	}
	env.progress("Run %s: task %s, arm %s, model %s, sign-in %s", env.ID, spec.TaskName, spec.Arm.Name, spec.Model, env.SignIn)

	// The workspace: the base, the arm's context, the setup, then the context commit.
	if err := checkout.New(ctx, env.Bare, spec.Task.Base, repo); err != nil {
		return finish(err)
	}
	if spec.Arm.Snapshot != "" {
		base, err := source.Commit(ctx, spec.Task.Base, "--git-dir", env.Bare)
		if err != nil {
			return finish(err)
		}
		snap, err := source.Commit(ctx, spec.Arm.Snapshot, "--git-dir", env.Bare)
		if err != nil {
			return finish(err)
		}
		overlay, err := snapshot.PlanOverlay(base, snap)
		if err != nil {
			return finish(fmt.Errorf("arm %s: %w", spec.Arm.Name, err))
		}
		if err := checkout.Write(repo, snap, append(overlay.Writes, overlay.Deletes...)); err != nil {
			return finish(fmt.Errorf("arm %s: %w", spec.Arm.Name, err))
		}
		if len(overlay.HarnessChanged) > 0 {
			rec.Notes = append(rec.Notes, "the arm changes what runs: "+strings.Join(overlay.HarnessChanged, ", "))
		}
	}
	if len(spec.Task.Setup) > 0 {
		var ok bool
		var err error
		if rec.Setup, ok, err = env.commands(ctx, repo, spec.Task.Setup, filepath.Join(rec.RecordsDir, "setup.log")); err != nil {
			return finish(err)
		}
		if !ok {
			rec.Outcome = claude.OutcomeInfra
			rec.Notes = append(rec.Notes, "setup failed: see setup.log")
			return finish(env.redactRecords(rec.RecordsDir))
		}
	}
	// Setup outputs that git does not ignore are part of the starting point, not the agent's work.
	if _, err := gitx.Run(ctx, "-C", repo, "add", "-A"); err != nil {
		return finish(err)
	}
	if _, err := gitx.Run(ctx, "-C", repo, "-c", "user.name=agentium", "-c", "user.email=agentium@localhost", "-c", "commit.gpgsign=false",
		"commit", "--quiet", "--allow-empty", "--no-verify", "-m", "agentium: context "+spec.Arm.Name); err != nil {
		return finish(err)
	}
	var err error
	if rec.ContextHead, err = gitx.Run(ctx, "-C", repo, "rev-parse", "HEAD"); err != nil {
		return finish(err)
	}

	// The agent.
	inv := claude.Invocation{CLI: env.CLI, Dir: repo, Prompt: spec.Instruction + suffix, Model: spec.Model, Effort: spec.Effort,
		BudgetUSD: spec.BudgetUSD, SignIn: env.SignIn, Secret: env.Secret, TokenFile: env.TokenFile, Home: env.Home,
		Deny: env.denied(workspace)}
	if env.SignIn != claude.SignInLogin {
		inv.ConfigDir = filepath.Join(workspace, "config")
		if err := os.MkdirAll(inv.ConfigDir, 0o700); err != nil {
			return finish(fmt.Errorf("run config folder: %w", err))
		}
	}
	transcriptPath := filepath.Join(rec.RecordsDir, "stream.jsonl")
	transcript, err := os.Create(transcriptPath)
	if err != nil {
		return finish(fmt.Errorf("run transcript: %w", err))
	}
	stderr, err := os.Create(filepath.Join(rec.RecordsDir, "stderr.txt"))
	if err != nil {
		transcript.Close()
		return finish(fmt.Errorf("run transcript: %w", err))
	}
	env.progress("  workspace ready; Claude Code is working (up to %s)", spec.Timeout)
	result, runErr := claude.Run(ctx, inv, env.Environ, transcript, stderr, spec.Timeout, env.Grace)
	transcript.Close()
	stderr.Close()
	rec.ExitCode = result.ExitCode
	if err := env.redactRecords(rec.RecordsDir); err != nil {
		return finish(err)
	}
	if runErr != nil {
		rec.Outcome = claude.OutcomeInfra
		rec.Notes = append(rec.Notes, "the run did not finish: "+runErr.Error())
		return finish(runErr)
	}
	if rec.Metrics, err = parseFile(transcriptPath); err != nil {
		return finish(err)
	}
	userConfig := claude.UserConfigDir(env.Environ, env.Home)
	projectSkills, err := skillNames(ctx, repo)
	if err != nil {
		return finish(err)
	}
	rec.Drift = claude.Check(rec.Metrics, claude.Expect{PersonalSkills: claude.PersonalSkills(userConfig), ProjectSkills: projectSkills})
	rec.Behavior.OutsideReads = outsideReads(rec.Metrics.FilePaths, repo, workspace,
		[]string{env.Layout.Root, env.ProjectRoot, filepath.Join(env.Home, ".claude"), userConfig})
	if rec.Behavior.OutsideReads > 0 {
		rec.Drift = append(rec.Drift, fmt.Sprintf("%d file tool call(s) reached Agentium's data, the repository or Claude's data", rec.Behavior.OutsideReads))
	}
	rec.Outcome = claude.Classify(rec.Metrics, result.TimedOut, rec.Drift)
	env.progress("  Claude Code: %s, $%.2f, %d turn(s)", rec.Outcome, rec.Metrics.CostUSD, rec.Metrics.Turns)

	// Grading, on a copy the agent never saw.
	if err := env.grade(ctx, spec, repo, &rec); err != nil {
		return finish(err)
	}
	if err := env.redactRecords(rec.RecordsDir); err != nil {
		return finish(err)
	}
	return finish(nil)
}

// grade copies the workspace into the records, measures the agent's changes from the context commit, adds the hidden
// tests and runs the verification commands.
func (env Env) grade(ctx context.Context, spec Spec, repo string, rec *Record) error {
	copyDir := filepath.Join(rec.RecordsDir, "verify")
	if !spec.Keep {
		defer os.RemoveAll(copyDir)
	}
	if err := copyTree(repo, copyDir); err != nil {
		return fmt.Errorf("verification copy: %w", err)
	}
	if _, err := gitx.Run(ctx, "-C", copyDir, "add", "-A"); err != nil {
		return err
	}
	numstat, err := gitx.Output(ctx, nil, "-C", copyDir, "diff", "--cached", "--numstat", "-z", "--no-renames", rec.ContextHead)
	if err != nil {
		return err
	}
	changed := measure(string(numstat), &rec.Behavior)
	patch, err := gitx.Output(ctx, nil, "-C", copyDir, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-color", rec.ContextHead)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(rec.RecordsDir, "agent.diff"), patch, 0o600); err != nil {
		return fmt.Errorf("agent diff: %w", err)
	}
	commits, err := gitx.Run(ctx, "-C", copyDir, "rev-list", "--count", rec.ContextHead+"..HEAD")
	if err != nil {
		return err
	}
	rec.Behavior.Commits, _ = strconv.Atoi(commits)
	rec.Behavior.BashCommands = len(rec.Metrics.Commands)
	rec.Behavior.Denials = rec.Metrics.Denials
	rec.Behavior.RanTests = ranTests(rec.Metrics.Commands)
	rec.Behavior.RanChecks = ranChecks(rec.Metrics.Commands, spec.Task.Verify)
	for _, p := range changed {
		rec.Behavior.TestsChanged = rec.Behavior.TestsChanged || task.IsTestFile(p)
	}
	if spec.Task.Solution != "" && len(spec.Task.HiddenTests) > 0 {
		solution, err := source.Commit(ctx, spec.Task.Solution, "--git-dir", env.Bare)
		if err != nil {
			return err
		}
		if err := checkout.Write(copyDir, solution, spec.Task.HiddenTests); err != nil {
			return fmt.Errorf("hidden tests: %w", err)
		}
	}
	commands, ok, err := env.commands(ctx, copyDir, spec.Task.Verify, filepath.Join(rec.RecordsDir, "verify.log"))
	rec.Verify = commands
	if err != nil {
		return err
	}
	rec.Passed = &ok
	env.progress("  verification: %s", map[bool]string{true: "passed", false: "failed"}[ok])
	return nil
}

// commands runs shell commands in dir until one fails, logging to logPath.
func (env Env) commands(ctx context.Context, dir string, commands []string, logPath string) ([]task.Command, bool, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("log: %w", err)
	}
	defer log.Close()
	var results []task.Command
	for _, command := range commands {
		fmt.Fprintf(log, "$ %s\n", command)
		result, err := runner.Run(ctx, runner.Spec{Dir: dir, Command: command, Timeout: env.VerifyTimeout, Output: log})
		results = append(results, task.Command{Command: command, ExitCode: result.ExitCode, TimedOut: result.TimedOut,
			Seconds: result.Duration.Round(time.Millisecond).Seconds()})
		if err != nil {
			return results, false, err
		}
		if !result.Passed() {
			return results, false, nil
		}
	}
	return results, true, nil
}

// denied lists what the agent may not read: Agentium's data except its own workspace (projects and hidden tests,
// records, artifacts, the database, other runs' workspaces), and the user's repository with its git data. Workspaces
// created after this run starts are not listed: a known gap for concurrent runs, which hold no hidden tests.
func (env Env) denied(workspace string) []string {
	db := env.Layout.Database
	paths := []string{filepath.Join(env.Layout.Root, "projects"), env.Layout.Records, env.Layout.Artifacts, db, db + "-wal", db + "-shm",
		env.ProjectRoot}
	if common, err := gitx.Run(context.Background(), "-C", env.ProjectRoot, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		paths = append(paths, filepath.Dir(common), common) // a worktree's shared git data lives elsewhere
	}
	if entries, err := os.ReadDir(env.Layout.Workspaces); err == nil {
		for _, e := range entries {
			if other := filepath.Join(env.Layout.Workspaces, e.Name()); other != workspace {
				paths = append(paths, other)
			}
		}
	}
	return paths
}

func (env Env) progress(format string, args ...any) {
	if env.Progress != nil {
		fmt.Fprintf(env.Progress, format+"\n", args...)
	}
}

// redactRecords removes the sign-in secret and credential-shaped strings from every text record of the run.
func (env Env) redactRecords(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && d.Name() == "verify" {
				return filepath.SkipDir // the verification copy is the agent's work tree, removed after grading
			}
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("redact %s: %w", p, err)
		}
		if clean := Redact(data, env.Secret); len(clean) != len(data) || string(clean) != string(data) {
			if err := os.WriteFile(p, clean, 0o600); err != nil {
				return fmt.Errorf("redact %s: %w", p, err)
			}
		}
		return nil
	})
}

// secretPatterns are credential shapes removed from records (as the pre-commit hook's scan knows them).
var secretPatterns = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}|\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,}|\bAKIA[0-9A-Z]{16}\b|` +
	`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})|\bxox[abprs]-[A-Za-z0-9-]{10,}|` +
	`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)

// Redact replaces secret, when not empty, and credential-shaped strings with [REDACTED].
func Redact(data []byte, secret string) []byte {
	text := string(data)
	if secret != "" {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return []byte(secretPatterns.ReplaceAllString(text, "[REDACTED]"))
}

func parseFile(p string) (claude.Metrics, error) {
	f, err := os.Open(p)
	if err != nil {
		return claude.Metrics{}, fmt.Errorf("run transcript: %w", err)
	}
	defer f.Close()
	return claude.Parse(f)
}

// skillNames lists the project skill names of the arm's context in repo.
func skillNames(ctx context.Context, repo string) ([]string, error) {
	src, err := source.WorkingTree(ctx, repo)
	if err != nil {
		return nil, err
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return nil, err
	}
	return claudectx.SkillNames(resolved, src), nil
}

// instructionFilesAbove lists instruction files in the folders above dir, which Claude Code would load into a run.
func instructionFilesAbove(dir string) []string {
	var found []string
	for d := filepath.Dir(dir); d != filepath.Dir(d); d = filepath.Dir(d) {
		for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"} {
			if info, err := os.Stat(filepath.Join(d, name)); err == nil && !info.IsDir() {
				found = append(found, filepath.Join(d, name))
			}
		}
	}
	return found
}

// outsideReads counts file tool paths inside a watched root but outside the run's workspace. Relative paths are the
// checkout's own.
func outsideReads(paths []string, repo, workspace string, watched []string) int {
	inside := func(p, root string) bool {
		rel, err := filepath.Rel(root, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	resolve := realPath // a path to a missing file must still match its root's resolved form
	count := 0
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(repo, p)
		}
		p = resolve(p)
		if inside(p, resolve(workspace)) {
			continue
		}
		for _, root := range watched {
			if root != "" && inside(p, resolve(root)) {
				count++
				break
			}
		}
	}
	return count
}

// realPath resolves symbolic links in the longest existing prefix of p (/var and /private/var on macOS), so paths to
// files that do not exist compare like the ones that do.
func realPath(p string) string {
	var missing []string
	p = filepath.Clean(p)
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, missing...)...)
		}
		missing = append([]string{filepath.Base(p)}, missing...)
		p = parent
	}
}

// measure reads `git diff --numstat -z` into the behavior counts and returns the changed paths.
func measure(numstat string, b *Behavior) []string {
	var paths []string
	for _, record := range strings.Split(numstat, "\x00") {
		fields := strings.SplitN(record, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		added, _ := strconv.Atoi(fields[0]) // "-" for binary files counts as 0
		removed, _ := strconv.Atoi(fields[1])
		b.FilesChanged++
		b.LinesAdded += added
		b.LinesRemoved += removed
		paths = append(paths, fields[2])
	}
	return paths
}

// testRunner matches commands that run tests.
var testRunner = regexp.MustCompile(`\b(go test|pytest|python3? -m (pytest|unittest)|(npm|pnpm|yarn|bun) (run )?test|jest|vitest|` +
	`cargo test|make test|mvn( -\S+)* test|gradlew? test|rspec|dotnet test|harness\.py check)\b`)

func ranTests(commands []string) bool {
	for _, c := range commands {
		if testRunner.MatchString(c) {
			return true
		}
	}
	return false
}

func ranChecks(commands, verify []string) bool {
	for _, c := range commands {
		for _, v := range verify {
			if strings.Contains(c, strings.TrimSpace(v)) {
				return true
			}
		}
	}
	return false
}

// copyTree copies src to dst (which must not exist), keeping modes and symbolic links as links.
func copyTree(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return errors.New(dst + " already exists")
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
		return nil // sockets and devices are not copied
	})
}
