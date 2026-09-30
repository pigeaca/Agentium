# Task checks and context use

- Date: 2026-09-30
- Status: Planned, not started. No paid runs.
- Scope: from the user's questions on 2026-09-30: "who writes test scenarios, and how do we know what is good?" and "what changed in context usage?". Two task checks make weak tasks visible before paid runs. Reports then show which parts of each arm's context the agent actually used.

## Why
- **Flaky tests.** `task validate` runs each stage once, so a flaky test passes validation and then adds noise to every experiment that uses the task.
- **Weak tests.** Nothing detects parts of the reference solution that the hidden tests never exercise, where an agent can pass without doing that part.
- **Context use.** Reports show tokens, cost and turns per arm, but not which context files, skills or subagents the agent used. The transcript parser already sees file paths, but they are not stored (`json:"-"`, because absolute paths are personal). Skill calls are only counted per tool, and subagent types are kept only for the model check. Each run's `stream.jsonl` is kept, so older runs can still be analysed.

## Acceptance
1. **Flaky tasks.**
   - `task validate --repeat N` runs every stage N times; the default of 1 keeps today's behavior.
   - A stage whose runs disagree makes the task `flaky`, which is invalid for experiments, with a message naming the arm and stage.
   - `experiment plan` lists, under "Before it runs", tasks whose latest validation used fewer than 3 repeats. This is a warning, not a gate.

   *Evidence:* tests with a verification command that alternates between passing and failing.
2. **Weak tests.**
   - `task validate --weak-tests` removes one hunk of the reference's non-test changes at a time, then reruns the verification with the hidden tests.
   - Hunks whose removal still passes are listed as "not tested by the hidden tests" in `task validate` and `task show`, and `task list` shows their count.
   - It is a warning, not a gate, because logging, comments and docs need no test.
   - A hunk whose removal breaks the build counts as tested. The check works the same for every language, and `--max-hunks` (default 20) caps its cost.

   *Evidence:* a fixture task with one tested and one untested hunk.
3. **Context use per run.** Each run records, from its transcript:
   - which of its arm's context files it read after start: on-demand rules, linked documents, skill files;
   - which of the project's skills it invoked;
   - which subagent types it used.

   Records store only repository-relative paths of the arm's own context and the project's own skill names, nothing else. *Evidence:* fake-`claude` transcripts with reads, a skill call and a subagent.
4. **Context use in reports.**
   - Terminal, Markdown and JSON reports show per arm how many runs used each item, for example "docs/harness.md: read in 7 of 12 runs".
   - Files loaded at start are listed once as "loaded at start", not per run.
   - Runs recorded before this change are analysed from their stored `stream.jsonl` when it exists; otherwise they show "not recorded".

   *Evidence:* golden reports, and a real console sample on the acceptance data.
5. **Docs:** `task` usage text, the README's task section, and the architecture's code map where responsibilities change.

## Work
Three PRs, each with green CI and a review.
- [ ] **1. Flaky tasks** (`internal/task`, `internal/cli/task.go`, the warning in `internal/cli/experiment.go`).
- [ ] **2. Weak tests** (the same files, after step 1).
- [ ] **3. Context use** (`internal/claude` parsing, `internal/run` records, `internal/experiment` counting, `internal/report`). It runs in parallel with steps 1–2 in its own worktree.
- [ ] Archive this plan.

## Boundaries
- No paid runs and no new dependencies.
- Stored forms change only by adding fields, and older records and validations still load.
- No absolute paths, personal names or user-level skill names in records or reports.
- Neither check judges code quality; that is the [judge pilot](2026-09-30-judge-pilot.md)'s question.

## Verification
`harness.py check changed`, fake-`claude` end-to-end tests, golden reports, CI, and a reviewer per PR.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
