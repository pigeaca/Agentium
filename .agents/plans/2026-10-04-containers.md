# Container mode: grading in Docker first, agents later

- Date: 2026-10-04
- Status: Planned (2026-10-04). Nothing built. The open decisions below await the user.
- Scope: the user's "Docker + codex" (2026-10-04). This plan covers Docker; Codex is planned separately and in parallel, and this plan names only the dependencies. It details the [isolation plan](2026-10-02-isolation.md)'s Part 2 and builds on the [direct-Docker decision](../decisions/2026-10-02-containers-direct-docker.md) and the isolation plan's decision 6. Planning only: nothing was pulled, built, started or paid for.

## Outcome
A third grader mode, `container-v1`, beside `host` and `sandbox-v1`. In it, a run's verification commands, and a validation stage's, run in a throwaway Linux container. The container has no network, mounts no host folders, uses a pinned toolchain image and has resource limits. The mode:
- closes what `sandbox-v1` leaves open: the `/mp-` semaphore and unified-log channels (isolation decision 8), and loopback reaching the LAN and the machine's services (isolation decision 7);
- gives Linux an isolated grader (today Linux grades on the host, unsandboxed);
- is the base for running agents in containers later: Claude Code, and Codex once its plan lands.

## This machine (read-only check, 2026-10-04)
- Apple M4 Pro (arm64), 12 cores, 24 GB of memory, macOS 15.7.7.
- The Docker CLI 28.0.4 (Homebrew, client only), colima 0.8.1 and Lima 1.0.7. There is no Docker Desktop, OrbStack or Podman.
- colima's `default` VM exists and is **stopped**: `vz`, aarch64, **2 CPUs, 2 GiB**, a 100 GiB disk (10 GB used), the docker runtime. Its mounts use sshfs, which is fixed when the VM is created. colima's default mounts put the home folder and `/tmp/colima` into the VM, writable.
- No daemon answers (`docker version` cannot connect). The only context is `default` (`/var/run/docker.sock`, which is absent); colima adds and activates its own context when it starts. There is no buildx plugin. `~/.docker/config.json` holds registry logins (`auths`, not read).
- Which images the VM holds is unknown until it runs.
- So step 0 needs the user to start colima, with more memory for JVM grades (for example `colima start --cpu 4 --memory 8`). Agentium never starts or configures it. Grades mount no host folders (below), so sshfs's speed and file locking do not matter.

## Design

### What runs where
- Agents stay on the host, in Claude Code's sandbox, as today. Only the verification commands move into containers, for grading and for validation (open decision 1).
- Setup, warm-ups, restoring the check scripts and adding the hidden tests stay on the host, unchanged. The container receives the finished grading copy.

### Images
- **One image per toolchain**: official images, pinned by multi-arch index digest in Agentium's source. The table lists toolchain, version, digest and compressed size per architecture.
  - Go: `golang:<ver>`.
  - JDK: `eclipse-temurin:<jdk>-jdk`. Projects without `mvnw` use `maven:<ver>-eclipse-temurin-<jdk>`. Gradle runs through the project's wrapper, and its distribution is warmed with the deps.
  - Python with uv: Astral's `ghcr.io/astral-sh/uv:<ver>-python<minor>-<debian>-slim`.
  - Rust: `rust:<ver>-slim`.
  - Node with pnpm comes with the TypeScript work.
- **No image build for grading.** A grade needs the toolchain, `sh` and `tar`, which these images have. There is no Dockerfile and no published Agentium image (open decision 2).
- **Run by digest** (`golang@sha256:…`), so the daemon cannot substitute another image. After a pull, Agentium checks the image's `RepoDigests`.
- **Version match.** The image's toolchain matches the agent's on the host, as `pool.DetectToolchain` reads it: Go major.minor, the JDK major version, the Python minor version, the Rust minor version. When no pinned image matches, the mode is refused and the error names the pinned versions (open decision 6). The record keeps both toolchains, the host's and the image's.
- **Consent.** Agentium never pulls on its own.
  - `agentium images` lists the pinned images a project needs, whether each is present, and their sizes.
  - `agentium images pull` shows the total, asks (or takes `--yes`), then runs `docker pull` by digest.
  - `experiment new --grader container` and `task validate --grader container` refuse while an image is missing, naming the command and the size.
  - Changing a pin is a dependency change and needs approval ([supply chain](../rules/supply-chain.md)).
- **Sizes** (approximate compressed downloads for arm64; step 0 measures them): Go about 0.3 GB, the JDK 0.2 GB, Python with uv under 0.1 GB, Rust 0.3 GB. That is under 1 GB for all four, and about 2.5 GB on disk in the VM.

### Dependencies
- The host's deps folder cannot be used as it is. Python venvs and native pieces are macOS builds. Mounting host folders into the VM is slow (sshfs here) and reaches outside the container.
- Container deps are **warmed inside a container** of the same image, with network, from the base commit only. This follows the trusted-repository rule that host warm-ups already follow. They go into a Docker volume per project and image digest (`agentium-deps-<data>-<project>-<digest12>`).
- The host deps folder's rules apply to this volume too: a warm-up lock (a host file), per-base stamps, last-use marks and cleanup.
- Grades mount the volume read-only. Each project, base and image needs one warm-up (time and network, measured in step 0).

### The grade's container
**Nothing from the host is mounted**: not the copy, the data folder, the home folder or the Docker socket. Each grade has one container, in this shape (step 0 settles the details it names):
```
docker create --name agentium-<data8>-<run>-grade --label agentium.data=<data8> --label agentium.run=<run>
  --label agentium.mode=container-v1 --rm --init --network none --ipc private --cap-drop ALL
  --security-opt no-new-privileges --read-only --user <non-root> --memory <m> --memory-swap <m>
  --pids-limit <p> --cpus <c> --shm-size 256m --log-driver none --tmpfs /tmp
  <writable /work and /cache> --mount type=volume,src=<deps volume>,dst=/deps,readonly
  <image>@sha256:<digest> sleep <deadline>
```
- **Inspect before start.** `docker inspect` of the created container is checked against what was asked, before it starts: network, IPC, capabilities, privileges, user, the read-only root, mounts and their modes, devices, limits and the image ID. Its normalized digest is recorded, as the sandbox's profile digest is.
- **Copy in.** Agentium writes the grading copy as a tar with `archive/tar`, never following a link, and caps its size. The image's own `tar` unpacks it into `/work` (`docker exec -i`), as the grade's user, before any agent code runs.
  - A link the agent planted points nowhere inside the container.
  - The host copy is never changed by the grade, so `--keep` keeps it as it was graded.
  - If copying in is too slow for large trees, the fallback is a read-only bind of the copy with a writable layer inside the container (step 0 measures).
- **Commands.** Each verification command runs as `docker exec --env-file <file> -w /work/<module> <name> sh -c <command>` through `runner`, in order, and stops at the first failure.
  - The environment is the agent's recipe (`buildtool.GraderEnv`) with paths inside the container (`/work`, `/cache`, `/deps`, `/tmp`), plus `GOPROXY=off` and `GOTOOLCHAIN=local`.
  - Loopback inside the container works, so `httptest` and Gradle workers need no special rule.
- **Cleanup.** `docker rm -f -v <name>` runs after the grade, on an error, on cancel (with `context.WithoutCancel` and a time bound) and on a panic.
  - Killing the `docker` client does not stop processes in the container, so a timeout or cancel kills the container, not just the client.
- **Nothing outlives its command.**
  - The container's main process ends at the deadline: every command's timeout plus a margin. `--rm` then removes the container and its anonymous volumes, even after Agentium itself was killed.
  - The container's name is written in the grade's folder before `docker create`. Recovery (`RecoverWarn`) and `agentium clean` remove the containers of dead runs by label.
  - Agentium starts no daemon, VM or background process. The user starts Docker.
- **A local daemon only** (a Unix socket). A remote `DOCKER_HOST` or an SSH context would send hidden tests off the machine, so the usability check refuses it (open decision 9).
- **The `docker` client's environment** is an allowlist: `PATH`, `HOME`, `DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG`, `DOCKER_CERT_PATH`, `DOCKER_TLS_VERIFY`.

### Proof of isolation: the canary's counterpart
There is no seatbelt log. Instead, three checks fail closed before the grade's code runs:
1. **The daemon's record.** The created container's `docker inspect` matches what was asked (above).
2. **In-container probes**, as the grade's user:
   - `true` runs;
   - `/tmp`, `/work` and `/cache` are writable; `/deps` and `/` are not (EROFS);
   - `/sys/class/net` lists only `lo`, and a connect to a TEST-NET address fails at once;
   - `/proc/self/status` shows `NoNewPrivs: 1`, `Seccomp: 2` and an empty `CapEff`;
   - there is no Docker socket, and the user is not root.
3. **The usability check**, when a command starts. The daemon is reachable and local; it runs Linux with seccomp and cgroup v2; the images are present by digest; and `docker info` reports more memory and CPUs than the grade's limits.

A failure is infrastructure and is retried. It is never a fail, and the grade is never run in another mode instead.

### Resource limits: isolation decision 3's counterpart
- Resource limits are the only limits a container imposes that the agent's sandbox does not. After the commands, an end probe reads the container's cgroup counters (`memory.events` `oom_kill`, `pids.events` `max`). The grade cannot write these kernel counters.
- A failed grade where either counter is above zero is left out, as `infra-sandbox` runs are: no retry, the per-arm check and the sensitivity line (open decision 7). A pass stays a pass.
- A failure from no network or from read-only deps counts as the agent's own, as in `sandbox-v1`: the agent's sandbox imposes both limits too.

### Side channels (isolation decisions 7 and 8)
| `sandbox-v1` leaves open | `container-v1` | Shown by (steps 2 and 5) |
|---|---|---|
| `/mp-` POSIX semaphores, kept until reboot | The container's own IPC namespace and `/dev/shm` (`--ipc private`), removed with it | A grade writes `/mp-` semaphores; the next grade and the host find none |
| The unified log (`logger`, `syslog`, paths in denial lines) | There is no `/dev/log` and no logd. Output goes only to Agentium's `verify.log`, and `--log-driver none` keeps nothing in the daemon | `logger` writes nowhere readable later; `docker logs` has nothing |
| Loopback means every address of the Mac (listening on the LAN, reaching local services) | `--network none` leaves only the container's own `lo`. Nothing outside can be reached or can reach in, and concurrent grades cannot reach each other | A server on `0.0.0.0` is unreachable from the host and the LAN; connects to the host gateway and to another grade fail |
| (no counterpart) | The kernel keyring is blocked by Docker's default seccomp profile | A `keyctl` probe fails |

What remains is a container escape (a kernel or runtime bug), which reaches the VM. colima mounts the home folder there, writable, by default. Grades need no VM mounts, so the user may narrow them; Agentium never changes them.

### Records, locks, compatibility
- The mode is `task.GraderContainer = "container-v1"`, chosen with `--grader container`.
- Records gain `Container`: the image reference and digest, the toolchains inside, the inspect digest, the canary, the limits, the cgroup counters, and the engine's version, OS and architecture. Never paths or the endpoint.
- Validations, designs and locks record the mode. Locks also record the image digests, so a resume uses the same images after Agentium's pins change.
- A new design version makes an older Agentium refuse a container design, as `DesignVersionSandbox` did for sandbox designs.
- One mode per experiment, as today. Readiness re-validates tasks in the mode when the experiment locks, and a lock in an unknown mode is refused.
- Old locks and records read unchanged. `host` and `sandbox-v1` behave as before, and their goldens stay unchanged.

### Comparability
- On a Mac, the agent builds with macOS toolchains and the grade uses Linux ones. This is the isolation plan's fairness argument for `sandbox-v1`. Both arms are graded the same way, so paired verdicts hold. Absolute pass rates may differ: Linux paths are case-sensitive, code can take OS-specific paths, and patch versions differ.
- Agreement is measured, not assumed:
  - validation stages in all three modes on the same tasks (free);
  - the paid check's kept trees, regraded in all three modes;
  - agreement counted per toolchain.
- On Linux, the agent and the grade both run Linux; only patch versions differ.

### Linux
- Containers become the Linux default grader (open decision 3). Today Linux grades unsandboxed.
- The macOS-only parts (`stopSandboxed`, `clonefile`) are not needed: Docker removes the container and its volumes.
- With nothing bind-mounted, user IDs of host files do not matter, whether on Linux, macOS virtiofs or sshfs. Files inside belong to the grade's user, and the host copy is only read.
- Rootless Docker should work the same. Podman's Docker-compatible socket is untested; step 5 checks it if one is available.

### The Go seam
- **`internal/container` (new): the Docker CLI driver.**
  - It builds the arguments (create, exec, inspect, kill, rm, volume, pull) and parses `docker info` and `inspect`.
  - It holds the pin table, the version match, names and labels, the usability check, the probes and the tar stream.
  - Pure functions are tested without Docker. Tests that need a daemon skip, with the reason, when none answers.
- **`internal/run`:**
  - `containergrade.go`: `gradeInContainer` beside `gradeInSandbox`, with the same inputs (copy, commands, the agent's recipe, timeout, log, warnings). `verifySandboxed` in `run.go` becomes `verifyIsolated` and chooses by mode.
  - `containerCommands` beside `sandboxedCommands`, for `task.CheckoutCommands.Sandboxed` (renamed `Isolated`).
  - The container deps warm-up beside `warmInThrowawayFor`.
  - New cleanup kinds for containers and volumes in `clean.go`, and recovery by label in `recover.go`.
- **`internal/buildtool`:** the recipe with container paths; container warm steps (Python's venv is built in the container); a container `GradeEnv`.
- **`internal/task`:** the mode, `KnownGrader`, `ParseGrader`, `DescribeGrader` and `Validator.sandboxed`.
- **`internal/cli`:** `--grader container`, `agentium images`, and `clean`'s new kinds.
- **Sites that read "not host" as "sandbox" today** must ask for the mode instead. Step 1 changes them with no change in behavior:
  - `run`: `Env.sandboxed`, `SandboxUsable`, `sandboxApplies` and `tools.go`'s `CheckoutCommands`;
  - `task.Validator.sandboxed`;
  - `experiment`: `design.go` (the design version and readiness), `lock.go` and `analyze.go` (the per-arm check);
  - `report.go` (the mode line);
  - `cli`: `runview.go` and `runshowview.go`.

### Codex and agents in containers (dependencies only)
- **Grading does not depend on the agent.** Codex runs get container grading at no extra cost, once they produce a grading copy as Claude Code runs do.
- **Agents in containers need a later plan, after Codex's.** That plan covers:
  - the agent CLI in a local image built from the pinned base, pinned to the lock's agent version;
  - credentials passed by name or as a file mounted read-only (isolation decision 6), and Codex's key or login file the same way;
  - egress only to the model's API, through an allowlisting proxy. On a Mac a Unix-socket bridge does not cross the VM, so the proxy is a second container or Agentium's own listener on a Docker network.
- **Why Codex makes it matter more.** Claude Code's sandbox and Codex's impose different limits (network, writable folders, process rules), which confound an agent A/B. One container gives both agents the same environment. An agent's own sandbox may not start inside a container, which then becomes the sandbox.
- **What this asks of the Codex plan:**
  - keep the agent invocation agent-neutral (command, environment allowlist, writable folders, network needs, credential source), so the container executor can wrap either agent;
  - record each agent's version in the lock, as Claude Code's is recorded.

## Acceptance
1. `--grader container` (mode `container-v1`) grades runs and validates tasks in containers, on macOS (colima or Docker Desktop) and on Linux. `host` and `sandbox-v1` behave as before: the run, report and Python goldens are unchanged.
2. **Fail closed.** Any of these refuses the mode or makes the grade infrastructure, with a test for each: a missing or unreachable daemon, a remote endpoint, a missing image, an inspect mismatch, a failed probe. Nothing is graded in another mode instead.
3. **A hostile stub agent's tree** (a `build.rs`, a `conftest.py` and a Gradle script) is graded with every attempt failing: writing the deps, reading host paths (the data folder, other tasks' hidden tests, the home folder), connecting out, and listening for the LAN.
4. **Tests show the side channels of isolation decisions 7 and 8 closed** (the table above).
5. **No container or anonymous volume outlives a grade**: after a success, a failure, a timeout, a cancel and a killed Agentium (by the deadline, and by the next recovery). Shown with a real daemon.
6. **Images are never pulled without consent**; the consent shows the size, and runs use digests only.
7. **Records, validations, designs and locks** name the mode and the image digests. Old ones read unchanged, and mixed modes are refused.
8. **A failure with an OOM kill or a hit on the process limit** is left out, as isolation decision 3 does. A pass stays a pass.
9. **Agreement across the three modes** is measured and recorded (step 5).

## Boundaries
- Agents stay on the host. Agents in containers get their own plan, after Codex's. No Harbor.
- No Go module is added; Agentium uses only the `docker` CLI. Agentium installs, starts and configures no Docker, colima or VM, and leaves the user's settings alone.
- No image is built for grading, and no Agentium image is published.
- Pulls, pin changes, and a CI job that pulls an image all need the user's approval.
- `sandbox-v1` stays the macOS default (open decision 4).
- Paid checks need their own approval.

## Work
Each step is one PR with green CI and the reviewer's [threat checklist](../roles/reviewer.md#threat-checklist). Steps go in order; step 0 informs steps 2–4.

- [ ] **0. Spike.** Free of charge, but it needs the user to start colima with at least 4 CPUs and 8 GiB, and the user's consent to pull the Go, JDK, Python and Rust images (about 1 GB). No code. Risk: low.
  - *Fixtures:* the isolation plan's step-0 set: Go with `httptest`, Maven jackson-core, Gradle junit-pioneer with Checkstyle, Cargo bytes with `build.rs`, pytest click. They are graded by hand, in this shape.
  - *Acceptance:* each fixture matches its host result or names its break. Timings for start, copy-in, warm-up and tests are compared with host and sandbox grades. Image and volume sizes are measured. These points are settled:
    - how `/work` and `/cache` become writable for a non-root user with every capability dropped (a tmpfs with owner options, a volume, or a step run as root);
    - a user the toolchains accept (Java takes `user.home` from the user database, not `HOME`);
    - whether `lo` has IPv6 under `--network none` (Gradle's bind address);
    - `docker exec` output with `--log-driver none`;
    - copying in by `docker cp` or by a tar through `docker exec -i` with a read-only root;
    - the cgroup counters readable inside, and an OOM of an exec'd process seen there;
    - what stays after killing the client, after `docker kill`, and at the deadline;
    - the side-channel probes.
  - *Packages:* none.
- [ ] **1. The mode seam** (no behavior change). The sites listed under The Go seam ask for the mode, not "not host". `task.GraderContainer` exists but is refused as unknown until step 4. Risk: medium. *Packages:* `task`, `run`, `experiment`, `report`, `cli`.
- [ ] **2. `internal/container`.** The driver, names and labels, the usability check, the inspect check, the probes, the tar stream, and the kill and removal on cancel. Unit tests use a fake `docker` (its argv pinned). Tests that need a real daemon skip without one. Risk: high (isolation, cancellation).
- [ ] **3. Images and container deps.** The pin table and the version match. `agentium images`, with consent and sizes. Deps volumes warmed in containers, Python's venv included. Recovery and `clean` for containers and volumes; pulled images are only listed, and removed only with a flag. Risk: high (downloads, consent, supply chain, cleanup).
- [ ] **4. Wiring and records.** `gradeInContainer` and validation's hook for containers; `--grader container`. Records, validations, designs and locks; the cgroup counters feed isolation decision 3's rule (open decision 7). Report and `run show` lines; the Linux default. Risk: high (hidden tests, persistence, concurrent runs).
- [ ] **5. Real check and docs.** Risk: medium.
  - *Free:* validate this repository's tasks and the bytes pilot's in container mode, and count validation agreement across the three modes. The hostile stub and the side-channel tests run on a real daemon.
  - *Paid (approval):* the check in open decision 5.
  - *Docs:* the guide (container grading and its limits; the sandbox's known limits point to it), the code map, the architecture and a [verification](../rules/testing.md) note. Then archive this plan.

A CI job running the real-daemon tests on GitHub's Linux runners would pull one pinned image. It is proposed in step 2 and needs approval.

## Verification
- `python3 scripts/harness.py check changed` for each step. The real-daemon tests run locally on colima and report their skips when no daemon answers.
- Mutation checks, as `sandbox-v1` had: each of these is weakened one at a time and must make a test fail:
  - the network, IPC, read-only deps, capabilities and no-new-privileges flags;
  - `--rm`, the deadline, the inspect check and the kill on cancel.
- Console samples of `images`, `run once --grader container` and `run show`.

## Open decisions (for the user)
1. **Grading only first, or agents too?** Recommended: grading first. It closes the open channels and Linux's gap without putting credentials into containers. Agents follow in their own plan after Codex's, which designs the proxy and the credentials once, for both agents.
2. **Where do the images come from?** Recommended: official images pinned by digest and pulled with consent, with no build for grading. Python with uv uses Astral's image, which is the vendor's own, not a Docker Official Image. A local build from pinned bases comes only when agents move into containers. No published Agentium image: it would make Agentium a registry to secure and sign. Downloads are about 1 GB for all four toolchains, or 0.3 GB for Go alone.
3. **The default on Linux and macOS.** Recommended: `container-v1` on Linux. Without a usable Docker, `experiment new` refuses and names `--grader host`, which is today's behavior made explicit. macOS keeps `sandbox-v1`. Existing experiments keep their mode. The alternative keeps `host` as Linux's default and prints a warning.
4. **Replace the macOS sandbox, or sit beside it?** Recommended: beside it. `sandbox-v1` stays the macOS default: it uses the agent's own toolchains, needs no VM and no downloads, and is faster. `--grader container` opts in, for untrusted repositories, for the closed channels, or for comparability with Linux. Revisit with step 5's agreement and timings.
5. **The paid real check.** Recommended: an A/A experiment graded in containers, on 2 Go tasks and 1 Cargo task (bytes), 1 run per arm: 6 runs. Within the same cap, keep 2 trees with `run once --keep` and regrade them in the three modes for free. Expected about $1–2: the sandbox check's 4 Go runs cost $0.71, and Cargo runs cost more. The worst case under `--run-budget 0.60` is about $5. **Cap: $6.** It gets its own approval when step 4 lands.
6. **No matching image.** Recommended: refuse, naming the pinned versions and how a pin is added, rather than grade with another toolchain version.
7. **Failures at a resource limit.** Recommended: isolation decision 3's rule. A failed grade with an OOM kill or a hit on the process limit is left out (no retry, the per-arm check); a pass stays a pass.
8. **Default limits per grade.** Recommended: 4 GiB of memory, 4,096 processes, and min(4, the VM's) CPUs. Refuse when the VM has less memory than one grade's limit, and warn when slots × the limit exceed it. This machine's colima VM (2 GiB) would be refused until the user gives it more.
9. **Remote Docker.** Recommended: refuse a daemon that is not local, since hidden tests would leave the machine.

## Risks
- **Toolchains differ on a Mac.** The agent's macOS toolchain and the grade's Linux one differ, so a correct tree can fail in Linux (case-sensitive paths, OS-specific code). Step 5 measures it; macOS keeps `sandbox-v1` as its default until then.
- **Docker CLI and engine changes** (flags, inspect fields, OOM reporting). Tests pin the argv and inspect fixtures, and the usability check requires a minimum engine version.
- **A shared kernel in the VM.** An escape reaches the VM and colima's writable home mount. No host mounts, dropped capabilities, seccomp and a non-root user reduce the risk; some remains.
- **Time.** Container start, copy-in, a second deps warm-up per project and image, and cold caches add time. Step 0 measures each.
- **Disk.** Images (about 2.5 GB) and deps volumes grow inside the VM. `clean` handles volumes and containers, and removes images only on request.
- **Friction.** A consent and a pull come before the first container grade. Docker Hub limits anonymous pulls.
- **A small VM.** Several concurrent grades can exhaust the VM's memory beyond the containers' own limits; the usability check compares them.
- **Docker is down at recovery.** A dead run's container stays until its deadline, when `--rm` removes it, and its marker stays until Docker is back.
- **Parallel work.** Step 1 touches `internal/experiment`, a hotspot. It runs after, not beside, other work there, including Codex's.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
