# Plans

Plan before implementation, sized to the change:

- **Plan file** (default): one short active plan here with outcome, approved scope, acceptance criteria, compatibility and focused verification. Use the [implementation](../templates/implementation-plan.md) or [product increment](../templates/big-step-plan.md) template. Required for contract, schema, persistence, dependency, security, cross-package or user-workflow changes, and for delegated/parallel work.
- **Inline plan** (small changes only): for a single coherent fix without those effects, state outcome, acceptance and the check in the conversation before editing, then repeat them in the commit body. No plan file or archive entry is needed. If the change grows, switch to a plan file before continuing.

Acceptance criteria are written before implementation and do not change without the user's agreement; reviewers judge the change against them. At completion, fill the plan's Metrics block (`harness.py metrics` compares them across plans to guide model routing). At completion or supersession, move a plan file to `archive/`, state the actual status, and add an `archive/INDEX.md` link. The archive commit is part of the task's PR. Archives are history, not default agent context. Check with `python3 scripts/harness.py check docs`.
