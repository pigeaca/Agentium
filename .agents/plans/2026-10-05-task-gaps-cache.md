# The task list keeps its checks

- Date: 2026-10-05
- Status: In Progress
- Scope/approval: the user, 2026-10-05. After [the fast report and task list](archive/2026-10-05-fast-report-and-task-list.md) (#173), `task list` still started about 460 git processes on every call. Asked "Do you want that cache, and keyed how?", the user chose "Yes, keyed by build": the task's base and solution IDs, instruction and file lists, plus a hash of the Agentium binary, git's version and the locale; the first `task list` after a new build or git recomputes once; a second PR after #173. Why: the [decision](../decisions/2026-10-05-gaps-cache-keyed-by-build.md).

## Outcome and boundaries
`agentium task list` answers in about 0.05 s once this build has checked each task, instead of 0.8 s (2.5 s on a busy machine): it starts 2 git processes, not 461.

- **What is kept:** each task's unstated requirements (`task.Gaps`), in `cache/gaps/<project ID>.json` of the data folder, by `task.GapsCache`.
- **The key, which says all an answer depends on:**
  - the task's input (`FairnessInput`): its base and solution commits, instruction, hidden test files and reference files. The commits are named by their full IDs, which git never changes; a check of any other name is not kept;
  - the checker: the build of Agentium that worked the answer out (`cli.Binary.ID`: the SHA-256 of the binary's file with the running program's own build information; none when the file is no longer the one the program started from), and git's identity (`gitx.Identity`: the program on `PATH`, its version and build options, and the locale variables it runs under, since a search that ignores case can answer differently under another build or locale). A checker reads only its own answers; the file holds those of the 8 checkers that saved last (one build is several checkers when its locale variables differ, as between a terminal, an agent's shell and a script).
- **Only complete answers are kept:** not a check that failed, was cancelled, or skipped a file it could not read (the rule #173 set for a command's own memory).
- **Only `task list` uses it.** `task show`, `task edit`, `task validate`, `experiment plan` and the rest check afresh, as now.

**Boundaries**
- No output changes: `task list` prints the same bytes with a warm file, a cold one and none.
- The file lives in the data folder's `cache`, which runs deny to agents (it holds text of hidden tests); nothing is written to the user's repository.
- No migration: the file is found by its path and read only when its format matches. An older Agentium ignores it; a file of another format, or one that cannot be read, is treated as empty and replaced.
- No background work: the file is read when `task list` starts and written, whole and then renamed, before it ends.
- `agentium clean` leaves it alone (a few kilobytes; deleting it costs one slow list).
- Without a build identity (tests, or a binary that cannot be read) nothing is kept and nothing changes.

## Acceptance
1. **Same bytes.** On the copy of the real data folder, `task list` as `--json`, the table, `--details` and the designed list prints the same with no file (cold), with the file (warm) and from #173's binary.
2. **Warm is fast.** A warm `task list --json` starts at most 2 git processes (a test counts them; the copy confirms) and is at least 10 times faster than #173's binary on the copy (medians of 5 alternating rounds).
3. **Never stale.** Tests show that nothing is taken from the file when the build, git's identity or a locale variable differs, and that a task whose base, solution, instruction, hidden tests or reference changed is checked afresh. A check that failed, was cancelled, skipped a read, or whose commits are not named by full IDs is not written.
4. **Stored safely.** The file is in the data folder's `cache/gaps` only, mode 0600 in folders 0700, written whole and renamed. A truncated, foreign or unreadable file is ignored and replaced. A save that fails is reported on stderr and changes neither stdout nor the exit code. Gaps of tasks the project no longer has are dropped.
5. **Off without a build identity:** no file is read or written, and the output is the same.
6. **Goldens untouched,** `internal/cli/testdata/contract.golden` unchanged, `check ci` green.

## Work
- [x] `gitx.Identity`, `source.Pinned`, `home.Layout.GapsCache`
- [x] `task.GapsCache` and `Fairness` taking and storing gaps through it
- [x] `cli`: `Env.BuildID`, `task list` opens and saves the file; `cmd/agentium` notes its binary at start
- [x] Tests for criteria 2 to 5
- [x] The copy: same bytes, counts, timings
- [x] Docs: guide, code map, architecture budget, the decision
- [ ] `check changed`, CI, the two reviews

## Verification and handoff
- **Same bytes (criterion 1): met.** On fresh copies of the real data folder, `task list` as `--json`, the table, `--details` and on a pseudo-terminal (120 and 80 columns, `--details`, `NO_COLOR`) prints the same from `main`'s binary (no cache), from this branch with no file (cold) and with the file (warm): 7 of 7 forms, stdout, stderr and exit code. The cold run writes the file; the warm run leaves it untouched. All 74 outputs of the earlier comparison (every experiment's report too) are identical between the two binaries.
- **Warm is fast (criterion 2): met.** On the copy a warm `task list --json` starts 2 git processes (the repository's root, git's identity); a cold one 462 (461 as before, plus git's identity). Medians of 5 alternating rounds, 12 cores, load about 9:

  | Command | `main` (#173) | Cold | Warm | Warm against `main` |
  |---|---|---|---|---|
  | `task list --json` | 0.77 s | 0.78 s | 0.026 s | 30.0x |
  | `task list` | 0.77 s | 0.79 s | 0.025 s | 30.4x |
  | `task list --details` | 0.77 s | 0.78 s | 0.025 s | 30.5x |

  Before #173 the same list took 4.2 s.
- **Never stale (criterion 3):** `TestGapsCacheKeepsEachCheckersAnswersApart` (another build, git, locale, and texts shifted between the two take nothing; an answer only one checker has is given to no other), `TestGapsCacheChecksAChangedTaskAfresh` (each part of a task), `TestGapsCacheKeepsOnlyCompleteAnswersOfPinnedCommits` (a failed check, a skipped read, a cancelled command, a cancel on the way, commits named by branches, text that is not UTF-8), `TestBinaryIDRefusesAFileThatChanged`, `TestIdentityChangesWithTheLocale`.
- **Stored safely (criterion 4):** on the copy the file is `cache/gaps/1.json`, 2,309 bytes, mode 0600 in folders 0700; the repository's checkout is unchanged. `TestGapsCacheIgnoresAFileItCannotUse`, `TestGapsCacheDropsWhatNoTaskHasAndReportsAFailedSave`, and in `TestTaskListKeepsItsChecksBetweenCommands` the save that fails (stderr says so, stdout and the exit code do not change).
- **Off without a build identity (criterion 5):** the same test's first and "unknown build" parts; every other CLI test runs without one.
- **Found while measuring:** one build is several checkers. A shell with no locale set, the same shell through a Python script (Python sets `LC_CTYPE=C.UTF-8` for what it starts) and a terminal with `LANG` set differ in git's identity, and a file of one checker was replaced at each switch. The file therefore holds the answers of the 8 checkers that saved last, each apart.
- **Limits:** only `task list` uses the file. The first list of every new build, git or locale takes as long as before. A binary replaced by a build with the same build information in the instant between the program's start and its first look at its own file would be taken for the running one (the decision record says why that is the one case left).

## Metrics
- Agent: Claude Code / claude-fable-5-1 / max
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
