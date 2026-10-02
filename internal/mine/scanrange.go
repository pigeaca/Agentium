package mine

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/pool"
	"github.com/pigeaca/agentium/internal/store"
)

// The task pool's scan (pool.Scanned's contract): a range of history after a watermark, read oldest first and bounded,
// with the candidates that are copies of tasks left out. Every git call goes through gitx (hooks and fsmonitor off) and
// with lazy fetching off, so a missing object is an error, never a network fetch.

// noLazyFetch keeps git from fetching a missing object from a promisor remote.
const noLazyFetch = "GIT_NO_LAZY_FETCH=1"

// RangeInput is what a range scan reads.
type RangeInput struct {
	Root  string // the user's repository
	Bare  string // Agentium's bare repository for the project, which holds the tasks' commits
	Range pool.ScanRange
	// Options bound the candidates as Scan's do; MaxCommits bounds the commits read inside the window. Since and
	// Exclude are ignored: the range's Since applies, and the tasks' commits are excluded.
	Options Options
	// Tasks are the project's tasks (retired ones included): their solution commits, and any commit whose change has the
	// same patch ID as one of theirs (a rebased or cherry-picked copy), are never offered.
	Tasks []store.Task
}

// RangeResult is a range scan's outcome: Scanned for the pool's pass, and Result for display (Candidates are the
// same; Scanned counts the commits read inside the window). Old counts the commits read outside it, which are never
// mined.
type RangeResult struct {
	Scanned pool.Scanned[Candidate]
	Result  Result
	Old     int
}

// ScanRange reads the commits reachable from in.Range.Head and from none of its Exclude commits (those the repository
// has; Unknown lists the others), streaming `git rev-list --topo-order --reverse` and stopping before the
// MaxCommits+1st commit inside the window. Each commit is judged by its own committer date: older ones are read past,
// not mined. (--max-count is never used: it applies before --reverse, and would read the newest commits instead of a
// prefix.) Through is the tips of what it read when it stopped early. The commits inside the window are classified as
// Scan classifies; dismissed commits, copies of tasks and copies of dismissed changes are set aside. Once ctx is done,
// the error wraps ctx.Err().
func ScanRange(ctx context.Context, in RangeInput) (RangeResult, error) {
	res, err := scanRange(ctx, in)
	if err != nil && ctx.Err() != nil {
		return RangeResult{}, fmt.Errorf("mine: %w", ctx.Err())
	}
	return res, err
}

func scanRange(ctx context.Context, in RangeInput) (RangeResult, error) {
	opts := withDefaults(in.Options)
	opts.Exclude = map[string]bool{}
	for _, t := range in.Tasks {
		if t.SolutionCommit != "" {
			opts.Exclude[t.SolutionCommit] = true
		}
	}
	r, root := in.Range, in.Root
	if !isHash(r.Head) {
		return RangeResult{}, fmt.Errorf("mine: the head %q is not a commit hash", r.Head)
	}
	shallow, err := gitx.Run(ctx, "-C", root, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return RangeResult{}, fmt.Errorf("mine: %w", err)
	}
	var out RangeResult
	known, unknown, err := commitsIn(ctx, r.Exclude, "-C", root)
	if err != nil {
		return RangeResult{}, err
	}
	out.Scanned.Unknown = unknown
	read, window, complete, err := readRange(ctx, root, r.Head, known, r.Since, opts.MaxCommits)
	if err != nil {
		return RangeResult{}, err
	}
	out.Scanned.Complete, out.Old = complete, len(read)-len(window)
	if !complete {
		out.Scanned.Through = tips(read)
	}
	commits, err := readCommits(ctx, root, window)
	if err != nil {
		return RangeResult{}, err
	}
	res := Result{Head: r.Head, Shallow: shallow == "true", Scanned: len(commits)}
	reject := func(c commit, rej Rejection) {
		rej.Hash, rej.Subject, rej.Date = c.hash, c.subject, c.date
		res.Rejected = append(res.Rejected, rej)
	}
	var cands []Candidate
	for _, c := range commits {
		cand, rej := classify(c, opts, res.Shallow)
		if rej == nil && slices.Contains(r.Dismissed, c.hash) {
			rej = &Rejection{Reason: ReasonDismissed, Detail: "a mined task of it was removed"}
		}
		if rej == nil {
			if rej, err = inspect(ctx, root, &cand); err != nil {
				return RangeResult{}, err
			}
		}
		if rej != nil {
			reject(c, *rej)
			continue
		}
		cands = append(cands, cand)
	}
	if cands, err = dropCopies(ctx, in, cands, reject, commits); err != nil {
		return RangeResult{}, err
	}
	newest, oldest := dateBounds(commits)
	for i := range cands {
		cands[i].Score, cands[i].Reasons = score(cands[i], newest, oldest)
	}
	slices.SortStableFunc(cands, compare)
	res.Candidates = cands
	out.Scanned.Candidates, out.Result = cands, res
	return out, nil
}

// dropCopies sets each candidate's patch ID and base date, and sets aside the candidates whose change is a task's (by
// patch ID: a rebase or cherry-pick gives a change a new commit, never a new patch ID) or a dismissed one's.
func dropCopies(ctx context.Context, in RangeInput, cands []Candidate, reject func(commit, Rejection), commits []commit) ([]Candidate, error) {
	if len(cands) == 0 {
		return cands, nil
	}
	pairs, parents := make([][2]string, len(cands)), make([]string, len(cands))
	for i, c := range cands {
		pairs[i], parents[i] = [2]string{c.Hash, c.Parent}, c.Parent
	}
	patches, err := PatchIDs(ctx, pairs, "-C", in.Root)
	if err != nil {
		return nil, err
	}
	bases, err := CommitTimes(ctx, parents, "-C", in.Root)
	if err != nil {
		return nil, err
	}
	taskPatches, err := TaskPatchIDs(ctx, in.Bare, in.Tasks)
	if err != nil {
		return nil, err
	}
	owner := map[string]string{} // a task's patch ID to the task's name
	for _, t := range in.Tasks {
		if p := taskPatches[t.SolutionCommit]; p != "" && owner[p] == "" {
			owner[p] = t.Name
		}
	}
	byHash := map[string]commit{}
	for _, c := range commits {
		byHash[c.hash] = c
	}
	kept := cands[:0]
	for _, c := range cands {
		c.Patch, c.BaseDate = patches[c.Hash], bases[c.Parent]
		switch {
		case c.Patch != "" && owner[c.Patch] != "":
			reject(byHash[c.Hash], Rejection{Reason: ReasonCopy, Detail: "the same change as task " + owner[c.Patch]})
		case c.Patch != "" && slices.Contains(in.Range.DismissedPatches, c.Patch):
			reject(byHash[c.Hash], Rejection{Reason: ReasonDismissed, Detail: "a copy of a change whose mined task was removed"})
		default:
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// dateBounds is the newest and oldest commit dates, which recency is scored within.
func dateBounds(commits []commit) (newest, oldest time.Time) {
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
	return newest, oldest
}

// listed is one commit of rev-list's output: what working out the tips needs, and nothing more (a first scan may read
// past a long history older than the window).
type listed struct {
	hash    string
	parents []string
}

// readRange streams the commits reachable from head and from none of exclude, oldest first in a topological order,
// until bound commits committed at or after since are read; it stops before the next such commit (older ones are read
// past: they are outside the window, so reading them handles them). read holds every commit read, window those inside
// the window; complete is whether the whole range was read.
func readRange(ctx context.Context, root, head string, exclude []string, since time.Time, bound int) (read []listed, window []string, complete bool, err error) {
	args := []string{"-C", root, "rev-list", "--topo-order", "--reverse", "--parents", "--timestamp", head}
	for _, c := range exclude {
		args = append(args, "^"+c)
	}
	args = append(args, "--")
	var bad error
	complete, err = gitx.Lines(ctx, []string{noLazyFetch}, func(line string) bool {
		fields := strings.Fields(line) // "<committer time> <hash> <parent>..."
		if len(fields) < 2 {
			bad = fmt.Errorf("mine: unexpected rev-list line %q", line)
			return false
		}
		secs, perr := strconv.ParseInt(fields[0], 10, 64)
		if perr != nil || !isHash(fields[1]) {
			bad = fmt.Errorf("mine: unexpected rev-list line %q", line)
			return false
		}
		at := time.Unix(secs, 0).UTC()
		inWindow := since.IsZero() || !at.Before(since)
		if inWindow && len(window) == bound {
			return false // the bound: this commit is the next pass's
		}
		read = append(read, listed{hash: fields[1], parents: fields[2:]})
		if inWindow {
			window = append(window, fields[1])
		}
		return true
	}, args...)
	if bad != nil {
		return nil, nil, false, bad
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("mine: read history: %w", err)
	}
	return read, window, complete, nil
}

// tips are the commits read that no other commit read has as a parent. As read is a prefix of a topological order,
// every commit of the range that a tip reaches was read.
func tips(read []listed) []string {
	parent := map[string]bool{}
	for _, c := range read {
		for _, p := range c.parents {
			parent[p] = true
		}
	}
	var out []string
	for _, c := range read {
		if !parent[c.hash] {
			out = append(out, c.hash)
		}
	}
	return out
}

// readCommits reads the log records (with per-file line counts) of the given commits, in that order.
func readCommits(ctx context.Context, root string, hashes []string) ([]commit, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	args := append(logArgs(root), "--no-walk=unsorted", "--stdin", "--")
	out, err := gitx.OutputEnv(ctx, []string{noLazyFetch}, strings.NewReader(strings.Join(hashes, "\n")+"\n"), args...)
	if err != nil {
		return nil, fmt.Errorf("mine: read history: %w", err)
	}
	return parseLog(out), nil
}

// hashPattern is a full SHA-1 or SHA-256 commit hash.
var hashPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func isHash(s string) bool { return hashPattern.MatchString(s) }

// commitsIn splits hashes into the commits the repository that where selects ("-C", root or "--git-dir", bare) has, and
// the others (unknown, not a commit, or not a full hash), each in the order given, without repeats. It asks `git
// cat-file --batch-check` once for all of them, which is `cat-file -e` for many objects: an unknown commit given to
// rev-list as ^C would fail it.
func commitsIn(ctx context.Context, hashes []string, where ...string) (known, unknown []string, err error) {
	var asked []string
	for _, h := range hashes {
		if isHash(h) && !slices.Contains(asked, h) {
			asked = append(asked, h)
		}
	}
	isCommit := map[string]bool{}
	if len(asked) > 0 {
		args := append(slices.Clone(where), "cat-file", "--batch-check=%(objectname) %(objecttype)")
		out, err := gitx.OutputEnv(ctx, []string{noLazyFetch}, strings.NewReader(strings.Join(asked, "\n")+"\n"), args...)
		if err != nil {
			return nil, nil, fmt.Errorf("mine: look up commits: %w", err)
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if len(lines) != len(asked) {
			return nil, nil, fmt.Errorf("mine: look up commits: %d answers for %d commits", len(lines), len(asked))
		}
		for i, h := range asked {
			isCommit[h] = lines[i] == h+" commit"
		}
	}
	for _, h := range hashes {
		switch {
		case slices.Contains(known, h) || slices.Contains(unknown, h):
		case isCommit[h]:
			known = append(known, h)
		default:
			unknown = append(unknown, h)
		}
	}
	return known, unknown, nil
}

// CommitTimes maps each of hashes that the repository has as a commit to its committer time; the others are left out.
func CommitTimes(ctx context.Context, hashes []string, where ...string) (map[string]time.Time, error) {
	times := map[string]time.Time{}
	known, _, err := commitsIn(ctx, hashes, where...)
	if err != nil || len(known) == 0 {
		return times, err
	}
	args := append(slices.Clone(where), "log", "--no-walk=unsorted", "--stdin", "--format=%H %ct", "--")
	out, err := gitx.OutputEnv(ctx, []string{noLazyFetch}, strings.NewReader(strings.Join(known, "\n")+"\n"), args...)
	if err != nil {
		return nil, fmt.Errorf("mine: read commit times: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		hash, secs, ok := strings.Cut(line, " ")
		n, err := strconv.ParseInt(secs, 10, 64)
		if ok && err == nil {
			times[hash] = time.Unix(n, 0).UTC()
		}
	}
	return times, nil
}

// PatchIDs maps each pair's first commit to the patch ID (`git patch-id --stable`) of the change from the pair's second
// commit (its base) to it, in the repository that where selects. Pairs whose commits the repository lacks, and empty
// changes, are left out. The diff is fixed on the command line (no renames, colour, external diff or text conversion; the
// Myers algorithm), so the same change gets the same ID in the user's repository and in Agentium's.
func PatchIDs(ctx context.Context, pairs [][2]string, where ...string) (map[string]string, error) {
	ids := map[string]string{}
	var all []string
	for _, p := range pairs {
		all = append(all, p[0], p[1])
	}
	known, _, err := commitsIn(ctx, all, where...)
	if err != nil {
		return nil, err
	}
	var stdin strings.Builder
	for _, p := range pairs {
		if slices.Contains(known, p[0]) && slices.Contains(known, p[1]) {
			fmt.Fprintf(&stdin, "%s %s\n", p[0], p[1])
		}
	}
	if stdin.Len() == 0 {
		return ids, nil
	}
	// core.quotePath and --text keep the configuration and attributes out of the patch: the user's repository may mark
	// files -diff or binary (go.sum -diff), or show non-ASCII paths unquoted, where Agentium's bare repository does not.
	args := append(append([]string{"-c", "core.quotePath=true"}, where...), "diff-tree", "--text", "--stdin", "-p", "--no-color", "--no-renames", "--no-ext-diff", "--no-textconv",
		"--diff-algorithm=myers")
	diff, err := gitx.OutputEnv(ctx, []string{noLazyFetch}, strings.NewReader(stdin.String()), args...)
	if err != nil {
		return nil, fmt.Errorf("mine: patch IDs: %w", err)
	}
	out, err := gitx.Output(ctx, bytes.NewReader(diff), append(slices.Clone(where), "patch-id", "--stable")...)
	if err != nil {
		return nil, fmt.Errorf("mine: patch IDs: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id, commit, ok := strings.Cut(line, " "); ok && isHash(id) && isHash(commit) {
			ids[commit] = id
		}
	}
	return ids, nil
}

// TaskPatchIDs maps the solution commit of each task that has one to its change's patch ID, read from Agentium's bare
// repository (from the task's base to its solution).
func TaskPatchIDs(ctx context.Context, bare string, tasks []store.Task) (map[string]string, error) {
	var pairs [][2]string
	for _, t := range tasks {
		if t.SolutionCommit != "" && t.BaseCommit != "" {
			pairs = append(pairs, [2]string{t.SolutionCommit, t.BaseCommit})
		}
	}
	if len(pairs) == 0 {
		return map[string]string{}, nil
	}
	return PatchIDs(ctx, pairs, "--git-dir", bare)
}

// TreeHas reports which of paths are files in commit's tree, in the repository that where selects. Paths are literal
// (no pathspec magic or globs).
func TreeHas(ctx context.Context, commit string, paths []string, where ...string) (map[string]bool, error) {
	has := map[string]bool{}
	if len(paths) == 0 {
		return has, nil
	}
	args := append(append([]string{"--literal-pathspecs"}, where...), "ls-tree", "-r", "-z", "--name-only", "--full-tree", commit, "--")
	out, err := gitx.OutputEnv(ctx, []string{noLazyFetch}, nil, append(args, paths...)...)
	if err != nil {
		return nil, fmt.Errorf("mine: list the files of %s: %w", commit, err)
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			has[p] = true
		}
	}
	return has, nil
}

// Contains reports whether commit is in head's history in the user's repository at root. A commit the repository does
// not have is not.
func Contains(ctx context.Context, root, head, commit string) (bool, error) {
	if !isHash(commit) {
		return false, nil
	}
	_, err := gitx.OutputEnv(ctx, []string{noLazyFetch}, nil, "-C", root, "merge-base", "--is-ancestor", commit, head)
	if ctx.Err() != nil {
		return false, fmt.Errorf("mine: %w", ctx.Err())
	}
	return err == nil, nil
}
