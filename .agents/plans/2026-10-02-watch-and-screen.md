# Automation A4: the pull-request cost screen (wave 4); the deep watch (A5) cancelled

- Date: 2026-10-02
- Status: **The watch is cancelled by the user (2026-10-02):** "It should be just a tool that you, hooks or AI calls; no watch subprocesses at all; remove it." Asked about the parts already merged (budget consent, weekly ledger, drift-chart storage): "Delete it all." Standing rule: Agentium starts no background or detached processes on its own.
  - **Removed:** A5 (the deep watch), the drift loop (panels, checks, points), launchd, the weekly ledger and the consent. Steps 4 and 5 are cancelled; step 4's code (`internal/watch`, `internal/store/watch.go`, the runs' pass and drift-check fields) is deleted, and migration 11 is now a no-op (`0011_removed_watch.sql`), kept so databases that applied it still open.
  - **Kept:** steps 1–3. `internal/stats/drift.go` stays as the pure, tested CUSUM of the [statistics note's](../../docs/research/2026-10-02-wave3-statistics-note.md) §4, which documents it; nothing calls it.
  - **The screen (A4) remains,** as a command that hooks or an AI call, in the foreground. Everything below that runs it in a nightly pass, inside the watch's weekly caps, or posts from a later pass is void: the screen needs its own per-call budget design (dollars and window share, consented per call or per repository) before step 6.
- Scope: sections 2 and 3 of the [automation plan](2026-10-01-automation.md), redesigned by **the user's decisions (2026-10-02, "plan all")**:
  - ~~**The drift chart is un-deferred** for A5.~~ Cancelled with the watch; the [cheaper verdicts plan's](archive/2026-10-02-cheaper-verdicts.md) step 6 is cancelled too.
  - **A4 runs without run reuse,** which stays deferred: fresh runs under `seq-v1`, with a small cap per check.
- Builds on: `seq-v1` and the isolated-run cost (merged), A1 (`--json`, exit codes; [plan](2026-10-02-headless.md)), and A2's foreground pool ([plan](2026-10-02-task-pool.md)); A2's queue and its git hooks are parked by the user's no-background rule.

## Outcome and boundaries

### Sign-in modes and budgets in two units (the user's requirement, 2026-10-02)
*Written for the watch and the screen. The watch's parts (passes, the weekly ledger, consent, launchd, the user-first deferral) are void; the two units still apply to the screen's per-call budget.*

"I want to use the subscription plan's window too, not only API keys": the subscription is a first-class mode for both features.

| Mode (`project.SignIn`) | Watch and screen | Budget units |
|---|---|---|
| `login` (the user's Claude Code login) | **The default for launchd on the Mac.** The plist is a LaunchAgent in the user's session, so the login's keychain is reachable. | Dollars and window share |
| `token-file` (`claude setup-token` through `AGENTIUM_CLAUDE_TOKEN_FILE`) | Supported locally. Hosted CI (later) uses it too; the user checks the plan's terms for automated use first. | Dollars and window share |
| `api-key` (`ANTHROPIC_API_KEY`) | The alternative. Usage gates are off, as in `experiment run` today. | Dollars only |

- **Dollars** are the list-price estimate, as today. On a subscription they are notional, but they still cap a runaway: the per-run cap, the per-check cap and the weekly cap all apply.
- **Window share** is a fraction of the five-hour window and of the seven-day window, taken from the usage readings in Claude Code's stream (`claude.UsageReading`). The consent carries two share caps:
  - **the watch's own use:** a pass never uses more than X% of one five-hour window (default 30%), and the watch never uses more than Y% of the seven-day window in 7 days (default 15%);
  - **the room left for the user:** no pair starts when the five-hour reading is over 50% or the seven-day reading is over 60%.

  Whichever cap binds first stops the work. The ledger keeps each pass's first and last readings. The rise counts as the watch's use: overlapping work, the user's included, errs high, as `UsagePerRun` already does.
- **The base is `experiment run`'s usage pause** (`internal/experiment`): before each pair the gate predicts the pair's share from `UsagePerRun` (6% a run until measured). If the pair does not fit, runs in flight finish and the execution pauses with `StatusUsage`.
  - **Mid-stage under `seq-v1`:** a pause leaves a stage partly settled. The next pass resumes the same stage, and the look happens only once the stage is settled, so the stage barrier and the look count do not change.
  - **Waiting:** the watch uses `Wait` only when the window resets before the pass's deadline. Otherwise it exits, and the next night resumes.
- **Stale readings:** readings arrive only with runs. A latest reading whose reset time has passed is treated as unknown, and the pass's first pair is the probe: its readings decide whether a second pair starts.
- **The user comes first.** Scheduled work defers to the next night when:
  - macOS reports input in the last 15 minutes (`ioreg`'s `HIDIdleTime`, read-only);
  - a `claude` process not started by Agentium is running;
  - or the five-hour reading rose more than the watch's own runs explain.
- **Reports and digests** show both units for every pass, check and drift point: dollars, and the five-hour and seven-day share.

### A5: the deep watch (cancelled by the user, 2026-10-02)
Removed with its code: `watch --once` from launchd, passes and `watch.lock`, the consent and weekly ledger, enrolment, continuing experiments across nights, the drift chart's loop, the digest and its notifications. See the status line.

### A4: the cost screen (warn-only)
*Still planned, as a foreground command that the user, a hook or an AI calls. The nightly pass, the queue, the weekly caps and posting by a later pass below came from the watch and are void; step 6 redesigns them with a per-call budget.*

- **Trigger:** `hooks print git` adds a `pre-push` script (chained, always exit 0, as A2's hooks). It only enqueues a `screen` job (A2's queue, kind `screen`, once per head commit) when the pushed range changes a context file the runs read: startup files, plus on-demand files that a stored run's context use shows were read. Changes to harness files (`claudectx`'s harness kind: settings, hooks, MCP) take the refusal path below.
- **Where it runs:** by default in the nightly watch pass, inside the weekly caps in both units, because 16 Agentium-sized runs use about a whole five-hour window. On a subscription a check may span passes: a pause mid-stage resumes on the next night, as above. `agentium screen run COMMIT` runs one at once, in the foreground, with its own preview (dollars and predicted window share) and consent.
- **Experiment shape:** a `seq-v1` cost experiment with 8 tasks, so one look: the note's fixed design at 3.5% (96.5% intervals; §3), with no new statistics.
  - Arms: the context snapshot at the merge base with the default branch, against the snapshot at the head commit. Model and effort are the project's defaults.
  - Tasks: 8 drawn from the pool by a seed derived from the head commit. With fewer than 8 valid tasks, there is no check, and the digest says so.
  - Caps: $3 per run, and a hard cap per check in each unit: dollars (Agentium-sized default $25; small-task default $4) and window share (default 100% of one five-hour window), both counted in the weekly caps. A budget stop gives "inconclusive"; a usage pause resumes later.
- **What it reports, honestly:** the check's dollars and window share; the cost verdict and its interval; time and tokens reported, never decided; the context's size and what the runs read. A fixed sentence: "Inconclusive is the usual result. With 8 tasks this screen detects a 20% cost change about 4 times in 10, and a 10% change about 1 in 10. Inconclusive does not mean unchanged." The comment offers the full `seq-v1` experiment as a command.
- **Tasks that broke** (exploratory, as the automation plan says): a task that passed on the base and failed on the head is re-run twice in both arms, at most 2 tasks per check. The listing shows how many flips chance alone gives, and never affects anything.
- **The comment and commit status, through `gh`** (the user's login), posted by the next watch pass once a pull request for the head commit exists: one comment marked and edited in place, and a status `agentium/cost-screen`. The status is always `success` once the check is over, with the verdict in its description, or why the check broke (`error` would fail the pull request, as `failure` does); a `pending` is always finished that way. Nothing is posted until the user enables posting for that repository (`agentium screen enable --post`, recorded like the watch's consent).
- **The harness-settings refusal:** in automated mode a range that changes any harness file is refused, whoever wrote it, and the digest says why. Commit authorship is not trust, since anyone can set an author email, and neither is a label. The user can run `screen run` by hand after reading the change. This is stricter than the automation plan's "unless the commit is the user's own"; see question 5.
- **Measured per check:** dollars, minutes and the five-hour and seven-day window shares, stored with the check and shown in the comment and the digest.

### Not in scope
Run reuse; hosted runners; Codex; a model comparison started on its own (the digest may propose one); posting the digest anywhere; installing launchd or hooks; writing the user's repository or settings.

## The screen's cost and power (σ = 0.19, one run per arm; note §6's model)
Power was simulated with the note's model (100,000 experiments per row). At 5% it reproduces the note's fixed-8 figures (47.7% against 47.1%; 32.2% against 31.7%).

| True change | τ | Decisive, 8 tasks at 3.5% | Full `seq-v1` to 16 (note §6) |
|---|---|---|---|
| none | 0.10 | 3.4% (all false) | 3.2–3.4% |
| 10% | 0.10 | 11% | — |
| 20% | 0.10 / 0.25 | 40% / 26% | 74% / 52% |
| 35% | 0.10 / 0.25 | 92% / 75% | 99.9% / 98% |
| 63% (model A/B) | 0.10 | 100% | 100% |

- **Cost per check:** 16 runs, plus re-runs of broken tasks (about 1.6 tasks flagged on average, capped at 2, so about 6 runs). That is about $1.6–2.2 on small tasks ($0.10 a run) and $18–24 on Agentium-sized tasks ($1.10 a run). The full `seq-v1` would average 26.4 runs ($2.6 / $29) at a 20% cut.
- **Time:** about 12 minutes on small tasks, and about 55 minutes on Agentium-sized ones (concurrency 2).
- **Window share:** 16 runs at the measured 5–6.7% each come to 80–100% of a five-hour window on Agentium-sized tasks (the automation plan's "about half" predates that measurement). At the default 30% pass cap, an Agentium-sized check therefore spans 3–4 nights; a small-task check is not yet measured and likely fits one pass. The seven-day share is unmeasured, and step 8 measures it.
- **Fraction of checks that are decisive:** this depends on what pull requests do. If 70% change nothing, 20% change cost by about 20% and 10% by 35% or more, then about 15–20% of checks are decisive, and 3.4% of the unchanged ones give a false verdict (about 1 to 2 a year at 50 checks).

## Dependencies and order
- `seq-v1` blocks step 6, and so does the screen's per-call budget design. Steps 4 and 5 are cancelled, and with them the screen's dependence on A2's background queue (parked by the user's no-background rule).
- A1 part 1b (experiment `--json`) and part 2 (`agentium.toml`) block nothing here. The new commands follow part 1's contract, consent lives in the store, and the TOML file can only lower caps later.
- **Can start now,** away from busy packages (`internal/experiment`, `internal/report`, `internal/pool`, `internal/store`, the pool CLI, `internal/buildtool`): steps 1, 2 and 3, and step 7 once approved.

## Work
- [x] **1. Drift statistics** (`internal/stats/drift.go`; `drift_test.go` calls it). **Risk: medium.**
  - The self-starting standardization, the CUSUM, the alarm and the start-of-climb point, as pure functions. The test-only simulation moves onto production code, as `seq-v1` did.
  - *Acceptance:* the long drift simulation, `AGENTIUM_LONG_SIM=50 go test -run TestDriftChart ./internal/stats`, passes the note's gate on production code: at most 5% of charts alarm within 52 checks in every scenario.
  - *Done (2026-10-02):* `DriftScore`, `DriftStep`, `DriftAlarm` and `DriftChart` (with the start of the climb) in `internal/stats/drift.go`; the simulation runs on them. The long run (`AGENTIUM_LONG_SIM=50`, 90,000 null charts, 43 s) reproduces the note: h = 6 alarms falsely within 52 checks in 3.59% [3.47, 3.71] pooled and 4.04% [3.53, 4.62] in the worst scenario (h = 5: 10.04%).
- [x] **2. Triggers and the refusal** (new `internal/screen`: `trigger.go`; reads through `gitx`, `source`, `claudectx`). **Risk: high** (security).
  - Classify a pushed range: read context, harness, or neither; refuse harness.
  - *Threats:* a hostile pull request (a symlink, a rename or a deletion of a harness file; settings under another name that Claude Code still loads, from `claudectx`'s own list; an `agentium.toml` change counts as harness).
  - *Acceptance:* table tests over fixture repositories, including each harness kind and a rename.
  - *Done (2026-10-02; review fixes the same day):* `screen.Classify(ctx, repo, Range{Old, New}, Options{DefaultBranch, Remote})` returns a `Result` (verdict, findings, the read-context changes of the push and of the arms). New helpers: `claudectx.IsHarness` (now Resolve's own test), `claudectx.HarnessFrontmatter`, `source.Link` and `source.CommitEnv`.
    - **Ranges.** The harness is checked over the arms a screen compares (the merge base with the default branch..new), so a later context-only push cannot hide an earlier harness change, plus the push's own findings on paths the arms differ in, so a harness change merged in from the default branch is not refused. A candidate needs context changes in both.
    - **Input.** `Old` and `New` must be full commit ids. An `Old` that is all zeros, unknown, not an ancestor (a force push) or behind the merge base falls back to the merge base, with a note: it is the screen's base arm anyway, so no claimed old commit can narrow the check. The default branch is read exactly (`symbolic-ref`, `show-ref --verify`), never through `rev-parse`'s search, so a tag named `refs/remotes/origin/HEAD` cannot move the merge base. An unknown new commit, no default branch, no shared history or a partial clone is an error; every git call sets `GIT_NO_LAZY_FETCH=1`.
    - **Harness:**
      - `claudectx.IsHarness`, applied to the path as a case-insensitive disk sees it (case folded as `strings.EqualFold` does, HFS+-ignored code points dropped) and at every folder (`pkg/.claude/settings.json`); `agentium.toml` anywhere; any other file in a `.claude` folder that Resolve does not place as instructions, a rule, a skill, a subagent or a command.
      - Every Markdown file in a `.claude` folder whose frontmatter, read through its symbolic links, declares `hooks`, `mcpServers`, `allowed-tools`, `permissionMode` or `memory`, or cannot be read safely (`claudectx.HarnessFrontmatter` fails closed on flow syntax, explicit keys, tags, anchors, merge keys, escapes, odd delimiters and line breaks other than LF or CRLF; block scalars' more indented lines are text); a link's target is then harness too.
      - Snapshot files (context, what context links reach, `.claude` files) that a harness file or harness frontmatter names, by full path or from its own folder; files outside the snapshot come from the task's base and never reach an arm.
      - When the commit can run commands (settings with hooks, helpers or plugins, hook scripts, hooks in frontmatter, a harness link), every snapshot file that is not prose, or is executable, since a command can find a file without naming it.
      - Everything reached through a harness path that is a symbolic link (`.claude/hooks -> ../scripts`); a changed link that leads to harness, or to a folder holding it.
      - Renames count as a deletion plus an addition. A commit over a bound (256 harness files, 1,024 Markdown files in `.claude`, 1 MiB a file, 8 MiB of names) refuses every change.
    - **Reading:** `rev-parse`, `merge-base`, `show-ref`, `diff-tree --raw --no-renames`, `ls-tree` and `cat-file` through `gitx`; no checkout, no index, no hooks, filters or text conversion (a test arms all of them).
    - *Verified:* table cases for each harness kind, renames, deletions, links both ways, case variants, nested files, the reviewer's frontmatter and link fixtures, computed paths and false refusals, plus range tests (an earlier push, odd old commits, a merged default branch, the tag spoof, bounds, nothing run). Each fix was checked by reverting it and seeing its tests fail. On Agentium's last 101 first-parent ranges: 79 context, 21 none and 1 refused, the commit adding a subagent with `permissionMode`; about 0.7 s a range.
    - *Limits:* Windows-only name equivalences (trailing dots, short names, backslashes) are not folded. Read-context detection is exact-case, like the snapshot, so a case-variant instruction file counts as neither. Resolve reads context files without a size bound. `memory` as a subagent field is from the docs as recalled, unverified. To verify in step 7: whether `` !`cmd` `` in a skill's or command's body, which runs when it is invoked, goes through Bash permissions inside the sandbox; until then a body edit with no harness frontmatter is context.
- [x] **3. `gh` adapter** (new `internal/ghx`). **Risk: high** (credentials, external writes). Done 2026-10-02: the lookup is `GET repos/{o}/{r}/commits/{sha}/pulls`, filtered to open pull requests headed by the commit on a branch of the repository itself (search lags and matches text; a fork's pull request at the same commit is left out, so with `origin` as the user's fork and the pull request in upstream, nothing is found). Statuses are only `pending` and `success`: GitHub's `error` fails a pull request like `failure`, and there is no `neutral`. Only `GH_TOKEN` passes to gh (not `GITHUB_TOKEN`). Logins GitHub no longer issues (ending in a hyphen or containing `--`) are refused, fail-closed: such an account cannot post. Review: changes requested, fixed; re-review pending.
  - Find the pull request of a commit, upsert one marked comment, set a status. Everything runs through `runner`, with a timeout and the user's environment, never inside a run.
  - *Threats:* credentials (`GH_TOKEN` is already dropped by `IsCredential`, and `.config/gh` is denied to agents; a test fixes both); text from the pull request is data (the comment is built only from Agentium's report, redacted).
  - *Acceptance:* tests with a fake `gh` on the `PATH` that record arguments; no network in tests.
- [x] ~~**4. Watch state**~~ **Cancelled by the user (2026-10-02); its code was removed.** It had added migration `0011_watch.sql` (consent, loops, passes, enrolments, screen checks, drift panels, checks and points; the runs' `watch_pass_id` and `drift_check_id`), `internal/store/watch.go` and `internal/watch` (consent, terminal confirmation, sign-in identity, ledger, drift panels). Version 11 is now the no-op `0011_removed_watch.sql`; databases that applied the old one keep its unused tables and columns, which nothing reads or writes (`store_test.go` covers both).
- [ ] ~~**5. `watch --once`, `watch enable|add|status`, `hooks print launchd`**~~ **Cancelled by the user (2026-10-02)** before it started.
- [ ] **6. The screen** (`internal/screen`, `internal/cli/screen.go`, the `pre-push` hook in `hooks print git`). **Risk: high.** After steps 2 and 3, and a per-call budget design the user approves (the watch's weekly caps and nightly pass are gone).
  - *Redesign first:* `screen run` in the foreground, with its own preview and consent per call (or per repository), a hard cap per check in dollars and window share, and posting in the same call; a hook may call it but never leaves it running in the background.
  - *Threats:* the refusal; budget (the per-check cap of the new per-call design); hidden tests (unchanged: pool tasks through today's checkouts); a hostile agent (unchanged run isolation).
  - *Acceptance:* the automation plan's acceptance 4, with a fake `gh` and a fake Claude Code; the honesty sentence fixed by a test; posting only after `screen enable --post`; a check that reaches its dollar or share cap stops in the same call and reports inconclusive (a later explicit call may continue it); the comment shows dollars and window share (dollars only with an API key).
- [ ] **7. Harness probe** (paid, small; approval). A scratch repository whose `.claude/settings.json` sets a hook and an `env` value, run once; it records only whether each took effect (never a credential's value). About $0.20–1. It must run before any teammate's push is screened.
- [ ] **8. Real checks, docs, review** (paid; approval), with a recorded review per step and a second review from the other client for steps 3, 5 and 6.
  - ~~**Watch:** a pass on samber/lo with drift points.~~ Cancelled with the watch.
  - **Screen:** on a small repository the user owns (its use, and posting to it, need approval), one context change and one refused harness change (free): about $2.
  - **Optional:** one Agentium-sized screen, about $18–24.
  - Then the guide, the code map and the automation plan's status; archive.

## Verification
`python3 scripts/harness.py check changed` per step; unit and CLI tests with a fake Claude Code, a fake `gh` and a fake clock; the long drift simulation (step 1); a real console sample of a screen comment.

## Decisions (the user, 2026-10-02)
0. **The watch is cancelled** and its merged code deleted; Agentium starts no background or detached processes on its own. Decisions 1 and 3 below were made for the watch and no longer bind the screen.
1. **Weekly budget:** $20 a week, shared by the watch and the screens.
2. **Screen size:** the full `seq-v1`, up to 16 tasks (stops at 8 when clear).
3. **Window shares:** as proposed: at most 30% of a five-hour window per pass and 15% of the seven-day window per week; nothing starts above 50% (five-hour) or 60% (seven-day).
4. **Harness-settings changes:** always refused in automated mode.
5. **Subscription windows** are a first-class budget, beside API keys (the user's standing requirement).

Proposed defaults stand for the other questions until the user changes them: broken-task re-runs off by default. Those for the watch (weekly drift checks, screens in the nightly pass, the user-first signals, the digest) went with it. Hosted CI with a token file waits for the user to confirm the plan's terms.

## Open questions for the user
Questions 1, 2, 4, 7, 8 and 9 were about the watch and are void; the screen's per-call budget replaces them.

1. **Weekly budget:** a default of $20? Do screens share it? Agentium-sized drift alone takes $9 of it each week.
2. **Drift cadence:** weekly (detection in 5–6 weeks, 12 weeks of warm-up), or twice a week at twice the cost?
3. **Screen cap:** 8 tasks and one look (40% decisive at a 20% cut; $1.6 or $18), or the full `seq-v1` to 16 (74%; $2.6 or $29 on average, $3.2 or $35 at most)?
4. **Screen timing:** nightly only (proposed), or right after a push when the usage gate allows?
5. **Harness changes:** refuse all in automated mode (proposed), or allow commits by the user's own author email, which can be forged?
6. **Broken-task re-runs:** twice per arm for at most 2 tasks (about +40% runs), or off by default?
7. **Window shares:** the watch uses at most 30% of a five-hour window per pass and 15% of the seven-day window per week, and starts nothing above 50% (five-hour) or 60% (seven-day). Are those right? At 30%, an Agentium-sized drift check takes two nights and a screen 3–4.
8. **User-first signals:** the quiet window of 02:00–06:00, input within 15 minutes (`HIDIdleTime`), another `claude` process, and usage rising faster than the watch's runs explain. Are these acceptable to read? And local notifications through `osascript`?
9. **Claude Code auto-updates** block multi-night experiments. Should the digest recommend pinning (a user-side setting that Agentium never writes)?
10. **Hosted CI with a token file** (later): confirm that the subscription's terms allow automated use before any hosted step is planned.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
- Per check (measured at step 8): screen <$> / <min> / <usage share>
