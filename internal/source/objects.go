package source

import (
	"bytes"
	"context"
	"slices"
	"sync"
)

// maxKept bounds the blob contents one Objects keeps, in bytes. A blob that does not fit is read from git each time,
// as a source of Commit reads it.
const maxKept = 64 << 20

// Objects reads the commits of one repository so that each of git's objects is read once: a commit named by its full
// ID is listed once, and a blob of such a commit is read once, whichever commits and paths hold it (the bases of a
// project's tasks and its snapshots share most of their context files). Git never changes an object under its ID, and
// a read names its commit by that ID, so nothing kept here can go stale.
//
// A commit named any other way (a branch, HEAD) can move between its listing and a read, so its source is the one
// Commit gives: listed every time, each read asked of git, nothing kept and nothing taken from what is kept.
//
// What it keeps is in memory only, at most maxKept bytes of blobs, for as long as the value lives: one serves one
// command. It is safe for concurrent use; readers of one object wait for the first, so no object is read twice even
// then. A read that fails keeps nothing, and its error is the one a source of Commit would give.
type Objects struct {
	where []string // cap == len: sources append their arguments to it from several goroutines
	limit int      // of kept blob bytes

	mu      sync.Mutex // guards the maps and kept; never held during a git call
	commits map[string]*keptListing
	blobs   map[string]*keptBlob
	kept    int
}

// keptListing is a commit's listing once read. mu is held while it is read, so one reader reads and the rest wait.
type keptListing struct {
	mu    sync.Mutex
	files listing
	ok    bool
}

// keptBlob is a blob's content once read, under mu as a keptListing is.
type keptBlob struct {
	mu   sync.Mutex
	data []byte
	ok   bool
}

// NewObjects returns a reader of the repository located by where (for example "--git-dir", bare).
func NewObjects(where ...string) *Objects {
	return &Objects{where: slices.Clip(slices.Clone(where)), limit: maxKept, commits: map[string]*keptListing{}, blobs: map[string]*keptBlob{}}
}

// Commit reads commit as the package's Commit does; a commit named by its full ID shares what o keeps with o's other
// sources.
func (o *Objects) Commit(ctx context.Context, commit string) (Source, error) {
	files, pinned, err := o.list(ctx, commit)
	if err != nil {
		return nil, err
	}
	// Each source has its own paths, as a source of Commit has: a caller that sorts or cuts them changes no other.
	files.paths = slices.Clone(files.paths)
	src := &commitSource{ctx: ctx, where: o.where, commit: commit, listing: files}
	if pinned {
		// Its reads name the commit by its ID, so each gives the blob the listing names, for good. A name that can move
		// would give a later commit's file under the listed blob's ID, and every source would then read it from o.
		src.objects = o
	}
	return src, nil
}

// list is commit's listing, and whether commit is pinned: named by its full ID, which only then is kept. A name that
// has an ID's form but another length than the repository's IDs (which its blobs show) is a branch's or an
// abbreviation's: not pinned.
func (o *Objects) list(ctx context.Context, commit string) (files listing, pinned bool, err error) {
	if !hexID(commit) {
		files, err = list(ctx, nil, commit, o.where)
		return files, false, err
	}
	o.mu.Lock()
	k := o.commits[commit]
	if k == nil {
		k = &keptListing{}
		o.commits[commit] = k
	}
	o.mu.Unlock()
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ok && ctx.Err() == nil {
		return k.files, true, nil
	}
	// A cancelled ctx asks git all the same, which fails as it did without o: a cancelled command is given nothing.
	if files, err = list(ctx, nil, commit, o.where); err != nil {
		return files, false, err
	}
	if files.idLength() != len(commit) {
		return files, false, nil
	}
	k.files, k.ok = files, true
	return files, true, nil
}

// blob returns the content of the blob id: what o kept of it, else what read returns, which o then keeps if it fits. A
// nil o keeps nothing. The caller owns the result. As in list, a cancelled ctx reads through git.
func (o *Objects) blob(ctx context.Context, id string, read func() ([]byte, error)) ([]byte, error) {
	if o == nil || id == "" {
		return read()
	}
	o.mu.Lock()
	k := o.blobs[id]
	if k == nil {
		k = &keptBlob{}
		o.blobs[id] = k
	}
	o.mu.Unlock()
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ok && ctx.Err() == nil {
		return bytes.Clone(k.data), nil
	}
	data, err := read()
	if err == nil && !k.ok && o.room(len(data)) {
		k.data, k.ok = bytes.Clone(data), true
	}
	return data, err
}

// room reports whether n more bytes fit under the limit, and counts them when they do.
func (o *Objects) room(n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.kept+n > o.limit {
		return false
	}
	o.kept += n
	return true
}

// hexID reports whether name has the form of an object's full ID: SHA-1's or SHA-256's length, in lower-case hex. In a
// repository whose IDs have that length git reads such a name as the ID, even when a branch has the same name, so it
// names one object for good; list checks the length.
func hexID(name string) bool {
	if len(name) != 40 && len(name) != 64 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
