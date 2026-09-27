"""Hidden acceptance test: review verdicts in plan metrics."""
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import harness

BLOCK = "# Plan\n\n## Metrics\n- Agent: Claude Code / m / high\n- Elapsed: 10m\n- Check-fix loops: 1\n- User corrections: 0\n{review}\n"


class HiddenReviewMetric(unittest.TestCase):
    def metrics(self, review_line):
        return harness.plan_metrics(BLOCK.format(review=review_line))

    def test_verdicts_are_normalised(self):
        cases = {"- Review: approve": "approve", "- Review: Approve": "approve",
                 "- Review: changes requested, 11 fixed": "changes requested",
                 "- Review: Changes requested (2 fixed)": "changes requested",
                 "- Review: not required (docs-only)": "not required",
                 "- Review: <approve | changes requested, n fixed | not required>": None,
                 "": None}
        for line, expected in cases.items():
            with self.subTest(line=line):
                self.assertEqual(self.metrics(line)["review"], expected)

    def test_other_fields_unchanged(self):
        result = self.metrics("- Review: approve")
        self.assertEqual({k: result[k] for k in ("agent", "minutes", "loops", "corrections")},
                         {"agent": "Claude Code / m / high", "minutes": 10, "loops": 1, "corrections": 0})

    def test_report_shows_an_approval_column(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(harness, "ROOT", Path(directory)), patch("builtins.print") as output:
            archive = Path(directory, ".agents/plans/archive")
            archive.mkdir(parents=True)
            (archive / "a.md").write_text(BLOCK.format(review="- Review: approve"))
            (archive / "b.md").write_text(BLOCK.format(review="- Review: changes requested, 1 fixed"))
            (archive / "c.md").write_text(BLOCK.format(review="- Review: not required"))
            harness.metrics_report()
        text = "\n".join(str(call.args[0]) for call in output.call_args_list if call.args)
        self.assertRegex(text.lower(), r"approv")
        row = next(line for line in text.splitlines() if line.startswith("Claude Code / m / high"))
        self.assertRegex(row, r"\b50(\.0)?\b")
