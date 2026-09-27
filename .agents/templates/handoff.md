# Handoff

Use this when passing work to another agent or back to a coordinator, instead of the conversation history. Cite `path:line` for claims and label inferences. Omit empty sections.

```text
Task: <one line; plan path if any>
Base: <branch> @ <commit>
Branch / worktree: <branch> at <absolute path>   (implementation handoffs)
Commits: <ids and subjects>                      (implementation handoffs)
Relevant files: <path:line - why it matters>
Findings: <facts with evidence>
Decisions: <what was chosen and why>
Constraints: <contracts, invariants, tests and docs that must keep holding>
Checks run: <command - result>
Risks: <what could break and what else it affects>
Unresolved: <open questions, or "none">
```
