package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Fixtures are built once per test run and copied into each test, because most of a test's wall time used to go to
// git and `agentium` commands that rebuild the same repository, data folder and snapshots. A copy is private to its
// test (own repository, data folder and home), so parallel tests share nothing mutable. The copies carry the template's
// commit hashes and record IDs; the absolute paths stored in them (the project root, log and record locations) are
// rewritten to the copy's own.

var templateRoot string // removed after the run; set by TestMain

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentium-cli-fixtures-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	templateRoot = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
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
	// Longest and resolved spellings first: on macOS /var/x is a substring of its resolved /private/var/x.
	var old, repl []string
	for i := range from {
		old = append(old, resolved(from[i]), from[i])
		repl = append(repl, resolved(to[i]), to[i])
	}
	for _, dir := range to {
		if err := rewritePaths(dir, old, repl); err != nil {
			t.Fatal(err)
		}
	}
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
		if strings.Contains(filepath.ToSlash(p), "/objects/") {
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

// rewritePaths replaces old path spellings with new ones in the text files under dir (git objects are compressed and
// hold no host paths) and in every text column of SQLite databases.
func rewritePaths(dir string, old, repl []string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		switch {
		case strings.HasSuffix(p, ".db"):
			return rewriteDB(p, old, repl)
		case strings.HasSuffix(p, "-wal"), strings.HasSuffix(p, "-shm"), strings.Contains(filepath.ToSlash(p), ".git/objects/"):
			return nil
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
		return os.WriteFile(p, changed, 0o644)
	})
}

func rewriteDB(path string, old, repl []string) error {
	db, err := sql.Open("sqlite3", path) // the driver is registered by internal/store
	if err != nil {
		return err
	}
	defer db.Close()
	// One statement per table, built from its text columns (names come from the schema, not from input).
	rows, err := db.Query(`SELECT m.name, c.name FROM sqlite_master m, pragma_table_info(m.name) c
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND upper(c.type) = 'TEXT'`)
	if err != nil {
		return err
	}
	var stmts []string
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			rows.Close()
			return err
		}
		expr := fmt.Sprintf("%q", col)
		for i := range old {
			expr = fmt.Sprintf("replace(%s, '%s', '%s')", expr, old[i], repl[i]) // paths hold no quotes
		}
		stmts = append(stmts, fmt.Sprintf("UPDATE %q SET %q = %s", table, col, expr))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}
