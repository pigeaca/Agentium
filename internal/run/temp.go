package run

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pigeaca/agentium/internal/home"
)

// A run's temp root (home.Layout.RunTemp) is Claude Code's CLAUDE_CODE_TMPDIR for its agent: a short folder in /tmp of
// the run's own, so the agent never needs the user's shared Claude Code temp folder, which it is denied
// (claude.Invocation.TempRoot). It lives outside the data folder because the data folder's path can be long, and a
// socket path too long makes Claude Code fall back to the shared folder. /tmp is shared with other users and the name
// is predictable, so it is created only fresh, owner-only, and never trusted when someone else made it.

// makeRunTemp creates root, owner-only. A folder of the user's own already there (left by a run whose Agentium process
// died) is removed first; anything else there (another user's folder, a link, a file) is refused, and the run stops:
// another local user can block a run that way, by creating its predictable root first, but never see into it.
func makeRunTemp(root string) error {
	if err := removeRunTemp(root); err != nil {
		return err
	}
	if _, err := os.Lstat(root); err == nil {
		return fmt.Errorf("the run's temp root %s exists and is not the user's own folder: remove it, or find who made it", root)
	}
	if err := os.Mkdir(root, 0o700); err != nil { // fails if anyone made it meanwhile
		return fmt.Errorf("the run's temp root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil { // Mkdir applies the umask
		return fmt.Errorf("the run's temp root: %w", err)
	}
	if !ownFolder(root) {
		return fmt.Errorf("the run's temp root %s is not the user's own folder", root)
	}
	return nil
}

// removeRunTemp removes root with what the run's Claude Code left in it, when it is a folder of the user's own; it
// leaves anything else alone (makeRunTemp refuses it). A missing root is not an error.
func removeRunTemp(root string) error {
	if root == "" || !ownFolder(root) {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove the run's temp root: %w", err)
	}
	return nil
}

// ownFolder reports whether p is a folder (not a link) owned by this process's user.
func ownFolder(p string) bool {
	info, err := os.Lstat(p)
	if err != nil || !info.IsDir() {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// runTemps lists the temp roots in layout.Temp other than own: those of runs that started earlier, of any data folder
// (concurrent ones, or ones a dead process left), which the run's agent may not read or write. A folder that cannot be
// listed is an error: the run would otherwise start without denying them.
func runTemps(layout home.Layout, own string) ([]string, error) {
	if layout.Temp == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(layout.Temp)
	if err != nil {
		return nil, fmt.Errorf("list the runs' temp roots: %w", err)
	}
	var roots []string
	for _, e := range entries {
		if p := filepath.Join(layout.Temp, e.Name()); home.IsRunTempName(e.Name()) && p != own {
			roots = append(roots, p)
		}
	}
	return roots, nil
}
