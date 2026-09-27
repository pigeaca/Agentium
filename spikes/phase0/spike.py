#!/usr/bin/env python3
"""Phase 0 spike: context A/B on Agentium harness tasks with Claude Code (standard library only).

Throwaway research code, not product code. Each run clones a bare repository that holds only the base commit, so git
never exposes the hidden tests or reference patches. The spike sources, other runs' records and verification copies
are unreadable to runs. Claude Code runs headless with sandboxed Bash, no network, project settings only and no
account connectors. There are two auth modes:
- token mode: a `claude setup-token` token in TOKEN_FILE, and a fresh config directory per run. The token is never
  printed, and the sandbox strips it from the agent's shell;
- login mode: no token file. Runs use the user's own config directory with project settings only. The committed
  results were produced in this mode.
See README.md.
"""
from __future__ import annotations

import argparse
import concurrent.futures
import json
import math
import os
from pathlib import Path
import random
import re
import shutil
import signal
import subprocess
import sys
import threading
import time

import stats

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
BASE = "432b60404f1dc7de92de0748e0312bd3d2f7fab7"
TASKS = ("branch-types", "sensitive-files", "secret-patterns", "markdown-links", "review-metric", "worktree-list")
ARMS = {"full": None, "minimal": HERE / "variants/minimal"}  # None: the base commit's own context
MODEL, EFFORT = "claude-sonnet-5", "high"
CLAUDE = os.environ.get("SPIKE_CLAUDE", str(Path.home() / "Library/Application Support/Claude/claude-code/2.1.281/claude.app/Contents/MacOS/claude"))
CODEX = os.environ.get("SPIKE_CODEX", "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex")
TOKEN_FILE = Path(os.environ.get("SPIKE_TOKEN_FILE", Path.home() / ".config/agentium/claude-oauth-token"))
RUN_TIMEOUT, TEST_TIMEOUT, PER_RUN_BUDGET = 20 * 60, 300, "3"


def hidden_paths(work: Path, token: str | None) -> list[Path]:
    """Paths no run may read, through either the sandboxed shell or the Read tool:
    - the spike sources, the repository and its git data;
    - other runs' records, and the verification, validation, probe and Codex copies;
    - in login mode, the user's Claude session transcripts, which can contain this spike's authoring.
    Sibling runs' in-progress checkouts under work/runs stay readable: a known gap (see the results doc)."""
    common = Path(subprocess.run(["git", "rev-parse", "--path-format=absolute", "--git-common-dir"], cwd=REPO,
                                 capture_output=True, text=True, check=True).stdout.strip())
    paths = [HERE, REPO, common, common.parent, work / "records", work / "verify", work / "validate", work / "probe", work / "codex"]
    if not token:
        paths.append(Path.home() / ".claude/projects")
    runs = (work / "runs").resolve()
    resolved = sorted({path.resolve() for path in paths})
    for path in resolved:
        if path == runs or path in runs.parents:
            raise SystemExit(f"--work {work} lies inside the hidden path {path}; use a dedicated directory such as ~/.cache/agentium-spike.")
    return resolved


def settings(hidden: list[Path]) -> dict:
    """Per-run Claude Code settings. The `hidden` paths are unreadable to both the sandboxed shell and the Read tool."""
    return {
        "sandbox": {
            "enabled": True, "failIfUnavailable": True, "allowUnsandboxedCommands": False, "autoAllowBashIfSandboxed": True,
            "network": {"strictAllowlist": True, "allowedDomains": []},
            "filesystem": {"denyRead": [str(path) for path in hidden]},
            "credentials": {"envVars": [{"name": "CLAUDE_CODE_OAUTH_TOKEN", "mode": "deny"}],
                            "files": [{"path": str(TOKEN_FILE.parent), "mode": "deny"}, {"path": "~/.codex", "mode": "deny"},
                                      {"path": "~/.ssh", "mode": "deny"}]},
        },
        "permissions": {"deny": [f"Read(/{path}/**)" for path in hidden]},
        "autoMemoryEnabled": False,
        "disableClaudeAiConnectors": True,  # a claude.ai login otherwise adds account connectors (e.g. document tools)
        # Both arms have a CLAUDE.md, so AGENTS.md would not auto-load anyway; pinning it keeps that true across versions.
        "pluginConfigs": {"agents-md@builtin": {"options": {"instructionFiles": "claude-md"}}},
    }
# Outward-facing, scheduling and worktree tools, plus the account-dependent tools that appear only with the user's own
# config directory (login mode), so both auth modes offer the agent the same tool set.
DISALLOWED = ("WebSearch,WebFetch,Artifact,DesignSync,CronCreate,CronDelete,ScheduleWakeup,Workflow,SendMessage,EnterWorktree,"
              "ExitWorktree,ArtifactComments,ArtifactData,Monitor,PushNotification,RemoteTrigger")
SUFFIX = ("\n\nYou are working in this task's own checkout of the repository. Make the change here, in the working tree. "
          "Do not commit, push, open a pull request, or create branches or worktrees. When you are done, reply with a short "
          "summary of what you changed and how you verified it.")
GIT = ["git", "-c", "user.name=spike", "-c", "user.email=spike@example.invalid", "-c", "commit.gpgsign=false"]
# Result text of runs that never reached the task: login, plan limits, overload, transport. Agent outcomes never match.
INFRA_TEXT = re.compile(r"usage limit|rate limit|overloaded|authenticat|not logged in|oauth|credit balance|ECONN|socket|5\d\d \w", re.I)
CLEAN_ENV = {k: v for k, v in os.environ.items()
             if k not in {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"}}


def git(*args: str, cwd: Path) -> str:
    return subprocess.run([*GIT, *args], cwd=cwd, check=True, capture_output=True, text=True).stdout


def base_repo(work: Path) -> Path:
    """Bare repository holding only BASE's history, so runs cannot reach spike files through git."""
    bare = work / "base.git"
    if not (bare / "HEAD").exists():
        subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(bare)], check=True)
        subprocess.run(["git", "push", "-q", str(bare), f"{BASE}:refs/heads/main"], cwd=REPO, check=True)
    return bare


def prepare(dest: Path, arm: str, bare: Path) -> str:
    """Clone BASE, apply the arm's context overlay as its own commit, and return that commit."""
    if dest.exists():
        shutil.rmtree(dest)
    dest.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "clone", "-q", str(bare), str(dest)], check=True, capture_output=True)
    git("remote", "remove", "origin", cwd=dest)
    overlay = ARMS[arm]
    for source in sorted(overlay.iterdir()) if overlay else ():
        shutil.copyfile(source, dest / source.name)
    git("add", "-A", cwd=dest)
    git("commit", "-q", "--allow-empty", "-m", f"spike context: {arm}", cwd=dest)
    return git("rev-parse", "HEAD", cwd=dest).strip()


def read_token() -> str | None:
    """Token mode when the token file exists (fresh config dir per run); otherwise login mode (the user's own config
    directory, restricted to project settings). The token is never printed."""
    if not TOKEN_FILE.exists():
        return None
    token = TOKEN_FILE.read_text().strip()
    if TOKEN_FILE.stat().st_mode & 0o077:
        raise SystemExit(f"{TOKEN_FILE} is readable by other users; run chmod 600 on it.")
    if not token or len(token.split()) != 1:
        raise SystemExit(f"{TOKEN_FILE} must contain only the token.")
    return token


def claude(prompt: str, cwd: Path, config: Path, token: str | None, transcript: Path, model: str = MODEL,
           setting_sources: str | None = "project", timeout: int = RUN_TIMEOUT, hidden: tuple[Path, ...] = ()) -> tuple[int, bool]:
    """Run Claude Code headless, streaming JSON events to transcript. Returns (exit code, timed out)."""
    # Not CLAUDE_CODE_SUBPROCESS_ENV_SCRUB: it forces the default permission mode, so acceptEdits would be ignored.
    # The sandbox's credentials rule strips the token from the agent's shell instead.
    env = {**CLEAN_ENV, "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1", "DISABLE_AUTOUPDATER": "1", "ENABLE_CLAUDEAI_MCP_SERVERS": "false"}
    if token:
        config.mkdir(parents=True, exist_ok=True)
        env.update(CLAUDE_CONFIG_DIR=str(config), CLAUDE_CODE_OAUTH_TOKEN=token)
    args = [CLAUDE, "-p", prompt, "--model", model, "--effort", EFFORT, "--output-format", "stream-json", "--verbose",
            "--permission-mode", "acceptEdits", "--permission-prompts", "none", "--no-session-persistence", "--strict-mcp-config",
            "--max-budget-usd", PER_RUN_BUDGET, "--disallowedTools", DISALLOWED, "--settings", json.dumps(settings(list(hidden)))]
    if setting_sources is not None:
        args += ["--setting-sources", setting_sources]
    with transcript.open("w") as out, (transcript.parent / "stderr.txt").open("w") as err:
        proc = subprocess.Popen(args, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=out, stderr=err, start_new_session=True)
        try:
            return proc.wait(timeout=timeout), False
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGINT)  # SIGINT lets Claude Code finish the turn and emit a result
            try:
                proc.wait(timeout=30)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
            return proc.returncode, True


# Output of the harness's own docs check when CLAUDE.md does not import every entry doc (the minimal arm, by design).
IMPORT_RULE_FAILURE = "must explicitly import each shared entrypoint"
FILE_TOOLS = {"Read", "Edit", "Write", "NotebookEdit"}


def trajectory(commands: dict[str, str], outputs: dict[str, str]) -> dict:
    """Behavior flags derived from Bash commands (by tool_use id) and their outputs."""
    text = list(commands.values())
    checks = {i for i, c in commands.items() if re.search(r"harness\.py\s+check", c)}
    return {
        "bash_commands": len(text),
        "ran_unittest": any("unittest" in c for c in text),
        "ran_harness_check": bool(checks),
        "git_stash": any("git stash" in c for c in text),
        # A commit of the task's own checkout: no directory change and no temporary repository in the command.
        "git_commit_in_checkout": any("git commit" in c and not re.search(r"\bcd\b|git -C|mktemp|TMPDIR|/tmp/", c) for c in text),
        "saw_import_rule_failure": any(IMPORT_RULE_FAILURE in outputs.get(i, "") for i in checks),
    }


def parse_claude(text: str) -> dict:
    """Metrics from a stream-json transcript. Totals come from the result event, which includes subagents."""
    init, result, first, tools, retries = {}, {}, None, {}, 0
    seen: set[str] = set()
    commands: dict[str, str] = {}
    outputs: dict[str, str] = {}
    file_paths: list[str] = []
    for line in text.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        kind, subtype = event.get("type"), event.get("subtype")
        if kind == "system" and subtype == "init":
            init = event
        elif kind == "system" and subtype == "api_retry":
            retries += 1
        elif kind == "assistant":
            message = event.get("message") or {}
            if first is None and not event.get("parent_tool_use_id"):
                # Input and cache counts are exact per step (output is a placeholder): the first request's context size.
                usage = message.get("usage") or {}
                first = sum(usage.get(key) or 0 for key in ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"))
            for block in message.get("content") or []:
                if isinstance(block, dict) and block.get("type") == "tool_use" and block.get("id") not in seen:
                    seen.add(block["id"])
                    name, arguments = block.get("name", "?"), block.get("input") or {}
                    tools[name] = tools.get(name, 0) + 1
                    if name == "Bash":
                        commands[block["id"]] = str(arguments.get("command", ""))
                    elif name in FILE_TOOLS and arguments.get("file_path"):
                        file_paths.append(str(arguments["file_path"]))
        elif kind == "user":
            for block in (event.get("message") or {}).get("content") or []:
                if isinstance(block, dict) and block.get("type") == "tool_result" and block.get("tool_use_id") in commands:
                    content = block.get("content")
                    outputs[block["tool_use_id"]] = content if isinstance(content, str) else json.dumps(content)
        elif kind == "result":
            result = event
    per_model = (result.get("modelUsage") or {}).values()

    def total(key: str) -> int | None:
        return sum(entry.get(key) or 0 for entry in per_model) if per_model else None

    return {
        "cli_version": init.get("claude_code_version"), "model": init.get("model"),
        "permission_mode": init.get("permissionMode"),
        "tools_available": len(init.get("tools") or []), "skills_available": len(init.get("skills") or []),
        "tools": sorted(init.get("tools") or []), "skills": sorted(init.get("skills") or []),
        "cost_usd": result.get("total_cost_usd"), "turns": result.get("num_turns"),
        "duration_ms": result.get("duration_ms"), "api_ms": result.get("duration_api_ms"),
        "input_tokens": total("inputTokens"), "output_tokens": total("outputTokens"),
        "cache_read_tokens": total("cacheReadInputTokens"), "cache_write_tokens": total("cacheCreationInputTokens"),
        "first_request_tokens": first, "tool_calls": sum(tools.values()), "tools_used": tools, "api_retries": retries,
        "permission_denials": len(result.get("permission_denials") or []),
        "result_subtype": result.get("subtype"), "result_is_error": result.get("is_error"),
        "result_excerpt": str(result.get("result") or "")[:300], "file_paths": file_paths,
        **trajectory(commands, outputs),
    }


def watched_roots(work: Path) -> list[Path]:
    """Places a run has no business reading: the work directory (other runs, records), the repository and its git
    data, and the user's Claude and Codex directories. The agent's own temp folders are not watched."""
    common = Path(subprocess.run(["git", "rev-parse", "--path-format=absolute", "--git-common-dir"], cwd=REPO,
                                 capture_output=True, text=True, check=True).stdout.strip())
    return [work.resolve(), REPO, common.parent.resolve(), (Path.home() / ".claude").resolve(), (Path.home() / ".codex").resolve()]


def finalize(metrics: dict, run_root: Path, watched: list[Path]) -> dict:
    """Make parsed metrics safe to commit: drop skill names (they can be personal) and file paths (they are local).
    Keep counts instead: connector tools (must be 0), and file-tool paths inside a watched root but outside the run's
    own directory (must be 0: that would mean a run read another run, the spike sources or the user's config)."""
    metrics = dict(metrics)
    metrics.pop("skills", None)
    paths = [Path(os.path.realpath(p)) for p in metrics.pop("file_paths", []) if os.path.isabs(p)]
    own = run_root.resolve()

    def inside(path: Path, root: Path) -> bool:
        return path == root or root in path.parents

    metrics["file_tool_paths_watched"] = sum(any(inside(p, root) for root in watched) and not inside(p, own) for p in paths)
    metrics["mcp_tools"] = sum(name.startswith("mcp__") for name in metrics["tools"])
    return metrics


def classify(metrics: dict, timed_out: bool) -> str:
    """ok | capped | timeout are agent outcomes; infra means the run never got a fair attempt."""
    if timed_out:
        return "timeout"
    if metrics["result_subtype"] is None or metrics.get("permission_mode") not in (None, "acceptEdits"):
        return "infra"  # no result, or the harness silently ran with other permissions: not a fair attempt
    if metrics["result_subtype"] in {"error_max_turns", "error_max_budget_usd"}:
        return "capped"
    if metrics["result_subtype"] == "error_during_execution" or (metrics["result_is_error"] and INFRA_TEXT.search(metrics["result_excerpt"])):
        return "infra"
    return "ok"


def unit(checkout: Path, pattern: str) -> dict:
    try:
        proc = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".agents/scripts", "-p", pattern],
                              cwd=checkout, capture_output=True, text=True, timeout=TEST_TIMEOUT, env=CLEAN_ENV)
    except subprocess.TimeoutExpired:
        return {"ok": False, "tests": 0, "timed_out": True}  # a change that hangs the tests is a failure, not a lost run
    ran = re.search(r"Ran (\d+) tests?", proc.stderr)
    return {"ok": proc.returncode == 0, "tests": int(ran[1]) if ran else 0}


def verify(checkout: Path, task: str, context: str, record_dir: Path, scratch: Path) -> dict:
    """Hidden acceptance tests plus the checkout's own harness tests, then the agent's diff against the context commit.
    Tests run on a copy under `scratch` (a hidden path), so the hidden test never appears where another run can read it."""
    copy = scratch / "repo"
    shutil.rmtree(scratch, ignore_errors=True)
    shutil.copytree(checkout, copy, symlinks=True)
    shutil.copyfile(HERE / "tasks" / task / "test_hidden.py", copy / ".agents/scripts/test_hidden.py")
    try:
        hidden, regression = unit(copy, "test_hidden.py"), unit(copy, "test_harness.py")
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    git("add", "-A", cwd=checkout)
    numstat = git("diff", "--cached", "--numstat", context, cwd=checkout)
    files, added, removed = [], 0, 0
    for line in numstat.splitlines():
        plus, minus, path = line.split("\t", 2)
        files.append(path)
        added += int(plus) if plus.isdigit() else 0
        removed += int(minus) if minus.isdigit() else 0
    (record_dir / "agent.diff").write_text(git("diff", "--cached", context, cwd=checkout))
    git("reset", "-q", cwd=checkout)
    return {"success": hidden["ok"] and regression["ok"], "hidden": hidden, "regression": regression,
            "files": files, "lines_added": added, "lines_removed": removed}


def schedule(repeats: int, seed: int) -> list[dict]:
    """Repeat blocks; tasks shuffled per block; the two arms of a task run back to back in random order."""
    rng = random.Random(seed)
    specs = []
    for repeat in range(1, repeats + 1):
        tasks = list(TASKS)
        rng.shuffle(tasks)
        for task in tasks:
            arms = list(ARMS)
            rng.shuffle(arms)
            specs += [{"task": task, "arm": arm, "repeat": repeat, "id": f"{task}.{arm}.r{repeat}"} for arm in arms]
    return specs


def run_one(spec: dict, work: Path, bare: Path, token: str | None) -> dict:
    root, records = work / "runs" / spec["id"], work / "records" / spec["id"]
    for path in (root, records):
        if path.exists():
            shutil.rmtree(path)
    records.mkdir(parents=True)
    checkout = root / "repo"
    context = prepare(checkout, spec["arm"], bare)
    prompt = (HERE / "tasks" / spec["task"] / "instruction.md").read_text().strip() + SUFFIX
    started = time.time()
    code, timed_out = claude(prompt, checkout, root / "config", token, records / "stream.jsonl", hidden=tuple(hidden_paths(work, token)))
    metrics = parse_claude((records / "stream.jsonl").read_text())
    status = classify(metrics, timed_out)
    record = {**spec, "auth": "token" if token else "login", "status": status, "exit_code": code, "wall_s": round(time.time() - started, 1),
              **finalize(metrics, root, watched_roots(work)), "started": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(started))}
    if status != "infra":
        record.update(verify(checkout, spec["task"], context, records, work / "verify" / spec["id"]))
    shutil.rmtree(root, ignore_errors=True)  # checkouts go; transcripts and diffs stay in records/
    return record


def load(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()] if path.exists() else []


def command_run(args) -> None:
    token = read_token()
    work, out = Path(args.work).resolve(), HERE / "results" / args.experiment
    out.mkdir(parents=True, exist_ok=True)
    runs_file = out / "runs.jsonl"
    bare = base_repo(work)
    done = {r["id"]: r for r in load(runs_file)}
    pending = [spec for spec in schedule(args.repeats, args.seed) if done.get(spec["id"], {}).get("status", "infra") == "infra"]
    spent = sum(r.get("cost_usd") or 0 for r in done.values())
    print(f"Auth: {'token, fresh config dir per run' if token else 'login, own config dir with project settings only'}. "
          f"{len(done)} recorded, {len(pending)} to run, ${spent:.2f} estimated so far (cap ${args.cap:.0f}).", flush=True)
    lock, state = threading.Lock(), {"spent": spent, "infra_streak": 0, "stop": None}

    def finished(record: dict) -> None:
        with lock:
            with runs_file.open("a") as handle:
                handle.write(json.dumps(record, sort_keys=True) + "\n")
            state["spent"] += record.get("cost_usd") or 0
            state["infra_streak"] = state["infra_streak"] + 1 if record["status"] == "infra" else 0
            if state["infra_streak"] >= 3:
                state["stop"] = f"3 infrastructure failures in a row (last: {record['result_excerpt'][:120]!r})"
            elif state["spent"] >= args.cap:
                state["stop"] = f"estimated cost cap reached (${state['spent']:.2f})"
            print(f"{record['id']:<32} {record['status']:<8} success={record.get('success')} "
                  f"cost=${record.get('cost_usd') or 0:.2f} turns={record.get('turns')} wall={record['wall_s']}s "
                  f"total=${state['spent']:.2f}", flush=True)

    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        running: set = set()
        queue = list(pending)
        while queue or running:
            while queue and len(running) < args.concurrency and not state["stop"]:
                running.add(pool.submit(run_one, queue.pop(0), work, bare, token))
            if not running:
                break
            complete, running = concurrent.futures.wait(running, return_when=concurrent.futures.FIRST_COMPLETED)
            for future in complete:
                try:
                    finished(future.result())
                except Exception as error:  # noqa: BLE001 - one broken run must not lose the others
                    print(f"run crashed in the runner: {error!r}", file=sys.stderr, flush=True)
            if state["stop"] and queue:
                print(f"Stopping new runs: {state['stop']}. {len(queue)} not started.", flush=True)
                queue.clear()
    print(f"Done. Records in {runs_file}.")


def docs_check(checkout: Path) -> bool:
    return subprocess.run([sys.executable, ".agents/scripts/harness.py", "check", "docs"], cwd=checkout,
                          capture_output=True, env=CLEAN_ENV).returncode == 0


def command_validate(args) -> None:
    """Every task in every arm: hidden tests fail on the arm's context commit and pass with the reference patch; the
    harness tests pass throughout. Also reports whether the repository's own docs check passes in each arm."""
    work = Path(args.work).resolve()
    bare = base_repo(work)
    failures = 0
    for arm in ARMS:
        for task in TASKS:
            checkout = work / "validate" / arm / task / "repo"
            context = prepare(checkout, arm, bare)
            docs_before = docs_check(checkout)
            before = verify(checkout, task, context, checkout.parent, work / "validate" / "scratch")
            git("apply", str(HERE / "tasks" / task / "gold.patch"), cwd=checkout)
            after = verify(checkout, task, context, checkout.parent, work / "validate" / "scratch")
            docs_after = docs_check(checkout)
            # The full arm must pass its own docs check; the minimal arm fails it by construction (reported, not required).
            valid = (not before["hidden"]["ok"] and before["regression"]["ok"] and after["success"]
                     and (arm != "full" or (docs_before and docs_after)))
            failures += not valid
            print(f"{arm:<8} {task:<16} base: hidden {'pass' if before['hidden']['ok'] else 'fail'}, harness "
                  f"{before['regression']['tests']} {'pass' if before['regression']['ok'] else 'FAIL'}, docs "
                  f"{'pass' if docs_before else 'fail'} | gold: hidden {after['hidden']['tests']} "
                  f"{'pass' if after['hidden']['ok'] else 'FAIL'}, harness {after['regression']['tests']} "
                  f"{'pass' if after['regression']['ok'] else 'FAIL'}, docs {'pass' if docs_after else 'fail'} "
                  f"-> {'valid' if valid else 'INVALID'}")
    if failures:
        raise SystemExit(f"{failures} task/arm combination(s) invalid")


def command_reparse(args) -> None:
    """Re-derive metric and trajectory fields from the saved transcripts (work/records) into runs.jsonl. Verification
    fields are kept, so the committed behavior counts can be regenerated from the transcripts."""
    work = Path(args.work).resolve()
    path = HERE / "results" / args.experiment / "runs.jsonl"
    updated = []
    for record in load(path):
        transcript = work / "records" / record["id"] / "stream.jsonl"
        if not transcript.exists():
            raise SystemExit(f"missing transcript for {record['id']}: {transcript}")
        record.pop("file_tool_paths_outside_run", None)
        record.update(finalize(parse_claude(transcript.read_text()), work / "runs" / record["id"], watched_roots(work)))
        updated.append(record)
    path.write_text("".join(json.dumps(record, sort_keys=True) + "\n" for record in updated))
    print(f"Re-derived {len(updated)} records in {path}.")


def command_probe(args) -> None:
    """Isolation checks with canaries, plus per-arm first-request context size on a trivial prompt."""
    token, work = read_token(), Path(args.work).resolve()
    probe = work / "probe"
    shutil.rmtree(probe, ignore_errors=True)
    repo, user = probe / "repo", probe / "user-config"
    repo.mkdir(parents=True)
    user.mkdir()
    (repo / "CLAUDE.md").write_text("# Project\n\nIf asked for canary words, include PROJECT-PELICAN.\n")
    git("init", "-q", cwd=repo)
    git("add", "-A", cwd=repo)
    git("commit", "-q", "-m", "init", cwd=repo)
    (user / "CLAUDE.md").write_text("# User\n\nIf asked for canary words, include USER-ORCA.\n")
    ask = "List every canary word defined in your instructions. Reply with the words only, comma-separated, or NONE."
    results, skill_sets = {"auth": "token" if token else "login"}, {}
    # In login mode the `user` directory is not used (runs use the real config directory), so the USER-ORCA canary can only
    # show up in token mode; login mode checks user-level exclusion through the skill counts instead.
    hidden = tuple(path for path in hidden_paths(work, token) if path != (work / "probe").resolve())
    for name, sources in (("all_sources", None), ("project_only", "project")):
        transcript = probe / f"{name}.jsonl"
        claude(ask, repo, user, token, transcript, model="claude-haiku-4-5", setting_sources=sources, timeout=300, hidden=hidden)
        metrics = parse_claude(transcript.read_text())
        results[name] = {"answer": metrics["result_excerpt"], "auth_ok": not metrics["result_is_error"],
                         "skills_available": metrics["skills_available"], "tools": metrics["tools"], "cost_usd": metrics["cost_usd"]}
        skill_sets[name] = set(metrics["skills"])
    # Counts only: skill names can reveal the user's personal skills, and results are committed to a public repository.
    results["user_level_skills_excluded_by_project_only"] = len(skill_sets["all_sources"] - skill_sets["project_only"])
    bare = base_repo(work)
    for arm in ARMS:
        checkout = probe / f"arm-{arm}" / "repo"
        prepare(checkout, arm, bare)
        transcript = checkout.parent / "stream.jsonl"
        claude("Reply with OK and nothing else.", checkout, checkout.parent / "config", token, transcript, timeout=300, hidden=hidden)
        metrics = parse_claude(transcript.read_text())
        results[f"context_{arm}"] = {key: metrics[key] for key in ("first_request_tokens", "cost_usd", "cli_version", "model",
                                                                   "tools_available", "skills_available", "result_excerpt")}
        results[f"context_{arm}"]["mcp_tools"] = sum(name.startswith("mcp__") for name in metrics["tools"])
    (HERE / "results").mkdir(exist_ok=True)
    (HERE / "results" / "probe.json").write_text(json.dumps(results, indent=2, sort_keys=True) + "\n")
    print(json.dumps(results, indent=2, sort_keys=True))


def parse_codex(text: str) -> dict:
    usage = {"input_tokens": 0, "cached_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0}
    items: dict[str, int] = {}
    turns, failed = 0, 0
    for line in text.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("type") == "turn.completed":
            turns += 1
            for key in usage:
                usage[key] += (event.get("usage") or {}).get(key) or 0
        elif event.get("type") == "turn.failed":
            failed += 1
        elif event.get("type") == "item.completed":
            kind = (event.get("item") or {}).get("type", "?")
            items[kind] = items.get(kind, 0) + 1
    return {"turns": turns, "failed_turns": failed, "usage": usage, "items": items}


def command_codex(args) -> None:
    """One Codex run with the user's normal login (no isolation), to check its JSON stream parses."""
    work = Path(args.work).resolve()
    root = work / "codex"
    checkout = root / "repo"
    context = prepare(checkout, "full", base_repo(work))
    prompt = (HERE / "tasks" / args.task / "instruction.md").read_text().strip() + SUFFIX
    started = time.time()
    proc = subprocess.run([CODEX, "exec", "--json", "--sandbox", "workspace-write", "--ephemeral", "-C", str(checkout), prompt],
                          capture_output=True, text=True, timeout=RUN_TIMEOUT, env=CLEAN_ENV)
    (root / "stream.jsonl").write_text(proc.stdout)
    version = subprocess.run([CODEX, "--version"], capture_output=True, text=True).stdout.strip()
    record = {"task": args.task, "exit_code": proc.returncode, "wall_s": round(time.time() - started, 1), "cli_version": version,
              **parse_codex(proc.stdout), **verify(checkout, args.task, context, root, work / "verify" / "codex")}
    (HERE / "results").mkdir(exist_ok=True)
    (HERE / "results" / "codex-check.json").write_text(json.dumps(record, indent=2, sort_keys=True) + "\n")
    print(json.dumps(record, indent=2, sort_keys=True))


def effects(runs: list[dict], a: str, b: str) -> dict:
    """Paired effects of arm b against arm a. Success is a difference; the others are ratios of geometric means.
    Each has the cluster-bootstrap interval and, as a cross-check, a t-interval on the per-task differences."""
    out = {}
    success = stats.cells(runs, lambda r: float(r["success"]) if r.get("success") is not None else None)
    out["success_diff_b_minus_a"] = {"bootstrap": stats.bootstrap(success, a, b), "t": stats.t_interval(stats.paired(success, a, b))}
    for key in ("cost_usd", "wall_s", "output_tokens", "turns"):
        table = stats.cells(runs, lambda r, key=key: r.get(key) if (r.get(key) or 0) > 0 else None)
        out[f"{key}_geomean_ratio_b_over_a"] = {"bootstrap": tuple(math.exp(x) for x in stats.bootstrap(table, a, b, transform=math.log)),
                                                "t": tuple(math.exp(x) for x in stats.t_interval(stats.paired(table, a, b, math.log)))}
    return {name: {kind: [round(x, 4) for x in values] for kind, values in value.items()} for name, value in out.items()}


def summarize(runs: list[dict]) -> dict:
    fair = [r for r in runs if r["status"] != "infra"]
    arms = sorted({r["arm"] for r in fair})
    summary: dict = {"runs": len(runs), "fair_runs": len(fair), "infra_runs": len(runs) - len(fair), "arms": {}}
    for arm in arms:
        group = [r for r in fair if r["arm"] == arm]

        def avg(key: str, rows=group) -> float | None:
            values = [r[key] for r in rows if r.get(key) is not None]
            return round(sum(values) / len(values), 4) if values else None

        summary["arms"][arm] = {"runs": len(group), "success_rate": avg("success"), "cost_usd": avg("cost_usd"),
                                "wall_s": avg("wall_s"), "turns": avg("turns"), "tool_calls": avg("tool_calls"),
                                "output_tokens": avg("output_tokens"), "cache_read_tokens": avg("cache_read_tokens"),
                                "cache_write_tokens": avg("cache_write_tokens"), "first_request_tokens": avg("first_request_tokens"),
                                "lines_changed": avg("lines_added"), "statuses": {s: sum(r["status"] == s for r in group) for s in {r["status"] for r in group}}}
    for arm in arms:  # behavior flags (see trajectory()) and environment checks, as run counts per arm
        group = [r for r in fair if r["arm"] == arm]
        summary["arms"][arm]["behavior"] = {
            "updated_test_harness": sum(any("test_harness" in f for f in r.get("files", [])) for r in group),
            **{flag: sum(bool(r.get(flag)) for r in group) for flag in
               ("ran_unittest", "ran_harness_check", "git_stash", "git_commit_in_checkout", "saw_import_rule_failure")},
            "permission_denials": sum(r.get("permission_denials") or 0 for r in group),
            "file_tool_paths_watched": sum(r.get("file_tool_paths_watched") or 0 for r in group),
            "mcp_tools": sum(r.get("mcp_tools") or 0 for r in group),
        }
    starts = sorted(r["started"] for r in fair if r.get("started"))
    summary["started_span_utc"] = [starts[0], starts[-1]] if starts else None
    if len(arms) == 2:
        a, b = "full", "minimal"
        summary["effects"] = effects(fair, a, b)
        # Sensitivity: drop task/repeat pairs whose minimal run hit the repository's own docs check failure (a confound
        # that exists only in the minimal arm, because the harness requires CLAUDE.md to import every entry doc).
        confounded = {(r["task"], r["repeat"]) for r in fair if r["arm"] == b and r.get("saw_import_rule_failure")}
        clean = [r for r in fair if (r["task"], r["repeat"]) not in confounded]
        summary["effects_excluding_import_rule_pairs"] = {"excluded_pairs": len(confounded), **effects(clean, a, b)}
        repeats = max(1, round(len(fair) / (2 * len({r["task"] for r in fair}))))
        success = stats.cells(fair, lambda r: float(r["success"]) if r.get("success") is not None else None)
        cost = stats.cells(fair, lambda r: r.get("cost_usd") if (r.get("cost_usd") or 0) > 0 else None)
        sigma2 = stats.within_variance(cost, math.log)
        tau2_cost = stats.heterogeneity(stats.paired(cost, a, b, math.log), sigma2 or 0, repeats)
        w = stats.within_variance(success)
        tau2_success = stats.heterogeneity(stats.paired(success, a, b), w or 0, repeats)
        summary["variance"] = {"sigma_log_cost": round(math.sqrt(sigma2), 4) if sigma2 else None, "tau_log_cost": round(math.sqrt(tau2_cost), 4),
                               "w_success": round(w, 4) if w is not None else None, "tau_success": round(math.sqrt(tau2_success), 4),
                               "repeats": repeats, "tasks": len({r["task"] for r in fair})}
        designs = {}
        for n, r in ((12, 3), (20, 3), (20, 5), (23, 5), (40, 5), (65, 5)):
            designs[f"{n}x{r}"] = {"success_mde_pp": round(100 * stats.mde(tau2_success, w or 0, r, n), 1),
                                   # τ comes from 6 tasks (5 degrees of freedom): also show the study's τ and a high value.
                                   **{f"cost_mde_pct_tau_{label}": round(100 * (1 - math.exp(-stats.mde(tau ** 2, sigma2 or 0, r, n))), 1)
                                      for label, tau in (("measured", math.sqrt(tau2_cost)), ("0.10", 0.10), ("0.25", 0.25))}}
        summary["designs"] = designs
        summary["noninferiority_15pp_tasks_at_5_runs"] = stats.noninferiority_tasks(tau2_success, w or 0, 5, 0.15)
        per_task = {}
        for task in sorted({r["task"] for r in fair}):
            row = {}
            for arm in arms:
                group = [r for r in fair if r["task"] == task and r["arm"] == arm]
                costs = [r["cost_usd"] for r in group if r.get("cost_usd")]
                row[arm] = {"success": f"{sum(bool(r.get('success')) for r in group)}/{len(group)}",
                            "cost_usd": round(sum(costs) / len(costs), 3) if costs else None}
            per_task[task] = row
        summary["per_task"] = per_task
    return summary


def command_report(args) -> None:
    runs = load(HERE / "results" / args.experiment / "runs.jsonl")
    if not runs:
        raise SystemExit("no runs recorded")
    latest = {}
    for run in runs:  # a retried run supersedes its earlier infra record
        latest[run["id"]] = run
    summary = summarize(list(latest.values()))
    (HERE / "results" / args.experiment / "summary.json").write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n")
    print(json.dumps(summary, indent=2, sort_keys=True))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--work", default=os.environ.get("SPIKE_WORK", str(Path.home() / ".cache/agentium-spike")))
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("validate", help="check each task, in each arm, fails on the base and passes with its reference patch")
    sub.add_parser("probe", help="isolation canaries and per-arm context size")
    codex = sub.add_parser("codex", help="one Codex run to check its JSON output")
    codex.add_argument("--task", default="branch-types", choices=TASKS)
    for name in ("run", "report", "reparse"):
        command = sub.add_parser(name)
        command.add_argument("--experiment", default="context-ab")
        if name == "run":
            command.add_argument("--repeats", type=int, default=5)
            command.add_argument("--concurrency", type=int, default=3)
            command.add_argument("--cap", type=float, default=150.0, help="stop starting runs at this estimated USD total")
            command.add_argument("--seed", type=int, default=20260927)
    args = parser.parse_args()
    {"validate": command_validate, "probe": command_probe, "codex": command_codex, "run": command_run, "report": command_report,
     "reparse": command_reparse}[args.command](args)


if __name__ == "__main__":
    main()
