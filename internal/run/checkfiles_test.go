package run

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/source"
)

// commitWith is a commit of a fresh repository holding files (path: content; "symlink:TARGET" makes a link), as a
// source, which knows its links.
func commitWith(t *testing.T, files map[string]string) source.Source {
	t.Helper()
	ctx := context.Background()
	repo := t.TempDir()
	for name, content := range files {
		full := filepath.Join(repo, filepath.FromSlash(name))
		must(t, os.MkdirAll(filepath.Dir(full), 0o755))
		if target, ok := strings.CutPrefix(content, "symlink:"); ok {
			must(t, os.Symlink(target, full))
			continue
		}
		writeFile(t, full, content)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := gitx.Run(ctx, append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		must(t, err)
		return out
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	src, err := source.Commit(ctx, git("rev-parse", "HEAD"), "-C", repo)
	must(t, err)
	return src
}

// A verify script that is a symbolic link brings the files it leads to within the repository, through a chain of
// links; a target outside the repository (absolute or above the root), missing, or a loop ends it.
func TestCheckFilesFollowsLinkedScriptsWithinTheRepository(t *testing.T) {
	base := commitWith(t, map[string]string{
		"scripts/real.sh":   "exit 1\n",
		"run_tests.sh":      "symlink:scripts/real.sh",
		"chain.sh":          "symlink:scripts/hop.sh",
		"scripts/hop.sh":    "symlink:real.sh",
		"outside.sh":        "symlink:../elsewhere.sh",
		"absolute.sh":       "symlink:/etc/hosts",
		"missing.sh":        "symlink:scripts/none.sh",
		"loop.sh":           "symlink:loop2.sh",
		"loop2.sh":          "symlink:loop.sh",
		"svc/check.sh":      "symlink:../scripts/real.sh",
		"svc/escape.sh":     "symlink:../../x.sh",
		"svc/run_tests.sh":  "exit 0\n",
		"scripts/plain.sh":  "exit 0\n",
		"scripts/to-dir.sh": "symlink:../scripts",
	})
	for _, c := range []struct {
		module, verify string
		want           []string
	}{
		{"", "sh run_tests.sh", []string{"run_tests.sh", "scripts/real.sh"}},
		{"", "sh chain.sh", []string{"chain.sh", "scripts/hop.sh", "scripts/real.sh"}},
		{"", "sh outside.sh && sh absolute.sh && sh missing.sh", []string{"absolute.sh", "missing.sh", "outside.sh"}},
		{"", "sh loop.sh", []string{"loop.sh", "loop2.sh"}},
		{"", "sh scripts/to-dir.sh && sh scripts/plain.sh", []string{"scripts/plain.sh", "scripts/to-dir.sh"}},
		{"svc", "sh check.sh && sh escape.sh && sh run_tests.sh", []string{"scripts/real.sh", "svc/check.sh", "svc/escape.sh", "svc/run_tests.sh"}},
	} {
		scripts, _ := checkFiles([]string{c.verify}, c.module, base)
		slices.Sort(scripts)
		if !slices.Equal(scripts, c.want) {
			t.Errorf("%q in %q: scripts %q, want %q", c.verify, c.module, scripts, c.want)
		}
	}
}

// After a plain `cd DIR` a command's words are read from DIR too, never instead of the module's folder; a DIR a shell
// would expand stops that, and each command starts again from the module's folder.
func TestCheckFilesFollowsSimpleCdPrefixes(t *testing.T) {
	files := fakeSource{"run.sh", "sub/run.sh", "svc/run_tests.sh", "svc/sub/x.sh", "tools/check.sh"}
	slices.Sort(files)
	for _, c := range []struct {
		module string
		verify []string
		want   []string
	}{
		{"svc", []string{"cd .. && sh tools/check.sh"}, []string{"tools/check.sh"}},
		{"svc", []string{"cd ..; cd tools && sh check.sh"}, []string{"tools/check.sh"}},
		{"svc", []string{"cd sub && sh x.sh && sh run_tests.sh"}, []string{"svc/run_tests.sh", "svc/sub/x.sh"}},
		{"", []string{"cd sub && sh run.sh"}, []string{"run.sh", "sub/run.sh"}}, // the root's reading stays
		{"", []string{"cd $DIR && sh run.sh", "cd ~ && sh tools/check.sh"}, []string{"run.sh", "tools/check.sh"}},
		{"", []string{"cd /tmp && sh run.sh"}, []string{"run.sh"}},
		{"", []string{"cd sub && cd $X && sh run.sh"}, []string{"run.sh"}}, // where the second cd leads is unknown
		{"", []string{"cd sub", "sh run.sh"}, []string{"run.sh"}},          // a later command starts at the module again
		{"svc", []string{"cd ../.. && sh run.sh"}, nil},                    // above the root: nothing
	} {
		scripts, _ := checkFiles(c.verify, c.module, files)
		slices.Sort(scripts)
		if !slices.Equal(scripts, c.want) {
			t.Errorf("%q in %q: scripts %q, want %q", c.verify, c.module, scripts, c.want)
		}
	}
}

// Runner configuration the agent adds, where base had none, is reported as an edited one is: in the module's folder
// and above it, for the runners the commands call; other new files are not.
func TestAddedConfigs(t *testing.T) {
	base := fakeSource{"pom.xml", "svc/pom.xml", "svc/run_tests.sh"}
	changed := []string{"conftest.py", "svc/conftest.py", "svc/sub/conftest.py", ".mvn/maven.config", "svc/new.txt", "pom.xml", "svc/pytest.ini",
		"requirements-dev.txt"}
	got := addedConfigs([]string{"sh run_tests.sh", ": mvn", ": python -m pytest"}, "svc", base, changed)
	slices.Sort(got)
	// pom.xml was in base (an edit, checkFiles' config); svc/sub is no ancestor; new.txt no runner's.
	if want := []string{".mvn/maven.config", "conftest.py", "requirements-dev.txt", "svc/conftest.py", "svc/pytest.ini"}; !slices.Equal(got, want) {
		t.Errorf("added configs %q, want %q", got, want)
	}
	if got := addedConfigs([]string{"sh run_tests.sh"}, "svc", base, changed); got != nil {
		t.Errorf("no runner called: %q", got)
	}
	if got := addedConfigs([]string{"python -m pytest"}, "", fakeSource{"value.txt"}, []string{"pytest.ini", "sub/conftest.py"}); !slices.Equal(got, []string{"pytest.ini"}) {
		t.Errorf("at the root: %q", got)
	}
}

// An agent that edits only the target of a linked verify script is graded with the starting version of the target, at
// the root and in a module; the link and its target are the verification's own scripts.
func TestOnceRestoresALinkedScriptsTarget(t *testing.T) {
	for _, module := range []string{"", "svc"} {
		t.Run("module="+module, func(t *testing.T) {
			inModule := func(p string) string { return strings.TrimPrefix(module+"/"+p, "/") }
			f := newModuleOnceWith(t, module, "decoy", "printf 'exit 0\\n' > "+inModule("scripts/real.sh"), map[string]string{
				inModule("scripts/real.sh"): "sh run_tests.sh\n",
				inModule("check.sh"):        "symlink:scripts/real.sh",
			})
			f.spec.Task.Verify = []string{"sh check.sh"}
			rec, err := Once(context.Background(), f.env, f.spec)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || *rec.Passed {
				t.Fatalf("outcome %s, passed %v, notes %v: the edited target decided the grade", rec.Outcome, rec.Passed, rec.Notes)
			}
			if !strings.Contains(strings.Join(rec.Notes, "\n"), "graded with the starting version: "+inModule("scripts/real.sh")) {
				t.Errorf("notes %v: want the target restored", rec.Notes)
			}
		})
	}
}

// A runner configuration the agent adds (a root conftest.py that skips the tests, a module's pytest.ini) lands in
// ConfigChanged, as an edited one does; at the root as in a module.
func TestOnceReportsAddedRunnerConfig(t *testing.T) {
	for _, module := range []string{"", "svc"} {
		t.Run("module="+module, func(t *testing.T) {
			inModule := func(p string) string { return strings.TrimPrefix(module+"/"+p, "/") }
			f := newModuleOnce(t, module, "decoy", "printf 'new\\n' > "+inModule("value.txt")+"; printf 'skip = 1\\n' > conftest.py; printf '[pytest]\\n' > "+
				inModule("pytest.ini")+"; printf 'x\\n' > "+inModule("notes.txt"))
			f.spec.Task.Verify = []string{"sh run_tests.sh", ": python -m pytest"}
			rec, err := Once(context.Background(), f.env, f.spec)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Passed == nil || !*rec.Passed {
				t.Fatalf("passed %v, notes %v", rec.Passed, rec.Notes)
			}
			want := slices.Sorted(slices.Values(slices.Compact([]string{"conftest.py", inModule("pytest.ini")})))
			if got := slices.Sorted(slices.Values(rec.Behavior.ConfigChanged)); !slices.Equal(got, want) {
				t.Errorf("config changed %q, want %q", got, want)
			}
		})
	}
}
