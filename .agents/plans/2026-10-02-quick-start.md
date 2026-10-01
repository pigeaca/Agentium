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
  - the 32 KiB cap;
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
- [ ] **1. Context lint:** `context lint`, `--hook` and `--print-hook` (`internal/claudectx`, `internal/cli/context.go`).
- [ ] **2. Calibration inside `experiment run`:** after the model A/B experiment step, since both change `internal/experiment` and `experiment_run.go`.
- [ ] **3. `agentium start` and north-star tracking.**
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
