package run

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/snapshot"
)

// memSource is an in-memory repository state.
type memSource map[string]string

func (m memSource) Paths() []string {
	var paths []string
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}
func (m memSource) ReadFile(p string) ([]byte, error) {
	if data, ok := m[p]; ok {
		return []byte(data), nil
	}
	return nil, os.ErrNotExist
}
func (m memSource) Executable(string) bool { return false }
func (m memSource) Describe() string       { return "memory" }

// project has every kind of context: an instruction file with an import and a linked document, an unscoped and a
// scoped rule, a skill, a subagent, and files that are not context.
var project = memSource{
	"CLAUDE.md":                      "# Project\n@AGENTS.md\nSee [testing](docs/testing.md).\n",
	"AGENTS.md":                      "Rules\n",
	".claude/rules/always.md":        "Always.\n",
	".claude/rules/go.md":            "---\npaths:\n  - \"**/*.go\"\n---\ngofmt\n",
	".claude/skills/review/SKILL.md": "---\nname: review\ndescription: Review a change.\n---\nSteps.\n",
	".claude/agents/investigator.md": "---\nname: investigator\ndescription: Look around.\n---\nRead.\n",
	"docs/testing.md":                "Run the tests.\n",
	"docs/other.md":                  "Not linked.\n",
	"main.go":                        "package main\n",
}

func TestUseOfKeepsOnlyTheArmsOwnContext(t *testing.T) {
	resolved, err := claudectx.Resolve(project)
	if err != nil {
		t.Fatal(err)
	}
	m := claude.Metrics{
		CWD: "/work/repo",
		ReadPaths: []string{
			"/work/repo/docs/testing.md", // a linked document: used
			"/work/repo/AGENTS.md",       // loaded at start anyway
			"/work/repo/main.go",         // not context
			"/work/repo/docs/other.md",   // a document nothing links to
			"/elsewhere/docs/testing.md", // another folder
		},
		Commands:      []string{"sed -n 1,20p ./.claude/rules/go.md", "cat docs/testing.mdx", "ls .claude/agents"},
		SkillCalls:    []string{"review", "personal-skill", "review"},
		SubagentTypes: []string{"investigator"},
	}
	got := UseOf(resolved, project, m, "/checkout", m.CWD)
	want := ContextUse{Start: []string{".claude/rules/always.md", "AGENTS.md", "CLAUDE.md"}, Files: []string{".claude/rules/go.md", "docs/testing.md"},
		Skills: []string{"review"}, Subagents: []string{"investigator"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("context use = %+v\nwant %+v", got, want)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"/work", "personal-skill"} {
		if strings.Contains(string(data), private) {
			t.Errorf("the record holds %q: %s", private, data)
		}
	}

	// Without any use, Start is still an empty list (not null) and nothing else is set.
	none := UseOf(resolved, project, claude.Metrics{})
	if none.Start == nil || none.Files != nil || none.Skills != nil || none.Subagents != nil {
		t.Errorf("no use: %+v", none)
	}
}

func TestNamedInMatchesWholePaths(t *testing.T) {
	for _, c := range []struct {
		command string
		want    bool
	}{
		{"cat docs/a.md", true},
		{"sed -n 1,40p ./docs/a.md", true},
		{"head /work/repo/docs/a.md", true},
		{`grep -n x "docs/a.md"`, true},
		{"grep -n x docs/a.md:12", true},
		{"cat docs/a.mdx", false},
		{"cat my-docs/a.md", false},
		{"cat docs/a.md.bak", false},
		{"ls docs", false},
	} {
		if got := namedIn("docs/a.md", []string{c.command}); got != c.want {
			t.Errorf("%q: %v, want %v", c.command, got, c.want)
		}
	}
}

// Runs recorded before context use was kept get it from their transcripts: each arm's starting context is the task's
// base, with the arm's snapshot applied.
func TestRecoveryFromTranscripts(t *testing.T) {
	ctx := context.Background()
	user := t.TempDir()
	gitCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", user, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitCmd("init", "-q")
	for p, data := range project {
		if err := os.MkdirAll(filepath.Join(user, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(user, p), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd("add", "-A")
	gitCmd("commit", "-q", "-m", "base")
	base := gitCmd("rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := gitx.FetchCommit(ctx, user, base, gitx.SourceRef(base), "--git-dir", bare); err != nil {
		t.Fatal(err)
	}
	lean, _, err := snapshot.Build(ctx, bare, memSource{"CLAUDE.md": "# Lean\n"}, "snapshot lean", nil)
	if err != nil {
		t.Fatal(err)
	}

	records := filepath.Join(t.TempDir(), "r1")
	if err := os.MkdirAll(records, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"system","subtype":"init","cwd":"/old/workspace/repo","claude_code_version":"2.1.281","model":"claude-sonnet-5"}`,
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-sonnet-5","content":[` +
			`{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/old/workspace/repo/docs/testing.md"}},` +
			`{"type":"tool_use","id":"t2","name":"Skill","input":{"skill":"review"}}]}}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.1}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(records, "stream.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := Record{ID: "r1", Arm: "A", RecordsDir: records}
	recovery := Recovery{Bare: bare}

	own, err := recovery.Recover(ctx, rec, base, "")
	if err != nil {
		t.Fatal(err)
	}
	want := &ContextUse{Start: []string{".claude/rules/always.md", "AGENTS.md", "CLAUDE.md"}, Files: []string{"docs/testing.md"}, Skills: []string{"review"}}
	if !reflect.DeepEqual(own, want) {
		t.Errorf("base arm: %+v\nwant %+v", own, want)
	}
	// The lean arm loads its own CLAUDE.md only, which links nothing and has no skills: the same reads are not its context.
	leanUse, err := recovery.Recover(ctx, rec, base, lean)
	if err != nil {
		t.Fatal(err)
	}
	if want := (&ContextUse{Start: []string{"CLAUDE.md"}}); !reflect.DeepEqual(leanUse, want) {
		t.Errorf("lean arm: %+v\nwant %+v", leanUse, want)
	}

	// A record that has it keeps it; a run without its transcript has none; a base the repository lacks is an error.
	kept := Record{ID: "r2", ContextUse: &ContextUse{Start: []string{"X.md"}}}
	if got, err := recovery.Recover(ctx, kept, base, ""); err != nil || got != kept.ContextUse {
		t.Errorf("kept: %+v, %v", got, err)
	}
	gone := Record{ID: "r3", RecordsDir: filepath.Join(t.TempDir(), "missing")}
	if got, err := recovery.Recover(ctx, gone, base, ""); err != nil || got != nil {
		t.Errorf("no transcript: %+v, %v", got, err)
	}
	if _, err := recovery.Recover(ctx, rec, strings.Repeat("0", 40), ""); err == nil || !strings.Contains(err.Error(), "run r1, arm A") {
		t.Errorf("unknown base: %v", err)
	}
}
