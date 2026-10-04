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
- [ ] **2. Mining and the agent:** module-scoped mining and hidden tests, the agent's starting folder, `claudectx` from the module, records and reports. Risk: high (hidden tests; the context experiments' meaning).
- [ ] **3. Real check and docs** (free). Risk: low.

## Boundaries
- A project is one registration (`projects.root` is unique) and holds tasks from several modules; a task is in one module. An experiment may mix modules (each task runs in its own); a combined per-module view is later.
- No new modules (Go dependencies), and no paid runs.
- Starts after the console screens (the user's order). Steps 1–2 touch `internal/buildtool`, `internal/project`, `internal/mine`, `internal/run`, `internal/claudectx` and `internal/cli`.

## Verification
`python3 scripts/harness.py check changed`, the fixtures above, a review per step (with the threat checklist for step 2), and the free real check.

Step 1, after its re-reviews (2026-10-04): `GOPROXY=off go test -race -count=1` passes for `internal/run`, `internal/experiment`, `internal/cli` and `internal/buildtool`, with the sandbox tests run on macOS; each new test fails with its fix reverted.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
