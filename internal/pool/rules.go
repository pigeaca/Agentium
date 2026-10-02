// Package pool keeps a project's task pool fresh: it mines new tasks after each pull, validates them, re-validates
// stale ones and retires dead ones, and never starts an agent. This file holds the rules, which are pure: the clock,
// the toolchain versions, the base commits' times and the default branch's files are parameters.
package pool

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// Day is a pool day: 24 hours of the UTC clock.
const Day = 24 * time.Hour

// Policy is when a validation goes stale and a task retires. A1's agentium.toml may set these later; DefaultPolicy is
// what the user chose (2026-10-02).
type Policy struct {
	StaleAfter      time.Duration // a validation older than this is made again
	FlakyRetryAfter time.Duration // a flaky task not tried for longer than this is tried again
	FlakyRepeat     int           // the runs per stage a flaky task is tried again with
	RetireAge       time.Duration // a task whose base is at least this old retires
	Limit           int           // the tasks a pass imports at most
}

// DefaultPolicy is the user's choice: stale after 30 days, flaky tasks retried weekly with --repeat 3, retirement at
// a base 270 or more days old, and up to 10 imports per pass.
func DefaultPolicy() Policy {
	return Policy{StaleAfter: 30 * Day, FlakyRetryAfter: 7 * Day, FlakyRepeat: 3, RetireAge: 270 * Day, Limit: 10}
}

// Staleness says why a task's last validation should be made again, and with what. The re-validation keeps the last
// one's arms (a task validated in a snapshot's context must stay validated there to stay eligible for its
// experiments) and its repeats.
type Staleness struct {
	Reasons []string // empty: not stale
	Arms    []task.Arm
	Repeat  int
	// WeakTests is the last weak-tests result, which a re-validation without that check (the pool never runs it) would
	// otherwise drop; nil when it was never checked.
	WeakTests *task.WeakTests
}

// Stale reports whether t's last validation should be made again at now, given the build tools' versions now
// (current; nil when they were not detected, which disables that rule). A task never validated is not stale (the
// pass validates new tasks on its own), nor is a retired one. Reasons:
//   - the validation is more than StaleAfter old (exactly StaleAfter is not);
//   - a tool it recorded reports another version now. A tool recorded then and not found now is no reason: a
//     validation without it could only fail, and would replace a good one because of the environment, not the task;
//   - it is flaky and was last tried more than FlakyRetryAfter ago. A flaky task is tried with at least FlakyRepeat
//     runs per stage, whatever made it stale.
func (p Policy) Stale(t store.Task, now time.Time, current task.Toolchain) Staleness {
	if t.Retired() || t.Validation == nil {
		return Staleness{}
	}
	v := task.ValidationOf(t)
	out := Staleness{Arms: v.Arms, Repeat: v.RepeatCount(), WeakTests: v.WeakTests}
	if len(out.Arms) == 0 {
		out.Arms = []task.Arm{{Name: "base"}}
	}
	if v.Status == "" {
		out.Reasons = append(out.Reasons, "its stored validation is unreadable")
	}
	age := now.Sub(v.At)
	if age > p.StaleAfter {
		out.Reasons = append(out.Reasons, fmt.Sprintf("validated on %s, more than %d days ago", v.At.UTC().Format(time.DateOnly), days(p.StaleAfter)))
	}
	if current != nil {
		for _, tool := range slices.Sorted(maps.Keys(v.Toolchain)) {
			if is, was := current[tool], v.Toolchain[tool]; is != "" && is != was {
				out.Reasons = append(out.Reasons, fmt.Sprintf("%s is %s now, %s then", tool, is, was))
			}
		}
	}
	if v.Status == task.StatusFlaky {
		if age > p.FlakyRetryAfter {
			out.Reasons = append(out.Reasons, fmt.Sprintf("flaky, last tried on %s (retried every %d days)", v.At.UTC().Format(time.DateOnly), days(p.FlakyRetryAfter)))
		}
		out.Repeat = max(out.Repeat, p.FlakyRepeat)
	}
	if len(out.Reasons) == 0 {
		return Staleness{}
	}
	return out
}

// Head is what the retirement rules read of the repository: whether the default branch's head has a file, whether
// its history contains a commit, and whether a commit's tree has a file.
type Head struct {
	Has      func(path string) bool
	Contains func(commit string) bool
	InCommit func(commit, path string) bool
}

// Retire says why t should retire at now, or "" when it should not (or is retired already). base is its base commit's
// time (zero when unknown, which disables that rule); a nil function in head disables the file rule.
//   - Its base commit is at least RetireAge old (exactly RetireAge retires).
//   - A file it names (a hidden test or a reference file) that the solution commit has is gone from the default
//     branch's head. The task's lists come from `git diff --name-only --no-renames base solution`, so they also name
//     the files the solution deleted and the old path of every rename: those were never meant to exist after it, and
//     count for nothing. The rule applies only when the head's history contains the solution commit: a task taken
//     from a branch that was never merged has files the default branch never had, which is no sign the code moved on.
func (p Policy) Retire(t store.Task, now, base time.Time, head Head) string {
	if t.Retired() {
		return ""
	}
	if !base.IsZero() {
		if age := now.Sub(base); age >= p.RetireAge {
			return fmt.Sprintf("its base commit is %d days old (%d or more retire)", days(age), days(p.RetireAge))
		}
	}
	if head.Has == nil || head.Contains == nil || head.InCommit == nil || t.SolutionCommit == "" || !head.Contains(t.SolutionCommit) {
		return ""
	}
	var gone []string
	for _, f := range append(slices.Clone(t.HiddenTests), t.Reference...) {
		if head.InCommit(t.SolutionCommit, f) && !head.Has(f) {
			gone = append(gone, f)
		}
	}
	switch len(gone) {
	case 0:
		return ""
	case 1:
		return gone[0] + " is gone from the default branch"
	}
	return fmt.Sprintf("%s and %d more files it names are gone from the default branch", gone[0], len(gone)-1)
}

// days is d in whole days, rounded down.
func days(d time.Duration) int {
	return int(d / Day)
}

// Health counts a project's tasks for pool status. Retired tasks count only as retired; the others are counted by
// their last validation (Valid, Flaky, Invalid, Unchecked and Unvalidated add up to Total minus Retired). Weak is the
// part of Valid with a weak-tests warning, and AwaitingReview the active tasks whose instruction needs a review,
// whatever their validation. The JSON keys are a public contract once pool status prints them (A1).
type Health struct {
	Total          int `json:"total"`
	Valid          int `json:"valid"`
	Weak           int `json:"weak"`
	Flaky          int `json:"flaky"`
	Invalid        int `json:"invalid"`
	Unchecked      int `json:"unchecked"`
	Unvalidated    int `json:"unvalidated"`
	AwaitingReview int `json:"awaiting_review"`
	Retired        int `json:"retired"`
	// LastPass is when the last pass ended (State.LastPass); OldestValidBase the oldest base commit time among the
	// active valid tasks. Zero (absent from JSON) when there is none.
	LastPass        time.Time `json:"last_pass,omitzero"`
	OldestValidBase time.Time `json:"oldest_valid_base,omitzero"`
}

// HealthOf counts tasks. baseTime gives a base commit's time (zero when unknown); lastPass is State.LastPass.
func HealthOf(tasks []store.Task, baseTime func(commit string) time.Time, lastPass time.Time) Health {
	h := Health{Total: len(tasks), LastPass: lastPass}
	for _, t := range tasks {
		if t.Retired() {
			h.Retired++
			continue
		}
		if t.NeedsReview {
			h.AwaitingReview++
		}
		switch v := task.ValidationOf(t); {
		case t.Validation == nil:
			h.Unvalidated++
		case v.Status == task.StatusValid:
			h.Valid++
			if v.WeakTests != nil && len(v.WeakTests.Untested) > 0 {
				h.Weak++
			}
			if at := baseTime(t.BaseCommit); !at.IsZero() && (h.OldestValidBase.IsZero() || at.Before(h.OldestValidBase)) {
				h.OldestValidBase = at
			}
		case v.Status == task.StatusFlaky:
			h.Flaky++
		case v.Status == task.StatusUnchecked:
			h.Unchecked++
		default: // invalid, or unreadable
			h.Invalid++
		}
	}
	return h
}
