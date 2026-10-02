# A simpler command line

- Date: 2026-10-02
- Status: Planned (2026-10-02). The user asked "Simplify everything that you found" after the flag investigation; this plan records that scope.
- Scope: Agentium's commands and flags. Today there are about 30 commands with 67 distinct flags (111 definitions), and `experiment new` alone has 24 (25 with `--judge-pairs`, #126). The everyday path (`start`, `start --yes`, `experiment report`) uses almost none of them, and the same per-project settings repeat across commands (`--verify` and `--setup` on 4 commands, timeouts in 5 places, `--require-lock`, `--jobs` and `--accept-mined` on 2–3).

## Outcome
About 35 visible flags instead of 67, with `experiment new` at about 10. Nothing a user can do today is lost: a derived flag gives the same result automatically, a project setting is set once with `init`, an expert flag still works but is listed only in the guide, and a removed command names its replacement.

## Changes
**1. Derive instead of asking** (step 1: `internal/cli` experiment and run files, `internal/experiment` where the design needs it):
- `--template` is removed and inferred from `--b`: no `--b` gives an A/A; a snapshot name gives a context A/B; `MODEL[:EFFORT]` (a model the price table knows, or any `claude-…` name) gives a model A/B. `experiment new` refuses a `--b` that is both a snapshot name and a model name, saying which it took. The stored design keeps `template`, and the JSON keeps its field.
- `--run-budget-a` and `--run-budget-b` are removed: `--run-budget` caps both arms, as it does by default today.
- `--effort` is removed: `--model MODEL[:EFFORT]` everywhere (`experiment new`, `run once`, `run calibrate`), the syntax model A/Bs already use.
- `--judge-model`, `--judge-effort` and `--judge-repeats` are removed: `--judge[=MODEL[:EFFORT]]` and `--judge-pairs[=MODEL[:EFFORT]]` (bool flags that take an optional value); repeats stay at the default (3).
- `run calibrate` leaves the help (it still works): `experiment run` calibrates by itself.

**2. Set once per project** (step 2: `internal/cli` init, task, pool and start files; `internal/store`):
- `agentium init` stores the project's settings in the data folder (never in the repository): `--verify CMD`… and `--setup CMD`… (the mined tasks' commands, default: the detected build tool's), `--require-lock`, `--jobs N`, `--verify-timeout DURATION` and `--allow-local-binding[=false]`. `init` without flags prints them. One migration adds them to `projects`.
- `task import`, `pool update`, `start` and `task validate` read them. Their own `--verify`, `--setup`, `--require-lock`, `--jobs`, `--timeout` and `--verify-timeout` become hidden per-command overrides. `task add` keeps `--verify` and `--setup` visible (a task by hand).
- `--no-allow-local-binding` is removed (`--allow-local-binding=false`).
- `--instruction-file FILE` is removed: `--instruction @FILE` reads a file (`@@` starts a literal `@`).
- This replaces the per-project part of the headless plan's `agentium.toml`, without a TOML module or a file a teammate's commit could change. Consent to spend stays on the command line (`--yes`).

**3. One way to mine** (step 2):
- `task mine` is removed; `agentium task mine` says to use `pool update` (or `pool update --dry-run` for the preview). `pool update --dry-run` keeps everything `task mine --dry-run` shows: candidates with scores, and why other commits were set aside.
- `start` keeps mining for first use, and `pool update` keeps tasks fresh. `--since` and the thresholds (`--max-files`, `--max-lines`) become hidden expert flags of `pool update`.

**4. Hide expert flags** (both steps, each in its own files): `--seed`, `--no-futility`, `--tier`, `--repeats`, `--concurrency`, `--timeout`, `--verify-timeout`, `--max-files`, `--max-lines`, `--max-hunks`, `--keep`, `--include-linked` and `--since` still parse but leave the usage text. The guide gets an "Advanced flags" table listing each with its default.

## Acceptance
1. Every removed flag fails with a usage error (exit 2) that names its replacement. Every hidden flag still works and is in the guide's table. A test asserts that the usage text lists none of the hidden flags.
2. `experiment new` infers each template correctly, with tests for an A/A, a context A/B, a model A/B (with and without `:EFFORT`) and an ambiguous name. Designs and locks made before this change load, resume and report unchanged, and the JSON `experiment` object keeps its fields.
3. `--model M:E` and `--judge[=M:E]` give the same stored design as the old flag pairs. The judge's repeats stay 3.
4. Project settings: `init` stores and prints them, and mining, import, validation and `start` use them. A per-command override wins for that call only. A project registered before this change (no settings) behaves exactly as today.
5. `task mine` is gone with a pointer. `pool update --dry-run` shows the candidates' scores and the reasons others were set aside.
6. Docs: README's commands table and the guide (all examples, the advanced-flags table, the scripting section) use the new forms; no help text or doc names a removed flag except the guide's short "Renamed and removed" list.
7. Visible flag count: at most 40 distinct names in the usage texts, `experiment new` at most 12. Counted by a test or a recorded script output.

## Boundaries
- No change to statistics, budgets, the sandbox, run behavior or stored experiment data. No new modules. No paid runs.
- Step 1 starts after #126 (`--judge-pairs`) merges, since both edit `internal/cli/experiment.go`. Step 2 can start now. The two steps own separate files; `docs/guide.md` and `README.md` changes are merged carefully.
- Compatibility: Agentium is early and has no external scripts to keep. Removed flags fail loudly with a pointer rather than staying as silent aliases.

## Work
- [ ] **1. Experiment and run flags** (derive, the judge, hide). Risk: medium (the experiment design and lock inputs).
- [x] **2. Project settings, one way to mine, the task and init flags** (hide). Risk: medium-high (a migration; mining's entry points). Done: migration 0012 adds the settings to `projects`; `init` stores and prints them (text and JSON); `pool update`, `start`, `task import`, `task add` and `task validate` read them, with hidden per-call overrides; `task mine`, `--no-allow-local-binding` and `--instruction-file` fail with exit 2 and name their replacements; `pool update --dry-run` shows the scores and the commits set aside, and `--since` re-scans without moving the watermark. Visible flags in the usage texts of init, task, pool, start and context: 42 before, 34 after (all usage texts: 68 to 61).
- [ ] **3. Docs and the count**: README, guide, help texts; the visible-flag count recorded.

## Verification
- Per step: `python3 scripts/harness.py check changed`, the CLI tests of the touched commands, and a review.
- The visible-flag count before and after (the investigation's script: distinct names in the usage texts).
- A real console sample of `init` showing the settings, `experiment new … --b claude-sonnet-5-5` inferring a model A/B, and a removed flag's error.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
