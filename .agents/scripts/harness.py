#!/usr/bin/env python3
"""Agentium's development process entrypoint (standard library only).

Stack-neutral until the product stack is chosen: docs/adapter validation, the pre-commit guard,
check selection, task worktrees, and plan metrics. See docs/harness.md#adding-stack-checks.
"""
from __future__ import annotations

import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
DOC_ENTRYPOINTS = ("AGENTS.md", ".agents/README.md", ".agents/rules/core.md", ".agents/architecture.md", ".agents/ROADMAP.md")
ENV = {**os.environ}
# Credential shapes rejected on staged additions. Only fake test values may opt out with "secret-scan: allow" on the line.
SECRET_PATTERNS = (
    ("Anthropic API key", r"\bsk-ant-[A-Za-z0-9_-]{20,}"),
    ("OpenAI API key", r"\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,}"),
    ("AWS access key", r"\bAKIA[0-9A-Z]{16}\b"),
    ("GitHub token", r"\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})"),
    ("Slack token", r"\bxox[abprs]-[A-Za-z0-9-]{10,}"),
    ("private key", r"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
)
BRANCH_PATTERN = re.compile(r"^[a-z0-9-]+/(?:feat|fix|analysis|refactor|docs|test|chore)/[a-z0-9][a-z0-9.-]*$")
METRIC_LINE = re.compile(r"^- (Agent|Elapsed|Check-fix loops|User corrections|Review):[ \t]*(.*)$", re.M)
# Lockfile -> offline install command. Only lockfile-exact, cache-only installs belong here (supply-chain rule).
OFFLINE_INSTALLERS = {
    "pnpm-lock.yaml": (["corepack", "pnpm", "install", "--frozen-lockfile", "--offline"], {"COREPACK_ENABLE_NETWORK": "0", "npm_config_update_notifier": "false"}),
}


def run(*args: str, cwd: Path | None = None, extra_env: dict[str, str] | None = None) -> None:
    # Resolve ROOT per call (not as a default argument) so tests and callers that repoint it never touch another checkout.
    print("\n[harness] " + " ".join(map(str, args)), flush=True)
    subprocess.run(list(map(str, args)), cwd=cwd or ROOT, env={**ENV, **(extra_env or {})}, check=True)


def git_output(*args: str) -> str:
    return subprocess.run(["git", *args], cwd=ROOT, capture_output=True, check=True).stdout.decode(errors="replace")


# --- Documentation and adapters -------------------------------------------------------------

def markdown_links(path: Path) -> list[str]:
    return re.findall(r"\]\(([^)#\s]+)(?:#[^)]*)?\)", path.read_text())


def check_doc_links(paths: list[Path]) -> None:
    for path in paths:
        for link in markdown_links(path):
            if not re.match(r"https?://", link) and not (path.parent / link).exists():
                raise ValueError(f"{path.relative_to(ROOT)}: broken link {link}")


def skill_metadata(path: Path, expected_name: str | None = None) -> tuple[str, str]:
    frontmatter = re.match(r"\A---\n(.*?)\n---\n", path.read_text(), re.S)
    fields = dict(re.findall(r"^(name|description): ([^\n]+)$", frontmatter[1], re.M)) if frontmatter else {}
    if fields.get("name") != (expected_name or path.parent.name) or not fields.get("description", "").strip():
        raise ValueError(f"Skill or role requires matching name and a description: {path}")
    return fields["name"], fields["description"]


def check_adapter_link(adapter: Path, canonical: Path, kind: str, name: str) -> None:
    targets = {(adapter.parent / link).resolve() for link in markdown_links(adapter) if not link.startswith("https://")}
    if canonical.resolve() not in targets:
        raise ValueError(f"Claude {kind} must link its canonical instructions: {name}")


def check_agent_adapters() -> None:
    # Our adapter deliberately uses direct standalone imports, not the full Claude grammar.
    claude = ROOT / "CLAUDE.md"
    body = re.sub(r"(?ms)^```[^\n]*\n.*?^```[ \t]*$|^~~~[^\n]*\n.*?^~~~[ \t]*$", "", claude.read_text())
    imports = re.findall(r"^@([^\s]+)\s*$", body, re.M)
    if len(imports) != len(DOC_ENTRYPOINTS) or set(imports) != set(DOC_ENTRYPOINTS):
        raise ValueError("CLAUDE.md must explicitly import each shared entrypoint once.")
    for target in imports:
        if not (claude.parent / target).is_file():
            raise ValueError(f"CLAUDE.md: missing import {target}")
    canonical = {p.parent.name: p for p in (ROOT / ".agents/skills").glob("*/SKILL.md")}
    adapters = {p.parent.name: p for p in (ROOT / ".claude/skills").glob("*/SKILL.md")}
    if canonical.keys() != adapters.keys():
        raise ValueError(f"Claude skill adapter mismatch: {sorted(canonical.keys() ^ adapters.keys())}")
    for name, path in canonical.items():
        if skill_metadata(path) != skill_metadata(adapters[name]):
            raise ValueError(f"Claude skill metadata differs from canonical skill: {name}")
        check_adapter_link(adapters[name], path, "skill", name)
    # Subagent roles follow the same rule: shared body in .agents/roles, Claude-only model/tool settings in the adapter.
    roles = {p.stem: p for p in (ROOT / ".agents/roles").glob("*.md")}
    subagents = {p.stem: p for p in (ROOT / ".claude/agents").glob("*.md")}
    if roles.keys() != subagents.keys():
        raise ValueError(f"Claude subagent adapter mismatch: {sorted(roles.keys() ^ subagents.keys())}")
    for name, path in roles.items():
        if skill_metadata(path, name) != skill_metadata(subagents[name], name):
            raise ValueError(f"Claude subagent metadata differs from canonical role: {name}")
        check_adapter_link(subagents[name], path, "subagent", name)


def check_docs() -> None:
    entries = [ROOT / entry for entry in (*DOC_ENTRYPOINTS, "CLAUDE.md")]
    words = sum(len(path.read_text().split()) for path in entries)
    linked_docs = entries + [ROOT / "README.md", ROOT / ".agents/skills/README.md", ROOT / ".agents/decisions/README.md", ROOT / ".agents/plans/README.md"]
    for folder in (".agents/rules", ".agents/reference", ".agents/skills", ".agents/roles", ".agents/templates", ".claude/skills", ".claude/agents", "docs"):
        linked_docs.extend((ROOT / folder).rglob("*.md"))
    check_doc_links(linked_docs)
    check_agent_adapters()
    if words > 1800:
        raise ValueError(f"Default context is {words} words; move details to targeted references (limit 1800).")
    plans = ROOT / ".agents/plans"
    archive = plans / "archive"
    complete = re.compile(r"^\s*[-*]?\s*Status:\s*(?:Complete|Completed)\.?\s*$", re.I | re.M)
    active = [p for p in plans.glob("*.md") if p.name != "README.md"]
    for path in active:
        if complete.search(path.read_text()):
            raise ValueError(f"Completed plan must be archived: {path.name}")
    archived = {p.name for p in archive.glob("*.md") if p.name != "INDEX.md"}
    indexed = set(re.findall(r"\]\(([^)]+\.md)\)", (archive / "INDEX.md").read_text()))
    if archived != indexed:
        raise ValueError(f"Archive/index mismatch: {sorted(archived ^ indexed)}")
    ignored = {"node_modules", ".git", "dist", "vendor", ".venv"}
    for directory, directories, files in os.walk(ROOT):
        directories[:] = [d for d in directories if d not in ignored and Path(directory, d) != archive and not Path(directory, d).is_symlink()]
        for name in files:
            path = Path(directory, name)
            if not name.endswith(".md") or path.is_symlink():
                continue
            for old in re.findall(r"plans/(?!archive/)([\w][\w.-]*\.md)", path.read_text()):
                if old in archived:
                    raise ValueError(f"Stale archived-plan link in {path.relative_to(ROOT)}: {old}")
    print(f"Agent entrypoints including Claude: {words}/1800 words; links and adapters valid. Plans: {len(active)} active, {len(archived)} archived.")


# --- Pre-commit guard -------------------------------------------------------------------------

def credential_findings(diff: str) -> list[str]:
    """Locate credential-shaped added lines in a zero-context diff without echoing the values."""
    findings, path, number, removed, added = [], "?", 0, 0, 0
    for line in diff.splitlines():
        # Hunk line counts decide what is content, so an added line beginning "++ " is never read as a file header.
        if removed or added:
            if line.startswith("-"):
                removed -= 1
            elif line.startswith("+"):
                added -= 1
                if "secret-scan: allow" not in line:
                    label = next((label for label, pattern in SECRET_PATTERNS if re.search(pattern, line)), None)
                    if label:
                        findings.append(f"{path}:{number}: added line looks like a {label}")
                number += 1
        elif line.startswith("+++ "):
            path = line[6:] if line.startswith("+++ b/") else line[4:]
        elif hunk := re.match(r"@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@", line):
            removed, number, added = int(hunk[1] or 1), int(hunk[2]), int(hunk[3] or 1)
    return findings


def sensitive_path(path: str) -> bool:
    name = Path(path).name
    return (name in {".env", "credentials.json"} or (name.startswith(".env.") and name != ".env.example")
            or name.endswith((".pem", ".key", ".p12", ".pfx"))
            or (name.startswith(("id_rsa", "id_ecdsa", "id_ed25519")) and not name.endswith(".pub")))


def check_staged() -> None:
    """Pre-commit guard over the index only; it stays fast and never runs test suites."""
    problems = []
    whitespace = subprocess.run(["git", "diff", "--cached", "--check"], cwd=ROOT, capture_output=True, text=True)
    if whitespace.returncode:
        problems.append("Whitespace errors:\n" + whitespace.stdout.strip())
    staged = [path for path in git_output("diff", "--cached", "--name-only", "-z", "--diff-filter=ACMR").split("\0") if path]
    problems += [f"{path}: credential and environment files must stay untracked" for path in staged if sensitive_path(path)]
    problems += credential_findings(git_output("diff", "--cached", "--no-color", "--no-ext-diff", "-U0", "--diff-filter=ACMR"))
    go_files = [path for path in staged if path.endswith(".go")]
    if go_files and not shutil.which("gofmt"):
        # GUI Git clients may lack the shell PATH; CI enforces formatting once a Go stack exists, so warn instead of blocking.
        print("[harness] gofmt not on PATH; staged Go formatting not checked.", file=sys.stderr)
        go_files = []
    for path in go_files:
        # Format the staged blob, not the working file, so partially staged hunks are judged as committed.
        blob = subprocess.run(["git", "show", f":{path}"], cwd=ROOT, capture_output=True, check=True).stdout
        result = subprocess.run(["gofmt", "-l"], input=blob, capture_output=True)
        if result.returncode or result.stdout.strip():
            problems.append(f"{path}: staged Go is not gofmt-formatted {result.stderr.decode().strip()}".rstrip())
    try:
        check_docs()
    except ValueError as error:
        problems.append(f"Documentation: {error}")
    if problems:
        print("\n".join(problems), file=sys.stderr)
        raise ValueError(f"{len(problems)} staged check(s) failed. Fix and re-stage; bypassing with --no-verify needs explicit user approval.")
    print(f"Staged checks passed for {len(staged)} file(s).")


def enable_hooks() -> None:
    """Point every worktree at its own tracked .githooks; refuse to replace someone else's hook path."""
    current = subprocess.run(["git", "config", "--get", "core.hooksPath"], cwd=ROOT, capture_output=True, text=True).stdout.strip()
    if current and current != ".githooks":
        raise ValueError(f"core.hooksPath is already {current}; add .githooks/pre-commit to it manually.")
    run("git", "config", "core.hooksPath", ".githooks")
    print("Pre-commit checks enabled for every worktree of this repository. Disable with: git config --unset core.hooksPath")


# --- Check selection --------------------------------------------------------------------------

def remote_default() -> str:
    """The remote's default branch as '<remote>/<branch>', e.g. 'origin/main'."""
    for remote in git_output("remote").split():
        ref = subprocess.run(["git", "symbolic-ref", "--short", f"refs/remotes/{remote}/HEAD"], cwd=ROOT, capture_output=True, text=True)
        if ref.returncode == 0:
            return ref.stdout.strip()
    raise ValueError("No remote default branch; run `git remote set-head <remote> --auto`.")


def plan_checks(paths: list[str]) -> tuple[list[tuple[list[str], str]], list[str]]:
    """Map changed paths to harness commands (fast to slow) plus suggestions that are not run.

    Stack rules belong here once the stack is chosen (see docs/harness.md#adding-stack-checks).
    """
    reasons: dict[tuple[str, ...], list[str]] = {}
    suggestions: set[str] = set()
    unmapped: list[str] = []
    for path in paths:
        mapped = False
        if path.endswith(".md") or path.startswith((".agents/", ".claude/", "docs/")):
            reasons.setdefault(("check", "docs"), []).append(path)
            mapped = True
        if path.startswith((".agents/scripts/", ".githooks/")):
            reasons.setdefault(("check", "harness"), []).append(path)
            mapped = True
        if path.startswith(".github/"):
            suggestions.add("CI workflow changed: verified only by the PR's CI run")
            mapped = True
        if not mapped and path not in {"LICENSE", ".gitignore"}:
            unmapped.append(path)
    if unmapped:
        suggestions.add(f"{len(unmapped)} file(s) have no mapped check yet (e.g. {unmapped[0]}); run the stack's checks by hand and add a rule")
    rank = {("check", "docs"): 0, ("check", "harness"): 1}
    planned = [(list(command), files[0] + (f" and {len(files) - 1} more" if len(files) > 1 else ""))
               for command, files in sorted(reasons.items(), key=lambda item: (rank.get(item[0], 9), item[0]))]
    return planned, sorted(suggestions)


def check_changed(args: list[str]) -> None:
    dry_run = "--dry-run" in args
    values = [value for value in args if value != "--dry-run"]
    if len(values) > 1 or any(value.startswith("--") for value in values):
        raise ValueError("Usage: check changed [--dry-run] [base]")
    base = values[0] if values else remote_default()
    merge_base = git_output("merge-base", base, "HEAD").strip()
    # Committed, staged and unstaged changes since the merge base, plus untracked files. --no-renames lists both sides of a move.
    changed = git_output("diff", "--name-only", "--no-renames", "-z", merge_base) + git_output("ls-files", "--others", "--exclude-standard", "-z")
    paths = sorted({path for path in changed.split("\0") if path})
    planned, suggestions = plan_checks(paths)
    print(f"[harness] {len(paths)} changed file(s) since {base} ({merge_base[:8]}).")
    for command, reason in planned:
        print(f"  {' '.join(command):<40} <- {reason}")
    for suggestion in suggestions:
        print(f"  suggested, not run: {suggestion}")
    if not planned:
        print("  nothing to run.")
    if dry_run:
        return
    for command, _ in planned:
        main(command)


# --- Task worktrees ---------------------------------------------------------------------------

def install_dependencies(root: Path) -> str:
    """Install locked dependencies offline for every known lockfile in the checkout; never downloads."""
    installed = []
    for lockfile, (command, env) in OFFLINE_INSTALLERS.items():
        for directory in sorted({path.parent for path in root.glob(f"*/{lockfile}")} | ({root} if (root / lockfile).is_file() else set())):
            if not shutil.which(command[0]):
                raise ValueError(f"{command[0]} is missing; run doctor. No installation attempted.")
            try:
                run(*command, cwd=directory, extra_env=env)
            except subprocess.CalledProcessError as error:
                raise ValueError(f"Offline install failed in {directory}: packages or the pinned package manager are not in the local cache. "
                                 "Nothing was downloaded; populate the cache with approved setup, then run `worktree deps`.") from error
            installed.append(str(directory.relative_to(root)) or ".")
    return f"installed offline in {', '.join(installed)}" if installed else "none configured for this stack yet"


def primary_root() -> Path:
    return Path(git_output("rev-parse", "--path-format=absolute", "--git-common-dir").strip()).parent


def worktree_branches() -> dict[str, Path]:
    branches, path = {}, None
    for line in git_output("worktree", "list", "--porcelain").splitlines():
        if line.startswith("worktree "):
            path = Path(line[len("worktree "):])
        elif line.startswith("branch refs/heads/") and path:
            branches[line[len("branch refs/heads/"):]] = path
    return branches


def fetch_default() -> str:
    base = remote_default()
    remote, _, branch = base.partition("/")
    if subprocess.run(["git", "fetch", "--quiet", remote, branch], cwd=ROOT, capture_output=True).returncode:
        print(f"[harness] Could not fetch {base}; using the last fetched state.", file=sys.stderr)
    return base


def worktree_new(branch: str, base: str | None) -> None:
    if not BRANCH_PATTERN.match(branch):
        raise ValueError("Branch must look like <agent>/<type>/<topic>, e.g. claude/fix/cancel-race (see the Git rules).")
    primary = primary_root()
    path = primary.parent / f"{primary.name}-worktrees" / branch.replace("/", "-")
    if path.exists():
        raise ValueError(f"{path} already exists; reuse it or choose another branch.")
    base = base or fetch_default()
    # --no-track: a task branch must not adopt the default branch as upstream (a bare push or pull would target it).
    run("git", "worktree", "add", "--no-track", "-b", branch, str(path), base)
    try:
        deps = install_dependencies(path)
    except ValueError as error:
        deps = f"NOT installed: {error}"
    print(f"\nWorktree ready (kept even if setup failed):\n  path: {path}\n  branch: {branch} from {base}\n  dependencies: {deps}\n"
          f"Run every command from that path, e.g. python3 {path}/.agents/scripts/harness.py check changed --dry-run")


def worktree_remove(branch: str) -> None:
    """Remove a merged task worktree and its local branch using only Git's non-forcing operations."""
    if not BRANCH_PATTERN.match(branch):
        raise ValueError("Only task branches (<agent>/<type>/<topic>) can be removed; the default branch never is.")
    base = fetch_default()
    if subprocess.run(["git", "merge-base", "--is-ancestor", branch, base], cwd=ROOT, capture_output=True).returncode:
        raise ValueError(f"{branch} is not merged into {base}; nothing removed. Squash-merged branches must be removed by hand.")
    path = worktree_branches().get(branch)
    if path:
        if path.resolve() in {ROOT.resolve(), primary_root().resolve()}:
            raise ValueError(f"{branch} is checked out in {path}, the current or primary checkout; switch it first.")
        if git_output("-C", str(path), "status", "--porcelain").strip():
            raise ValueError(f"{path} has uncommitted or untracked work; nothing removed.")
        run("git", "worktree", "remove", str(path))
    run("git", "branch", "-d", branch)


# --- Metrics ----------------------------------------------------------------------------------

def plan_metrics(text: str) -> dict | None:
    section = re.search(r"^## Metrics\n(.*?)(?=^## |\Z)", text, re.S | re.M)
    fields = dict(METRIC_LINE.findall(section[1])) if section else {}
    agent = [part.strip() for part in fields.get("Agent", "").split("/")]
    if len(agent) < 3 or any(not part or "<" in part for part in agent[:3]):
        return None

    def number(key: str) -> int | None:
        match = re.match(r"\s*(\d+)", fields.get(key, ""))
        return int(match[1]) if match else None

    return {"agent": " / ".join(agent[:3]), "minutes": number("Elapsed"), "loops": number("Check-fix loops"), "corrections": number("User corrections")}


def metrics_report() -> None:
    rows = [metric for path in sorted((ROOT / ".agents/plans/archive").glob("*.md")) if (metric := plan_metrics(path.read_text()))]
    if not rows:
        print("No archived plan has a filled Metrics block yet.")
        return

    def average(values: list[int | None]) -> str:
        known = [value for value in values if value is not None]
        return f"{sum(known) / len(known):.1f}" if known else "-"

    print(f"{'client / model / effort':<44} {'tasks':>5} {'0-fix %':>8} {'loops':>6} {'fixes':>6} {'min':>6}")
    for agent in sorted({row["agent"] for row in rows}):
        group = [row for row in rows if row["agent"] == agent]
        rated = [row for row in group if row["corrections"] is not None]
        clean = [row for row in rated if row["corrections"] == 0]
        rate = f"{100 * len(clean) / len(rated):.0f}" if rated else "-"
        print(f"{agent:<44} {len(group):>5} {rate:>8} {average([r['loops'] for r in group]):>6} {average([r['corrections'] for r in group]):>6} {average([r['minutes'] for r in group]):>6}")
    print(f"\n{len(rows)} measured plan(s). '0-fix %' = tasks accepted without user corrections. Small samples are anecdotes.")


HELP = """Agentium harness (Python standard library)
  doctor                     Inspect installed tools; no installation
  check docs|harness|staged|ci
  check changed [--dry-run] [base]
                             Select and run the checks for this branch's changes
  hooks                      Enable the shared pre-commit hook for all worktrees
  worktree new <branch> [--base REF]
                             Task worktree from the remote default branch, offline dependency install
  worktree deps              Offline-install locked dependencies in this checkout
  worktree remove <branch>   Remove a merged, clean task worktree and its local branch
  metrics                    Summarize archived plans' Metrics blocks by agent and model

ci = docs + harness tests. staged = pre-commit checks on the index.
Stack checks are added when the stack is chosen (docs/harness.md#adding-stack-checks).
Nothing is downloaded; worktree setup installs locked dependencies only from the local cache.
"""


def main(args: list[str]) -> None:
    command, *rest = args or ["help"]
    if command in {"help", "--help", "-h"}:
        print(HELP)
    elif command == "doctor":
        print(f"Python {sys.version.split()[0]}")
        for tool in ("git", "gh", "gofmt", "corepack"):
            print(f"  {tool:<9} {shutil.which(tool) or 'not found'}")
    elif command == "check":
        scope = rest[0] if rest else "ci"
        if scope == "changed":
            check_changed(rest[1:])
        elif scope == "staged":
            check_staged()
        elif scope in {"docs", "harness", "ci"}:
            if scope in {"docs", "ci"}:
                check_docs()
            if scope in {"harness", "ci"}:
                run(sys.executable, "-m", "unittest", "discover", "-s", ".agents/scripts", "-p", "test_harness.py")
        else:
            raise ValueError(f"Unknown check scope: {scope}")
    elif command == "hooks":
        enable_hooks()
    elif command == "worktree":
        action, *values = rest or [""]
        if action == "new" and len(values) in {1, 3} and (len(values) == 1 or values[1] == "--base"):
            worktree_new(values[0], values[2] if len(values) == 3 else None)
        elif action == "deps" and not values:
            print(f"Dependencies: {install_dependencies(ROOT)}")
        elif action == "remove" and len(values) == 1:
            worktree_remove(values[0])
        else:
            raise ValueError("Usage: worktree new <branch> [--base REF] | worktree deps | worktree remove <branch>")
    elif command == "metrics":
        metrics_report()
    else:
        raise ValueError(f"Unknown command: {command}. Run python3 .agents/scripts/harness.py help.")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except subprocess.CalledProcessError as error:
        sys.exit(error.returncode or 1)
    except (OSError, ValueError) as error:
        print(f"[harness] {error}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        sys.exit(130)
