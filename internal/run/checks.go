package run

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
// they appear. A rename counts under both names, and a deleted file counts as changed. Headers are read as the judge
// reads them, whatever the diff settings were when the change was stored: "a/X b/X", no prefixes, other one-letter
// prefixes (i/, w/, c/, o/) and quoted names; a block whose header names no file takes its names from its ---/+++ lines.
// ok is false when the change cannot be read: a read error, or a block that gives no path at all (never "nothing
// changed"). The change is read line by line, so a large one costs no more than its longest header.
func ChangedPaths(r io.Reader) (paths []string, ok bool) {
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	ok = true
	var header string // the current block's "diff --git" line, less its prefix
	var from, to, minus, plus string
	open := false
	flush := func() {
		if !open {
			return
		}
		switch got := blockPaths(header, from, to, minus, plus); {
		case len(got) == 0:
			ok = false
		default:
			for _, p := range got {
				add(p)
			}
		}
		open, header, from, to, minus, plus = false, "", "", "", "", ""
	}
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if line != "" || err == nil {
			switch {
			case strings.HasPrefix(line, "diff --git "):
				flush()
				open, header = true, strings.TrimPrefix(line, "diff --git ")
			case !open:
			case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "copy from "):
				from = unquotePath(line[strings.Index(line, "from ")+len("from "):])
			case strings.HasPrefix(line, "rename to "), strings.HasPrefix(line, "copy to "):
				to = unquotePath(line[strings.Index(line, "to ")+len("to "):])
			case strings.HasPrefix(line, "--- ") && minus == "":
				minus = line[len("--- "):]
			case strings.HasPrefix(line, "+++ ") && plus == "":
				plus = line[len("+++ "):]
			case strings.HasPrefix(line, "@@"):
				// The hunks are not headers: a line inside them could look like one.
				flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				return nil, false
			}
			break
		}
	}
	flush()
	return paths, ok
}

// maxHeaderLine is how much of a line is kept: a header holds paths, so a longer line (a minified file's) is cut.
const maxHeaderLine = 64 << 10

// readLine reads one line without its newline (and carriage return), keeping at most maxHeaderLine bytes of it.
func readLine(br *bufio.Reader) (string, error) {
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(line) < maxHeaderLine {
			line = append(line, chunk...)
		}
		if err != nil || !isPrefix {
			return string(line), err
		}
	}
}

// blockPaths are the files one diff block touches: a rename's or copy's two names; else the header's, read as the judge
// reads it; else the names on its ---/+++ lines (not /dev/null). Empty when none can be read.
func blockPaths(header, from, to, minus, plus string) []string {
	if from != "" || to != "" {
		return nonEmpty(from, to)
	}
	if p, ok := headerPath(header); ok {
		return []string{p}
	}
	var out []string
	for _, l := range []string{minus, plus} {
		l, _, _ = strings.Cut(l, "\t") // a timestamp follows a tab
		if l = unquotePath(l); l != "" && l != "/dev/null" {
			if len(l) > 2 && l[1] == '/' { // a one-letter prefix, which a header that gave no path may have had
				l = l[2:]
			}
			if !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	return out
}

func nonEmpty(names ...string) []string {
	var out []string
	for _, n := range names {
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// headerPath reads the path a "diff --git" header names (less its "diff --git " prefix), as internal/judge's filePath
// does: both sides must name the same file, bare or after the same one-letter prefix, which also tells where an
// unquoted name with spaces splits. A rename in the header cannot be read here: ok is false.
func headerPath(h string) (string, bool) {
	same := func(src, dst string) (string, bool) {
		if src == dst {
			return dst, true
		}
		if len(src) > 2 && len(dst) > 2 && src[1] == '/' && dst[1] == '/' && src[2:] == dst[2:] {
			return dst[2:], true
		}
		return "", false
	}
	if strings.HasPrefix(h, `"`) {
		first, rest, ok := splitQuoted(h)
		if !ok || !strings.HasPrefix(rest, " ") {
			return "", false
		}
		return same(first, unquotePath(rest[1:]))
	}
	for i := 0; i < len(h); i++ {
		if h[i] == ' ' {
			if p, ok := same(h[:i], h[i+1:]); ok {
				return p, true
			}
		}
	}
	return "", false
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

// commands reads the shell commands the run's agent ran from its records folder, with the run's own adapter, which
// knows its transcript's files. A folder or transcript that is missing or cannot be parsed, or an agent this Agentium
// does not know, leaves the run unread.
func (r CheckReader) commands(rec Record) ([]string, bool) {
	for _, dir := range r.dirs(rec) {
		m, err := parseRecords(adapterFor(rec.Agent), dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue // no transcript here: the record's own folder may hold it
		}
		if err != nil {
			return nil, false
		}
		return m.RanCommands, true
	}
	return nil, false
}

// paths reads the paths the run's stored change touches.
func (r CheckReader) paths(rec Record) ([]string, bool) {
	file, ok := r.find(rec, "agent.diff")
	if !ok {
		return nil, false
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	return ChangedPaths(f)
}

// dirs are the run's records folders to look in: the data folder's own first, then the record's (which may name a
// data folder that has since moved).
func (r CheckReader) dirs(rec Record) []string {
	var dirs []string
	if r.Records != "" && rec.ID != "" {
		dirs = append(dirs, filepath.Join(r.Records, rec.ID))
	}
	if rec.RecordsDir != "" {
		dirs = append(dirs, rec.RecordsDir)
	}
	return dirs
}

// find is the run's file name in its records folder.
func (r CheckReader) find(rec Record, name string) (string, bool) {
	for _, dir := range r.dirs(rec) {
		file := filepath.Join(dir, name)
		if _, err := os.Stat(file); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return file, true // one that exists but cannot be read is found, then unread
		}
	}
	return "", false
}
