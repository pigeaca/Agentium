# `agentium clean`: free the space of caches nothing uses

- Date: 2026-10-03
- Status: Planned. The user asked (2026-10-03), after hearing what accumulates: "Yes" to adding a cleanup command.
- Scope: a new command that removes what Agentium keeps for reuse but no longer needs. Today nothing trims `~/.agentium/cache` and `~/.agentium/deps`: offline dependencies (Go modules, the Maven repository, Cargo, Python venvs per key), the grading seeds (one per project, tool set and base commit, #130) and the quarantine. Our pilots used 66–109 MB per project, growing with every new base commit and toolchain.

## Outcome
`agentium clean` shows what it would remove and how much space that frees; `agentium clean --yes` removes it. It runs only when called (no background process, the user's rule). It never touches the user's repositories, the database, run records and artifacts (reports need them) or saved snapshots.

## Rules
- **What may go:**
  - a grading seed or a per-base dependency folder whose base no task in the pool uses any more (removed or retired tasks), and that no locked, unfinished experiment uses;
  - one that is in use but has not been used for `--older-than` (default 30 days); it is warmed again on its next use;
  - the quarantine (what `Recover` could not remove), with the same safe removal (`removeTree`);
  - temporary and workspace folders of runs that are no longer running (as recovery finds them).
- **What stays:** anything a running run or validation uses. `clean` takes the run lock, so it never races a run. If another command holds it, it says so and exits 1.
- **Last use:** recorded when a seed or dependency folder is warmed or used (a stamp file, or its modification time if that is reliable), so age means "unused for", not "created".
- **Safety:**
  - every removal goes through `removeTree` (no link is followed);
  - only folders under the data folder's `cache` and `deps` are candidates, and anything else is refused;
  - the dry run is the default.

## Acceptance
1. **Without `--yes`:** a table by kind (seeds, dependencies, quarantine, leftovers) with counts and sizes, the total, and what is kept and why (in use by a task, by an experiment, or recently used). It writes nothing.
2. **With `--yes`:** it removes exactly what the dry run listed, and reports the space freed.
3. **Tests:**
   - in-use seeds and dependencies are kept;
   - unused ones and old ones are removed;
   - a locked, unfinished experiment's bases are kept;
   - the run lock is respected;
   - a link planted in a candidate is never followed;
   - a missing cache is not an error;
   - `--json` follows the headless contract.
4. **Docs:** the README's commands table and the guide (data folder section).
5. A real console sample on a data folder with pilot data.

## Boundaries
- No new modules, no paid runs, no background work.
- Package ownership: a new `internal/cli/clean.go` and a small service (in `internal/run` or a new `internal/cleanup`). Isolation step 3 (#131) is in flight in `internal/run` and `internal/cli`: avoid its files, and merge main before landing.

## Work
- [x] **1. The command** (risk: medium-high: it deletes data). Rules, the dry run, `--yes`, the stamps, tests, docs. Done: `internal/run/clean.go`, `clean_leftovers.go` and `internal/cli/clean.go`; last use is the seed folder's and the warm-up stamp's modification time (`markUsed`); nothing used in the last hour goes (validations take no run lock); removals go through the quarantine.

## Verification
- `python3 scripts/harness.py check changed`, the new tests, and a review with the threat checklist (crash and recovery, persistence, concurrent runs).

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
