#!/usr/bin/env python3
"""Check suite selection without installing any SDK or invoking agent CLIs."""

import json
import os
import re
import runpy
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

MODULE = runpy.run_path(str(Path(__file__).with_name("ci-scope.py")))
select_scope = MODULE["select_scope"]


class ScopeTests(unittest.TestCase):
    def test_changed_paths_select_existing_suites(self):
        cases = [
            (["README.md", "docs/concepts/otel-setup.md", "go/README.md", "llms.txt"], []),
            (["python-frameworks/langchain/agento11y_langchain/handler.py"], ["python:langchain", "python-lint"]),
            (["python-providers/openai/pyproject.toml"], ["python:openai", "python-lint"]),
            (
                ["python-frameworks/langgraph/tests/test_handler.py", "go-providers/gemini/go.mod"],
                ["python:langgraph", "python-lint", "go-providers/gemini"],
            ),
            (
                ["python/agento11y/client.py"],
                ["python-all", "python-core", "python-lint", "hermes", "conformance", "proto"],
            ),
            (["go/agento11y/client.go"], ["go-all", "go-plugin-windows", "conformance", "proto"]),
            (["plugins/hermes/agento11y_hermes/plugin.py"], ["hermes"]),
            (["plugins/pi/src/index.ts"], ["typescript"]),
            (["js/src/client.ts"], ["typescript", "conformance", "proto"]),
            (["java/core/src/Client.java"], ["java", "conformance", "proto"]),
            (["dotnet/src/Client.cs"], ["dotnet", "conformance", "proto"]),
            (["plugins/codex/hooks/hooks.json"], ["plugins/agento11y", "go-plugin-windows", "typescript", "proto"]),
            (
                ["plugins/agento11y/internal/skills/content/setup-coding-agent/SKILL.md"],
                ["plugins/agento11y", "go-plugin-windows", "typescript", "proto"],
            ),
            (["python-frameworks/langchain/tests/fixtures/README.md"], ["python:langchain", "python-lint"]),
        ]
        for paths, expected in cases:
            with self.subTest(paths=paths):
                self.assertEqual(select_scope(paths), sorted(expected))

    def test_shared_or_unknown_paths_run_everything(self):
        for path in [
            "proto/agento11y/v1/generation_ingest.proto",
            "redaction/patterns.json",
            "conformance/hooks/fixtures.json",
            ".github/workflows/ci.yml",
            ".github/scripts/ci-scope.py",
            "mise.toml",
            "pnpm-lock.yaml",
            "go.work",
            "ruff.toml",
            "docs/check_examples.py",
            "assets/readme/generate.js",
            "python-frameworks/new-framework/handler.py",
            "new-directory/code.py",
        ]:
            with self.subTest(path=path):
                self.assertEqual(select_scope(["README.md", path]), ["all"])

    def test_python_mapping_matches_current_matrix(self):
        workflow = Path(__file__).parents[1] / "workflows/ci.yml"
        names = set(re.findall(r"- \{ name: ([\w-]+), cmd:", workflow.read_text()))
        self.assertEqual(MODULE["PYTHON_SUITES"] | {"core"}, names)

    def test_diff_includes_deleted_and_renamed_paths_without_line_splitting(self):
        base, head, ancestor = "a" * 40, "b" * 40, "c" * 40
        with patch.object(
            MODULE["subprocess"],
            "check_output",
            side_effect=[
                ancestor + "\n",
                b"python/old.py\0docs/new name\nwith newline.md\0",
            ],
        ) as git:
            self.assertEqual(MODULE["changed_paths"](base, head), ["python/old.py", "docs/new name\nwith newline.md"])
            self.assertEqual(
                git.call_args.args[0], ["git", "diff", "--no-renames", "--name-only", "-z", ancestor, head, "--"]
            )
        with self.assertRaises(ValueError):
            MODULE["changed_paths"]("--unsafe-option", head)

    def test_main_builds_run_everything_without_diffing(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "push", "GITHUB_OUTPUT": str(output)}, clear=True):
                with patch.object(MODULE["subprocess"], "check_output") as git:
                    MODULE["main"]()
                    git.assert_not_called()
            self.assertEqual(json.loads(output.read_text().removeprefix("scope=")), ["all"])


if __name__ == "__main__":
    unittest.main()
