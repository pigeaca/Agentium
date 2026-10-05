# A quieter console and tasks that can answer

- Date: 2026-10-05
- Status: Planned (2026-10-05). It waits for the user's approval of this plan and of the [open decisions](#open-decisions-for-the-user). Nothing is built yet. The one paid step (T1's real check) needs its own approval, with a preview.
- Scope: the user, 2026-10-05, after a review of the project and console mock-ups shown in the session: "I'd like first 3 variants Report Preview Tasks + last `$ agentium experiment run opus-vs-sonnet`. Plan UI changes, README update and suggested tool improvements." Agentium is its owner's personal tool ([decision](../decisions/2026-10-05-personal-tool.md)), so each step is judged by whether it helps the owner decide something about their own setup.

## Why
What the review measured on 2026-10-05, from the data folders of the earlier checks and from the code:
- **Tasks often cannot tell two setups apart.**
  - In the model A/B on samber/lo ([report](../../docs/examples/model-ab-report.md)), 6 of 8 tasks ended the same in both arms. Two of them failed in both, and the report counts them in one grey line.
  - In the 16-run context A/B ([report](../../docs/examples/context-ab-16-report.md)), 16 of 16 runs passed.
  - A task's text is the raw commit message. In this repository the messages describe the fix; in samber/lo they are terse, and the hidden tests can need names the text never gives.
- **A preview does not say when a question is too small.** The 16-run context A/B could see a cost change of about 25% or more. The context was 4,575 tokens smaller; with 97% of the input read from the prompt cache, that can save about 4% of a run (an estimate from the runs' records: 38 requests a run, $1.23 a run). It ended "not sure" after $19.65.
- **A pass is an exit code.** A run passes when the test command exits 0, with no proof that the hidden tests ran ([#160](https://github.com/pigeaca/Agentium/issues/160)).
- **Passes get no picture.** The report draws a range bar for cost and only words for passes.
- **The live dashboard repeats itself.** It animates the same four steps on every run, shows only on a terminal, and is about 2,600 lines with 1,600 lines of tests. The user, 2026-10-05: "right now we have like animated visualization of procces and i dont know do we need it".
- **The owner pays by subscription.** The budget that matters is the plan's five-hour limit; the model A/B used about 4% of one.
- **Runs leave folders behind.** Each run with the owner's login leaves a session folder in Claude Code's project list, and `agentium clean` does not remove them.

## Outcome
Four screens change, the README follows them, and five changes make the answers more useful. The mock-ups the user chose are described in words here; the pictures arrive with each step's PR.
- **Report** (`experiment report` on a terminal): a bar for cost and a bar for passes; every task in groups, with each version's cost as a bar; the plan's share.
- **Preview** (`experiment plan`, and inside `start`): "can it answer?", and the plan's five-hour limit as a bar.
- **Task list** (`task list`): each task's last runs and what the task tells you.
- **Run** (`experiment run`): a quiet view by default: progress, spend, what runs now, the answer so far, the last results. No step boxes and no moving dot.
- **Tool:** task text drafted from the change, proof that the hidden tests ran, checks for the owner's own rules, cleanup of what runs leave behind, and a skill so that agents can run the tool.

## Acceptance
Fixed before implementation. Steps S1–S5 and T1–T5 are in [Work](#work).

**Screens**
1. **Report: both answers drawn (S1).** The answer box has a range bar for cost and one for passes. A metric without a verdict gets a grey bar, the words the report uses today ("too few to tell") and one line that names what would settle it. *Evidence:* the report scenes' goldens redone, plus a scene where passes have a verdict; a real sample of `opus-vs-sonnet` from a copy of its data folder.
2. **Report: every task (S1).** The task block lists every counted task in groups, in this order:
   - the tasks where the versions differ;
   - the tasks both failed, labelled "check these tasks", with the command that shows a task;
   - the tasks both passed: at most 6 rows, then a count.

   Each row has each version's ✓ and ✗, its cost as a bar on one scale, and the cost. A last line counts the tasks each version was cheaper on. Under 70 columns the bars are left out. *Evidence:* goldens at 60, 80 and 120 columns, in color and ASCII.
3. **Report: the plan's share (S1).** With a subscription sign-in, the line under the question states the experiment's share of the plan's five-hour limit, from the same per-run estimate the preview uses. With an API key it is absent. *Evidence:* a unit test on recorded readings; the sample above (about 4%).
4. **Report files unchanged (S1).** `--details`, `--markdown` and `--json` print what they print today. *Evidence:* their goldens do not change.
5. **Preview: can it answer? (S2).** A "can it answer?" panel shows the smallest change this size can see: percent of cost, or points of passes for `--goal better`.
   - For a context experiment whose two contexts are calibrated, with at least 3 earlier runs on the model, it also shows the change expected from the contexts' size alone, and "likely result: not sure" when that is below what the size can see. It says that size is not everything: a context that changes what the agent does can move cost more.
   - For a model experiment it shows no expected change.
   - It never blocks a run.

   *Evidence:* a unit test that reproduces the 16-run context A/B (4,575 tokens, 38 requests a run, $1.23 a run: about 4%, against 25% or more); goldens; `experiment plan --json` carries the same numbers under new keys.
6. **Preview: the plan's limit (S2).** With a subscription and a current reading, a "your plan" panel shows the five-hour limit as a bar: used now, the mark where runs pause (`--usage-limit`), how many runs fit now and how many limits the experiment needs. Without a reading it keeps today's line. *Evidence:* goldens for both cases.
7. **Preview: tasks that cannot separate (S2).** For a `--goal better` experiment, "before it runs" names the tasks that passed every time (4 or more graded runs) and those that never passed (2 or more). Sampling does not change. *Evidence:* goldens and a CLI test.
8. **Task list (S3).** Per task, `task list` shows its last graded runs as ✓ and ✗ (all versions together, newest last, at most 8) and one tag:
   - what blocks it (invalid, flaky, not reviewed, unstated requirements);
   - else "not run yet", or "passed n of n" with fewer than 4 runs;
   - else "always passes: too easy", "never passed: check the task text" (2 or more runs) or "separates".

   A last line counts the tags, and `pool status` gains the same line. `task list --details` prints today's table. `task list --json` gains `graded_runs`, `passed_runs` and `tells`. *Evidence:* CLI tests on a store with each case; goldens; the contract golden shows additions only.
9. **Quiet run view (S4).** On a terminal, `experiment run` (and `start --yes`) shows by default:
   - the question;
   - runs done of all, with a bar and the time;
   - spend against the budget, and the plan's share;
   - one line per running run: version, task, step, time;
   - the answer so far, each version's passes and the next check;
   - the last 3 results.

   It redraws when something changes, and at most once a second for the clock. It covers every state the dashboard covers: getting ready, calibration, a pause at the plan's limit, a budget stop, each check of the answer, a blocked or left-out grade, the host grader's warning, judge-graded runs, retries and the end. *Evidence:* goldens of the dashboard's scenes (start, mid, moments, host, judged, final) in the quiet view, in color and ASCII; a recording with the test stand-in.
10. **Views (S4).** `--view dashboard` (and `AGENTIUM_VIEW=dashboard`) gives the quiet view. `--view flow` gives today's step boxes ([decision 1](#open-decisions-for-the-user)). `--view log` is unchanged. Off a terminal, with `--json` or with `NO_COLOR`, the output is today's, byte for byte. *Evidence:* the plain and log goldens do not change; a CLI test per view name.
11. **README, gallery and guide (S5).**
    - The README leads with the questions the tool answers well, shows the new report first, lists what the tool has answered so far (with links to the stored reports) and says it is a personal tool, shared as is.
    - The gallery and the guide show the four screens.
    - Every picture is recorded from real output with `scripts/readme_images`, and none holds a home path or a name.

    *Evidence:* `check docs`; the README's own grep over the SVGs.

**Tool**
12. **Drafts (T1).** `task draft NAME` asks Claude, without tools, for a task text from the commit message, the reference change and the hidden tests: the problem and the names the tests need, never the fix.
    - A draft is stored beside the instruction and never replaces it by itself.
    - A draft is refused when the fairness check finds an unstated requirement in it, or the giveaway check finds a name only the reference has.
    - `task show` prints both texts. `task edit NAME --accept-draft` puts the draft in place; the task still awaits review unless `--reviewed` is given.
    - Each call has a cap and counts as paid, with the usual consent.

    *Evidence:* tests with the stand-in for each path (stored, refused for a gap, refused for a giveaway, cap reached, a crash between the call and the store); a forward migration test.
13. **Drafts, a real check (T1; paid, own approval).** Drafts for this repository's valid tasks and for the samber/lo tasks of the model A/B. Recorded in this plan: how many pass both checks, how many the owner accepts as they are, and the spend.
14. **Proof that the hidden tests ran, for Go (T2).** For a task whose hidden tests are Go tests, a grade is a pass only when `go test -json` reported a pass for each of the task's own hidden tests. A zero exit without that is a failure, with the note "the hidden tests did not run".
    - The issue's three cases fail: unchanged code, a `TestMain` that exits, an `init` that exits.
    - New experiments record the rule in their lock. Locked experiments keep theirs, and their verdicts do not change.
    - Validation uses the same proof.

    *Evidence:* the issue's reproduction as tests; a lock compatibility test; the contract golden.
15. **Rule checks (T3).** `agentium check add NAME (--ran TEXT | --changed GLOB | --not-changed GLOB)`, `check list` and `check rm` keep checks in the project's settings, in the data folder.
    - Reports count, per version, the runs that met each check, from the stored transcript and change. Experiments that ran before a check existed are counted too.
    - The terminal report draws them as bars under "what the agents did"; the Markdown and JSON reports gain rows and keys.
    - They are counts, never a verdict.

    *Evidence:* tests on recorded transcripts; a real sample on the stored runs of the 16-run context A/B (free).
16. **Cleanup (T4).** A run removes its own session folder from Claude Code's `projects` folder once its transcript is in the records. `agentium clean` lists the session folders that earlier runs left, only those whose path is a workspace of this data folder, and removes them with `--yes`. Nothing else in Claude Code's folder is touched. *Evidence:* tests with a stand-in config folder, one of them holding a folder whose name only looks alike; a list-only sample on the owner's machine.
17. **A skill for agents (T5).** A skill tells a coding agent how to use Agentium for the owner: free commands with `--json`, the preview first, a paid run only after the owner's yes in the conversation, then the report in plain words. *Evidence:* `check docs`; one session in which an agent follows it up to the preview (free).

## Work
Each step is one PR with green CI and the usual two reviews; S5 and T5 are docs-only. Order: S1 and S4 together; then S3, then S2 (both change the contract golden); then S5; then T1 and T2 together; then T3 and T4 together; T5 last. At most two implementers at a time: they share the owner's plan.

- [ ] **S1. Report.** `internal/cli` (`reportview.go`), `internal/report` (the plan's share), goldens. Contract: none. Risk: low; an implementer.
- [ ] **S2. Preview.** `internal/experiment` (the size estimate, a pure function beside `Detect`; a hotspot, so nothing else touches the package meanwhile), `internal/cli` (`planview.go`, `json_experiment.go`). Contract: new JSON keys. Risk: medium; an implementer.
- [ ] **S3. Task list.** `internal/cli` (`task.go`, `json_task.go`, `pool.go`). Contract: a new flag and JSON keys. Risk: low; an implementer.
- [ ] **S4. Quiet run view.** `internal/cli` (`rundash.go`, `runview.go`, `runscreen.go`, the recorder). It starts from the dashboard's one-line layout (`armText`, today's fallback for small terminals) and names the step by the run's agent, so it lands after the Codex adapter (#156). Contract: a new `--view` value; the guide's list of renamed flags says what `dashboard` now shows. Risk: medium (the live region); an implementer.
- [ ] **S5. README, gallery, guide.** New pictures of the four screens; the README as criterion 11 says. Later steps add their own commands to the README and the guide.
- [ ] **T1. Drafts.** `internal/task` (the draft and its two checks), `internal/store` (a forward migration), `internal/claude` (the call the judge already makes), `internal/cli`. Then the real check (criterion 13). Risk: high (money, consent, persistence); implementer-critical.
- [ ] **T2. Proof that the hidden tests ran.** `internal/run` (grading), `internal/task` (it already lists a task's own Go tests, in `gofilter.go`), `internal/experiment` (the lock). Closes #160. Not at the same time as the Codex plan's step 5: both change the lock. Risk: high; implementer-critical.
- [ ] **T3. Rule checks.** `internal/run` (reading stored transcripts again, through `claude.Parse`), `internal/report`, `internal/cli`, `internal/store` (settings). Contract: new commands, report rows and keys. Risk: medium; an implementer.
- [ ] **T4. Cleanup.** `internal/run` (`run.go`, `clean.go`), `internal/claude` (`SessionFolder`). Risk: high (it deletes in the owner's Claude Code folder); implementer-critical.
- [ ] **T5. A skill for agents.** `.agents/skills/` and its adapter. Where else it is installed is [decision 5](#open-decisions-for-the-user).

## Boundaries
- **Console only.** No web UI, no new module, no background process ([console decision](../decisions/2026-09-30-console-instead-of-web-ui.md)).
- **The console design's rules stay:** plain words, the palette by role, restraint ([plan](archive/2026-10-03-console-design.md)). Its default live view, the step boxes with a moving dot that the user settled on 2026-10-03, gives way to the quiet view (the user, 2026-10-05).
- **Machines see additions only:** plain output and exit codes do not change; `--json` and the Markdown report only gain keys and rows.
- **Locked experiments keep** their method, their grading rule and their verdicts.
- **The data folder migrates forward only** (T1, T3).
- **Nothing is written to the owner's repositories.** T4 is the only step that deletes outside the data folder, and only what Agentium's own runs created.
- **No sampling change.** Tasks are tagged and warned about, never picked or dropped by their history; that needs a statistics note first (Later).
- **Not changed by this plan:** Codex, containers, the judge, the pull-request cost screen.

## Later (not in this plan)
- Picking tasks by their history, blind to the arm, after a statistics note and a simulation.
- Verdicts for rule checks.
- The proof for Cargo, Maven, Gradle, pytest and TypeScript tests.
- `experiment list` with each answer in words; a board of setups against tasks.
- A mining score that prefers larger commits (today: `pool update --max-lines` and `--max-files`).

## Verification
- Per PR: `python3 scripts/harness.py check changed`, the goldens named in its criteria, and a real console sample in the PR (from the recorders, with the test stand-in: nothing paid).
- Contract changes are declared in the PR; `pr land` reads the contract golden.
- Paid: only criterion 13, after its own approval.
- This plan's PR: `check docs`.

## Open decisions (for the user)
Each has a recommendation; an answer of "as recommended" approves them all.
1. **The step boxes.** Keep them as `--view flow`, frozen, and delete them the first time they need a fix (recommended); or delete them in S4.
2. **Today's `task list` columns** (source, graded by, tests, files). Move them to `task list --details` (recommended), or keep every column.
3. **Drafts and `--accept-mined`.** A draft always needs the owner's review; `--accept-mined` never accepts one (recommended).
4. **The drafter's model.** `claude-sonnet-5-5` at its default effort, capped at $0.50 a call (recommended).
5. **The agents' skill.** Keep it in this repository, and the owner copies it into their own Claude Code skills folder (recommended); or the step installs it there after a yes.
6. **Plans in progress, now that the tool is personal.** Recommended:
   - park container mode: leave #161 unmerged;
   - park the pull-request cost screen and the judge's next steps;
   - keep Codex;
   - keep TypeScript and monorepo modules only if the owner will use Agentium on such a repository.
7. **The README's words** for "a personal tool, shared as is" and its new first questions: the owner approves them in S5's PR.

## Risks
- **The quiet view loses a state the dashboard shows** (a blocked sandbox, a pause). The dashboard's scenes are the checklist.
- **The expected change is an estimate from size alone.** The panel says so and never blocks a run.
- **A draft is written by a model that has seen the fix.** The two checks and the owner's review stand between a draft and an experiment.
- **The Go proof does not stop code that prints forged test results.** It stops tests that never ran.
- **Tags from few runs mislead.** Under 4 runs the list shows counts, not "too easy".
- **T2 and Codex step 5 both change the lock:** one at a time.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <failed check → fix → re-run cycles>
- User corrections: <count> (<what the user had to correct>)
- Review: <approve | changes requested, n fixed | not required>
