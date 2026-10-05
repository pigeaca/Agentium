---
name: use-agentium
description: Use when the owner asks what a model, a context file, a rule or a skill changes for their coding agents on this repository, in cost or in results.
---

# Use Agentium

Agentium is a command-line tool that runs coding agents on tasks from the owner's own repository, and compares models or versions of the project's context (AGENTS.md, CLAUDE.md, skills, rules) on cost and passed tasks. It never writes to the owner's repository.

## Free commands

Reading costs nothing. Add `--json`: it prints one JSON document; read its fields and `status`, not its prose.
- `agentium task list --json`, `agentium pool status --json`, `agentium check list --json`
- `agentium experiment list --json`, `agentium experiment show NAME --json`, `agentium experiment plan NAME --json`, `agentium experiment report NAME --json`

Creating an experiment is free: `agentium experiment new NAME --b MODEL` compares two models, `--b SNAPSHOT` two contexts. `agentium context snapshot NAME` saves the current context as a version. `agentium start` without `--yes` is free but slow (it mines and validates tasks); it stops at a preview.

Exit codes: 0 success, 1 runtime failure or a bad result (read the document), 2 usage error.

## The preview comes first

Before you propose any run, run `agentium experiment plan NAME --json`. Tell the owner, in plain words:
- the question the experiment asks;
- what this size can answer: `can_answer.floor_met`, `can_answer.smallest_change` and, for two contexts, `can_answer.expected_change`;
- what it may spend (`spend`) and what it takes of the plan's five-hour limit (`usage`);
- what is not ready (`ready`, `readiness`).

If `can_answer.likely_result_not_sure` is true, say so and do not recommend the run.

## A paid run needs the owner's yes

These cost money or plan usage: `agentium experiment run NAME`, `agentium start --yes`, `agentium run once`, `agentium run calibrate`, `agentium task draft NAME`.

Run one only after the owner said yes to that run in this conversation, having heard its preview. With `--json` they need `--yes`: add it only after that yes. A yes for one run is not a yes for the next. Never raise a budget or a usage limit yourself.

## Then the report

Run `agentium experiment report NAME --json` and tell the owner, in plain words:
- which version is cheaper and by how much;
- whether it passes as many tasks;
- how sure each answer is, and what would settle an open one;
- the tasks both versions failed, as tasks to check (`agentium task show NAME`).

Never turn "not sure" into an answer.

## Leave to the owner

Marking a task reviewed (`task edit --reviewed`), accepting a draft (`--accept-draft`), `--accept-mined`, `agentium clean --yes`, removing tasks or experiments, and signing in.
