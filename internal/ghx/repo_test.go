package ghx

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/gitx"
)

func TestParseRemote(t *testing.T) {
	want := Repo{Owner: "octo", Name: "widgets"}
	for _, raw := range []string{
		"https://github.com/octo/widgets", "https://github.com/octo/widgets.git", "https://github.com/octo/widgets/",
		"https://GitHub.com/octo/widgets.git", "https://www.github.com/octo/widgets", "http://github.com/octo/widgets",
		"https://user:ghp_secret@github.com/octo/widgets.git", // secret-scan: allow
		"ssh://git@github.com/octo/widgets.git", "ssh://git@github.com:22/octo/widgets.git", "ssh://git@ssh.github.com:443/octo/widgets.git",
		"git://github.com/octo/widgets.git", "git@github.com:octo/widgets.git", "git@github.com:/octo/widgets.git", "github.com:octo/widgets",
		"  git@github.com:octo/widgets.git\n",
	} {
		got, err := ParseRemote(raw)
		if err != nil || got != want {
			t.Errorf("ParseRemote(%q) = %+v, %v", raw, got, err)
		}
	}
	if got, err := ParseRemote("git@github.com:my-org/repo.name_2.git"); err != nil || got != (Repo{"my-org", "repo.name_2"}) {
		t.Errorf("dotted name: %+v, %v", got, err)
	}
	for _, raw := range []string{
		"", "/local/path/repo", "../repo", "file:///srv/octo/widgets.git", "https://gitlab.com/octo/widgets",
		"https://github.com.evil.example/octo/widgets", "https://evil.example/github.com/octo/widgets",
		"git@github-work:octo/widgets.git", "https://github.com/octo", "https://github.com/octo/widgets/pulls",
		"https://github.com/octo/widgets?x=1", "https://github.com/octo/widgets#frag", "https://github.com/-octo/widgets",
		"https://github.com/octo/..", "https://github.com/octo/wid%2Fgets", "ext::sh -c touch% /tmp/x", "https://ghp_secret@gitlab.com/x/y", // secret-scan: allow
	} {
		got, err := ParseRemote(raw)
		if !errors.Is(err, ErrNotGitHub) {
			t.Errorf("ParseRemote(%q) = %+v, %v; want ErrNotGitHub", raw, got, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("the error quotes the URL: %v", err)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitx.Environ(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestRepoOf(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "remote", "add", "origin", "https://x-access-token:ghs_secret@github.com/octo/widgets.git") // secret-scan: allow
	git(t, dir, "remote", "add", "upstream", "git@gitlab.com:octo/widgets.git")
	if got, err := RepoOf(ctx, dir, "origin"); err != nil || got != (Repo{"octo", "widgets"}) {
		t.Errorf("origin: %+v, %v", got, err)
	}
	for _, remote := range []string{"upstream", "missing", "--help", "-v", ""} {
		_, err := RepoOf(ctx, dir, remote)
		if err == nil {
			t.Errorf("remote %q accepted", remote)
		} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "gitlab") {
			t.Errorf("remote %q: the error quotes a URL: %v", remote, err)
		}
	}
	if _, err := RepoOf(ctx, t.TempDir(), "origin"); err == nil {
		t.Error("a folder that is not a repository was accepted")
	}
}

// The packages a run executes in (the agent's run, its sandbox, its checkout, grading, the experiment engine and the
// judge) never reach this package, directly or through another: gh runs only in the user's own Agentium process.
func TestRunPathDoesNotImportGHX(t *testing.T) {
	const module = "github.com/pigeaca/agentium/"
	root := filepath.Join("..", "..")
	imports := map[string][]string{} // package path (relative to the module) -> internal imports
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (strings.HasPrefix(d.Name(), ".") && path != root || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		for _, spec := range file.Imports {
			p, _ := strconv.Unquote(spec.Path.Value)
			if dep, ok := strings.CutPrefix(p, module); ok && !slices.Contains(imports[rel], dep) {
				imports[rel] = append(imports[rel], dep)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(imports) < 10 {
		t.Fatalf("found only %d packages; is the module root right?", len(imports))
	}
	for _, start := range []string{"internal/run", "internal/runner", "internal/claude", "internal/checkout", "internal/task",
		"internal/experiment", "internal/judge", "internal/buildtool"} {
		seen := map[string]bool{}
		var reaches func(pkg string, path []string) []string
		reaches = func(pkg string, path []string) []string {
			if pkg == "internal/ghx" {
				return path
			}
			if seen[pkg] {
				return nil
			}
			seen[pkg] = true
			for _, dep := range imports[pkg] {
				if found := reaches(dep, append(slices.Clone(path), dep)); found != nil {
					return found
				}
			}
			return nil
		}
		if path := reaches(start, []string{start}); path != nil {
			t.Errorf("%s reaches internal/ghx: %s", start, strings.Join(path, " -> "))
		}
	}
}
