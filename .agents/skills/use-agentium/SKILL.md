---
name: use-agentium
description: Use when the owner asks what a model, a context file, a rule or a skill changes for their coding agents on this repository, in cost or in results.
---

# Use Agentium

Agentium is a command-line tool that runs coding agents on tasks from the owner's own repository, and compares models or versions of the project's context (AGENTS.md, CLAUDE.md, skills, rules) on cost and passed tasks. It never writes to the owner's repository.

## Free commands

First run `agentium experiment list --json`. If a finished experiment already asks the owner's question, go straight to its report (below).

Reading costs nothing. Add `--json`: it prints one JSON document; read its fields and `status`, not its prose.
- `agentium task list --json`, `agentium task show NAME --json`, `agentium pool status --json`, `agentium check list --json`
- `agentium experiment list --json`, `agentium experiment show NAME --json`, `agentium experiment plan NAME --json`, `agentium experiment report NAME --json`

Creating an experiment is free: `agentium experiment new NAME --a MODEL --b MODEL` compares two models, `--b SNAPSHOT` two contexts. It asks about cost by default; add `--goal better` when the owner asks whether a version passes more tasks. `agentium context snapshot NAME` saves the current context as a version. `agentium start` without `--yes` is free but slow (it mines and validates tasks); it stops at a preview.

Exit codes: 0 success, 1 runtime failure or a bad result (read the document), 2 usage error.

## The preview comes first

Before you propose any run, run `agentium experiment plan NAME --json`. Tell the owner, in plain words:
- the question the experiment asks;
- what this size can answer: `can_answer.floor_met`, `can_answer.smallest_change` and, for two contexts, `can_answer.expected_change`;
- what it will likely cost (`spend.expected_usd`), the most it can cost (`spend.max_usd`; `spend.worst_case_usd` is every run at its cap, and what the budget reserves); any of them is null when `spend.known` is false, so say the estimate is missing;
- what it takes of the plan's five-hour limit (`usage`, null with an API key): `usage.windows` windows, each filled to `usage.limit`;
- what is not ready (`ready`, `readiness`).

If `can_answer.likely_result_not_sure` is true, say so and do not recommend the run. If `can_answer.floor_met` is false, this size is too small for any answer on this question (`smallest_change` is null): tell the owner how many tasks and repeats it needs (`can_answer.floor_tasks`, `can_answer.floor_repeats`) and do not recommend the run.

## A paid run needs the owner's yes

These cost money or plan usage: `agentium experiment run NAME`, `agentium start --yes`, `agentium run once`, `agentium run calibrate`, `agentium task draft NAME`.

Run one only after the owner said yes to that run in this conversation, having heard its preview. With `--json` they need `--yes`: add it only after that yes. A yes for one run is not a yes for the next. You may lower a budget for a careful owner (`--budget USD`, `--run-budget USD`); raising a budget or a usage limit is the owner's call.

## Then the report

Run `agentium experiment report NAME --json` and tell the owner, in plain words:
- which version is cheaper and by how much;
- whether it passes as many tasks;
- how sure each answer is, and what would settle an open one;
- the tasks both versions failed, as tasks to check (`agentium task show NAME`).

Each metric's verdict is `analysis.results[].verdict` (with `metric`). `improved`, `improved, but small`, `regressed`, `no loss beyond the margin` and `equivalent` are answers. `inconclusive` and `exploratory` (too small a design for a verdict) are not: say "not sure", never turn them into an answer.

## Leave to the owner

Marking a task reviewed (`task edit --reviewed`), accepting a draft (`--accept-draft`), `--accept-mined`, `agentium clean --yes`, removing tasks or experiments, and signing in.
