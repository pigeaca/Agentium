package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Identity names the program, its version and the locale, and changes when any locale variable does; the variables
// it reads reach git, since Environ keeps them.
func TestIdentityChangesWithTheLocale(t *testing.T) {
	ctx := context.Background()
	for _, name := range localeVariables {
		t.Setenv(name, "")
	}
	plain, err := Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain, "git version ") || !strings.Contains(plain, "/git\n") || !strings.Contains(plain, "LC_ALL=\nLC_CTYPE=\nLANG=") {
		t.Errorf("identity = %q, want the program's path, its version and the locale variables", plain)
	}
	if again, err := Identity(ctx); err != nil || again != plain {
		t.Errorf("asked again: %q, %v; want the same", again, err)
	}
	seen := map[string]string{plain: "no locale"}
	for _, name := range localeVariables {
		t.Setenv(name, "tr_TR.UTF-8")
		got, err := Identity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if earlier, ok := seen[got]; ok {
			t.Errorf("%s=tr_TR.UTF-8 has the identity of %s", name, earlier)
		}
		seen[got] = name
		t.Setenv(name, "")
		if kept := strings.Join(Environ([]string{name + "=tr_TR.UTF-8"}), " "); !strings.Contains(kept, name+"=tr_TR.UTF-8") {
			t.Errorf("Environ drops %s, which Identity reads: %q", name, kept)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Identity(cancelled); err == nil {
		t.Error("a cancelled context got an identity")
	}
}

// A git that ran and failed is an *ExitError with its status, so a caller can tell git's own "nothing matches" (git
// grep's status 1, with nothing written) from a git that a signal ended, which answered nothing. The message is the
// one such failures always had.
func TestExitErrorCarriesGitsStatus(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := Run(ctx, append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("some text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "one")

	_, err := Output(ctx, nil, "-C", repo, "grep", "-F", "-l", "-e", "no such text", "HEAD")
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 1 || exit.Stderr != "" || !strings.HasSuffix(err.Error(), "HEAD: ") {
		t.Errorf("a search that matches nothing: %#v (%v); want status 1 and an empty message", exit, err)
	}
	_, err = Output(ctx, nil, "-C", repo, "grep", "-F", "-l", "-e", "x", "no-such-commit")
	if !errors.As(err, &exit) || exit.Code <= 1 || exit.Stderr == "" || !strings.Contains(err.Error(), exit.Stderr) {
		t.Errorf("a search git refuses: %#v (%v); want a status above 1 and git's message", exit, err)
	}
	if complete, err := Lines(ctx, nil, func(string) bool { return true }, "-C", repo, "grep", "-F", "-l", "-e", "no such text", "HEAD"); complete || !errors.As(err, &exit) || exit.Code != 1 {
		t.Errorf("Lines for a search that matches nothing: %v, %v", complete, err)
	}

	// A git that a signal ends: status -1, and the same empty message as "nothing matches".
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nkill -9 $$\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = Output(ctx, nil, "-C", repo, "grep", "-F", "-l", "-e", "some text", "HEAD")
	if !errors.As(err, &exit) || exit.Code != -1 || exit.Stderr != "" {
		t.Errorf("a git that was killed: %#v (%v); want status -1", exit, err)
	}
}

// Replaced finds replacement refs wherever git keeps them: loose, packed, or in a reftable.
func TestReplacedFindsReplacementRefs(t *testing.T) {
	ctx := context.Background()
	for _, format := range []string{"files", "reftable"} {
		t.Run(format, func(t *testing.T) {
			repo := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				out, err := Run(ctx, append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			if _, err := Run(ctx, "init", "-q", "-b", "main", "--ref-format="+format, repo); err != nil {
				t.Skipf("this git cannot make a %s repository: %v", format, err)
			}
			gitDir := filepath.Join(repo, ".git")
			if (format == "reftable") != exists(filepath.Join(gitDir, "reftable")) {
				t.Fatalf("a %s repository: reftable folder present %v", format, exists(filepath.Join(gitDir, "reftable")))
			}
			var commits []string
			for _, text := range []string{"one\n", "two\n"} {
				if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
				git("add", "-A")
				git("commit", "-q", "-m", text)
				commits = append(commits, git("rev-parse", "HEAD"))
			}
			want := func(replaced bool, when string) {
				t.Helper()
				if got, err := Replaced(ctx, gitDir); err != nil || got != replaced {
					t.Errorf("%s: Replaced = %v, %v; want %v", when, got, err, replaced)
				}
			}
			want(false, "a repository without replacement refs")
			git("pack-refs", "--all")
			want(false, "the same with its refs packed")
			git("replace", commits[1], commits[0])
			want(true, "with a replacement ref")
			git("pack-refs", "--all")
			want(true, "with the replacement ref packed")
			git("replace", "-d", commits[1])
			want(false, "after the replacement ref was deleted")
		})
	}
	if replaced, err := Replaced(ctx, filepath.Join(t.TempDir(), "no-such-repository")); err != nil || replaced {
		t.Errorf("a folder that is not there: %v, %v", replaced, err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
