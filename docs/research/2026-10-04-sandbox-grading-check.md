# Sandbox grading: real check (isolation step 4)

- Date: 2026-10-04
- Scope: [isolation plan](../../.agents/plans/2026-10-02-isolation.md) step 4, part 1 (macOS sandboxed grading, `sandbox-v1`).
- Machine: Apple Silicon, macOS 15 (Darwin 24.6), APFS; shared with other agents running Go tests, so timings are noisy.
- Spend: $0.71 (paid part, approved cap: worst case ≤ $4, approved as about $3). Free parts cost nothing.

Console output below is real, trimmed, and home paths are shown as `~`.

## Free part

### Re-validation in sandbox mode

`agentium task validate --all --grader sandbox`, run from each project's folder with that project's data folder.

**This repository (Go), data folder `~/.agentium-acceptance`:** 15 of 16 tasks valid; the one invalid (`calibration-sign-in`) is also invalid on the host (`base/hidden-tests wanted fail`, pre-existing), so the sandbox is not the cause.

**Java/Rust pilot, data folder `~/.agentium-pilot`:**

| Project | Toolchain | Result in sandbox |
|---|---|---|
| bytes | Cargo | 3 of 3 valid |
| jackson-core | Maven (JVM) | invalid (also invalid on the host) |
| junit-pioneer | Gradle (JVM) | invalid (also invalid on the host) |

The JVM tasks fail in **both** host and sandbox mode, for reasons unrelated to the sandbox:

- **junit-pioneer (Gradle 8.5):** the machine's default JDK is now Java 22 (Corretto 22.0.2). Gradle 8.5 rejects it: `BUG! exception in phase 'semantic analysis' ... Unsupported class file major version 66`. The dependency warm-up and the reference stage both fail before grading matters.
- **jackson-core (Maven):** `Non-resolvable parent POM ... com.fasterxml.jackson:jackson-base:pom:2.21.0-SNAPSHOT (absent) ... was not found in ... maven-snapshots`. A stale upstream snapshot was purged; the offline warm-up and the online reference build both fail.

Both are host-environment drift (a JDK upgrade and an expired upstream snapshot) that happened since the pilot was built on 2026-09-30. They are recorded here as a limitation, not a sandbox regression. Only Go and Cargo tasks were gradeable for the agreement and denial-count work.

### Flagged-denial counts per toolchain

Denials read from the stored sandbox validations (`task.SandboxGrade`). "Flagged" = a limit the grading profile imposes that the agent's own sandbox does not.

| Toolchain | Flagged denials (per stage) | Notes |
|---|---|---|
| Go (this repo, 16 tasks) | 0 | no denials logged at all |
| Cargo (bytes, 3 tasks) | 2 | `mach-lookup com.apple.distributed_notifications@Uv3`, `mach-lookup com.apple.metadata.mds` |
| JVM (Maven, Gradle) | not measurable | builds fail before grading (above); one non-flagged `network-bind local:*:0` seen |

Cargo also logs denied (not flagged) writes to the read-only deps cache (`<deps>/cargo/.package-cache`, `.global-cache`, `.package-cache-mutate`); the grades still pass, so Cargo tolerates them. These writes are a limit the agent's sandbox imposes too, so they are correctly not flagged.

The two Cargo Mach lookups were absorbed by the per-task harmless mechanism (the reference stage logged them too while passing). Because they are pure system-service lookups that leak nothing when denied — the service is simply unreachable and the grade passes — they are now added to `sandbox.Denial.Noise` (system notification and Spotlight-metadata daemons), so they no longer depend on the per-task harmless set. `distributed_notifications` carries a per-boot instance suffix, so both match by prefix. The profile still **denies** them; Noise only stops the false flag. A unit test covers the new noise and a near-miss (`com.apple.metadata.mdwrite`, and a `file-read` of a like-named target, are not noise).

### The open question: channels out of a grade

Step 1's limits left open whether to deny agents `log show` or to accept the `/mp-` semaphore and unified-log channels explicitly. Both channels were confirmed under the real profile:

- **`/mp-` semaphores:** `go test ./internal/sandbox -run TestSandboxLimitsPOSIXIPC` passes: a grade may create `/mp-<name>` semaphores (Python multiprocessing needs them) but every other semaphore name and all POSIX shared memory are refused.
- **Unified log:** a sandboxed process (allowing `com.apple.logd`, as the grading profile does) wrote a marker with `/usr/bin/logger`, and an unsandboxed `log show --predicate "eventMessage CONTAINS '<marker>'"` read it back:

```
inside-sandbox marker: HIDDENTEST-<ts>-grepforme
wrote-via-logger
=== unsandboxed log show reads it back: ===
2
```

**Decision (recommended): accept the risk explicitly for `sandbox-v1`.** The `/mp-` semaphore channel cannot be closed without breaking Python multiprocessing grades, so denying `log show` would close only one of two equivalent channels. Both require the model to collude with itself across runs (the hostile build and a later agent being the same model), and container mode (part 2) closes both cleanly. The code and the plan already document both as known limits.

A full stub-agent-through-grading hostile fixture was not built end to end; the channels were demonstrated directly under the real profile and by the committed `internal/sandbox` darwin tests, which answers the open question.

## Paid part

A small A/A calibration (both arms `base`), 2 Go tasks (`deny-login-file`, `scrub-whole-paths`) × 1 run per arm = 4 runs, `--grader sandbox`, `--run-budget 0.60`, `--budget 4`.

Preview worst case: $3.00 for the 4 runs + at most $0.65 calibration = $3.65, within the $4 cap.

The run paused once at the subscription usage limit (the 5-hour window was at 74%) after 2 runs and resumed after the window reset. Final:

```
Experiment sandbox-check: done
  4 of 4 runs settled; spent $0.71 of $4.00 (calibration $0.10 of it, not in the arms' costs)
ARM  CONTEXT  SETTLED  FAIR  SUCCESSES  UNFAIR  INFRA  CANCELLED   COST
A    base         2/2     2          2       0      0          0  $0.31
B    base         2/2     2          2       0      0          0  $0.29
```

Per-run sandbox grading (from `report --json`): every run `grader=sandbox-v1`, `canary=passed`, 0 denials, 0 flagged. All 4 passed the hidden tests, graded in the sandbox.

Report note: "Graded in Agentium's grading sandbox (sandbox-v1): no network but this machine's, writes only to each grade's own folders. On macOS the sandbox's localhost is every address of the machine, so a grade could accept connections from the network."

**Host vs sandbox agreement:** for the valid Go (this repo) and Cargo (bytes) tasks, the reference solution passes in both modes (validation is OK in both), and the 4 real sandboxed runs graded exactly as their tests dictated (all pass). A per-tree `run once --keep` host-vs-sandbox regrade was not run: the $4 cap was spent on the 4-run experiment, and the JVM trees (the toolchains most likely to differ, through Gradle workers) are unavailable because of the environment drift above.

## Verdict

Sandboxed grading works on real Claude Code runs: the canary holds, grades run in the sandbox, denials are read, and legitimate Go and Cargo grades produce no flagged denials (after the noise tuning). The known loopback and exfiltration limits stand and are documented. The JVM toolchains could not be exercised end to end on this machine because of unrelated host drift; that gap, and the missing per-tree host/sandbox regrade, are the limits of this check.
