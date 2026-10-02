// Package screen is the pull-request cost screen. Classify decides what a pushed range is to the screen: a change to
// the context Claude Code reads (a screen candidate), a change to the harness, which automated mode refuses, or
// neither.
//
// Everything is read from git objects through gitx and source: no checkout, no index write, no hook, filter or
// external diff, no lazy fetch, and nothing from the repository is executed. The range's text is data: it is
// matched, never run. Where the reading is in doubt, the answer is harness.
package screen

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	ErrUnknownCommit   = errors.New("not a full commit id of this repository")
	ErrNoDefaultBranch = errors.New("no default branch to compare with")
	ErrNoMergeBase     = errors.New("the pushed commit shares no history with the default branch")
	ErrPartialClone    = errors.New("this is a partial clone (--filter): reading its history could fetch objects from the network")
)

// Range is a push, as a pre-push hook receives it: full commit ids. Old is the remote's commit before the push (empty
// or all zeros for a new branch), New the pushed one. Names are not accepted, so no branch or tag can stand in.
type Range struct {
	Old, New string
}

// Options locate the default branch, whose merge base with the pushed commit is the screen's base arm.
type Options struct {
	// DefaultBranch is a full ref such as "refs/remotes/origin/main", resolved exactly (a tag of the same name never
	// wins). Empty: what <Remote>/HEAD points to, then <Remote>/HEAD, <Remote>/main and <Remote>/master.
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
	DefaultBranch string  `json:"default_branch"` // the ref Base was taken against
	// Start is where the pushed range starts: Range.Old when it is an ancestor of Head and not behind Base, otherwise
	// Base, and StartNote says why (a new branch, an unknown or force-pushed old commit, a merged default branch).
	Start     string `json:"start"`
	StartNote string `json:"start_note,omitempty"`
	// Pushed are the read-context files changed between Start and Head; Arms those that differ between Base and Head,
	// which is what a screen would compare.
	Pushed []Change `json:"pushed"`
	Arms   []Change `json:"arms"`
	// Harness are the reasons for refusal: every harness difference between the arms, so an earlier push that changed
	// the harness cannot hide behind a later push that changes only context; and the push's own findings on paths the
	// arms differ in (a harness change the default branch brought in, by a merge, does not differ between the arms
	// and never reaches a run).
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

// noLazyFetch is on every git call of the screen: a missing object fails the read instead of being fetched from a
// promisor remote (git 2.44 and later; older versions ignore it, and the partial-clone refusal covers them).
const noLazyFetch = "GIT_NO_LAZY_FETCH=1"

// Classify reads the push in the repository at repo. It is a context candidate only when the push changes context
// and the arms differ in context; any harness finding (see Result.Harness) refuses it. An old commit that is missing,
// all zeros, not an ancestor of the new one (a force push) or behind the merge base is replaced by the merge base:
// that is the screen's base arm anyway, so no claimed old commit can narrow what is checked. Errors mean the push
// could not be classified (an unknown pushed commit, no default branch, no shared history, a partial clone).
func Classify(ctx context.Context, repo string, pushed Range, opts Options) (Result, error) {
	if partial, err := gitx.PartialClone(ctx, repo); err != nil {
		return Result{}, fmt.Errorf("screen: %w", err)
	} else if partial {
		return Result{}, ErrPartialClone
	}
	c := &classifier{ctx: ctx, repo: repo, sides: map[string]*side{}}
	head, err := c.commitID(pushed.New)
	if err != nil {
		return Result{}, fmt.Errorf("screen: the pushed commit %q: %w", pushed.New, err)
	}
	branch, tip, err := c.defaultBranch(opts)
	if err != nil {
		return Result{}, err
	}
	base, err := c.git("merge-base", head, tip)
	if err != nil || base == "" {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("screen: %s and %s: %w", short(head), branch, ErrNoMergeBase)
	}
	res := Result{Head: head, Base: base, DefaultBranch: branch}
	res.Start, res.StartNote = c.pushedStart(pushed.Old, head, base)
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
	harness := slices.Clone(arms.harness)
	for _, f := range push.harness {
		if arms.changed[f.Path] {
			harness = append(harness, f)
		}
	}
	res.Harness = mergeFindings(harness)
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

type classifier struct {
	ctx   context.Context
	repo  string
	sides map[string]*side
}

// git runs git in the repository, with no lazy fetch, and returns trimmed stdout.
func (c *classifier) git(args ...string) (string, error) {
	out, err := gitx.OutputEnv(c.ctx, []string{noLazyFetch}, nil, append([]string{"-C", c.repo}, args...)...)
	return strings.TrimSpace(string(out)), err
}

var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// commitID checks that id is a full object id of a commit here (a full id is never read as a ref name).
func (c *classifier) commitID(id string) (string, error) {
	if !objectID.MatchString(id) {
		return "", ErrUnknownCommit
	}
	oid, err := c.git("rev-parse", "--verify", "--quiet", "--end-of-options", id+"^{commit}")
	if err != nil || oid == "" {
		if c.ctx.Err() != nil {
			return "", c.ctx.Err()
		}
		return "", ErrUnknownCommit
	}
	return oid, nil
}

var (
	remoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	fullRef    = regexp.MustCompile(`^refs/[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+$`)
)

// defaultBranch finds the default branch's full ref and commit. Refs are read exactly (symbolic-ref, then show-ref
// --verify), never through rev-parse's search, where refs/tags/refs/remotes/origin/HEAD would win over a missing
// refs/remotes/origin/HEAD and move the merge base wherever its author likes.
func (c *classifier) defaultBranch(opts Options) (string, string, error) {
	candidates := []string{opts.DefaultBranch}
	if opts.DefaultBranch == "" {
		remote := cmp.Or(opts.Remote, "origin")
		if !remoteName.MatchString(remote) {
			return "", "", fmt.Errorf("screen: remote %q: %w", remote, ErrNoDefaultBranch)
		}
		prefix := "refs/remotes/" + remote + "/"
		candidates = []string{prefix + "HEAD", prefix + "main", prefix + "master"}
		if target, err := c.git("symbolic-ref", "--quiet", prefix+"HEAD"); err == nil && strings.HasPrefix(target, prefix) {
			candidates = append([]string{target}, candidates...)
		}
	}
	for _, ref := range candidates {
		if !fullRef.MatchString(ref) || strings.Contains(ref, "..") {
			continue
		}
		oid, err := c.git("show-ref", "--verify", "--hash", ref)
		if err == nil {
			if commit, err := c.commitID(oid); err == nil {
				return ref, commit, nil
			}
		}
		if c.ctx.Err() != nil {
			return "", "", c.ctx.Err()
		}
	}
	return "", "", fmt.Errorf("screen: tried %s (give a full ref, such as refs/remotes/origin/main): %w",
		strings.Join(candidates, ", "), ErrNoDefaultBranch)
}

// pushedStart picks where the pushed range starts, and says why when it is not old.
func (c *classifier) pushedStart(old, head, base string) (string, string) {
	const fallback = ": the pushed range starts at the merge base with the default branch"
	if strings.Trim(old, "0") == "" {
		return base, "no old commit (a new branch)" + fallback
	}
	oid, err := c.commitID(old)
	if err != nil {
		return base, "the old commit is not a commit of this repository (a force push, or not fetched)" + fallback
	}
	if _, err := c.git("merge-base", "--is-ancestor", oid, head); err != nil {
		return base, "the old commit is not an ancestor of the new one (a force push)" + fallback
	}
	if _, err := c.git("merge-base", "--is-ancestor", oid, base); oid != base && err == nil {
		return base, "the old commit is behind the merge base (the default branch was merged in)" + fallback
	}
	return oid, ""
}

func short(oid string) string { return oid[:min(12, len(oid))] }

type diff struct {
	context []Change
	harness []Finding
	changed map[string]bool
}

// change is one path that differs between two commits.
type change struct {
	path, status string
}

// changes lists the paths that differ between from and to: plumbing only (no external diff, no text conversion,
// nothing checked out), and no rename detection, so a rename is a deletion plus an addition and both paths count.
func (c *classifier) changes(from, to string) ([]change, error) {
	out, err := gitx.OutputEnv(c.ctx, []string{noLazyFetch}, nil, "-C", c.repo, "diff-tree", "-r", "-z", "--no-renames", "--raw", from, to)
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
	d := diff{changed: map[string]bool{}}
	for _, ch := range changes {
		if err := c.ctx.Err(); err != nil {
			return diff{}, err
		}
		d.changed[ch.path] = true
		var found []Finding
		present := false
		for _, s := range []*side{a, b} {
			if source.Has(s.src, ch.path) {
				present = true
				found = append(found, s.harnessFindings(ch)...)
			}
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

func mergeFindings(findings []Finding) []Finding {
	seen := map[Finding]bool{}
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Bounds on what one commit may make the screen read. Past any of them the commit is blind, which refuses every change
// to its files, rather than being read slowly, held in memory or checked partly.
const (
	maxHarnessFiles     = 256     // harness files followed and read for what they name
	maxFrontmatterFiles = 1024    // Markdown files in .claude folders read for harness frontmatter
	maxBlobBytes        = 1 << 20 // one file read for its frontmatter or names
	maxNamesBytes       = 8 << 20 // all files read for names, together
)

// side is what one commit holds, as the screen sees it.
type side struct {
	src   source.Source
	sizes map[string]int64 // blob sizes, from ls-tree
	// kinds are the read-context files: claudectx's entries other than harness, and linked documents.
	kinds map[string]string
	// harness says why each file of the commit is harness (see harnessByName), including files reached through a
	// symbolic link that is harness and the targets of links whose frontmatter is harness.
	harness map[string]string
	// contextLinks are read-context files that are symbolic links: the files they reach are context too.
	contextLinks []reach
	// fields says why a Markdown file in a .claude folder has harness frontmatter (read through its links).
	fields map[string]string
	// names are the path tokens of harness files and of harness frontmatter (not bodies): a snapshot file they name
	// may run (a hook's script, an MCP server's program), so a change to it is a harness change.
	names map[string]map[string]bool
	// commands says why Claude Code may run commands at this commit (hooks, helpers), or is "": a command can find a
	// file without naming it, so then every change to a snapshot file that is not prose, or is executable, is harness.
	commands string
	// blind says why this commit's harness could not be checked in full; every change to its files is then refused.
	blind string
}

// side reads a commit once.
func (c *classifier) side(commit string) (*side, error) {
	if s := c.sides[commit]; s != nil {
		return s, nil
	}
	src, err := source.CommitEnv(c.ctx, []string{noLazyFetch}, commit, "-C", c.repo)
	if err != nil {
		return nil, fmt.Errorf("screen: read %s: %w", short(commit), err)
	}
	s := &side{src: src, kinds: map[string]string{}, harness: map[string]string{}, fields: map[string]string{},
		names: map[string]map[string]bool{}}
	if s.sizes, err = c.blobSizes(commit); err != nil {
		return nil, err
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return nil, fmt.Errorf("screen: the context of %s: %w", short(commit), err)
	}
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
		if err := c.ctx.Err(); err != nil {
			return nil, err
		}
		if why := s.harnessByName(p); why != "" {
			s.harness[p] = why
		}
	}
	if err := c.followHarnessLinks(s); err != nil {
		return nil, err
	}
	for _, p := range sortedKeys(s.kinds) {
		r, err := s.follow(p)
		if err != nil {
			return nil, err
		}
		if r != nil {
			s.contextLinks = append(s.contextLinks, *r)
		}
	}
	if err := c.readFrontmatter(s); err != nil {
		return nil, err
	}
	if err := c.readNames(s); err != nil {
		return nil, err
	}
	c.sides[commit] = s
	return s, nil
}

// blobSizes lists the size of every file of commit, so nothing too large is read into memory.
func (c *classifier) blobSizes(commit string) (map[string]int64, error) {
	out, err := gitx.OutputEnv(c.ctx, []string{noLazyFetch}, nil, "-C", c.repo, "ls-tree", "-r", "-z", "-l", commit)
	if err != nil {
		return nil, fmt.Errorf("screen: list %s: %w", short(commit), err)
	}
	sizes := map[string]int64{}
	for _, entry := range strings.Split(string(out), "\x00") { // "<mode> <type> <object> <size>\t<path>"
		meta, p, ok := strings.Cut(entry, "\t")
		if fields := strings.Fields(meta); ok && len(fields) == 4 {
			if n, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
				sizes[p] = n
			}
		}
	}
	return sizes, nil
}

// followHarnessLinks makes harness what a harness path that is a symbolic link reaches (.claude/hooks -> ../scripts
// makes every script a hook), and so on through further links, until nothing new is reached.
func (c *classifier) followHarnessLinks(s *side) error {
	queue := sortedKeys(s.harness)
	for len(queue) > 0 {
		if len(s.harness) > maxHarnessFiles {
			s.blind = fmt.Sprintf("more than %d harness files", maxHarnessFiles)
			return nil
		}
		link := queue[0]
		queue = queue[1:]
		r, err := s.follow(link)
		if err != nil {
			return err
		}
		if r == nil {
			continue
		}
		s.commands = cmp.Or(s.commands, "the harness path "+link+" is a symbolic link")
		for _, p := range s.src.Paths() {
			if err := c.ctx.Err(); err != nil {
				return err
			}
			if _, done := s.harness[p]; !done && r.reaches(p) {
				s.harness[p] = "reached through the symbolic link " + link + ", which is harness"
				queue = append(queue, p)
			}
		}
	}
	return nil
}

// readFrontmatter checks every Markdown file in a .claude folder (any letter case, any depth: skills, subagents and
// commands, and whatever Claude Code reads there) for harness frontmatter, read through its symbolic links. When a
// link's target declares it, the target and the links on the way are harness too, so editing the target is caught.
func (c *classifier) readFrontmatter(s *side) error {
	var candidates []string
	for _, p := range s.src.Paths() {
		if folded := foldPath(p); slices.Contains(strings.Split(folded, "/"), ".claude") && claudectx.IsDocumentExt(folded) {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) > maxFrontmatterFiles {
		s.blind = cmp.Or(s.blind, fmt.Sprintf("more than %d Markdown files in .claude folders", maxFrontmatterFiles))
		return nil
	}
	for _, p := range candidates {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		r, err := s.follow(p)
		if err != nil {
			return err
		}
		real := p
		if r != nil {
			if real = r.real; !source.Has(s.src, real) {
				continue // a link out of the repository or to a folder: Claude Code cannot load it as a file either
			}
		}
		why := ""
		var h claudectx.HarnessFields
		if s.sizes[real] > maxBlobBytes {
			why = fmt.Sprintf("larger than %d bytes, too large to check its frontmatter", maxBlobBytes)
		} else {
			data, err := s.src.ReadFile(p)
			if err != nil {
				continue // unreadable, so not loaded
			}
			switch h = claudectx.HarnessFrontmatter(data); {
			case h.Doubt != "":
				why = "its frontmatter cannot be read safely: " + h.Doubt
			case len(h.Fields) > 0:
				why = "its frontmatter declares " + strings.Join(h.Fields, ", ") + ", which change what runs"
			}
		}
		if why == "" {
			continue
		}
		s.fields[p] = why
		s.names[p] = pathTokens(h.Text)
		if h.Doubt != "" || slices.Contains(h.Fields, "hooks") || h.Text == "" {
			s.commands = cmp.Or(s.commands, p+" may define hooks in its frontmatter")
		}
		if r != nil {
			for _, q := range append(slices.Clone(r.hops[1:]), r.real) {
				if _, done := s.harness[q]; !done && source.Has(s.src, q) {
					s.harness[q] = "the target of the symbolic link " + p + ": " + why
				}
			}
		}
	}
	return nil
}

// readNames reads every harness file for the paths it names, and for whether it makes Claude Code run commands.
func (c *classifier) readNames(s *side) error {
	if s.blind != "" {
		return nil
	}
	var total int64
	for _, p := range sortedKeys(s.harness) {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		folded := "/" + foldPath(p)
		if strings.Contains(folded, "/.claude/hooks/") || strings.HasSuffix(folded, "/.claude/hooks") {
			s.commands = cmp.Or(s.commands, p+" is a hook script")
		}
		real := p
		if r, err := s.follow(p); err != nil {
			return err
		} else if r != nil {
			real = r.real
		}
		if !source.Has(s.src, real) {
			continue // a link to a folder or out of the repository names nothing itself; what it reaches is harness
		}
		if total += s.sizes[real]; s.sizes[real] > maxBlobBytes || total > maxNamesBytes {
			s.blind = fmt.Sprintf("%s is too large to check what it names", p)
			return nil
		}
		data, err := s.src.ReadFile(p)
		if err != nil {
			continue
		}
		s.names[p] = pathTokens(string(data))
		if base := path.Base(folded); strings.HasSuffix(base, ".json") && base != ".mcp.json" { // MCP servers never start in runs
			if why := runsCommands(data); why != "" {
				s.commands = cmp.Or(s.commands, p+" "+why)
			}
		}
	}
	return nil
}

// commandKeys are settings keys (normalized: lower case, no "-" or "_") whose values are commands Claude Code runs.
var commandKeys = []string{"hooks", "command", "statusline", "apikeyhelper", "awsauthrefresh", "awscredentialexport", "otelheadershelper", "filesuggestion"}

// runsCommands says why a settings file makes Claude Code run commands, or returns "". A file it cannot parse may.
func runsCommands(data []byte) string {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return "cannot be parsed, so it may define hooks"
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch v := v.(type) {
		case map[string]any:
			for k, inner := range v {
				if slices.Contains(commandKeys, strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(k))) || walk(inner) {
					return true
				}
			}
		case []any:
			return slices.ContainsFunc(v, walk)
		}
		return false
	}
	if walk(v) {
		return "defines hooks or helper commands"
	}
	return ""
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

// travels reports whether p reaches a screen's arm from this commit: snapshot files are the context (with what
// context links reach) and .claude folders. Other files come from the task's base, never from the push.
func (s *side) travels(p string) bool {
	if s.kinds[p] != "" || slices.Contains(strings.Split(foldPath(p), "/"), ".claude") {
		return true
	}
	return slices.ContainsFunc(s.contextLinks, func(r reach) bool { return r.reaches(p) })
}

// harnessFindings lists why ch, a file of this commit, is a harness change, as this commit sees it.
func (s *side) harnessFindings(ch change) []Finding {
	var found []Finding
	add := func(reason string) { found = append(found, Finding{Path: ch.path, Status: ch.status, Reason: reason}) }
	if s.blind != "" {
		add("the commit's harness could not be checked in full: " + s.blind)
	}
	if why := s.harness[ch.path]; why != "" {
		add(why)
	}
	if why := s.fields[ch.path]; why != "" {
		add(why)
	}
	// A link from context to harness (CLAUDE.md -> .claude/settings.json), or to a folder holding it. Link targets
	// are read only from the tree, and follow fails only when the context is cancelled, which compare reports.
	if r, err := s.follow(ch.path); err == nil && r != nil {
		for _, hop := range append(slices.Clone(r.hops[1:]), r.real) {
			if why := s.harnessAt(hop); hop != "" && why != "" {
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
	} else if err != nil {
		add("its symbolic link could not be read: " + err.Error())
	}
	if !s.travels(ch.path) {
		return found
	}
	for _, ref := range sortedKeys(s.names) {
		if ref != ch.path && names(s.names[ref], ch.path, ref) {
			add("named by " + ref + ", which is harness: the file may run")
		}
	}
	if s.commands != "" && (!claudectx.IsDocumentExt(foldPath(ch.path)) || s.executable(ch.path)) {
		add("Claude Code runs commands at this commit (" + s.commands + "), and a command can find and run a file that is not prose without naming it")
	}
	return found
}

// executable reports whether p, or the file its links lead to, is executable.
func (s *side) executable(p string) bool {
	if r, err := s.follow(p); err == nil && r != nil {
		p = r.real
	}
	return s.src.Executable(p)
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

// pathTokens are the path-like words of a text, once: split at spaces, quotes and shell or JSON punctuation, with
// leading "./" and "/" removed, and each word's every tail after a "/" too ($CLAUDE_PROJECT_DIR/scripts/x.sh names
// scripts/x.sh).
func pathTokens(text string) map[string]bool {
	tokens := map[string]bool{}
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("\"'`;|&<>(){}[],=:*?\\", r)
	}) {
		for strings.HasPrefix(word, "./") || strings.HasPrefix(word, "/") {
			word = strings.TrimPrefix(strings.TrimPrefix(word, "."), "/")
		}
		for word != "" {
			tokens[word] = true
			_, tail, ok := strings.Cut(word, "/")
			if !ok {
				break
			}
			word = tail
		}
	}
	return tokens
}

// names reports whether tokens (of the harness file ref) name p: by its path from the repository root, or from ref's
// own folder (a skill's hook naming ./run.sh beside it).
func names(tokens map[string]bool, p, ref string) bool {
	if tokens[p] {
		return true
	}
	dir := path.Dir(ref)
	return dir != "." && strings.HasPrefix(p, dir+"/") && tokens[strings.TrimPrefix(p, dir+"/")]
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
