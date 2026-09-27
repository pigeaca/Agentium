# Phase 0 spike

Throwaway research code for the [feasibility study](../../docs/research/2026-09-27-ai-development-lab.md) Phase 0. It measures what a context A/B experiment costs and how noisy it is, using real Claude Code runs on Agentium's own harness. It is not product code, and the MVP will not reuse it as is. Plan: `.agents/plans/2026-09-27-phase0-spike.md`.

## What it runs

- **Tasks.** Six small changes to `.agents/scripts/harness.py`, in `tasks/<name>/`. Each has:
  - an issue-style `instruction.md`;
  - `test_hidden.py`, which the agent never sees;
  - `gold.patch`, a reference solution used only for validation.
- **Arms:**
  - `full`: the base commit's own context, a `CLAUDE.md` that imports about 1,100 words of entry docs;
  - `minimal`: the 64-word `CLAUDE.md` and `AGENTS.md` in `variants/minimal/`.
- **Success.** A run succeeds when the hidden tests pass and the checkout's own harness tests still pass.
- **Design.** Six tasks × two arms × five runs. The two arms of a task and repeat run back to back in random order.

## Isolation

- **Clones.** Every run clones a bare repository that holds only the base commit `432b604`. No run can reach the spike files through git, and the clone has no remote.
- **Claude Code** runs headless on `claude-sonnet-5` at `high` effort, with:
  - a fresh `CLAUDE_CONFIG_DIR` and `--setting-sources project`, so no user-level instructions, skills, plugins or memory load;
  - sandboxed Bash with no network and no unsandboxed fallback;
  - `acceptEdits`, with web, artifact, scheduling and worktree tools disabled;
  - a $3 budget per run and a 20-minute timeout.
- **Hidden paths.** The shell and the Read tool are denied the spike sources, other runs' records and the validation checkouts.
- **Authentication** uses the user's subscription through a long-lived token in `~/.config/agentium/claude-oauth-token` (mode 600):
  - `spike.py` passes it only to the claude process;
  - the sandbox strips it from the agent's shell;
  - it is never printed or stored elsewhere.

  Create the token yourself:

  ```bash
  "$HOME/Library/Application Support/Claude/claude-code/2.1.281/claude.app/Contents/MacOS/claude" setup-token
  ```

  Copy the printed token, then save it from the clipboard, so it never enters shell history:

  ```bash
  mkdir -p ~/.config/agentium && chmod 700 ~/.config/agentium && pbpaste > ~/.config/agentium/claude-oauth-token && chmod 600 ~/.config/agentium/claude-oauth-token
  ```

## Commands

Run from this directory. `--work` is a scratch directory outside the repository. Checkouts and transcripts go there; only metrics are written to `results/`.

| Command | What it does | Model calls |
|---|---|---|
| `python3 -m unittest test_spike` | Parser, scheduling, settings and statistics tests | none |
| `python3 spike.py --work DIR validate` | Each task fails on the base commit and passes with its reference patch | none |
| `python3 spike.py --work DIR probe` | Isolation canaries (Haiku) and per-arm first-request context size (Sonnet) | about 4 small runs |
| `python3 spike.py --work DIR codex` | One Codex run with the normal login, to check its JSON output | 1 Codex run |
| `python3 spike.py --work DIR run` | The experiment; resumable. It stops starting new runs at the estimated-cost cap ($150) or after three infrastructure failures in a row | up to 60 runs |
| `python3 spike.py report` | Paired effects, variance components and detectable effects, written to `results/<experiment>/summary.json` | none |

Costs are Claude Code's own estimates (`total_cost_usd`). A subscription is not billed per run; the estimates show what the same runs would cost on the API.
