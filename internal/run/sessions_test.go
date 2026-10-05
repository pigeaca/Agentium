package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
)

// sessionsFixture is a data folder (my.data, so that its path holds a separator) beside a stand-in Claude Code config
// folder, in one resolved temp folder: nothing here reaches the user's own ~/.claude.
type sessionsFixture struct {
	base     string // the resolved temp folder
	layout   home.Layout
	config   string // the stand-in config folder
	projects string
	now      time.Time
	known    map[string]bool // the workspaces whose runs the fixture's store knows (CleanInput.KnownWorkspace)
}

func newSessionsFixture(t *testing.T) sessionsFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	data := filepath.Join(base, "my.data")
	f := sessionsFixture{base: base, config: filepath.Join(base, "claude-config"), now: time.Now(), known: map[string]bool{},
		layout: home.Layout{Root: data, Database: filepath.Join(data, "agentium.db"), Artifacts: filepath.Join(data, "artifacts"),
			Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records"), Cache: filepath.Join(data, "cache"),
			Deps: filepath.Join(data, "deps")}}
	f.projects = filepath.Join(f.config, "projects")
	must(t, os.MkdirAll(f.layout.Workspaces, 0o700))
	must(t, os.MkdirAll(f.projects, 0o700))
	return f
}

// runID is a run's ID, as NewID makes it.
func runID(t *testing.T) string {
	t.Helper()
	id, err := NewID(time.Now())
	must(t, err)
	return id
}

// session makes the session folder Claude Code keeps for a session started in dir (which need not exist), with a
// saved tool output and the given top-level files, all last modified ago before now; it returns its path.
func (f sessionsFixture) session(t *testing.T, dir string, ago time.Duration, files ...string) string {
	t.Helper()
	p := claude.SessionFolder(f.config, dir)
	fill(t, filepath.Join(p, "tool-results"), 3000)
	for _, name := range files {
		must(t, os.WriteFile(filepath.Join(p, name), []byte("{}\n"), 0o600))
	}
	f.age(t, p, ago)
	return p
}

// age sets the modification time of everything under p to ago before now.
func (f sessionsFixture) age(t *testing.T, p string, ago time.Duration) {
	t.Helper()
	must(t, filepath.WalkDir(p, func(q string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(q, f.now.Add(-ago), f.now.Add(-ago))
	}))
}

// ours is the session folder of a finished, stored run of the fixture's data folder, in the named workspace.
func (f sessionsFixture) ours(t *testing.T, workspace string, module ...string) string {
	t.Helper()
	f.known[workspace] = true
	return f.session(t, filepath.Join(append([]string{f.layout.Workspaces, workspace, "repo"}, module...)...), 48*time.Hour)
}

func (f sessionsFixture) plan(t *testing.T) CleanPlan {
	t.Helper()
	plan, err := PlanClean(context.Background(), f.input(f.layout, f.config))
	must(t, err)
	return plan
}

// input is the fixture's cleanup input for a layout and a config folder.
func (f sessionsFixture) input(layout home.Layout, config string) CleanInput {
	return CleanInput{Layout: layout, Now: f.now, OlderThan: CleanDefaultAge, ClaudeConfig: config,
		KnownWorkspace: func(name string) (bool, error) { return f.known[name], nil }}
}

// sessionItems are a list's sessions items, by path.
func sessionItems(items []CleanItem) map[string]CleanItem {
	out := map[string]CleanItem{}
	for _, it := range items {
		if it.Kind == CleanSessions {
			out[it.Path] = it
		}
	}
	return out
}

// The recogniser reads back the workspace of every name claude.SessionFolder gives a run of this data folder: a run's
// ID, an experiment slot's name, with or without a module, and no other name.
func TestSessionWorkspaceAgreesWithSessionFolder(t *testing.T) {
	f := newSessionsFixture(t)
	workspaces := realPath(f.layout.Workspaces)
	id := runID(t)
	for _, c := range []struct {
		workspace string
		module    []string
	}{
		{id, nil}, {"e12-s3-t2", nil}, {"e1-s10-t1", nil},
		{id, []string{"svc"}}, {"e7-s1-t1", []string{"go.mod.d", "api_v2"}}, {id, []string{".hidden"}},
	} {
		dir := filepath.Join(append([]string{f.layout.Workspaces, c.workspace, "repo"}, c.module...)...)
		must(t, os.MkdirAll(dir, 0o700)) // resolved by SessionFolder, as the run's folder is
		name := filepath.Base(claude.SessionFolder(f.config, dir))
		if ws, ok := sessionWorkspace(workspaces, name); !ok || ws != c.workspace {
			t.Errorf("%s: sessionWorkspace = %q, %v; want %q", name, ws, ok, c.workspace)
		}
	}
	for _, dir := range []string{
		filepath.Join(f.layout.Workspaces, "r1", "repo"),                      // not a workspace name Agentium makes
		filepath.Join(f.layout.Workspaces, id),                                // not the checkout
		filepath.Join(f.layout.Workspaces, id, "repository"),                  // nor this
		filepath.Join(f.layout.Workspaces, id, "repo") + "X",                  // nor this
		filepath.Join(f.layout.Workspaces, strings.ToUpper(id), "repo"),       // an ID is lower-case hex
		filepath.Join(f.layout.Workspaces, "e1-s2", "repo"),                   // half a slot's name
		filepath.Join(f.base, "my.data-b", "workspaces", id, "repo"),          // a sibling data folder
		filepath.Join(f.base, "my.dat", "workspaces", id, "repo"),             // a data folder whose name is a prefix
		filepath.Join(f.base, "my..data", "workspaces", id, "repo"),           // the same letters, a separator more
		filepath.Join(f.base, "mydata", "workspaces", id, "repo"),             // the same letters, a separator fewer
		filepath.Join(f.base, "my.data", "workspaces2", id, "repo"),           // another folder of the data folder
		filepath.Join(f.base, "my.data", "workspaces", "x", id, "repo"),       // deeper
		filepath.Join(f.base, "other", "my.data", "workspaces", id, "repo"),   // elsewhere
		filepath.Join(f.base, "my.data", "workspaces", id, "repo", "..", "x"), // cleaned to another folder
	} {
		name := filepath.Base(claude.SessionFolder(f.config, dir))
		if ws, ok := sessionWorkspace(workspaces, name); ok {
			t.Errorf("%s: recognised as workspace %q", dir, ws)
		}
	}
}

// clean lists exactly the folders this data folder's runs left, without --yes changes nothing, and removes them with it;
// each other folder stays, each in a test of its own beside folders it removes.
func TestCleanSessionsRemovesOnlyThisDataFoldersFolders(t *testing.T) {
	for name, other := range map[string]func(t *testing.T, f sessionsFixture) (path string, kept string){
		"the owner's project": func(t *testing.T, f sessionsFixture) (string, string) {
			p := filepath.Join(f.projects, "-Users-someone-code-my-data")
			fill(t, filepath.Join(p, "tool-results"), 100)
			must(t, os.WriteFile(filepath.Join(p, "0f3a.jsonl"), []byte("{}\n"), 0o600))
			f.age(t, p, 90*24*time.Hour)
			return p, ""
		},
		"a sibling data folder's": func(t *testing.T, f sessionsFixture) (string, string) {
			return f.session(t, filepath.Join(f.base, "my.data-b", "workspaces", runID(t), "repo"), 48*time.Hour), ""
		},
		"a sibling data folder's experiment": func(t *testing.T, f sessionsFixture) (string, string) {
			return f.session(t, filepath.Join(f.base, "my.data.old", "workspaces", "e1-s1-t1", "repo"), 48*time.Hour), ""
		},
		"a name that only looks alike": func(t *testing.T, f sessionsFixture) (string, string) {
			return f.session(t, filepath.Join(f.base, "my..data", "workspaces", runID(t), "repo"), 48*time.Hour), ""
		},
		"a folder with a session file": func(t *testing.T, f sessionsFixture) (string, string) {
			return f.session(t, filepath.Join(f.layout.Workspaces, runID(t), "repo"), 48*time.Hour, "3c1d.jsonl"), CleanKeptSessionFile
		},
		"a link": func(t *testing.T, f sessionsFixture) (string, string) {
			target := filepath.Join(f.base, "elsewhere")
			fill(t, target, 100)
			p := claude.SessionFolder(f.config, filepath.Join(f.layout.Workspaces, runID(t), "repo"))
			must(t, os.Symlink(target, p))
			return p, ""
		},
		"a link to the owner's project": func(t *testing.T, f sessionsFixture) (string, string) {
			own := filepath.Join(f.projects, "-Users-someone-code-app")
			fill(t, own, 100)
			f.age(t, own, 48*time.Hour)
			p := claude.SessionFolder(f.config, filepath.Join(f.layout.Workspaces, "e9-s1-t1", "repo"))
			must(t, os.Symlink(filepath.Base(own), p))
			return own, ""
		},
		"a folder whose workspace exists": func(t *testing.T, f sessionsFixture) (string, string) {
			id := runID(t)
			must(t, os.MkdirAll(filepath.Join(f.layout.Workspaces, id), 0o700))
			return f.ours(t, id), CleanKeptWorkspace
		},
		"a file where a folder is expected": func(t *testing.T, f sessionsFixture) (string, string) {
			p := claude.SessionFolder(f.config, filepath.Join(f.layout.Workspaces, runID(t), "repo"))
			must(t, os.WriteFile(p, []byte("x"), 0o600))
			return p, ""
		},
		"a folder used in the last hour": func(t *testing.T, f sessionsFixture) (string, string) {
			id := runID(t)
			f.known[id] = true
			return f.session(t, filepath.Join(f.layout.Workspaces, id, "repo"), 10*time.Minute), CleanKeptRecent
		},
		"a run the store does not know": func(t *testing.T, f sessionsFixture) (string, string) {
			return f.session(t, filepath.Join(f.layout.Workspaces, runID(t), "repo"), 48*time.Hour), CleanKeptUnknownRun
		},
		"an experiment slot the store does not know": func(t *testing.T, f sessionsFixture) (string, string) {
			return f.session(t, filepath.Join(f.layout.Workspaces, "e3-s1-t3", "repo"), 48*time.Hour), CleanKeptUnknownRun
		},
		// The review's probe: a kept run of a data folder whose path differs only in punctuation (my_data), its workspace
		// there, its folder unused for two hours. The name is this data folder's too; its store does not know the run.
		"a colliding data folder's kept run": func(t *testing.T, f sessionsFixture) (string, string) {
			id := runID(t)
			must(t, os.MkdirAll(filepath.Join(f.base, "my_data", "workspaces", id, "repo"), 0o700))
			p := f.session(t, filepath.Join(f.base, "my_data", "workspaces", id, "repo"), 2*time.Hour)
			if p != claude.SessionFolder(f.config, filepath.Join(f.layout.Workspaces, id, "repo")) {
				t.Fatal("the probe's names do not collide")
			}
			return p, CleanKeptUnknownRun
		},
		"a folder of a workspace name Agentium does not make": func(t *testing.T, f sessionsFixture) (string, string) {
			return f.session(t, filepath.Join(f.layout.Workspaces, "r1", "repo"), 48*time.Hour), ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSessionsFixture(t)
			ours := []string{f.ours(t, runID(t)), f.ours(t, "e3-s1-t2"), f.ours(t, runID(t), "svc", "api")}
			path, keptAs := other(t, f)
			before := sessionTree(t, f.base)

			plan := f.plan(t)
			remove, keep := sessionItems(plan.Remove), sessionItems(plan.Keep)
			if len(remove) != len(ours) {
				t.Errorf("listed for removal: %v; want %v", slices.Collect(mapsKeys(remove)), ours)
			}
			for _, p := range ours {
				it, ok := remove[p]
				if !ok || it.Reason != CleanRunSession || it.Bytes < 3000 || !strings.Contains(it.Detail, "left by run ") {
					t.Errorf("%s: %+v, listed %v", filepath.Base(p), it, ok)
				}
			}
			if it, ok := keep[path]; keptAs == "" && ok || keptAs != "" && (!ok || it.Reason != keptAs) {
				t.Errorf("kept %+v (listed %v); want reason %q", it, ok, keptAs)
			}
			if len(keep) > 1 || len(plan.Notes) > 0 {
				t.Errorf("kept %v, notes %v", keep, plan.Notes)
			}
			if after := sessionTree(t, f.base); after != before {
				t.Errorf("planning changed something:\n%s\n---\n%s", before, after)
			}

			for i, err := range RemoveClean(context.Background(), f.layout, plan.Remove) {
				if err != nil {
					t.Errorf("%s: %v", plan.Remove[i].Path, err)
				}
			}
			for _, p := range ours {
				if exists(p) {
					t.Errorf("%s is still there", filepath.Base(p))
				}
			}
			if !exists(path) {
				t.Errorf("%s was removed", filepath.Base(path))
			}
			if exists(quarantine(f.layout)) {
				t.Error("a session folder went through the data folder's quarantine")
			}
			// What stays is as it was, inside too.
			left := sessionTree(t, f.base)
			for _, line := range strings.Split(before, "\n") {
				if strings.HasPrefix(line, strings.TrimPrefix(f.projects, f.base)+" ") {
					continue // the projects folder itself: one entry fewer
				}
				if line != "" && !slices.ContainsFunc(ours, func(p string) bool { return strings.HasPrefix(line, strings.TrimPrefix(p, f.base)) }) && !strings.Contains(left, line) {
					t.Errorf("changed: %s", line)
				}
			}
		})
	}
}

// sessionTree lists everything under root (never following a link) with its type, size and modification time.
func sessionTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	must(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		target := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			target, _ = os.Readlink(p)
		}
		fmt.Fprintf(&b, "%s %s %d %d %s\n", strings.TrimPrefix(p, root), info.Mode(), info.Size(), info.ModTime().UnixNano(), target)
		return nil
	}))
	return b.String()
}

func mapsKeys(m map[string]CleanItem) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(filepath.Base(k)) {
				return
			}
		}
	}
}

// Under --yes each folder is checked again: one that changed since it was listed stays.
func TestCleanSessionsChecksEachFolderAgainAtRemoval(t *testing.T) {
	for name, c := range map[string]struct {
		change func(t *testing.T, f sessionsFixture, p, workspace string)
		want   error // nil: any error
	}{
		"a session file appeared": {func(t *testing.T, f sessionsFixture, p, _ string) {
			must(t, os.WriteFile(filepath.Join(p, "new.jsonl"), nil, 0o600))
			f.age(t, p, 48*time.Hour)
		}, ErrCleanUsed},
		"used since": {func(t *testing.T, f sessionsFixture, p, _ string) {
			must(t, os.WriteFile(filepath.Join(p, "tool-results", "late.txt"), nil, 0o600))
		}, ErrCleanUsed},
		"its workspace appeared": {func(t *testing.T, f sessionsFixture, _, workspace string) {
			must(t, os.MkdirAll(workspace, 0o700))
		}, nil},
		"swapped for a link": {func(t *testing.T, f sessionsFixture, p, _ string) {
			target := filepath.Join(f.base, "target")
			fill(t, target, 10)
			must(t, os.RemoveAll(p))
			must(t, os.Symlink(target, p))
		}, errSessionNotReal},
		"swapped for a file": {func(t *testing.T, f sessionsFixture, p, _ string) {
			must(t, os.RemoveAll(p))
			must(t, os.WriteFile(p, []byte("x"), 0o600))
		}, errSessionNotReal},
		"the store no longer knows its run": {func(t *testing.T, f sessionsFixture, _, workspace string) {
			delete(f.known, filepath.Base(workspace))
		}, nil},
		"the projects folder swapped for a link": {func(t *testing.T, f sessionsFixture, _, _ string) {
			moved := filepath.Join(f.base, "moved-projects")
			must(t, os.Rename(f.projects, moved))
			must(t, os.Symlink(moved, f.projects))
		}, errProjectsNotReal},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSessionsFixture(t)
			id := runID(t)
			p := f.ours(t, id)
			plan := f.plan(t)
			if len(plan.Remove) != 1 {
				t.Fatalf("planned %v", plan.Remove)
			}
			c.change(t, f, p, filepath.Join(f.layout.Workspaces, id))
			before := sessionTree(t, f.base)
			err := RemoveClean(context.Background(), f.layout, plan.Remove)[0]
			if err == nil || c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("removal: %v; want %v", err, c.want)
			}
			if strings.Contains(err.Error(), f.base) || strings.Contains(err.Error(), filepath.Base(p)) {
				t.Errorf("the error names the folder: %v", err)
			}
			if after := sessionTree(t, f.base); after != before {
				t.Errorf("a refused removal changed something:\n%s\n---\n%s", before, after)
			}
		})
	}
}

// A link inside a run's folder goes as a link: what it points to stays.
func TestCleanSessionsRemovesALinkInsideAsALink(t *testing.T) {
	f := newSessionsFixture(t)
	p := f.ours(t, runID(t))
	own := filepath.Join(f.projects, "-Users-someone-code-app")
	fill(t, own, 100)
	must(t, os.WriteFile(filepath.Join(own, "a.jsonl"), nil, 0o600))
	outside := filepath.Join(f.base, "outside")
	fill(t, outside, 100)
	must(t, os.Symlink(own, filepath.Join(p, "tool-results", "to-project")))
	must(t, os.Symlink(outside, filepath.Join(p, "to-outside")))
	must(t, os.Symlink("..", filepath.Join(p, "tool-results", "up")))
	f.age(t, p, 48*time.Hour)
	f.now = f.now.Add(2 * time.Hour) // the links' own times are now (os.Chtimes sets their targets')
	plan := f.plan(t)
	if errs := RemoveClean(context.Background(), f.layout, plan.Remove); len(errs) != 1 || errs[0] != nil {
		t.Fatalf("removal: %v", errs)
	}
	if exists(p) {
		t.Error("the folder is still there")
	}
	for _, kept := range []string{filepath.Join(own, "data"), filepath.Join(own, "a.jsonl"), filepath.Join(outside, "data")} {
		if !exists(kept) {
			t.Errorf("%s went through a link", kept)
		}
	}
}

// Without the login's config folder, or with no config folder or projects folder at all, there is nothing to list and
// no error; a projects folder that is a link is not looked into, and a note says so.
func TestCleanSessionsWithoutAProjectsFolder(t *testing.T) {
	f := newSessionsFixture(t)
	p := f.ours(t, runID(t))
	for name, config := range map[string]string{"another sign-in": "", "no config folder": filepath.Join(f.base, "none"),
		"no projects folder": filepath.Join(f.base, "my.data")} {
		plan, err := PlanClean(context.Background(), f.input(f.layout, config))
		if err != nil || len(plan.Remove)+len(plan.Keep)+len(plan.Notes) != 0 {
			t.Errorf("%s: %+v, %v", name, plan, err)
		}
	}
	linked := filepath.Join(f.base, "linked-config")
	must(t, os.MkdirAll(linked, 0o700))
	must(t, os.Symlink(f.projects, filepath.Join(linked, "projects")))
	plan, err := PlanClean(context.Background(), f.input(f.layout, linked))
	if err != nil || len(plan.Remove)+len(plan.Keep) != 0 || len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "is not listed") {
		t.Errorf("linked projects folder: %+v, %v", plan, err)
	}
	if !exists(p) {
		t.Error("the folder went")
	}
	// A config folder that is a link is the user's own setup: it is followed to a real projects folder.
	must(t, os.Symlink(f.config, filepath.Join(f.base, "config-link")))
	plan, err = PlanClean(context.Background(), f.input(f.layout, filepath.Join(f.base, "config-link")))
	if err != nil || len(plan.Remove) != 1 {
		t.Errorf("linked config folder: %+v, %v", plan, err)
	}
}

// sessionOnce is a run with the user's login whose fake Claude Code keeps a session folder, as the real one does, in
// the stand-in config folder under the test's home ($HOME/.claude); then it runs agentScript (with $p the folder).
func sessionOnce(t *testing.T, agentScript string) (moduleOnce, string) {
	t.Helper()
	script := `p="$HOME/.claude/projects/$(printf '%s' "$start" | sed 's/[^A-Za-z0-9]/-/g')"; mkdir -p "$p/tool-results" && echo out > "$p/tool-results/t1.txt"; ` + agentScript
	f := newModuleOnce(t, "svc", "decoy", script)
	projects := filepath.Join(f.env.Home, ".claude", "projects")
	must(t, os.MkdirAll(projects, 0o700))
	return f, projects
}

// ownSessionOf is the session folder a run in workspace (and module svc) keeps under projects.
func ownSessionOf(t *testing.T, projects string, env Env) string {
	t.Helper()
	dir := filepath.Join(realPath(env.Layout.Workspaces), env.workspaceName(), "repo", "svc")
	return claude.SessionFolder(filepath.Dir(projects), dir)
}

// With the login, a run removes the session folder Claude Code kept for it, and only that one; with --keep it stays.
func TestOnceRemovesItsOwnSessionFolder(t *testing.T) {
	f, projects := sessionOnce(t, "")
	own := filepath.Join(projects, "-Users-someone-code-app")
	fill(t, own, 10)
	must(t, os.WriteFile(filepath.Join(own, "a.jsonl"), nil, 0o600))
	before := sessionTree(t, own)
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil || rec.Outcome != agent.OutcomeOK {
		t.Fatalf("run: %s, %v, %v", rec.Outcome, err, rec.Notes)
	}
	entries, _ := os.ReadDir(projects)
	if len(entries) != 1 || entries[0].Name() != filepath.Base(own) || sessionTree(t, own) != before {
		t.Errorf("projects folder after the run: %v", entries)
	}
	if strings.Contains(strings.Join(rec.Notes, "\n"), "session folder") {
		t.Errorf("notes: %v", rec.Notes)
	}

	kept, projects := sessionOnce(t, "")
	kept.spec.Keep = true
	if _, err := Once(context.Background(), kept.env, kept.spec); err != nil {
		t.Fatal(err)
	}
	p := ownSessionOf(t, projects, kept.env)
	if !exists(filepath.Join(p, "tool-results", "t1.txt")) {
		t.Error("a kept run's session folder went")
	}
	// The start file names it, for a recovery.
	data, err := os.ReadFile(filepath.Join(kept.env.Layout.Records, kept.env.ID, startFile))
	must(t, err)
	var s start
	must(t, json.Unmarshal(data, &s))
	if s.Session != p {
		t.Errorf("the start file's session folder is %q, want %q", s.Session, p)
	}
}

// A session folder the agent made a link, or that holds a session file, stays, with a note; the run is not failed.
func TestOnceLeavesASessionFolderItCannotProve(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	fill(t, outside, 10)
	for name, script := range map[string]string{
		"a link":       `rm -rf "$p" && ln -s "` + outside + `" "$p"`,
		"session file": `echo '{}' > "$p/s.jsonl"`,
	} {
		t.Run(name, func(t *testing.T) {
			f, projects := sessionOnce(t, script)
			rec, err := Once(context.Background(), f.env, f.spec)
			if err != nil || rec.Outcome != agent.OutcomeOK {
				t.Fatalf("run: %s, %v, %v", rec.Outcome, err, rec.Notes)
			}
			p := ownSessionOf(t, projects, f.env)
			if _, err := os.Lstat(p); err != nil {
				t.Errorf("the folder went: %v", err)
			}
			if !exists(filepath.Join(outside, "data")) {
				t.Error("the link's target went")
			}
			notes := strings.Join(rec.Notes, "\n")
			if !strings.Contains(notes, "Claude Code's session folder of this run was left in its projects folder") || strings.Contains(notes, filepath.Base(p)) {
				t.Errorf("notes: %v", rec.Notes)
			}
			// The final start file holds the note too: a recovery after a crash stores it.
			data, err := os.ReadFile(filepath.Join(f.env.Layout.Records, f.env.ID, startFile))
			must(t, err)
			var s start
			must(t, json.Unmarshal(data, &s))
			if !s.Finished || !strings.Contains(strings.Join(s.Record.Notes, "\n"), "session folder of this run was left") {
				t.Errorf("the final start file's notes: %v", s.Record.Notes)
			}
		})
	}
}

// With an API key the run's config folder is its own, in its workspace: the user's projects folder is not touched,
// even where a folder of the run's name is there.
func TestOnceWithAnAPIKeyLeavesTheUsersProjectsFolder(t *testing.T) {
	f, projects := sessionOnce(t, "")
	f.env.SignIn, f.env.Secret = claude.SignInAPIKey, "sk-ant-api03-session-test" // secret-scan: allow
	// The fake agent writes to the user's projects folder: whatever is there after the run was not Agentium's to remove.
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil || rec.Outcome != agent.OutcomeOK {
		t.Fatalf("run: %s, %v, %v", rec.Outcome, err, rec.Notes)
	}
	if p := ownSessionOf(t, projects, f.env); !exists(filepath.Join(p, "tool-results", "t1.txt")) {
		t.Error("an API key run removed a folder in the user's projects folder")
	}
}

// A dead Agentium's run: recovery removes the session folder its start file names with the workspace; one that is not
// the start file's own workspace's stays, with a warning.
func TestRecoverRemovesADeadRunsSessionFolder(t *testing.T) {
	f := newSessionsFixture(t)
	start := func(id, session string) {
		dir := filepath.Join(f.layout.Records, id)
		must(t, os.MkdirAll(filepath.Join(f.layout.Workspaces, id, "repo"), 0o700))
		data, err := json.Marshal(start{Record: Record{ID: id, RecordsDir: dir, SignIn: claude.SignInLogin, Task: "t", Arm: "a"},
			Workspace: filepath.Join(f.layout.Workspaces, id), AgentStarted: true, Session: session})
		must(t, err)
		must(t, os.MkdirAll(dir, 0o700))
		must(t, os.WriteFile(filepath.Join(dir, startFile), data, 0o600))
	}
	dead, other, stray := runID(t), runID(t), runID(t)
	deadSession := f.ours(t, dead)
	otherSession := f.ours(t, other)
	start(dead, deadSession)
	start(stray, otherSession) // names another run's folder
	var warnings []string
	orphans, err := RecoverWarn(context.Background(), f.layout, func(string) (bool, error) { return false, nil }, nil, f.now,
		func(w string) { warnings = append(warnings, w) })
	if err != nil || len(orphans) != 2 {
		t.Fatalf("recover: %v, %v", orphans, err)
	}
	if exists(deadSession) || exists(filepath.Join(f.layout.Workspaces, dead)) {
		t.Error("the dead run's session folder or workspace is still there")
	}
	if !exists(otherSession) {
		t.Error("recovery removed another run's session folder")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], stray) || strings.Contains(warnings[0], f.base) {
		t.Errorf("warnings: %v", warnings)
	}
}

// Without a store to ask (KnownWorkspace nil), no folder goes: each is kept as an unknown run's.
func TestCleanSessionsWithoutAStoreRemovesNothing(t *testing.T) {
	f := newSessionsFixture(t)
	p := f.ours(t, runID(t))
	in := f.input(f.layout, f.config)
	in.KnownWorkspace = nil
	plan, err := PlanClean(context.Background(), in)
	must(t, err)
	if it, ok := sessionItems(plan.Keep)[p]; len(plan.Remove) != 0 || !ok || it.Reason != CleanKeptUnknownRun {
		t.Errorf("plan %+v", plan)
	}
}

// A data folder named in another letter case (one folder on macOS) matches none of the names its runs left: Claude Code
// names a folder by the path as the agent's process saw it, and Agentium compares names exactly.
func TestCleanSessionsMatchNothingForAnotherLetterCase(t *testing.T) {
	f := newSessionsFixture(t)
	f.ours(t, runID(t))
	f.ours(t, "e1-s1-t1")
	other := f.layout
	other.Workspaces = filepath.Join(f.base, "My.Data", "workspaces")
	plan, err := PlanClean(context.Background(), f.input(other, f.config))
	must(t, err)
	if len(plan.Remove)+len(plan.Keep) != 0 {
		t.Errorf("another letter case matched: %+v", plan)
	}
}

// A start file written before runs recorded their session folder (no "session" key): recovery removes the workspace
// and leaves Claude Code's folder, without a warning; cleanup lists it later.
func TestRecoverLeavesTheSessionFolderOfAnOlderStartFile(t *testing.T) {
	f := newSessionsFixture(t)
	id := runID(t)
	p := f.ours(t, id)
	dir := filepath.Join(f.layout.Records, id)
	workspace := filepath.Join(f.layout.Workspaces, id)
	must(t, os.MkdirAll(filepath.Join(workspace, "repo"), 0o700))
	must(t, os.MkdirAll(dir, 0o700))
	data := `{"record":{"id":"` + id + `","records":"` + dir + `","sign_in":"login"},"workspace":"` + workspace + `","agent_started":true}`
	must(t, os.WriteFile(filepath.Join(dir, startFile), []byte(data), 0o600))
	var warnings []string
	orphans, err := RecoverWarn(context.Background(), f.layout, func(string) (bool, error) { return false, nil }, nil, f.now,
		func(w string) { warnings = append(warnings, w) })
	if err != nil || len(orphans) != 1 || len(warnings) != 0 {
		t.Fatalf("recover: %v, %v, warnings %v", orphans, err, warnings)
	}
	if exists(workspace) || !exists(filepath.Join(p, "tool-results", "data")) {
		t.Errorf("workspace there %v, session folder there %v", exists(workspace), exists(p))
	}
	if plan := f.plan(t); len(plan.Remove) != 1 || plan.Remove[0].Path != p {
		t.Errorf("cleanup does not list it: %+v", plan.Remove)
	}
}

// A run removes its own folder only under cleanup's checks: an absolute path whose name fits this data folder's runs
// and is its own workspace's; otherwise the folder stays, with a note (none when nothing is there).
func TestRemoveOwnSessionChecksAsCleanupDoes(t *testing.T) {
	f := newSessionsFixture(t)
	id, other := runID(t), runID(t)
	own := f.ours(t, id)
	otherRun := f.ours(t, other)
	env := Env{ID: id, Layout: f.layout}
	for name, session := range map[string]string{
		"another run's folder": otherRun,
		"a relative path":      filepath.Join("claude-config", "projects", filepath.Base(own)),
		"a name of no run":     filepath.Join(f.projects, "-Users-someone-code-app"),
	} {
		fill(t, filepath.Join(f.projects, "-Users-someone-code-app"), 10)
		env.ownSession = session
		note := env.removeOwnSession()
		if !strings.Contains(note, "its path is not one Agentium's runs make") {
			t.Errorf("%s: note %q", name, note)
		}
	}
	if !exists(otherRun) || !exists(own) || !exists(filepath.Join(f.projects, "-Users-someone-code-app", "data")) {
		t.Error("a folder went")
	}
	env.ownSession = filepath.Join(f.projects, "missing")
	if note := env.removeOwnSession(); note != "" {
		t.Errorf("nothing there: note %q", note)
	}
	env.ownSession = own
	if note := env.removeOwnSession(); note != "" || exists(own) {
		t.Errorf("its own folder: note %q, still there %v", note, exists(own))
	}
}
