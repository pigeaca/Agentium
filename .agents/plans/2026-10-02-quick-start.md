# Quick start: from a fresh clone to a running experiment

- Date: 2026-10-02
- Status: Planned (2026-10-02): a wave-2 feature track in the [next chapter](2026-10-01-next-chapter.md), which the user approved. Step 1 starts after refactor step 2 (#65). Steps 2 and 3 change experiment code, so they run after the [model A/B](2026-10-01-model-ab.md) step that changes it too. No paid runs are needed beyond the wave's exit gate, which has its own approval.
- Scope: the north star's "setup in 3 commands". Today a first experiment takes about 12 commands across four README sections, and two of them (calibration and validation) are easy to forget.

## Why
- **Too many steps:**
  - A first experiment needs `init`, `context snapshot`, `task mine`, task review, `task validate`, `run calibrate` per context, `experiment new`, `plan` and `run`.
  - Each step can stop with a message that names the next command, so the user loops through readiness checks.
- **Calibration is a separate paid command:** `experiment plan` refuses to run an arm whose context is not calibrated on the current Claude Code version, so every Claude Code update breaks a ready experiment until the user calibrates again.
- **No feedback while editing context:** the cheapest moment to catch a broken `@` import or a context that grew past the cap is the edit itself, and today nothing runs then.
- **The north star is not measured:** nobody records when a project got its first decisive verdict, or what it spent up to then.

## Outcome and boundaries
- **`agentium context lint`:** a free check of the committed or working-tree context, with no agent runs. It reports:
  - the size change against the last snapshot;
  - broken `@` imports;
  - `AGENTS.md` files Codex would cut off at 32 KiB;
  - the warnings `context show` gives.

  With `--hook`, it reads a Claude Code `PostToolUse` payload on stdin and checks only when the edited file is a context file. It always exits 0, so it never blocks the session. `--print-hook` prints the settings snippet to add to the user's own `~/.claude/settings.json`; Agentium never writes it. Runs load project settings only, so the hook cannot fire inside them.
- **Calibration inside `experiment run`:**
  - An arm whose context has no healthy calibration on the current Claude Code version and model is calibrated by `experiment run` before the first pair.
  - The preview counts its cost, and the budget and reserve include it.
  - `run calibrate` stays for explicit use.
  - A calibration that fails its checks stops the experiment before any task run, with the same reasons `run calibrate` prints.
- **`agentium start [--yes] [--budget USD] [--b SNAPSHOT]`**, from a repository folder:
  1. registers it if needed (as `init`);
  2. saves the committed context as a snapshot if none exists;
  3. mines and validates tasks until the cost floor (8 valid tasks) is met, or the candidates run out;
  4. creates an experiment at the cost floor: the committed context against `--b`, or an A/A calibration when no second context exists;
  5. prints its preview: runs, estimated cost and detectable effect.

  It stops before any paid run unless `--yes` is given or the user answers yes at a prompt on a terminal. Each stage is skipped when already done, so running `start` again resumes. If the model A/B step has merged, `start` also accepts `--models A,B`.
- **North-star tracking:** computed from stored data, with no migration. `start` and `experiment report` show:
  - the time from the project's registration to its first decisive verdict (improved, regressed or no loss);
  - the total spend up to that verdict, agent and judge, through `run.Spend`;
  - "none yet" until a verdict exists.
- **Unchanged:** the engine, verdicts, statistics and every existing command's output. One exception: `experiment plan` reports a missing calibration as "calibrated when the experiment runs, about $X", where it used to fail.
- **Boundaries:** no new Go modules; Agentium never writes the user's repository or settings; no network beyond what setup and grading use today.

## Acceptance
1. **Lint:**
   - Editing `CLAUDE.md` in a session prints the size change and warnings within a second, at no cost.
   - A payload for a non-context file prints nothing.
   - Malformed input exits 0 with a one-line note.

   *Evidence:* CLI tests with recorded hook payloads, a timing assertion with a generous margin, and a real hook sample in this plan.
2. **Calibration inside run:**
   - An experiment with an uncalibrated arm calibrates it first, records the calibration as `run calibrate` does, and counts its cost in the spend and the preview.
   - A failed calibration stops the experiment before any task run.
   - A calibrated arm is not calibrated again.
   - Resume does not repeat a calibration.

   *Evidence:* hermetic CLI tests with a fake Claude Code.
3. **Start:**
   - On a fixture repository, `start` reaches a preview in one command, with no prompts and no paid runs.
   - A second `start` skips finished stages.
   - `--yes` runs the experiment.
   - With fewer than 8 valid tasks, it says how many it found and what to do.

   *Evidence:* hermetic CLI tests, and a real `start` on a public repository up to the preview (free) recorded in this plan.
4. **North star:** a project with a decisive verdict shows its time and spend. One with only inconclusive or exploratory verdicts shows "none yet". *Evidence:* tests over stored experiments.
5. **Compatibility:** existing tests and report golden files pass unchanged, except the `plan` readiness line named above, which gets its own golden update.
6. **Docs:** the README quick start becomes `agentium start`, with the manual commands kept below it; help texts; and the code map, if a package's responsibility changes.

## Work
Each step is one PR with green CI and a review.
- [x] **1. Context lint:** `context lint`, `--hook` and `--print-hook` (`internal/claudectx`, `internal/cli/context.go`). Done 2026-10-02.
  - **How it works:**
    - `claudectx.LintContext` resolves the context and separates problems from the other warnings. Problems are broken `@` imports (now also listed in `Context.Broken`) and `AGENTS.md` chains over 32 KiB. That is Codex's `project_doc_max_bytes`: Codex joins the `AGENTS.md` files from the root down to a folder and reads only the first 32 KiB of the whole. The lint sums each chain, from the files whether or not Claude Code loads them, skips vendored and test-data folders, and warns once, at the folder whose file tips the chain over, naming the files. Whether Codex follows `@imports` is unverified (H: it reads them as plain text), as in the study. The size loaded at session start is information only: the change against the project's most recent snapshot. Claude Code documents no per-file size warning for `CLAUDE.md` (it loads files up to 4 MiB in full), so none is added.
    - Plain `context lint` checks the working tree, like `context show`; `--ref REF` checks a commit. Problems print as `warning:` lines and the exit code stays 0, as `context show` and `context snapshot` do with the same warnings. The check is advisory, so there is no `--strict`. Without a registered project or a snapshot, a one-line note replaces the comparison.
    - **The database is read-only** (`store.OpenReadOnly`): no creation, migration or write lock, a 200 ms busy timeout and no retry. A schema this binary does not know gives "no comparison" (a newer one is worded separately; busy or corrupt databases are "busy or unreadable"). With no `-wal` file (no other process has the database open) it reads the file as immutable: no side files appear, and the size, modification time and `-wal` existence are compared before and after the read, discarding the result if anything changed. With a `-wal` file, a normal read-only connection is used, and SQLite may create or update its own `-shm`/`-wal` coordination files, which is harmless; the database file itself is never written.
    - `--hook` reads the payload from stdin (an oversize one is drained and ignored), finds the repository from the edited file's path (it must exist) and lints the working tree. It decides relevance in steps. The path alone rules out most edits: only files that load by presence (`CLAUDE.md`, `AGENTS.md`, `.claude/...`, `.mcp.json`) and documents by extension (so an imported `@test/README.md` counts) can be context. For a document, the context is resolved and the file must be in it, or be the target of a symlinked context file (`CLAUDE.md -> AGENTS.md`). Only then is the database read. The hook prints nothing after 2 s. A broad check on the path (name, document extension, `/.claude/`, `.mcp.json`) runs before git starts. It prints nothing unless `tool_name` is Edit, Write or MultiEdit (or absent) and the file is relevant. Bad input gives a one-line note. It always exits 0, and its text has no colors even under `FORCE_COLOR`.
    - **Output form:** one JSON object, `{"systemMessage": "..."}`. Claude Code documents `systemMessage` as a universal hook output field, a "warning message shown to the user", and its PostToolUse section does not list it among the discarded fields (checked 2026-10-02 against https://code.claude.com/docs/en/hooks, JSON output and PostToolUse). Plain stdout of a hook that exits 0 is not shown in the normal view, so it is not used.
    - `--print-hook` prints the settings object on stdout and its explanation on stderr, so the snippet can be piped. Its command is the absolute path of the running binary, not resolved through symlinks so a Homebrew-style link survives upgrades (a non-interactive shell may lack the user's `PATH`), with `"timeout": 5`; a warning on stderr appears when that path is in a temporary folder, as with `go run`.
  - **Known limit:** imports of non-document files (for example `@notes.txt`) are not detected as context by the hook.
  - **Measured:** the hook takes about 0.02 s on this repository (270 tracked files), three runs, and also 0.02 s while another process holds the database's write lock.
  - **Real hook sample** (this repository, after a one-line edit of `AGENTS.md`, with a snapshot `start` taken before it):

    ```text
    {"systemMessage":"Agentium context lint: AGENTS.md changed\nAt session start: about 3612 tokens (14.1 KB, estimated); +2 tokens against snapshot start (about 3610)\nNo problems found."}
    ```

    And `agentium context lint`:

    ```text
    Context lint of claude-feat-context-lint (working tree)
    At session start: about 3612 tokens (14.1 KB, estimated); +2 tokens against snapshot start (about 3610)
    No problems found.
    ```
- [ ] **2. Calibration inside `experiment run`:** after the model A/B experiment step, since both change `internal/experiment` and `experiment_run.go`.
- [x] **3. `agentium start` and north-star tracking** (2026-10-02, branch `claude/feat/agentium-start`). How it works:
  - **Code:** `internal/cli/start.go` (the handler, the stages) and `start_tasks.go` (supplying tasks); `internal/report/northstar.go` (the measure). Only new files, plus wiring: dispatch, help, `Env.Stdin`/`StdinTerminal`, the report's line, the README and the code map.
  - **Stages**, each skipped when done and printed as one line: registered (`init`'s own output the first time); a snapshot (arm A is `baseline`, saved from HEAD if the project has none, else the newest snapshot `--b` does not name; the source commit is shown, and a note says when HEAD's context has moved on); tasks; the experiment `quick-aa-<A>` or `quick-<A>-vs-<B>` (8 tasks × 1 run per arm, a sample of the ready ones, Sonnet, budget a quarter above the estimate or `--budget`); the preview (`LoadReview` and `Review.Write`); then the run.
  - **Tasks:** if the experiment exists already, this stage is skipped. Otherwise it counts the ready ones (`EligibleTasks`). Below the floor it validates tasks that lack a validation in some arm's context, then mines (`mine.Prepare`/`Import`, the default limits, the detected test command) and validates (`validateBatch`, 2 at a time, base plus each experiment snapshot) until 8 are ready, the history runs out, a round validates no task as valid, or 24 tasks (3 × the floor) are imported; the tasks set aside are printed with their reasons. Fewer than 8: it says how many it found and what to do, exits 1 and creates no experiment.
  - **The review gate stays.** Mined instructions are `needs_review`, and experiments refuse such tasks. `start` does not skip that: it stops and names `task show` and `task edit --reviewed`. The opt-in flag `--accept-mined` accepts, without a person's review, only the tasks `start` itself mined (names kept in `start-mined.json` beside the project's repository, so a later run knows them; a task from a pull request, a ticket or `task import` is never accepted). It checks solution headings, reference-file names in the instruction and unstated hidden-test requirements, prints the accepted names, and says that a message that explains the fix is not detected. **Known limitation, follow-up:** accepted tasks carry no mark that nobody read them; recording that needs a migration (0009 is taken by another branch).
  - **Budget:** the preview, the prompt and the printed command quote the budget a run would have: the lock's (or the design's), raised to `--budget`; a lower `--budget` is a usage error before any question, and the printed `experiment run` command carries `--budget` when it was given.
  - **The run:** `--yes`, or `y` at a prompt when stdin and stdout are terminals, calls `experimentRun`, so it behaves as `agentium experiment run NAME`. Otherwise it prints `agentium experiment run NAME`. Not ready (for example, a context not calibrated before step 2) it does not run, and exits 1 only with `--yes`.
  - **North star:** `report.LoadNorthStar` reads the project's registration, its locked experiments and its runs. It analyses each experiment as its report does; the first experiment (by the end of its last run) with a decisive verdict on its primary or guard metric (improved, improved but small, regressed, equivalent, no loss) gives the time from registration to that run's end, and the spend (`run.StoredSpend`: agent and judge) of every project run started up to its last run. Inconclusive and exploratory verdicts, and A/A calibrations (they answer nothing about a change), do not count. No migration. Only experiments with status done count. The line shows in `start` and in `experiment report` (terminal and Markdown; the JSON has `north_star`), set by `report.Load`; `Build` does not set it, so no golden file changed.
  - **Tests:** `start_test.go` (hermetic, with the fake Claude Code: a preview in one run with no paid run, a second run skips, `--yes`, too few tasks, `--b` versus A/A, the review wait, usage, the prompt only on terminals) and `report/northstar_test.go` (none yet; decisive with its time and spend, judge spend included, later runs left out; inconclusive, exploratory and A/A only).
  - **Real console sample,** `start` without `--yes` on a scratch Go repository of 11 commits, with `AGENTIUM_HOME` in a scratch folder (no paid runs; Claude Code 2.1.285; long lines cut). Mining, 8 validations in two contexts and the whole first command took 9 s (8 s of it validation, 2 at a time):
    ```
    $ agentium start
    Registered demo (project 1)
      ...
    Context: saved snapshot baseline from HEAD (a5816b4d7432): 1 file(s)
    Mining: 10 candidate(s) in 11 commit(s) read; imported 8 of 8 tried (verify: go test ./...)
    Validating 8 task(s) in 2 context(s), 2 at a time
      8 valid of 8, in 8s
    Tasks: 8 valid, 0 ready of the 8 an experiment needs: the others wait for your review
      Read each instruction for solution leaks: agentium task show NAME, then agentium task edit NAME --reviewed
      (or agentium start --accept-mined accepts the ones start mined without your review, after automatic checks that miss an instruction explaining the fix)
      waiting: add-trim-to-the-library-03008c9, add-last-to-the-library-28c606e, ...
    $ agentium start --accept-mined --budget 60
    Project demo: registered (skipped)
    Context: snapshot baseline from a5816b4d7432 (skipped)
    Accepted 8 mined instruction(s) without your review (--accept-mined): add-trim-to-the-library-03008c9, ...
      Only solution headings, reference-file names and unstated test requirements were checked; a message that explains the fix is not detected.
    Tasks: 8 ready (needs 8), in 0s
    Experiment quick-aa-baseline: created, 8 task(s) × 1 run per arm = 16 runs, budget $60.00

    note: no second context was given, so this is an A/A calibration of baseline ... It never counts toward the first decisive verdict.

    Experiment quick-aa-baseline: A/A calibration of context baseline
      ...
    Before it runs:
      ok       Claude Code 2.1.285 at /opt/homebrew/bin/claude
      MISSING  context baseline is not calibrated: agentium run calibrate --model claude-sonnet-5 --snapshot baseline
      ...
    Not ready to run: see above.

    First decisive verdict: none yet ($0.00 spent since init)
    Nothing was run: fix what is missing above, then agentium start --yes (or agentium experiment run quick-aa-baseline --budget 60)
    $ agentium start --budget 10
    agentium start: --budget $10.00 is below the experiment's $60.00: a budget can only be raised
    ```
    A later `start` printed four "(skipped)" lines, then the same preview, in 0.3 s. The "not calibrated" line is expected until step 2.
  - **Deviations from the brief:** the `--accept-mined` flag (without it the review gate makes the one-command preview impossible); A/A calibrations do not count toward the north star.
- [ ] **4. Real check (free up to the preview)** on a public repository, then docs.
- [ ] **5. README pictures** (the user's decision on 2026-10-02: refresh them once, after quick start, not before). The pictures date from 2026-09-30. Since then `plan`, `report` and the status line have changed, and the README's claim that they show today's output no longer holds.
  - **Redo from stored data with today's binary:** `experiment plan`, the run summary, `experiment report` and `run show`. Re-record the live status-line animation with the stand-in agent.
  - **Add:**
    - `agentium start`, as the first picture;
    - `task mine --dry-run`;
    - the report's Judge section from the `judge-check` experiment, whose judge called a passing run "partly" fixed.
  - **Caption:** state the date and the Claude Code version of the data.
  - **Tools:** `scripts/readme_images/`, saved from the 2026-09-30 session, standard library only:
    - `ptyrun.py` captures a command's styled output on a pseudo-terminal;
    - `ansi2svg.py` renders it as a terminal window;
    - `cast2svg.py` animates a timed recording given as JSON `[[seconds, text], …]`.

    A timed recorder for the animation is still to write, and a short usage note goes beside the tools.
  - **Cost:** free; no new agent runs.
  - Then archive this plan.

## Verification and handoff
Hermetic CLI tests with a fake Claude Code, `harness.py check changed`, CI, and a reviewer per step. The wave-2 exit gate (a decisive verdict on an external repository, about $60) is run separately, with the user's approval.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
