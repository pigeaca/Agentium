# Automation: Agentium without anyone running it

- Date: 2026-10-01
- Status: Planned (2026-10-01). **The pull-request cost screen (loop 2) is parked by the user (2026-10-05;** [plan](2026-10-05-quiet-console-useful-tasks.md), decision 6**).**
  - **Design:** on the user's Mac, driven by hooks with no daemon (the user's decisions).
  - **Revised the same day** after an independent review: the $5 check could not start, time and tokens get no verdict, the smoke check was dishonest, and teammates' settings changes are a security gap.
  - **Phases** follow the [next chapter](2026-10-01-next-chapter.md): the free context-lint hook in wave 2, headless mode in wave 3, the rest in wave 4.
  - **Paid steps** need their own approval.
  - **The watch is cancelled (the user, 2026-10-02):** "It should be just a tool that you, hooks or AI calls; no watch subprocesses at all; remove it." Its merged code is deleted. Standing rule: Agentium starts no background or detached processes on its own. So section 3 (A5) is cancelled, nothing runs on a schedule, and the screen (A4) is a foreground command that hooks or an AI call, with its own per-call budget still to design.
- Scope: how Agentium runs on its own in the user's development apps: on changes to AI context, on a schedule, and as new code lands.

## Why
Today a person runs every command. The value comes from three questions:
- Did this change to `CLAUDE.md`, skills or rules make the agent better, worse, or cost more?
- Did a new Claude Code version or model change results on our tasks?
- Do we have enough good tasks to answer those questions?

Each can run on a trigger, inside a budget, and report where the team already looks.

## Decisions (the user, 2026-10-01)
- **On the user's Mac first.** The sandbox recipes are proven there, the data folder persists (so base runs and calibrations are reused), and the subscription's usage windows are used rather than API prices.
- **Hooks, no daemon.** An event starts one job, and the job exits.
- **Hosted runners later.** They need the Linux spike (A3), credentials (below), a cached data folder, and a GitHub Action.

## The loops

### 0. Context lint (free; wave 2, with quick start)
- **Trigger:** a Claude Code `PostToolUse` hook in the user's own settings, on edits to context files. Agentium's runs load project settings only, so the hook cannot fire inside them.
- **Does:** a quick context check with no agent runs: the size change against the last snapshot, broken imports, an `AGENTS.md` over Codex's 32 KiB limit, and the warnings `context show` already gives.
- **Cost:** none. It shows its result in the session.

### 1. Supply: a task pool that keeps itself fresh (wave 4; [plan](2026-10-02-task-pool.md))
- **Triggers:**
  - git `post-merge` (after `git pull`) and `post-rewrite` (`pull --rebase` does not fire `post-merge`);
  - ~~the scheduled pass~~ (cancelled with the watch).

  Not `FETCH_HEAD`: IDEs fetch all the time.
- **Does:** `agentium pool update` (its `--background` mode is parked by the no-background rule) runs `task mine --since <last>`, validates the candidates in a batch, re-validates stale tasks, and retires tasks whose base is too old.
- **Cost:** no agent runs.
- **Shows:** pool health: valid, flaky, weak, awaiting review.

### 2. Cost screen on pull requests (wave 4; warn-only; [plan](2026-10-02-watch-and-screen.md))
- **Redesigned (the user, 2026-10-02):** fresh runs under `seq-v1` with a small cap per check, without run reuse (still deferred), on the subscription's usage windows as well as API keys; the plan supersedes the budget below.
- **What it can honestly say:** only cost gets a verdict, since time and tokens are reported but never decided (`experiment/analyze.go`). At the floor (8 tasks × 1 run per arm) it detects changes of about 25–30%. So it is a *screen*, never a gate.
- **Trigger:** a git `pre-push` hook, when the pushed commits change context files that the runs actually read (from context use).
  - Not MCP, model or Claude Code version changes: runs pass `--strict-mcp-config` with no servers and use the design's own model, so those arms would behave identically.
  - The job records the result against the head commit. The pull request often does not exist yet at push time; ~~the scheduled pass posts the comment later~~ (cancelled with the watch), so when and how a later call posts it is part of the per-call design.
- **Budget:**
  - at least $15 per check. Validation needs two run caps ($6 at the $3 default; $18 with the judge's 3 repeats). With the base arm reused (wave 3), the head arm is about $8–12;
  - off by default on a subscription, where 16 runs take about half of a five-hour window.
- **Shows:**
  - a pull request comment and a commit status (only GitHub Apps can create checks);
  - the cost verdict with its detectable effect, the context's size and what the runs used;
  - optionally, the judge's second opinion.

  It never fails anything: the exit code is 0 unless the check itself broke.
- **Tasks that broke, honestly:** a single run per task flips from pass to fail by chance about 20% of the time, so about 83% of unchanged pull requests would list a "broken" task.
  - A task that passed on the base and failed on the head is re-run k times in both arms before it is listed.
  - The report shows how many flips chance alone would give.
  - The list is labelled exploratory and never affects the exit code.
- **Security (required before any teammate's pull request is checked):**
  - Runs load project settings, and the per-run settings set neither `disableAllHooks` nor `env`. So a pull request's `.claude/settings.json` (hooks, `env` such as `ANTHROPIC_BASE_URL`, `apiKeyHelper`) or `.mcp.json` could run commands or redirect credentials outside the Bash sandbox.
  - In automated mode, refuse any context change to harness settings (settings, hooks, MCP: `claudectx`'s harness files) unless the commit is the user's own.
  - Add a real probe of what project hooks and `env` can do in a run (paid, small; approval).
  - The opt-in label is not trust, since anyone with triage access can add it.
- **Queue:** ~~a background queue with one consumer~~ parked by the no-background rule: a hook or an AI calls the screen in the foreground, and a busy run lock is reported to the caller.

### 3. Deep watch: cancelled by the user (2026-10-02)
Kept for history; nothing below is planned. The user removed the watch, its merged state (consent, weekly ledger, drift-chart storage) and launchd; see the status line and the [watch and screen plan](2026-10-02-watch-and-screen.md).
- **Redesigned (the user, 2026-10-02):** the drift chart is un-deferred (note §4), budgets come in dollars and usage-window share, consent lives in the data folder, and the digest stays local.
- **Trigger:** a launchd calendar entry (nightly) runs `agentium watch --once`, a single budgeted pass in an idle usage window. It also posts deferred pull request comments.
- **Does:**
  - continues long experiments under the wave-3 group-sequential design (pre-declared looks and alpha spending; no optional stopping);
  - checks drift on a new Claude Code version or model as a control chart with a false-alarm budget;
  - a model and effort comparison when a new model appears.
- **Budget:** a weekly cap, stated in `agentium.toml` (default $20), and the usage gate.
- **Shows:** history and trends, a weekly digest as a GitHub issue through `gh`, and alerts on drift.

## Shared foundations
- **Headless mode (A1, wave 3):**
  - `--json` and stable exit codes everywhere;
  - no prompts;
  - `agentium start --yes`;
  - a committed `agentium.toml` (triggers, budgets, task selection, the judge), which Agentium reads and never writes.
- **Installing:** `agentium hooks print git|claude` prints the hooks, chained with existing ones such as this repository's `.githooks`. The user installs them; Agentium never writes the repository or the user's settings.
- **Credentials:**
  - **On the Mac:** the existing Claude Code login and the `gh` login, which must be allowed to comment on pull requests and set commit statuses.
  - **On hosted runners (later):** an `ANTHROPIC_API_KEY` secret (billed per token), or a `claude setup-token` token written to a file that `AGENTIUM_CLAUDE_TOKEN_FILE` names (the subscription's usage windows, shared with the user's own sessions; check the plan's terms for automated use).
    - Agentium passes the token to Claude Code only, and the sandbox denies those variables to the agent's shell.
    - The workflow's `GITHUB_TOKEN` with `pull-requests: write` posts comments.
    - GitHub gives no secrets to workflows from fork pull requests.

## Later: autopilot (exploratory)
- **The idea:** Agentium proposes context changes from its own data, for example dropping files and skills no run read ([context use](archive/2026-09-30-task-checks-context-use.md)), tests them in an experiment the user starts, and opens a pull request with the winner.
- **When:** only after decisive verdicts are routine, and only through the user, who merges.

## Phases
| Phase | Wave | What | Needs |
|---|---|---|---|
| Lint hook | 2 | The free context check on edits, through a Claude Code hook | quick start |
| A1 Headless | 3 | `--json`, exit codes, `start --yes`, `agentium.toml` | quick start |
| [A2 Supply](2026-10-02-task-pool.md) | 4 | `pool update`, pool health; the background mode, git hooks and queue parked (no background processes) | task mining, A1 |
| A3 Linux runs | Later | A short paid spike, only for hosted runners | temp isolation |
| [A4 Cost screen](2026-10-02-watch-and-screen.md) | 4 | The warn-only screen, honest broken-task listing, the harness-settings refusal and its probe, deferred comments, commit status; redesigned on fresh `seq-v1` runs, without reuse; a foreground command that hooks or an AI call | A1, A2's pool, `seq-v1`, a per-call budget design |
| ~~A5 Deep watch~~ | — | **Cancelled by the user (2026-10-02)**; its merged code removed | — |
| A6 Autopilot | Later | Propose, test and open pull requests with context changes | A4 |

## Acceptance (per phase, refined when it starts)
1. **Lint hook:** editing `CLAUDE.md` in a session shows the size change and warnings within a second, at no cost. The hook never fires inside Agentium's runs.
2. **A1:** a fresh clone reaches a preview with no prompts. Every command's `--json` is documented, and exit codes are fixed by tests.
3. **A2:** after `git pull` or `pull --rebase`, the pool gains the new commit's task, with no agent runs. Pool health is printed and in JSON. (The queue for a busy lock is parked.)
4. **A4:**
   - A push that changes a read context file records a check.
   - The comment and commit status land when the pull request exists (when and through which call: part of the per-call design).
   - The listing of broken tasks re-runs flagged tasks, and shows the count chance alone would give.
   - A teammate's harness-settings change is refused.
   - Measured: dollars, minutes and the usage-window share per check.
5. ~~**A5**~~: cancelled by the user (2026-10-02).

## Boundaries
- Agentium never writes the user's repository or settings: comments, statuses and digests go through `gh`.
- No new Go modules without approval.
- Real runs only on the user's own commits, or on teammates' commits that change no harness settings.

## Verification
Per phase: unit and CLI tests with a fake Claude Code; one real end-to-end run on a test repository (paid; approval); a reviewer.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
