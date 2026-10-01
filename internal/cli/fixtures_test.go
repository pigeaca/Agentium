package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// Fixtures are built once per test run and copied into each test, because most of a test's wall time used to go to
// git and `agentium` commands that rebuild the same repository, data folder and snapshots. A copy is private to its
// test (own repository, data folder and home), so parallel tests share nothing mutable. The copies carry the template's
// commit hashes and record IDs; the absolute paths stored in them (the project root, log and record locations) are
// rewritten to the copy's own.

var templateRoot string // removed after the run; set by TestMain

func TestMain(m *testing.M) {
	removeStaleTemplates()
	dir, err := os.MkdirTemp("", fmt.Sprintf("%s%d-", templatePrefix, os.Getpid()))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	templateRoot = dir
	code := 1
	defer func() { // also when a test panics; a test calling os.Exit leaves the folder to removeStaleTemplates
		os.RemoveAll(dir)
		if r := recover(); r != nil {
			panic(r)
		}
		os.Exit(code)
	}()
	code = m.Run()
}

// templatePrefix names template folders "agentium-cli-fixtures-<pid>-<random>", so a later run can tell whose they are.
const templatePrefix = "agentium-cli-fixtures-"

// removeStaleTemplates deletes template folders left by runs that no longer exist (killed, or exited through os.Exit).
func removeStaleTemplates() {
	stale, _ := filepath.Glob(filepath.Join(os.TempDir(), templatePrefix+"*"))
	for _, dir := range stale {
		pid, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(filepath.Base(dir), templatePrefix), "-", 2)[0])
		if err != nil || pid == os.Getpid() {
			continue
		}
		if proc, _ := os.FindProcess(pid); proc != nil && proc.Signal(syscall.Signal(0)) == nil {
			continue // its run is still going
		}
		os.RemoveAll(dir)
	}
}

// template is a fixture built once: its folders under templateRoot, or the failure that stopped it.
type template struct {
	once sync.Once
	dirs []string
	bad  bool
}

// templates is keyed by the fixture's name; the map is fixed at start-up, so only each entry's Once needs locking.
var templates = map[string]*template{"run": {}, "experiment": {}}

// build runs setup once, with the first caller's t, into a fresh folder per directory name, and returns the
// directories. Later callers fail at once if the first build failed (it reported why).
func (tp *template) build(t *testing.T, name string, names []string, setup func(dirs []string)) []string {
	t.Helper()
	tp.once.Do(func() {
		tp.bad = true // stays set if setup fails the test (runtime.Goexit) before finishing
		for _, n := range names {
			tp.dirs = append(tp.dirs, filepath.Join(templateRoot, name, n))
			if err := os.MkdirAll(tp.dirs[len(tp.dirs)-1], 0o700); err != nil {
				t.Fatal(err)
			}
		}
		setup(tp.dirs)
		tp.bad = false
	})
	if tp.bad {
		t.Fatalf("the %s fixture could not be built (see the first failing test)", name)
	}
	return tp.dirs
}

// cloneDirs copies each template directory to the matching destination and rewrites the template's absolute paths in
// the copies to the destinations'.
func cloneDirs(t *testing.T, from, to []string) {
	t.Helper()
	for i := range from {
		if err := copyTree(from[i], to[i]); err != nil {
			t.Fatal(err)
		}
	}
	// Resolved spellings before raw ones: on macOS /var/x is a substring of its resolved /private/var/x, so replacing the
	// raw one first would corrupt the resolved one.
	var old, repl []string
	for i := range from {
		old = append(old, resolved(from[i]), from[i])
		repl = append(repl, resolved(to[i]), to[i])
	}
	for _, dir := range to {
		if err := rewritePaths(dir, old, repl); err != nil {
			t.Fatal(err)
		}
		if err := findLeftovers(dir, old); err != nil {
			t.Fatal(err)
		}
	}
}

// sharedObject reports whether rel (slash-separated, relative to the cloned folder) is inside a git object store
// (".git/objects" or a bare "x.git/objects"). Those files are hard-linked, never rewritten, and hold no host paths.
// The decision is made on the relative path, so the folder the template lives in cannot change it.
func sharedObject(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "objects" && (parts[i-1] == ".git" || strings.HasSuffix(parts[i-1], ".git")) {
			return true
		}
	}
	return false
}

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// copyTree copies a directory tree, keeping modes and symlinks (git objects are hard-linked).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		// Git never rewrites an object or pack file in place (it adds files and, at most, deletes them), so the copies
		// may share them: a link is far cheaper than a copy, and the template stays intact.
		if sharedObject(rel) {
			if err := os.Link(p, target); err == nil {
				return nil
			}
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// rewritePaths replaces old path spellings with new ones in the text files under dir (git objects are shared and hold
// no host paths) and in every text value of SQLite databases. A changed file is written to a new file and renamed over
// the old one, so a hard-linked inode is never written in place.
func rewritePaths(dir string, old, repl []string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case sharedObject(rel), strings.HasSuffix(p, "-wal"), strings.HasSuffix(p, "-shm"):
			return nil
		case strings.HasSuffix(p, ".db"):
			return rewriteDB(p, old, repl)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		changed := body
		for i := range old {
			changed = bytes.ReplaceAll(changed, []byte(old[i]), []byte(repl[i]))
		}
		if bytes.Equal(changed, body) {
			return nil
		}
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		tmp := p + ".rewrite"
		if err := os.WriteFile(tmp, changed, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Rename(tmp, p)
	})
}

// findLeftovers fails when any template spelling is still in the copy: in a file, a symlink target or a database value.
// A future migration or fixture that stores a path somewhere the rewrite does not reach must fail here, not quietly
// point a test at the template's files.
func findLeftovers(dir string, old []string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		var body []byte
		switch {
		case sharedObject(rel), strings.HasSuffix(p, "-wal"), strings.HasSuffix(p, "-shm"):
			return nil
		case d.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			body = []byte(link)
		case strings.HasSuffix(p, ".db"):
			return dbLeftovers(p, old)
		default:
			if body, err = os.ReadFile(p); err != nil {
				return err
			}
		}
		for _, o := range old {
			if bytes.Contains(body, []byte(o)) {
				return fmt.Errorf("%s still holds the template path %s after cloning", p, o)
			}
		}
		return nil
	})
}

// dbColumns lists every table and column of a database.
func dbColumns(db *sql.DB) ([][2]string, error) {
	rows, err := db.Query(`SELECT m.name, c.name FROM sqlite_master m, pragma_table_info(m.name) c
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols [][2]string
	for rows.Next() {
		var c [2]string
		if err := rows.Scan(&c[0], &c[1]); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func dbLeftovers(path string, old []string) error {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return err
	}
	defer db.Close()
	cols, err := dbColumns(db)
	if err != nil {
		return err
	}
	for _, c := range cols {
		for _, o := range old {
			var n int
			q := fmt.Sprintf("SELECT count(*) FROM %q WHERE instr(CAST(%q AS TEXT), ?) > 0", c[0], c[1])
			if err := db.QueryRow(q, o).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("%s: %s.%s still holds the template path %s after cloning", path, c[0], c[1], o)
			}
		}
	}
	return nil
}

func rewriteDB(path string, old, repl []string) error {
	db, err := sql.Open("sqlite3", path) // the driver is registered by internal/store
	if err != nil {
		return err
	}
	defer db.Close()
	cols, err := dbColumns(db) // names come from the schema, not from input
	if err != nil {
		return err
	}
	for _, c := range cols {
		expr := fmt.Sprintf("%q", c[1])
		for i := range old {
			expr = fmt.Sprintf("replace(%s, '%s', '%s')", expr, old[i], repl[i]) // paths hold no quotes
		}
		stmt := fmt.Sprintf("UPDATE %q SET %q = %s WHERE typeof(%q) = 'text'", c[0], c[1], expr, c[1])
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}
