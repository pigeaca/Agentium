package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// SessionFolderUnder reads back what SessionFolder makes for a folder below root, resolved the same way, and nothing
// for another root: one that only shares a prefix, or has the same letters with other separators.
func TestSessionFolderUnderAgreesWithSessionFolder(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir()) // the roots that do not exist are not resolved
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "data.x", "workspaces")
	if err := os.MkdirAll(filepath.Join(root, "w_1", "repo", "svc.api"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(filepath.Join(dir, "data.x"), link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ dir, rest string }{
		{filepath.Join(root, "w_1", "repo"), "w-1-repo"},
		{filepath.Join(root, "w_1", "repo", "svc.api"), "w-1-repo-svc-api"},
		{filepath.Join(link, "workspaces", "w_1", "repo"), "w-1-repo"}, // resolved, as SessionFolder resolves it
	} {
		name := filepath.Base(SessionFolder("/cfg", c.dir))
		for _, r := range []string{root, filepath.Join(link, "workspaces")} {
			if rest, ok := SessionFolderUnder(r, name); !ok || rest != c.rest {
				t.Errorf("SessionFolderUnder(%s, %s) = %q, %v; want %q", r, name, rest, ok, c.rest)
			}
		}
	}
	name := filepath.Base(SessionFolder("/cfg", filepath.Join(root, "w_1", "repo")))
	for _, other := range []string{
		filepath.Join(dir, "data.x", "work"),        // a prefix that ends inside a part
		filepath.Join(dir, "datax", "workspaces"),   // the same letters, a separator fewer
		filepath.Join(dir, "data..x", "workspaces"), // the same letters, a separator more
	} {
		if rest, ok := SessionFolderUnder(other, name); ok {
			t.Errorf("SessionFolderUnder(%s, %s) = %q: matched another root", other, name, rest)
		}
	}
	// A name never leads back to one path: a root whose encoding is a prefix of the name's, up to a separator, matches,
	// and so does one that differs only in its separators. Callers check what follows (the run package's forms).
	for other, want := range map[string]string{
		filepath.Join(dir, "data"):                      "x-workspaces-w-1-repo",
		filepath.Join(dir, "data_x", "workspaces"):      "w-1-repo",
		filepath.Join(dir, "data.x", "workspaces", "w"): "1-repo",
	} {
		if rest, ok := SessionFolderUnder(other, name); !ok || rest != want {
			t.Errorf("SessionFolderUnder(%s, %s) = %q, %v; want %q", other, name, rest, ok, want)
		}
	}
	// The whole root alone, or a name with characters SessionFolder never leaves, is no session below it.
	for _, bad := range []string{filepath.Base(SessionFolder("/cfg", root)), filepath.Base(SessionFolder("/cfg", root)) + "-", filepath.Base(SessionFolder("/cfg", root)) + "-w.1"} {
		if rest, ok := SessionFolderUnder(root, bad); ok {
			t.Errorf("SessionFolderUnder(root, %s) = %q: want no match", bad, rest)
		}
	}
}
