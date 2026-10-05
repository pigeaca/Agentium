package run

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
)

// Session folders. With the user's login, Claude Code keeps every run's session in the user's own config folder:
// <config>/projects/<the agent's starting folder, encoded> (claude.SessionFolder), which outlives the run's workspace.
// Runs pass --no-session-persistence, so a run's folder holds no session file (a top-level .jsonl), only the large
// tool outputs and subagent files Claude Code saved there. Agentium removes such a folder in three places:
//   - Once, for its own run, where it removes the workspace (Env.removeOwnSession): the folder it computed for itself
//     before the agent started;
//   - recovery, for a dead Agentium's run, from its start file (recoverSession);
//   - cleanup's sessions kind, for what earlier runs of this data folder left (planner.sessions, removeSessionItem).
//
// This is the only place Agentium deletes outside its data folder, so every removal holds to the same rules:
//   - the projects folder is a real folder of this user's, not a link, opened once as a root (os.Root) that is checked
//     to be that folder; nothing else in the config folder is opened, listed or changed;
//   - the session folder is a direct child of it, a real folder of this user's, not a link, opened through that root
//     and checked to be the folder looked at (a swap for a link or another folder between the look and the open is
//     refused);
//   - it holds no session file of its own: the user's own projects do, a run's never does;
//   - its content is removed through its own descriptor (os.Root.RemoveAll, which never follows a link and never leaves
//     the folder), and the folder itself only while its name is still that folder.
//
// Errors never name the folder: its name holds the user's home path (nameless).

// sessionRest matches what follows the workspaces folder in the name of a run's session folder: a workspace's name (a
// run's ID, NewID, or an experiment slot's, e<experiment>-s<slot>-t<try>), "-repo", and the module's path, if any.
var sessionRest = regexp.MustCompile(`^([0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}|e[0-9]+-s[0-9]+-t[0-9]+)-repo(?:-[A-Za-z0-9-]+)?$`)

// sessionWorkspace recognises the name of a session folder a run of the data folder whose workspaces folder is
// workspaces left (with its real path: claude.SessionFolder resolves the agent's folder), and returns the run's
// workspace name. Two data folders whose paths differ only in characters other than letters and digits give the same
// names (claude.SessionFolderUnder): the other conditions of a removal (no workspace of that name, no session file,
// unused for CleanGrace) are what keep another data folder's live or kept run's folder.
func sessionWorkspace(workspaces, name string) (string, bool) {
	rest, ok := claude.SessionFolderUnder(workspaces, name)
	if !ok {
		return "", false
	}
	m := sessionRest.FindStringSubmatch(rest)
	if m == nil {
		return "", false
	}
	return m[1], true
}

var (
	// errProjectsNotReal: the projects folder is a link, not a folder, or another user's.
	errProjectsNotReal = errors.New("Claude Code's projects folder is a link, or not a folder of yours")
	// errSessionNotReal: the session folder is a link, a file, or another user's.
	errSessionNotReal = errors.New("it is a link, a file, or not yours")
	// errSessionChanged: the folder was swapped while it was being looked at or removed.
	errSessionChanged = errors.New("it changed while it was being removed")
	// errSessionFile: the folder holds a session file of its own, which no run leaves.
	errSessionFile = errors.New("it holds a session file of its own, which no run leaves")
)

// nameless is err without the path it names (an *fs.PathError's or *os.LinkError's): a session folder's name holds the
// user's home path.
func nameless(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Errorf("%s: %w", le.Op, le.Err)
	}
	return err
}

// ownedByMe reports whether info is of a file this process's user owns.
func ownedByMe(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Getuid())
}

// openProjects opens Claude Code's projects folder as a root: only a real folder of this user's, and only that folder
// (one swapped in between the look and the open is refused). A missing folder is fs.ErrNotExist.
func openProjects(projects string) (*os.Root, error) {
	info, err := os.Lstat(projects)
	if err != nil {
		return nil, nameless(err)
	}
	if !info.IsDir() || !ownedByMe(info) {
		return nil, errProjectsNotReal
	}
	root, err := os.OpenRoot(projects)
	if err != nil {
		return nil, nameless(err)
	}
	if here, err := root.Stat("."); err != nil || !os.SameFile(here, info) {
		root.Close()
		return nil, errProjectsNotReal
	}
	return root, nil
}

// sessionLook is what lookSession saw of a session folder.
type sessionLook struct {
	info        fs.FileInfo // the folder's own (never a link's target)
	sessionFile bool        // it holds a top-level .jsonl
	bytes       int64       // its disk space (treeSize's rules)
	newest      time.Time   // the newest modification in it, itself included
}

// lookSession looks at the entry name of projects (a root from openProjects): it must be a real folder of this user's,
// and it is opened as a root of its own, checked to be the folder looked at. Its size and newest modification are read
// through that root, which never leaves it. The caller closes the returned root.
func lookSession(projects *os.Root, name string) (sessionLook, *os.Root, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return sessionLook{}, nil, errSessionNotReal
	}
	info, err := projects.Lstat(name)
	if err != nil {
		return sessionLook{}, nil, nameless(err)
	}
	if !info.IsDir() || !ownedByMe(info) {
		return sessionLook{}, nil, errSessionNotReal
	}
	sub, err := projects.OpenRoot(name)
	if err != nil {
		return sessionLook{}, nil, nameless(err)
	}
	if here, err := sub.Stat("."); err != nil || !os.SameFile(here, info) {
		sub.Close()
		return sessionLook{}, nil, errSessionChanged
	}
	look := sessionLook{info: info, newest: info.ModTime()}
	seen := map[[2]uint64]bool{}
	_ = fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // what cannot be read counts as nothing
		}
		if p != "." && path.Dir(p) == "." && strings.HasSuffix(d.Name(), ".jsonl") {
			look.sessionFile = true
		}
		fi, err := sub.Lstat(p)
		if err != nil {
			return nil
		}
		if fi.ModTime().After(look.newest) {
			look.newest = fi.ModTime()
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			look.bytes += fi.Size()
			return nil
		}
		key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
		if st.Nlink > 1 {
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		look.bytes += int64(st.Blocks) * 512
		return nil
	})
	return look, sub, nil
}

// removeSession removes the session folder name in the projects folder projects, under the rules at the top of this
// file; allow, when set, may refuse it after the look. A missing projects folder or session folder is nothing to do.
func removeSession(projects, name string, allow func(sessionLook) error) error {
	root, err := openProjects(projects)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	look, sub, err := lookSession(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer sub.Close()
	if look.sessionFile {
		return errSessionFile
	}
	if allow != nil {
		if err := allow(look); err != nil {
			return err
		}
	}
	dir, err := sub.Open(".")
	if err != nil {
		return nameless(err)
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return nameless(err)
	}
	// Through the folder's own root: whatever is renamed or linked meanwhile, nothing outside the folder looked at goes.
	var first error
	for _, n := range names {
		if err := sub.RemoveAll(n); err != nil && first == nil {
			first = nameless(err)
		}
	}
	if first != nil {
		return first
	}
	// The folder itself goes only while its name is still the folder that was emptied (Remove takes an empty folder).
	now, err := root.Lstat(name)
	if err != nil {
		return nameless(err)
	}
	if !os.SameFile(now, look.info) {
		return errSessionChanged
	}
	return nameless(root.Remove(name))
}

// removeOwnSession removes the session folder Claude Code kept for this run with the user's login (Env.ownSession,
// computed before the agent started), and says in a note why it could not; a failure never fails the run.
func (env Env) removeOwnSession() string {
	if env.ownSession == "" {
		return ""
	}
	if err := removeSession(filepath.Dir(env.ownSession), filepath.Base(env.ownSession), nil); err != nil {
		return "Claude Code's session folder of this run was left in its projects folder: " + err.Error()
	}
	return ""
}

// recoverSession removes the session folder a dead run's start file names (start.Session), once its workspace is
// gone: only one in a folder named projects, whose name is this data folder's session folder name of the start file's
// own workspace (sessionWorkspace).
func recoverSession(layout home.Layout, s start) error {
	if s.Session == "" {
		return nil
	}
	projects, name := filepath.Dir(s.Session), filepath.Base(s.Session)
	ws, ok := sessionWorkspace(realPath(layout.Workspaces), name)
	if !filepath.IsAbs(s.Session) || filepath.Base(projects) != "projects" || !ok || ws != filepath.Base(s.Workspace) {
		return errors.New("its start file names a folder that is not the run's: it was left")
	}
	if _, err := os.Lstat(filepath.Join(layout.Workspaces, ws)); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("its workspace is still there: it was left")
	}
	return removeSession(projects, name, nil)
}

// sessionRef is a sessions item's folder: the projects folder and the folder's name in it.
type sessionRef struct {
	projects, name string
}

// The reasons of the sessions kind (CleanItem.Reason).
const (
	// CleanRunSession: a session folder a run of this data folder left in Claude Code's projects folder.
	CleanRunSession = "left_by_run_session"
	// CleanKeptWorkspace: its run's workspace is there (a run in progress, one kept with --keep, a dead run's leftovers).
	CleanKeptWorkspace = "workspace_exists"
	// CleanKeptSessionFile: it holds a session file of its own, which no run leaves.
	CleanKeptSessionFile = "holds_session_file"
)

// sessions plans the session folders runs of this data folder left in Claude Code's projects folder (CleanInput.ClaudeConfig;
// none without it). Only names sessionWorkspace recognises are looked at; a link or a file of such a name is left out,
// a folder whose run's workspace exists or that holds a session file is kept, and one used within CleanGrace too.
func (c *planner) sessions(ctx context.Context) error {
	if c.in.ClaudeConfig == "" || c.in.Layout.Workspaces == "" {
		return nil
	}
	projects := filepath.Join(c.in.ClaudeConfig, "projects")
	root, err := openProjects(projects)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		c.plan.Notes = append(c.plan.Notes, "what runs left in Claude Code's projects folder is not listed: "+err.Error())
		return nil
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		c.plan.Notes = append(c.plan.Notes, "what runs left in Claude Code's projects folder is not listed: "+nameless(err).Error())
		return nil
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		c.plan.Notes = append(c.plan.Notes, "what runs left in Claude Code's projects folder is not listed: "+nameless(err).Error())
		return nil
	}
	workspaces := realPath(c.in.Layout.Workspaces)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		ws, ok := sessionWorkspace(workspaces, name)
		if !ok {
			continue // not a run's: never looked at
		}
		look, sub, err := lookSession(root, name)
		if err != nil {
			continue // a link, a file, another user's, or gone
		}
		sub.Close()
		it := CleanItem{Kind: CleanSessions, Path: filepath.Join(projects, name), Bytes: look.bytes, LastUsed: look.newest,
			session: &sessionRef{projects: projects, name: name}}
		_, wsErr := os.Lstat(filepath.Join(c.in.Layout.Workspaces, ws))
		switch age := c.in.Now.Sub(look.newest); {
		case !errors.Is(wsErr, fs.ErrNotExist):
			c.add(it, verdict{reason: CleanKeptWorkspace, detail: "its run's workspace is still there (a run in progress, one kept with --keep, or a stopped run's leftovers)"})
		case look.sessionFile:
			c.add(it, verdict{reason: CleanKeptSessionFile, detail: "named like a run's, but it holds a session file of its own, which no run leaves"})
		case age < CleanGrace:
			c.add(it, verdict{reason: CleanKeptRecent, detail: "used " + ago(age) + " ago"})
		default:
			c.add(it, verdict{gone: true, reason: CleanRunSession, detail: "left by run " + ws + " in Claude Code's projects folder"})
		}
	}
	return nil
}

// removeSessionItem removes a sessions item, checking each condition again first: the name is this data folder's, no
// workspace of its run exists, and removeSession's rules; it is not used since the plan (ErrCleanUsed). It is never
// moved into the quarantine: it may be on another volume.
func removeSessionItem(layout home.Layout, it CleanItem) error {
	ref := it.session
	if ref == nil {
		return errors.New("refused: not a session folder cleanup listed")
	}
	ws, ok := sessionWorkspace(realPath(layout.Workspaces), ref.name)
	if !ok {
		return errors.New("refused: not the session folder of a run of this data folder")
	}
	if _, err := os.Lstat(filepath.Join(layout.Workspaces, ws)); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("refused: its run's workspace is there now")
	}
	err := removeSession(ref.projects, ref.name, func(look sessionLook) error {
		if look.newest.After(it.LastUsed) {
			return ErrCleanUsed
		}
		return nil
	})
	if errors.Is(err, errSessionFile) { // it had none when listed
		return ErrCleanUsed
	}
	return err
}
