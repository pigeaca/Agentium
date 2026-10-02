package buildtool

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// PythonLocked reports whether a Python project's commit pins its dependencies, so that a warm-up installs the versions
// the commit was tested with rather than today's: uv.lock at its root, or requirement files (requirements.txt and the
// test requirement files the warm-up reads, with the files they include) in which every requirement is pinned to one
// version (pip-tools' output, say). files are the commit's paths at its root and in requirements/ (slash-separated);
// read returns a file's content, ok false when it is missing or unreadable. A poetry.lock does not count: pip, not
// poetry, resolves such a project (see pythonProfile).
//
// It reads the files only: unlike the warm-up's check of its resolve (pinnedByFiles), it does not see whether the
// project's own dependencies in pyproject.toml are among the pins.
func PythonLocked(files []string, read func(path string) ([]byte, bool)) bool {
	if slices.Contains(files, "uv.lock") {
		return true
	}
	var queue []string
	for _, f := range files {
		if f == "requirements.txt" || testRequirementFile.MatchString(f) {
			queue = append(queue, f)
		}
	}
	if len(queue) == 0 {
		return false
	}
	seen, pins := map[string]bool{}, 0
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		if seen[f] {
			continue
		}
		if seen[f] = true; len(seen) > 64 {
			return false
		}
		data, ok := read(f)
		if !ok {
			return false
		}
		for _, line := range requirementLines(string(data)) {
			if m := requirementInclude.FindStringSubmatch(line); m != nil {
				next := path.Join(path.Dir(f), m[1])
				if strings.Contains(m[1], "://") || path.IsAbs(m[1]) || next == ".." || strings.HasPrefix(next, "../") {
					return false // outside the repository: what it pins is unknown
				}
				queue = append(queue, next)
				continue
			}
			if strings.HasPrefix(line, "-") {
				continue // other options (an index, an editable local path) choose no version
			}
			if !pinLine.MatchString(hashOption.ReplaceAllString(line, "")) {
				return false
			}
			pins++
		}
	}
	return pins > 0
}

// hashOption is a --hash option on a requirement line (pip-tools' --generate-hashes).
var hashOption = regexp.MustCompile(`\s+--hash[=\s]\S+`)

// requirementComment is a comment in a requirement file: a "#" at a line's start or after whitespace.
var requirementComment = regexp.MustCompile(`(^|\s)#.*$`)

// requirementLines are a requirement file's logical lines: continuations ("\" at a line's end) joined, comments and
// blank lines dropped.
func requirementLines(data string) []string {
	var out []string
	var current strings.Builder
	for _, raw := range strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n") {
		raw = requirementComment.ReplaceAllString(raw, "")
		if trimmed := strings.TrimRight(raw, " \t"); strings.HasSuffix(trimmed, "\\") {
			current.WriteString(strings.TrimSuffix(trimmed, "\\") + " ")
			continue
		}
		current.WriteString(raw)
		if line := strings.TrimSpace(current.String()); line != "" {
			out = append(out, line)
		}
		current.Reset()
	}
	if line := strings.TrimSpace(current.String()); line != "" {
		out = append(out, line)
	}
	return out
}
