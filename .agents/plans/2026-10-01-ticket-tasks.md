# Tasks from tickets, graded without tests

- Date: 2026-10-01
- Status: Planned, not started. Needs the [per-run judge](2026-10-01-llm-judge.md). Live Jira access needs its own approval (credentials and a live integration). The real check is paid and needs approval.
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
- [ ] **1. Ticket files and judge-graded tasks** (`internal/task`, CLI).
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
