---
name: product-increment
description: Plan and complete a cohesive Agentium increment across code, focused verification, review and documentation, ending in a PR.
---

# Product increment

1. Follow [core rules](../../rules/core.md), reading them if not already loaded; inspect the relevant [roadmap](../../ROADMAP.md) item and code. Do not reload the entire documentation tree. Run commands from the repository root.
2. Create a task worktree (`python3 .agents/scripts/harness.py worktree new <agent>/<type>/<topic>`) and write a plan sized to the change: outcome, scope, acceptance criteria, compatibility and verification. Continue within the user's approved scope.
3. Implement the complete useful path. Reuse existing code and harness commands.
4. Verify coherent slices with `python3 .agents/scripts/harness.py check changed`; add stack checks the selection does not cover yet. Capture browser evidence for changed UI.
5. Commit, push and open a PR. Get the [reviewer](../../roles/reviewer.md) verdict unless the change is docs-only or inline-plan, and fix confirmed findings. Read CI and fix failures caused by the change.
6. Update affected docs and roadmap, record real results, limitations and metrics, archive the completed plan and update its index.

Routine task-scoped Git work is authorized by the [Git rules](../../rules/git-workflow.md); use [separate worktrees](../../rules/collaboration.md) for parallel writers. Integrations, dependencies, external effects and destructive actions still need explicit scope. Follow-up capabilities belong in the roadmap; do not inflate this increment.
