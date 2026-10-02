// Package screen is the pull-request cost screen. Classify decides what a pushed range is to the screen: a change to
// the context Claude Code reads (a screen candidate), a change to the harness, which automated mode refuses, or
// neither.
//
// Everything is read from git objects through gitx and source: no checkout, no index write, no hook, filter or
// external diff, and nothing from the repository is executed. The range's text is data: it is matched, never run.
package screen

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/source"
)

// Verdict is what a pushed range is to the screen.
type Verdict string

const (
	// VerdictContext is a screen candidate: the push changes files Claude Code reads as context, and the head's
	// context differs from the merge base's.
	VerdictContext Verdict = "context"
	// VerdictHarness is refused in automated mode: the range changes what runs (settings, hooks, MCP servers,
	// Agentium's configuration), whoever wrote it. It wins over a context change. The user may still run the screen
	// by hand after reading the change.
	VerdictHarness Verdict = "harness"
	// VerdictNone needs no screen: no context file changed (code only, for example).
	VerdictNone Verdict = "none"
)

// KindLinked marks a document that context files link to ([text](path)): the agent reads it only if it opens it.
const KindLinked = "linked"

var (
	ErrUnknownCommit   = errors.New("not a commit in this repository")
	ErrNoDefaultBranch = errors.New("no default branch to compare with")
	ErrNoMergeBase     = errors.New("the pushed commit shares no history with the default branch")
	ErrPartialClone    = errors.New("this is a partial clone (--filter): reading its history could fetch objects from the network")
)

// Range is a push: Old is the remote's commit before it (empty or all zeros for a new branch), New the pushed one.
// They are object names or revisions; nothing else is accepted from them.
type Range struct {
	Old, New string
}

// Options locate the default branch, whose merge base with the pushed commit is the screen's base arm.
type Options struct {
	// DefaultBranch is a revision such as "origin/main". Empty: <Remote>/HEAD, then <Remote>/main, then <Remote>/master.
	DefaultBranch string
	// Remote is the pushed-to remote ("origin" when empty), as a pre-push hook receives it.
	Remote string
}

// Change is a read-context file a range changes.
type Change struct {
	Path   string `json:"path"`
	Status string `json:"status"` // added, deleted, modified or type-changed (a file became a link, or the reverse)
	Kind   string `json:"kind"`   // the claudectx kind, or KindLinked
	// Via is the context file that reaches Path through a symbolic link (CLAUDE.md -> docs/rules.md), if any.
	Via string `json:"via,omitempty"`
}

// Finding is one reason a range is refused.
type Finding struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// Result is a classified push.
type Result struct {
	Verdict       Verdict `json:"verdict"`
	Head          string  `json:"head"`           // the pushed commit
	Base          string  `json:"base"`           // its merge base with the default branch: the screen's base arm
	DefaultBranch string  `json:"default_branch"` // the revision Base was taken against
	// Start is where the pushed range starts: Range.Old when it is an ancestor of Head, otherwise Base, and StartNote
	// says why (a new branch, an unknown or force-pushed old commit).
	Start     string `json:"start"`
	StartNote string `json:"start_note,omitempty"`
	// Pushed are the read-context files changed between Start and Head; Arms those that differ between Base and Head,
	// which is what a screen would compare.
	Pushed []Change `json:"pushed"`
	Arms   []Change `json:"arms"`
	// Harness are the reasons for refusal, from either range: checking the arms too means an earlier push that
	// changed the harness cannot be hidden by a later push that changes only context.
	Harness []Finding `json:"harness,omitempty"`
}

// Refused reports whether automated mode must refuse the range.
func (r Result) Refused() bool { return r.Verdict == VerdictHarness }

// Reason is one line for the digest.
func (r Result) Reason() string {
	switch r.Verdict {
	case VerdictHarness:
		parts := make([]string, 0, 3)
		for _, f := range r.Harness[:min(3, len(r.Harness))] {
			parts = append(parts, fmt.Sprintf("%s (%s: %s)", f.Path, f.Status, f.Reason))
		}
		more := ""
		if len(r.Harness) > 3 {
			more = fmt.Sprintf(", and %d more", len(r.Harness)-3)
		}
		return "refused: the range changes what Claude Code runs, which automated mode never screens: " +
			strings.Join(parts, "; ") + more + ". Read the change, then run the screen by hand if it is safe."
	case VerdictContext:
		return fmt.Sprintf("a screen candidate: the push changes %d context file(s), and the head's context differs from the base's in %d", len(r.Pushed), len(r.Arms))
	}
	if len(r.Pushed) > 0 {
		return "no screen: the push changes context, but the head's context is the same as the merge base's"
	}
	return "no screen: the range changes no file Claude Code reads as context"
}

// Classify reads the push in the repository at repo. The harness is checked over both the pushed range (Start..Head)
// and the arms (Base..Head), and any harness change refuses the range. It is a context candidate only when the push
// changes context and the arms differ in context. An old commit that is missing, all zeros or not an ancestor of the
// new one (a force push) is replaced by the merge base: that is the screen's base arm anyway, so the classification
// matches what runs would compare, and no claimed old commit can narrow the range. Errors mean the push could not be
// classified (an unknown pushed commit, no default branch, no shared history, a partial clone).
func Classify(ctx context.Context, repo string, pushed Range, opts Options) (Result, error) {
	if partial, err := gitx.PartialClone(ctx, repo); err != nil {
		return Result{}, fmt.Errorf("screen: %w", err)
	} else if partial {
		return Result{}, ErrPartialClone
	}
	head, err := resolveCommit(ctx, repo, pushed.New)
	if err != nil {
		return Result{}, fmt.Errorf("screen: the pushed commit %q: %w", pushed.New, err)
	}
	branch, tip, err := defaultBranch(ctx, repo, opts)
	if err != nil {
		return Result{}, err
	}
	base, err := gitx.Run(ctx, "-C", repo, "merge-base", head, tip)
	if err != nil || base == "" {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("screen: %s and %s: %w", short(head), branch, ErrNoMergeBase)
	}
	res := Result{Head: head, Base: base, DefaultBranch: branch}
	res.Start, res.StartNote = pushedStart(ctx, repo, pushed.Old, head, base)
	c := &classifier{ctx: ctx, repo: repo, sides: map[string]*side{}}
	arms, err := c.compare(base, head)
	if err != nil {
		return Result{}, err
	}
	push := arms
	if res.Start != base {
		if push, err = c.compare(res.Start, head); err != nil {
			return Result{}, err
		}
	}
	res.Pushed, res.Arms = push.context, arms.context
	res.Harness = mergeFindings(push.harness, arms.harness)
	switch {
	case len(res.Harness) > 0:
		res.Verdict = VerdictHarness
	case len(res.Pushed) > 0 && len(res.Arms) > 0:
		res.Verdict = VerdictContext
	default:
		res.Verdict = VerdictNone
	}
	return res, nil
}

// resolveCommit turns a revision into a commit id; anything that could read as an option is refused.
func resolveCommit(ctx context.Context, repo, rev string) (string, error) {
	if rev == "" || strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, "\x00\n\r") {
		return "", ErrUnknownCommit
	}
	oid, err := gitx.Run(ctx, "-C", repo, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil || oid == "" {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrUnknownCommit
	}
	return oid, nil
}

var remoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// defaultBranch finds the default branch's name and commit.
func defaultBranch(ctx context.Context, repo string, opts Options) (string, string, error) {
	candidates := []string{opts.DefaultBranch}
	if opts.DefaultBranch == "" {
		remote := cmp.Or(opts.Remote, "origin")
		if !remoteName.MatchString(remote) {
			return "", "", fmt.Errorf("screen: remote %q: %w", remote, ErrNoDefaultBranch)
		}
		candidates = []string{"refs/remotes/" + remote + "/HEAD", "refs/remotes/" + remote + "/main", "refs/remotes/" + remote + "/master"}
	}
	for _, name := range candidates {
		if oid, err := resolveCommit(ctx, repo, name); err == nil {
			return name, oid, nil
		} else if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
	}
	return "", "", fmt.Errorf("screen: tried %s: %w", strings.Join(candidates, ", "), ErrNoDefaultBranch)
}

// pushedStart picks where the pushed range starts, and says why when it is not old.
func pushedStart(ctx context.Context, repo, old, head, base string) (string, string) {
	const fallback = ": the pushed range starts at the merge base with the default branch"
	if strings.Trim(old, "0") == "" {
		return base, "no old commit (a new branch)" + fallback
	}
	oid, err := resolveCommit(ctx, repo, old)
	if err != nil {
		return base, "the old commit is not in this repository (a force push, or not fetched)" + fallback
	}
	if _, err := gitx.Run(ctx, "-C", repo, "merge-base", "--is-ancestor", oid, head); err != nil {
		return base, "the old commit is not an ancestor of the new one (a force push)" + fallback
	}
	return oid, ""
}

func short(oid string) string { return oid[:min(12, len(oid))] }

type classifier struct {
	ctx   context.Context
	repo  string
	sides map[string]*side
}

type diff struct {
	context []Change
	harness []Finding
}

// change is one path that differs between two commits.
type change struct {
	path, status string
}

// changes lists the paths that differ between from and to: plumbing only (no external diff, no text conversion,
// nothing checked out), and no rename detection, so a rename is a deletion plus an addition and both paths count.
func (c *classifier) changes(from, to string) ([]change, error) {
	out, err := gitx.Output(c.ctx, nil, "-C", c.repo, "diff-tree", "-r", "-z", "--no-renames", "--raw", from, to)
	if err != nil {
		return nil, fmt.Errorf("screen: compare %s with %s: %w", short(from), short(to), err)
	}
	fields := strings.Split(string(out), "\x00") // ":<mode> <mode> <oid> <oid> <status>" NUL <path> NUL ...
	var changes []change
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(fields[i])
		if len(meta) != 5 || !strings.HasPrefix(meta[0], ":") || meta[4] == "" {
			return nil, fmt.Errorf("screen: unexpected git diff-tree output %q", fields[i])
		}
		status := map[byte]string{'A': "added", 'D': "deleted", 'M': "modified", 'T': "type-changed"}[meta[4][0]]
		changes = append(changes, change{path: fields[i+1], status: cmp.Or(status, "changed")})
	}
	return changes, nil
}

// compare classifies every path that differs between from and to, looking at both commits.
func (c *classifier) compare(from, to string) (diff, error) {
	a, err := c.side(from)
	if err != nil {
		return diff{}, err
	}
	b, err := c.side(to)
	if err != nil {
		return diff{}, err
	}
	changes, err := c.changes(from, to)
	if err != nil {
		return diff{}, err
	}
	var d diff
	for _, ch := range changes {
		var found []Finding
		present := false
		for _, s := range []*side{a, b} {
			if !source.Has(s.src, ch.path) {
				continue
			}
			present = true
			more, err := s.harnessFindings(ch)
			if err != nil {
				return diff{}, err
			}
			found = append(found, more...)
		}
		if why := b.harnessByName(ch.path); !present && why != "" { // a submodule is a file in neither commit
			found = append(found, Finding{Path: ch.path, Status: ch.status, Reason: why})
		}
		if len(found) > 0 {
			d.harness = append(d.harness, found...)
			continue // a harness file is never also context
		}
		if kind, via := b.contextKind(ch.path); kind != "" {
			d.context = append(d.context, Change{Path: ch.path, Status: ch.status, Kind: kind, Via: via})
		} else if kind, via := a.contextKind(ch.path); kind != "" {
			d.context = append(d.context, Change{Path: ch.path, Status: ch.status, Kind: kind, Via: via})
		}
	}
	d.harness = mergeFindings(d.harness)
	return d, nil
}

func mergeFindings(lists ...[]Finding) []Finding {
	var out []Finding
	for _, list := range lists {
		for _, f := range list {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// maxHarnessFiles caps the harness files of one commit that are followed and read for the paths they name. A commit
// with more (a link to the root makes every file harness) gets tooMany, which refuses every change to its files,
// rather than being read slowly or checked partly.
const maxHarnessFiles = 256

// side is what one commit holds, as the screen sees it.
type side struct {
	src source.Source
	// kinds are the read-context files: claudectx's entries other than harness, and linked documents.
	kinds map[string]string
	// harness says why each file of the commit is harness (see harnessByName), including files reached through a
	// symbolic link that is harness.
	harness map[string]string
	// contextLinks are read-context files that are symbolic links: the files they reach are context too.
	contextLinks []reach
	// fields are skills, subagents and commands whose frontmatter declares harness fields.
	fields map[string][]string
	// referrers are the contents of harness files and of files with harness frontmatter: a file they name changes
	// what runs (a hook script, an MCP server's program), so a change to it is a harness change.
	referrers map[string][]byte
	tooMany   bool
}

// side reads a commit once.
func (c *classifier) side(commit string) (*side, error) {
	if s := c.sides[commit]; s != nil {
		return s, nil
	}
	src, err := source.Commit(c.ctx, commit, "-C", c.repo)
	if err != nil {
		return nil, fmt.Errorf("screen: read %s: %w", short(commit), err)
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return nil, fmt.Errorf("screen: the context of %s: %w", short(commit), err)
	}
	s := &side{src: src, kinds: map[string]string{}, harness: map[string]string{}, fields: map[string][]string{}, referrers: map[string][]byte{}}
	for _, e := range resolved.Entries {
		if e.Kind != claudectx.KindHarness {
			s.kinds[e.Path] = e.Kind
		}
	}
	for _, p := range resolved.Linked {
		if _, ok := s.kinds[p]; !ok {
			s.kinds[p] = KindLinked
		}
	}
	for _, p := range src.Paths() {
		if why := s.harnessByName(p); why != "" {
			s.harness[p] = why
		}
	}
	// A harness path that is a symbolic link makes what it reaches harness too (.claude/hooks -> ../scripts makes
	// every script a hook), and so on through further links, until nothing new is reached.
	queue := sortedKeys(s.harness)
	for len(queue) > 0 && len(s.harness) <= maxHarnessFiles {
		link := queue[0]
		queue = queue[1:]
		r, err := s.follow(link)
		if err != nil {
			return nil, err
		}
		if r == nil {
			continue
		}
		for _, p := range src.Paths() {
			if _, done := s.harness[p]; !done && r.reaches(p) {
				s.harness[p] = "reached through the symbolic link " + link + ", which is harness"
				queue = append(queue, p)
			}
		}
	}
	for _, p := range sortedKeys(s.kinds) {
		r, err := s.follow(p)
		if err != nil {
			return nil, err
		}
		if r != nil {
			s.contextLinks = append(s.contextLinks, *r)
		}
		switch s.kinds[p] {
		case claudectx.KindSkill, claudectx.KindSubagent, claudectx.KindCommand:
			data, err := src.ReadFile(p)
			if err != nil {
				continue // Resolve could not read it either, so it is not loaded
			}
			if fields := claudectx.HarnessFrontmatter(data); len(fields) > 0 {
				s.fields[p], s.referrers[p] = fields, data
			}
		}
	}
	if s.tooMany = len(s.harness) > maxHarnessFiles; !s.tooMany {
		for _, p := range sortedKeys(s.harness) {
			if data, err := src.ReadFile(p); err == nil { // an unreadable link names nothing; the path itself counts
				s.referrers[p] = data
			}
		}
	}
	c.sides[commit] = s
	return s, nil
}

// harnessByName says why p is harness by its path alone, or returns "". The list of harness files is claudectx's
// (IsHarness), applied as a case-insensitive file system would see p and at every folder, so .Claude/Settings.json
// and pkg/.claude/settings.json count. On top of it, with no list to drift: agentium.toml anywhere, and any file
// in a .claude folder other than the instructions, rules, skills, subagents and commands that Resolve found there
// (settings.local.json, output styles, a folder replaced by a link, whatever a later Claude Code reads).
func (s *side) harnessByName(p string) string {
	folded := foldPath(p)
	parts := strings.Split(folded, "/")
	for i := range parts {
		if claudectx.IsHarness(strings.Join(parts[i:], "/")) {
			if i == 0 && folded == p {
				return "Claude Code's settings, hooks or MCP servers"
			}
			return "Claude Code's settings, hooks or MCP servers, in another letter case or a nested folder"
		}
	}
	if parts[len(parts)-1] == "agentium.toml" {
		return "Agentium's own configuration"
	}
	if slices.Contains(parts, ".claude") && !s.claudeFolderContext(p) {
		return "a file in Claude Code's configuration folder (.claude) that is not instructions, a rule, a skill, a subagent or a command"
	}
	return ""
}

// claudeFolderContext reports whether p, in a .claude folder, is read context there.
func (s *side) claudeFolderContext(p string) bool {
	switch s.kinds[p] {
	case claudectx.KindInstructions, claudectx.KindRule, claudectx.KindScopedRule, claudectx.KindSkill,
		claudectx.KindSkillFile, claudectx.KindSubagent, claudectx.KindCommand:
		return true
	case claudectx.KindNested: // pkg/.claude/CLAUDE.md; an imported or linked file there is not
		return path.Base(foldPath(p)) == "claude.md"
	}
	return false
}

// harnessAt says why p, a file or a link's target that need not be a file, is harness in this commit.
func (s *side) harnessAt(p string) string {
	if why, ok := s.harness[p]; ok {
		return why
	}
	return s.harnessByName(p)
}

// harnessFindings lists why ch, a file of this commit, is a harness change, as this commit sees it.
func (s *side) harnessFindings(ch change) ([]Finding, error) {
	var found []Finding
	add := func(reason string) { found = append(found, Finding{Path: ch.path, Status: ch.status, Reason: reason}) }
	if why := s.harness[ch.path]; why != "" {
		add(why)
	}
	if fields := s.fields[ch.path]; len(fields) > 0 {
		add("its frontmatter declares " + strings.Join(fields, ", ") + ", which change what runs")
	}
	if s.tooMany {
		add(fmt.Sprintf("the commit has more than %d harness files, too many to check what they reach and name", maxHarnessFiles))
	}
	r, err := s.follow(ch.path)
	if err != nil {
		return nil, err
	}
	if r != nil { // a link from context to harness (CLAUDE.md -> .claude/settings.json), or to a folder holding it
		for _, hop := range append(r.hops[1:], r.real) {
			if why := s.harnessAt(hop); why != "" {
				add("a symbolic link to " + hop + ": " + why)
				break
			}
		}
		for _, h := range sortedKeys(s.harness) {
			if h != ch.path && r.real != "" && (r.real == "." || strings.HasPrefix(h, r.real+"/")) {
				add("a symbolic link to the folder " + r.real + ", which holds harness files")
				break
			}
		}
	}
	for _, ref := range sortedKeys(s.referrers) {
		if ref != ch.path && (mentions(s.referrers[ref], ch.path) || mentions(s.referrers[ref], path.Base(ch.path))) {
			add("named by " + ref + ", which is harness: the file may run")
		}
	}
	return found, nil
}

// contextKind is p's read-context kind in this commit, directly or through a context file that links to it.
func (s *side) contextKind(p string) (kind, via string) {
	if kind := s.kinds[p]; kind != "" {
		return kind, ""
	}
	for _, r := range s.contextLinks {
		if r.reaches(p) {
			return s.kinds[r.from], r.from
		}
	}
	return "", ""
}

// reach is where a symbolic link leads in one commit.
type reach struct {
	from string
	// hops are the links passed through, from first; real is the resolved path ("." for the root, "" when the
	// links leave the repository or loop).
	hops []string
	real string
}

// reaches reports whether p, a file of the commit, is reached: it is a link on the way, the target or under it.
func (r reach) reaches(p string) bool {
	if p != r.from && slices.Contains(r.hops, p) {
		return true
	}
	return r.real != "" && (p == r.real || r.real == "." || strings.HasPrefix(p, r.real+"/"))
}

// maxLinks bounds the links resolved for one path, as an operating system bounds them.
const maxLinks = 40

// follow resolves p as a checkout's file system would: every link, in any folder of the path, relative to the
// link's folder. It returns nil when p is not a symbolic link. Only link targets are read.
func (s *side) follow(p string) (*reach, error) {
	if _, isLink, err := source.Link(s.src, p); err != nil || !isLink {
		return nil, err
	}
	r := &reach{from: p}
	rest, cur := strings.Split(p, "/"), ""
	for len(rest) > 0 {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if cur == "" {
				return r, nil // leaves the repository
			}
			cur = parent(cur)
			continue
		}
		next := part
		if cur != "" {
			next = cur + "/" + part
		}
		target, isLink, err := source.Link(s.src, next)
		if err != nil {
			return nil, fmt.Errorf("screen: %w", err)
		}
		if !isLink {
			cur = next
			continue
		}
		if r.hops = append(r.hops, next); len(r.hops) > maxLinks || path.IsAbs(target) {
			return r, nil // a loop, or outside the repository
		}
		rest = append(strings.Split(target, "/"), rest...)
		cur = parent(next)
	}
	r.real = cmp.Or(cur, ".")
	return r, nil
}

func parent(p string) string {
	if dir := path.Dir(p); dir != "." {
		return dir
	}
	return ""
}

// mentions reports whether data names name as a whole token: not inside a longer file name or path segment.
func mentions(data []byte, name string) bool {
	if name == "" {
		return false
	}
	text := string(data)
	for i := 0; ; {
		at := strings.Index(text[i:], name)
		if at < 0 {
			return false
		}
		start, end := i+at, i+at+len(name)
		if (start == 0 || !nameByte(text[start-1])) && (end == len(text) || !nameByte(text[end])) {
			return true
		}
		i = start + 1
	}
}

func nameByte(c byte) bool {
	return c == '.' || c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// foldPath is p as a case-insensitive file system (macOS, by default) compares names: letter case folded the way
// strings.EqualFold folds it (so the long s ſ is s, and the Kelvin sign is k), and the code points HFS+ ignores
// dropped, as git's own .git checks drop them. ASCII letters come out lower case.
func foldPath(p string) string {
	var b strings.Builder
	for _, r := range p {
		if hfsIgnorable(r) {
			continue
		}
		folded := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			folded = min(folded, f)
		}
		if folded <= unicode.MaxASCII {
			folded = unicode.ToLower(folded)
		}
		b.WriteRune(folded)
	}
	return b.String()
}

// hfsIgnorable are the code points HFS+ leaves out of file name comparisons (git's utf8.c, next_hfs_char).
func hfsIgnorable(r rune) bool {
	return r >= 0x200c && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x206a && r <= 0x206f || r == 0xfeff
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
