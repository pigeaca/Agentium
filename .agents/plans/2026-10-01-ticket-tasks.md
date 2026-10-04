# Tasks from tickets, graded without tests

- Date: 2026-10-01
- Status: In Progress (2026-10-02): resumed without the judge gate, by the user's decision ("integrate it without any proofs"). Step 1 merged (#55). Steps 2 and 3 done on `claude/feat/judge-grader` (2026-10-04, review notes fixed the same day). Step 4, the paid real check, ran on 2026-10-04 ($1.06; [results](../../docs/research/2026-10-04-judge-grading-check.md)): one judge-graded task, 4 runs, all graded fixed (5 of 5), and the grades are right; but one easy task with no wrong fix to reject does not validate the judge. Because the pilot's grading-without-tests result was INCONCLUSIVE and step 4 did not change that, judge-graded outcomes stay labeled "judge, unvalidated", kept in separate metrics, exploratory, and never count as a decisive verdict; an experiment with judge-graded tasks never counts toward the north star until a check with negative controls validates the judge. Step 5 (live Jira) is planned separately.
- Scope: the user's question on 2026-09-30 about using "some specific Jira task" in an A/B, and the [decision](../decisions/2026-10-01-llm-judge-alongside-tests.md) that lists grading tasks without tests as a later use.

## Why
- **Today:** a task needs hidden tests. `task add --instruction-file` can already take a ticket's text, but a fix whose pull request added no tests cannot be graded.
- **Most tickets are like that.** For them, the judge against the reference fix (the merged pull request) is the only grade available.
- **The pilot's limit:** this use was INCONCLUSIVE there. The judge judged all 4 failing runs with a change not fixed, but 3 came from one task where it also flagged passing runs. So these grades are shown as judge grades, apart from test results, never mixed with them.

## Outcome
- **Creating a task from a ticket:** `task add NAME --base REF --solution REF --ticket-file FILE` reads an exported ticket (Jira's JSON export, or Markdown: title, description, acceptance criteria) into the instruction. As today, the user reviews the instruction before the task is used.
- **Grading mode:** a task whose solution has no test changes becomes judge-graded (`grading: judge`). `task validate` checks that its reference diff is non-empty and that the instruction states what to do.
- **Grading a run:** a run of a judge-graded task is graded by the judge's majority of 5 repeats, and "yes" counts as fixed. The run record and reports say "judge-graded" wherever a pass is shown.
- **Experiments:**
  - an experiment can mix test-graded and judge-graded tasks;
  - success is reported in two separate metrics: tests' success and the judge's success;
  - a verdict is never computed over the two together;
  - floors apply to each metric separately.
- **Later step (approval): live Jira.** `task import --jira KEY` fetches the ticket and finds its linked pull request. It needs a Jira URL and an API token, under the [secrets](../rules/secrets.md) rules.

## Acceptance
1. **Ticket files:** the Jira JSON and Markdown parsers have fixtures; the instruction is built from title, description and acceptance criteria; HTML and Jira markup are converted to plain text.
2. **Judge-graded tasks:** created when the solution has no tests, shown by `task list` and `task show`, and validated as above.
3. **Grading:** a judge-graded run passes on the judge's "yes", by majority of 5. A judge error leaves the grade pending, graded again from the stored change (never by running the agent again); a refusal, a reply still malformed when asked again, or a tie leaves the run ungraded for good (left out, not tried again, counted per arm). Never failed.
4. **Experiments and reports:**
   - the two success metrics are kept apart, with their own floors;
   - every judge grade is labelled;
   - a report of test-graded tasks only is unchanged (golden files).
5. **Real check (paid; approval):** one judge-graded task from an Agentium pull request without tests, 2 runs per arm.
6. **Docs:** README (tickets, and judge-graded tasks with their limits) and help.

## Work
- [x] **1. Ticket files and judge-graded tasks** (`internal/task`, CLI).
  - **Tickets:** `task add --ticket-file` reads Jira JSON exports (plain text, ADF or wiki markup) and Markdown tickets, up to 1 MiB, into an instruction: title, description, and "Acceptance criteria:".
    - The ticket key is stored as the source ("ticket ABC-123").
    - Ticket tasks need review, and sections such as "Root cause" or "Fix" are named as possible leaks.
  - **Grading mode:** `tasks.grading` is `tests` or `judge` (migration 0008; old tasks are `tests`).
    - A ticket whose solution has no tests becomes judge-graded.
    - A hand-written instruction needs `--judge-graded` for that (a choice made in review: safer than turning every testless solution into a judge-graded task).
    - `task import` never makes judge-graded tasks.
  - **Validation:** `task validate` checks the instruction and the reference's code diff, and warns when the diff is longer than the judge reads.
  - **Refusals:** experiments and `run once` refuse judge-graded tasks until step 2.
  - **Review:** approved with notes (an ADF panic, a heading inside the criteria, and others), all fixed.
  - **Limitation:** a Jira text field is read as wiki markup, so Markdown in it loses its sections.
  - **For step 2:** add the grading mode to the locked task's digest, and remove the `run once` and experiment refusals together with their tests.
- [x] **2. Grading and experiments:** the judge as grader, separate metrics and floors (2026-10-04, branch `claude/feat/judge-grader`).
  - **Grading:** a judge-graded run's verification commands do not run; the judge compares its `agent.diff` with the reference's code diff, 5 calls on the default judge, and `judge.Grade` decides: a majority of the 5 requested saying "yes" passes, a majority saying otherwise fails, a run that changed no code fails. The agent's run always stands (its outcome is the agent's), and the grade never fails it for want of answers:
    - **Pending** (`run.NeedsGrading`): a judge error (a usage limit, a call that cannot be made, timeouts, error results, the call cap, an interrupt, Agentium dying while grading) leaves the grade open. The run settles its slot, so the scheduler never runs the agent again; the analysis counts it nowhere. The execution grades it again from its stored records (`Env.Regrade`, `experiment/regrade.go`): before the runs on a resume (crash recovery included: the run is persisted pending before the first call and after each call's cost), after the runs, and before each seq-v1 look, which waits for every grade of its stages (`slotsDone`). A re-grade continues the verdict (`judge.GradeRun`: answers and refusals stand, only unsettled calls are asked), holds the grading's cap against the budget, and stores the record and its `passed` column (`store.SetRunGrade`). A usage limit pauses the experiment without counting; `run.MaxGradeErrors` (3) attempts that end in other errors leave the run ungraded, and passes after one that left grades pending wait the retry backoff (30 seconds, then 2 minutes). A stored run's leftover `judge` folder (a config folder that may hold the sign-in) is removed on recovery, as the pair judge's was.
    - **Ungraded** (`Record.Ungraded`): a refusal, a reply still malformed when asked again (`Verdict.Refused`), or a tie, which leave no majority within reach (`judge.Open`), are final: left out of every metric, never a fail or a pass, never tried again. `UngradedCheck` counts them per arm, as `SandboxCheck` does flagged runs (same threshold, a sensitivity analysis with them counted), and demotes every verdict they would feed (cost, time, output tokens; never the tests' success) when the arms differ or counting them changes it.
    - The verdict is `Record.Judge` (same spend accounting), `Record.GradedBy` is `judge`; judge-graded runs get no second-opinion judge. The grading prompt is its own (`judge.GradingVersion` 2, recorded in each grading verdict): the instruction cut at 20,000 characters, the tags' content declared data in the prompt and the system prompt, closing tags neutralised in everything quoted. The second opinion's prompt stays the pilot's (version 1).
  - **Experiments:** `--task` names a judge-graded task; samples and `start` never draw one. The design records `judge_graded` and `judge_grading` and is stored as version 5 (an older Agentium refuses it); the lock's task digest covers `grading` (test-graded digests unchanged), and a task whose grading changed since `experiment new` refuses the lock.
  - **Metrics and floors:** success is the test-graded tasks' alone; `judge_success` (the judge says fixed) is the judge-graded tasks', always exploratory, with success's floor counted over its own tasks; no verdict is computed over both. Cost, time and tokens count every graded run of both kinds; a fair judge-graded run without a grade (never stored so) would be left out of all of them.
  - **seq-v1:** looks count tasks with graded cost in both arms, of both kinds; a stage waits for its pending grades, so a look never sees a run whose grade can change later.
  - **North star:** an experiment with judge-graded tasks never counts, whatever its verdict, until step 4 validates the judge (its cost verdict has no tests' guard over those tasks); the north star line and JSON (`left_out`) say so. `experiment new` warns when the test-graded tasks are fewer than success's floor.
  - **Budget:** each judge-graded run holds its grading at its cap (5 × 2 × $0.50 = $5.00, the per-call overshoot allowance for any other judge) in the reserve (`Plan.CapOf`), the worst case, the minimum budget and the calibration check; the estimate adds 5 × $0.065 a run. `run once` names the grading's cap in its consent line.
- [x] **3. Reports and docs** (2026-10-04, same branch).
  - **Labels:** `run once` and `run show` (`judge: fixed (4 of 5)`, a grading and a judge line), their JSON (`graded_by`, `judge_grade`), the plain progress line, the dashboard (the grading box "the judge" outside the sandbox outline, the result "judge says ✓ fixed", the log line `✓ judge: fixed … (4 of 5)`), `experiment show` (a line per arm; JSON `judge_graded`, `judge_fixed`), and the report: a headline and a metrics row for "The judge says fixed", "Success (passed the tests, test-graded tasks only)", a judge line, `(judge)` in the per-task table and the "where they differ" grid, both kinds in the answer box and the version boxes, a note; JSON `judge_grading`, `judged`, `graded_by`, `judge_pass_at_1`.
  - **Unchanged:** reports of test-graded tasks are byte for byte origin/main's (100 files: Markdown, JSON, `--details` plain and colored, the terminal view at 60, 80 and 120 columns plain and colored, for every test-graded fixture).
  - **Docs:** the guide ("Tasks from tickets, graded by the judge", the JSON fields), help, the README's commands table, the code map; previews `docs/images/console-run-judge-graded.svg` and `console-report-judge-graded.svg` (test scenes).
  - **Labels (review fixes):** `run list` shows `yes (judge)`, `no (judge)`, `pending (judge)` or `ungraded (judge)`, and its JSON rows `graded_by` (absent for the tests'); progress, the dashboard, `experiment show` (`judge_pending`, `judge_ungraded`) and the report (per-arm counts, the demotion, the "not counted" line) name pending and ungraded runs.
  - **Review fixes (2026-10-04):** F1 pending and ungraded grades as above; F2 the north star and the floor warning; F3 the grading prompt; F4 `run list`; F5 tests that kill the review's surviving mutations (the second-opinion skip, the lock-time refusal, the grading estimate, the calibration's pair cap, the plan's slot caps); F6 the instruction cut and `task validate`'s warning.
  - **Limitations:** the judge reads the agent's diff, which the agent wrote and can use to argue its case (the grading prompt marks it as data and neutralises its closing tags; nothing stops an argument written as code comments); a crash during a grading attempt loses that attempt's answers (persisted: its spend), so their calls are asked again; `run once` makes one attempt and keeps a pending grade (nothing re-grades a run outside an experiment); a pair whose runs' grades land after the runs is compared on the next resume; each re-grade attempt holds the full grading cap; re-grade calls, like the second opinion's, are not gated by `--usage-limit` (a real limit pauses them); an older Agentium (before design version 5) refuses these experiments, but its north star and report do not check the design version, so an older binary reading this database counts a judge-graded experiment in its north star and shows its runs as test-graded (cannot be fixed in old code); no paid check yet (step 4).
- [x] **4. Real check (paid; approval).** **Done 2026-10-04** (branch `claude/feat/real-checks`); results in [docs/research/2026-10-04-judge-grading-check.md](../../docs/research/2026-10-04-judge-grading-check.md).
  - *Task.* `fix-small-mining-round`, judge-graded, built from Agentium commit `2623356b` (fix to `start`'s mining-stop, one file, no test changes), base its parent. The instruction states the symptom and the wanted behavior without naming the fix.
  - *Experiment.* A/A, `--task fix-small-mining-round --goal better --repeats 2` = 4 runs, judge = claude-opus-5-5 high, majority of 5, `--run-budget 1.25 --budget 33` (preview worst case $25.60 ≤ $33, expected $6.30). Spent **$1.06** ($0.46 judge).
  - *Result.* All 4 runs graded `fixed (5 of 5)`; 0 pending, 0 ungraded; ~$0.115 per grade. All four agents produced the reference fix (`mined > 2`, equivalent to the reference `mined >= 3`); the judge's reasons correctly identify the equivalence and that the round cap is untouched. The grades are right.
  - **Does this validate the judge? No.** One easy task where the correct fix is nearly forced, all four runs correct, no incorrect-but-plausible fix to test false positives. It shows the mechanism works end to end and the judge gives no false negatives on an obviously-correct fix — weak evidence, one task, all positive, no negative control. The plan's validation criterion is not met: judge-graded outcomes stay "judge, unvalidated", kept apart, and a judge-graded experiment still never counts toward the north star.
- [ ] **5. Live Jira (credentials and integration; approval):** planned separately when wanted.

## Boundaries
- No live network integration until step 5 is approved. No new Go modules.
- Judge grades never merge with test grades.

## Verification
Fixtures, unit, CLI and golden tests; `harness.py check changed`; CI; a reviewer per step; one paid real check.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
