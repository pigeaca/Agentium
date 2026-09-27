"""Hidden acceptance test: package-manager credential files and keystores."""
import unittest

import harness


class HiddenSensitiveFiles(unittest.TestCase):
    def test_new_credential_files_are_blocked(self):
        for path in (".npmrc", "web/.npmrc", ".pypirc", "tools/.pypirc", ".netrc", "certs/app.jks", "android/release.keystore"):
            self.assertTrue(harness.sensitive_path(path), path)

    def test_lookalikes_stay_allowed(self):
        for path in ("docs/npmrc.md", "src/netrc_parser.py", "docs/keystore.md", "npmrc.example.txt", ".env.example", "id_ed25519.pub"):
            self.assertFalse(harness.sensitive_path(path), path)

    def test_existing_rules_unchanged(self):
        for path in (".env", "config/.env.local", "tls.key", "id_rsa", "credentials.json", ".claude/settings.local.json"):
            self.assertTrue(harness.sensitive_path(path), path)
