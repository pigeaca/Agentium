# Security policy

## Supported versions

Agentium has no releases yet. Security fixes land on `main`.

## Reporting a vulnerability

Report privately, never in a public issue, pull request or discussion. Use GitHub's private vulnerability reporting: open the repository's **Security** tab and choose **Report a vulnerability**, or go straight to [the report form](https://github.com/pigeaca/Agentium/security/advisories/new).

Include:
- what is affected (file, harness command, hook or workflow)
- steps to reproduce
- the impact you expect

Leave real credentials out. If you found a leaked secret, say where it is and do not use it.

Follow-up happens in the private advisory. The fix and the advisory are published together, and you are credited unless you prefer not to be.

## Scope

Today the repository contains the development process: the Python harness, the Git hook, the CI workflow and the agent instructions. In scope:
- a harness command or hook that runs untrusted input, leaks secrets or destroys work
- a workflow with more permissions than it needs
- agent instructions that could lead an agent to expose credentials or act on untrusted content

Vulnerabilities in third-party tools such as Claude Code, Codex or GitHub belong with their vendors.
