"""Hidden acceptance test: perf and ci branch types."""
import unittest

import harness


class HiddenBranchTypes(unittest.TestCase):
    def test_new_types_are_accepted(self):
        for branch in ("claude/perf/faster-docs-check", "codex/ci/pin-actions", "agent/perf/x2"):
            self.assertTrue(harness.BRANCH_PATTERN.match(branch), branch)

    def test_existing_types_still_accepted_and_others_rejected(self):
        for branch in ("claude/fix/login-timeout", "codex/feat/csv-export", "claude/analysis/cache", "codex/chore/deps"):
            self.assertTrue(harness.BRANCH_PATTERN.match(branch), branch)
        for branch in ("claude/feature/x", "claude/performance/x", "claude/cicd/x", "perf/x", "claude/perf/", "Claude/perf/x", "main"):
            self.assertFalse(harness.BRANCH_PATTERN.match(branch), branch)

    def test_worktree_new_rejects_before_touching_git(self):
        with self.assertRaisesRegex(ValueError, "agent>/<type>/<topic"):
            harness.worktree_new("claude/performance/x", None)
