# Monorepos: measure one module of a big product

- Date: 2026-10-03
- Status: Planned. Queued by the user (2026-10-03, "Yes" to doing it after the console screens). It grew from their question: "Also will this tool work with so big products that can't be run or have lack of tests or lack of info?"
- Scope: repositories whose build files are not at the root, such as `services/billing/go.mod`, `backend/pom.xml` or `packages/api/pyproject.toml`. Today Agentium detects build tools only at the root, so such a repository gets no verify command, no offline dependencies and no mined tasks.

## Outcome
A user registers one module of a monorepo and measures it like a whole repository:
- they run `agentium init --module services/billing`;
- `init` without `--module` lists the modules it finds when the root has no build file;
- tasks, validation, runs, grading and reports then work inside that module.

The agent still gets the whole repository: a module's code depends on its neighbours. It starts in the module's folder, as a developer would, so Claude Code loads the root's and the module's instructions the way it does in real use.

## Design
- **The module is each task's.** A task records its module (`tasks.module`, migration 0013; old tasks are root tasks).
  - Setup, verify, validation, warm-up and grading all use the task's module, never the project's current setting.
  - Experiment locks record each task's module (`LockedTask.Module`, omitted for the root, so old locks and digests are unchanged), so resuming grades where the lock says.
  - One registration holds tasks from several modules.
- **The module setting:** a project setting stored with the others (#128), `init --module PATH`; it is only the default module of new tasks (added or imported; mined ones in step 2). A hidden `--module` of `task add` and `task import` overrides it.
  - The path must be a folder inside the repository, reached through no link, holding a detected build file, and committed in HEAD under exactly that spelling.
  - `--module ""` clears it.
  - Without a setting, behaviour is today's.
  - Until step 2, `pool update` and `start` mine nothing while the setting is set (they say so).
- **The module's folder is not trusted at grading time:** every use walks the path with `Lstat` (`buildtool.ModuleDir`). In grading (host and sandbox) a module the agent removed or linked is a failed grade with a note, never infra. At setup and validation it is an error before anything runs.
- **Detection:**
  - Build profiles are detected in the module's folder instead of the root (`DetectIn(module)`).
  - `init` lists candidate modules: folders up to 4 levels deep holding a build file, nested ones folded into their nearest parent module, at most 20 shown.
- **Commands:**
  - Verify and setup commands run in the module's folder: the checkout path joined with the module.
  - The default verify command is the module's tool's (`go test ./...` in `services/billing`).
- **Offline dependencies:** the warm-up runs in the module's folder, keyed by the module too. The deps folder and grading seeds include the module in their keys.
- **Mining** (`pool update`, `start`, `task import`):
  - a candidate commit must change code and tests inside the module;
  - size limits count only the module's files;
  - commits that touch the module only incidentally (renames, formatting) are set aside with a reason;
  - the hidden tests are the module's test files.
- **The agent:**
  - it starts in the module's folder;
  - the denied paths and the sandbox stay as today (the whole checkout is the agent's);
  - `claudectx` resolves the files Claude Code loads from that folder, which can include a nested `CLAUDE.md`, and context snapshots record them;
  - a context A/B can then target the module's own instructions.
- **Grading:** in the sandbox (#131), as today. Commands run in the module's folder, and the profile's writable roots are unchanged.
- **Records and reports:** the module is part of the experiment's lock and is named in reports, e.g. "lean-vs-trimmed · services/billing". Experiments made before keep the root.

## Acceptance
1. **A monorepo fixture**, with Go, Python and Maven modules and no root build file:
   - `init` lists the modules;
   - `init --module` picks one;
   - `pool update` mines only that module's commits;
   - `task validate` runs in it;
   - a fake-agent run is graded in the module.
2. **The agent's starting folder:**
   - it is the module;
   - `context show` lists the root `CLAUDE.md` and the module's nested one;
   - a snapshot captures both.
3. **Keys:**
   - deps and seeds keyed by module;
   - two modules of one repository never share a venv or seed.
4. **Compatibility:**
   - projects without a module behave and store exactly as today (goldens unchanged);
   - old locks resume.
5. **A free real check** on a public monorepo (for example a Go or Python one with several modules): `init` lists its modules, `pool update --dry-run` finds candidates in one, and `task validate` passes for one task. No agent runs.
6. **Docs:** the guide gets a "Monorepos" section.

## Work
- [x] **1. Setting, detection and commands:** `init --module`, module listing, detection and verify in the module, deps keyed by module. Risk: medium. Done on `claude/feat/monorepo-module` (migration 0013; stamps, venvs and seeds keyed by `buildtool.ModuleKey`; the deps folder stays per project). Re-review fixes: the scripts and runner configuration grading restores or reports are read from the task's module (`svc/run_tests.sh`, `svc/Makefile`), and the module's folder is checked before they are restored; a module folder that a test swaps out between two sandboxed commands fails the grade with a note, never infra; `start` with a module set says mining is off and how to go on, never "no more candidates"; tests for the lock's module and for validation using the task's module. Second re-review: runner configuration is matched in the module's folder and every folder above it (Maven's parent `pom.xml` and `.mvn`, pytest's root `pyproject.toml` and `conftest.py`, Gradle's root build files), so an edited ancestor config lands in `ConfigChanged` and the run is no success; a script that cannot be restored (its folder linked or replaced) fails the grade with a note, never infra (also at the root).
  - Limitations of step 1:
    - host verify does not re-check the module's folder between commands; acceptable, as host grading runs the agent's code unsandboxed anyway;
    - done (reliability round 2, also at the root): a verify script that is a symbolic link in the base brings its targets within the repository, through chains of links, so an agent that edits only the target is graded with the starting version; a target that is absolute, above the root or missing is never followed. Not covered: a script reached through a linked folder (`tools -> scripts`, then `sh tools/check.sh`), which the base does not list as a file;
    - done (same): runner configuration the agent adds where the base had none (a new `pytest.ini`, `conftest.py`, `.mvn/maven.config`), in the module's folder or above it, is reported in `ConfigChanged`, as an edited one is;
    - done (same): after a plain `cd DIR` (no `$`, `~`, globs or `-`) the words of a command are read from `DIR` too, never instead of the module's folder, so `cd .. && sh tools/check.sh` protects `tools/check.sh`. The words are not parsed as a shell would (a `cd` in a subshell still counts), which can only add a path. A runner's configuration is still looked for in the module's folder and above it, not below a `cd` into a subfolder.
- [x] **2. Mining and the agent:** module-scoped mining and hidden tests, the agent's starting folder, `claudectx` from the module, records and reports. Risk: high (hidden tests; the context experiments' meaning). Done on `claude/feat/monorepo-step2`. Decisions:
  - **Mining** (`mine.Options.Module`): a candidate changes the module, and every test, code, dependency and vendored file it changes is in it; documents may change anywhere. New reasons "outside the module" (nothing in it changes) and "changes outside the module" (the hostile case: tests elsewhere, which the module's commands would never run). The other rules, size limits included, then see only module files. Test languages, the default verify command and the `--require-lock` check are the module folder's (`ModuleDir`, Lstat-checked). `task import` and `task add --solution` into a module refuse hidden tests outside it. Mined tasks record the module; hidden tests stay root-relative paths.
  - **The pool** keeps one watermark per module (`State.Modules`, `module_watermarks`, omitted when empty, so root-only state files are unchanged); dismissals, pending and mined records stay the project's; one pass at a time per project as before.
  - **The agent** starts in the module's folder (`ModuleDir` right before it starts, after the arm's files and setup; a link or a missing folder is Agentium's error before any spend). The whole checkout stays its own: `claude.Invocation.Repo` adds `--add-dir <checkout>` and the checkout to the sandbox's `allowWrite`, and the build tools' environment still names the checkout (Python's import root). Root runs: byte-identical command. The session folder follows the starting folder.
  - **Context** (`claudectx.ResolveIn`): a session in a module loads, from every folder from the root down to the module (root first), the instruction file (`CLAUDE.md`/`.claude/CLAUDE.md`, else `AGENTS.md`) with its imports, `.claude/rules`, skill/subagent/command descriptions, and harness files; other folders' instruction files stay on demand, other folders' `.claude` is no context. `context show`, `context snapshot` (`snapshot.BuildIn`; the manifest records `module`, omitted at the root), arms in runs and validation (`PlanOverlayIn`), context use and its recovery all use the task's (or the setting's) module. A snapshot is a set of files: applied to a task in a module, the arm loads exactly what a session there loads of them, so a root snapshot drops the module's own `.claude` files; `start` notes a snapshot taken for another module.
  - **Records and reports:** `run.Record.Module` (omitted at the root); report titles name the module ("Experiment x · svc/billing", or "· modules (root), a" when mixed); `pool update` names the module it mines (text and `--json` `module`); `init` offers the module's mined verify command. No migration.
  - Limitations of step 2:
    - which `.claude` folders above the starting folder Claude Code loads (rules, skills, commands, settings) is Agentium's reading of the docs, not yet checked in a real session; only `CLAUDE.md` loading up the tree is documented. A paid probe session would settle it; a run's start-of-session skill list shows a difference;
    - `--add-dir` of the checkout may load the root's skills by itself (the docs say skills of added folders load); consistent with the model above, unverified;
    - context lint, its hook and the pull-request screen still read the root's context only, and calibration runs still start at the checkout's root;
    - test files are recognized by their root-relative path (as `task.Split` does): a module whose own path has a test folder name (`e2e/api`) mines nothing, and its imports are refused as tests only;
    - dismissals are the project's: a change dismissed at the root is not offered in a module either (and the other way round);
    - an experiment locked with module tasks between #140 and this step resumes with its agent in the module and its arms applied there, so its runs mix two setups (near zero exposure: no such experiment is known);
    - follow-up: at the root, a `CLAUDE.md` that imports `.claude/CLAUDE.md` counts it twice (an import and an instruction file), in entries and startup bytes, as it always has; a test pins it, so changing it must be deliberate (root manifests' `startup_bytes` and `context show` move);
    - follow-up: pool update's `--accept-mined` skips tasks validation set aside, as start's does (fixed in 3c4d5fa for both), but only start's has a test.
  - Review fixes (PR #146): `experiment new` and `run once` note an arm's snapshot taken for another module than its tasks' (as `start` does); tests for the root's double count, the notes, and a sibling folder whose name starts with the module's (`svc/billing2`) in mining and imports; the guide's caveat on the loading model.
- [ ] **3. Real check and docs** (free). Risk: low.

## Boundaries
- A project is one registration (`projects.root` is unique) and holds tasks from several modules; a task is in one module. An experiment may mix modules (each task runs in its own); a combined per-module view is later.
- No new modules (Go dependencies), and no paid runs.
- Starts after the console screens (the user's order). Steps 1–2 touch `internal/buildtool`, `internal/project`, `internal/mine`, `internal/run`, `internal/claudectx` and `internal/cli`.

## Verification
`python3 scripts/harness.py check changed`, the fixtures above, a review per step (with the threat checklist for step 2), and the free real check.

Step 1, after its re-reviews (2026-10-04): `GOPROXY=off go test -race -count=1` passes for `internal/run`, `internal/experiment`, `internal/cli` and `internal/buildtool`, with the sandbox tests run on macOS; each new test fails with its fix reverted.

Step 2 (2026-10-04): `GOPROXY=off go test -race -count=1` passes for `internal/mine`, `internal/pool`, `internal/claudectx`, `internal/snapshot`, `internal/run`, `internal/claude`, `internal/task`, `internal/report`, `internal/cli` and `internal/experiment` (macOS, sandbox tests included); `harness.py check changed` passes (12 packages). The monorepo fixture (Go, Python and Maven modules, no root build file) gets a history with a module commit, another module's commit and the hostile one: `pool update --dry-run` offers only the module's commit, the mined task records the module and validates there, `task import` refuses the hostile commit, `start` mines in the module, `context show` and a snapshot hold the root's and the module's `CLAUDE.md`. A fake agent records its starting folder (the module) and arguments (`--add-dir` the checkout). Five mutations, each killed by its test: tests outside the module accepted by mining, the import's refusal removed, the agent started at the root, the arm's overlay applied at the root, and module passes moving the root's watermark.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
