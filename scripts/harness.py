#!/usr/bin/env python3
"""Agentium's development process entrypoint (standard library only).

Docs/adapter validation, the pre-commit guard, Go checks, check selection, task worktrees and plan metrics.
The stack is Go + React + SQLite (see .agents/decisions); React checks arrive with the UI in Phase 2.
"""
from __future__ import annotations

import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
DOC_ENTRYPOINTS = ("AGENTS.md", ".agents/README.md", ".agents/rules/core.md", ".agents/architecture.md", ".agents/ROADMAP.md")
# Provider credentials never reach checks or tests; the Go toolchain never switches or downloads itself, and
# builds never rewrite go.mod/go.sum.
CREDENTIAL_ENV = ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY")


def harness_env(environ: dict[str, str]) -> dict[str, str]:
    """The environment for harness commands: provider credentials removed, and Go pinned to the local toolchain with
    read-only modules. GOFLAGS is replaced, not appended to, so a personal GOFLAGS cannot change what checks run."""
    return {**{k: v for k, v in environ.items() if k not in CREDENTIAL_ENV}, "GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly"}


ENV = harness_env(dict(os.environ))
GOVULNCHECK = "golang.org/x/vuln/cmd/govulncheck@v1.8.0"
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
# Ignored path components that tools recreate; any other ignored file blocks `worktree remove`.
REGENERABLE = {"node_modules", "__pycache__", "dist", "build", "target", ".venv", ".pytest_cache", "coverage.out", "test-results", "playwright-report", ".DS_Store"}


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
    skipped = {archive, ROOT / ".claude/worktrees"}  # Client-managed worktrees are other checkouts, not this tree's docs.
    for directory, directories, files in os.walk(ROOT):
        directories[:] = [d for d in directories if d not in ignored and Path(directory, d) not in skipped and not Path(directory, d).is_symlink()]
        for name in files:
            path = Path(directory, name)
            if not name.endswith(".md") or path.is_symlink():
                continue
            for old in re.findall(r"plans/(?!archive/)([\w][\w.-]*\.md)", path.read_text()):
                if old in archived:
                    raise ValueError(f"Stale archived-plan link in {path.relative_to(ROOT)}: {old}")
    print(f"Agent entrypoints including Claude: {words}/1800 words; links and adapters valid. Plans: {len(active)} active, {len(archived)} archived.")


# --- Go -------------------------------------------------------------------------------------------

def go_mod_version() -> str:
    """The exact toolchain go.mod pins, e.g. '1.27.1': its `toolchain` line if present (setup-go prefers it too),
    otherwise its `go` line."""
    try:
        text = (ROOT / "go.mod").read_text()
    except FileNotFoundError:
        raise ValueError("No go.mod in this checkout.") from None
    match = (re.search(r"^toolchain\s+go(\d+\.\d+(?:\.\d+)?)\s*$", text, re.M)
             or re.search(r"^go\s+(\d+\.\d+(?:\.\d+)?)\s*$", text, re.M))
    if not match:
        raise ValueError("go.mod has no go version line.")
    return match[1]


def toolchain_version(go: str) -> str | None:
    # Outside the module, so go.mod cannot influence which version answers.
    result = subprocess.run([go, "version"], cwd=Path(go).parent, capture_output=True, text=True, env=ENV)
    match = re.search(r"\bgo(\d+\.\d+(?:\.\d+)?)\b", result.stdout)
    return match[1] if result.returncode == 0 and match else None


def go_binary() -> Path:
    """The toolchain go.mod pins: `go` on PATH when it matches, else ~/sdk/go<version>/bin/go. Never installs one."""
    wanted = go_mod_version()
    for candidate in (shutil.which("go"), str(Path.home() / f"sdk/go{wanted}/bin/go")):
        if candidate and Path(candidate).is_file() and toolchain_version(candidate) == wanted:
            return Path(candidate)
    raise ValueError(f"Go {wanted} (pinned in go.mod) is not on PATH or in ~/sdk/go{wanted}. Nothing was installed; with approval, "
                     f"run: go install golang.org/dl/go{wanted}@latest && ~/go/bin/go{wanted} download")


def toolchain_tool(go: Path, name: str) -> Path | None:
    """A tool from the same GOROOT as `go` (a symlinked `go` on PATH has no tools beside it)."""
    result = subprocess.run([str(go), "env", "GOROOT"], cwd=go.parent, capture_output=True, text=True, env=ENV)
    tool = Path(result.stdout.strip()) / "bin" / name
    return tool if result.returncode == 0 and tool.is_file() else None


def go_tool(name: str) -> str | None:
    """A tool from the pinned toolchain (e.g. gofmt), falling back to PATH when go.mod or the toolchain is absent."""
    try:
        tool = toolchain_tool(go_binary(), name)
    except ValueError:
        tool = None
    return str(tool) if tool else shutil.which(name)


def offline_go_env() -> dict[str, str]:
    # Locally, module fetches are off: a missing dependency fails instead of downloading. CI (CI=true) may download.
    return {} if os.environ.get("CI") else {"GOPROXY": "off"}


def check_go() -> None:
    go = go_binary()
    gofmt = toolchain_tool(go, "gofmt")
    if not gofmt:
        raise ValueError(f"gofmt not found in the GOROOT of {go}.")
    files = [path for path in git_output("ls-files", "-co", "--exclude-standard", "-z", "*.go").split("\0") if path]
    if files:
        result = subprocess.run([str(gofmt), "-l", *files], cwd=ROOT, capture_output=True, text=True, env=ENV)
        if result.returncode:
            raise ValueError(f"gofmt failed: {result.stderr.strip()}")
        if result.stdout.split():
            raise ValueError(f"Not gofmt-formatted: {', '.join(result.stdout.split())} (run gofmt -w on them).")
    run(str(go), "vet", "./...", extra_env=offline_go_env())
    run(str(go), "test", "-race", "-count=1", "./...", extra_env=offline_go_env())


def check_vuln() -> None:
    """govulncheck at the pinned version. It queries the online Go vulnerability database; locally it runs only when
    the tool is already in the module cache, and CI (CI=true) may download that exact version."""
    go = go_binary()
    args = (str(go), "run", GOVULNCHECK, "./...")
    if os.environ.get("CI"):
        run(*args)
        return
    skipped = (f"[harness] check vuln skipped: {GOVULNCHECK}, its dependencies or the project's dependencies are not in "
               "the module cache, and the harness never downloads. CI runs it; to run it locally, fetch them once with approval.")
    cache = subprocess.run([str(go), "env", "GOMODCACHE"], cwd=ROOT, capture_output=True, text=True, env=ENV).stdout.strip()
    module, version = GOVULNCHECK.partition("/cmd/")[0], GOVULNCHECK.rsplit("@", 1)[1]  # golang.org/x/vuln, v1.8.0
    if not (Path(cache) / f"{module}@{version}").is_dir():
        print(skipped, file=sys.stderr)
        return
    print("\n[harness] " + " ".join(args), flush=True)
    result = subprocess.run(list(args), cwd=ROOT, capture_output=True, text=True, env={**ENV, **offline_go_env()})
    if result.returncode and "GOPROXY=off" in result.stderr:
        print(skipped, file=sys.stderr)  # the tool's own dependencies are not cached
        return
    print(result.stdout, end="")
    print(result.stderr, end="", file=sys.stderr)
    if result.returncode:
        raise subprocess.CalledProcessError(result.returncode, list(args))


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
    return (name in {".env", "credentials.json", "settings.local.json"} or (name.startswith(".env.") and name != ".env.example")
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
    gofmt = go_tool("gofmt") if go_files else None
    if go_files and not gofmt:
        # GUI Git clients may lack the shell PATH; CI enforces formatting once a Go stack exists, so warn instead of blocking.
        print("[harness] gofmt not on PATH; staged Go formatting not checked.", file=sys.stderr)
        go_files = []
    for path in go_files:
        # Format the staged blob, not the working file, so partially staged hunks are judged as committed.
        blob = subprocess.run(["git", "show", f":{path}"], cwd=ROOT, capture_output=True, check=True).stdout
        result = subprocess.run([gofmt, "-l"], input=blob, capture_output=True)
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

    Add a rule here with each new stack check (see docs/harness.md#adding-stack-checks).
    """
    reasons: dict[tuple[str, ...], list[str]] = {}
    suggestions: set[str] = set()
    unmapped: list[str] = []
    for path in paths:
        mapped = False
        if path.endswith(".md") or path.startswith((".agents/", ".claude/", "docs/")):
            reasons.setdefault(("check", "docs"), []).append(path)
            mapped = True
        # Only the harness itself: other scripts added later should surface as unmapped until they get a rule.
        if path in {"scripts/harness.py", "scripts/test_harness.py"} or path.startswith(".githooks/"):
            reasons.setdefault(("check", "harness"), []).append(path)
            mapped = True
        # Go code plus everything embedded or read by Go tests (migrations, testdata) under cmd/ and internal/.
        if path.endswith(".go") or path in {"go.mod", "go.sum"} or path.startswith(("cmd/", "internal/")):
            reasons.setdefault(("check", "go"), []).append(path)
            mapped = True
        if path in {"go.mod", "go.sum"}:
            reasons.setdefault(("check", "vuln"), []).append(path)
        if path.startswith(".github/"):
            suggestions.add("CI workflow changed: verified only by the PR's CI run")
            mapped = True
        if not mapped and path not in {"LICENSE", ".gitignore"}:
            unmapped.append(path)
    if unmapped:
        suggestions.add(f"{len(unmapped)} file(s) have no mapped check yet (e.g. {unmapped[0]}); run the stack's checks by hand and add a rule")
    rank = {("check", "docs"): 0, ("check", "harness"): 1, ("check", "go"): 2, ("check", "vuln"): 3}
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


def https_url(url: str) -> str | None:
    """HTTPS form of an SSH GitHub remote (git@github.com:owner/repo.git), for shells without an SSH key."""
    match = re.match(r"^(?:ssh://)?git@github\.com[:/]+(.+?)/?$", url)
    return f"https://github.com/{match[1]}" if match else None


def fetch_default() -> tuple[str, bool]:
    """Fetch the remote default branch; fall back to HTTPS for SSH GitHub remotes. Returns (ref, fetched)."""
    base = remote_default()
    remote, _, branch = base.partition("/")
    if subprocess.run(["git", "fetch", "--quiet", remote, branch], cwd=ROOT, capture_output=True).returncode == 0:
        return base, True
    fallback = https_url(git_output("remote", "get-url", remote).strip())
    if fallback and subprocess.run(["git", "fetch", "--quiet", fallback, f"+refs/heads/{branch}:refs/remotes/{base}"],
                                   cwd=ROOT, capture_output=True).returncode == 0:
        return base, True
    print(f"[harness] Could not fetch {base} (SSH and HTTPS); its last fetched state may be stale.", file=sys.stderr)
    return base, False


def worktree_new(branch: str, base: str | None) -> None:
    if not BRANCH_PATTERN.match(branch):
        raise ValueError("Branch must look like <agent>/<type>/<topic>, e.g. claude/fix/cancel-race (see the Git rules).")
    primary = primary_root()
    path = primary.parent / f"{primary.name}-worktrees" / branch.replace("/", "-")
    if path.exists():
        raise ValueError(f"{path} already exists; reuse it or choose another branch.")
    if base is None:
        base, fetched = fetch_default()
        if not fetched:
            # A stale default branch silently starts the task from old code; make the choice explicit instead.
            raise ValueError(f"Could not fetch {base}; nothing created. Fix access, or pass --base {base} to use the stale state deliberately.")
    # --no-track: a task branch must not adopt the default branch as upstream (a bare push or pull would target it).
    run("git", "worktree", "add", "--no-track", "-b", branch, str(path), base)
    try:
        deps = install_dependencies(path)
    except ValueError as error:
        deps = f"NOT installed: {error}"
    print(f"\nWorktree ready (kept even if setup failed):\n  path: {path}\n  branch: {branch} from {base}\n  dependencies: {deps}\n"
          f"Run every command from that path, e.g. python3 {path}/scripts/harness.py check changed --dry-run")


def worktree_remove(branch: str) -> None:
    """Remove a merged task worktree and its local branch using only Git's non-forcing operations."""
    if not BRANCH_PATTERN.match(branch):
        raise ValueError("Only task branches (<agent>/<type>/<topic>) can be removed; the default branch never is.")
    base, _ = fetch_default()  # A stale base can only make a merged branch look unmerged, so removal errs on refusing.
    if subprocess.run(["git", "merge-base", "--is-ancestor", branch, base], cwd=ROOT, capture_output=True).returncode:
        raise ValueError(f"{branch} is not merged into {base}; nothing removed. Squash-merged branches must be removed by hand.")
    path = worktree_branches().get(branch)
    if path:
        if path.resolve() in {ROOT.resolve(), primary_root().resolve()}:
            raise ValueError(f"{branch} is checked out in {path}, the current or primary checkout; switch it first.")
        if git_output("-C", str(path), "status", "--porcelain").strip():
            raise ValueError(f"{path} has uncommitted or untracked work; nothing removed.")
        # `git worktree remove` also deletes ignored files; only regenerable ones may go without asking.
        # --ignored=matching lists each ignored pattern match (web/node_modules/), not its collapsed parent (web/).
        status = git_output("-C", str(path), "status", "--porcelain", "--ignored=matching", "--untracked-files=all")
        ignored = [line[3:] for line in status.splitlines() if line.startswith("!! ")]
        kept = [entry for entry in ignored if not REGENERABLE.intersection(entry.rstrip("/").split("/"))]
        if kept:
            raise ValueError(f"{path} has ignored files that may be personal work ({', '.join(kept[:5])}); move or delete them first.")
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
  check docs|harness|go|vuln|staged|ci
  check changed [--dry-run] [base]
                             Select and run the checks for this branch's changes
  hooks                      Enable the shared pre-commit hook for all worktrees
  worktree new <branch> [--base REF]
                             Task worktree from the remote default branch, offline dependency install
  worktree deps              Offline-install locked dependencies in this checkout
  worktree remove <branch>   Remove a merged, clean task worktree and its local branch
  metrics                    Summarize archived plans' Metrics blocks by agent and model

ci = docs + harness tests + go. go = gofmt, vet, race tests. vuln = pinned govulncheck (online DB).
staged = pre-commit checks on the index. Go runs at the go.mod version with GOTOOLCHAIN=local.
No toolchains, modules or tools are downloaded locally; worktree setup installs locked dependencies only from the
local cache. check vuln reads the online vulnerability database, and in CI may download its pinned version.
"""


def main(args: list[str]) -> None:
    command, *rest = args or ["help"]
    if command in {"help", "--help", "-h"}:
        print(HELP)
    elif command == "doctor":
        print(f"Python {sys.version.split()[0]}")
        for tool in ("git", "gh", "corepack"):
            print(f"  {tool:<9} {shutil.which(tool) or 'not found'}")
        try:
            go = go_binary()
            print(f"  {'go':<9} {go} (go{toolchain_version(str(go))}, pinned in go.mod)")
        except ValueError as error:
            print(f"  {'go':<9} {error}")
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
                run(sys.executable, "-m", "unittest", "discover", "-s", "scripts", "-p", "test_harness.py")
            if scope == "ci" and (ROOT / "go.mod").is_file():
                check_go()
        elif scope == "go":
            check_go()
        elif scope == "vuln":
            check_vuln()
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
        raise ValueError(f"Unknown command: {command}. Run python3 scripts/harness.py help.")


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
