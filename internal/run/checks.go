package run

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/claudectx"
)

// The kinds of a rule check, as the project keeps them (store.RuleCheck.Kind).
const (
	CheckRan        = "ran"         // a shell command the agent ran contains the pattern (a plain substring, case kept)
	CheckChanged    = "changed"     // the agent's change touches a path that matches the glob
	CheckNotChanged = "not-changed" // the agent's change touches no path that matches the glob
)

// Check is one rule of the project's owner, to be counted over stored runs.
type Check struct {
	Name, Kind, Pattern string
}

// CheckResult is what a check says of one run. A run whose transcript (for CheckRan) or change (for the others) is
// missing or cannot be read is CheckUnread: neither met nor unmet.
type CheckResult int

const (
	CheckUnread CheckResult = iota
	CheckMet
	CheckUnmet
)

// CheckCommands reports whether a shell command among commands (agent.Metrics.RanCommands: the denied ones are not
// there) contains text.
func CheckCommands(commands []string, text string) bool {
	for _, c := range commands {
		if strings.Contains(c, text) {
			return true
		}
	}
	return false
}

// CheckPaths reports whether the change touching paths meets a changed or not-changed check with glob. An empty
// change touches nothing: changed is unmet and not-changed is met.
func CheckPaths(paths []string, kind, glob string) bool {
	touched := false
	for _, p := range paths {
		if claudectx.GlobMatch(glob, p) {
			touched = true
			break
		}
	}
	return touched == (kind == CheckChanged)
}

// ChangedPaths lists the paths a stored change (agent.diff) touches, from its file headers, each once in the order
// they appear. A rename counts under both names, and a deleted file counts as changed.
func ChangedPaths(diff []byte) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	var header string // the current block's "diff --git" line, less its prefix
	var from, to string
	flush := func() {
		if from != "" || to != "" { // a rename or copy names its files itself
			add(from)
			add(to)
		} else if header != "" {
			for _, p := range headerPaths(header) {
				add(p)
			}
		}
		header, from, to = "", "", ""
	}
	for _, line := range strings.Split(string(diff), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			header = strings.TrimSuffix(strings.TrimPrefix(line, "diff --git "), "\r")
		case header == "":
		case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "copy from "):
			from = unquotePath(line[strings.Index(line, "from ")+len("from "):])
		case strings.HasPrefix(line, "rename to "), strings.HasPrefix(line, "copy to "):
			to = unquotePath(line[strings.Index(line, "to ")+len("to "):])
		case strings.HasPrefix(line, "@@"):
			// The hunks are not headers: a line inside them could look like one.
			flush()
		}
	}
	flush()
	return out
}

// headerPaths are the paths of a "diff --git a/OLD b/NEW" line: both when they differ, one when they are the same.
func headerPaths(h string) []string {
	var a, b string
	if strings.HasPrefix(h, `"`) {
		first, rest, ok := splitQuoted(h)
		if !ok {
			return nil
		}
		a, b = first, unquotePath(strings.TrimSpace(rest))
	} else if n := (len(h) - 1) / 2; len(h)%2 == 1 && h[n] == ' ' && h[:n] != "" && strings.HasPrefix(h[:n], "a/") && h[n+1:] == "b/"+h[:n][2:] {
		return []string{h[n+3:]} // the common case, and exact for names with spaces: both sides are the same name
	} else if i := strings.Index(h, " b/"); i > 0 {
		a, b = h[:i], h[i+1:]
	} else if strings.HasPrefix(h, "a/") && strings.Contains(h, ` "b/`) {
		i := strings.Index(h, ` "b/`)
		a, b = h[:i], unquotePath(h[i+1:])
	} else {
		return nil
	}
	a, b = strings.TrimPrefix(a, "a/"), strings.TrimPrefix(b, "b/")
	if a == b {
		return []string{a}
	}
	return []string{a, b}
}

// splitQuoted splits a leading C-style quoted name off s.
func splitQuoted(s string) (name, rest string, ok bool) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return unquotePath(s[:i+1]), s[i+1:], true
		}
	}
	return "", "", false
}

// unquotePath reads a path as git writes it: bare, or quoted with C escapes (octal bytes for non-ASCII names).
func unquotePath(s string) string {
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

// CheckReader reads stored runs again to count rule checks. Records is the data folder's records folder, where each
// run's files are looked for first (a record's own folder may name a data folder that has since moved).
type CheckReader struct {
	Records string
}

// Results evaluates checks on one stored run, in order. It reads the transcript only when a check is of kind ran and
// the change only for the other kinds, so no checks read nothing.
func (r CheckReader) Results(rec Record, checks []Check) []CheckResult {
	out := make([]CheckResult, len(checks))
	var commands, paths []string
	var haveCommands, havePaths, triedCommands, triedPaths bool
	for i, c := range checks {
		if c.Kind == CheckRan {
			if !triedCommands {
				triedCommands = true
				commands, haveCommands = r.commands(rec)
			}
			if haveCommands {
				out[i] = unmet(CheckCommands(commands, c.Pattern))
			}
			continue
		}
		if !triedPaths {
			triedPaths = true
			paths, havePaths = r.paths(rec)
		}
		if havePaths {
			out[i] = unmet(CheckPaths(paths, c.Kind, c.Pattern))
		}
	}
	return out
}

func unmet(met bool) CheckResult {
	if met {
		return CheckMet
	}
	return CheckUnmet
}

// commands reads the shell commands the run's agent ran from its stored transcript, with the run's own adapter.
func (r CheckReader) commands(rec Record) ([]string, bool) {
	file, ok := r.find(rec, "stream.jsonl")
	if !ok {
		return nil, false
	}
	m, err := parseFile(adapterFor(rec.Agent), file)
	if err != nil {
		return nil, false
	}
	return m.RanCommands, true
}

// paths reads the paths the run's stored change touches.
func (r CheckReader) paths(rec Record) ([]string, bool) {
	file, ok := r.find(rec, "agent.diff")
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	return ChangedPaths(data), true
}

// find is the run's file name in its records folder.
func (r CheckReader) find(rec Record, name string) (string, bool) {
	var dirs []string
	if r.Records != "" && rec.ID != "" {
		dirs = append(dirs, filepath.Join(r.Records, rec.ID))
	}
	if rec.RecordsDir != "" {
		dirs = append(dirs, rec.RecordsDir)
	}
	for _, dir := range dirs {
		file := filepath.Join(dir, name)
		if _, err := os.Stat(file); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return file, true // one that exists but cannot be read is found, then unread
		}
	}
	return "", false
}
