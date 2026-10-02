# ADR: Containers driven directly through Docker, not Harbor

## Status
Accepted (2026-10-02). Supersedes the container clause of [the hybrid strategy](2026-09-27-hybrid-strategy.md); the rest of that decision stands.

## Context
The hybrid strategy said containers would come later through Harbor, pinned and called out of process. Planning the container mode ([isolation plan](../plans/2026-10-02-isolation.md), part 2) found that Harbor would duplicate what Agentium already owns: Claude Code's isolated invocation, stream metrics, grading and records. It also brings its own task format, per-trial image builds, and a Python dependency tree that includes LiteLLM, which shipped a credential stealer in two releases in 2026-03.

## Decision
- Agentium drives containers itself through the `docker` CLI as a separate process (Podman and colima work through the same CLI). No Go module is added.
- Images are per toolchain (Go, JDK with Maven or Gradle, Python with uv, Node with pnpm) plus a shared Claude Code layer, from official base images; project dependencies are warmed into a volume mounted read-only. A repository's own `Dockerfile` or `.devcontainer` may be used instead.
- The executor (local or container) is recorded in each run and lock; one experiment never mixes them.
- Harbor's task format stays a possible import and export path for public benchmarks.

## Consequences
- Agentium maintains its own container invocation, images and their pins, and tests them against Docker CLI changes.
- Container runs on a Mac run Linux toolchains in a VM: slower, and not comparable with local runs.
- Sign-in inside containers uses an API key or a subscription token file, mounted read-only.
