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
	"slices"
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
// names (claude.SessionFolderUnder), so a name alone never proves a folder this data folder's: cleanup also requires
// that this data folder's store knows the workspace's run (CleanInput.KnownWorkspace). What a colliding data folder can
// still lose is a folder of a run whose workspace name both stores know (the same run ID, which is random, or the same
// experiment, slot and try), once it is unused for CleanGrace: only this data folder's workspaces are looked for, so
// that run's kept workspace over there does not keep it.
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
	// As cleanup checks: an absolute path, and a name that fits this data folder's runs (a relative CLAUDE_CONFIG_DIR, or
	// a workspace name Agentium does not make, leaves it).
	ws, ok := sessionWorkspace(realPath(env.Layout.Workspaces), filepath.Base(env.ownSession))
	if !filepath.IsAbs(env.ownSession) || !ok || ws != env.workspaceName() {
		// A relative path cannot be looked at (runs refuse a relative config folder, so none gets here).
		if _, err := os.Lstat(env.ownSession); filepath.IsAbs(env.ownSession) && err != nil {
			return "" // nothing there to leave
		}
		return "Claude Code's session folder of this run was left in its projects folder: its path is not one Agentium's runs make"
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
	projects, name, ws, ok := startSession(layout, s)
	if !ok {
		return errors.New("its start file names a folder that is not the run's: it was left")
	}
	if _, err := os.Lstat(filepath.Join(layout.Workspaces, ws)); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("its workspace is still there: it was left")
	}
	return removeSession(projects, name, nil)
}

// startSession is the session folder a start file names (start.Session), split into its projects folder and name, with
// its workspace's name: ok only for an absolute path in a folder named projects whose name is this data folder's session
// folder name of the start file's own workspace.
func startSession(layout home.Layout, s start) (projects, name, ws string, ok bool) {
	if s.Session == "" || !filepath.IsAbs(s.Session) {
		return "", "", "", false
	}
	projects, name = filepath.Dir(s.Session), filepath.Base(s.Session)
	ws, ok = sessionWorkspace(realPath(layout.Workspaces), name)
	if !ok || filepath.Base(projects) != "projects" || ws != filepath.Base(s.Workspace) {
		return "", "", "", false
	}
	return projects, name, ws, true
}

// leftoverSession is the part of a dead run's leftovers (planner.leftovers) that is its session folder: what recovery
// removes of it (recoverSession), sized through the same looks; ok is false when recovery would leave it.
func leftoverSession(layout home.Layout, s start) (cleanPart, bool) {
	projects, name, _, ok := startSession(layout, s)
	if !ok {
		return cleanPart{}, false
	}
	root, err := openProjects(projects)
	if err != nil {
		return cleanPart{}, false
	}
	defer root.Close()
	look, sub, err := lookSession(root, name)
	if err != nil {
		return cleanPart{}, false
	}
	sub.Close()
	if look.sessionFile {
		return cleanPart{}, false
	}
	return cleanPart{path: filepath.Join(projects, name), bytes: look.bytes, session: look.info}, true
}

// sessionRef is a sessions item's folder: the projects folder and the folder's name in it, and how to ask the store
// again at removal (CleanInput.KnownWorkspace).
type sessionRef struct {
	projects, name string
	known          func(name string) (bool, error)
}

// The reasons of the sessions kind (CleanItem.Reason).
const (
	// CleanRunSession: a session folder a run of this data folder left in Claude Code's projects folder.
	CleanRunSession = "left_by_run_session"
	// CleanKeptWorkspace: its run's workspace is there (a run in progress, one kept with --keep, a dead run's leftovers).
	CleanKeptWorkspace = "workspace_exists"
	// CleanKeptSessionFile: it holds a session file of its own, which no run leaves.
	CleanKeptSessionFile = "holds_session_file"
	// CleanKeptUnknownRun: its name fits, but this data folder's store knows no run of that workspace: it may be another
	// data folder's whose path encodes the same.
	CleanKeptUnknownRun = "unknown_run"
)

// sessions plans the session folders runs of this data folder left in Claude Code's projects folder (CleanInput.ClaudeConfig;
// none without it). Only names sessionWorkspace recognises are looked at; a link or a file of such a name is left out,
// a folder whose run's workspace exists, that holds a session file, whose workspace's run the store does not know
// (CleanInput.KnownWorkspace) or that was used within CleanGrace is kept.
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
	// Dead runs' session folders, planned with their leftovers: compared by identity (os.SameFile), since a start file
	// may spell the config folder through a link and this cleanup by its real path, or the other way round.
	var leftover []fs.FileInfo
	for _, it := range append(slices.Clone(c.plan.Remove), c.plan.Keep...) {
		if it.Kind == CleanLeftovers {
			for _, p := range it.parts {
				if p.session != nil {
					leftover = append(leftover, p.session)
				}
			}
		}
	}
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
		if slices.ContainsFunc(leftover, func(info fs.FileInfo) bool { return os.SameFile(info, look.info) }) {
			continue // a dead run's: planned, and counted, with its leftovers (recovery removes it)
		}
		it := CleanItem{Kind: CleanSessions, Path: filepath.Join(projects, name), Bytes: look.bytes, LastUsed: look.newest,
			session: &sessionRef{projects: projects, name: name, known: c.in.KnownWorkspace}}
		_, wsErr := os.Lstat(filepath.Join(c.in.Layout.Workspaces, ws))
		switch age := c.in.Now.Sub(look.newest); {
		case !errors.Is(wsErr, fs.ErrNotExist):
			c.add(it, verdict{reason: CleanKeptWorkspace, detail: "its run's workspace is still there (a run in progress, one kept with --keep, or a stopped run's leftovers)"})
		case look.sessionFile:
			c.add(it, verdict{reason: CleanKeptSessionFile, detail: "named like a run's, but it holds a session file of its own, which no run leaves"})
		case !c.knownWorkspace(ws):
			c.add(it, verdict{reason: CleanKeptUnknownRun, detail: "named like a run's, but this data folder has no run " + ws + ": perhaps another data folder's"})
		case age < CleanGrace:
			c.add(it, verdict{reason: CleanKeptRecent, detail: "used " + ago(age) + " ago"})
		default:
			c.add(it, verdict{gone: true, reason: CleanRunSession, detail: "left by run " + ws + " in Claude Code's projects folder"})
		}
	}
	return nil
}

// knownWorkspace reports whether the store knows the run of the named workspace (CleanInput.KnownWorkspace); with no
// way to ask, or when asking fails, it knows none, and the folder is kept.
func (c *planner) knownWorkspace(ws string) bool {
	if c.in.KnownWorkspace == nil {
		return false
	}
	known, err := c.in.KnownWorkspace(ws)
	return err == nil && known
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
	// The same proof as the listing's: this data folder's store knows the run.
	if ref.known == nil {
		return errors.New("refused: this data folder has no run " + ws)
	}
	if known, err := ref.known(ws); err != nil || !known {
		return errors.Join(errors.New("refused: this data folder has no run "+ws), err)
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
