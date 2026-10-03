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
- [x] **1. The command** (risk: medium-high: it deletes data). Rules, the dry run, `--yes`, the stamps, tests, docs. Done: `internal/run/clean.go`, `clean_leftovers.go` and `internal/cli/clean.go`; last use is the seed folder's, the warm-up stamp's and the deps folder's modification time (`markUsed`: a run's setup, a validation's start and each of its commands, a seed handed out); nothing used in the last hour goes (validations take no run lock); a locked, unfinished experiment's bases stay whatever their age; removals go through the quarantine. Review round (#132): F1 to F9 fixed, each with a test that a mutation of its rule fails.

## Verification
- `python3 scripts/harness.py check changed`, the new tests, and a review with the threat checklist (crash and recovery, persistence, concurrent runs).

## Limits
- A seed is handed out under a shared lock on its lock file (`seedTaken`), and clean takes it exclusively, rechecks the seed's last use and only then moves it away. From the hand-out to the grade's clone the seed holds no lock: the mark it got covers that window, since nothing used within the hour goes.
- Validation takes no run lock: what it uses is marked at its start and at each command, so a validation never goes an hour without a mark. One that waits more than an hour inside a single command is the case left: a removal then fails its next command.
- Sizes count disk blocks once per item; a venv's files hard-linked to the uv cache free less than they show.
- Not cleaned: throwaway warm-up checkouts (`cache/warm/w-*`), Agentium's own build caches, workspaces without records, lock files.
- **Follow-up:** #131's validation grade roots (`<artifacts>/tasks/<id>/<ts>/grading/<label>`) are cleaned by neither recovery nor clean; a validation that dies leaves one.

## Console sample
Offline, on a scratch data folder: a Python (uv) repository cloned locally; `init`; two tasks imported and validated, which warmed two real venvs. The seeds, the quarantined folder and the stopped run were made by hand (nothing makes seeds before #131 lands); then `task rm sub` and the modification times set back two or three days to stand for time passing. `<data>` replaces the scratch path. After it, `task validate mul` was still valid.

```
$ agentium clean
Cleaning <data> would free 4.9 MB (a dry run: nothing was removed)
  kind          remove      size  keep    size
  seeds              1    4.4 MB     1  4.4 MB
  dependencies       2   77.8 kB     1  4.5 MB
  quarantine         1  229.4 kB     0     0 B
  leftovers          1  241.7 kB     0     0 B
  total              5    4.9 MB     2  8.8 MB

What would go:
  seed          cache/grading-seed/1/python-1a2b3c4d-9f224ea6ad1ebe14f8a2755958672e96a631d8a6    4.4 MB  no task or experiment uses its base
  dependencies  deps/1/py-meta/d8dd32fd6c87a195                                                  4.1 kB  no task or experiment uses its base
  dependencies  deps/1/py/2a22fda4d36898c2                                                      73.7 kB  no task or experiment uses its base
  quarantine    cache/quarantine/20261001-ab12-grading-1f2e3d                                  229.4 kB  could not be removed when it was put there
  leftovers     records/20261002-dead                                                          241.7 kB  a run that stopped before its agent started

Kept:
  seed          cache/grading-seed/1/python-1a2b3c4d-712e56852fca1e2cd31f49e324f50b7c35819ef9  4.4 MB  in use by task mul
  dependencies  deps/1                                                                         4.5 MB  in use by task mul

Run agentium clean --yes to remove it. What a task uses goes too once unused for 30d (--older-than); what a locked, unfinished experiment uses stays.
[exit 0]
$ agentium clean --yes
Cleaned <data>: 4.9 MB freed
  kind          removed      size  kept    size
  seeds               1    4.4 MB     1  4.4 MB
  dependencies        2   77.8 kB     1  4.5 MB
  quarantine          1  229.4 kB     0     0 B
  leftovers           1  241.7 kB     0     0 B
  total               5    4.9 MB     2  8.8 MB

What went:
  seed          cache/grading-seed/1/python-1a2b3c4d-9f224ea6ad1ebe14f8a2755958672e96a631d8a6    4.4 MB  no task or experiment uses its base
  dependencies  deps/1/py-meta/d8dd32fd6c87a195                                                  4.1 kB  no task or experiment uses its base
  dependencies  deps/1/py/2a22fda4d36898c2                                                      73.7 kB  no task or experiment uses its base
  quarantine    cache/quarantine/20261001-ab12-grading-1f2e3d                                  229.4 kB  could not be removed when it was put there
  leftovers     records/20261002-dead                                                          241.7 kB  a run that stopped before its agent started

Kept:
  seed          cache/grading-seed/1/python-1a2b3c4d-712e56852fca1e2cd31f49e324f50b7c35819ef9  4.4 MB  in use by task mul
  dependencies  deps/1                                                                         4.5 MB  in use by task mul
[exit 0]
$ agentium clean
Cleaning <data> would free 0 B (a dry run: nothing was removed)
  kind          remove  size  keep    size
  seeds              0   0 B     1  4.4 MB
  dependencies       0   0 B     1  4.5 MB
  quarantine         0   0 B     0     0 B
  leftovers          0   0 B     0     0 B
  total              0   0 B     2  8.8 MB

Kept:
  seed          cache/grading-seed/1/python-1a2b3c4d-712e56852fca1e2cd31f49e324f50b7c35819ef9  4.4 MB  in use by task mul
  dependencies  deps/1                                                                         4.5 MB  in use by task mul

Nothing to remove.
[exit 0]
```

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
