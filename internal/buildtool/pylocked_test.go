package buildtool

import "testing"

// A base is locked by uv.lock, or by requirement files whose every requirement names one version; ranges, a missing
// include, an include out of the repository, poetry.lock alone, or no requirement at all leave it unlocked.
func TestPythonLocked(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"uv.lock", map[string]string{"pyproject.toml": "", "uv.lock": ""}, true},
		{"nothing pinned", map[string]string{"pyproject.toml": ""}, false},
		{"poetry.lock alone (pip resolves it)", map[string]string{"pyproject.toml": "", "poetry.lock": ""}, false},
		{"pinned, with markers, extras, hashes and comments", map[string]string{"requirements.txt": "# compiled\nattrs==23.1.0 ; python_version >= \"3.8\"\n" +
			"requests[socks]==2.31.0 \\\n    --hash=sha256:aa \\\n    --hash=sha256:bb\n--index-url https://example.com/simple\n-e .\n"}, true},
		{"a range", map[string]string{"requirements.txt": "attrs==23.1.0\npytest>=8\n"}, false},
		{"unpinned name", map[string]string{"requirements-dev.txt": "pytest\n"}, false},
		{"an included file pins", map[string]string{"requirements.txt": "-r requirements/base.txt\nattrs==1.0\n", "requirements/base.txt": "six==1.16.0\n"}, true},
		{"an included file is missing", map[string]string{"requirements.txt": "-r requirements/base.txt\nattrs==1.0\n"}, false},
		{"an include out of the repository", map[string]string{"requirements.txt": "-c ../constraints.txt\nattrs==1.0\n"}, false},
		{"a test requirement file in requirements/", map[string]string{"requirements/test.txt": "pytest==8.0.0\n"}, true},
		{"only options", map[string]string{"requirements.txt": "--index-url https://example.com/simple\n"}, false},
		{"other .txt files are not requirements", map[string]string{"notes.txt": "pytest==8\n"}, false},
	} {
		var files []string
		for f := range c.files {
			files = append(files, f)
		}
		read := func(p string) ([]byte, bool) {
			data, ok := c.files[p]
			return []byte(data), ok
		}
		if got := PythonLocked(files, read); got != c.want {
			t.Errorf("%s: PythonLocked = %v, want %v", c.name, got, c.want)
		}
	}
}
