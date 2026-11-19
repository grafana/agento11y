#!/usr/bin/env python3
"""Select existing CI suites. Unknown paths and main builds run everything."""

import json
import os
import re
import subprocess
from pathlib import PurePosixPath

PYTHON_SUITES = {
    "openai",
    "anthropic",
    "gemini",
    "langchain",
    "langgraph",
    "openai-agents",
    "llamaindex",
    "google-adk",
    "litellm",
    "pydantic-ai",
    "deepeval",
    "harbor",
    "strands",
}
GO_ADAPTERS = {
    "go-providers/openai",
    "go-providers/anthropic",
    "go-providers/gemini",
    "go-frameworks/google-adk",
}
LAUNCHER_PLUGINS = {"claude-code", "codex", "copilot", "cursor", "kiro", "sigil", "vibe"}


def select_scope(paths: list[str]) -> list[str]:
    scope: set[str] = set()
    for path in paths:
        parts = PurePosixPath(path).parts
        # Embedded skills and test fixtures can change runtime behavior, even as Markdown.
        runtime_content = path.startswith("plugins/agento11y/internal/") or any(
            part in {"tests", "testdata", "fixtures"} for part in parts
        )
        if not runtime_content and (
            PurePosixPath(path).name in {"README.md", "CHANGELOG.md", "AGENTS.md", "CLAUDE.md"}
            or path == "llms.txt"
            or (path.startswith("docs/") and path.endswith(".md"))
            or (path.startswith("assets/readme/") and PurePosixPath(path).suffix in {".png", ".jpg", ".svg", ".webp"})
        ):
            continue
        if path.startswith("python/"):
            scope.update({"python-all", "python-core", "python-lint", "hermes", "conformance", "proto"})
        elif parts[0] in {"python-frameworks", "python-providers"} and len(parts) > 2:
            if parts[1] not in PYTHON_SUITES:
                return ["all"]
            scope.update({f"python:{parts[1]}", "python-lint"})
        elif path.startswith("go/"):
            scope.update({"go-all", "go-plugin-windows", "conformance", "proto"})
        elif "/".join(parts[:2]) in GO_ADAPTERS:
            scope.add("/".join(parts[:2]))
        elif path.startswith(("js/", "js-core/")):
            scope.update({"typescript", "conformance", "proto"})
        elif path.startswith("java/"):
            scope.update({"java", "conformance", "proto"})
        elif path.startswith("dotnet/"):
            scope.update({"dotnet", "conformance", "proto"})
        elif path.startswith("plugins/hermes/"):
            scope.add("hermes")
        elif path.startswith(("plugins/pi/", "plugins/opencode/")):
            scope.add("typescript")
        elif path.startswith("plugins/agento11y/") or (
            len(parts) > 2 and parts[0] == "plugins" and parts[1] in LAUNCHER_PLUGINS
        ):
            # The Go binary embeds the local viewer and shares launcher code across hosts.
            scope.update({"plugins/agento11y", "go-plugin-windows", "typescript", "proto"})
        else:
            # Includes workflow/toolchain/lockfile changes, proto/redaction/conformance,
            # examples, and new integrations not yet mapped to an existing CI suite.
            return ["all"]
    return sorted(scope)


def changed_paths(base: str, head: str) -> list[str]:
    for revision in (base, head):
        if not re.fullmatch(r"[0-9a-f]{40}", revision):
            raise ValueError("PR base and head must be full commit SHAs")
    merge_base = subprocess.check_output(["git", "merge-base", base, head], text=True).strip()
    output = subprocess.check_output(["git", "diff", "--no-renames", "--name-only", "-z", merge_base, head, "--"])
    return [path.decode("utf-8") for path in output.split(b"\0") if path]


def main() -> None:
    scope = ["all"]
    if os.environ["GITHUB_EVENT_NAME"] == "pull_request":
        scope = select_scope(changed_paths(os.environ["PR_BASE_SHA"], os.environ["PR_HEAD_SHA"]))
    encoded = json.dumps(scope)
    print(f"Selected CI scopes: {encoded}")
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        output.write(f"scope={encoded}\n")


if __name__ == "__main__":
    main()
