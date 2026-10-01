// Package home locates Agentium's data folder. It lives outside every user repository, so Agentium never writes to the
// repositories it measures.
package home

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Layout is the data folder: the SQLite database, per-project artifacts (validation logs, reports), and runs. A run's
// workspace (the agent's checkout) lives apart from its records (transcripts, verification copies with hidden tests),
// so the agent can be denied everything but its own workspace.
type Layout struct {
	Root       string
	Database   string
	Artifacts  string
	Workspaces string
	Records    string
	// Cache holds the build caches of the commands Agentium runs itself (setup, validation, grading). Agents may not
	// read it: it holds compiled hidden tests.
	Cache string
	// Temp holds the runs' own Claude Code temp roots (RunTemp): /tmp, outside the data folder, because the root's
	// path must stay short (claude.TempRootFits). Empty in a Layout not made by Resolve: runs refuse to start then.
	Temp string
}

// RunTempPrefix starts the name of every run's temp root in Layout.Temp.
const RunTempPrefix = "ag-"

// runTempHex is how many hex characters follow RunTempPrefix in a run's temp root name (40 bits of a hash).
const runTempHex = 10

// RunTemp is the temp root (CLAUDE_CODE_TMPDIR) of the run in the named workspace, or "" when l.Temp is unset. The
// name is derived from the data folder and the workspace's name, not random, so a run can deny the roots of runs that
// may overlap it before they exist, as it denies their workspaces (run.Env.Predicted), and recovery finds a dead run's
// root from its start file's workspace alone. On macOS and Windows the data folder's path is lower-cased first: their
// file systems ignore case, so ~/.Agentium and ~/.agentium are one data folder and get the same roots.
//
// Since the name can be guessed and Temp is shared with other users, whoever creates it must refuse a folder someone
// else made (run's makeRunTemp): another local user who creates a predicted root first stops that run, with an error
// naming the folder, but cannot get into it.
//
// Roots are removed when their run ends, and by recovery for runs whose Agentium process died. One is left in Temp
// only when its data folder, or that run's records, are deleted before recovery: a small, owner-only folder, which
// later runs are denied and which the system's /tmp cleaning removes. Agentium does not sweep such roots, since a
// root's name does not say which data folder it belongs to.
func (l Layout) RunTemp(workspace string) string {
	if l.Temp == "" {
		return ""
	}
	root := realPath(l.Root)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		root = strings.ToLower(root)
	}
	sum := sha256.Sum256([]byte(root + "\x00" + workspace))
	return filepath.Join(l.Temp, RunTempPrefix+hex.EncodeToString(sum[:])[:runTempHex])
}

// IsRunTempName reports whether name has the form of a run's temp root name (whatever its data folder).
func IsRunTempName(name string) bool {
	hexPart, ok := strings.CutPrefix(name, RunTempPrefix)
	if !ok || len(hexPart) != runTempHex {
		return false
	}
	for _, c := range hexPart {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// Resolve returns the layout under $AGENTIUM_HOME, or ~/.agentium when that is unset. getenv is os.Getenv outside tests.
func Resolve(getenv func(string) string) (Layout, error) {
	root := getenv("AGENTIUM_HOME")
	if root == "" {
		userHome := getenv("HOME")
		if userHome == "" {
			return Layout{}, fmt.Errorf("resolve data folder: neither AGENTIUM_HOME nor HOME is set")
		}
		root = filepath.Join(userHome, ".agentium")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve data folder %q: %w", root, err)
	}
	return Layout{Root: root, Database: filepath.Join(root, "agentium.db"), Artifacts: filepath.Join(root, "artifacts"),
		Workspaces: filepath.Join(root, "workspaces"), Records: filepath.Join(root, "records"), Cache: filepath.Join(root, "cache"),
		Temp: "/tmp"}, nil
}

// Ensure creates missing folders readable only by the owner: transcripts and checkouts contain source code. It never
// changes an existing folder (AGENTIUM_HOME may point anywhere, even at the home folder); an existing data folder that
// other users can open is refused instead.
func (l Layout) Ensure() error {
	info, err := os.Stat(l.Root)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("data folder %s is not a folder", l.Root)
	case err == nil && info.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("data folder %s is open to other users (mode %04o): run `chmod 700 %s`, or set AGENTIUM_HOME to a new folder",
			l.Root, info.Mode().Perm(), l.Root)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("data folder %s: %w", l.Root, err)
	}
	if err := os.MkdirAll(filepath.Dir(l.Root), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(l.Root), err)
	}
	for _, dir := range []string{l.Root, l.Artifacts, l.Workspaces, l.Records} {
		err := os.Mkdir(dir, 0o700)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil { // Mkdir applies the umask; this folder is new, so restrict it
			return fmt.Errorf("restrict %s: %w", dir, err)
		}
	}
	return nil
}

// ProjectRepo is the bare repository holding a project's snapshots and fetched commits.
func (l Layout) ProjectRepo(projectID int64) string {
	return filepath.Join(l.Root, "projects", strconv.FormatInt(projectID, 10), "repo.git")
}

// CheckOutside refuses a data folder that is the repository at repoRoot (symlinks resolved) or inside it: writing
// there would change the repository Agentium promises only to read. Call it before Ensure creates anything.
func (l Layout) CheckOutside(repoRoot string) error {
	// Lower case: on case-insensitive file systems (macOS, Windows) "Repo" and "repo" are the same folder.
	rel, err := filepath.Rel(strings.ToLower(repoRoot), strings.ToLower(realPath(l.Root)))
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("the data folder %s is inside the repository %s: set AGENTIUM_HOME to a folder outside it", l.Root, repoRoot)
	}
	return nil
}

// realPath resolves symbolic links in the longest existing prefix of p, which may not exist yet.
func realPath(p string) string {
	var missing []string
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
