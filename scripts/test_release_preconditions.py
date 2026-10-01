#!/usr/bin/env python3
"""Exercise fail-closed release checks with synthetic values, never real secrets."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SECRETS = (
    "GITHUB_TOKEN", "HOMEBREW_TAP_GITHUB_TOKEN", "QUILL_SIGN_P12",
    "QUILL_SIGN_PASSWORD", "QUILL_NOTARY_KEY", "QUILL_NOTARY_KEY_ID",
    "QUILL_NOTARY_ISSUER",
)
DUMMY = "synthetic-value-must-never-be-printed"


class Preconditions(unittest.TestCase):
    def run_check(self, visibility="false", missing=None):
        env = {"PATH": os.environ["PATH"], **{name: DUMMY for name in SECRETS}}
        if visibility is not None:
            env["REPOMAN_REPOSITORY_PRIVATE"] = visibility
        if missing:
            env.pop(missing)
        result = subprocess.run(
            ["bash", str(ROOT / "scripts/check-release-preconditions.sh")],
            env=env, capture_output=True, text=True, check=False,
        )
        self.assertNotIn(DUMMY, result.stdout + result.stderr)
        return result

    def test_private_or_unknown_is_blocked(self):
        for visibility in ("true", "", "null", "False", None):
            with self.subTest(visibility=visibility):
                result = self.run_check(visibility)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("publishing is blocked", result.stderr)

    def test_every_required_secret_is_checked(self):
        for name in SECRETS:
            with self.subTest(name=name):
                result = self.run_check(missing=name)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Missing release secret: " + name, result.stderr)

    def test_public_and_present_passes(self):
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout + result.stderr, "")

    def test_workflow_checks_before_publishing(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertLess(workflow.index("bash scripts/check-live-release.sh"),
                        workflow.index("args: release --clean"))
        guard = (ROOT / "scripts/check-live-release.sh").read_text()
        self.assertIn("gh api --hostname github.com repos/richhaase/repoman --jq .private", guard)
        config = (ROOT / ".goreleaser.yaml").read_text()
        self.assertIn("{{ if .IsSnapshot }}true{{ else }}bash scripts/check-live-release.sh{{ end }}", config)
        for name in SECRETS:
            self.assertIn("secrets." + name, workflow)

    def test_live_check_uses_fresh_exact_repository_metadata(self):
        for value, exit_code, expected in (("false", 0, 0), ("true", 0, 1), ("", 1, 1), ("null", 0, 1)):
            with self.subTest(value=value, exit_code=exit_code), tempfile.TemporaryDirectory() as temp:
                gh = Path(temp) / "gh"
                gh.write_text("#!/usr/bin/env bash\n"
                              "[[ \"$*\" == 'api --hostname github.com repos/richhaase/repoman --jq .private' ]] || exit 9\n"
                              + "printf '%s\\n' '" + value + "'\nexit " + str(exit_code) + "\n")
                gh.chmod(0o700)
                env = {"PATH": temp + os.pathsep + os.environ["PATH"],
                       "REPOMAN_REPOSITORY_PRIVATE": "false",
                       "GITHUB_REPOSITORY": "unrelated/public-repo",
                       **{name: DUMMY for name in SECRETS}}
                result = subprocess.run(["bash", str(ROOT / "scripts/check-live-release.sh")],
                                        env=env, capture_output=True, text=True, check=False)
                self.assertEqual(result.returncode == 0, expected == 0, result.stderr)
                self.assertNotIn(DUMMY, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
