"""Hidden acceptance test: Google API keys and Stripe live secret keys."""
import unittest

import harness


def fake(*parts):
    return "".join(parts)


def scan(line, path="app/config.py"):
    return harness.credential_findings(f"+++ b/{path}\n@@ -0,0 +1 @@\n+{line}")


class HiddenSecretPatterns(unittest.TestCase):
    def test_google_key_is_reported_with_its_label(self):
        key = fake("AI", "za", "Ab3_-" * 7)
        self.assertEqual(scan(f'KEY = "{key}"'), ["app/config.py:1: added line looks like a Google API key"])

    def test_stripe_live_key_is_reported_with_its_label(self):
        key = fake("sk_", "live_", "Z9" * 12)
        self.assertEqual(scan(f"STRIPE={key}"), ["app/config.py:1: added line looks like a Stripe secret key"])

    def test_non_matches(self):
        self.assertEqual(scan(fake("AI", "za", "a" * 30)), [])
        self.assertEqual(scan(fake("sk_", "test_", "a" * 30)), [])
        self.assertEqual(scan(fake("sk_", "live_", "short")), [])

    def test_opt_out_and_values_not_echoed(self):
        key = fake("sk_", "live_", "q" * 30)
        self.assertEqual(scan(f"{key}  # secret-scan: allow (fixture)"), [])
        findings = scan(key)
        self.assertEqual(len(findings), 1)
        self.assertNotIn(key, findings[0])

    def test_existing_shapes_still_reported(self):
        self.assertEqual(len(scan(fake("sk-", "ant-", "a" * 24))), 1)
