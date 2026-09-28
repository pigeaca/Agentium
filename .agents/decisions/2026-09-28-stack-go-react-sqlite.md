# ADR: Go, React and SQLite

## Status

Accepted by the user on 2026-09-28.

## Context

The [hybrid strategy](2026-09-27-hybrid-strategy.md) needs:
- a core that drives agent CLIs, runs git and processes, and computes statistics;
- a local web UI;
- local storage.

The study proposed a Go core, a React UI and SQLite, and the Phase 0 spike did not change that. The owner works in Go and React daily (Orchid), and the supply-chain rules favour few, vetted dependencies.

## Decision

- **Core:** Go 1.27.1, in a single binary (`cmd/agentium`), module `github.com/pigeaca/agentium`.
  - Standard library first: `os/exec`, `net/http`, `encoding/json` and `embed` cover the CLI, the local API and process supervision.
  - Code lives in `internal/` packages; `cmd/agentium` only wires them.
  - Conventions:
    - every blocking call takes a `context.Context`;
    - errors are wrapped with `%w` and context;
    - no package-level mutable state;
    - I/O goes through `io.Writer` parameters so it can be tested.
- **Storage:** SQLite through `github.com/mattn/go-sqlite3`, approved on 2026-09-28 and added when Phase 1 builds storage.
  - It has no Go dependencies, but needs cgo. Apple clang covers macOS and gcc covers the CI runners; release binaries are built per platform.
- **UI:** React with TypeScript, built into the binary with `embed`. Its toolchain (package manager, bundler and libraries) is vetted and approved when Phase 2 is planned. Until then there is no Node dependency.
- **Toolchain and supply chain:**
  - `go.mod` pins the Go version. The harness runs with `GOTOOLCHAIN=local` and `GOFLAGS=-mod=readonly`, so nothing downloads a toolchain or rewrites modules by itself.
  - Each new module needs approval, and `go.sum` is committed.
  - CI installs Go with `actions/setup-go` v7.0.0, pinned by SHA.
  - govulncheck (`golang.org/x/vuln` v1.8.0) runs in CI, and locally when cached.

## Consequences

- `harness.py check go` (gofmt, vet, race tests) joins `check ci`, and Go paths map to it in `check changed`. `check vuln` (govulncheck) runs separately: in CI after `check ci`, and through `check changed` when `go.mod` or `go.sum` change.
- Developers need Go 1.27.1. The harness finds it on `PATH` or in `~/sdk/go1.27.1`, and fails clearly otherwise.
- cgo means per-platform builds for releases. If that becomes a burden, a pure-Go driver (`modernc.org/sqlite`) is the fallback, at the cost of more dependencies.
