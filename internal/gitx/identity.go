package gitx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// localeVariables are the variables that set the locale git matches text in, in the order the C library reads them.
var localeVariables = []string{"LC_ALL", "LC_CTYPE", "LANG"}

// Identity tells one git, as Agentium runs it, from another: the program found on PATH, its version and build
// options, and the locale variables it runs under. An answer of git that Agentium keeps between commands
// (task.GapsCache) is kept for one identity: a search that ignores case can match differently under another build of
// git or another locale, so what one found says nothing sure about the other.
func Identity(ctx context.Context) (string, error) {
	program, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git's identity: %w", err)
	}
	version, err := Run(ctx, "version", "--build-options")
	if err != nil {
		return "", fmt.Errorf("git's identity: %w", err)
	}
	parts := []string{program, version}
	for _, name := range localeVariables { // git gets them from this process: Environ keeps them
		parts = append(parts, name+"="+os.Getenv(name))
	}
	return strings.Join(parts, "\n"), nil
}

// Replaced reports whether the repository whose git folder is gitDir has replacement refs (refs/replace/...). With
// one, git reads another object in place of the one an ID names (unless configuration turns that off), so the same
// commit ID can give other files than it did: what was worked out from a commit's ID cannot be kept for it then
// (task.GapsCache). Agentium makes none; only someone working in its repository by hand does.
//
// The refs are looked for where git keeps them as files, loose under refs/replace and packed in packed-refs, so the
// usual answer starts no process; a repository that keeps its refs another way (reftable) is asked through git.
func Replaced(ctx context.Context, gitDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(gitDir, "reftable")); err == nil {
		out, err := Run(ctx, "--git-dir", gitDir, "for-each-ref", "--count=1", "--format=%(refname)", "refs/replace/")
		if err != nil {
			return false, fmt.Errorf("look for replacement refs: %w", err)
		}
		return out != "", nil
	}
	loose := false
	err := filepath.WalkDir(filepath.Join(gitDir, "refs", "replace"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			loose = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("look for replacement refs: %w", err)
	}
	if loose {
		return true, nil
	}
	packed, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look for replacement refs: %w", err)
	}
	defer packed.Close()
	lines := bufio.NewScanner(packed)
	lines.Buffer(make([]byte, 64<<10), maxLine)
	for lines.Scan() { // "<object> <ref>", after a header line and between "^<peeled object>" lines
		if _, ref, ok := strings.Cut(lines.Text(), " "); ok && strings.HasPrefix(ref, "refs/replace/") {
			return true, nil
		}
	}
	if err := lines.Err(); err != nil {
		return false, fmt.Errorf("look for replacement refs: %w", err)
	}
	return false, nil
}
