# Isolation: sandboxed grading on macOS, then a container mode

- Date: 2026-10-02
- Status: In progress (2026-10-02): the user answered the open questions (see Decisions). Step 0, the free spike, is done; step 1 is next.
- Scope: the user's "plan all" (2026-10-02). Part 1 comes first; part 2 is its own later track, planned here in shape only. It closes the [Java and Rust plan](archive/2026-09-30-java-rust.md)'s open threat and the [Python plan](2026-10-02-python-ts.md)'s decision 5.

## The threat today (from the code)
- **Grading** (`run.Env.grade` → `commands` → `runner.Run`) and **validation** (`task.Validator.runStage`) run the repository's build and tests on the host, with no sandbox. `runner.Environ` drops credential-looking variables only. Files are still reachable: `~/.ssh`, `~/.aws`, the keychain (Claude Code's own credential), and the token file.
- In grading the code is the agent's: its `build.gradle(.kts)`, `pom.xml` plugins, `build.rs`, `conftest.py` or `setup.py` can write the deps folder and the user's `GOMODCACHE` (later agents read them), write the shared `<data>/cache` (later grades read it: a poisoned build cache can turn another run's fail into a pass), read other runs' workspaces, records and hidden tests, and use the network.
- Validation runs only the user's code (base, overlay, solution): lower risk, but it must run in grading's mode to prove a task gradeable that way. Setup and warm-ups stay on the host with network (trusted-repository rule); they never run agent-written code.
- **Agent runs: the keychain (found by the step 0 spike; fixed, not yet confirmed in a session).** Claude Code 2.1.285's profile allows the Mach lookups `com.apple.SecurityServer` and `com.apple.securityd.xpc`, and the run's settings left `~/Library/Keychains` readable. Inside, `security list-keychains` showed the login keychain and searches ran. Claude Code and gh store their tokens with `add-generic-password -U` and no `-T`, so the agent's shell could probably read both without a prompt; no item was read to confirm it.
  - *The fix* (`internal/claude`, every project and sign-in mode): `~/Library/Keychains` (and its real form behind a link) and `/Library/Keychains` are in the run's `denyRead`, `DeniedPaths`, the Read-tool rules and the sandbox's denied credential files. When `HOME` is redirected, the account's own `Library/Keychains` (its home folder in the user database, `user.Current`) is denied too, since the login keychain stays there. Claude Code's settings cannot deny Mach lookups (`network.allowMachLookup` only adds services), so the security server stays reachable; with the folder denied, opening the keychain file fails in the `security` client ("Operation not permitted"), even when an attacker names its path, and the login keychain leaves the shell's search list.
  - *Tests:* a darwin test turns the run's `denyRead` into a `sandbox-exec` profile (everything else allowed, as the spike did), with `HOME` the account's and redirected. Inside, `security show-keychain-info` on `login.keychain-db` fails (lock settings only, never items), the folder cannot be listed, and `list-keychains` exits 0 but no longer names the login keychain. Outside, all three succeed. Go builds and tests, Python's `ssl` and git ran unchanged with both folders denied.
  - *The real check* belongs to the Python pilot's first run: inside the session, `security list-keychains` must not name the login keychain, and both a listing of `~/Library/Keychains` and `security show-keychain-info ~/Library/Keychains/login.keychain-db` must be denied. A search, if tried at all, uses a made-up service name, never a real one. Claude Code reads its own login outside the sandbox, so sign-in should be unaffected; the same run confirms that too.
  - *Residual risks:* the data-protection keychain is reached through `com.apple.securityd.xpc`, which stays allowed; access to it is gated by entitlements (keychain access groups), so tools without those entitlements get nothing from it, but a few entitled platform binaries reach their own groups. `security lock-keychain -a` can still lock the user's keychains: a nuisance, not a leak.
  - *Follow-ups:* other credential stores in the home folder are not denied yet, browser profiles (cookies, saved logins) first. The strong defense in depth is running agents as a separate macOS user (Part 2).

## Part 1: sandboxed grading and validation

### Design
- **Mode:** a grader mode, `host` or `sandbox-v1`. In sandbox mode every grading and validation command runs as `/usr/bin/sandbox-exec -f <profile> /bin/sh -c <command>`, inside its own process group as today.
- **Profile source: Agentium's own generator** (a new `internal/sandbox`), written in Go and modeled on the rule set of Claude Code 2.1.285. It is not taken from Claude Code itself. Reasons:
  - Claude Code's generator is internal to its binary (the leak checks extracted it with a throwaway harness), and Claude Code changed version five times in three days. Its npm sandbox runtime would be a Node dependency needing [approval](../rules/supply-chain.md).
  - `(allow default)` is too weak: mach lookups to the security server would let a build script read the keychain. The profile is deny-default, with Claude Code's process and sysctl allowlist. Its mach allowlist is narrowed: Claude Code's own allows the security server (step 0).
  - Our own is versioned with Agentium (records name the grader), reviewable, and can express rules Claude Code's settings lack (the loopback rule below). The step 0 spike compares the two on the same fixtures and explains each difference.
- **The profile:** no network, except loopback when needed (below). Writes only to the grading copy, the run's grading cache, its temp root and `/dev/null`. Reads everything except the denied paths runs already use (`run.Env.denied`, with the grading copy and the run's folders allowed, plus credential files and the token file's folder), in their real forms; deps read-only. `forms`, `realForm` and the `/tmp` owner rule move from `internal/claude` to `internal/sandbox`, one implementation for both.
- **Toolchains:** the host's own, as in the agent's run, so agent and grader see the same JDK, Go and Python: the fairness argument against grading in Linux containers.
- **The grader's environment is the agent's recipe** (the profiles' `AgentEnv`, `PrepareRun`, deps read-only), already proven offline in this sandbox for Go, Maven, Gradle, Cargo and Python, instead of the shared `CommandEnv`. Two additions from step 0:
  - for the JVM profiles, `JAVA_TOOL_OPTIONS=-Djava.io.tmpdir=<temp root>`;
  - for Gradle, `GRADLE_DAEMON_BIND_ADDRESS=::1`, with no IPv4-only flag.

  Its cache is per run (a shared writable one is the poisoning path): a clone of a seed only trusted commands write (validation, warm-up), removed after grading. The clone is one `clonefile(2)` call on the seed folder, not `cp -Rc`, which clones file by file (step 0 timing).
- **Fail closed:** a canary under the same profile first runs `/usr/bin/true`, writes the temp root and fails to read a denied path. If `sandbox-exec` is missing, refused or nested (Agentium inside a sandbox), the grade is an infrastructure outcome (retried or left out), never a fail, never a host grade instead.

### What will break under a sandboxed grader
- **Gradle's file-lock service** binds a local UDP socket: Gradle grading needs local binding.
- **Gradle worker daemons** (Checkstyle, PMD, CodeNarc, forked compilers) connect back over `::ffff:127.0.0.1`, which `localhost:*` does not match, and Gradle strips `JAVA_TOOL_OPTIONS` from workers. **Settled by step 0:** no seatbelt rule names that address. The fix is Gradle's own `GRADLE_DAEMON_BIND_ADDRESS=::1`, which makes the build listen on and announce `::1`, which `localhost:*` matches. Checkstyle passed with it. Without it, Gradle does not even start under a loopback-only profile (its single-use daemon listens on `127.0.0.1`, which a JVM opens as `::ffff:127.0.0.1`).
- **JVM temp files:** on macOS the JVM takes `java.io.tmpdir` from the user's `/var/folders/<x>/T`, not from `TMPDIR`, and the sandbox denies writes there (jackson-core: 5 errors under our profile, 35 under Claude Code's, which also lost a jansi lock). The grader sets it to the temp root. The agent's sandbox has the same limit today.
- **Tests that need sockets:** Go's `httptest` and servers on `127.0.0.1:0` need loopback bind and connect. Tests needing the internet, Docker, Testcontainers or a local database fail.
- **A dependency the agent added** can no longer be fetched at grading (the agent's sandbox never could): a fail, made readable with `GOPROXY=off` and offline flags; a recorded difference between modes.
- **Tests that write outside the copy** fail: `~/Library/Caches`, `$HOME` dotfiles, the keychain, `launchctl`.
- **Telling a sandbox failure from a test failure.** A sandboxed grade never silently turns a real pass into a fail:
  1. **Validation in the same mode.** A lock in sandbox mode accepts only tasks whose last validation ran in that mode with the same profile version (a new `Validation.Grader` JSON field, so no migration). The reference solution passing in the sandbox proves the environment can grade a correct solution.
  2. **The canary** separates "the sandbox did not start" (infrastructure) from "the tests ran".
  3. **Denials are recorded.** Source (step 0): the unified log, matched by a tag unique to each grade, which the profile names in `(deny default (with message "<tag>"))`. A failed grade with denials outside what the agent's own sandbox imposes is flagged in the record and the report; decision 3 says how it counts.

### Compatibility
- Records gain `grader` and the profile digest; the lock gains `grader`. Empty means `host`, so old records keep their meaning and old locks resume unchanged. A run or resume whose grader differs from the lock is refused: one experiment never mixes modes. The report shows the mode.
- Comparability is measured: step 4 grades the same kept agent trees in both modes and records how often they agree.

### Work
- [x] **0. Spike** (free, no agent run; not code). Risk: low.
  - Hand-written profiles, ours and Claude Code 2.1.285's extracted one, on fixtures: Go with `httptest`, Maven (jackson-core), Gradle with Checkstyle (junit-pioneer b8b747c), Cargo with `build.rs` (bytes), pytest with `conftest.py`.
  - *Acceptance:* each fixture matches its host result or names its break; the worker-daemon loopback rule is settled; the denial source is readable without admin rights; keychain and credential reads, deps writes and network are denied; a warm seed's clone time is measured.
  - *Packages:* none.
  - **Done 2026-10-02.** No Claude Code session ran and nothing was spent.
    - *The harness* (throwaway, not committed):
      - a scratch copy of the module, printing what `claude.Invocation` builds for a run: the environment, the settings and `DeniedPaths`, and running `buildtool`'s warm-ups and `PrepareRun`;
      - Claude Code 2.1.285's profile function (`WV` and its helpers), extracted from its binary and called from Node with the run's settings;
      - our profile, written by a short Python script.
    - *The fixtures* were cloned into a scratch folder, never into a user's repository. Their deps were warmed there with network, by the profiles' recipes (Python's from its plan).
    - *A fake data folder:*
      - the grading copy at `<data>/records/<run>/verify`, as `run.Env.grade` keeps it;
      - the grading cache at `<data>/cache/grading/<run>`;
      - another run's records and workspace, a shared cache entry, a database and a project repository;
      - a temp root per run.
    - *Each grade* started from a fresh copy and an empty cache, in the agent's environment (`Invocation.Command`'s, less Claude Code's own variables, with `TMPDIR` set to the temp root). The host grade used the same environment.
    - *Claude Code's profile* got the run's settings, plus `allowRead` for the grading copy, cache and temp root, which lie inside the denied `records` and `cache`. The `denyWrite` entries that hold those three were dropped; writes are denied by default anyway. No proxy ports.
    - *A deviation:* the temp root was not one of Agentium's short `/tmp` roots, so the shared Claude Code temp folders were not denied; the scratch folder lay inside one.
  - **Per fixture.** The commands, from the grading copy:
    - Go: `go test ./...` (GOMODCACHE in the deps folder, `GOPROXY=off`);
    - Maven: `./mvnw -B test`;
    - Gradle: `./gradlew checkstyleMain checkstyleTest`, then `./gradlew test --tests DisabledUntilExtensionTests --tests RetryingTestExtensionTests`;
    - Cargo: `cargo test`, with a `build.rs` added as an agent would;
    - Python: `python -m pytest tests --continue-on-collection-errors`.

    | Fixture | Host | Our profile | Claude Code's profile (the run's settings) | Breaks and causes |
    |---|---|---|---|---|
    | Go: a scratch module, an `httptest` server and a module dependency | 2 passed | 2 passed | **fail**: `listen tcp6 [::1]:0: bind: operation not permitted` | Claude Code's settings give only Gradle projects local binding; ours allows loopback for every project |
    | Maven: jackson-core `cacf488` | 1983 run, 0 failed, 2 skipped | the same, with the JVM temp folder set (without it: 5 errors) | 35 errors; with the JVM temp folder set, the same as the host | `java.io.tmpdir` is the user's `/var/folders/<x>/T`, which both profiles keep read-only. The grader points it at the temp root (`JAVA_TOOL_OPTIONS`); the agent's sandbox has the same limit today |
    | Gradle with Checkstyle: junit-pioneer `b8b747c` (Gradle 9.7.1) | Checkstyle passed; 37 tests passed | the same with `GRADLE_DAEMON_BIND_ADDRESS=::1`; without it Gradle does not start | as configured, Gradle does not start ("Could not connect to the Gradle daemon"). With Claude Code's `-Djava.net.preferIPv4Stack=true`: the pilot's break (tests pass, the Checkstyle worker cannot connect). With `::1` and no IPv4-only flag: the same as the host | The IPv4-mapped loopback (below) |
    | Cargo with `build.rs`: bytes `7930d93` | 1305 passed | 1305 passed | 1305 passed | none |
    | pytest with `conftest.py`: click `06b2a67` | 2182 passed, 25 skipped, 1 xfailed, 1 collection error | the same | the same | None from the sandbox. The error is the Python recipe's known limit: `test_deprecations.py` calls `importlib.metadata.version("click")`, and the project is not installed. It belongs to the Python plan |

    *Times* were noisy (other work shared the machine) and show no consistent cost of the sandbox:
    - Go: 5–11 s;
    - Maven: 17–24 s per mode, and once 131 s on the host, run beside the Cargo grades;
    - Gradle: host 24–61 s, ours 31–156 s, Claude Code's 29–39 s;
    - Cargo: 57–127 s;
    - pytest: 3–16 s.
  - **The worker-daemon loopback rule: no seatbelt rule exists.**
    - A network address in a profile must have the host `*` or `localhost`. `127.0.0.1`, `::ffff:127.0.0.1`, `[::ffff:127.0.0.1]` and `lo0` are refused when the profile compiles ("host must be * or localhost").
    - The `ip4`, `ip6`, `tcp`, `tcp4` and `tcp6` forms of `localhost:*` compile but do not match the mapped address. `remote-address`, `ip-prefix` and regexes do not exist.
    - A Java probe under `localhost:*` rules: `::1` binds and connects; `127.0.0.1` is denied at bind and connect. Only `(allow network*)` or `*:*` lets it through.
    - Gradle has no Unix-socket transport for workers.
    - **The fix is in Gradle, not the profile.** Gradle 9.7.1's `InetAddressFactory` reads `GRADLE_DAEMON_BIND_ADDRESS`. Set to `::1`, the build listens on `::1` and tells workers that address. Workers lose the environment but get the address, connect over `::1`, and `localhost:*` matches them. With it, `checkstyleMain checkstyleTest` passed under both profiles.
    - It conflicts with Claude Code's `-Djava.net.preferIPv4Stack=true` ("Unsupported address type").
    - **For agents** (the Java plan's open item): Claude Code adds that flag to `JAVA_TOOL_OPTIONS` after the inherited value, unless the value already contains the flag. An inherited `-Djava.net.preferIPv4Stack=true -Djava.net.preferIPv4Stack=false` plus the bind address passed Checkstyle under Claude Code's profile (the JVM keeps the last value). This needs a real session, and it rests on Claude Code's internals.
    - Gradle versions before 9.7.1 were not checked for the variable.
    - **The rule for step 1:** `(allow network-bind (local ip "localhost:*"))`, `(allow network-inbound (local ip "localhost:*"))` and `(allow network-outbound (remote ip "localhost:*"))`.
    - *Found:* the `localhost` bind and inbound rules also let a process listen on the wildcard address. A server inside our profile bound to `0.0.0.0` (or `::`) accepted a connection made to the machine's LAN address from outside the sandbox, on the same machine. A connection from another host was not tried. So a hostile build could serve the network while it grades: no outbound connect, but a party that can reach the machine could connect in. The firewall and NAT are the guard. Claude Code's `allowLocalBinding` (`*:*`) allows the same and more. No narrower seatbelt rule was found; step 1 records the limit.
  - **Denial source** (without `sudo`; the account is an administrator, so a non-administrator account was not checked):
    - `/usr/bin/log stream --style compact --predicate 'eventMessage ENDSWITH "<tag>"'` during the grade, or `/usr/bin/log show --last <n>m` with the same predicate afterwards. Use the absolute path: zsh has a `log` builtin.
    - It prints the kernel's `Sandbox: <process>(<pid>) deny(1) <operation> <path>`, then the tag. The tag is the profile's `(deny default (with message "<tag>"))`, the mechanism Claude Code's own monitor uses; each grade needs its own tag.
    - The kernel merges repeats ("10 duplicate reports for ...") and limits the rate, so counts are lower bounds.
    - Tools add denials unrelated to the grade: `security`, for one, looks up `com.apple.diagnosticd` and `com.apple.analyticsd`. Claude Code's monitor ignores these lookups; ours should too.
    - `sandbox-exec` reports nothing itself. The `(trace ...)` directive is accepted but writes nothing.
    - For the canary: a nested `sandbox-exec`, a missing profile and a profile that does not compile all exit 65, which a test can also return; a command's own exit code passes through.
  - **Hostile probes**, from the grading copy, with output thrown away (reads open the file and read nothing):

    | Probe | Ours | Claude Code's (agent) profile |
    |---|---|---|
    | Mach lookup of `com.apple.SecurityServer` (no message sent) | denied | **allowed** |
    | Keychain search (a made-up service name) | fails: no security server | **runs** (the login keychain is searched) |
    | Read `~/Library/Keychains/login.keychain-db` | denied | **allowed** |
    | `~/.ssh`, `~/.config/gh` | denied | denied |
    | `~/.claude/.credentials.json`, `~/.pypirc`, `~/.netrc`, pip's `pip.conf`, uv's `credentials.toml` | absent on this machine (a missing path reads as missing, deny or not); ours lists them all | the first three are listed, pip's and uv's are not |
    | Write the deps folder, the user's `GOMODCACHE`, another run's workspace, the shared cache | denied | denied |
    | Read another run's hidden test, this run's own records, the shared cache, the database, the deps folder's Gradle home | denied | denied |
    | `curl https://example.com`; `curl https://1.1.1.1`; TCP to 1.1.1.1:443; UDP to 1.1.1.1:53 | denied (no DNS; connect and send refused) | denied |
    | Links planted in the grading copy, to the shared cache, `~/.ssh` and the deps folder (read, list, write) | denied | denied |
    | Loopback TCP on `127.0.0.1` (Python) | allowed | allowed (Gradle settings) |

    The Claude Code credential itself was not read: the permission system refused keychain access in this session (see the agent-run finding above). No probe file was left in the deps folder or `GOMODCACHE`.
  - **Timing of the per-run cache.** Seeds:
    - a run's Gradle home after a grade: 259 MB, 2,385 files;
    - a Go build cache of this repository's tests: 147 MB, 1,890 files;
    - six copies of both: 2.4 GB, 25,650 files.

    Each was timed three times on a loaded machine:

    | Seed | `clonefile(2)` on the folder | `cp -Rc` | `cp -R` | Removal |
    |---|---|---|---|---|
    | Gradle home | 0.03–0.13 s | 0.9–3.5 s | 0.7–6.1 s | 0.9–1.4 s |
    | Go build cache | 0.03–0.14 s | 0.9–1.9 s | 1.8–3.1 s | 0.3–0.8 s |
    | 2.4 GB, 25,650 files | 0.40–0.53 s | 11–22 s | 11–38 s | 2–15 s |

    `cp -Rc` clones file by file and costs about as much as a plain copy. One `clonefile(2)` call on the folder costs under a second even for the large seed. Removal is the larger cost; it can run after the grade, off the critical path. Go reaches `clonefile` through the cgo the SQLite driver already needs, or through `golang.org/x/sys/unix`, a new module that needs [approval](../rules/supply-chain.md).
  - **Recommended profile source for step 1: our own (confirmed).** Claude Code's profile, as the run's settings build it:
    - has no loopback for non-Gradle projects;
    - keeps the security server and the keychain files open;
    - binds any local address when local binding is on;
    - lacks pip's and uv's credential stores.

    Ours, as the spike ran it:
    - `(deny default (with message "<tag>"))`;
    - Claude Code's process and sysctl rules;
    - Mach lookups only for `opendirectoryd.libinfo`, `opendirectoryd.membership`, `bsd.dirhelper`, `logd`, `system.logger` and `system.notification_center`. The security server, launch services, fonts, audio and power were dropped, and none of the five fixtures needed them;
    - reads everywhere except the whole data folder and the run's denied paths (`DeniedPaths`), plus `~/Library/Keychains`, `~/.config/pip`, `~/Library/Application Support/pip`, `~/.config/uv` and `~/.local/share/uv/credentials`. `~/.local/share/uv/python` stays readable. Then the grading copy, its cache, the temp root and the project's deps are allowed, then `DepsDenied` is denied again;
    - metadata reads of those folders' parents only;
    - writes to the grading copy, the cache, the temp root and the standard devices only;
    - the loopback rule above.

    Our profile closed every gap found in Claude Code's: the security server and the keychain files denied, loopback for every project, pip's and uv's credential stores denied. The grader's environment adds the two variables under Design.
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
- **On the Mac, a separate user:** running agents (and grading) as their own macOS account, with no login keychain, browser profiles or dotfiles of the user's, is the strong defense in depth for local runs: it removes what the keychain and credential denies only hide. It needs a one-time account setup by the user and shared access to the workspaces; its own plan decides whether it comes before containers.
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
