"""Hidden acceptance test: worktree list."""
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import harness


class HiddenWorktreeList(unittest.TestCase):
    def git(self, *args, cwd=None):
        return subprocess.run(["git", "-c", "user.name=T", "-c", "user.email=t@example.com", *args],
                              cwd=cwd or self.primary, check=True, capture_output=True, text=True).stdout

    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve()
        self.primary = self.base / "repo"
        subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(self.base / "remote.git")], check=True)
        subprocess.run(["git", "init", "-q", "-b", "main", str(self.primary)], check=True)
        (self.primary / "a.txt").write_text("a\n")
        self.git("add", "a.txt")
        self.git("commit", "-q", "-m", "initial")
        self.git("remote", "add", "origin", str(self.base / "remote.git"))
        self.git("push", "-q", "origin", "main")
        self.git("remote", "set-head", "origin", "main")
        self.done = self.base / "wt" / "done"
        self.wip = self.base / "wt" / "wip"
        self.git("worktree", "add", "-q", "-b", "claude/fix/done", str(self.done), "main")
        (self.done / "b.txt").write_text("b\n")
        self.git("add", "b.txt", cwd=self.done)
        self.git("commit", "-q", "-m", "done", cwd=self.done)
        self.git("merge", "-q", "--ff-only", "claude/fix/done")
        self.git("push", "-q", "origin", "main")
        self.git("worktree", "add", "-q", "-b", "codex/feat/wip", str(self.wip), "main")
        (self.wip / "c.txt").write_text("c\n")
        self.git("add", "c.txt", cwd=self.wip)
        self.git("commit", "-q", "-m", "wip", cwd=self.wip)
        patcher = patch.object(harness, "ROOT", self.primary)
        patcher.start()
        self.addCleanup(patcher.stop)

    def listed(self):
        with patch.object(harness, "fetch_default", side_effect=AssertionError("worktree list must not fetch")), \
                patch("builtins.print") as output:
            harness.main(["worktree", "list"])
        return [str(call.args[0]) for call in output.call_args_list if call.args and str(call.args[0]).strip()]

    def test_lines_state_and_order(self):
        lines = self.listed()
        self.assertEqual(len(lines), 2, lines)
        parsed = [line.split("  ") for line in lines]
        self.assertEqual([(branch, state) for branch, state, _ in parsed], [("claude/fix/done", "merged"), ("codex/feat/wip", "open")])
        self.assertEqual([Path(path).resolve() for _, _, path in parsed], [self.done, self.wip])

    def test_help_mentions_the_command(self):
        with patch("builtins.print") as output:
            harness.main(["help"])
        self.assertIn("worktree list", "".join(str(call.args[0]) for call in output.call_args_list))
