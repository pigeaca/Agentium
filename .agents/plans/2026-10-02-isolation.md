# Isolation: sandboxed grading on macOS, then a container mode

- Date: 2026-10-02
- Status: Ready (2026-10-02): the user answered the open questions (see Decisions). Part 1 starts with step 0, the free spike.
- Scope: the user's "plan all" (2026-10-02). Part 1 comes first; part 2 is its own later track, planned here in shape only. It closes the [Java and Rust plan](archive/2026-09-30-java-rust.md)'s open threat and the [Python plan](2026-10-02-python-ts.md)'s decision 5.

## The threat today (from the code)
- **Grading** (`run.Env.grade` → `commands` → `runner.Run`) and **validation** (`task.Validator.runStage`) run the repository's build and tests on the host, with no sandbox. `runner.Environ` drops credential-looking variables only. Files are still reachable: `~/.ssh`, `~/.aws`, the keychain (Claude Code's own credential), and the token file.
- In grading the code is the agent's: its `build.gradle(.kts)`, `pom.xml` plugins, `build.rs`, `conftest.py` or `setup.py` can write the deps folder and the user's `GOMODCACHE` (later agents read them), write the shared `<data>/cache` (later grades read it: a poisoned build cache can turn another run's fail into a pass), read other runs' workspaces, records and hidden tests, and use the network.
- Validation runs only the user's code (base, overlay, solution): lower risk, but it must run in grading's mode to prove a task gradeable that way. Setup and warm-ups stay on the host with network (trusted-repository rule); they never run agent-written code.

## Part 1: sandboxed grading and validation

### Design
- **Mode:** a grader mode, `host` or `sandbox-v1`. In sandbox mode every grading and validation command runs as `/usr/bin/sandbox-exec -f <profile> /bin/sh -c <command>`, inside its own process group as today.
- **Profile source: Agentium's own generator** (a new `internal/sandbox`), written in Go and modeled on the rule set of Claude Code 2.1.285. It is not taken from Claude Code itself. Reasons:
  - Claude Code's generator is internal to its binary (the leak checks extracted it with a throwaway harness), and Claude Code changed version five times in three days. Its npm sandbox runtime would be a Node dependency needing [approval](../rules/supply-chain.md).
  - `(allow default)` is too weak: mach lookups to the security server would let a build script read the keychain. The profile is deny-default, with Claude Code's process, sysctl and mach allowlist.
  - Our own is versioned with Agentium (records name the grader), reviewable, and can express rules Claude Code's settings lack (the loopback rule below). The step 0 spike compares the two on the same fixtures and explains each difference.
- **The profile:** no network, except loopback when needed (below). Writes only to the grading copy, the run's grading cache, its temp root and `/dev/null`. Reads everything except the denied paths runs already use (`run.Env.denied`, with the grading copy and the run's folders allowed, plus credential files and the token file's folder), in their real forms; deps read-only. `forms`, `realForm` and the `/tmp` owner rule move from `internal/claude` to `internal/sandbox`, one implementation for both.
- **Toolchains:** the host's own, as in the agent's run, so agent and grader see the same JDK, Go and Python: the fairness argument against grading in Linux containers.
- **The grader's environment is the agent's recipe** (the profiles' `AgentEnv`, `PrepareRun`, deps read-only), already proven offline in this sandbox for Go, Maven, Gradle, Cargo and Python, instead of the shared `CommandEnv`. Its cache is per run (a shared writable one is the poisoning path): an APFS clone (`cp -Rc`) of a seed only trusted commands write (validation, warm-up), removed after grading.
- **Fail closed:** a canary under the same profile first runs `/usr/bin/true`, writes the temp root and fails to read a denied path. If `sandbox-exec` is missing, refused or nested (Agentium inside a sandbox), the grade is an infrastructure outcome (retried or left out), never a fail, never a host grade instead.

### What will break under a sandboxed grader
- **Gradle's file-lock service** binds a local UDP socket: Gradle grading needs local binding.
- **Gradle worker daemons** (Checkstyle, PMD, CodeNarc, forked compilers) connect back over `::ffff:127.0.0.1`, which `localhost:*` does not match, and Gradle strips `JAVA_TOOL_OPTIONS` from workers. Our profile can try a rule Claude Code's settings lack (the IPv4-mapped loopback); the spike decides. If none works, **grading Checkstyle tasks fails**, and sandboxed validation marks them not gradeable before any run.
- **Tests that need sockets:** Go's `httptest` and servers on `127.0.0.1:0` need loopback bind and connect. Tests needing the internet, Docker, Testcontainers or a local database fail.
- **A dependency the agent added** can no longer be fetched at grading (the agent's sandbox never could): a fail, made readable with `GOPROXY=off` and offline flags; a recorded difference between modes.
- **Tests that write outside the copy** fail: `~/Library/Caches`, `$HOME` dotfiles, the keychain, `launchctl`.
- **Telling a sandbox failure from a test failure.** A sandboxed grade never silently turns a real pass into a fail:
  1. **Validation in the same mode.** A lock in sandbox mode accepts only tasks whose last validation ran in that mode with the same profile version (a new `Validation.Grader` JSON field, so no migration). The reference solution passing in the sandbox proves the environment can grade a correct solution.
  2. **The canary** separates "the sandbox did not start" (infrastructure) from "the tests ran".
  3. **Denials are recorded.** The spike picks the source: the unified log's sandbox reports for the grade's process group, else the commands' output. A failed grade with denials outside what the agent's own sandbox imposes is flagged in the record and the report (open question 3 decides whether it counts).

### Compatibility
- Records gain `grader` and the profile digest; the lock gains `grader`. Empty means `host`, so old records keep their meaning and old locks resume unchanged. A run or resume whose grader differs from the lock is refused: one experiment never mixes modes. The report shows the mode.
- Comparability is measured: step 4 grades the same kept agent trees in both modes and records how often they agree.

### Work
- [ ] **0. Spike** (free, no agent run; not code). Risk: low.
  - Hand-written profiles, ours and Claude Code 2.1.285's extracted one, on fixtures: Go with `httptest`, Maven (jackson-core), Gradle with Checkstyle (junit-pioneer b8b747c), Cargo with `build.rs` (bytes), pytest with `conftest.py`.
  - *Acceptance:* each fixture matches its host result or names its break; the worker-daemon loopback rule is settled; the denial source is readable without admin rights; keychain and credential reads, deps writes and network are denied; a warm seed's clone time is measured.
  - *Packages:* none.
- [ ] **1. `internal/sandbox`:** the profile generator, the moved `forms`, the canary, and a wrapper that turns a `runner.Spec` into a sandboxed one. **Risk: high** (sandbox, credentials and network).
  - *Threats:* a hostile agent (a grading copy holding links to deps, the data folder or `/tmp` entries is written through to the real path and denied); credentials (keychain, token file, `~/.ssh`); concurrent runs (each grade has its own profile file, cache and temp root).
  - *Acceptance:* golden profile tests, plus darwin-only tests that really run `sandbox-exec` for each deny; the moved `forms` keeps its tests.
  - *Packages:* `internal/sandbox`, `internal/claude` (uses the moved code).
- [ ] **2. The grading environment:** the per-run grading cache from a clone of the seed, and the agent's recipe for the grader. **Risk: high** (hidden tests, concurrent runs).
  - *Threats:* the seed is written only by validation and warm-ups, never by a grade; two grades never share a writable cache; the clone is removed even on cancel.
  - *Packages:* `internal/buildtool`, `internal/run`.
- [ ] **3. Wiring and records:** sandboxed grading in `run.Once` and sandboxed validation in `task.Validator`; `grader` in records, validations and the lock; readiness refuses tasks validated in another mode; canary outcomes and flagged denials; report lines; a `--grader` flag. **Risk: high** (hidden tests, persistence, concurrent runs).
  - *Acceptance:* tests for resuming an old lock, for a mixed-mode refusal, for the canary turning into infrastructure, and for a hidden-test pass staying a pass.
  - *Packages:* `internal/run`, `internal/task`, `internal/experiment`, `internal/report`, `internal/cli`.
- [ ] **4. Real check and docs.** Risk: medium.
  - *Free part:* re-validate this repository's and the Java/Rust pilot's tasks in sandbox mode; grade kept agent trees in both modes and record agreement; a hostile fixture (a stub agent, not Claude Code) whose `build.rs`, `conftest.py` and Gradle script try to write deps, read the data folder, connect out and read the keychain: every attempt denied, the grade recorded.
  - *Paid part (approval):* one small sandbox-mode experiment, 4 runs, about $3.
  - *Docs:* the guide, the architecture's code map, and a [verification](../rules/testing.md) note.

Steps go in order. Each is one PR with green CI, the reviewer's [threat checklist](../roles/reviewer.md#threat-checklist), and a second review from the other client (Codex) for steps 1–3.

## Part 2: container mode (later, its own track)
- **Images per toolchain, not per project:** Go; JDK with Maven and Gradle; Python with uv; Node with pnpm. Each is built from an official base image pinned by digest, with a shared Claude Code layer pinned to the lock's version.
  - Project dependencies are warmed into a volume, mounted read-only for the agent and the grader.
  - When a repository has its own `Dockerfile` or `.devcontainer`, that is reused instead (built with network, before any agent runs).
- **Isolation:** the agent's container mounts only its workspace and the deps volume, with egress only to the Anthropic API (an allowlisting proxy; details in the track's plan). Grading runs in a second container from the same image, with no network; the hidden tests exist only there.
- **Sign-in:** `ANTHROPIC_API_KEY`, passed by name (`-e NAME`, never in arguments), or `AGENTIUM_CLAUDE_TOKEN_FILE` mounted read-only. *Caveat:* using a subscription token in automated container runs must fit Anthropic's subscription terms. An API key is the clean path for hosted runs.
- **Never mix:** the executor (`local` or `container` with its image digest) goes in the lock. Container and local runs never share an experiment, and their toolchains differ by design.
- **Harbor or direct Docker.**
  - *Harbor* (the [strategy decision](../decisions/2026-09-27-hybrid-strategy.md)) offers cloud sandbox providers and benchmark datasets, but brings its own task format and reward files, its own Claude Code install under `bypassPermissions` (duplicating Agentium's stream-json metrics, hidden grading, overlays and cost), per-trial image builds, and a Python tree with LiteLLM, whose PyPI releases once shipped a credential stealer.
  - *Direct Docker:* Agentium drives the `docker` CLI out of process. That adds no Go module, works with Podman and colima, and reuses every executor contract that exists today.
  - **Recommendation: direct Docker** for the container executor. Harbor's task format stays an import and export path for public benchmarks, and Harbor stays a later option for cloud providers. This changes the strategy decision, so it needs a new decision record superseding its container clause, written before any code.
- **What it unlocks:** untrusted repositories and external pull-request branches, Linux and hosted runs ([automation](2026-10-01-automation.md) A3), and public benchmarks.
- **First spike** (needs Docker running; the user starts Docker Desktop or colima, and Agentium never installs or starts tools): build the Go image with the Claude Code layer; run 2–4 sessions on small Go tasks (this repository's or samber/lo's), graded in a second container; measure build and start time, egress (only the API reachable), stream-json parity, and cost and time against local runs.
  - *Estimate:* about $0.20–1.25 per session (samber/lo averaged $0.19 per run; Phase 1's Go runs averaged $1.23), so $1–5 with a calibration, **capped at $6**. Separate approval.

## Decisions (the user, 2026-10-02)
1. **Default mode:** new experiments on macOS grade in the sandbox; `--grader host` opts out. Existing experiments keep their mode.
2. **Loopback in grading:** allowed by default, as in the agent's own sandbox.
3. **A failed grade with flagged denials:** a fail only when the agent's own sandbox imposes the same limit; otherwise infrastructure (retried or left out).
4. **Grading cost:** a per-run cache clone is accepted, to close the poisoning path.
5. **Re-validation:** locking in sandbox mode re-validates host-validated tasks automatically (time, no money).
6. **Part 2:** Docker driven directly, not through Harbor ([decision](../decisions/2026-10-02-containers-direct-docker.md)). Sign-in by `ANTHROPIC_API_KEY` or a subscription token file (`AGENTIUM_CLAUDE_TOKEN_FILE`), both mounted read-only; the subscription's usage windows are a first-class budget (the user checks the plan's terms for automated use). The container spike (about $1–5, cap $6) needs Docker started by the user and its own approval.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
