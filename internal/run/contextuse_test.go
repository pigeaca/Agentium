package run

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/gitx/gitxtest"
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
// scoped rule, a folder's instructions, a skill, a command, a subagent, and files that are not context.
var project = memSource{
	"CLAUDE.md":                      "# Project\n@AGENTS.md\nSee [testing](docs/testing.md).\n",
	"AGENTS.md":                      "Rules\n",
	".claude/rules/always.md":        "Always.\n",
	".claude/rules/go.md":            "---\npaths:\n  - \"**/*.go\"\n---\ngofmt\n",
	".claude/rules/sql.md":           "---\npaths: [\"db/*.sql\"]\n---\nNo SELECT *.\n",
	".claude/skills/review/SKILL.md": "---\nname: review\ndescription: Review a change.\n---\nSteps.\n",
	".claude/commands/ship.md":       "---\ndescription: Ship it.\n---\nSteps.\n",
	".claude/agents/investigator.md": "---\nname: investigator\ndescription: Look around.\n---\nRead.\n",
	"pkg/CLAUDE.md":                  "Package rules.\n",
	"docs/testing.md":                "Run the tests.\n",
	"docs/other.md":                  "Not linked.\n",
	"main.go":                        "package main\n",
	"pkg/x.go":                       "package pkg\n",
}

func TestUseOfKeepsOnlyTheArmsOwnContext(t *testing.T) {
	resolved, err := claudectx.Resolve(project)
	if err != nil {
		t.Fatal(err)
	}
	m := agent.Metrics{
		CWD:       "/work/repo",
		FilePaths: []string{"/work/repo/pkg/x.go", "/work/repo/docs/testing.md"}, // pkg/x.go loads go.md and pkg/CLAUDE.md
		ReadPaths: []string{
			"/work/repo/docs/testing.md", // a linked document: used
			"/work/repo/AGENTS.md",       // loaded at start anyway
			"/work/repo/docs/other.md",   // a document nothing links to
			"/elsewhere/docs/testing.md", // another folder
		},
		RanCommands:   []string{"cat docs/other.md", "echo note >> .claude/rules/sql.md", "ls .claude/agents"},
		SkillCalls:    []string{"review", "personal-skill", "ship", "review"},
		SubagentTypes: []string{"Explore", "investigator", "my-personal-agent"},
	}
	got := UseOf(resolved, project, m, "/checkout", m.CWD)
	want := ContextUse{Start: []string{".claude/rules/always.md", "AGENTS.md", "CLAUDE.md"},
		Files:  []string{".claude/rules/go.md", "docs/testing.md", "pkg/CLAUDE.md"},
		Skills: []string{"review", "ship"}, Subagents: []string{"Explore", "investigator"}, OtherSubagents: 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("context use = %+v\nwant %+v", got, want)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"/work", "personal-skill", "my-personal-agent"} {
		if strings.Contains(string(data), private) {
			t.Errorf("the record holds %q: %s", private, data)
		}
	}

	// A rule's file read directly counts too; one whose patterns nothing matched does not.
	read := []string{"sed -n 1,20p ./.claude/rules/sql.md"}
	direct := UseOf(resolved, project, agent.Metrics{Commands: read, RanCommands: read})
	if !reflect.DeepEqual(direct.Files, []string{".claude/rules/sql.md"}) {
		t.Errorf("a rule read with sed: %v", direct.Files)
	}
	// The same command denied (in Commands, not in RanCommands) read nothing.
	if denied := UseOf(resolved, project, agent.Metrics{Commands: read}); denied.Files != nil {
		t.Errorf("a denied sed counted as reading: %v", denied.Files)
	}
	// Without any use, Start is still an empty list (not null) and nothing else is set.
	none := UseOf(resolved, project, agent.Metrics{})
	if none.Start == nil || none.Files != nil || none.Skills != nil || none.Subagents != nil || none.OtherSubagents != 0 {
		t.Errorf("no use: %+v", none)
	}
}

func TestNamedByReader(t *testing.T) {
	for _, c := range []struct {
		command string
		want    bool
	}{
		{"cat docs/a.md", true},
		{"sed -n 1,40p ./docs/a.md", true},
		{"head -50 /work/repo/docs/a.md", true},
		{`grep -n "x y" "docs/a.md"`, true},
		{"cd /work/repo && cat docs/a.md | head", true},
		{"LC_ALL=C sort docs/a.md", true},
		{"/usr/bin/cat docs/a.md", true},
		{"cat docs/a.mdx", false},
		{"cat my-docs/a.md", false},
		{"cat sub/docs/a.md", false},
		{"cat /other/docs/a.md", false},
		{"echo hi >> docs/a.md", false},
		{"cat x.md > docs/a.md", false},
		{"cat x.md >docs/a.md", false},
		{"grep -r y . 2> docs/a.md", false},
		{"git add docs/a.md", false},
		{"ls docs", false},
		{"(cat docs/a.md)", true},
		{"cat docs/a.md>out.txt", true},
		{`grep "a|b" docs/a.md`, true},
		{"grep -rn docs/a.md .", false}, // the pattern, not a file
		{"rg docs/a.md", false},
		{"grep -e x -f docs/a.md src", true},   // -f reads the patterns from it: a read
		{"grep -n -e x docs/a.md", true},       // -e gave the pattern, so docs/a.md is a file
		{"sed -i 's/a/b/' docs/a.md", false},   // a write
		{"sort -o docs/a.md docs/b.md", false}, // sort writes -o's file
		{"sort --output=x.txt docs/a.md", true},
	} {
		if got := namedByReader("docs/a.md", "", []string{c.command}, []string{"/work/repo"}); got != c.want {
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
	unknown := strings.Repeat("0", 40)
	if _, err := recovery.Recover(ctx, rec, unknown, ""); err == nil || !strings.HasPrefix(err.Error(), "arm A: ") {
		t.Errorf("unknown base: %v", err)
	}

	// A data folder that moved: the transcript is found under the current records folder by run ID.
	moved := Record{ID: "r1", Arm: "A", RecordsDir: "/gone/records/r1"}
	if got, err := (&Recovery{Bare: bare, Records: filepath.Dir(records)}).Recover(ctx, moved, base, ""); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("moved data folder: %+v, %v", got, err)
	}

	// Cancelled while resolving: an error, and nothing kept, so a later call resolves afresh.
	fresh := &Recovery{Bare: bare}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := fresh.Recover(cancelled, rec, base, ""); err == nil {
		t.Error("a cancelled recovery succeeded")
	}
	if got, err := fresh.Recover(ctx, rec, base, ""); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("after a cancelled one: %+v, %v", got, err)
	}
}

// An experiment's runs have different bases and snapshots that share most of their context files, and resolving one
// context reads a file several times: a Recovery lists each commit once and reads each blob once, whatever the number
// of runs, and gives every run the context use it would get alone.
func TestRecoveryReadsEachObjectOnce(t *testing.T) {
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
	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	// Two task bases: the second changes code only, so its context files are the first's blobs.
	var bases []string
	for i, code := range []string{"package main\n", "package main\n\nfunc main() {}\n"} {
		files := memSource{}
		for p, data := range project {
			files[p] = data
		}
		files["main.go"] = code
		for p, data := range files {
			if err := os.MkdirAll(filepath.Join(user, filepath.Dir(p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(user, p), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		gitCmd("add", "-A")
		gitCmd("commit", "-q", "-m", "base "+string(rune('1'+i)))
		base := gitCmd("rev-parse", "HEAD")
		if err := gitx.FetchCommit(ctx, user, base, gitx.SourceRef(base), "--git-dir", bare); err != nil {
			t.Fatal(err)
		}
		bases = append(bases, base)
	}
	lean, _, err := snapshot.Build(ctx, bare, memSource{"CLAUDE.md": "# Lean\n"}, "snapshot lean", nil)
	if err != nil {
		t.Fatal(err)
	}
	records := t.TempDir()
	transcript := strings.Join([]string{
		`{"type":"system","subtype":"init","cwd":"/old/workspace/repo","claude_code_version":"2.1.281","model":"claude-sonnet-5"}`,
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-sonnet-5","content":[` +
			`{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/old/workspace/repo/docs/testing.md"}},` +
			`{"type":"tool_use","id":"t2","name":"Skill","input":{"skill":"review"}}]}}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.1}`,
	}, "\n")
	type recorded struct {
		rec            Record
		base, snapshot string
		want           *ContextUse
	}
	full := &ContextUse{Start: []string{".claude/rules/always.md", "AGENTS.md", "CLAUDE.md"}, Files: []string{"docs/testing.md"}, Skills: []string{"review"}}
	var runs []recorded
	for i, base := range bases {
		for j, arm := range []struct {
			name, snapshot string
			want           *ContextUse
		}{{"A", "", full}, {"B", lean, &ContextUse{Start: []string{"CLAUDE.md"}}}} {
			id := "r" + string(rune('1'+2*i+j))
			if err := os.MkdirAll(filepath.Join(records, id), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(records, id, "stream.jsonl"), []byte(transcript), 0o600); err != nil {
				t.Fatal(err)
			}
			runs = append(runs, recorded{rec: Record{ID: id, Arm: arm.name}, base: base, snapshot: arm.snapshot, want: arm.want})
		}
	}

	calls := gitxtest.Calls(t)
	recovery := Recovery{Bare: bare, Records: records}
	for range 2 { // a second pass over the runs asks git nothing
		for _, r := range runs {
			got, err := recovery.Recover(ctx, r.rec, r.base, r.snapshot)
			if err != nil || !reflect.DeepEqual(got, r.want) {
				t.Errorf("run %s: %+v, %v\nwant %+v", r.rec.ID, got, err, r.want)
			}
		}
	}
	made := calls()
	if lists := gitxtest.Count(made, "ls-tree"); lists != 3 {
		t.Errorf("%d listings, want 3 (two bases and the snapshot)", lists)
	}
	// Every blob is read once: the two bases' context files are the same blobs, read for the first base alone.
	blobs := map[string]string{}
	for _, call := range made {
		if gitxtest.Subcommand(call) != "cat-file" {
			continue
		}
		spec := call[len(call)-1] // <commit>:<path>
		id, err := gitx.Run(ctx, "--git-dir", bare, "rev-parse", spec)
		if err != nil {
			t.Fatal(err)
		}
		if earlier, ok := blobs[id]; ok {
			t.Errorf("blob %s read twice: as %s and as %s", id[:12], earlier, spec)
		}
		blobs[id] = spec
	}
	if len(blobs) == 0 || len(made) != 3+len(blobs) {
		t.Errorf("%d git calls for %d blobs, want 3 listings and one read a blob:\n%v", len(made), len(blobs), made)
	}
	for spec := range maps.Values(blobs) {
		if commit, _, _ := strings.Cut(spec, ":"); commit == bases[1] {
			t.Errorf("%s read from the second base: the first base has the same blob", spec)
		}
	}
}
