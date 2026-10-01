// Package mine finds commits in a repository's history that would make good tasks: a non-merge commit that changes
// both tests and code, small enough to review, with a message that can serve as the instruction. Scan reads only the
// commit log with per-file line counts (never a whole diff); file contents are read for the few commits that pass
// every cheap filter (generated code, inline Rust tests). Every candidate's score is a sum of named parts, and every
// rejected commit keeps its reason, so a caller can explain why a history yields few tasks.
package mine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/task"
)

// Defaults for Options fields left at zero.
const (
	DefaultMaxCommits = 2000
	DefaultMaxFiles   = 15
	DefaultMaxLines   = 600
)

// Options bound a scan. Zero values take the defaults.
type Options struct {
	// Ref is the branch or commit whose history is read; empty means the default branch (the local branch origin/HEAD
	// names, else main, else master, else HEAD).
	Ref string
	// Since skips commits committed before it; zero reads the whole history (up to MaxCommits).
	Since time.Time
	// MaxCommits bounds how many commits (merges included) are read, newest first.
	MaxCommits int
	// MaxFiles and MaxLines bound a candidate's changed test and code files and their added plus deleted lines.
	// Documentation is not counted.
	MaxFiles, MaxLines int
	// Exclude holds the full hashes of commits that are already tasks.
	Exclude map[string]bool
	// Now is the time recency is measured from; zero means time.Now().
	Now time.Time
}

// Reason says why a commit is not a candidate.
type Reason string

// Rejection reasons, in the order Scan checks them.
const (
	ReasonMerge        Reason = "merge commit"
	ReasonRoot         Reason = "no parent commit"
	ReasonImported     Reason = "already a task"
	ReasonRevert       Reason = "revert"
	ReasonEmpty        Reason = "no file changes"
	ReasonDocsOnly     Reason = "documentation only"
	ReasonNoTests      Reason = "no test changes"
	ReasonTestsOnly    Reason = "tests only"
	ReasonDependencies Reason = "dependency update"
	ReasonFormatting   Reason = "formatting sweep"
	ReasonTooLarge     Reason = "too large"
	ReasonGenerated    Reason = "generated code"
	ReasonInlineRust   Reason = "inline Rust tests"
)

// Part is one named contribution to a candidate's score.
type Part struct {
	Points int
	Text   string
}

func (p Part) String() string { return fmt.Sprintf("%+d %s", p.Points, p.Text) }

// Candidate is a commit that could become a task: its parent is the base, its message the instruction, its test
// files the hidden tests and its code files the reference.
type Candidate struct {
	Hash, Parent     string
	Subject, Body    string // Body excludes trailers such as Co-Authored-By
	Date             time.Time
	Tests, Code      []string
	Docs             []string // documentation the commit also changes (not counted in the limits)
	TestLines, Lines int      // added plus deleted lines: in test files, and in test and code files together
	Score            int
	Reasons          []Part
}

// Rejection is a commit that is not a candidate, and why.
type Rejection struct {
	Hash, Subject string
	Date          time.Time
	Reason        Reason
	Detail        string
}

// Result is a scan's outcome.
type Result struct {
	Ref        string // what was read, as resolved
	Scanned    int    // commits read, merges included
	Candidates []Candidate
	Rejected   []Rejection
}

// Counts tallies the rejections per reason.
func (r Result) Counts() map[Reason]int {
	counts := map[Reason]int{}
	for _, rej := range r.Rejected {
		counts[rej.Reason]++
	}
	return counts
}

// commit is one log record with its per-file line counts.
type commit struct {
	hash    string
	parents []string
	date    time.Time
	subject string
	body    string
	files   []fileStat
}

type fileStat struct {
	path  string
	lines int // added plus deleted; 0 for binary files
}

// Scan reads the history of opts.Ref in the git repository at repoRoot and returns its candidates, best first (score,
// then the newest), and every other commit it read with the reason it was set aside.
func Scan(ctx context.Context, repoRoot string, opts Options) (Result, error) {
	opts = withDefaults(opts)
	ref, err := resolveRef(ctx, repoRoot, opts.Ref)
	if err != nil {
		return Result{}, err
	}
	commits, err := readLog(ctx, repoRoot, ref, opts)
	if err != nil {
		return Result{}, err
	}
	res := Result{Ref: ref, Scanned: len(commits)}
	for _, c := range commits {
		cand, rej := classify(c, opts)
		if rej == nil {
			rej, err = inspect(ctx, repoRoot, &cand)
			if err != nil {
				return Result{}, err
			}
		}
		if rej != nil {
			rej.Hash, rej.Subject, rej.Date = c.hash, c.subject, c.date
			res.Rejected = append(res.Rejected, *rej)
			continue
		}
		cand.Score, cand.Reasons = score(cand, opts)
		res.Candidates = append(res.Candidates, cand)
	}
	slices.SortStableFunc(res.Candidates, func(a, b Candidate) int {
		if a.Score != b.Score {
			return b.Score - a.Score
		}
		if c := b.Date.Compare(a.Date); c != 0 {
			return c
		}
		return strings.Compare(a.Hash, b.Hash)
	})
	return res, nil
}

func withDefaults(o Options) Options {
	if o.MaxCommits <= 0 {
		o.MaxCommits = DefaultMaxCommits
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = DefaultMaxFiles
	}
	if o.MaxLines <= 0 {
		o.MaxLines = DefaultMaxLines
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	return o
}

// resolveRef returns ref, or the default branch when ref is empty.
func resolveRef(ctx context.Context, root, ref string) (string, error) {
	exists := func(r string) bool {
		out, err := gitx.Run(ctx, "-C", root, "rev-parse", "--verify", "--quiet", "--end-of-options", r+"^{commit}")
		return err == nil && out != ""
	}
	if ref != "" {
		if !exists(ref) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return "", fmt.Errorf("mine: %q is not a commit in %s", ref, root)
		}
		return ref, nil
	}
	var tries []string
	if head, err := gitx.Run(ctx, "-C", root, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil && head != "" {
		branch := strings.TrimPrefix(head, "refs/remotes/origin/")
		tries = append(tries, "refs/heads/"+branch, head) // the local branch can be ahead of the remote one
	}
	tries = append(tries, "refs/heads/main", "refs/heads/master", "HEAD")
	for _, r := range tries {
		if exists(r) {
			return strings.TrimPrefix(r, "refs/heads/"), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("mine: %s has no commits", root)
}

// Separators of the log records: a record starts with RS, and its header fields end with US.
const (
	recordSep = "\x1e"
	fieldSep  = "\x1f"
)

// readLog lists ref's history, newest first, with per-file line counts. Options that a user's configuration could
// change (renames, relative paths, signatures, colour, text conversion) are fixed on the command line.
func readLog(ctx context.Context, root, ref string, o Options) ([]commit, error) {
	args := []string{"-C", root, "log", "--numstat", "-z", "--no-renames", "--no-relative", "--no-color",
		"--no-show-signature", "--no-textconv", "--no-ext-diff",
		"--format=" + recordSep + "%H" + fieldSep + "%P" + fieldSep + "%ct" + fieldSep + "%s" + fieldSep + "%B" + fieldSep,
		"--max-count=" + strconv.Itoa(o.MaxCommits)}
	if !o.Since.IsZero() {
		args = append(args, "--since="+o.Since.UTC().Format(time.RFC3339))
	}
	args = append(args, "--end-of-options", ref, "--")
	out, err := gitx.Output(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("mine: read history: %w", err)
	}
	return parseLog(out)
}

// parseLog parses readLog's output: per commit, RS, the header fields each ended by US, then NUL-terminated numstat
// entries ("added\tdeleted\tpath"; "-\t-\tpath" for binary files).
func parseLog(out []byte) ([]commit, error) {
	var commits []commit
	for _, rec := range strings.Split(string(out), recordSep) {
		if strings.TrimSpace(strings.Trim(rec, "\x00")) == "" {
			continue
		}
		fields := strings.SplitN(rec, fieldSep, 6)
		if len(fields) != 6 {
			return nil, fmt.Errorf("mine: unexpected log record %.60q", rec)
		}
		secs, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("mine: commit %s date: %w", fields[0], err)
		}
		c := commit{hash: fields[0], parents: strings.Fields(fields[1]), date: time.Unix(secs, 0).UTC(), subject: fields[3]}
		_, body, _ := strings.Cut(fields[4], "\n")
		c.body = stripTrailers(body)
		for _, entry := range strings.Split(fields[5], "\x00") {
			entry = strings.TrimLeft(entry, "\n")
			if entry == "" {
				continue
			}
			parts := strings.SplitN(entry, "\t", 3)
			if len(parts) != 3 {
				return nil, fmt.Errorf("mine: commit %s: unexpected numstat entry %q", c.hash, entry)
			}
			added, _ := strconv.Atoi(parts[0]) // "-" for binary files counts as 0
			deleted, _ := strconv.Atoi(parts[1])
			c.files = append(c.files, fileStat{path: parts[2], lines: added + deleted})
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// trailer matches a Git trailer line ("Co-Authored-By: ...", "Signed-off-by: ...").
var trailer = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*: `)

// stripTrailers drops the body's final paragraph when every line in it is a trailer, and trims the rest.
func stripTrailers(body string) string {
	body = strings.TrimSpace(body)
	paras := strings.Split(body, "\n\n")
	last := paras[len(paras)-1]
	for _, line := range strings.Split(last, "\n") {
		if !trailer.MatchString(line) {
			return body
		}
	}
	return strings.TrimSpace(strings.Join(paras[:len(paras)-1], "\n\n"))
}

var (
	revertMessage = regexp.MustCompile(`(?i)^revert\b|this reverts commit [0-9a-f]{7,}`)
	depsMessage   = regexp.MustCompile(`(?i)\bbump(s|ed)?\b|\bdeps\b|\b(update|upgrade)s? (the )?dependenc|\bdependabot\b|\brenovate\b`)
	formatMessage = regexp.MustCompile(`(?i)\b(gofmt|goimports|rustfmt|prettier|reformat(s|ted)?|whitespace)\b`)
	issueRef      = regexp.MustCompile(`(^|[\s(])#[0-9]+\b|/(issues|pull)/[0-9]+|(?i:\b(fix(es|ed)?|close[sd]?|resolve[sd]?|refs?)\s+)[A-Z][A-Z0-9]+-[0-9]+\b`)
	wipMessage    = regexp.MustCompile(`(?i)\bwip\b|\btypos?\b`)
)

// dependencyFiles are manifests and lockfiles: a commit whose code changes are only these updates dependencies.
var dependencyFiles = map[string]bool{
	"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true,
	"package.json": true, "package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "bun.lockb": true,
	"Cargo.toml": true, "Cargo.lock": true,
	"Gemfile": true, "Gemfile.lock": true,
	"pyproject.toml": true, "poetry.lock": true, "Pipfile": true, "Pipfile.lock": true, "uv.lock": true, "requirements.txt": true,
	"composer.json": true, "composer.lock": true,
	"pom.xml": true, "build.gradle": true, "build.gradle.kts": true, "gradle.lockfile": true, "libs.versions.toml": true,
}

// isDependencyFile reports whether p is a manifest, a lockfile or vendored code.
func isDependencyFile(p string) bool {
	for _, dir := range strings.Split(path.Dir(p), "/") {
		if dir == "vendor" || dir == "node_modules" {
			return true
		}
	}
	base := path.Base(p)
	return dependencyFiles[base] || strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")
}

// Formatting sweeps: at least sweepFiles code files with at most sweepLines changed lines each on average.
const (
	sweepFiles = 5
	sweepLines = 4
)

// classify applies the checks that need only the log: on success the candidate has its files and sizes but no score.
func classify(c commit, o Options) (Candidate, *Rejection) {
	reject := func(r Reason, format string, args ...any) (Candidate, *Rejection) {
		return Candidate{}, &Rejection{Reason: r, Detail: fmt.Sprintf(format, args...)}
	}
	switch {
	case len(c.parents) > 1:
		return reject(ReasonMerge, "%d parents", len(c.parents))
	case len(c.parents) == 0:
		return reject(ReasonRoot, "the first commit has no base to start from")
	case o.Exclude[c.hash]:
		return reject(ReasonImported, "")
	case revertMessage.MatchString(c.subject) || revertMessage.MatchString(c.body):
		return reject(ReasonRevert, "")
	}
	cand := Candidate{Hash: c.hash, Parent: c.parents[0], Subject: c.subject, Body: c.body, Date: c.date}
	codeLines := 0
	for _, f := range c.files {
		switch {
		case task.IsTestFile(f.path):
			cand.Tests = append(cand.Tests, f.path)
			cand.TestLines += f.lines
		case claudectx.IsDocument(f.path):
			cand.Docs = append(cand.Docs, f.path)
			continue
		default:
			cand.Code = append(cand.Code, f.path)
			codeLines += f.lines
		}
		cand.Lines += f.lines
	}
	switch {
	case len(c.files) == 0:
		return reject(ReasonEmpty, "")
	case len(cand.Tests) == 0 && len(cand.Code) == 0:
		return reject(ReasonDocsOnly, "%d documents", len(cand.Docs))
	case len(cand.Tests) == 0:
		return reject(ReasonNoTests, "%d code files, no test files", len(cand.Code))
	case len(cand.Code) == 0:
		return reject(ReasonTestsOnly, "%d test files, nothing to implement", len(cand.Tests))
	}
	if !slices.ContainsFunc(cand.Code, func(p string) bool { return !isDependencyFile(p) }) {
		return reject(ReasonDependencies, "only manifests or lockfiles change: %s", strings.Join(cand.Code, ", "))
	}
	if depsMessage.MatchString(c.subject) {
		return reject(ReasonDependencies, "the subject reads as a dependency update")
	}
	if formatMessage.MatchString(c.subject) {
		return reject(ReasonFormatting, "the subject reads as a formatting change")
	}
	if len(cand.Code) >= sweepFiles && codeLines <= sweepLines*len(cand.Code) {
		return reject(ReasonFormatting, "%d code files with %d changed lines in all", len(cand.Code), codeLines)
	}
	if files := len(cand.Tests) + len(cand.Code); files > o.MaxFiles {
		return reject(ReasonTooLarge, "%d files (limit %d)", files, o.MaxFiles)
	}
	if cand.Lines > o.MaxLines {
		return reject(ReasonTooLarge, "%d changed lines (limit %d)", cand.Lines, o.MaxLines)
	}
	return cand, nil
}

// inspect applies the checks that read file contents, for a commit that passed classify.
func inspect(ctx context.Context, root string, c *Candidate) (*Rejection, error) {
	generated, err := generatedFiles(ctx, root, c.Hash, c.Code)
	if err != nil {
		return nil, err
	}
	if len(generated) > 0 {
		return &Rejection{Reason: ReasonGenerated, Detail: strings.Join(generated, ", ")}, nil
	}
	if !slices.ContainsFunc(c.Code, func(p string) bool { return path.Ext(p) == ".rs" }) {
		return nil, nil
	}
	inline, err := task.InlineRustTests(ctx, c.Parent, c.Hash, c.Code, "-C", root)
	if err != nil {
		return nil, fmt.Errorf("mine: commit %s: %w", c.Hash, err)
	}
	if len(inline) > 0 {
		return &Rejection{Reason: ReasonInlineRust, Detail: strings.Join(inline, ", ")}, nil
	}
	return nil, nil
}

// generatedHeader is the standard marker of generated files (https://go.dev/s/generatedcode), in any line comment
// style. It is only looked for in a file's first headerLines lines.
var generatedHeader = regexp.MustCompile(`^(//|#|--|;)\s*Code generated .* DO NOT EDIT\.?\s*$`)

const headerLines = 10

// generatedFiles lists the files among paths that are generated in commit, reading them in one cat-file batch. Files
// the commit deletes are skipped.
func generatedFiles(ctx context.Context, root, commit string, paths []string) ([]string, error) {
	var stdin bytes.Buffer
	for _, p := range paths {
		fmt.Fprintf(&stdin, "%s:%s\n", commit, p)
	}
	out, err := gitx.Output(ctx, &stdin, "-C", root, "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("mine: read files of %s: %w", commit, err)
	}
	var generated []string
	r := bufio.NewReader(bytes.NewReader(out))
	for _, p := range paths {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("mine: read files of %s: %w", commit, err)
		}
		fields := strings.Fields(header) // "<oid> blob <size>" or "<name> missing"
		if len(fields) != 3 {
			continue
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("mine: read files of %s: header %q", commit, header)
		}
		content := make([]byte, size+1) // the content and its newline
		if _, err := io.ReadFull(r, content); err != nil {
			return nil, fmt.Errorf("mine: read files of %s: %w", commit, err)
		}
		if fields[1] == "blob" && isGenerated(content[:size]) {
			generated = append(generated, p)
		}
	}
	return generated, nil
}

func isGenerated(content []byte) bool {
	for i, line := range strings.SplitN(string(content), "\n", headerLines+1) {
		if i < headerLines && generatedHeader.MatchString(strings.TrimRight(line, "\r")) {
			return true
		}
	}
	return false
}

// score sums a candidate's named parts.
func score(c Candidate, o Options) (int, []Part) {
	var parts []Part
	add := func(points int, format string, args ...any) {
		parts = append(parts, Part{Points: points, Text: fmt.Sprintf(format, args...)})
	}

	// The message becomes the instruction.
	switch {
	case len(c.Body) >= 30:
		add(3, "the message has a subject and a body")
	case len(c.Subject) < 15 || len(strings.Fields(c.Subject)) < 3:
		add(-3, "a very short message")
	default:
		add(0, "the message is a subject only")
	}
	if issueRef.MatchString(c.Subject + "\n" + c.Body) {
		add(2, "references an issue")
	}
	if wipMessage.MatchString(c.Subject) {
		add(-4, "work in progress or a typo fix")
	}

	switch {
	case c.Lines < 10:
		add(0, "a tiny change (%d lines)", c.Lines)
	case c.Lines < 20:
		add(1, "a small change (%d lines)", c.Lines)
	case c.Lines <= 300:
		add(3, "a moderate size (%d lines)", c.Lines)
	default:
		add(1, "a large change (%d lines)", c.Lines)
	}

	codeLines := c.Lines - c.TestLines
	switch {
	case codeLines == 0:
		add(0, "no changed code lines (binary files?)")
	case c.TestLines*4 < codeLines:
		add(0, "few test lines (%d for %d code lines)", c.TestLines, codeLines)
	case c.TestLines > 4*codeLines:
		add(0, "mostly tests (%d test lines for %d code lines)", c.TestLines, codeLines)
	default:
		add(2, "tests in proportion to code (%d test lines, %d code lines)", c.TestLines, codeLines)
	}

	switch age := o.Now.Sub(c.Date); {
	case age < 90*24*time.Hour:
		add(2, "recent (%d days)", max(0, int(age.Hours()/24)))
	case age < 365*24*time.Hour:
		add(1, "this year (%d days)", int(age.Hours()/24))
	default:
		add(0, "older than a year")
	}

	dirs := map[string]bool{}
	for _, p := range append(slices.Clone(c.Tests), c.Code...) {
		dirs[path.Dir(p)] = true
	}
	if len(dirs) > 4 {
		add(-2, "touches %d directories", len(dirs))
	}

	total := 0
	for _, p := range parts {
		total += p.Points
	}
	return total, parts
}
