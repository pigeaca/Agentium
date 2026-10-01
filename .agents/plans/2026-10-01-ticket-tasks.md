# Tasks from tickets, graded without tests

- Date: 2026-10-01
- Status: In Progress (2026-10-01): step 1 in its PR. Step 2 needs the [per-run judge](2026-10-01-llm-judge.md)'s step 2 (#54). Live Jira needs its own approval (credentials and a live integration). The real check is paid and needs approval.
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
3. **Grading:** a judge-graded run passes on the judge's "yes", by majority of 5. A judge error leaves the run ungraded (infra), never failed.
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
- [ ] **2. Grading and experiments:** the judge as grader, separate metrics and floors.
- [ ] **3. Reports and docs.**
- [ ] **4. Real check (paid; approval).**
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
