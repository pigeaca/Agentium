package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/source"
)

// DefaultMaxHunks is how many reference hunks the weak-tests check tries unless told otherwise.
const DefaultMaxHunks = 20

// ErrNoSolution means a task has no solution (or no reference files) whose hunks could be removed.
var ErrNoSolution = errors.New("the task has no solution with reference changes to check")

// Hunk is one contiguous change of the reference solution, from `git diff -U0`. Start and End are line numbers
// (inclusive) in the solution's file, or in the base's file when Deleted (the solution has no lines there).
type Hunk struct {
	File    string `json:"file"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Deleted bool   `json:"deleted,omitempty"`

	// Where the hunk sits in each side, for rebuilding the file without it.
	baseStart, baseCount, newStart, newCount int
}

// String is "file:12-14", or "file:12" for one line; a deleted range says so.
func (h Hunk) String() string {
	text := h.File + ":" + strconv.Itoa(h.Start)
	if h.End > h.Start {
		text += "-" + strconv.Itoa(h.End)
	}
	if h.Deleted {
		text += " (lines the solution deletes)"
	}
	return text
}

// WeakTests is the outcome of the weak-tests check: which parts of the reference solution the hidden tests do not need.
// Absent from a Validation means it was not checked.
type WeakTests struct {
	Checked  int    `json:"checked"`             // hunks removed one at a time and tried
	Skipped  int    `json:"skipped,omitempty"`   // hunks past the cap, not tried
	Untested []Hunk `json:"untested,omitempty"`  // removing the hunk still passed: nothing in the hidden tests needs it
	TimedOut int    `json:"timed_out,omitempty"` // of the checked hunks, how many timed out (counted as tested)
	Reason   string `json:"reason,omitempty"`    // why nothing was checked, when that is so
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// Hunks lists the hunks of each reference file's change between base and solution, in file then line order. Binary
// files have none, and are not checked.
func Hunks(ctx context.Context, base, solution string, files []string, where ...string) ([]Hunk, error) {
	var hunks []Hunk
	for _, file := range files {
		args := append(append([]string{}, where...), "--literal-pathspecs", "diff", "-U0", "--no-color", "--no-ext-diff",
			"--no-textconv", "--no-renames", base, solution, "--", file)
		out, err := gitx.Output(ctx, nil, args...)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(out), "\n") {
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			num := func(s string, def int) int {
				if s == "" {
					return def
				}
				n, _ := strconv.Atoi(s)
				return n
			}
			h := Hunk{File: file, baseStart: num(m[1], 0), baseCount: num(m[2], 1), newStart: num(m[3], 0), newCount: num(m[4], 1)}
			if h.newCount > 0 {
				h.Start, h.End = h.newStart, h.newStart+h.newCount-1
			} else {
				h.Start, h.End, h.Deleted = h.baseStart, h.baseStart+h.baseCount-1, true
			}
			hunks = append(hunks, h)
		}
	}
	return hunks, nil
}

// splitLines keeps each line's terminator, so joining them gives the text back.
func splitLines(text string) []string {
	var lines []string
	for text != "" {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			lines, text = append(lines, text), ""
			break
		}
		lines, text = append(lines, text[:i+1]), text[i+1:]
	}
	return lines
}

// without rebuilds the solution's file with hunk h undone: the solution's lines for the hunk are replaced by the
// base's. -U0 hunks do not overlap, so the rest of the solution's file is left as it is.
func (h Hunk) without(solutionText, baseText string) string {
	sol, base := splitLines(solutionText), splitLines(baseText)
	start := h.newStart - 1 // 0-based first changed line; for a pure deletion newStart is the line before the gap
	if h.newCount == 0 {
		start = h.newStart
	}
	restored := []string{}
	if h.baseCount > 0 {
		restored = base[h.baseStart-1 : h.baseStart-1+h.baseCount]
	}
	out := append(append(append([]string{}, sol[:start]...), restored...), sol[start+h.newCount:]...)
	return strings.Join(out, "")
}

// weakTests removes one hunk of the reference at a time and reruns the verification in the base's own context. spec's
// hidden tests and reference are written as in the reference stage; only the one hunk is undone. A hunk whose removal
// still passes is untested. Cancellation and checkouts that cannot be made are errors, not results.
func (v Validator) weakTests(ctx context.Context, spec Spec, solution source.Source) (*WeakTests, error) {
	if solution == nil || len(spec.Reference) == 0 || len(spec.HiddenTests) == 0 {
		return nil, ErrNoSolution
	}
	hunks, err := Hunks(ctx, spec.Base, spec.Solution, spec.Reference, "--git-dir", v.Bare)
	if err != nil {
		return nil, err
	}
	base, err := source.Commit(ctx, spec.Base, "--git-dir", v.Bare)
	if err != nil {
		return nil, err
	}
	result := &WeakTests{}
	limit := v.MaxHunks
	if limit <= 0 {
		limit = DefaultMaxHunks
	}
	if len(hunks) > limit {
		result.Skipped = len(hunks) - limit
		hunks = hunks[:limit]
	}
	for i, h := range hunks {
		if v.Started != nil {
			v.Started("base", fmt.Sprintf("weak tests %d/%d", i+1, len(hunks)))
		}
		passed, timeout, err := v.tryWithout(ctx, spec, h, i+1, base, solution)
		if err != nil {
			return result, err
		}
		result.Checked++
		verdict := "tested"
		switch {
		case timeout:
			result.TimedOut++
			verdict = "tested (timed out)"
		case passed:
			result.Untested = append(result.Untested, h)
			verdict = "NOT TESTED"
		}
		if v.Progress != nil {
			fmt.Fprintf(v.Progress, "  hunk %-3s %-40s without it: %s\n", fmt.Sprintf("%d/%d", i+1, len(hunks)), h, v.Style.Status(verdict))
		}
	}
	return result, nil
}

// tryWithout runs the verification in a fresh checkout prepared like the reference stage, with hunk h undone.
func (v Validator) tryWithout(ctx context.Context, spec Spec, h Hunk, n int, base, solution source.Source) (passed, timeout bool, err error) {
	label := fmt.Sprintf("base-weak-%d", n)
	dir := filepath.Join(v.WorkDir, label)
	if err := checkout.New(ctx, v.Bare, spec.Base, dir); err != nil {
		return false, false, err
	}
	if !v.Keep {
		defer os.RemoveAll(dir)
	}
	log, err := os.Create(filepath.Join(v.LogDir, label+".log"))
	if err != nil {
		return false, false, fmt.Errorf("validation log: %w", err)
	}
	defer log.Close()
	fmt.Fprintf(log, "[agentium] reference without %s\n", h)
	if len(spec.Setup) > 0 {
		if _, ok, err := v.run(ctx, log, dir, spec.Setup); err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("weak tests: setup failed for hunk %s (see %s)", h, log.Name())
			}
			return false, false, err
		}
	}
	files := append(append([]string{}, spec.HiddenTests...), spec.Reference...)
	if err := checkout.Write(dir, solution, files); err != nil {
		return false, false, fmt.Errorf("weak tests, %s: %w", h, err)
	}
	if err := h.undo(dir, base, solution); err != nil {
		return false, false, err
	}
	commands, passed, err := v.run(ctx, log, dir, spec.Verify)
	if err != nil {
		return false, false, err
	}
	return passed, timedOut(commands), nil
}

// undo rewrites the hunk's file in dir without the hunk (deleting it when that leaves a file the base did not have).
func (h Hunk) undo(dir string, base, solution source.Source) error {
	var solText, baseText string
	if source.Has(solution, h.File) {
		b, err := solution.ReadFile(h.File)
		if err != nil {
			return fmt.Errorf("weak tests, %s: %w", h, err)
		}
		solText = string(b)
	}
	if source.Has(base, h.File) {
		b, err := base.ReadFile(h.File)
		if err != nil {
			return fmt.Errorf("weak tests, %s: %w", h, err)
		}
		baseText = string(b)
	}
	full := filepath.Join(dir, filepath.FromSlash(h.File)) // checkout.Write already vetted the path
	if !source.Has(base, h.File) {
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) { // a new file: dropping it is the hunk
			return fmt.Errorf("weak tests, %s: %w", h, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("weak tests, %s: %w", h, err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(full); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(full, []byte(h.without(solText, baseText)), mode); err != nil {
		return fmt.Errorf("weak tests, %s: %w", h, err)
	}
	return nil
}
