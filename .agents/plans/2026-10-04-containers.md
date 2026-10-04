# Container mode: grading in Docker first, agents later

- Date: 2026-10-04
- Status: Approved (2026-10-04). Step 0 (the spike) is done with gaps ([results](../../docs/research/2026-10-04-container-spike.md)). The user decided the open decisions below: 1–9 as recommended, and 10 as a local build per pinned base (git and less added). Steps 1 (the mode seam) and 2 (the driver) are done; step 3 is next. The paid real check (cap $6) still needs separate approval. During the spike the Gradle warm-up also fetched from `download.eclipse.org` (Spotless's Eclipse formatter), which was not on the brief's list; it is recorded in the results.
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
- For step 0 the user started colima with 4 CPUs and 8 GiB: context `colima`, engine 27.4.0, linux/arm64, cgroup v2, seccomp and AppArmor on, 84 GB free on the VM's disk.

## Design

### What runs where
- Agents stay on the host, in Claude Code's sandbox, as today. Only the verification commands move into containers, for grading and for validation (open decision 1).
- Setup, warm-ups, restoring the check scripts and adding the hidden tests stay on the host, unchanged. The container receives the finished grading copy.

### Images
- **One image per toolchain**: official images, pinned by multi-arch index digest in Agentium's source. The table lists toolchain, version, digest and compressed size per architecture.
  - Go: `golang:<ver>`.
  - JDK: `eclipse-temurin:<jdk>-jdk`. Projects without `mvnw` use `maven:<ver>-eclipse-temurin-<jdk>`. Gradle runs through the project's wrapper, and its distribution is warmed with the deps.
  - Python with uv: Astral's `ghcr.io/astral-sh/uv:<ver>-python<minor>-trixie-slim`. Step 0 found no version-pinned bookworm tags for uv 0.11.28, and the unversioned bookworm tag stopped being rebuilt in February 2026 (uv 0.9.30). So the pin is trixie.
  - Rust: `rust:<ver>-slim`, pinned to a minor version. Step 0's `rust:slim` was 1.99, against the host's 1.95.
  - The JDK image is Ubuntu-based (26.04), not Debian.
  - Node with pnpm comes with the TypeScript work.
- **No image build for grading** (open decision 2). There is no Dockerfile and no published Agentium image.
  - **Step 0 showed that a grade needs more than the toolchain, `sh` and `tar`.** junit-pioneer's build runs `git describe` while it configures, and the JDK image has no git. click's pager tests run `less`, which the slim uv image lacks. That made one pilot task invalid in the container.
  - What images must hold beyond the toolchain is open decision 10.
- **Run by digest** (`golang@sha256:…`), so the daemon cannot substitute another image. After a pull, Agentium checks the image's `RepoDigests`.
- **Version match.** The image's toolchain matches the agent's on the host, as `pool.DetectToolchain` reads it: Go major.minor, the JDK major version, the Python minor version, the Rust minor version. When no pinned image matches, the mode is refused and the error names the pinned versions (open decision 6). The record keeps both toolchains, the host's and the image's.
- **Consent.** Agentium never pulls on its own.
  - `agentium images` lists the pinned images a project needs, whether each is present, and their sizes.
  - `agentium images pull` shows the total, asks (or takes `--yes`), then runs `docker pull` by digest.
  - `experiment new --grader container` and `task validate --grader container` refuse while an image is missing, naming the command and the size.
  - Changing a pin is a dependency change and needs approval ([supply chain](../rules/supply-chain.md)).
- **Sizes** (measured in step 0, arm64): compressed, Go 309 MB, the JDK 225 MB, Python with uv 67 MB and Rust (slim) 291 MB, 890 MB for all four. Unpacked in the VM: 2.44 GB. Pulls took 23–53 s each.

### Dependencies
- The host's deps folder cannot be used as it is. Python venvs and native pieces are macOS builds. Mounting host folders into the VM is slow (sshfs here) and reaches outside the container.
- Container deps go into a Docker volume per project and image digest (`agentium-deps-<data>-<project>-<digest12>`). Step 0 measured two sources:
  - **Seeded from the host's deps folder**, where the cache is platform-independent: Go's module downloads, Maven's repository and wrapper, Gradle's `modules-2` and wrapper distributions, and Cargo's registry. Agentium writes them as a tar, and the daemon unpacks it into the volume (`docker cp`) before a container starts. This took 0.04–1.8 s per fixture, and no network.
  - **Warmed inside a container** of the same image, with network, from the base commit only, where the seed falls short. This follows the trusted-repository rule that host warm-ups already follow.
    - Python always warms this way: the host's venvs are macOS builds, and its uv cache could not resolve offline.
    - Gradle warms this way when the host's warm-up failed. A Gradle 8.5 base took 52 s and 58 MiB.
- **Every folder in a seed's tar is listed.** `docker cp` creates missing parents as root, and the volume's own root stays root-owned. So Agentium makes the volume's top-level folders, owned by the grade's user, with the first tar.
- **Per-grade caches are seeded** from a seed in the volume that only trusted commands write (validation, warm-up): Go's build cache and the Gradle home, including the wrapper distribution and Gradle's other caches. Spotless's Eclipse formatter, for one, lives in `caches/p2-data`, not `modules-2`. This is the container counterpart of the sandbox's cloned seed. Copying a seed into the grade's `cache/` took 0.12–0.17 s for 124–404 MB. It cut the Go fixture's grade from 21 s to 0.3 s, and junit-pioneer's from 86 s to 13 s.
- The host deps folder's rules apply to this volume too: a warm-up lock (a host file), per-base stamps, last-use marks and cleanup.
- Grades mount the volume read-only. Each project, base and image needs one warm-up (time and network, measured in step 0).

### The grade's container
**Nothing from the host is mounted**: not the copy, the data folder, the home folder or the Docker socket. Each grade has one container, in this shape (settled by step 0):
```
docker create --name agentium-<data8>-<run>-grade --label agentium.data=<data8> --label agentium.run=<run>
  --label agentium.mode=container-v1 --rm --init --network none --ipc private --cap-drop ALL
  --security-opt no-new-privileges --read-only --user 65533:65533 --memory <m> --memory-swap <m>
  --pids-limit <p> --cpus <c> --shm-size 256m --log-driver none --tmpfs /tmp:rw,exec,nosuid,nodev,size=<t>
  --mount type=volume,dst=/grade --mount type=volume,src=<deps volume>,dst=/deps,readonly
  <image>@sha256:<digest> sleep <deadline>
docker cp - <name>:/grade < <work/ and cache/, owned by 65534, mode 0700>
```
- **The grade's user is 65534:65534 (`nobody`),** which every image's `/etc/passwd` lists. Every command of the grade, and the copy-in, runs as it (`docker exec --user 65534:65534`). The main process (the init and the deadline's `sleep`) and the counters' reads run as 65533, which the grade cannot signal (step 2's review: as 65534, the grade could stop the `sleep` past its deadline, or end its own container).
  - Java then reads `user.home=/nonexistent`, where nothing can be written, as in the agent's sandbox. All five step-0 toolchains accepted that.
  - A user ID missing from `/etc/passwd` gives Java `user.name=?` and `user.home=/`, so it is not used.
  - `HOME` is set to `/grade/cache/home`.
- **`/grade/work` and `/grade/cache` are on one anonymous volume.** The daemon makes both folders from a two-entry tar before the container starts, so no step runs as root and no capability is added.
  - The volume's root stays root-owned, which is why the two subfolders exist.
  - A tmpfs with `uid=`/`gid=` options also works. Its files count against the memory limit, though, and a Gradle grade keeps 0.4–0.7 GB there.
  - The volume has no size limit (overlay2 on ext4), so a grade can fill the VM's disk until its deadline (Risks).
  - `--rm` and `rm -v` remove the volume.
- **Inspect before start.** `docker inspect` of the created container is checked against what was asked, before it starts: network, IPC, capabilities, privileges, user, the read-only root, mounts and their modes, devices, limits and the image ID. Its normalized digest is recorded, as the sandbox's profile digest is.
- **Copy in.** Agentium writes the grading copy as a tar with `archive/tar`, never following a link, and caps its size. The image's own `tar` unpacks it into `/grade/work` (`docker exec -i`), as the grade's user, before any agent code runs.
  - A link the agent planted points nowhere inside the container.
  - The host copy is never changed by the grade, so `--keep` keeps it as it was graded.
  - `docker cp` is used only for trusted content (the skeleton and the seeds), so the daemon never resolves paths from a tree the agent wrote. It is refused anyway for a read-only root and for a tmpfs (`container rootfs is marked read-only`).
  - Step 0 timings: 0.05–0.19 s for 0.4–6.2 MB trees, and 0.53 s for 177 MB in 9,092 files. No bind fallback is needed.
- **Commands.** Each verification command runs as `docker exec --env-file <file> -w /work/<module> <name> sh -c <command>` through `runner`, in order, and stops at the first failure.
  - The environment is the agent's recipe (`buildtool.GraderEnv`) with paths inside the container (`/grade/work`, `/grade/cache`, `/deps`, `/tmp`), plus `GOPROXY=off` and `GOTOOLCHAIN=local`.
  - `docker exec` inherits the image's environment (`PATH`, `JAVA_HOME`, `CARGO_HOME`, `RUSTUP_HOME`), and `--env-file` overrides it. A `PATH` the recipe sets, such as Python's venv first, extends the image's `PATH`, which Agentium reads from `inspect`'s `Config.Env`.
  - Loopback inside the container works, `::1` included, so `httptest` and Gradle workers need no special rule. Gradle's Checkstyle workers connected without `GRADLE_DAEMON_BIND_ADDRESS`.
  - Output streams to the client with `--log-driver none`, and exit codes pass through.
- **Cleanup.** `docker rm -f -v <name>` runs after the grade, on an error, on cancel (with `context.WithoutCancel` and a time bound) and on a panic.
  - Killing the `docker` client does not stop processes in the container, so a timeout or cancel kills the container, not just the client.
- **Nothing outlives its command.**
  - The container's main process ends at the deadline: every command's timeout plus a margin. `--rm` then removes the container and its anonymous volumes, even after Agentium itself was killed. In step 0 an exec'd process was killed (exit 137) at the deadline, and the container and its volume were gone.
  - **A created container that never started is the gap.** Neither `--rm` nor the deadline applies to it, so it stays `Created`. If Agentium dies between `create` and `start`, only recovery or `clean` removes it, by label.
  - The container's name is written in the grade's folder before `docker create`. Recovery (`RecoverWarn`) and `agentium clean` remove the containers of dead runs by label.
  - Agentium starts no daemon, VM or background process. The user starts Docker.
- **A local daemon only** (a Unix socket). A remote `DOCKER_HOST` or an SSH context would send hidden tests off the machine, so the usability check refuses it (open decision 9).
- **The `docker` client's environment** is an allowlist: `PATH`, `HOME`, `DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG`, `DOCKER_CERT_PATH`, `DOCKER_TLS_VERIFY`, for the endpoint lookup only. Every later call sees only `PATH` and runs with an empty configuration (`--config`): the client copies its `config.json` proxies, credentials included, into every container it creates (step 2's review).

### Proof of isolation: the canary's counterpart
There is no seatbelt log. Instead, three checks fail closed before the grade's code runs:
1. **The daemon's record.** The created container's `docker inspect` matches what was asked (above).
2. **In-container probes**, as the grade's user:
   - `true` runs;
   - `/tmp`, `/grade/work` and `/grade/cache` are writable; `/deps` and `/` are not (EROFS);
   - `/sys/class/net` lists only `lo`, and a connect to a TEST-NET address fails at once;
   - `/proc/self/status` shows `NoNewPrivs: 1`, `Seccomp: 2` and an empty `CapEff`;
   - there is no Docker socket, and the user is not root.
3. **The usability check**, when a command starts. The daemon is reachable and local; it runs Linux with seccomp and cgroup v2; the images are present by digest; and `docker info` reports more memory and CPUs than the grade's limits.

A failure is infrastructure and is retried. It is never a fail, and the grade is never run in another mode instead.

### Resource limits: isolation decision 3's counterpart
- Resource limits are the only limits a container imposes that the agent's sandbox does not. After the commands, an end probe reads the container's cgroup counters (`memory.events` `oom_kill`, `pids.events` `max`, and `memory.peak` for the record). The grade cannot write these kernel counters. Step 0 confirmed this:
  - The container has its own cgroup namespace, and the files are readable but not writable.
  - An exec'd process killed by the OOM killer set `oom_kill 1`, and the container kept running. `docker inspect` then reports `State.OOMKilled=true`.
  - A process-limit hit set `pids.events max`.
  - A grade can trip a counter on purpose, to have a failure left out rather than counted. The per-arm check is the guard, as for `infra-sandbox` runs.
- A failed grade where either counter is above zero is left out, as `infra-sandbox` runs are: no retry, the per-arm check and the sensitivity line (open decision 7). A pass stays a pass.
- A failure from no network or from read-only deps counts as the agent's own, as in `sandbox-v1`: the agent's sandbox imposes both limits too.

### Side channels (isolation decisions 7 and 8)
| `sandbox-v1` leaves open | `container-v1` | Shown by (steps 2 and 5) |
|---|---|---|
| `/mp-` POSIX semaphores, kept until reboot | The container's own IPC namespace and `/dev/shm` (`--ipc private`), removed with it | A grade writes `/mp-` semaphores; the next grade and the host find none |
| The unified log (`logger`, `syslog`, paths in denial lines) | There is no `/dev/log` and no logd. Output goes only to Agentium's `verify.log`, and `--log-driver none` keeps nothing in the daemon | `logger` writes nowhere readable later; `docker logs` has nothing |
| Loopback means every address of the Mac (listening on the LAN, reaching local services) | `--network none` leaves only the container's own `lo`. Nothing outside can be reached or can reach in, and concurrent grades cannot reach each other | A server on `0.0.0.0` is unreachable from the host and the LAN; connects to the host gateway and to another grade fail |
| (no counterpart) | The kernel keyring is blocked by Docker's default seccomp profile | A `keyctl` probe fails |

Step 0 showed every row closed on a real daemon, with a positive control in the grade itself ([results](../../docs/research/2026-10-04-container-spike.md#side-channels)). `/mp-` semaphores, POSIX and SysV shared memory, and SysV message queues left in grade A were invisible to a concurrent grade, a later grade and the VM. There is no `/dev/log`, and kernel-log reads are denied. Listeners on `0.0.0.0`, `::` and abstract Unix sockets were refused from outside the grade, and every connect out was `Network is unreachable`. `add_key`, `keyctl` and `request_key` returned EPERM.

**The Docker socket is the new channel.** An agent that reaches the host's Docker socket (colima's `~/.colima/default/docker.sock`) can `docker exec` or `docker cp` into a concurrent grade and read its hidden tests, or start a privileged container. The agent's sandbox must keep Unix sockets denied. Step 4 shows, in a real agent run, that a `docker` command fails.

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

- [x] **0. Spike.** Free of charge. It needed the user to start colima with at least 4 CPUs and 8 GiB, and the user's consent to pull the Go, JDK, Python and Rust images (about 1 GB). No code. Risk: low.
  - *Fixtures:* the isolation plan's step-0 set: Go with `httptest`, Maven jackson-core, Gradle junit-pioneer with Checkstyle, Cargo bytes with `build.rs`, pytest click. They are graded by hand, in this shape.
  - *Acceptance:* each fixture matches its host result or names its break. Timings for start, copy-in, warm-up and tests are compared with host and sandbox grades. Image and volume sizes are measured. The questions listed under the results below are settled, and so are the side-channel probes.
  - *Packages:* none.
  - **Done with gaps (2026-10-04;** [results](../../docs/research/2026-10-04-container-spike.md)**).** No agent session ran, and nothing was spent.
    - *Pulled* (approved): `golang:1.27`, `eclipse-temurin:21-jdk`, `rust:slim` and `ghcr.io/astral-sh/uv:python3.12-bookworm-slim`. That is 890 MB compressed and 2.44 GB unpacked; the index digests are in the results.
    - *Dependencies fetched* by warm-up containers: 58 MiB for a Gradle 8.5 base, including Spotless's Eclipse formatter from `download.eclipse.org`, which the build fetches; and 1.8 MiB from PyPI for click's tests. Everything else came from the host's caches.
    - *Fixtures:*
      - Go (with cgo), Maven jackson-core and Cargo bytes with `build.rs` match their host results exactly: 2 passed; 1983 run, 2 skipped; 1305 passed.
      - Gradle junit-pioneer breaks at configuration, because the JDK image has no `git` (the build runs `git describe`). With a stub `git`, it matches: Checkstyle passed and 37 tests passed, without `GRADLE_DAEMON_BIND_ADDRESS`.
      - pytest click breaks on 24 `test_echo_via_pager` cases, because the slim image has no `less`. Otherwise it matches: 2159 passed against 2182 on the host, with 24 failed and one fewer skipped.
    - *Pilot tasks* (the base with the hidden tests must fail, and the reference must pass):
      - These grade correctly: one Go task (Agentium's), one Maven 3.x task and one Cargo task.
      - **Two Gradle tasks that the host cannot grade today now grade correctly on JDK 21:** one on Gradle 8.5 (after a network warm-up) and one on 8.14.2. The host's break is its default JDK 22.
      - The jackson 2.x tasks stay broken: their parent POM was purged upstream.
      - The click task is invalid in the container, because of `less`.
    - *Timings:* a grade's overhead is 0.3–0.5 s (create, skeleton and start, copy-in, removal). The tests ran as fast as on the host or faster. Cold per-grade caches cost 21 s (Go with cgo) and about 70 s (Gradle); seeded caches remove that.
    - *Settled:*
      - writable folders: one anonymous volume with daemon-made subfolders (a tmpfs counts against memory);
      - the user: 65534 `nobody`;
      - `lo` has `::1` under `--network none`;
      - `docker exec` output streams with `--log-driver none`, and `docker logs` keeps nothing;
      - copying in: `tar` through `docker exec -i` (`docker cp` is refused for a read-only root and for a tmpfs);
      - the cgroup counters are readable and not writable, and an exec'd process's OOM kill shows;
      - after a killed client, the exec'd process keeps running. `docker kill` and the deadline remove the container and its volume. A created, never-started container stays.
    - *Side channels:* `/mp-` semaphores and POSIX shm, SysV IPC, the system log, loopback and LAN listeners, and the kernel keyring are all closed. The design above records each result.
    - *Cleanup:* no `agentium-spike-*` container or volume remains. The images are kept.
    - *Gaps:*
      - the `rust:1.95-slim` and trixie uv images that the version match needs were not pulled;
      - no hostile stub agent was used (the probes ran as builds would);
      - the Docker socket is not yet shown closed to a real agent run (step 4);
      - Podman and Docker Desktop are not covered;
      - timings come from a shared, noisy machine.
- [x] **1. The mode seam** (no behavior change). The sites listed under The Go seam ask for the mode, not "not host". `task.GraderContainer` exists but is refused as unknown until step 4. Risk: medium. *Packages:* `task`, `run`, `experiment`, `report`, `cli`.
  - **Done (2026-10-04, branch `claude/refactor/grader-mode-seam`).** No behavior change for host and `sandbox-v1`; the contract golden and every other golden are unchanged.
    - *Renames:* `verifySandboxed` is `verifyIsolated` (it refuses any mode but the sandbox's); `CheckoutCommands.Sandboxed` is `Isolated`; `Env.sandboxed` is `gradesInSandbox`; `Validator.sandboxed` is `inSandbox`. Both now test `== GraderSandbox`, not `!= GraderHost`.
    - *`task.GraderContainer`* (`container-v1`) exists. `KnownGrader` and `ParseGrader` refuse it as unknown (the same error and exit code), so `experiment new`, `run once` and `task validate --grader container` stay usage errors; a lock or design that names it, and readiness, refuse it too. `run.unknownGrader` is the one refusal text.
    - *Sites the plan missed:* `run.CheckoutCommands` now refuses an unknown mode before any warm-up; `experiment.NeedsRevalidation` and `runservice.go`'s harmless-denials copy read "not host" as sandbox, and now name the sandbox; `experiment.Ineligible` refuses a mode not graded in (readiness); `task.DescribeGrader` names the container and calls any other mode unknown, where it called every non-host mode a sandbox; `Validator.verify` refuses an unknown mode itself. `lock.go` and `cli/runview.go` needed no change (they already refused or named the sandbox).
    - *Tests:* `TestContainerModeIsNotKnownYet` (task), `TestContainerModeIsRefusedLikeAnUnknownMode` and `TestOnceRefusesTheContainerMode` (run), `TestContainerModeIsRefused` and `TestSandboxCheckIsOnlyTheSandboxs` (experiment), `TestContainerGraderIsRefusedAsUnknown` (cli: the three commands and a resumed lock).
    - *Mutation checks* (3, in `git archive` copies): the run's `SandboxUsable` and `sandboxApplies` back to "not host is the sandbox", `Validator.inSandbox` back to `!= GraderHost`, and `Ineligible` accepting the mode. Each made its test fail.
    - *Limits:* a new design version for container designs, and the report and `run show` lines for the mode, wait for step 4.
- [x] **2. `internal/container`.** The driver, names and labels, the usability check, the inspect check, the probes, the tar stream, and the kill and removal on cancel. Unit tests use a fake `docker` (its argv pinned). Tests that need a real daemon skip without one. Risk: high (isolation, cancellation).
  - **Done (2026-10-04, branch `claude/feat/container-driver`).** Nothing is wired into grading; `container-v1` stays refused as unknown.
    - *Files:* `docker.go` (the client: `Open`, `CheckLocal`, `Fits`, `Image`, `Usable`, the environment allowlist), `spec.go` (limits, names, labels, the create argv), `inspect.go` (the inspect check and its digest), `probe.go` (the probes and the counters), `tarstream.go` (`WriteTar` and the skeleton), `container.go` (`Run`, `CopyIn`, `Exec`, `Counters`, removal, `Leftovers`, `RemoveRun`).
    - *Beyond the design above:*
      - `Open` reads the endpoint once (`docker context inspect`, with the user's own configuration), refuses anything but `unix://`, and pins it with `--host` on every later call. Those calls see only `PATH` and an empty configuration folder (`--config`; one Open makes, which `Close` removes, or an empty one the caller gives), so the user's `config.json` proxies never reach a container. Errors never name the endpoint, which holds a home path under colima.
      - The main process runs as 65533 (`MainUser`), the grade's commands and copy-in as 65534 (`User`), and the counters are read as 65533, so the grade can neither stop the deadline nor end its container or the counters' read.
      - The inspect check requires the container's environment to be the image's own exactly, and its runtime to be the daemon's default.
      - The daemon's API must be 1.41 (Docker 20.10) or newer. Images are found by digest reference or by image ID (step 3's local builds), never by tag, and `create` also runs with `--pull never`.
      - `--entrypoint sleep` replaces the image's entrypoint, and the check requires exactly `sleep <deadline>`.
      - The anonymous volume carries the labels too (`volume-label=`), so a volume orphaned without its container is found as well.
      - Commands take `--env NAME=value` arguments instead of an env file. A bare `NAME`, which docker would fill from the client's own environment, is refused.
      - A command's result counts only when the counters can be read after it. Once the grade's input is used, a result that cannot be judged is `ErrUnjudgeable`: the counters unreadable (a fork bomb left running), the container ended mid-command (the deadline), the client failed, or the copy-in refused or failed on the tree (`ErrTooLarge`, a file that changed or cannot be read, `tar` failing). Agentium's own cancel, and every error before the grade's input is used (`Open`, `Usable`, `ErrMismatch`, `ErrProbe`, create and start), stay plain errors. A timeout reads the counters and then removes the container; any later call is `ErrGone`.
      - A deps volume must already exist and be a plain local volume: the local driver's options can bind a host folder.
      - A `create` that the daemon refused (its name in use) never leads to a removal, since that container is not the grade's. A cancelled or timed-out create is removed. If the client exits non-zero after the daemon really made the container, it is left for recovery to remove by label.
      - The probes run as the grade's user, since they prove its view (its user, what it can write); no code of the grade has run by then.
      - The probes read the namespace inodes against the kernel's initial ones, and the routing table, instead of trying a TEST-NET connect, which would need a tool the image may lack. The real-daemon tests do try those connects.
      - A copy-in's limits are a parameter (`CopyLimits`): by default 2 GiB of content, 1,000,000 entries and 10 minutes.
    - *Unit tests (fake `docker`: the test binary linked as `docker`):*
      - `testdata/argv.golden` pins all 21 calls: the usability check, create, inspect, the skeleton, start, the probes, the copy-in, a command, the counters, the removal, and recovery's listing and removal.
      - The inspect check refuses 71 mismatches (and 5 unreadable records), each by name: network, IPC, PID, UTS, user and cgroup namespaces, privileges, capabilities, security options, a writable root, masked paths, binds (the Docker socket among them), devices, ports, sysctls, `/tmp`, limits, OOM-kill, `--rm`, `--init`, logs, the main user (the grade's own refused), the environment (proxy variables added, a variable added or changed, none), the runtime, entrypoint, deadline, labels, image, name, and the mounts. The probes refuse 33 failures.
      - Each way to make a result unjudgeable is `ErrUnjudgeable` and removes the container: a fork bomb, the counters killed or cut short, the container ended mid-command, a timeout with unreadable counters, and a copy-in with too much content, too many entries or a failing `tar`. A missing root is not, nor are an inspect mismatch, a failed probe or a cancel.
      - A cancelled create removes the container, and a refused one does not. The endpoint lookup's failure hides the socket path, and a cancelled `Open` keeps `context.Canceled` in its chain.
      - Remote endpoints (tcp, ssh, npipe, fd, http, a relative or empty socket) are refused after one call. Unusable daemons are refused, and so are a missing image or one of another digest (with no pull) and unpinned references.
      - The environment allowlist holds for the lookup, and every later call has `--config <empty>`, `--host` and only `PATH`. `Open` refuses a configuration folder that holds a `config.json`. `Run` removes the container after an error, a panic, an inspect mismatch, a failed probe and a failed inspect. A cancel removes it inside `Exec`, and a timeout reads the counters, then removes it.
      - The tar never follows a link, drops setuid, skips pipes, refuses a swapped link or pipe without hanging, and honors the limit.
      - The inspect and probe fixtures are the daemon's real output, normalized (`TestRealFixtures -update` rewrites them).
    - *Real-daemon tests* (colima, engine 27.4.0; `golang:1.27` by digest; they skip without a local daemon or the image, and never pull):
      - the shape: a Go test with an `httptest` server passes, offline, as user 65534; a planted link to a host file points nowhere; the host copy is unchanged;
      - an OOM kill and a process-limit hit show in the counters (`{OOMKills:1 PidsMax:1}`);
      - the side channels: grade A leaves a `/mp-` semaphore, POSIX shm, SysV shm and a message queue, and a `0.0.0.0` listener, and sees them itself. Grade B, beside A, and grade C, after it, see none of them. Their connects to A's port on `127.0.0.1` and `::1` are refused; connects to TEST-NET, the colima host, the slirp gateway and `1.1.1.1` are unreachable; DNS fails; there is no `/dev/log`, and `logger` fails;
      - nothing is left after a cancel, a timeout, the deadline (`--rm`), or a container created and never started (found by label, then removed);
      - a user configuration with a proxy password never reaches the grade: its environment is the image's own. A positive control, a create with that configuration, does carry the password;
      - as 65534, `kill -STOP`, `-TERM` and `-KILL` on the deadline's `sleep` (running as 65533) and on pid 1 are all denied, the `sleep` keeps sleeping, and the deadline (8 s) still ends the next command, as `ErrUnjudgeable`, and removes the container;
      - a Python fork bomb left running after a failing command (process limit 64) makes the result `ErrUnjudgeable`;
      - a loop left running that kills every process it can does not stop the counters' read (as 65533): the failing command is judged, with its counters.
      - Every test checks, by name and by label, that no container or volume is left. None was.
    - *Mutation checks* (5, in `git archive` copies), each caught:
      - `--network none` dropped: the golden, and the inspect check on the real daemon;
      - a bind mount added: the same two;
      - the inspect check never refusing: the mismatch tests;
      - `Exec` not removing on cancel: the fake and the real cancel tests;
      - `CheckLocal` accepting everything: the remote-endpoint tests.
    - *Review (PR #155) fixes, with 4 more mutation checks, each caught:*
      - `--config` dropped: `TestClientEnvironment` (and the golden). The real proxy test does not catch it, since its proxies come through `DOCKER_CONFIG`, which the `PATH`-only environment already drops; without `--config`, the user's own `~/.docker/config.json` would be read, which a test cannot plant;
      - `MainUser` set to the grade's user: the real deadline test (`kill -KILL 1` ended the container) and the real killer test;
      - `Exec` returning a plain error when the counters cannot be read: the fake unjudgeable cases and the real fork-bomb test;
      - `mayExist` set only after a successful create: the cancelled-create case.
    - *Re-review fixes:*
      - a copy-in's client error or timeout once the tree streams is `ErrUnjudgeable` (`CopyLimits.Timeout`, 10 minutes by default);
      - the probes and the counters run with `--workdir /`;
      - a probe shows that the grade cannot signal pid 1;
      - the `Config.Env` mismatch names variables, never their values;
      - a real backstop test: proxies written into `Open`'s accepted empty folder make `Run` fail with `ErrMismatch` on `Config.Env`.

      Two mutation checks were caught: the copy-in timeout returning a plain error (the fake test), and `--config` dropped (the real backstop test).
    - *Verification:* `GOPROXY=off go test -race -count=1 ./internal/container` passed with the real-daemon tests; `harness.py check changed` passed.
    - *Limits:*
      - Background processes that a command leaves behind keep running into the next command. A fork bomb left running makes the counters unreadable, which is `ErrUnjudgeable` (left out under step 4's rule). Step 4 decides whether to sweep them between commands.
      - The grade's code can still make its own result unjudgeable on purpose (a fork bomb, filling memory until the OOM killer picks the main process, a tree that fails the copy-in). That is why step 4 leaves such results out and counts them per arm, never retrying them.
      - A command's environment is in docker's argv, which local processes can see, so it must never hold a credential.
      - A Unix socket forwarded to another machine cannot be told from a local one.
      - Podman, Docker Desktop and rootless Docker are untested.
      - CI has Docker but not the image, so the real-daemon tests skip there. A CI job that pulls the pinned image needs the user's approval.
- [ ] **3. Images and container deps.** The pin table and the version match. `agentium images`, with consent and sizes. Deps volumes warmed in containers, Python's venv included. Recovery and `clean` for containers and volumes; pulled images are only listed, and removed only with a flag. Risk: high (downloads, consent, supply chain, cleanup).
- [ ] **4. Wiring and records.** `gradeInContainer` and validation's hook for containers; `--grader container`. Records, validations, designs and locks; the cgroup counters feed isolation decision 3's rule (open decision 7). Report and `run show` lines; the Linux default. Risk: high (hidden tests, persistence, concurrent runs).
  - **No re-rolls (decision 3; from step 2's review).** Any failure after the grade's input was used settles as left out, exactly as `Counters.Hit()` failures and `infra-sandbox` runs do: no retry, counted in the per-arm check and the sensitivity line. That is every `container.ErrUnjudgeable`, from `Exec` (counters unreadable, the container ended mid-command, the client failed) and from `CopyIn` (`ErrTooLarge`, a tree that changed or cannot be read, `tar` failing). Only errors before the grade's input is used (the usability check, `ErrMismatch`, `ErrProbe`, create and start) are infrastructure that may be retried. A pass stays a pass.
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
Decided by the user on 2026-10-04: 1–9 as recommended ("Yes, as recommended"); 10 as a local build from each pinned official base, adding `git` and `less` through apt, with the download shown and consented once per base ("Local build per base"). That changes 2: grading images are now built locally from the pinned bases, and no Agentium image is published.

1. **Grading only first, or agents too?** Recommended: grading first. It closes the open channels and Linux's gap without putting credentials into containers. Agents follow in their own plan after Codex's, which designs the proxy and the credentials once, for both agents.
2. **Where do the images come from?** Recommended: official images pinned by digest and pulled with consent, with no build for grading. Python with uv uses Astral's image, which is the vendor's own, not a Docker Official Image. A local build from pinned bases comes only when agents move into containers. No published Agentium image: it would make Agentium a registry to secure and sign. Downloads were 890 MB for all four toolchains in step 0, or 0.3 GB for Go alone. Step 0 found that the images lack tools that builds and tests run (decision 10).
3. **The default on Linux and macOS.** Recommended: `container-v1` on Linux. Without a usable Docker, `experiment new` refuses and names `--grader host`, which is today's behavior made explicit. macOS keeps `sandbox-v1`. Existing experiments keep their mode. The alternative keeps `host` as Linux's default and prints a warning.
4. **Replace the macOS sandbox, or sit beside it?** Recommended: beside it. `sandbox-v1` stays the macOS default: it uses the agent's own toolchains, needs no VM and no downloads, and is faster. `--grader container` opts in, for untrusted repositories, for the closed channels, or for comparability with Linux. Revisit with step 5's agreement and timings.
5. **The paid real check.** Recommended: an A/A experiment graded in containers, on 2 Go tasks and 1 Cargo task (bytes), 1 run per arm: 6 runs. Within the same cap, keep 2 trees with `run once --keep` and regrade them in the three modes for free. Expected about $1–2: the sandbox check's 4 Go runs cost $0.71, and Cargo runs cost more. The worst case under `--run-budget 0.60` is about $5. **Cap: $6.** It gets its own approval when step 4 lands.
6. **No matching image.** Recommended: refuse, naming the pinned versions and how a pin is added, rather than grade with another toolchain version.
7. **Failures at a resource limit.** Recommended: isolation decision 3's rule. A failed grade with an OOM kill or a hit on the process limit is left out (no retry, the per-arm check); a pass stays a pass.
8. **Default limits per grade.** Recommended: 4 GiB of memory, 4,096 processes, and min(4, the VM's) CPUs. Refuse when the VM has less memory than one grade's limit, and warn when slots × the limit exceed it. This machine's colima VM (2 GiB) would be refused until the user gives it more. Step 0's peaks fit: Gradle 1.8–2.6 GiB, Maven 1.6 GiB, Go up to 0.8 GiB, Cargo 0.5 GiB and pytest 0.14 GiB.
9. **Remote Docker.** Recommended: refuse a daemon that is not local, since hidden tests would leave the machine.
10. **What the images hold beyond the toolchain** (from step 0). junit-pioneer's build runs `git` and click's tests run `less`. The JDK and slim uv images have neither, while the host has both. Choices:
    - (a) A local image built from each pinned base, adding a short, fixed package list (`git` and `less` so far), and tagged with its base and list digests. The build needs network, apt and the user's consent once per base. This is the build that decision 2 put off until agents move into containers.
    - (b) Fuller official images: `golang` is already built on `buildpack-deps` and holds git. JDK and Python images with git were not checked or pulled.
    - (c) Plain images, with the breaks recorded as disagreement between the modes (step 5).

    Recommended: (a), if the user accepts one apt build per base. Of the three, only (a) is known to grade both of step 0's broken fixtures as the host does. Otherwise (c), with the break named in validation.

## Risks
- **Toolchains differ on a Mac.** The agent's macOS toolchain and the grade's Linux one differ, so a correct tree can fail in Linux (case-sensitive paths, OS-specific code). Step 5 measures it; macOS keeps `sandbox-v1` as its default until then.
- **Docker CLI and engine changes** (flags, inspect fields, OOM reporting). Tests pin the argv and inspect fixtures, and the usability check requires a minimum engine version.
- **A shared kernel in the VM.** An escape reaches the VM and colima's writable home mount. No host mounts, dropped capabilities, seccomp and a non-root user reduce the risk; some remains.
- **Time.** Container start, copy-in, a second deps warm-up per project and image, and cold caches add time. Step 0 measures each.
- **Disk.** Images (2.44 GB for four) and deps volumes (0.1–1.4 GB per project in step 0) grow inside the VM. `clean` handles volumes and containers, and removes images only on request. A grade's anonymous volume has no size limit, so a hostile grade can fill the VM's disk until its deadline. A full disk fails other grades as infrastructure.
- **The Docker socket.** An agent that reaches it can read concurrent grades' hidden tests (Side channels). The agent's sandbox must deny it, and step 4 shows that it does.
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
