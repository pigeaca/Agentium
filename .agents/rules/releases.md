# Releases and versions

Versions are SemVer (`vMAJOR.MINOR.PATCH`) applied to Agentium's **contract**, what users rely on:
- commands and flags;
- exit codes (0, 1, 2);
- `--json` output keys and their meaning;
- the data folder and SQLite schema: migrations are forward-only and applied automatically, and an older binary refuses a newer database;
- locked experiments: design versions, lock digests, statistics method versions and the verdicts of existing experiments;
- report Markdown and JSON.

| Bump | What changed | Examples |
|---|---|---|
| PATCH | Fixes only, no contract change | a crash, a wrong number, a visible bug |
| MINOR | Features and additive contract changes | a new command, flag or JSON key; a forward migration applied automatically; a new statistics method used only by new experiments (old ones keep theirs) |
| MAJOR | Breaking changes | a command or flag removed or renamed; a JSON key removed or its meaning changed; the data folder needing manual action; existing experiments' verdicts or digests changing; an exit code changing meaning |

**Before 1.0** a breaking change or a feature bumps MINOR (0.1 to 0.2) and a fix bumps PATCH (0.1.0 to 0.1.1). 1.0 is the user's decision; the tooling never proposes it.

**Marking breaking changes:** a `!` in the PR title (`feat(cli)!: ...`) and a `Breaking:` line in the PR body saying what users must do. `harness.py pr land` also reads the diff and refuses a PR whose detected contract change is not declared; the release bump is the larger of the declared and the detected one, never lower. The diff is of `internal/cli/testdata/contract.golden`, generated from the code (commands, flags, exit codes, `--json` schemas, migrations, design and method versions). For a false positive, put `Contract: none - <reason>` in the PR body; the release notes list it.

**What counts:** `feat`, `fix` and `perf` PRs, and any detected contract change, make a release due. `docs`, `test`, `chore`, `ci` and `refactor` PRs with no contract change never do on their own.

**Who releases:** the first release (v0.1.0) is cut explicitly (`release cut --first`) after its checklist ([plan](../plans/2026-10-04-releases.md)). After that releases are automatic: `pr land` runs `release plan` after a merge and cuts when something is due and the checks pass (`--no-release` skips). Tags are annotated and pushed over HTTPS, with a GitHub Release whose notes are the changelog; there is no CHANGELOG.md and no commit to main. Details: [harness](../../docs/harness.md#releases).
