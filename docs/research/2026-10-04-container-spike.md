# Container grading: spike (container plan, step 0)

- Date: 2026-10-04
- Scope: [container plan](../../.agents/plans/2026-10-04-containers.md) step 0. Hand-run, with throwaway bash and Python scripts in a scratch folder; no product code, no agent session, nothing paid.
- Machine: Apple M4 Pro, macOS 15.7 (Darwin 24.6). colima 0.8.1 (`vz`), started by the user with 4 CPUs and 8 GiB; Docker context `colima`, engine 27.4.0, linux/arm64, cgroup v2, seccomp and AppArmor on. Other agents shared the machine, so timings are noisy.
- Console output below is real and trimmed. Home paths are shown as `~`, and the scratch folder as `<scratch>`.

## What was done

- Pulled the four approved images (below).
- Made one Docker volume of dependencies per fixture (`agentium-spike-deps-*`). Each was seeded from the host's platform-independent caches by `docker cp` of a tar, before its container started. Where a fixture needed more, a warm-up container with network fetched it from the fixture's base commit.
- Graded each fixture in the plan's container shape:

  ```
  docker create --rm --init --network none --ipc private --cap-drop ALL --security-opt no-new-privileges --read-only
    --user 65534:65534 --memory 4g --memory-swap 4g --pids-limit 4096 --cpus 4 --shm-size 256m --log-driver none
    --tmpfs /tmp:rw,exec,nosuid,nodev,size=1g --mount type=volume,dst=/v --mount type=volume,src=<deps>,dst=/deps,readonly
    <image> sleep 3600
  docker cp - <name>:/v < skeleton.tar        # work/ and cache/, owned by 65534, before start
  docker start <name>
  docker exec -i -w /v/work <name> tar -x < tree.tar
  docker exec --env-file <file> -w /v/work <name> sh -c '<command>'   # in order, stop at the first failure
  docker exec <name> sh -c '<end probe: memory.events, pids.events, memory.peak>'
  docker rm -f -v <name>
  ```

- Ran the same fixtures on the host for comparison, against copies of the same caches in the scratch folder.
- Nothing was written to the user's repositories or to the pilot data folders. Trees came from `git archive`, and caches were read and tarred. No home folder, data folder or Docker socket was mounted into any container.

## Images

| Image (tag pulled) | Multi-arch index digest | Inside | arm64 compressed | Unpacked | Pull |
|---|---|---|---|---|---|
| `golang:1.27` | `sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190` | Go 1.27.1, Debian 13 (trixie), gcc, git | 308.6 MB | 900 MB | 53 s |
| `eclipse-temurin:21-jdk` | `sha256:3e3c176ffed168beb42c607be9bc1639b466cf00261a0fb04425562c9d0c5c2b` | Temurin 21.0.12.1, **Ubuntu 26.04**, no git | 224.5 MB | 509 MB | 40 s |
| `rust:slim` | `sha256:70d3b1a5e21806b8615c3fb2a59abea6e931aa29bf93f0a9d9c46e743beae096` | Rust 1.99.0, Debian 13 | 290.7 MB | 839 MB | 46 s |
| `ghcr.io/astral-sh/uv:python3.12-bookworm-slim` | `sha256:e5b65587bce7de595f299855d7385fe7fca39b8a74baa261ba1b7147afa78e58` | Python 3.12.12, **uv 0.9.30**, Debian 12, built 2026-02-04, no `less` | 66.5 MB | 195 MB | 23 s |
| Total | | | **890 MB** | **2.44 GB** | |

- Each digest is the OCI image index (`docker manifest inspect <repo>@<digest>` returns `application/vnd.oci.image.index.v1+json` with a linux/arm64 entry). Compressed sizes are the arm64 layers' sum from `docker manifest inspect -v`. Unpacked sizes come from `docker image inspect`.
- `rust:slim` was chosen over `rust:latest` (556.5 MB compressed) because the plan names the slim variant. Both are the latest stable.
- **Versions against the host's:**
  - Go 1.27.1 matches exactly.
  - The JDK is 21 against the host's default 22 (Corretto 22.0.2; Corretto 21.0.6 is also installed).
  - Rust is **1.99** against the host's **1.95**. The plan's version match would refuse it; `rust:1.95-slim` was not pulled.
  - The uv image runs Python 3.12.12 (host 3.12.13) and **uv 0.9.30 (host 0.11.28)**. Astral publishes no version-pinned bookworm tags for uv 0.11.28 (`0.11.28-python3.12-bookworm-slim`: "manifest unknown"), only trixie ones (`0.11.28-python3.12-trixie-slim`, 69.2 MB). The unversioned bookworm tag stopped being rebuilt in February.
- Every JVM start under this VM prints `OpenJDK 64-Bit Server VM warning: Unable to get SVE vector length on this system. Disabling SVE.` on stderr. No test was affected.

## Fixtures

The isolation spike's set, at the same commits, plus one real pilot task per toolchain: the base with the hidden tests (must fail) and the reference (must pass).

| Fixture | Host (isolation spike, 2026-10-02) | Host today | Container | Break and cause |
|---|---|---|---|---|
| Go: scratch module with an `httptest` server and a cgo module (`mattn/go-sqlite3`) | 2 passed (its dependency differed) | 2 passed | 2 passed | none. cgo builds with the image's gcc |
| Go: Agentium task `deny-login-file` | valid | base fails `TestReq5HiddenPathsAndCredentialsAreDenied`; reference passes | the same test fails; reference passes | none |
| Maven: jackson-core `cacf488`, `./mvnw -B test` | 1983 run, 0 failed, 2 skipped | the same, on JDK 22 and on JDK 21 | the same | none. The `3.3.0-SNAPSHOT` parent resolved offline from the seeded repository |
| Maven: pilot task `release-resources-in-utf8writer-close` (3.x) | valid | not run | base: 2 failures in `UTF8WriterTest`; reference passes | none |
| Maven: pilot tasks on the 2.x line | invalid on the host: the `2.21.0-SNAPSHOT` parent was purged upstream | not run | not graded | The same upstream purge. These bases also use the jar-based wrapper, which downloads its jar into the checkout on first use (`wget` failed under `--network none`). A container cannot restore a purged artifact |
| Gradle: junit-pioneer `b8b747c`, Checkstyle, then 37 tests | Checkstyle passed; 37 tests passed | the same (JDK 22, Gradle 9.7.1) | **fails at configuration**: `Cannot run program "git"`. With a stub `git` on `PATH` (exit 128): Checkstyle passed and 37 tests passed | The build script runs `git describe` while it configures, and the JDK image has no git. Gradle's workers connected over the container's own loopback **without** `GRADLE_DAEMON_BIND_ADDRESS` |
| Gradle: pilot task `avoid-skipping-the-last-error` (Gradle **8.5**) | invalid: JDK 22 (`Unsupported class file major version 66`) | not run | **valid on JDK 21**: the base fails `RetryingTestExtensionTests`, and the reference passes. This took a network warm-up of its base (58 MiB) and Spotless's Eclipse formatter cache (`caches/p2-data`) in the grade's Gradle home | Host drift only. The JDK 21 image fixes it |
| Gradle: pilot task `add-reproducible-disableduntil-dates` (Gradle 8.14.2) | invalid on the host in sandbox mode (a socket bind refused) | not run | **valid**: the base fails `compileTestJava`; the reference passes 684 tests (1 skipped) | none in the container |
| Cargo: bytes `7930d93` with an added `build.rs` (writes into `OUT_DIR`) | 1305 passed | 1305 passed | 1305 passed (Rust 1.99) | none. Cargo ran with a read-only `CARGO_HOME` and printed no warnings |
| Cargo: pilot task `fix-sign-extend-n-byte-integers` | valid | not run | base: 1 failed (`try_get_int_sign_extends`); reference: 1305 passed | none |
| pytest: click `06b2a67`, `python -m pytest tests --continue-on-collection-errors` | 2182 passed, 25 skipped, 1 xfailed, 1 collection error | the same | 2159 passed, **24 failed**, 24 skipped, 1 xfailed, 1 collection error | The 24 are `test_echo_via_pager`'s `less` cases: the slim image has no `less`. The collection error is the known missing-metadata one (the recipe now adds the metadata) |
| pytest: pilot task `list-every-argument-in-the-positional` | valid | not run | **invalid**: the reference fails the same 24 `less` cases (the base fails 27) | The image lacks a tool that the tests run. The task's `--deselect tests/test_utils.py::test_echo_via_pager` names a path that moved, so the pager tests run |

```
[pt5-ref] create 0.04s  skeleton+start 0.11s  copy-in 0.06s (1.5M)
[pt5-ref] exit=0 24.92s  ./gradlew --console=plain test
[end probe] memory.events oom_kill=0 pids.events max=0 memory.peak=2439 MiB; /v: 733M
[pt5-ref] rm -f -v 0.17s  total 25.66s  rc=0
SUCCESS: Executed 684 tests in 5.6s (1 skipped)
BUILD SUCCESSFUL in 24s

[pioneer] exit=1 12.39s  ./gradlew --console=plain checkstyleMain checkstyleTest
* What went wrong:
Cannot run program "git": Exec failed, error: 2 (No such file or directory)

$ python -m pytest tests --continue-on-collection-errors -q      (click, in the container)
FAILED tests/test_utils/test_echo_via_pager.py::test_echo_via_pager[test9- less ]
ERROR tests/test_deprecations.py
24 failed, 2159 passed, 24 skipped, 31000 deselected, 1 xfailed, 1 error in 2.39s
```

## Dependencies

| Fixture | Seed from host caches (`docker cp`) | Container warm-up | Volume |
|---|---|---|---|
| Go | the module's four download files from the host's module cache (`~/go/pkg/mod/cache/download`; the pilot data folders hold no Go module cache): 3 MB, 0.04 s | `go mod download` with `GOPROXY=off` extracted it: 0.11 s. A build-cache seed (the base's test build into `/deps/go-build-seed`): 21 s, 124 MB | 135 MB |
| Maven | the pilot's `m2` and `mvnw-home`: 96 MB, 0.36 s | none needed | 99 MB |
| Gradle | the pilot's `gradle-ro/modules-2` and the 9.7.1, 8.5 and 8.14.2 wrapper distributions: 545 MB, 1.8 s | A Gradle-home seed for `b8b747c`: 17.9 s, 404 MB. **A network warm-up** of the Gradle 8.5 base: 52 s, 58 MiB received | 1.42 GB |
| Cargo | the pilot's `cargo` home (registry): 297 MB, 1.24 s | none needed | 307 MB |
| Python | the Python pilot's `uv-cache`: 169 MB, 0.71 s; **unusable** | **A network warm-up** of click's locked `tests` group (`uv export --frozen --only-group tests`; pytest 9.0.2, pluggy, iniconfig, packaging, pygments) into `/deps/py/venv`: 1.1 s, 1.8 MiB received, venv 7 MB | 177 MB |

- The seeds that work are the platform-independent ones: Go's module downloads, Maven's repository, Gradle's `modules-2` and wrapper distributions, and Cargo's registry.
- The Python pilot's uv cache could not install pytest offline, from the host's uv 0.11.28 or the image's 0.9.30. It holds wheels but not the index data an offline resolve needs. Its venvs are macOS builds. So Python warms in a container, as the plan says.
- **A `docker cp` tar must name every parent folder.** Missing parents are created as root, and the grade's user then cannot write there (`go: open /deps/gomod/cache/download/...lock: permission denied`). The volume's own root stays root-owned even when the tar's `./` entry is owned by 65534. So a deps volume's top-level folders are created by the daemon (`docker cp` of a skeleton) before any warm-up.
- **Seeded per-grade caches** are the container counterpart of the sandbox's cloned seed. `cp -a` from a seed in the deps volume into the grade's `cache/` took 0.12 s for Go's 124 MB and 0.17 s for Gradle's 404 MB. It cut the Go grade from 21 s to 0.3 s, and the junit-pioneer grade from 86 s to 13 s.
- **Gradle keeps more than `modules-2`.** Spotless's Eclipse formatter (`com.diffplug.spotless` 6.25.0 with `eclipse()`) is cached in `<GRADLE_USER_HOME>/caches/p2-data`, not in `modules-2`. A grade whose Gradle home lacks it fails at configuration (`UnknownHostException: download.eclipse.org`). Container grades need it seeded, as the host's grading seed carries it.

## Questions settled

1. **Writable `/work` and `/cache` for a non-root user with every capability dropped.** Two ways work; neither needs a step run as root or a capability.
   - A tmpfs with owner options (`--tmpfs /work:rw,exec,nosuid,nodev,uid=65534,gid=65534,mode=0700`). Its files **count against the container's memory limit**: a 300 MB file showed as `shmem 300003328` in `memory.stat`. A Gradle grade holds about 0.4–0.7 GB in its cache and work folders, besides its JVMs' 2–2.6 GiB peak.
   - **An anonymous volume** (`--mount type=volume,dst=/v`), with its `work/` and `cache/` folders made by `docker cp` of a two-entry tar (owned by 65534, mode 0700) before the container starts. It is on disk, and `--rm` or `rm -v` removes it. The volume's root itself stays `root 755` (`touch /v/x: Permission denied`). Volumes have no size limit here (overlay2 on ext4), so a grade can fill the VM's disk until its deadline.
   - **Chosen: the anonymous volume.** `/tmp` stays a sized tmpfs.

   ```
   drwxr-xr-x 4 root   root    4096 /v
   drwx------ 2 nobody nogroup 4096 /v/cache
   drwxr-xr-x 7 nobody nogroup 4096 /v/work
   writable
   touch: cannot touch '/v/x': Permission denied
   ```
2. **A user the toolchains accept: 65534:65534 (`nobody`),** which every image's `/etc/passwd` lists.
   - Java then reads `user.name=nobody` and `user.home=/nonexistent`. No folder exists there, so nothing can be written to the home folder, as in the agent's sandbox.
   - All five toolchains accepted it, with `HOME` set to a folder under `cache/` and each tool's own variables (`GRADLE_USER_HOME`, `MAVEN_USER_HOME`, `CARGO_HOME`, `GOCACHE`, `UV_CACHE_DIR`).
   - A user ID missing from `/etc/passwd` (10001) gives `user.name=?` and `user.home=/`, so it is not used.
   - `-Duser.home` through `JAVA_TOOL_OPTIONS` works, but no fixture needed it.
3. **`lo` has IPv6 under `--network none`.** `/proc/net/if_inet6` lists `::1` on `lo`, and `disable_ipv6` is 0. A JVM binds and connects on `::1`, `127.0.0.1` and `localhost`. Gradle's Checkstyle workers connected without `GRADLE_DAEMON_BIND_ADDRESS`. A connect to `192.0.2.1` fails at once: `Network is unreachable`.
4. **`docker exec` output with `--log-driver none`.** stdout and stderr stream to the client as usual, exit codes pass through (`exit 3` gives 3), and stdin works for `exec -i`. Nothing is kept: `docker logs` fails with `configured logging driver does not support reading`, and `LogPath` is empty.
5. **Copying in: `docker cp` against a tar through `docker exec -i`, with a read-only root.**
   - `docker cp` is refused for the root and for a tmpfs: `Error response from daemon: container rootfs is marked read-only`.
   - It works into a volume, even before the container starts, and it keeps the tar's owners.
   - `tar -x` run through `docker exec -i` as the grade's user works into a tmpfs or a volume. jackson-core (6.2 MB, 575 entries) took 0.05–0.13 s. A 177 MB tree of 9,092 files took 0.53 s, against 0.73 s by `docker cp`.
   - **Chosen:** `docker cp` only for trusted content before start (the skeleton and the seeds). The grading copy goes in through `tar` run as the grade's user, so the daemon never resolves paths from a tree the agent wrote.
   - The plan's fallback, a read-only bind, is not needed.
6. **The cgroup counters are readable inside, and an OOM of an exec'd process shows there.**
   - The container has its own cgroup namespace (`/proc/self/cgroup` is `0::/`). `memory.events`, `pids.events`, `memory.peak` and `memory.stat` are readable. Writes fail with `Read-only file system`.
   - An exec'd `tail /dev/zero` under a 512 MiB limit was killed: the exec exited 137 and `memory.events` went to `oom 1`, `oom_kill 1`. The container kept running, and `docker inspect` then reports `State.OOMKilled=true`.
   - 100 background `sleep`s under `--pids-limit 64` gave `sh: 0: Cannot fork` and `pids.events max 1`.
7. **What stays when.**

   | Event | Container | Exec'd process | Anonymous volume |
   |---|---|---|---|
   | The `docker exec` client killed (SIGKILL) | keeps running | **keeps running** (`sleep 99` still listed) | stays |
   | `docker kill` | removed (`--rm`) | killed | removed |
   | The deadline (the main `sleep` ends) | removed | killed: the exec returned 137 after 5.99 s of a 6 s deadline | removed |
   | `docker rm -f -v` of a created container | removed | none | removed |
   | Created and never started | **stays `Created`**: neither `--rm` nor the deadline applies | none | stays |

## Side channels

Grade A, run as a hostile build would, tried to leave a marker. Grade B ran beside it, and grade C after A was removed. A probe container with `--ipc host --network host` gave the VM's view. A positive control, run in A itself, found every marker it had left: `/dev/shm` held `sem.mp-SELFCHECK` and `SELFCHECK`, and SysV `shm` and `msg` had key `0x5eed`.

```
sem_open /mp-<marker>                        OK (left)
shm file /dev/shm/<marker>                   OK (left)
SysV shmget key 0x5eed                       OK (left) id=0
SysV msgget+msgsnd key 0x5eed                OK (left) id=0
/dev/log exists                              refused
send to /dev/log                             refused [Errno 2] No such file or directory
w /dev/kmsg                                  refused [Errno 13] Permission denied: '/dev/kmsg'
r /proc/kmsg                                 refused [Errno 13] Permission denied: '/proc/kmsg'
klogctl read (dmesg)                         refused errno=EPERM
add_key to the user keyring                  refused errno=EPERM
keyctl GET_KEYRING_ID                        refused errno=EPERM
interfaces: ['lo']
listen ('0.0.0.0', 47123)                    OK (left) (inside this container's own netns)
listen ('::', 47124)                         OK (left) (inside this container's own netns)
abstract unix socket @agentium-hidden        OK (left) (netns-scoped)
connect 192.168.5.2:80                       refused [Errno 101] Network is unreachable
connect 1.1.1.1:80                           refused [Errno 101] Network is unreachable
DNS example.com                              refused [Errno -3] Temporary failure in name resolution
write /deps                                  refused [Errno 30] Read-only file system
write /                                      refused [Errno 30] Read-only file system
docker socket present: False
[B, concurrent] /dev/shm entries: []  marker found: False
[B, concurrent] sem_open /mp-<marker> (no create): absent, errno=ENOENT
[B, concurrent] /proc/sysvipc/shm: 0 entries; key 0x5eed: False
[B, concurrent] connect ('127.0.0.1', 47123): [Errno 111] Connection refused
[B, concurrent] abstract socket: [Errno 111] Connection refused
[B, concurrent] request_key user:agentium-hidden: EPERM
[VM, concurrent] /dev/shm entries: []  marker found: False
[VM, concurrent] connect ('127.0.0.1', 47123): [Errno 111] Connection refused
[C, later] /dev/shm entries: []  marker found: False
[C, later] /proc/sysvipc/msg: 0 entries; key 0x5eed: False
```

| Channel | Result |
|---|---|
| `/mp-` POSIX semaphores and POSIX shm | **Closed.** They live in the container's own `/dev/shm`. A concurrent grade, a later grade and the VM saw none |
| SysV shm and message queues | **Closed.** They live in the container's IPC namespace (`--ipc private`) and are gone with it |
| The system log | **Closed.** There is no `/dev/log`, and `logger` silently drops the message (with `--socket-errors=on`: `socket /dev/log: No such file or directory`). `/dev/kmsg` and `/proc/kmsg` are denied, and `klogctl` returns EPERM. `--log-driver none` keeps nothing in the daemon |
| Loopback and LAN listeners | **Closed.** Listeners on `0.0.0.0`, `::` and an abstract Unix socket exist only in the grade's own network namespace. A concurrent grade and the VM's host network got `Connection refused`. Every outbound connect is `Network is unreachable`, and DNS fails |
| The kernel keyring | **Closed** by Docker's default seccomp profile: `add_key`, `keyctl` and `request_key` return EPERM. Without seccomp, the same user ID in every container would share one user keyring, so the seccomp check in the probes matters |
| The deps volume and the image | read-only (`EROFS`) |

**One channel is outside the grade: the Docker socket on the host** (`~/.colima/default/docker.sock`). Anything that can reach it can `docker exec` or `docker cp` into a running grade and read its hidden tests, or start a privileged container. The agent's sandbox must not reach it. This was not tested, since no agent session ran. Step 4 must show that a `docker` command fails inside a real agent run.

## Timings

The container numbers are this spike's. "Host" is today's run on this machine with the same commands and caches. "Sandbox" is the isolation spike's (2026-10-02) or the stored pilot validations; the sandbox was not re-run.

| Phase | Container |
|---|---|
| `docker create` | 0.03–0.10 s |
| Skeleton `docker cp` and `docker start` | 0.09–0.46 s |
| Copy-in by `tar` (0.4–6.2 MB trees) | 0.05–0.19 s |
| `docker rm -f -v` | 0.05–0.30 s |
| Overhead per grade, all four | about 0.3–0.5 s |

| Test command | Container | Host | Sandbox |
|---|---|---|---|
| Go fixture, `go test ./...` | 21 s with an empty `GOCACHE` (cgo); 0.3 s with a seeded one (and 0.12 s to copy it) | 14 s cold, 0.15 s warm | 5–11 s |
| Go task, `go test ./internal/claude/` | 2.3 s | 4.7–4.9 s | not timed |
| Maven fixture, `./mvnw -B test` | 18.5 s | 19 s (JDK 22), 18 s (JDK 21) | 17–24 s |
| Maven task | 18.4–18.7 s | not run | 19.2 s (failing stage) |
| Gradle fixture, Checkstyle then 37 tests | 71 s + 15 s with an empty Gradle home; 7.8 s + 4.9 s with a seeded one | 108 s + 2 s (empty home) | 31–156 s |
| Gradle tasks, `./gradlew test` | 18–25 s | not run | not comparable (invalid) |
| Cargo fixture, `cargo test` | 7.7 s | 59 s | 57–127 s |
| Cargo task | 2.4 s (fails), 7.8 s | not run | 2–4 s (failing stage) |
| pytest fixture | 3.4 s | 7 s | 3–16 s |
| pytest task, `uv run pytest` | 5.3–8.0 s | not run | not timed |

- Warm-ups in a container: Go modules 0.1 s; a Go build seed 21 s; a Gradle home seed 18 s; the Gradle 8.5 network warm-up 52 s (58 MiB); Python 1.1 s (1.8 MiB).
- Seeding the volumes from host tars: 0.04–1.8 s each.
- Peak memory per grade: Go 0.2–0.8 GiB, Maven 1.6 GiB, Gradle 1.8–2.6 GiB, Cargo 0.5 GiB, pytest 0.14 GiB. Nothing reached the 4 GiB limit, and no OOM kill or process-limit hit occurred.
- In this noisy sample, containers were not slower. The Linux builds ran as fast as the host's or faster, and the overhead was under half a second. The cost is cold per-grade caches, which seeds remove.

## Sizes

- Images: 890 MB compressed and 2.44 GB unpacked for the four. The VM's disk had 84 GB free of 96 GB.
- Deps volumes: Go 135 MB, Maven 99 MB, Gradle 1.42 GB (three wrapper distributions, `modules-2`, a 404 MB Gradle-home seed and the 8.5 warm-up), Cargo 307 MB, Python 177 MB (169 MB of it the unusable seed). All were removed.
- Per grade, in its anonymous volume: Go 124 MB (the build cache), Maven 15–17 MB, Gradle 0.4–0.7 GB, Cargo 0.2 GB, pytest 2 MB.

## Downloads

- Images: the four above, 890 MB compressed. Manifests only (no layers): `rust:latest` and several `ghcr.io/astral-sh/uv` tags, while choosing.
- Dependencies, from warm-up containers with network:
  - **The Gradle 8.5 base** (junit-pioneer `e9e465ed`): 58 MiB. This came from the Gradle plugin portal and Maven Central, and also from **`download.eclipse.org`**, which Spotless's Eclipse formatter fetches through P2 while the build configures. That host is not one of the registries listed in the approval. The fixture's own build reached for it, as Agentium's host warm-up would.
  - **click's `tests` group** from PyPI: 1.8 MiB.
- Nothing else was fetched. Every grade ran with `--network none`.

## Cleanup

All `agentium-spike-*` containers and volumes were removed. Afterwards no container or volume named `agentium-spike*` or labelled `agentium.spike` remained, and the counts matched the start: 13 containers and 400 volumes, all the user's own. The four pulled images are kept. Only the scratch folder holds the scripts and copies.

## Verdict

The container shape works for all five toolchains. Grades start, copy in and clean up in under half a second. Every side channel that `sandbox-v1` leaves open is closed. JDK 21 in a container makes two pilot Gradle tasks valid that the host cannot grade today.

Three things change the plan's design:
- **The images need more than the toolchain, `sh` and `tar`.** A build ran `git`, and tests ran `less`.
- **Caches need seeding per grade**, or Go and Gradle grades are 10–70× slower.
- **A created, never-started container outlives everything** but recovery.

The Rust and uv images do not match the host's versions.
