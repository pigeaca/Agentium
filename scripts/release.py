"""Agentium releases: versions from merged PRs and the contract diff, then tag and GitHub Release (standard library,
git and gh only). The policy is .agents/rules/releases.md; the commands are `release plan` and `release cut` in
harness.py, which also calls after_merge from `pr land`.

Helpers that talk to GitHub or run checks (fetch_default, gh, ci_runs, run ...) are looked up on the harness module at
call time, so tests replace them there. Functions taking `repo` read only that git repository."""
from __future__ import annotations

import json
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any, Callable

import harness

TAG_PATTERN = re.compile(r"^v(\d+)\.(\d+)\.(\d+)$")
FIRST_VERSION = (0, 1, 0)
LEVELS = {"none": 0, "patch": 1, "minor": 2, "major": 3}
CONVENTIONAL = re.compile(r"^(\w+)(?:\([^)]*\))?(!)?:\s*\S")
BREAKING_LINE = re.compile(r"^(?:Breaking|BREAKING CHANGE):[ \t]*\S", re.M)
MIGRATIONS = "internal/store/migrations/"
# Sources of the public JSON documents and the flags/commands/constants users script against.
JSON_SOURCES = ("internal/cli/json", "internal/report/")
CLI_SOURCES = "internal/cli/"
CONSTANT_SOURCES = (("internal/experiment/", re.compile(r"^\s*(?:const\s+)?((?:Design|Method)\w*)\s*(?:string\s*)?=\s*(.+?)\s*(?://.*)?$")),
                    ("internal/cli/cli.go", re.compile(r"^\s*(Exit\w+)\s*=\s*(.+?)\s*(?://.*)?$")))
GO_FLAG = re.compile(r"""\.(?:String|Bool|Int|Int64|Uint|Uint64|Float64|Duration|Func|BoolFunc)\("([\w-]+)"|"""
                     r"""\.(?:StringVar|BoolVar|IntVar|Int64Var|UintVar|Float64Var|DurationVar|Var)\([^,]+,\s*"([\w-]+)\"""")
FIRST_CHECKLIST = """The first release (v0.1.0) is gated by this checklist; confirm every item before going on:
  [ ] the sandbox real check passed (isolation step 4)
  [ ] the repository cleanup is done: personal paths scrubbed, old branches deleted
  [ ] checks are green: CI, or --local-checks while GitHub Actions runs no jobs
  [ ] the README has the install section and the known limits
  [ ] `agentium version` works"""


def git(repo: Path, *args: str, check: bool = True) -> str:
    result = subprocess.run(["git", "-C", str(repo), *args], capture_output=True, text=True)
    if check and result.returncode:
        raise ValueError(f"git {' '.join(args[:2])} failed: {(result.stderr or result.stdout).strip()}")
    return result.stdout


def show(repo: Path, rev: str | None, path: str) -> str:
    """A file's text at a revision; empty when absent (or rev is None)."""
    if rev is None:
        return ""
    result = subprocess.run(["git", "-C", str(repo), "show", f"{rev}:{path}"], capture_output=True, text=True)
    return result.stdout if result.returncode == 0 else ""


# --- Versions -----------------------------------------------------------------------------------

def parse_tag(tag: str) -> tuple[int, int, int] | None:
    match = TAG_PATTERN.match(tag)
    return (int(match[1]), int(match[2]), int(match[3])) if match else None


def last_tag(repo: Path, rev: str = "HEAD") -> str | None:
    """The highest vX.Y.Z tag reachable from rev, or None before the first release."""
    tags = [t for t in git(repo, "tag", "--merged", rev, "--list", "v*").split() if parse_tag(t)]
    return max(tags, key=lambda t: parse_tag(t), default=None)


def next_version(last: tuple[int, int, int] | None, level: int) -> tuple[int, int, int] | None:
    """The version after `last` for a bump level (0 none, 1 patch, 2 minor, 3 major); None when nothing is releasable.
    Before 1.0 a breaking change or a feature bumps MINOR; 1.0 itself is the user's decision, never computed."""
    if level == 0:
        return None
    if last is None:
        return FIRST_VERSION
    major, minor, patch = last
    if major == 0:
        return (0, minor + 1, 0) if level >= 2 else (0, minor, patch + 1)
    if level == 3:
        return (major + 1, 0, 0)
    return (major, minor + 1, 0) if level == 2 else (major, minor, patch + 1)


def fmt(version: tuple[int, int, int]) -> str:
    return "v%d.%d.%d" % version


# --- Merged PRs ---------------------------------------------------------------------------------

def classify(title: str, body: str) -> str:
    """breaking (a `!` in the title or a Breaking: line), feature (feat), fix (fix, perf) or other."""
    match = CONVENTIONAL.match(title.strip())
    if (match and match[2]) or BREAKING_LINE.search(body or ""):
        return "breaking"
    kind = match[1].lower() if match else ""
    return "feature" if kind == "feat" else "fix" if kind in {"fix", "perf"} else "other"


def merged_entries(repo: Path, since: str | None, rev: str) -> list[dict[str, Any]]:
    """First-parent commits of rev since a tag: PR merges ('Merge pull request #N'; the body's first line is the PR title)
    and direct commits (their subject). Merges of branches into main (`Merge branch`) are not changes."""
    span = f"{since}..{rev}" if since else rev
    out = git(repo, "log", "--first-parent", "--format=%H%x1f%s%x1f%b%x1e", span)
    entries = []
    for record in filter(None, (r.strip("\n") for r in out.split("\x1e"))):
        sha, subject, body = (record.split("\x1f") + ["", ""])[:3]
        pr = re.match(r"^Merge pull request #(\d+)\b", subject)
        if pr:
            title, _, rest = body.strip().partition("\n")
            entries.append({"number": int(pr[1]), "title": title.strip() or subject, "body": rest, "sha": sha})
        elif not subject.startswith("Merge "):
            entries.append({"number": None, "title": subject, "body": body, "sha": sha})
    return entries


def github_prs() -> dict[int, dict[str, Any]]:
    """Merged PRs' real titles and bodies in one gh call; empty when gh cannot answer (the commit text is the fallback)."""
    try:
        listed = harness.gh_json("pr", "list", "--repo", harness.github_repo(), "--state", "merged", "--limit", "1000",
                                 "--json", "number,title,body,url")
    except (ValueError, OSError, subprocess.CalledProcessError):
        return {}
    return {pr["number"]: pr for pr in listed}


def list_prs(repo: Path, since: str | None, rev: str, lookup: dict[int, dict[str, Any]] | None = None) -> list[dict[str, Any]]:
    lookup = github_prs() if lookup is None else lookup
    try:
        base_url = f"https://github.com/{harness.github_repo()}"
    except (ValueError, subprocess.CalledProcessError):
        base_url = None
    prs = []
    for entry in merged_entries(repo, since, rev):
        known = lookup.get(entry["number"], {}) if entry["number"] else {}
        title, body = known.get("title", entry["title"]), known.get("body", entry["body"]) or ""
        url = known.get("url") or (f"{base_url}/pull/{entry['number']}" if base_url and entry["number"] else None)
        prs.append({"number": entry["number"], "title": title, "body": body, "url": url, "sha": entry["sha"],
                    "kind": classify(title, body)})
    return prs


# --- Contract detection -------------------------------------------------------------------------

def json_keys(text: str) -> set[str]:
    def walk(value: Any, prefix: str) -> set[str]:
        if isinstance(value, dict):
            return {k for key, item in value.items() for k in [f"{prefix}.{key}"] + sorted(walk(item, f"{prefix}.{key}"))}
        if isinstance(value, list):
            return {k for item in value for k in walk(item, prefix + "[]")}
        return set()
    try:
        return walk(json.loads(text), "")
    except ValueError:
        return set()


def go_surface(path: str, text: str) -> dict[str, str]:
    """What a Go file says about the contract: key -> value. JSON tags, flags, commands and listed constants."""
    surface: dict[str, str] = {}
    if path.endswith(".go") and not path.endswith("_test.go"):
        if path.startswith(JSON_SOURCES):
            struct = None
            for line in text.split("\n"):
                if m := re.match(r"^type (\w+) struct \{", line):
                    struct = m[1]
                elif line.startswith("}"):
                    struct = None
                elif struct and (m := re.search(r'json:"([^",]+)', line)) and m[1] != "-":
                    surface[f"json {struct}.{m[1]}"] = ""
        if path.startswith(CLI_SOURCES):
            flagset, switch_indent = "?", None
            for line in text.split("\n"):
                if m := re.search(r'flag\.NewFlagSet\("([^"]+)"', line):
                    flagset = m[1]
                for m in GO_FLAG.finditer(line):
                    surface[f"flag {path}:{flagset} --{m[1] or m[2]}"] = ""
                if m := re.match(r"^(\t*)switch (?:command|args\[0\]) \{", line):
                    switch_indent = m[1]
                elif switch_indent is not None and line == switch_indent + "}":
                    switch_indent = None
                elif switch_indent is not None and (m := re.match(r"^\s*case ((?:\"[^\"]*\"(?:, )?)+):", line)):
                    for word in re.findall(r'"([a-z][a-z-]*)"', m[1]):
                        surface[f"command {path}: {word}"] = ""
        for prefix, pattern in CONSTANT_SOURCES:
            if path.startswith(prefix):
                for line in text.split("\n"):
                    if m := pattern.match(line):
                        surface[f"constant {m[1]}"] = m[2]
    return surface


def detect_contract(repo: Path, base: str | None, head: str) -> list[dict[str, str]]:
    """Contract changes between two revisions, by static comparison: [{"kind": "breaking" | "additive", "text": ...}].
    It sees new or changed migrations, JSON keys, flags, commands and design/method/exit constants; it cannot see a
    changed meaning or exit code, which stay declared by a PR."""
    if base is None:
        return []
    changes: list[dict[str, str]] = []

    def add(kind: str, text: str) -> None:
        changes.append({"kind": kind, "text": text})

    status = git(repo, "diff", "--no-renames", "--name-status", base, head)
    for line in filter(None, status.split("\n")):
        flag, _, path = line.partition("\t")
        old, new = show(repo, base, path), show(repo, head, path)
        if path.startswith(MIGRATIONS):
            if flag == "A":
                add("additive", f"new migration {path.removeprefix(MIGRATIONS)} (applied automatically, forward-only)")
            else:
                add("breaking", f"{'deleted' if flag == 'D' else 'changed'} migration {path.removeprefix(MIGRATIONS)}")
        elif path.endswith(".json") and "/testdata/" in path and path.startswith(("internal/cli/", "internal/report/")):
            before, after = json_keys(old), json_keys(new)
            for key in sorted(before - after):
                add("breaking", f"JSON key {key} removed ({path})")
            for key in sorted(after - before) if flag != "A" else []:
                add("additive", f"JSON key {key} added ({path})")
        elif path.endswith(".go"):
            before, after = go_surface(path, old), go_surface(path, new)
            for key in sorted(before):
                if key not in after:
                    add("breaking", f"{key} removed")
                elif before[key] != after[key]:
                    kind = "additive" if key == "constant MethodVersion" else "breaking"
                    add(kind, f"{key} changed: {before[key]} -> {after[key]}")
            for key in sorted(set(after) - set(before)):
                add("additive", f"{key} added")
    return changes


# --- The plan -----------------------------------------------------------------------------------

def make_plan(repo: Path, rev: str = "HEAD", lookup: dict[int, dict[str, Any]] | None = None) -> dict[str, Any]:
    tag = last_tag(repo, rev)
    prs = list_prs(repo, tag, rev, lookup)
    changes = detect_contract(repo, tag, rev)
    declared = max([{"breaking": 3, "feature": 2, "fix": 1}.get(pr["kind"], 0) for pr in prs], default=0)
    detected = 3 if any(c["kind"] == "breaking" for c in changes) else 2 if changes else 0
    warnings = []
    if detected == 3 and declared < 3:
        warnings.append("breaking-looking contract changes that no PR declares (`!` and a `Breaking:` line); counted as breaking")
    level = max(declared, detected)
    if tag is None and prs:
        level = max(level, 1)  # the first release covers everything so far
    version = next_version(parse_tag(tag) if tag else None, level)
    bump = {v: k for k, v in LEVELS.items()}[level]
    plan = {"last_tag": tag, "next_version": fmt(version) if version else None, "due": version is not None, "bump": bump,
            "declared": {v: k for k, v in LEVELS.items()}[declared], "detected": {v: k for k, v in LEVELS.items()}[detected],
            "prs": prs, "contract_changes": changes, "warnings": warnings, "first": tag is None}
    plan["notes"] = notes(plan)
    return plan


def notes(plan: dict[str, Any]) -> str:
    def item(pr: dict[str, Any]) -> str:
        ref = f" ([#{pr['number']}]({pr['url']}))" if pr["number"] and pr["url"] else f" (#{pr['number']})" if pr["number"] else ""
        return f"- {pr['title']}{ref}"
    out = []
    for heading, kind in (("Breaking", "breaking"), ("Features", "feature"), ("Fixes", "fix")):
        rows = [pr for pr in plan["prs"] if pr["kind"] == kind]
        if rows:
            out += [f"## {heading}", *map(item, rows), ""]
    if plan["contract_changes"]:
        out += ["## Contract changes", *(f"- {c['kind']}: {c['text']}" for c in plan["contract_changes"]), ""]
    other = sum(pr["kind"] == "other" for pr in plan["prs"])
    if other:
        out.append(f"{other} other change(s) (docs, tests, refactors, chores) are not listed.")
    return "\n".join(out).strip() + "\n"


def print_plan(plan: dict[str, Any]) -> None:
    last = plan["last_tag"] or "none (no release yet)"
    counts = {kind: sum(pr["kind"] == kind for pr in plan["prs"]) for kind in ("breaking", "feature", "fix", "other")}
    print(f"Last release: {last}\nMerged PRs since: {len(plan['prs'])} "
          f"({counts['breaking']} breaking, {counts['feature']} features, {counts['fix']} fixes, {counts['other']} other)")
    for warning in plan["warnings"]:
        print(f"WARNING: {warning}")
    if not plan["due"]:
        print("Nothing releasable: no feature, fix or contract change since the last release.")
        return
    print(f"Bump: {plan['bump']} (declared {plan['declared']}, detected {plan['detected']})")
    print(f"Next version: {plan['next_version']}" + (" (first release: needs `release cut --first`)" if plan["first"] else ""))
    print("\nDraft release notes\n-------------------\n" + plan["notes"], end="")


def release_plan(args: list[str]) -> None:
    if set(args) - {"--json"}:
        raise ValueError("Usage: release plan [--json]")
    plan = make_plan(harness.ROOT)
    if "--json" in args:
        print(json.dumps(plan, indent=2))
    else:
        print_plan(plan)


# --- Cutting ------------------------------------------------------------------------------------

CUT_USAGE = "Usage: release cut [--dry-run] [--first] [--local-checks]"
CHECK_POLL_SECONDS = 30
ACTIONS_NOTE = ("GitHub Actions currently runs no jobs (the account's minutes are likely exhausted), so the checks run "
                "here instead of waiting for CI.")


def local_checks() -> list[list[str]]:
    script = str(harness.ROOT / "scripts" / "harness.py")
    commands = [[sys.executable, script, "check", "ci"], [sys.executable, script, "check", "vuln"]]
    if sys.platform == "darwin":
        commands.append([str(harness.go_binary()), "test", "-race", "-count=1", "./internal/sandbox", "./internal/run"])
    return commands


def commit_ci(repo_name: str, sha: str) -> tuple[str, str]:
    """('pending' | 'success' | 'failure', detail) of the CI workflow on one commit of the default branch (push runs)."""
    runs = harness.gh_json("api", f"repos/{repo_name}/actions/runs?head_sha={sha}&per_page=100").get("workflow_runs", [])
    return harness.ci_state([r for r in runs if r.get("name") == harness.CI_WORKFLOW and r.get("head_sha") == sha])


def sync_tags() -> None:
    """Best effort: fetch the remote's tags, so a release made elsewhere is seen."""
    for remote in harness.git_output("remote").split():
        subprocess.run(["git", "fetch", "--quiet", "--tags", remote], cwd=harness.ROOT, capture_output=True)
        return


def release_cut(dry_run: bool = False, first: bool = False, use_local_checks: bool = False, timeout_minutes: float = 40,
                sleep: Callable[[float], None] = time.sleep, clock: Callable[[], float] = time.monotonic) -> str:
    """Tag and release the planned version; returns the tag. Every refusal is a ValueError and changes nothing."""
    root = harness.ROOT
    if git(root, "status", "--porcelain").strip():
        raise ValueError("Refusing to release: the working tree is not clean.")
    ref, fetched = harness.fetch_default()
    if not fetched:
        raise ValueError(f"Refusing to release: could not fetch {ref}, so HEAD cannot be proven current.")
    head = git(root, "rev-parse", "HEAD").strip()
    if head != git(root, "rev-parse", ref).strip():
        raise ValueError(f"Refusing to release: HEAD is not the freshly fetched {ref}; check out the default branch and "
                         "fast-forward it first.")
    sync_tags()
    plan = make_plan(root, "HEAD")
    if not plan["due"]:
        raise ValueError("Nothing to release: no feature, fix or contract change since the last release.")
    tag = plan["next_version"]
    if git(root, "tag", "--list", tag).strip():
        raise ValueError(f"Refusing to release: tag {tag} already exists.")
    if plan["first"] and not first:
        raise ValueError(f"The first release ({tag}) must be cut explicitly with --first.\n{FIRST_CHECKLIST}")
    if first and not plan["first"]:
        raise ValueError(f"--first is only for the first release; the last one is {plan['last_tag']}.")
    prefix = "dry run: " if dry_run else ""
    print_plan(plan)
    if plan["first"]:
        print(f"\n{FIRST_CHECKLIST}")
    repo_name = harness.github_repo()
    if use_local_checks:
        print(f"\n{prefix}{ACTIONS_NOTE}")
        for command in local_checks():
            print(f"{prefix}{'would run' if dry_run else 'running'}: {' '.join(command)}")
            if not dry_run:
                try:
                    harness.run(*command)
                except subprocess.CalledProcessError as error:
                    raise ValueError(f"Refusing to release: `{' '.join(command[-3:])}` failed (exit {error.returncode}).") from None
    elif dry_run:
        state, detail = commit_ci(repo_name, head)
        print(f"\ndry run: CI on {head[:12]}: {state} ({detail}); a real cut waits up to {timeout_minutes:g} min for success.")
        if state == "failure":
            raise ValueError(f"Refusing to release: {detail}")
    else:
        deadline = clock() + timeout_minutes * 60
        while True:
            state, detail = commit_ci(repo_name, head)
            if state != "pending":
                break
            if clock() >= deadline:
                raise ValueError(f"Refusing to release: CI on {head[:12]} is still pending after {timeout_minutes:g} min "
                                 f"({detail}). If Actions runs no jobs, use --local-checks.")
            sleep(CHECK_POLL_SECONDS)
        if state != "success":
            raise ValueError(f"Refusing to release: {detail}")
        print(f"\nCI is green on {head[:12]}: {detail}")
    push_url = f"https://github.com/{repo_name}.git"
    steps = [f"git tag -a {tag} -F <notes> {head[:12]}", f"git push {push_url} refs/tags/{tag}",
             f"gh release create {tag} --repo {repo_name} --title {tag} --notes-file <notes> --verify-tag"]
    if dry_run:
        print("\ndry run: would run\n  " + "\n  ".join(steps) + "\nNothing was tagged, pushed or created.")
        return tag
    with tempfile.NamedTemporaryFile("w", suffix=".md", delete=False) as handle:
        handle.write(plan["notes"])
    try:
        subprocess.run(["git", "-C", str(root), "tag", "-a", tag, "-F", handle.name, head], check=True, capture_output=True)
        print(f"Created tag {tag}")
        subprocess.run(["git", "-C", str(root), "push", push_url, f"refs/tags/{tag}"], check=True, capture_output=True)
        print(f"Pushed {tag}")
        url = harness.gh("release", "create", tag, "--repo", repo_name, "--title", tag, "--notes-file", handle.name,
                         "--verify-tag").strip()
    except subprocess.CalledProcessError as error:
        raise ValueError(f"Release {tag} stopped at `{' '.join(map(str, error.cmd[:4]))}`: "
                         f"{(error.stderr or b'').decode(errors='replace').strip()} (the tag may exist locally; "
                         "fix the cause and rerun, or delete it with `git tag -d`).") from None
    finally:
        Path(handle.name).unlink(missing_ok=True)
    print(f"Released {tag}: {url}")
    return tag


def cut_command(args: list[str]) -> None:
    flags = {"--dry-run", "--first", "--local-checks"}
    if set(args) - flags:
        raise ValueError(CUT_USAGE)
    release_cut(dry_run="--dry-run" in args, first="--first" in args, use_local_checks="--local-checks" in args)


def release_command(args: list[str]) -> None:
    action, *rest = args or [""]
    if action == "plan":
        release_plan(rest)
    elif action == "cut":
        cut_command(rest)
    else:
        raise ValueError("Usage: release plan [--json] | release cut [--dry-run] [--first] [--local-checks]")


# --- Hooks for pr land --------------------------------------------------------------------------

def pr_contract_changes(repo_name: str, pr: dict[str, Any], tip: str) -> list[dict[str, str]]:
    """Contract changes a PR's head makes against the base tip it contains. Fetches the PR head (read-only)."""
    root, number, sha = harness.ROOT, pr["number"], pr["headRefOid"]
    remote = harness.remote_default().partition("/")[0]
    ref = f"refs/pull/{number}/head"
    fetched = subprocess.run(["git", "fetch", "--quiet", remote, ref], cwd=root, capture_output=True).returncode == 0
    fallback = harness.https_url(harness.git_output("remote", "get-url", remote).strip()) or f"https://github.com/{repo_name}"
    if not fetched and subprocess.run(["git", "fetch", "--quiet", fallback, ref], cwd=root, capture_output=True).returncode:
        raise ValueError(f"Could not fetch the head of PR #{number} to look for contract changes.")
    if subprocess.run(["git", "cat-file", "-e", tip], cwd=root, capture_output=True).returncode:
        harness.fetch_default()
    return detect_contract(root, tip, sha)


def contract_refusal(pr: dict[str, Any], changes: list[dict[str, str]]) -> str | None:
    """Why a PR's detected contract changes are not declared, or None. Breaking needs `!` and a Breaking: line; additive
    needs a feat, fix or breaking declaration."""
    if not changes:
        return None
    title, body = pr.get("title", ""), pr.get("body", "")
    kind = classify(title, body)
    match = CONVENTIONAL.match(title.strip())
    breaking = [c for c in changes if c["kind"] == "breaking"]
    problem = None
    if breaking and not (match and match[2] and BREAKING_LINE.search(body or "")):
        problem = "breaking changes need a `!` in the title (`feat(cli)!: ...`) and a `Breaking:` line in the body saying what users must do"
    elif not breaking and kind == "other":
        problem = "contract changes need a feat or fix title (`docs`, `test`, `chore`, `ci` and `refactor` do not release)"
    if problem is None:
        return None
    listed = "\n".join(f"  - {c['kind']}: {c['text']}" for c in changes)
    return f"PR #{pr.get('number')} changes the contract without declaring it ({problem}). Detected:\n{listed}"


def after_merge(timeout_minutes: float = 40) -> None:
    """After a merge: plan, and cut when a release is due and one already exists (never the first). Failures raise
    ValueError; the caller reports them without undoing the merge."""
    root = harness.ROOT
    ref, fetched = harness.fetch_default()
    default = ref.partition("/")[2]
    branch = git(root, "branch", "--show-current").strip()
    if fetched and branch == default and not git(root, "status", "--porcelain").strip():
        subprocess.run(["git", "-C", str(root), "merge", "--quiet", "--ff-only", ref], capture_output=True)  # sync, never a commit
    sync_tags()
    plan = make_plan(root, ref if fetched else "HEAD")
    if not plan["due"]:
        print("[harness] release: nothing releasable yet.")
    elif plan["first"]:
        print(f"[harness] release: {plan['next_version']} is due but the first release is cut explicitly: release cut --first")
    else:
        print(f"[harness] release: {plan['next_version']} is due ({plan['bump']}); cutting it")
        release_cut(timeout_minutes=timeout_minutes)
