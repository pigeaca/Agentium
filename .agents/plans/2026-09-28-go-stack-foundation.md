# Go stack foundation

- Date: 2026-09-28
- Status: In Progress
- Scope/approval: the user confirmed Go + React + SQLite (2026-09-28) and approved:
  - Go 1.27.1, which the user installs;
  - `mattn/go-sqlite3`, to be added when Phase 1 builds storage, not in this change;
  - `actions/setup-go` v7.0.0 pinned to `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`;
  - govulncheck from `golang.org/x/vuln` v1.8.0;
  - later the same day: `actions/checkout` upgraded from v4.4.0 to v7.0.1 (`3d3c42e5aac5ba805825da76410c181273ba90b1`), because Node.js 20 is deprecated on runners.

## Outcome and boundaries
The repository becomes a Go project with the process checks the rules expect, before any product code:
- the stack decision recorded;
- a module with a minimal `agentium` command;
- Go checks in the harness, `check changed` rules and CI;
- the architecture's code map and conventions.

Boundaries:
- **Dependencies:** no third-party Go modules and no React or Node toolchain yet. The UI's toolchain is vetted when Phase 2 is planned.
- **No downloads from the harness:** `GOTOOLCHAIN=local`, and govulncheck runs locally only when it is already in the module cache. CI downloads the pinned version.
- **Credentials:** the harness environment clears provider credentials, so checks never reach live services.

## Acceptance
1. A decision record for the stack: Go 1.27.1, module path, layout, conventions, the dependency policy, SQLite driver and UI stack. Architecture and roadmap updated. Evidence: docs.
2. `go.mod` (`go 1.27.1`, no requirements) and `cmd/agentium` running `version` and `help`, with the logic in `internal/cli`, tested; unknown commands exit 2. Evidence: `check go`.
3. **Harness:**
   - `check go` runs gofmt (listing), `go vet`, `go test -race` and govulncheck when cached, otherwise reporting it as skipped;
   - `check ci` includes Go when `go.mod` exists;
   - `check changed` maps `*.go`, `go.mod` and `go.sum` to `check go`;
   - `doctor` shows the resolved Go;
   - the harness uses the go.mod version: from PATH if it matches, else `~/sdk/go<version>`, else it fails clearly without installing;
   - the environment sets `GOTOOLCHAIN=local` and `GOFLAGS=-mod=readonly`, and clears provider credentials.

   Evidence: new harness tests plus `check harness`.
4. **CI:** one job runs `check ci` with pinned `setup-go` (reading `go.mod`), then govulncheck v1.8.0. Evidence: the PR's CI run.
5. **Docs:** `docs/harness.md` (commands, the Go resolution, the "adding stack checks" status) and `.gitignore` for build output. Evidence: `check docs`.
6. The PR's CI passes and a review verdict is recorded.

## Work
- [ ] Decision record, architecture, roadmap
- [ ] Go module, CLI and tests
- [ ] Harness: Go resolution, `check go`, mappings, environment, doctor, tests
- [ ] CI job; docs; `.gitignore`

## Verification and handoff
`python3 .agents/scripts/harness.py check ci` locally with Go 1.27.1, the PR's CI, and a review.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
