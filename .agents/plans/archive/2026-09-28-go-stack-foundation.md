# Go stack foundation

- Date: 2026-09-28
- Status: Completed
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
   - `check go` runs gofmt (listing), `go vet` and `go test -race`, with no module fetches locally. `check vuln` runs govulncheck, locally only when cached, otherwise reporting it as skipped. The split was clarified in review;
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
- [x] Decision record, architecture, roadmap
- [x] Go module, CLI and tests
- [x] Harness: Go resolution, `check go`, mappings, environment, doctor, tests
- [x] CI job; docs; `.gitignore`
- [x] Upgrade `actions/checkout` to v7.0.1 (approved mid-task)

## Verification and handoff
Planned: `harness.py check ci` locally with Go 1.27.1, the PR's CI, and a review.

Results:
- Before Go 1.27.1 was installed, `doctor` and `check go` stopped with the install command and installed nothing.
- After the user installed it, the harness found `~/sdk/go1.27.1` with no PATH change.
- `check changed` ran and passed docs, harness (37 tests), and go (gofmt, vet, race tests). `check vuln` was skipped locally as designed (not cached).
- CI on e94e288 (run 36394489837) passed:
  - checkout v7.0.1 and setup-go installed 1.27.1 from `go.mod`;
  - 37 harness tests and the Go race tests passed;
  - govulncheck v1.8.0: "No vulnerabilities found."
- `agentium version` prints `agentium dev (go1.27.1 darwin/arm64)`.

Limitations and follow-up:
- No SQLite or React yet; SQLite is added with Phase 1 storage, and the UI toolchain is vetted in Phase 2.
- CI module caching stays off until there is a `go.sum`.
- When the first dependency is added, fetch it once with approval (`go mod download`), because local checks run with `GOPROXY=off`.

## Review
Independent read-only reviewer (Opus) on 44ce402: **changes requested**, 8 findings plus nits.
- **Medium:**
  - gofmt was taken from beside a symlinked `go` (reproduced), breaking `check go` and the pre-commit hook;
  - `check go` could download modules once dependencies exist.
- **Low:**
  - the ADR and plan wording;
  - `check vuln` with a partial cache;
  - download wording;
  - a credential test that tested nothing;
  - missing tests;
  - gofmt's exit code ignored.

All were fixed in e94e288. **Re-check: approve.** The two remaining optional items (skip-message wording, and `persist-credentials: false` on checkout) were applied before archiving. The reviewer suggests an optional second review from Codex, because the change touches the harness's no-download and credential guarantees.

## Metrics
- Agent: Claude Code / claude-opus-5-5 / default
- Elapsed: 120m
- Check-fix loops: 3 (Python 3.9 f-string/annotation in tests; stale workflow pin test; unmapped-example test after adding Go rules)
- User corrections: 0
- Review: changes requested, 8 fixed
