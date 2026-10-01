# Phase 2: agent comparison and Codex

- Date: 2026-10-01
- Status: Deferred (2026-10-01) by the user: Codex is skipped for now. Comparing Claude Code models and efforts moved to its own [plan](../2026-10-01-model-ab.md). Resume from this file; it needs the Codex CLI installed by the user.
- Scope: the roadmap's Phase 2. The [feasibility study](../../../docs/research/2026-09-27-ai-development-lab.md) calls for:
  - Claude Code and Codex adapters, with Codex "integrated through the CLI";
  - an "Agent/Model comparison" experiment;
  - effective context per agent (Codex reads `AGENTS.md` up to 32 KiB and does not follow `@` imports).

## Why
Agentium compares context versions for one agent. Its users also ask which agent and model to use on their own repository, at what cost. Arms that differ in agent or model, under the same statistics, answer that.

## Outcome
- **Experiments:** `experiment new NAME --template agent-ab --a claude:MODEL[:EFFORT] --b codex:MODEL[:EFFORT]` compares two agent profiles on the same tasks and context. Model-only comparisons within Claude Code (`--a claude:sonnet --b claude:opus`) also work.
- **Codex runs headless** (`codex exec --json --ephemeral --ignore-user-config`) in an isolated checkout:
  - its own `CODEX_HOME`;
  - `workspace-write` sandboxing with no network;
  - denied paths, and its own build cache;
  - graded and recorded like Claude Code runs.
- **Cost:** Codex reports tokens, not cost, so Agentium prices its runs from tokens at dated OpenAI list prices (`internal/pricing`), marked as estimates.
- **Context:** reports show the effective context per agent, with warnings such as "AGENTS.md exceeds Codex's 32 KiB cap" and "@ imports are not followed by Codex".
- **Fairness and noise:** verdicts and floors work as for context A/B. Different agents differ in noise, so the A/A noise check runs per agent.

## Design
- **An agent adapter interface** (`internal/agent`) over today's `internal/claude`, covering:
  - command and environment;
  - denied paths and sandbox;
  - stream parsing into the shared metrics (turns, tokens, cost or estimate, tool calls, denials);
  - outcome classification and drift.

  Claude Code's adapter is today's code, unchanged; a golden test is written first.
- **The Codex adapter** (`internal/codex`):
  - `codex exec --json` events into metrics;
  - `--output-schema` is not needed;
  - sign-in by API key or ChatGPT login, presence only, like Claude's;
  - isolation through `CODEX_HOME` and `--ignore-user-config`;
  - sandbox settings in a run-local config.
- **Experiments:**
  - a new template `agent-ab`, whose arms are agent profiles over one context snapshot;
  - the lock records the agent versions;
  - a version drift during an experiment is reported, as Claude Code's is today.
- **Context per agent** (`internal/claudectx` gains a Codex view): Codex's `AGENTS.md` discovery and cap, and what it does not load.
- **Pricing:** dated OpenAI list prices for the models used, with the source and date in the table.

## Acceptance
1. **Recipe (paid; approval):** a short Codex session in a run's sandbox edits and tests a Go fixture offline, with no denials and no access to the user's `CODEX_HOME`. *Evidence:* the recipe and a session summary in this plan.
2. **Claude Code unchanged:** behind the adapter interface, its environment, arguments and denied paths are identical (a golden test written first).
3. **The Codex adapter:** event parsing (fixtures from a real `--json` stream), isolation, sign-in modes, outcomes and token pricing are unit-tested.
4. **Experiments:** the `agent-ab` design, validation, lock, preview (Codex priced from past runs or a default profile) and execution are tested with fake agents.
5. **Reports:** agent profiles per arm, cost estimates marked, effective context per agent, and per-agent noise; reports without them are unchanged.
6. **Real check (paid; approval):** an A/A per agent and one `agent-ab` (Claude Code against Codex) on 4 tasks × 1.
7. **Docs:** architecture, README (requirements: Codex CLI version, sign-in) and help.

## Work
- [ ] **0. Prerequisites (the user):** install the Codex CLI and sign in; approve the recipe sessions.
- [ ] **1. Codex recipe spike (paid; approval).**
- [ ] **2. Agent adapter interface,** with Claude Code unchanged (golden test first).
- [ ] **3. Codex adapter.**
- [ ] **4. `agent-ab` experiments and pricing.**
- [ ] **5. Context per agent, and reports.**
- [ ] **6. Real check (paid; approval),** then docs and archive.

## Boundaries
- Claude Code and Codex only, local mode only. No containers (Harbor stays later). No new Go modules.
- Never `--dangerously-bypass-approvals-and-sandbox`, or its Claude equivalent, on the host.
- Sandbox and credential changes are security-relevant: besides the reviewer, the user is asked for a review from the other client (Codex).

## Verification
Golden and unit tests per adapter, fake-agent CLI tests, `harness.py check changed`, CI, a reviewer per step, and paid real checks in steps 1 and 6.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
