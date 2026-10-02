package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Forms are p cleaned and, when different, its real form (RealForm): the macOS sandbox matches real paths, Claude
// Code's Read tool the path as written. Agent runs (internal/claude) and grading profiles list both.
func Forms(p string) []string {
	out := []string{filepath.Clean(p)}
	if real := RealForm(out[0]); real != out[0] {
		out = append(out, real)
	}
	return out
}

// WithForms lists every path's forms (see Forms), each once, in order.
func WithForms(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		for _, form := range Forms(p) {
			if !seen[form] {
				seen[form] = true
				out = append(out, form)
			}
		}
	}
	return out
}

// RealForm is p as the macOS sandbox matches it: symbolic links resolved in the longest existing prefix, and the
// missing tail appended as written, so a denied path that a warm-up or another run creates after the sandboxed process
// starts is still denied where it will lie (Claude Code itself lists only the unresolved form of a path that does not
// exist, or of one that resolves elsewhere, and the sandbox ignores a deny on a form that is not the real one).
//
// Below /tmp (or /private/tmp) the entry directly in it decides, by its owner (tmpForm). /tmp is sticky: only an
// entry's owner (or root) can remove or replace it, but any local user can create a name not taken yet, as a link too
// (/tmp/claude and /tmp/cc-socks carry no uid). Following another user's link would deny its target, anything they
// chose, which can make a run refuse to start; resolving through another user's folder races them swapping it for
// such a link between two looks. So only the user's own entries and root's are resolved through.
func RealForm(p string) string {
	for _, tmp := range []string{"/tmp", "/private/tmp"} {
		rel, err := filepath.Rel(tmp, p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		name, rest, _ := strings.Cut(rel, string(filepath.Separator))
		owner, exists := entryOwner(filepath.Join(tmp, name))
		return tmpForm(p, tmp, name, rest, exists, owner, os.Getuid())
	}
	return resolvedPrefix(p)
}

// tmpForm is the real form of p, the entry name in tmp (/tmp or /private/tmp) followed by rest, given who owns the
// entry: the user uid's own entry or root's is resolved through, like any path (its owner alone can replace it); for
// another user's entry, or a missing one, only tmp itself (the system's /tmp → /private/tmp link) is resolved, and the
// rest is kept as written, so neither a link that user made nor one they make after this look is followed.
func tmpForm(p, tmp, name, rest string, exists bool, owner uint32, uid int) string {
	if exists && (owner == 0 || int64(owner) == int64(uid)) {
		return resolvedPrefix(p)
	}
	return filepath.Join(resolvedPrefix(tmp), name, rest)
}

// entryOwner is the uid owning path itself (a link is not followed), and whether it exists. An entry whose owner
// cannot be told is reported missing, so it is not resolved through.
func entryOwner(path string) (uint32, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// resolvedPrefix resolves symbolic links in the longest existing prefix of p and appends the rest as written.
func resolvedPrefix(p string) string {
	var missing []string
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, missing...)...)
		}
		missing = append([]string{filepath.Base(p)}, missing...)
		p = parent
	}
}
