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

The two Cargo Mach lookups were absorbed by the per-task harmless mechanism (the reference stage logged them too while passing). Because they are pure system-service lookups that leak nothing when denied — the service is simply unreachable and the grade passes — they are now added to `sandbox.Denial.Noise`, so they no longer depend on the per-task harmless set. launchd names distributed notifications' per-user and system instances (`@Uv3`, `@0v3`, `@1v3`) and Spotlight has a family (`mds`, `mds.index`, `mds.xpcs`), so both match by prefix. The profile still **denies** them; noise only stops the flag. Unit tests cover the new noise, that it is not flagged, and near-misses (`com.apple.metadata.mdwrite`, and a `file-read` of a like-named target, are not noise); removing either prefix fails them.

Two trade-offs, both rare and accepted:

- Claude Code's agent sandbox allows distributed notifications. So a failed grade blocked on that lookup, which decision 3 left out as `infra-sandbox`, now counts as the agent's failure. It matters only where the candidate makes a lookup the reference does not.
- Noise is left out of `DenialCount`, so an attempt to post data through distributed notifications is still denied but no longer visible in the records.

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

**The user's decision 8 (2026-10-04, "Accept, document it"): both channels are accepted for `sandbox-v1`.** It rests on the direct demonstrations above, not on the planned hostile fixture, which was not built. The `/mp-` semaphore channel cannot be closed without breaking Python multiprocessing grades, so denying `log show` would close only one of two equivalent channels. Both require the model to collude with itself across runs (the hostile build and a later agent being the same model), and container mode (part 2) closes both cleanly. The code and the plan already document both as known limits.

The planned hostile fixture (a stub agent whose `build.rs`, `conftest.py` or Gradle script tries deps writes, data-folder reads, outbound connections and the keychain, then a later stub reading the channels back) was not built; that remains a gap.

## Paid part

A small A/A calibration (both arms `base`), 2 Go tasks (`deny-login-file`, `scrub-whole-paths`) × 1 run per arm = 4 runs, `--grader sandbox`, `--run-budget 0.60`, `--budget 4`.

Preview worst case: $3.00 for the 4 runs (each at its $0.60 cap plus the $0.15 overshoot allowance the budget holds per run) + at most $0.65 calibration = $3.65, within the $4 cap.

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

Done with gaps. Sandboxed grading works on real Claude Code runs: the canary holds, grades run in the sandbox, denials are read, and legitimate Go and Cargo grades produce no flagged denials (after the noise tuning). Loopback and the two channels stay as documented limits (decisions 7 and 8).

Gaps:

- the hostile fixture (above) was not built;
- no pytest flagged-denial counts (the Python pilot was not re-validated);
- reads back into the agent's old workspace path (a venv built in the checkout, with absolute shebangs; from the #142 review) were not checked;
- no per-tree host vs sandbox regrade;
- the JVM toolchains were not exercised (host drift on this machine);
- docs: the guide names the new noise and the accepted channels; the code map and a verification note are not updated.
