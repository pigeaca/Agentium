# Automation A5 and A4: the deep watch and the pull-request cost screen (wave 4)

- Date: 2026-10-02
- Status: Ready (2026-10-02): the user answered the main questions (see Decisions); the rest use the proposed defaults until the user says otherwise. Steps 1–3 can start; paid steps need their own approval.
- Scope: sections 2 and 3 of the [automation plan](2026-10-01-automation.md), redesigned by **the user's decisions (2026-10-02, "plan all")**:
  - **The drift chart is un-deferred** for A5. It moves here from the [cheaper verdicts plan](2026-10-02-cheaper-verdicts.md) (deferred step 6), with the [statistics note's](../../docs/research/2026-10-02-wave3-statistics-note.md) §4 design unchanged.
  - **A4 runs without run reuse,** which stays deferred: fresh runs under `seq-v1`, with a small cap per check.
- Builds on: `seq-v1` (cheaper verdicts step 1, in review), the isolated-run cost (merged), A1 part 1 (`--json`, exit codes; [plan](2026-10-02-headless.md) on its branch), and A2's pool, queue and `hooks print git` ([plan](2026-10-02-task-pool.md)).

## Outcome and boundaries

### Sign-in modes and budgets in two units (the user's requirement, 2026-10-02)
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

### A5: the deep watch
- **`agentium watch --once`** is one budgeted pass, then exit. launchd starts it from a plist that `agentium hooks print launchd` prints (nightly at 02:00 by default); the user installs it, and Agentium never does. A Mac asleep at 02:00 runs the missed pass on wake, so the busy gate below matters. A `watch.lock` (flock) allows one pass at a time; a pass that finds the run lock busy exits 0 and reports it.
- **A pass, in order:** post deferred screen results (A4); run `pool update` (A2's code, no agent); a drift check if one is due; continue enrolled experiments; run queued screens. Each part stops at the first gate that closes (budget, usage, busy, deadline 06:00).
- **Consent and the weekly budget.**
  - The user enables the watch with `agentium watch enable --weekly-budget USD [--pass-share 30 --weekly-share 15]` at a terminal. The store records the amounts, the sign-in mode, the time, the OS user, Agentium's version and the enabled loops (experiments, drift, screens). Only this command raises a cap, and a changed sign-in mode needs it again. `agentium.toml` (A1 part 2, later) may only lower the caps, and a missing record means the watch spends nothing.
  - Enrolling is explicit: `agentium watch add EXPERIMENT`. The experiment keeps its own locked budget, consented at its preview; the weekly budget is an outer limit.
  - **The ledger** is derived from stored run spend (agent and judge) and stored readings, tagged with the pass, over a rolling 7 days, never a separate counter, so a crash cannot double-count. A pair starts only if two run caps ($3 each by default) fit the week's dollar remainder, so estimate errors cannot overshoot it, and only if its predicted share fits both share caps.
- **Continuing an experiment under `seq-v1`** reuses the executor unchanged: a pass may stop mid-stage (budget, usage, deadline), and the next pass resumes the stage. No run of stage k+1 starts before look k, whichever night it falls on. Pairs stay adjacent, so day effects cancel within a pair. If Claude Code updated meanwhile, `Lock.Check` refuses as today, and the digest says the experiment is blocked; the watch never re-pins or reinstalls.
- **The drift chart** (note §4, as specified):
  - **Panel:** 8 tasks drawn by a recorded seed from the pool's valid, reviewed, non-flaky tasks whose base retires no sooner than 120 days. The panel is fixed. When a panel task retires, turns invalid or is edited, that chart closes and a new panel starts a new chart; points are never spliced across task sets.
  - **Key:** one chart per (project, model, effort, context snapshot, panel). The context is the snapshot pinned when the chart starts, so the chart measures the agent and the model, not the project's own context changes (A4 and experiments measure those).
  - **Points:** each check runs every panel task once (run kind `drift`) on the installed Claude Code, and stores y = the mean log isolated-run cost, the version, and the counted tasks. A check may span passes, but all its runs use one version: a version change mid-check restarts it. A check with a lost task stores no point.
  - **Statistics:** the self-starting standardization over all earlier in-control points (4 reference checks); a two-sided CUSUM with k = 0.5 and h = 6; a point that does not alarm joins the baseline. Before 12 points the report says "warming up: no alarm is not yet evidence of no change". Alarms can come from the first point after the reference and count.
  - **An alarm** writes a digest entry and a local notification (`osascript`), naming the change's direction and the point and version where the CUSUM began climbing. It proposes, as a command for the user, one pre-declared interleaved A/B of the two versions. It never makes a verdict and never starts that A/B.
  - **Claude Code version changes** are readings on the chart, marked on the point, never a fresh test and never a reset.
- **The digest:** a Markdown file per ISO week, in the data folder's `watch/`, plus `agentium watch status [--json]`. It shows the spend in dollars and window share, the passes and why each stopped, the experiments' looks, the chart and the screens. It follows the report's redaction. Nothing is sent anywhere; a GitHub issue through `gh` is a later opt-in.
- **Gates:** the share caps and user-first deferral above, the quiet hours (02:00–06:00), and the run lock.

### A4: the cost screen (warn-only)
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

## Drift panel cost
One check is 8 runs: about $0.80 on small tasks and $9 on Agentium-sized ones. On Agentium-sized tasks that is 40–54% of a five-hour window, over the 30% pass cap, so a drift check there needs two passes or a higher cap (question 7); the point is stored only when all 8 runs are in. Weekly, that is $42 or $470 a year. Weekly checks catch a 20% change in a median of 5–6 weeks (79–94% within 8 checks), and "no alarm" means something only after 12 weeks.

## Dependencies and order
- `seq-v1` (in review) blocks steps 5 and 6. A2 step 1 (its migration `0010`) blocks step 4's migration; A2 steps 2–4 (`pool update`, the queue, `hooks print git`) block steps 5 and 6.
- A1 part 1b (experiment `--json`) and part 2 (`agentium.toml`) block nothing here. The new commands follow part 1's contract, consent lives in the store, and the TOML file can only lower caps later.
- **Can start now,** away from busy packages (`internal/experiment`, `internal/report`, `internal/pool`, `internal/store`, the pool CLI, `internal/buildtool`): steps 1, 2 and 3, and step 7 once approved.

## Work
- [x] **1. Drift statistics** (`internal/stats/drift.go`; `drift_test.go` calls it). **Risk: medium.**
  - The self-starting standardization, the CUSUM, the alarm and the start-of-climb point, as pure functions. The test-only simulation moves onto production code, as `seq-v1` did.
  - *Acceptance:* the long drift simulation, `AGENTIUM_LONG_SIM=50 go test -run TestDriftChart ./internal/stats`, passes the note's gate on production code: at most 5% of charts alarm within 52 checks in every scenario.
  - *Done (2026-10-02):* `DriftScore`, `DriftStep`, `DriftAlarm` and `DriftChart` (with the start of the climb) in `internal/stats/drift.go`; the simulation runs on them. The long run (`AGENTIUM_LONG_SIM=50`, 90,000 null charts, 43 s) reproduces the note: h = 6 alarms falsely within 52 checks in 3.59% [3.47, 3.71] pooled and 4.04% [3.53, 4.62] in the worst scenario (h = 5: 10.04%).
- [ ] **2. Triggers and the refusal** (new `internal/screen`: `trigger.go`; reads through `gitx`, `source`, `claudectx`). **Risk: high** (security).
  - Classify a pushed range: read context, harness, or neither; refuse harness.
  - *Threats:* a hostile pull request (a symlink, a rename or a deletion of a harness file; settings under another name that Claude Code still loads, from `claudectx`'s own list; an `agentium.toml` change counts as harness).
  - *Acceptance:* table tests over fixture repositories, including each harness kind and a rename.
- [x] **3. `gh` adapter** (new `internal/ghx`). **Risk: high** (credentials, external writes). Done 2026-10-02: the lookup is `GET repos/{o}/{r}/commits/{sha}/pulls`, filtered to open pull requests headed by the commit on a branch of the repository itself (search lags and matches text; a fork's pull request at the same commit is left out, so with `origin` as the user's fork and the pull request in upstream, nothing is found). Statuses are only `pending` and `success`: GitHub's `error` fails a pull request like `failure`, and there is no `neutral`. Only `GH_TOKEN` passes to gh (not `GITHUB_TOKEN`). Logins GitHub no longer issues (ending in a hyphen or containing `--`) are refused, fail-closed: such an account cannot post. Review: changes requested, fixed; re-review pending.
  - Find the pull request of a commit, upsert one marked comment, set a status. Everything runs through `runner`, with a timeout and the user's environment, never inside a run.
  - *Threats:* credentials (`GH_TOKEN` is already dropped by `IsCredential`, and `.config/gh` is denied to agents; a test fixes both); text from the pull request is data (the comment is built only from Agentium's report, redacted).
  - *Acceptance:* tests with a fake `gh` on the `PATH` that record arguments; no network in tests.
- [ ] **4. Watch state** (`internal/store` migration after `0010`, new `internal/watch`). **Risk: high** (money and consent; persistence). Starts after A2 step 1 merges.
  - Consent records (dollar and share caps, sign-in mode), enrolment, the ledger query in both units, panels, charts and points, screen checks, and run kind `drift`.
  - *Threats:* budget and consent (no consent row means no spend; only the terminal command raises a cap; a changed sign-in mode needs consent again); crash and recovery (the ledger derived from runs and their readings; a point is stored once, by check id).
  - *Acceptance:* the migration applies on a populated database; ledger tests with a fake clock at the 7-day boundary and across five-hour and seven-day resets; an API-key project's ledger has dollars and no share.
- [ ] **5. `watch --once`, `watch enable|add|status`, `hooks print launchd`** (`internal/watch`, `internal/cli/watch*.go`, `internal/cli/hooks*.go`, a small `RunOptions` limit in `internal/experiment`). **Risk: high.** After `seq-v1` and A2 steps 2–4.
  - The share gate extends `experiment run`'s usage gate (`usageGate`, `UsagePerRun`) with the seven-day reading and the watch's own caps; the user-first checks sit behind an interface, so tests fake them.
  - *Threats:* budget and consent (the weekly reserve in both units on every pair, retry and resume); concurrent runs (`watch.lock`, the run lock, the A2 queue consumer); crash and recovery (a kill mid-stage resumes without repeating or skipping a look); the stage barrier across nights and usage pauses.
  - *Acceptance:* with a fake Claude Code emitting usage readings, and a fake clock:
    - a pass continues a `seq-v1` experiment across two nights, without crossing a look early, within the weekly caps;
    - a share cap hit mid-stage pauses after the runs in flight, and the next pass resumes the same stage and takes the look once;
    - each user-first signal defers the pass; a stale reading makes the first pair the probe;
    - in API-key mode, the share gates are off and dollars still bind;
    - a version change gives a drift reading marked on its point, not a test;
    - an alarm writes the digest and a notification, with no verdict;
    - no consent means no run; the digest shows both units.
- [ ] **6. The screen** (`internal/screen`, `internal/cli/screen.go`, the `pre-push` hook in `hooks print git`, the queue kind). **Risk: high.** After steps 2, 3 and 5.
  - *Threats:* the refusal; budget (the per-check cap inside the weekly budget); hidden tests (unchanged: pool tasks through today's checkouts); a hostile agent (unchanged run isolation).
  - *Acceptance:* the automation plan's acceptance 4, with a fake `gh` and a fake Claude Code; the honesty sentence fixed by a test; posting only after `screen enable --post`; a check paused by its share cap resumes on the next pass; the comment shows dollars and window share (dollars only with an API key).
- [ ] **7. Harness probe** (paid, small; approval). A scratch repository whose `.claude/settings.json` sets a hook and an `env` value, run once; it records only whether each took effect (never a credential's value). About $0.20–1. It must run before any teammate's push is screened.
- [ ] **8. Real checks, docs, review** (paid; approval), with a recorded review per step and a second review from the other client for steps 3, 5 and 6.
  - **Watch:** on samber/lo, signed in with the login (subscription), one pass continues an enrolled `seq-v1` experiment by one stage (16 runs, about $1.6 at list price), plus 4 drift points over four nights (32 runs, about $3.2). That is about $5 at list price, at most 2 hours of runs. It measures the five-hour and seven-day share per check and per point.
  - **Screen:** on a small repository the user owns (its use, and posting to it, need approval), one context change and one refused harness change (free): about $2.
  - **Optional:** one Agentium-sized screen, about $18–24.
  - Then the guide, the code map and the automation plan's status; archive.

## Verification
`python3 scripts/harness.py check changed` per step; unit and CLI tests with a fake Claude Code, a fake `gh` and a fake clock; `-race` on the pass's gates; the long drift simulation; real console samples of `watch status` and a screen comment.

## Decisions (the user, 2026-10-02)
1. **Weekly budget:** $20 a week, shared by the watch and the screens.
2. **Screen size:** the full `seq-v1`, up to 16 tasks (stops at 8 when clear).
3. **Window shares:** as proposed: at most 30% of a five-hour window per pass and 15% of the seven-day window per week; nothing starts above 50% (five-hour) or 60% (seven-day).
4. **Harness-settings changes:** always refused in automated mode.
5. **Subscription windows** are a first-class budget, beside API keys (the user's standing requirement).

Proposed defaults stand for the other questions until the user changes them: weekly drift checks, screens in the nightly pass only, broken-task re-runs off by default, the user-first signals as listed, and a digest that recommends pinning Claude Code. Hosted CI with a token file waits for the user to confirm the plan's terms.

## Open questions for the user
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
- Per check (measured at step 8): screen <$> / <min> / <usage share>; drift point <$> / <min> / <usage share>
