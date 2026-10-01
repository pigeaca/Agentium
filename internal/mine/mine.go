// Package mine finds commits in a repository's history that would make good tasks: a non-merge commit that changes
// both tests and source code, small enough to review, with a message that can serve as the instruction. Scan reads only
// the commit log with per-file line counts (never a whole diff); file contents are read for the few commits that pass
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
	// Ref is the branch or commit whose history is read; empty means the default branch (see defaultBranch). A
	// branch name is preferred over a tag of the same name.
	Ref string
	// Since skips commits committed before it; zero reads the whole history (up to MaxCommits).
	Since time.Time
	// MaxCommits bounds how many commits (merges included) are read, newest first.
	MaxCommits int
	// MaxFiles and MaxLines bound a candidate's changed test and code files and their added plus deleted lines.
	// Documentation and dependency files are not counted.
	MaxFiles, MaxLines int
	// Exclude holds the full hashes of commits that are already tasks.
	Exclude map[string]bool
	// Languages names the languages the project's verify commands test, as the languages table names them ("go",
	// "python", "rust", "java", "kotlin", ...). When it is set, a commit is rejected (ReasonTestLanguage) unless one of
	// its test files with a source extension is in one of them or, when every test file is data (golden files,
	// snapshots: no source extension), one of its code files is. Empty accepts every commit.
	Languages []string
	// TestCommand names the verify commands in that rejection's detail (for example "go test"); optional.
	TestCommand string
}

// Reason says why a commit is not a candidate.
type Reason string

// Rejection reasons. ReasonOrder lists them in the order Scan checks them.
const (
	ReasonUnreadable   Reason = "unreadable log record"
	ReasonMerge        Reason = "merge commit"
	ReasonShallow      Reason = "shallow clone boundary"
	ReasonRoot         Reason = "no parent commit"
	ReasonImported     Reason = "already a task"
	ReasonFixup        Reason = "fixup commit"
	ReasonRevert       Reason = "revert"
	ReasonEmpty        Reason = "no file changes"
	ReasonDocsOnly     Reason = "documentation only"
	ReasonNoTests      Reason = "no test changes"
	ReasonVendored     Reason = "vendored code changes"
	ReasonDependencies Reason = "dependency update"
	ReasonTestsOnly    Reason = "tests only"
	ReasonNoSource     Reason = "no source code"
	ReasonTestLanguage Reason = "tests in another language"
	ReasonFormatting   Reason = "formatting sweep"
	ReasonTooLarge     Reason = "too large"
	ReasonGenerated    Reason = "generated code"
	ReasonInlineRust   Reason = "inline Rust tests"
)

// ReasonOrder lists every rejection reason in the order Scan checks them, for tables that must not depend on map
// order.
func ReasonOrder() []Reason {
	return []Reason{ReasonUnreadable, ReasonMerge, ReasonShallow, ReasonRoot, ReasonImported, ReasonFixup, ReasonRevert,
		ReasonEmpty, ReasonDocsOnly, ReasonNoTests, ReasonVendored, ReasonDependencies, ReasonTestsOnly, ReasonNoSource, ReasonTestLanguage,
		ReasonFormatting, ReasonTooLarge, ReasonGenerated, ReasonInlineRust}
}

// Part is one named contribution to a candidate's score.
type Part struct {
	Points int
	Text   string
}

func (p Part) String() string { return fmt.Sprintf("%+d %s", p.Points, p.Text) }

// Candidate is a commit that could become a task: its parent is the base, its message the instruction, its test
// files the hidden tests and its other files the reference.
type Candidate struct {
	Hash, Parent  string
	Subject, Body string // Body excludes trailers such as Co-Authored-By
	Date          time.Time
	Tests, Code   []string
	Docs          []string // documentation the commit also changes
	Dependencies  []string // manifests and lockfiles the commit also changes (not counted in the limits)
	// TestLines and Lines are added plus deleted lines: in test files, and in test and code files together (docs and
	// dependencies are not counted).
	TestLines, Lines int
	Score            int
	Reasons          []Part
	raw              string // the body as written, trailers included (issue references may sit in trailers)
}

// Instruction is the task instruction a candidate gives: the subject, a blank line and the body without trailers.
func (c Candidate) Instruction() string {
	if c.Body == "" {
		return c.Subject
	}
	return c.Subject + "\n\n" + c.Body
}

// dirs counts the folders of the candidate's tests and code.
func (c Candidate) dirs() int {
	dirs := map[string]bool{}
	for _, p := range c.Tests {
		dirs[path.Dir(p)] = true
	}
	for _, p := range c.Code {
		dirs[path.Dir(p)] = true
	}
	return len(dirs)
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
	Ref        string // what was read, for display (main, origin/main or the Ref given)
	Head       string // the commit hash the scan started from
	Shallow    bool   // the repository is a shallow clone: older history is missing
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
	hash       string
	parents    []string
	date       time.Time
	subject    string
	rawBody    string // as written, trailers included
	files      []fileStat
	unreadable string // why the record could not be parsed; the other fields but hash may be empty
}

type fileStat struct {
	path   string
	lines  int // added plus deleted
	binary bool
}

// Scan reads the history of opts.Ref in the git repository at repoRoot and returns its candidates, best first, and
// every other commit it read with the reason it was set aside. Once ctx is done, the error wraps ctx.Err().
func Scan(ctx context.Context, repoRoot string, opts Options) (Result, error) {
	res, err := scan(ctx, repoRoot, withDefaults(opts))
	if err != nil && ctx.Err() != nil {
		return Result{}, fmt.Errorf("mine: %w", ctx.Err())
	}
	return res, err
}

func scan(ctx context.Context, root string, opts Options) (Result, error) {
	display, head, err := resolveRef(ctx, root, opts.Ref)
	if err != nil {
		return Result{}, err
	}
	shallow, err := gitx.Run(ctx, "-C", root, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return Result{}, fmt.Errorf("mine: %w", err)
	}
	commits, err := readLog(ctx, root, head, opts)
	if err != nil {
		return Result{}, err
	}
	res := Result{Ref: display, Head: head, Shallow: shallow == "true", Scanned: len(commits)}
	var newest, oldest time.Time
	for _, c := range commits {
		if c.date.IsZero() {
			continue
		}
		if newest.IsZero() || c.date.After(newest) {
			newest = c.date
		}
		if oldest.IsZero() || c.date.Before(oldest) {
			oldest = c.date
		}
	}
	for _, c := range commits {
		cand, rej := classify(c, opts, res.Shallow)
		if rej == nil {
			if rej, err = inspect(ctx, root, &cand); err != nil {
				return Result{}, err
			}
		}
		if rej != nil {
			rej.Hash, rej.Subject, rej.Date = c.hash, c.subject, c.date
			res.Rejected = append(res.Rejected, *rej)
			continue
		}
		cand.Score, cand.Reasons = score(cand, newest, oldest)
		res.Candidates = append(res.Candidates, cand)
	}
	slices.SortStableFunc(res.Candidates, compare)
	return res, nil
}

// compare orders candidates: higher score, then fewer files, fewer folders, the newer, and the hash.
func compare(a, b Candidate) int {
	if a.Score != b.Score {
		return b.Score - a.Score
	}
	if fa, fb := len(a.Tests)+len(a.Code), len(b.Tests)+len(b.Code); fa != fb {
		return fa - fb
	}
	if da, db := a.dirs(), b.dirs(); da != db {
		return da - db
	}
	if c := b.Date.Compare(a.Date); c != 0 {
		return c
	}
	return strings.Compare(a.Hash, b.Hash)
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
	return o
}

// commitOf returns the commit ref names (best a full ref name or a hash), or "" when it names none.
func commitOf(ctx context.Context, root, ref string) string {
	out, err := gitx.Run(ctx, "-C", root, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return out
}

// resolveRef returns ref for display and the commit it names. A branch wins over a tag of the same name, because the
// commit is resolved from its full ref name, and git log is given the hash. With ref empty, the default branch is used.
func resolveRef(ctx context.Context, root, ref string) (display, head string, err error) {
	if ref == "" {
		return defaultBranch(ctx, root)
	}
	if h := commitOf(ctx, root, "refs/heads/"+ref); h != "" {
		return ref, h, nil
	}
	if h := commitOf(ctx, root, ref); h != "" {
		return ref, h, nil
	}
	return "", "", fmt.Errorf("mine: %q is not a commit in %s", ref, root)
}

// defaultBranch picks the branch origin/HEAD names (the local branch or its remote, whichever contains the other;
// the local one if they diverged), else main, else master, else HEAD.
func defaultBranch(ctx context.Context, root string) (display, head string, err error) {
	if remote, err := gitx.Run(ctx, "-C", root, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil && remote != "" {
		branch := strings.TrimPrefix(remote, "refs/remotes/origin/")
		local, theirs := commitOf(ctx, root, "refs/heads/"+branch), commitOf(ctx, root, remote)
		switch {
		case local != "" && theirs != "" && local != theirs && isAncestor(ctx, root, local, theirs):
			return "origin/" + branch, theirs, nil
		case local != "":
			return branch, local, nil
		case theirs != "":
			return "origin/" + branch, theirs, nil
		}
	}
	for _, branch := range []string{"main", "master"} {
		if h := commitOf(ctx, root, "refs/heads/"+branch); h != "" {
			return branch, h, nil
		}
	}
	if h := commitOf(ctx, root, "HEAD"); h != "" {
		return "HEAD", h, nil
	}
	return "", "", fmt.Errorf("mine: %s has no commits", root)
}

// isAncestor reports whether a is an ancestor of b (git exits 1 when it is not; any failure counts as not).
func isAncestor(ctx context.Context, root, a, b string) bool {
	_, err := gitx.Run(ctx, "-C", root, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Separators of the log records: a record starts with RS, and each header field ends with US.
const (
	recordSep = "\x1e"
	fieldSep  = "\x1f"
	// headerFields is the number of US-terminated fields: hash, parents, date, subject, body.
	headerFields = 5
)

// recordStart matches the start of a real record (a hash and its separator), so an RS inside a message is not
// taken for one.
var recordStart = regexp.MustCompile(`^[0-9a-f]{40,64}` + fieldSep)

// readLog lists the history from head, newest first, with per-file line counts. Everything the user's or the
// repository's configuration could change in this output (renames, relative paths, signatures, colour, text
// conversion, external diff, the diff algorithm, the file order, the message encoding) is fixed on the command line.
// The global configuration is still read (gitx drops only the system one): it carries safe.directory, without which
// git refuses a repository owned by another user.
func readLog(ctx context.Context, root, head string, o Options) ([]commit, error) {
	args := []string{"-C", root, "log", "--numstat", "-z", "--no-renames", "--no-relative", "--no-color",
		"--no-show-signature", "--no-textconv", "--no-ext-diff", "--encoding=UTF-8", "--diff-algorithm=myers", "-O", "/dev/null",
		"--format=" + recordSep + "%H" + fieldSep + "%P" + fieldSep + "%ct" + fieldSep + "%s" + fieldSep + "%b" + fieldSep,
		"--max-count=" + strconv.Itoa(o.MaxCommits)}
	if !o.Since.IsZero() {
		args = append(args, "--since="+o.Since.UTC().Format(time.RFC3339))
	}
	args = append(args, head, "--")
	out, err := gitx.Output(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("mine: read history: %w", err)
	}
	return parseLog(out), nil
}

// parseLog parses readLog's output: per commit, RS, the header fields each ended by US, then NUL-terminated numstat
// entries ("added\tdeleted\tpath"; "-\t-\tpath" for binary files). A record whose message holds an RS or US byte
// cannot be split reliably and comes back marked unreadable.
func parseLog(out []byte) []commit {
	var records []string
	var glued []bool // the record absorbed a stray RS
	for i, rec := range strings.Split(string(out), recordSep) {
		switch {
		case i == 0:
			continue // before the first RS
		case recordStart.MatchString(rec) || len(records) == 0:
			records, glued = append(records, rec), append(glued, false)
		default:
			records[len(records)-1] += recordSep + rec
			glued[len(glued)-1] = true
		}
	}
	commits := make([]commit, 0, len(records))
	for i, rec := range records {
		c := parseRecord(rec)
		if glued[i] && c.unreadable == "" {
			c.unreadable = "a record separator byte (0x1e) in the message"
		}
		commits = append(commits, c)
	}
	return commits
}

func parseRecord(rec string) commit {
	hash, _, _ := strings.Cut(rec, fieldSep)
	c := commit{hash: hash}
	if n := strings.Count(rec, fieldSep); n != headerFields {
		c.unreadable = fmt.Sprintf("%d field separators (0x1f) where %d belong", n, headerFields)
		return c
	}
	fields := strings.Split(rec, fieldSep)
	secs, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		c.unreadable = "commit date " + strconv.Quote(fields[2])
		return c
	}
	c.parents, c.date, c.subject, c.rawBody = strings.Fields(fields[1]), time.Unix(secs, 0).UTC(), fields[3], fields[4]
	for _, entry := range strings.Split(fields[5], "\x00") {
		entry = strings.TrimLeft(entry, "\n")
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 3)
		if len(parts) != 3 {
			c.unreadable = "numstat entry " + strconv.Quote(entry)
			return c
		}
		added, errA := strconv.Atoi(parts[0])
		deleted, errD := strconv.Atoi(parts[1])
		c.files = append(c.files, fileStat{path: parts[2], lines: added + deleted, binary: errA != nil || errD != nil})
	}
	return c
}

// trailerKeys are the trailers stripped from a body: attribution and bookkeeping, which say nothing about the task.
var trailerKeys = map[string]bool{"co-authored-by": true, "signed-off-by": true, "reviewed-by": true, "acked-by": true,
	"tested-by": true, "reported-by": true, "suggested-by": true, "helped-by": true, "cc": true, "change-id": true,
	"generated-by": true, "assisted-by": true}

// stripTrailers drops the body's final paragraph when every line in it is a known trailer (or a continuation line of
// one), and trims the rest. Prose such as "Note: ..." stays.
func stripTrailers(body string) string {
	body = strings.TrimSpace(body)
	paras := strings.Split(body, "\n\n")
	last := paras[len(paras)-1]
	for i, line := range strings.Split(last, "\n") {
		if i > 0 && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			continue
		}
		key, _, ok := strings.Cut(line, ":")
		if !ok || !trailerKeys[strings.ToLower(key)] {
			return body
		}
	}
	return strings.TrimSpace(strings.Join(paras[:len(paras)-1], "\n\n"))
}

var (
	fixupMessage  = regexp.MustCompile(`^(fixup|squash|amend)!`)
	revertMessage = regexp.MustCompile(`^Revert "|(?m)^This reverts commit [0-9a-f]{7,}`)
	// depsMessage reads as a dependency update; it rejects a commit only together with a manifest change.
	depsMessage = regexp.MustCompile(`(?i)^\w+\(deps(-dev)?\)!?:|^bump \S+ from \S+ to \S+`)
	// formatMessage is a formatting change by its subject: a style: type, or a formatter's name first.
	formatMessage = regexp.MustCompile(`(?i)^(style(\([^)]*\))?!?:|(run |apply )?(gofmt|goimports|gofumpt|rustfmt|cargo fmt|prettier|black|ruff format|clang-format)\b)`)
	issueRef      = regexp.MustCompile(`(^|[\s(])#[0-9]+\b|/(issues|pull|merge_requests)/[0-9]+|(?i:\b(fix(es|ed)?|close[sd]?|resolve[sd]?|refs?)\b:?\s+)[A-Z][A-Z0-9]+-[0-9]+\b`)
	wipMessage    = regexp.MustCompile(`(?i)\bwip\b|\btypos?\b`)
	reviewMessage = regexp.MustCompile(`(?i)\baddress(es|ed|ing)?\b.*\breview|\breview (fixes|notes|follow-?ups?)\b`)
	conventional  = regexp.MustCompile(`^([a-z]+)(\([^)]*\))?!?:`)
)

// dependencyFiles are manifests and lockfiles.
var dependencyFiles = map[string]bool{
	"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true,
	"package.json": true, "package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "bun.lockb": true,
	"Cargo.toml": true, "Cargo.lock": true,
	"Gemfile": true, "Gemfile.lock": true,
	"pyproject.toml": true, "poetry.lock": true, "Pipfile": true, "Pipfile.lock": true, "uv.lock": true,
	"composer.json": true, "composer.lock": true,
	"pom.xml": true, "build.gradle": true, "build.gradle.kts": true, "gradle.lockfile": true, "libs.versions.toml": true,
}

// isVendored reports whether p is vendored code: an offline agent could not reproduce a change to it, so such a
// commit is no task.
func isVendored(p string) bool {
	for _, dir := range strings.Split(path.Dir(p), "/") {
		if dir == "vendor" || dir == "node_modules" {
			return true
		}
	}
	return false
}

// isDependencyFile reports whether p is a manifest or a lockfile.
func isDependencyFile(p string) bool {
	base := path.Base(p)
	return dependencyFiles[base] || strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")
}

// languages names source languages by file extension; a commit needs a code file with one of these to be a task.
var languages = map[string]string{
	".go": "go", ".py": "python", ".rs": "rust", ".java": "java", ".kt": "kotlin", ".kts": "kotlin", ".scala": "scala",
	".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".ts": "typescript", ".tsx": "typescript",
	".rb": "ruby", ".php": "php", ".cs": "c#", ".swift": "swift", ".c": "c", ".h": "c", ".cc": "c++", ".cpp": "c++",
	".hpp": "c++", ".m": "objective-c", ".dart": "dart", ".ex": "elixir", ".exs": "elixir", ".erl": "erlang", ".hs": "haskell",
	".clj": "clojure", ".lua": "lua", ".sh": "shell", ".vue": "vue", ".svelte": "svelte", ".zig": "zig",
	".cxx": "c++", ".hxx": "c++", ".hh": "c++", ".mm": "objective-c", ".groovy": "groovy", ".fs": "f#", ".vb": "visual basic",
	".r": "r", ".jl": "julia", ".pl": "perl", ".ml": "ocaml", ".sql": "sql", ".proto": "protobuf", ".html": "html",
	".css": "css", ".scss": "scss",
}

// isSource reports whether p has a known source extension.
func isSource(p string) bool {
	_, ok := languages[strings.ToLower(path.Ext(p))]
	return ok
}

// language names p's language, or its extension when it is not a known source language.
func language(p string) string {
	ext := strings.ToLower(path.Ext(p))
	if l, ok := languages[ext]; ok {
		return l
	}
	if ext == "" {
		return "extensionless"
	}
	return ext
}

// Formatting sweeps: at least sweepFiles code files with at most sweepLines changed lines each on average.
const (
	sweepFiles = 5
	sweepLines = 4
)

// classify applies the checks that need only the log: on success the candidate has its files and sizes but no score.
func classify(c commit, o Options, shallow bool) (Candidate, *Rejection) {
	reject := func(r Reason, format string, args ...any) (Candidate, *Rejection) {
		return Candidate{}, &Rejection{Reason: r, Detail: fmt.Sprintf(format, args...)}
	}
	switch {
	case c.unreadable != "":
		return reject(ReasonUnreadable, "%s", c.unreadable)
	case len(c.parents) > 1:
		return reject(ReasonMerge, "%d parents", len(c.parents))
	case len(c.parents) == 0 && shallow:
		return reject(ReasonShallow, "the clone's history ends here: fetch more to mine it")
	case len(c.parents) == 0:
		return reject(ReasonRoot, "the first commit has no base to start from")
	case o.Exclude[c.hash]:
		return reject(ReasonImported, "")
	case fixupMessage.MatchString(c.subject):
		return reject(ReasonFixup, "to be squashed into another commit")
	case revertMessage.MatchString(c.subject) || revertMessage.MatchString(c.rawBody):
		return reject(ReasonRevert, "")
	}
	cand := Candidate{Hash: c.hash, Parent: c.parents[0], Subject: c.subject, Body: stripTrailers(c.rawBody), Date: c.date, raw: c.rawBody}
	codeLines, codeBinary := 0, 0
	var vendored []string
	for _, f := range c.files {
		switch {
		case isVendored(f.path):
			vendored = append(vendored, f.path)
			continue
		case task.IsTestFile(f.path):
			cand.Tests = append(cand.Tests, f.path)
			cand.TestLines += f.lines
		case claudectx.IsDocument(f.path):
			cand.Docs = append(cand.Docs, f.path)
			continue
		case isDependencyFile(f.path):
			cand.Dependencies = append(cand.Dependencies, f.path)
			continue
		default:
			cand.Code = append(cand.Code, f.path)
			codeLines += f.lines
			if f.binary {
				codeBinary++
			}
		}
		cand.Lines += f.lines
	}
	switch {
	case len(c.files) == 0:
		return reject(ReasonEmpty, "")
	case len(cand.Tests) == 0 && len(cand.Code) == 0 && len(cand.Dependencies) == 0 && len(vendored) == 0:
		return reject(ReasonDocsOnly, "%d documents", len(cand.Docs))
	case len(cand.Tests) == 0:
		return reject(ReasonNoTests, "%d code files, no test files", len(cand.Code)+len(cand.Dependencies)+len(vendored))
	case len(vendored) > 0:
		return reject(ReasonVendored, "%d vendored files, such as %s", len(vendored), vendored[0])
	case len(cand.Code) == 0 && len(cand.Dependencies) > 0:
		return reject(ReasonDependencies, "only manifests or lockfiles change besides tests: %s", strings.Join(cand.Dependencies, ", "))
	case len(cand.Code) == 0:
		return reject(ReasonTestsOnly, "%d test files, nothing to implement", len(cand.Tests))
	case len(cand.Dependencies) > 0 && depsMessage.MatchString(c.subject):
		return reject(ReasonDependencies, "the subject reads as a dependency update and %s changes", strings.Join(cand.Dependencies, ", "))
	}
	if !slices.ContainsFunc(cand.Code, isSource) {
		return reject(ReasonNoSource, "only %s", strings.Join(cand.Code, ", "))
	}
	if detail := testLanguage(cand, o.Languages); detail != "" {
		if o.TestCommand != "" {
			detail += "; the verify command is " + o.TestCommand
		}
		return reject(ReasonTestLanguage, "%s", detail)
	}
	if formatMessage.MatchString(c.subject) {
		return reject(ReasonFormatting, "the subject names a formatting change")
	}
	if text := len(cand.Code) - codeBinary; text >= sweepFiles && codeLines <= sweepLines*text {
		return reject(ReasonFormatting, "%d text code files with %d changed lines in all", text, codeLines)
	}
	if files := len(cand.Tests) + len(cand.Code); files > o.MaxFiles {
		return reject(ReasonTooLarge, "%d files (limit %d)", files, o.MaxFiles)
	}
	if cand.Lines > o.MaxLines {
		return reject(ReasonTooLarge, "%d changed lines (limit %d)", cand.Lines, o.MaxLines)
	}
	return cand, nil
}

// testLanguage returns why the verify commands for langs would not run c's tests, or "" when they would (or langs
// is empty). Test files with a source extension must include one in langs; when every test file is data, a code file
// must be in langs instead (a golden file serves the tests of the code beside it).
func testLanguage(c Candidate, langs []string) string {
	if len(langs) == 0 {
		return ""
	}
	in := func(p string) bool { return isSource(p) && slices.Contains(langs, language(p)) }
	names := func(paths []string, keep func(string) bool) string {
		var out []string
		for _, p := range paths {
			if l := language(p); keep(p) && !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
		return strings.Join(out, " and ")
	}
	if slices.ContainsFunc(c.Tests, isSource) {
		if slices.ContainsFunc(c.Tests, in) {
			return ""
		}
		return names(c.Tests, isSource) + " tests"
	}
	if slices.ContainsFunc(c.Code, in) {
		return ""
	}
	return names(c.Tests, func(string) bool { return true }) + " test data for " + names(c.Code, isSource) + " code"
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

const (
	headerLines = 10
	// maxHeaderBlob bounds the files read for a generated-code header; larger ones are not inspected.
	maxHeaderBlob = 1 << 20
)

// generatedFiles lists the files among paths that are generated in commit. A --batch-check pass learns which paths
// exist as blobs and their sizes; one --batch pass then reads the ones small enough. Files the commit deletes, larger
// files, binary files and paths with a newline (which the batch protocol cannot name) are skipped.
func generatedFiles(ctx context.Context, root, commit string, paths []string) ([]string, error) {
	var queried []string
	for _, p := range paths {
		if !strings.Contains(p, "\n") {
			queried = append(queried, p)
		}
	}
	sizes, err := batch(ctx, root, commit, queried, "--batch-check", nil)
	if err != nil {
		return nil, err
	}
	var small []string
	for _, p := range queried {
		if size, ok := sizes[p]; ok && size <= maxHeaderBlob {
			small = append(small, p)
		}
	}
	var generated []string
	_, err = batch(ctx, root, commit, small, "--batch", func(p string, content []byte) {
		if !bytes.Contains(content, []byte{0}) && isGenerated(content) {
			generated = append(generated, p)
		}
	})
	return generated, err
}

// batch runs git cat-file mode (--batch-check or --batch) on commit:path for each path and returns the size of every
// path that is a blob; with --batch, read receives each blob's content. Paths cat-file reports as missing or
// ambiguous are left out. The response header is checked by its suffix first, since a path may contain spaces.
func batch(ctx context.Context, root, commit string, paths []string, mode string, read func(p string, content []byte)) (map[string]int, error) {
	sizes := map[string]int{}
	if len(paths) == 0 {
		return sizes, nil
	}
	var stdin bytes.Buffer
	for _, p := range paths {
		fmt.Fprintf(&stdin, "%s:%s\n", commit, p)
	}
	out, err := gitx.Output(ctx, &stdin, "-C", root, "cat-file", mode)
	if err != nil {
		return nil, fmt.Errorf("mine: read files of %s: %w", commit, err)
	}
	r := bufio.NewReader(bytes.NewReader(out))
	for _, p := range paths {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("mine: read files of %s: %w", commit, err)
		}
		header = strings.TrimSuffix(header, "\n")
		if strings.HasSuffix(header, " missing") || strings.HasSuffix(header, " ambiguous") {
			continue
		}
		fields := strings.Fields(header) // "<oid> <type> <size>"
		if len(fields) != 3 {
			return nil, fmt.Errorf("mine: read files of %s: unexpected header %q", commit, header)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("mine: read files of %s: unexpected header %q", commit, header)
		}
		if mode == "--batch" {
			content := make([]byte, size+1) // the content and its newline
			if _, err := io.ReadFull(r, content); err != nil {
				return nil, fmt.Errorf("mine: read files of %s: %w", commit, err)
			}
			if fields[1] == "blob" {
				read(p, content[:size])
			}
		}
		if fields[1] == "blob" {
			sizes[p] = size
		}
	}
	return sizes, nil
}

func isGenerated(content []byte) bool {
	for i, line := range strings.SplitN(string(content), "\n", headerLines+1) {
		if i < headerLines && generatedHeader.MatchString(strings.TrimRight(line, "\r")) {
			return true
		}
	}
	return false
}

// score sums a candidate's named parts. newest and oldest bound the dates of the scanned window, which recency is
// measured within.
func score(c Candidate, newest, oldest time.Time) (int, []Part) {
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
	if issueRef.MatchString(c.Subject + "\n" + c.raw) {
		add(2, "references an issue")
	}
	if wipMessage.MatchString(c.Subject) {
		add(-4, "work in progress or a typo fix")
	}
	if reviewMessage.MatchString(c.Subject) {
		add(-3, "a review follow-up")
	}
	if m := conventional.FindStringSubmatch(c.Subject); m != nil {
		switch m[1] {
		case "feat", "fix":
			add(1, "a %s commit", m[1])
		case "docs", "chore", "ci", "style", "build", "test":
			add(-2, "a %s commit", m[1])
		}
	}

	switch {
	case c.Lines < 10:
		add(0, "a tiny change (%d lines)", c.Lines)
	case c.Lines < 30:
		add(1, "a small change (%d lines)", c.Lines)
	case c.Lines <= 150:
		add(3, "a moderate size (%d lines)", c.Lines)
	case c.Lines <= 300:
		add(2, "a medium size (%d lines)", c.Lines)
	default:
		add(1, "a large change (%d lines)", c.Lines)
	}

	codeLines := c.Lines - c.TestLines
	switch {
	case codeLines == 0:
		add(0, "no changed code lines (binary files)")
	case c.TestLines*4 < codeLines:
		add(0, "few test lines (%d for %d code lines)", c.TestLines, codeLines)
	case c.TestLines > 4*codeLines:
		add(0, "mostly tests (%d test lines for %d code lines)", c.TestLines, codeLines)
	default:
		add(2, "tests in proportion to code (%d test lines, %d code lines)", c.TestLines, codeLines)
	}

	if len(c.Dependencies) > 0 {
		add(-2, "changes a manifest or lockfile (new dependencies cannot be fetched offline)")
	}

	if span := newest.Sub(oldest); span > 0 {
		switch age := newest.Sub(c.Date); {
		case age*3 <= span:
			add(2, "among the newest third of the scanned history")
		case age*3 <= span*2:
			add(1, "in the middle third of the scanned history")
		default:
			add(0, "in the oldest third of the scanned history")
		}
	}

	if n := c.dirs(); n > 4 {
		add(-2, "touches %d folders", n)
	}

	total := 0
	for _, p := range parts {
		total += p.Points
	}
	return total, parts
}
