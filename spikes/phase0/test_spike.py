"""Spike unit tests: stream parsers, scheduling, classification and statistics. No model calls, no network."""
import json
import math
from pathlib import Path
import random
import subprocess
import tempfile
import unittest
import unittest.mock

import spike
import stats


def stream(*events):
    return "\n".join(json.dumps(event) for event in events) + "\n"


class ClaudeStream(unittest.TestCase):
    TEXT = stream(
        {"type": "system", "subtype": "init", "permissionMode": "acceptEdits", "claude_code_version": "2.1.281", "model": "claude-sonnet-5", "tools": ["Bash", "Read"], "skills": ["a"]},
        {"type": "assistant", "message": {"id": "m1", "usage": {"input_tokens": 5, "cache_creation_input_tokens": 9000, "cache_read_input_tokens": 1000},
                                          "content": [{"type": "tool_use", "id": "t1", "name": "Read"}, {"type": "tool_use", "id": "t2", "name": "Bash"}]}},
        {"type": "assistant", "message": {"id": "m1", "usage": {"input_tokens": 5}, "content": [{"type": "tool_use", "id": "t2", "name": "Bash"}]}},
        {"type": "assistant", "parent_tool_use_id": "t9", "message": {"id": "s1", "content": [{"type": "tool_use", "id": "t3", "name": "Read"}]}},
        {"type": "system", "subtype": "api_retry", "attempt": 1},
        {"type": "result", "subtype": "success", "is_error": False, "total_cost_usd": 0.42, "num_turns": 7, "duration_ms": 61000,
         "duration_api_ms": 50000, "result": "Done.", "permission_denials": [{"tool_name": "Bash"}],
         "modelUsage": {"claude-sonnet-5": {"inputTokens": 100, "outputTokens": 900, "cacheReadInputTokens": 50000, "cacheCreationInputTokens": 12000},
                        "claude-haiku-4-5": {"inputTokens": 10, "outputTokens": 90, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0}}},
    )

    def test_totals_first_request_and_tool_calls(self):
        metrics = spike.parse_claude(self.TEXT)
        self.assertEqual((metrics["cost_usd"], metrics["turns"], metrics["duration_ms"]), (0.42, 7, 61000))
        self.assertEqual((metrics["input_tokens"], metrics["output_tokens"], metrics["cache_read_tokens"], metrics["cache_write_tokens"]),
                         (110, 990, 50000, 12000))
        self.assertEqual(metrics["first_request_tokens"], 10005)
        self.assertEqual(metrics["tools_used"], {"Read": 2, "Bash": 1})  # duplicates counted once, subagent calls included
        self.assertEqual((metrics["api_retries"], metrics["permission_denials"], metrics["tools_available"]), (1, 1, 2))
        self.assertEqual(spike.classify(metrics, timed_out=False), "ok")

    def test_classification(self):
        base = spike.parse_claude(self.TEXT)
        self.assertEqual(spike.classify({**base, "result_subtype": None}, False), "infra")
        self.assertEqual(spike.classify(base, True), "timeout")
        self.assertEqual(spike.classify({**base, "result_subtype": "error_max_budget_usd"}, False), "capped")
        limited = {**base, "result_is_error": True, "result_excerpt": "Claude AI usage limit reached|1790000000"}
        self.assertEqual(spike.classify(limited, False), "infra")
        self.assertEqual(spike.classify({**base, "permission_mode": "default"}, False), "infra")
        wrong = {**base, "result_is_error": True, "result_excerpt": "I could not make the tests pass."}
        self.assertEqual(spike.classify(wrong, False), "ok")

    def test_empty_stream(self):
        metrics = spike.parse_claude("not json\n")
        self.assertIsNone(metrics["cost_usd"])
        self.assertEqual(spike.classify(metrics, False), "infra")


class Trajectory(unittest.TestCase):
    TEXT = stream(
        {"type": "system", "subtype": "init", "permissionMode": "acceptEdits", "tools": ["Bash", "mcp__x__y"], "skills": ["personal"]},
        {"type": "assistant", "message": {"id": "m1", "content": [
            {"type": "tool_use", "id": "b1", "name": "Bash", "input": {"command": "python3 .agents/scripts/harness.py check docs"}},
            {"type": "tool_use", "id": "b2", "name": "Bash", "input": {"command": "git stash && python3 -m unittest discover"}},
            {"type": "tool_use", "id": "b3", "name": "Bash", "input": {"command": 'cd "$TMP" && git commit -m t'}},
            {"type": "tool_use", "id": "r1", "name": "Read", "input": {"file_path": "/elsewhere/secret.py"}},
            {"type": "tool_use", "id": "r2", "name": "Edit", "input": {"file_path": "/run/root/repo/a.py"}}]}},
        {"type": "user", "message": {"content": [
            {"type": "tool_result", "tool_use_id": "b1", "content": [{"type": "text", "text": "[harness] CLAUDE.md must explicitly import each shared entrypoint once."}]},
            {"type": "tool_result", "tool_use_id": "r1", "content": "must explicitly import each shared entrypoint (source code, not a check)"}]}},
        {"type": "result", "subtype": "success", "is_error": False, "total_cost_usd": 0.1, "num_turns": 2},
    )

    def test_flags(self):
        metrics = spike.parse_claude(self.TEXT)
        self.assertEqual((metrics["bash_commands"], metrics["ran_unittest"], metrics["ran_harness_check"], metrics["git_stash"]), (3, True, True, True))
        self.assertFalse(metrics["git_commit_in_checkout"], "a commit in a temporary repository is not a commit of the task")
        self.assertTrue(metrics["saw_import_rule_failure"])

    def test_import_failure_only_counts_harness_check_output(self):
        text = self.TEXT.replace("python3 .agents/scripts/harness.py check docs", "cat notes.txt")
        self.assertFalse(spike.parse_claude(text)["saw_import_rule_failure"])

    def test_finalize_keeps_counts_not_names_or_paths(self):
        final = spike.finalize(spike.parse_claude(self.TEXT), Path("/run/root"), [Path("/elsewhere"), Path("/run")])
        self.assertNotIn("skills", final)
        self.assertNotIn("file_paths", final)
        self.assertEqual((final["file_tool_paths_watched"], final["mcp_tools"]), (1, 1))  # /elsewhere/secret.py; own root excluded
        unwatched = spike.finalize(spike.parse_claude(self.TEXT), Path("/run/root"), [Path("/other")])
        self.assertEqual(unwatched["file_tool_paths_watched"], 0)


class Verification(unittest.TestCase):
    def test_unit_timeout_is_a_failure_not_an_exception(self):
        with unittest.mock.patch.object(spike.subprocess, "run", side_effect=subprocess.TimeoutExpired("x", 1)):
            self.assertEqual(spike.unit(Path("."), "test_x.py"), {"ok": False, "tests": 0, "timed_out": True})

    def test_hidden_test_never_lands_in_the_checkout(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            checkout = root / "run/repo"
            (checkout / ".agents/scripts").mkdir(parents=True)
            (checkout / ".agents/scripts/harness.py").write_text("X = 1\n")
            (checkout / ".agents/scripts/test_harness.py").write_text("import unittest\n")
            subprocess.run(["git", "init", "-q", str(checkout)], check=True)
            spike.git("add", "-A", cwd=checkout)
            spike.git("commit", "-q", "-m", "base", cwd=checkout)
            context = spike.git("rev-parse", "HEAD", cwd=checkout).strip()
            seen = []
            with unittest.mock.patch.object(spike, "unit", side_effect=lambda repo, pattern: seen.append((repo, pattern, (repo / ".agents/scripts/test_hidden.py").exists())) or {"ok": True, "tests": 1}):
                result = spike.verify(checkout, "branch-types", context, root, root / "verify/x")
            self.assertTrue(all(exists and repo != checkout for repo, _, exists in seen))
            self.assertFalse((checkout / ".agents/scripts/test_hidden.py").exists())
            self.assertFalse((root / "verify/x").exists())
            self.assertEqual(result["files"], [])


class HiddenPaths(unittest.TestCase):
    def test_login_mode_hides_session_transcripts_and_rejects_a_work_dir_inside_hidden_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            paths = spike.hidden_paths(Path(directory) / "work", token=None)
            self.assertIn((Path.home() / ".claude/projects").resolve(), paths)
            self.assertIn(spike.HERE, paths)
            self.assertNotIn((Path.home() / ".claude/projects").resolve(), spike.hidden_paths(Path(directory) / "work", token="t"))
        with self.assertRaises(SystemExit):
            spike.hidden_paths(spike.HERE / "work", token=None)


class CodexStream(unittest.TestCase):
    def test_usage_and_items(self):
        text = stream({"type": "thread.started"}, {"type": "turn.started"},
                      {"type": "item.completed", "item": {"type": "command_execution"}}, {"type": "item.completed", "item": {"type": "file_change"}},
                      {"type": "turn.completed", "usage": {"input_tokens": 1200, "cached_input_tokens": 800, "output_tokens": 300, "reasoning_output_tokens": 120}})
        parsed = spike.parse_codex(text)
        self.assertEqual(parsed["usage"], {"input_tokens": 1200, "cached_input_tokens": 800, "output_tokens": 300, "reasoning_output_tokens": 120})
        self.assertEqual((parsed["turns"], parsed["items"]), (1, {"command_execution": 1, "file_change": 1}))


class Scheduling(unittest.TestCase):
    def test_pairs_are_adjacent_and_every_cell_is_covered(self):
        specs = spike.schedule(repeats=3, seed=1)
        self.assertEqual(len(specs), 3 * len(spike.TASKS) * 2)
        self.assertEqual(len({s["id"] for s in specs}), len(specs))
        for first, second in zip(specs[::2], specs[1::2]):
            self.assertEqual((first["task"], first["repeat"]), (second["task"], second["repeat"]))
            self.assertNotEqual(first["arm"], second["arm"])
        self.assertEqual(specs, spike.schedule(repeats=3, seed=1))
        self.assertGreater(len({s["arm"] for s in specs[::2]}), 1, "arm order within pairs should be randomized")


class Overlay(unittest.TestCase):
    def test_minimal_arm_replaces_entry_files_in_its_own_commit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "source"
            source.mkdir()
            (source / "CLAUDE.md").write_text("full\n")
            (source / "code.py").write_text("x = 1\n")
            subprocess.run(["git", "init", "-q", "-b", "main", str(source)], check=True)
            spike.git("add", "-A", cwd=source)
            spike.git("commit", "-q", "-m", "base", cwd=source)
            bare = root / "base.git"
            subprocess.run(["git", "clone", "-q", "--bare", str(source), str(bare)], check=True)
            context = spike.prepare(root / "run", "minimal", bare)
            self.assertEqual((root / "run/CLAUDE.md").read_text(), (spike.HERE / "variants/minimal/CLAUDE.md").read_text())
            self.assertEqual(spike.git("rev-parse", "HEAD", cwd=root / "run").strip(), context)
            self.assertEqual(spike.git("remote", cwd=root / "run").strip(), "")
            full = spike.prepare(root / "run2", "full", bare)
            self.assertEqual(spike.git("diff", "--name-only", "HEAD~1", full, cwd=root / "run2").strip(), "")


class RunSettings(unittest.TestCase):
    def test_hidden_paths_are_denied_to_shell_and_read_tool(self):
        settings = spike.settings([Path("/work/records"), Path("/src")])
        self.assertEqual(settings["sandbox"]["filesystem"]["denyRead"], ["/work/records", "/src"])
        self.assertEqual(settings["permissions"]["deny"], ["Read(//work/records/**)", "Read(//src/**)"])
        self.assertEqual(settings["sandbox"]["network"], {"strictAllowlist": True, "allowedDomains": []})
        self.assertFalse(settings["sandbox"]["allowUnsandboxedCommands"])
        self.assertIn({"name": "CLAUDE_CODE_OAUTH_TOKEN", "mode": "deny"}, settings["sandbox"]["credentials"]["envVars"])
        self.assertTrue(settings["disableClaudeAiConnectors"])


class Statistics(unittest.TestCase):
    def synthetic(self, effect, tasks=8, repeats=5, seed=3):
        rng = random.Random(seed)
        runs = []
        for task in range(tasks):
            difficulty = rng.uniform(0.5, 3.0)
            for arm, factor in (("full", 1.0), ("minimal", effect)):
                for _ in range(repeats):
                    runs.append({"task": f"t{task}", "arm": arm, "cost": difficulty * factor * math.exp(rng.gauss(0, 0.3))})
        return stats.cells(runs, lambda r: r["cost"])

    def test_ratio_estimate_and_interval_cover_the_true_effect(self):
        table = self.synthetic(effect=0.7)
        estimate, low, high = stats.bootstrap(table, "full", "minimal", transform=math.log, draws=2000)
        self.assertAlmostEqual(math.exp(estimate), 0.7, delta=0.1)
        self.assertLess(math.exp(low), 0.7)
        self.assertGreater(math.exp(high), 0.7)
        self.assertLess(math.exp(high), 1.0)

    def test_variance_components(self):
        table = self.synthetic(effect=1.0, tasks=30, repeats=6)
        sigma2 = stats.within_variance(table, math.log)
        self.assertAlmostEqual(math.sqrt(sigma2), 0.3, delta=0.05)
        tau2 = stats.heterogeneity(stats.paired(table, "full", "minimal", math.log), sigma2, 6)
        self.assertLess(tau2, 0.02)

    def test_t_interval(self):
        center, low, high = stats.t_interval([1.0, 2.0, 3.0])
        self.assertEqual(center, 2.0)
        self.assertAlmostEqual(high - center, 4.303 * 1.0 / math.sqrt(3), places=3)
        self.assertAlmostEqual(center - low, high - center)

    def test_planner_matches_the_study_formula(self):
        # Study section 5.6: w=0.20, tau=0.05, R=5, n=65 -> about 10 pp; non-inferiority at 15 pp -> 23 tasks.
        self.assertAlmostEqual(stats.mde(0.05 ** 2, 0.20, 5, 65), 0.10, delta=0.002)
        self.assertEqual(stats.noninferiority_tasks(0.05 ** 2, 0.20, 5, 0.15), 23)


if __name__ == "__main__":
    unittest.main()
