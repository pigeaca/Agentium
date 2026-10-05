# A fast report and task list

- Date: 2026-10-05
- Status: Done (2026-10-05): #173. Both reviews clean after one round of fixes. The cache the handoff asks about was approved the same day and is its own plan (`2026-10-05-task-gaps-cache.md`).
- Scope/approval: the user, 2026-10-05: "two everyday read-only commands are slow on real data. Find out why with a profile, then make them fast without changing any output." A cache must be keyed so it can never serve a stale answer and live in the data folder; a design choice (such as storing recovered context use back on the run) needs the user's answer first.

## Outcome and boundaries
`agentium experiment report NAME` and `agentium task list` answer in about a second on a real data folder instead of 7 to 13 seconds, with every output byte for byte as before.

**The profile** (a copy of a real data folder: 8 experiments, 84 stored runs, 17 tasks; the machine was busy, load about 40, so times are upper bounds; a wall-clock sampler of the main goroutine plus a timer on every git call):

| Command | Wall | Git processes | Where the time went |
|---|---|---|---|
| `experiment report ab16 --json` | 12.2 s | 1,297 (96% of the wall) | 99.3% in `report.loadRuns` → `run.Recovery.Recover`; 95% of it resolving each arm's starting context, one `git cat-file` per file read |
| `experiment report aa --json` | 2.0 s | 157 | the same path: 96.6% in `Recover` |
| `task list --json` | 8.4 s | 620 (97%) | 99.8% in `task.Gaps`: 268 `git grep`, 323 `git cat-file`, 28 `git ls-tree` |
| `task list` (piped table) | 7.9 s | 534 | the same, without the second pass the JSON form makes |

- **The report reads the same few files over and over.** Its 1,264 `git cat-file` calls read 90 different `commit:path` pairs, which are 23 different blobs: the 8 task bases and the 2 snapshots share their context files. The 32 `git ls-tree` calls list 10 commits. `Recovery` already keeps each resolved context per base, snapshot and module, but `ab16` has one run per task and arm, so nothing repeats at that level; and one resolution reads each file several times (`snapshot.PlanOverlayIn` resolves the context, then `claudectx.ResolveIn` does again; imports, classification and linked documents each read the file).
- **Not the cause:** reading the transcripts again (0.06 s for 16 runs), the bootstrap and the rest of the analysis (0.06 s, both analyses together), `LoadNorthStar` (0.04 s) and `report.Build` (0.03 s).
- **The task list's git calls are mostly different from each other.** All 268 searches differ (213 look for a hidden test's text in the task's reference files, 37 search a whole base, 18 look for a name); the 323 reads are 164 different blobs; the JSON form works out the gaps of a task that awaits a review twice. The work is one git process after another, about 13 ms each on the busy machine.

**The fix** keeps every git command and every decision as it is, and changes only how often and in what order git is asked:
- `internal/source`: `Objects`, one per command, reads a repository's commits so that each object is read once. A commit named by its full ID is listed once; a blob is read once, whatever commits and paths hold it. Git's objects never change under their IDs, so nothing it keeps can be stale. It keeps them in memory only, up to a bound, and is safe for concurrent use.
- `internal/run`: `Recovery` reads its bases and snapshots through one `Objects`.
- `internal/task`: `Fairness` reads through one `Objects`, keeps each task's gaps once worked out, and becomes safe for concurrent use; `PrepareGaps` works out the gaps of several tasks at a time.
- `internal/cli`: `task list` prepares the gaps its form needs before it prints.

**Boundaries**
- No output changes: the report's terminal view, Markdown, `--details` and `--json`, and `task list` in every form. No golden is edited; `internal/cli/testdata/contract.golden` does not change.
- `internal/stats` and `internal/experiment` are not touched: the same bootstrap draws, seeds and verdicts.
- Nothing new is stored: no file in the data folder, no migration, nothing in the user's repository. A cache on disk is not needed for the report; for the task list it is the user's choice (see the handoff).
- No background processes: the gap checks of one command run several at a time and all end before it prints.
- The other callers of `source.Commit` and of `task.Gaps` keep their behavior; they can adopt `Objects` and `PrepareGaps` later.

## Acceptance
1. **Same bytes on real data.** On the copy, the fixed binary's output equals the baseline binary's for every experiment's report as `--json`, Markdown, `--details` and the terminal view, and for `task list` as `--json`, the piped table, `--details` and the designed list. Evidence: a comparison script's result in the PR.
2. **Goldens untouched.** `git diff origin/main -- internal/cli/testdata internal/report/testdata` is empty, and `check ci` passes.
3. **Fewer git processes,** counted by a test through a `git` wrapper on `PATH` and on the copy:
   - a report whose runs share blobs reads each blob and lists each commit once (`ab16` on the copy: at most 40 git processes, from 1,297);
   - `task list --json` starts no git process twice (the copy: at most 470, from 620).
4. **Faster on the same copy, measured the same way:** `experiment report ab16 --json` at least 10 times faster than the baseline; `task list --json` at least 3 times faster. Medians of 5 runs, with the machine's load noted.
5. **Nothing stored.** The two commands leave the copy's files as the baseline binary does (a listing before and after).
6. **Tests** cover: one read per blob and one listing per full commit ID; a commit named another way listed each time; a failed or cancelled read not kept; reads safe from several goroutines (`-race`); the bound; `Recovery` and `Fairness` giving the answers they gave; prepared gaps equal to the ones worked out one by one, in any order, with a failing task not kept.

## Work
- [x] `source.Objects` with tests
- [x] `run.Recovery` through it, with a call-count test
- [x] `task.Fairness`: `Objects`, kept gaps, safe for concurrent use, `PrepareGaps`, with tests
- [x] `task list` prepares gaps; call-count test
- [x] Before and after timings and the byte comparison on the copy
- [x] Docs: the code map and the architecture's conventions
- [x] Checks, CI, the two reviews and their fixes, the listing for criterion 5

## Verification and handoff
- **Same bytes (criterion 1): met.** 74 of 74 outputs identical between a binary of `origin/main` and this branch on the copy (794,011 bytes of stdout, with stderr and exit codes): all 8 experiments' reports as `--json`, Markdown, `--markdown`, `--details` and on a pseudo-terminal (120 and 80 columns, `--details`, `NO_COLOR`), and `task list` as `--json`, the table, `--details` and the designed list. Compared again after the review fixes.
- **Goldens (criterion 2): met.** No file under `testdata` is in the diff; CI passed.
- **Git processes (criterion 3): met.** On the copy: `experiment report ab16 --json` 1,297 to 34 (10 listings, 23 blobs, 1 `rev-parse`); `experiment report aa --json` 157 to 20; `task list --json` 620 to 461 and `task list` 534 to 461, none repeated, 8 tasks at a time. Tests count the same through a `git` wrapper on `PATH` (`internal/gitx/gitxtest`).
- **Timings (criterion 4): met.** Medians of 5 rounds, the two binaries alternating on one copy, 12 cores.

  | Command | Quiet (load 5 to 7): before | after | faster | Busy (load 28 to 70): before | after | faster |
  |---|---|---|---|---|---|---|
  | `experiment report ab16 --json` | 7.98 s | 0.32 s | 24.9x | 32.08 s | 1.41 s | 22.8x |
  | `experiment report aa --json` | 0.98 s | 0.18 s | 5.4x | 3.25 s | 0.54 s | 6.1x |
  | `experiment report ab16` | 7.59 s | 0.31 s | 24.2x | 34.55 s | 1.78 s | 19.4x |
  | `task list --json` | 4.18 s | 0.78 s | 5.3x | 9.16 s | 1.75 s | 5.2x |
  | `task list` | 3.61 s | 0.74 s | 4.9x | 7.27 s | 1.58 s | 4.6x |
  | `task list --details` | 4.04 s | 0.84 s | 4.8x | 7.20 s | 1.61 s | 4.5x |
  | `experiment show ab16` | 0.01 s | 0.01 s | same | 0.03 s | 0.04 s | same |

- **Nothing stored (criterion 5): met.** A listing of a copy before and after the seven read-only commands: the old and the new binary both add only the two empty folders `artifacts` and `workspaces` (every command makes them), and the two listings after are identical in names and sizes. The repository's checkout is unchanged.
- **Tests (criterion 6): met.** `go test -race` passes for `internal/source` and the gap tests of `internal/task` (3 times each), the recovery tests of `internal/run` and the task list tests of `internal/cli`. The new recovery and task list tests fail against the old code (a build overlay of `origin/main`'s files).
- **Checks:** `harness.py check changed` ended `[harness] exit=1`: 14 of 15 packages passed and `internal/cli` failed on `TestCleanRemovesAStoppedValidationsGradeFolder`, which found a process still using a validation's grade folder while the machine's load was 28 to 70. Run alone 3 times with the race detector it passed; the change touches no `clean` code, and the reviewer agreed it is unrelated. CI passed on both heads that had code changes.
- **Reviews.** The Claude reviewer asked for changes (2 findings) and Codex raised 3; all 5 were fixed in one round (`49a4e9d`), then the reviewer approved and Codex reported them fixed with no regression:
  - a blob was kept under the ID its listing named while the read named the commit, so a branch that moved in between put a later commit's file under the old blob's ID (both reviewers). Only a commit named by its full ID, of the repository's ID length, now shares what is kept;
  - gaps worked out while a read failed were kept (a check skips a file it cannot read). They are given as before and no longer kept; the same for the base's fields;
  - a caller whose context had ended could get kept gaps with no error, from another context's sources or after waiting for another goroutine's check. It gets its context's error, and waits no longer than its context lives;
  - a comment promised more than the code did about cancelled callers; the code now does what it says.
- **A cancelled command:** a gap check that a cancel overtakes is an error and keeps nothing, like a cancelled search; before, it could answer from the reads that were left.
- **Limits:** `task list` still starts about 460 git processes on this data, each a different search or read. Considered and left out: searching in process instead of `git grep` (git's case folding depends on its build and the locale, so the answers could differ); one `git grep` for several texts (it cannot say which text matched).
- **Decided by the user (2026-10-05):** a cache of each task's gaps in the data folder, keyed by the task and by the build of Agentium, git and the locale. It is the next plan.
- **Follow-ups, not started:** `experiment plan`'s readiness, `pool update --json` and `task validate --all --json` ask for gaps task by task and could call `PrepareGaps`; `LoadNorthStar` analyses every finished experiment (0.04 s today, about 30 ms more for each).

## Metrics
- Agent: Claude Code / claude-fable-5-1 / max
- Elapsed: 160m
- Check-fix loops: 1
- User corrections: 0
- Review: changes requested, 5 fixed
