package run

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/source"
)

// ContextUse is what a run used of its arm's context. Records keep only what the repository itself defines: paths
// relative to it and the project's own skill names. Local paths, other skill names and what the agent said stay out.
// Subagent types are kept, as Metrics.SubagentModels keeps them.
type ContextUse struct {
	// Start are the files Claude Code loads whole at session start: the instruction file, its imports and unscoped
	// rules. Skill, subagent and command descriptions load at start too, but not their bodies, so they are not here.
	Start []string `json:"start"`
	// Files are the context files that load on demand, and the documents the context links to, that the agent read:
	// with the Read tool, or by naming them in a shell command (cat, sed, head...).
	Files     []string `json:"files,omitempty"`
	Skills    []string `json:"skills,omitempty"`    // the project's own skills it invoked with the Skill tool
	Subagents []string `json:"subagents,omitempty"` // the subagent types it started
}

// UseOf works out a run's context use from its transcript's metrics and the context its arm started with (resolved
// from src). dirs are the folders the agent's file tools name paths under: its checkout, and the working folder its
// transcript reports.
func UseOf(resolved claudectx.Context, src source.Source, m claude.Metrics, dirs ...string) ContextUse {
	use := ContextUse{Start: []string{}}
	onDemand := map[string]bool{}
	for _, e := range resolved.Entries {
		switch e.Kind {
		case claudectx.KindInstructions, claudectx.KindImport, claudectx.KindRule:
			use.Start = append(use.Start, e.Path)
		case claudectx.KindHarness: // changes what runs, not what the model reads
		default:
			onDemand[e.Path] = true
		}
	}
	for _, p := range resolved.Linked {
		onDemand[p] = true
	}
	read := map[string]bool{}
	for _, p := range m.ReadPaths {
		if rel, ok := relativeTo(p, dirs); ok && onDemand[rel] {
			read[rel] = true
		}
	}
	for p := range onDemand {
		if !read[p] && namedIn(p, m.Commands) {
			read[p] = true
		}
	}
	for p := range read {
		use.Files = append(use.Files, p)
	}
	project := claudectx.SkillNames(resolved, src)
	for _, name := range m.SkillCalls {
		if slices.Contains(project, name) && !slices.Contains(use.Skills, name) {
			use.Skills = append(use.Skills, name)
		}
	}
	use.Subagents = slices.Clone(m.SubagentTypes)
	sort.Strings(use.Start)
	sort.Strings(use.Files)
	sort.Strings(use.Skills)
	return use
}

// Recovery works out the context use of runs recorded before records kept it, from each run's transcript and its arm's
// starting context: the task's base commit, with the arm's snapshot applied when it has one, both in the bare
// repository Bare. One value serves one command: it keeps each base and snapshot's resolved context, which every run of
// a task in that arm shares.
type Recovery struct {
	Bare     string
	contexts map[[2]string]armStart
}

type armStart struct {
	src      source.Source
	resolved claudectx.Context
}

// Recover returns rec's context use: the record's own when it has one, else one worked out from its transcript. A run
// whose transcript is gone gets nil and no error.
func (r *Recovery) Recover(ctx context.Context, rec Record, base, snapshotCommit string) (*ContextUse, error) {
	if rec.ContextUse != nil {
		return rec.ContextUse, nil
	}
	if rec.RecordsDir == "" {
		return nil, nil
	}
	m, err := parseFile(filepath.Join(rec.RecordsDir, "stream.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", rec.ID, err)
	}
	start, err := r.start(ctx, base, snapshotCommit)
	if err != nil {
		return nil, fmt.Errorf("run %s, arm %s: %w", rec.ID, rec.Arm, err)
	}
	use := UseOf(start.resolved, start.src, m, m.CWD)
	return &use, nil
}

// start resolves the context a run started with: base with snapshotCommit applied (none: base's own).
func (r *Recovery) start(ctx context.Context, base, snapshotCommit string) (armStart, error) {
	key := [2]string{base, snapshotCommit}
	if s, ok := r.contexts[key]; ok {
		return s, nil
	}
	src, err := source.Commit(ctx, base, "--git-dir", r.Bare)
	if err != nil {
		return armStart{}, err
	}
	if snapshotCommit != "" {
		snap, err := source.Commit(ctx, snapshotCommit, "--git-dir", r.Bare)
		if err != nil {
			return armStart{}, err
		}
		if src, err = snapshot.Apply(src, snap); err != nil {
			return armStart{}, err
		}
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return armStart{}, err
	}
	if r.contexts == nil {
		r.contexts = map[[2]string]armStart{}
	}
	r.contexts[key] = armStart{src: src, resolved: resolved}
	return r.contexts[key], nil
}

// relativeTo returns p relative to the first of dirs that holds it, with forward slashes.
func relativeTo(p string, dirs []string) (string, bool) {
	p = filepath.Clean(p)
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if rel, ok := strings.CutPrefix(p, filepath.Clean(dir)+string(filepath.Separator)); ok {
			return filepath.ToSlash(rel), true
		}
	}
	return "", false
}

// namedIn reports whether a shell command names p as a whole path: docs/a.md, ./docs/a.md or /any/folder/docs/a.md.
// A longer name that only contains it (docs/a.mdx, my-docs/a.md) does not count.
func namedIn(p string, commands []string) bool {
	pattern := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(p) + `($|[^A-Za-z0-9_./-])`)
	for _, c := range commands {
		if pattern.MatchString(c) {
			return true
		}
	}
	return false
}
