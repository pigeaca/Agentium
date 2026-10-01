# Task checks and context use

- Date: 2026-09-30
- Status: In Progress (2026-10-01): steps 1 and 3 run in parallel; step 2 follows step 1. No paid runs.
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
- [x] **3. Context use**, done by the coordinator. The reviewer requested changes twice and then approved: shell reads only as reading commands' file arguments; path-scoped rules and folder instructions counted for the files the agent worked with; only the project's and Claude Code's subagents named; recovery that survives a moved data folder and cancellation; `subagent_models` filtered in shared reports. On the real 16-run A/B, both arms load the same 6 files at start, no run opened a linked document, and one minimal-arm run started the `investigator` subagent.
- [ ] Archive this plan.

## Boundaries
- No paid runs and no new dependencies.
- Stored forms change only by adding fields, and older records and validations still load.
- No absolute paths, personal names or user-level skill names in records or reports.
- Neither check judges code quality; that is the [judge pilot](2026-09-30-judge-pilot.md)'s question.

## Verification
`harness.py check changed`, fake-`claude` end-to-end tests, golden reports, CI, and a reviewer per PR.

## Parallel ownership
All steps start from `origin/main` at `c0414eb`, except step 2, which starts from step 1's reviewed head. Worktrees live under `/Users/pigeaca/GolandProjects/Agentium-worktrees/`. A separate session owns `claude/fix/run-estimates` (the preview's usage and cost estimates): `internal/experiment/usage.go`, `plan.go`, and the preview lines of `internal/cli/experiment.go`.

| Step | Owner | Branch / worktree | Editable scope | Must not touch |
|---|---|---|---|---|
| 1 | `implementer` (Sonnet, medium) | `claude/feat/flaky-tasks` / `claude-feat-flaky-tasks` | `internal/task/validate.go`; `internal/cli/task.go`; one warning line in `experiment plan`'s "Before it runs" (`internal/cli/experiment.go`); `internal/experiment/design.go` (`Ineligible`'s message only); README's task section; tests | `internal/claude`, `internal/run`, `internal/report`, `internal/stats`; `internal/experiment` apart from `Ineligible`; the store's schema |
| 2 | the same `implementer` | `claude/feat/weak-tests` / `claude-feat-weak-tests` | `internal/task`; `internal/cli/task.go`; README's task section; tests | as step 1 |
| 3 | coordinator (Opus) | `claude/feat/context-use` / `claude-feat-context-use` | `internal/claude` (transcript parsing); `internal/run` (records); `internal/report` and its golden files; `internal/cli` (report wiring only); added while implementing: `internal/claudectx` (rule patterns, subagent names) and `internal/snapshot` (`Apply`), with counting in `internal/report` instead of `internal/experiment`; architecture and README | `internal/task`, `internal/cli/task.go`, `internal/experiment/usage.go`, `plan.go` |

Each implementer pushes its branch and returns a handoff; the coordinator runs the reviewer, opens one PR per step and integrates.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
