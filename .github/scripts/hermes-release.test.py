#!/usr/bin/env python3
"""Offline release tests. Git commits and tags exist only in temporary repositories."""

import importlib.util
import json
import os
import shutil
import subprocess
import tempfile
import tomllib
import unittest
from pathlib import Path
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("hermes_release", SOURCE / "hermes-release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.previous = Path.cwd()
        os.chdir(self.temp.name)
        self.env = patch.dict(
            os.environ,
            {
                "PATH": os.environ["PATH"],
                "HOME": self.temp.name,
                "GIT_CONFIG_NOSYSTEM": "1",
                "GIT_CONFIG_GLOBAL": os.devnull,
            },
            clear=True,
        )
        self.env.start()
        Path(".github/scripts").mkdir(parents=True)
        for name in ("bump-version", "changelog-for-release", "prepend-changelog", "changelog-top-version"):
            shutil.copyfile(SOURCE / f"{name}.sh", Path(".github/scripts") / f"{name}.sh")
        Path(".github/sdk-releases.json").write_text(
            json.dumps(
                [
                    {
                        "id": "hermes",
                        "tag_prefix": "plugins/hermes",
                        "changelog": "plugins/hermes/CHANGELOG.md",
                        "paths": ["plugins/hermes"],
                    }
                ]
            )
        )
        release.PACKAGE.mkdir(parents=True)
        (release.PACKAGE / "pyproject.toml").write_text(
            '[project]\nname = "agento11y-hermes"\nversion = "0.11.0"\n\n[tool.example]\nversion = "9.0.0"\n'
        )
        (release.PACKAGE / "uv.lock").write_text(
            'version = 1\n\n[[package]]\nname = "agento11y-hermes"\nversion = "0.11.0"\n'
            'source = { editable = "." }\n\n[[package]]\nname = "other"\nversion = "0.11.0"\n'
        )
        (release.PACKAGE / "CHANGELOG.md").write_text("# Changelog\n\n## [0.11.0] - 2026-01-01\n\nFirst release.\n")
        self.git("init", "-q")
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.invalid")
        self.git("add", ".")
        self.git("commit", "-qm", "feat: initial release")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")

    def tearDown(self):
        self.env.stop()
        os.chdir(self.previous)
        self.temp.cleanup()

    def git(self, *args):
        return subprocess.check_output(["git", *args], text=True).strip()

    def tag(self):
        self.git("tag", "plugins/hermes/v0.11.0")

    def guard(self, ref="refs/tags/plugins/hermes/v0.11.0"):
        return release.guard(ref, "refs/remotes/origin/main")

    def test_bootstrap_tag_needs_no_previous_tag(self):
        self.tag()
        self.assertEqual(self.guard(), "0.11.0")

    def test_prepare_refuses_missing_initial_tag_without_changes(self):
        with self.assertRaisesRegex(ValueError, "Current release lacks"):
            release.prepare("patch", "refs/remotes/origin/main")
        self.assertEqual(self.git("status", "--porcelain"), "")

    def test_prepare_refuses_missing_future_tag(self):
        self.tag()
        release.prepare("patch", "refs/remotes/origin/main")
        with self.assertRaisesRegex(ValueError, "Current release lacks"):
            release.prepare("patch", "refs/remotes/origin/main")

    def test_prepare_bumps_both_files_and_changelog(self):
        for bump, expected in (("patch", "0.11.1"), ("minor", "0.12.0"), ("major", "1.0.0")):
            with self.subTest(bump=bump):
                self.git("reset", "--hard", "HEAD")
                if not self.git("tag", "-l"):
                    self.tag()
                self.assertEqual(release.prepare(bump, "refs/remotes/origin/main"), expected)
                self.assertEqual(release.current_version(), expected)
                lock = tomllib.loads((release.PACKAGE / "uv.lock").read_text())
                self.assertEqual(lock["package"][1]["version"], "0.11.0")
                project = tomllib.loads((release.PACKAGE / "pyproject.toml").read_text())
                self.assertEqual(project["tool"]["example"]["version"], "9.0.0")

    def test_prepare_refuses_existing_target_tag(self):
        self.tag()
        self.git("tag", "plugins/hermes/v0.11.1")
        with self.assertRaisesRegex(ValueError, "already has a tag"):
            release.prepare("patch", "refs/remotes/origin/main")
        self.assertEqual(self.git("status", "--porcelain"), "")

    def test_prepare_uses_current_tag_as_changelog_base(self):
        self.tag()
        (release.PACKAGE / "extra").write_text("new change")
        self.git("add", ".")
        self.git("commit", "-qm", "fix: include this change")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.git("tag", "plugins/hermes/v99.0.0")
        release.prepare("patch", "refs/remotes/origin/main")
        changelog = (release.PACKAGE / "CHANGELOG.md").read_text()
        self.assertIn("include this change", changelog)
        self.assertNotIn("initial release", changelog)

    def test_nonrelease_refs_rejected(self):
        for ref in (
            "plugins/hermes/v0.11.0",
            "refs/heads/plugins/hermes/v0.11.0",
            "refs/tags/plugins/hermes/v0.11.0rc1",
            "refs/tags/plugins/hermes/v0.11.0+build",
            "refs/tags/plugins/hermes/v00.11.0",
            "refs/tags/sdk-python/v0.11.0",
        ):
            with self.subTest(ref=ref), self.assertRaises(ValueError):
                self.guard(ref)

    def test_tag_version_mismatch(self):
        with self.assertRaisesRegex(ValueError, "Tag disagrees"):
            self.guard("refs/tags/plugins/hermes/v0.12.0")

    def test_changelog_mismatch(self):
        (release.PACKAGE / "CHANGELOG.md").write_text("## [0.10.0]\n")
        with self.assertRaisesRegex(ValueError, "Changelog top"):
            release.current_version()

    def test_lock_mismatch_does_not_update_project(self):
        lock = release.PACKAGE / "uv.lock"
        lock.write_text(lock.read_text().replace('version = "0.11.0"', 'version = "0.10.0"'))
        before = (release.PACKAGE / "pyproject.toml").read_text()
        with self.assertRaisesRegex(ValueError, "locked Hermes"):
            release.update_version("0.11.0", "0.12.0")
        self.assertEqual((release.PACKAGE / "pyproject.toml").read_text(), before)
        with self.assertRaisesRegex(ValueError, "uv.lock"):
            release.current_version()

    def test_imported_release_rejected(self):
        release.update_version("0.11.0", "0.10.0")
        with self.assertRaisesRegex(ValueError, "Imported 0.10.0"):
            release.current_version()

    def test_off_main_tag_rejected(self):
        (release.PACKAGE / "extra").write_text("off main")
        self.git("add", ".")
        self.git("commit", "-qm", "feat: off main")
        self.tag()
        with self.assertRaises(subprocess.CalledProcessError):
            self.guard()

    def test_checkout_must_match_tag(self):
        self.tag()
        (release.PACKAGE / "extra").write_text("later")
        self.git("add", ".")
        self.git("commit", "-qm", "feat: later")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")
        with self.assertRaisesRegex(ValueError, "Checkout does not match"):
            self.guard()


class WorkflowTests(unittest.TestCase):
    def test_privilege_and_artifact_boundaries(self):
        workflow = (SOURCE.parent / "workflows/hermes-publish.yml").read_text()
        prepare = workflow.split("  prepare:\n", 1)[1].split("  release-pr:\n", 1)[0]
        pr = workflow.split("  release-pr:\n", 1)[1].split("  build:\n", 1)[0]
        publish = workflow.split("  publish:\n", 1)[1]
        self.assertIn("default: true", workflow)
        self.assertNotIn("create-github-app-token", prepare)
        self.assertNotIn("upload-artifact", prepare)
        self.assertIn("if: ${{ !inputs.dry-run }}", pr)
        self.assertNotIn("--auto", workflow)
        self.assertIn("needs: build", publish)
        self.assertIn("environment: pypi", publish)
        self.assertIn("attestations: true", publish)
        self.assertNotIn("run:", publish)
        self.assertNotIn("actions/checkout", publish)
        self.assertIn('--expected-version "$VERSION" --output-dir "$PWD/dist/hermes"', workflow)


if __name__ == "__main__":
    unittest.main()
