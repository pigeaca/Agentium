# A fast report and task list

- Date: 2026-10-05
- Status: In Progress
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
- [ ] The full `check changed` run, CI, the two reviews, the listing for criterion 5

## Verification and handoff
State on 2026-10-05, when the session ran out of usage: implemented and committed; not yet reviewed.

- **Same bytes (criterion 1): met.** 74 of 74 outputs identical between a binary of `origin/main` and this branch on the copy (794,011 bytes of stdout, with stderr and exit codes): all 8 experiments' reports as `--json`, Markdown, `--markdown`, `--details` and on a pseudo-terminal (120 and 80 columns, `--details`, `NO_COLOR`), and `task list` as `--json`, the table, `--details` and the designed list.
- **Git processes (criterion 3): met on the copy.** `experiment report ab16 --json`: 1,297 to 34 (10 listings, 23 blobs, 1 `rev-parse`). `task list --json`: 620 to 461, none repeated, 8 tasks at a time.
- **Timings (criterion 4): met.** Medians of 5 rounds, the two binaries alternating on one copy, 12 cores, load average 28 to 70 (other test runs), so upper bounds:

  | Command | Before | After | Faster |
  |---|---|---|---|
  | `experiment report ab16 --json` | 32.08 s | 1.41 s | 22.8x |
  | `experiment report aa --json` | 3.25 s | 0.54 s | 6.1x |
  | `experiment report ab16` | 34.55 s | 1.78 s | 19.4x |
  | `task list --json` | 9.16 s | 1.75 s | 5.2x |
  | `task list` | 7.27 s | 1.58 s | 4.6x |
  | `task list --details` | 7.20 s | 1.61 s | 4.5x |
  | `experiment show ab16` | 0.03 s | 0.04 s | unchanged |

  In a quieter minute (load about 9) the fixed report took 0.35 s (`ab16`) and 0.20 s (`aa`).
- **Tests (criterion 6):** `go test -race` passes for `internal/source`, the recovery tests of `internal/run`, the gap tests of `internal/task` and the task list tests of `internal/cli`. The new recovery and task list tests fail against the old code (checked with a build overlay of `origin/main`'s files).
- **Not done yet:** the full `check changed` run (started, not read), CI, both reviews, and the before and after listing of the data folder for criterion 5 (the change writes no file; the listing is the evidence still owed).
- **A cancelled command:** a gap check that a cancel overtakes is now an error and keeps nothing, like a cancelled search; before, it could answer from the reads that were left.
- **Limits:** `task list` still starts about 460 git processes on this data, each a different search or read. Considered and left out: searching in process instead of `git grep` (git's case folding depends on its build and the locale, so the answers could differ); one `git grep` for several texts (it cannot say which text matched).
- **For the user to decide:** a cache of each task's gaps in the data folder would make `task list` start one git process (about 0.05 s). Its key would be the task's base and solution IDs, instruction and file lists, plus what identifies the code that checked: Agentium's build, git's version and the locale. It is stored state with its own rules, so it waits for an answer.
- **Follow-ups, not started:** `experiment plan`'s readiness, `pool update --json` and `task validate --all --json` ask for gaps task by task and could call `PrepareGaps`; `LoadNorthStar` analyses every finished experiment (0.04 s today, about 30 ms more for each).

## Metrics
- Agent: Claude Code / claude-fable-5-1 / max
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
