"""Small, explicit case conventions shared by local and stored judges."""

from __future__ import annotations

import json
import re
from typing import Any

from .types import TestCase

_PATH = re.compile(r"^(input|expected|metadata)(?:\.[a-zA-Z0-9_-]+)*$")
_VARIABLE = re.compile(r"\{\{\s*([^{}]+?)\s*\}\}|(?<!\{)\{(input|expected|output)\}(?!\})")


def text_case(
    case_id: str,
    prompt: str,
    *,
    expected: str | None = None,
    rubric: str | None = None,
    context: list[str] | None = None,
    metadata: dict[str, Any] | None = None,
) -> TestCase:
    """Create a grafana.text.v1 case; references are never agent input.

    Omit expected for rubric-only or reference-free evaluation. Empty-string
    references are valid; missing references are not interchangeable with them.
    """
    if not isinstance(case_id, str) or not case_id.strip():
        raise ValueError("case_id must be a nonempty string")
    if not isinstance(prompt, str) or not prompt.strip():
        raise ValueError("prompt must be a nonempty string")
    if expected is not None and not isinstance(expected, str):
        raise ValueError("expected must be a string")
    if rubric is not None and (not isinstance(rubric, str) or not rubric.strip()):
        raise ValueError("rubric must be a nonempty string")
    if context is not None and (not isinstance(context, list) or not all(isinstance(x, str) for x in context)):
        raise ValueError("context must be a list of strings")
    return TestCase(
        test_case_id=case_id,
        name=case_id,
        input={"prompt": prompt, **({"context": list(context)} if context is not None else {})},
        expected={
            **({"assistant_response": expected} if expected is not None else {}),
            **({"rubric": rubric} if rubric is not None else {}),
        },
        metadata={**(metadata or {}), "case_format": "grafana.text.v1"},
    )


def case_value(case: TestCase, path: str) -> Any:
    """Resolve an explicit dot path; reject missing/null values before inference."""
    path = path.removeprefix("test_case.")
    if not _PATH.fullmatch(path):
        raise ValueError(f"invalid case selector: {path!r}")
    parts = path.split(".")
    value = getattr(case, parts[0])
    for key in parts[1:]:
        if isinstance(value, dict) and key in value:
            value = value[key]
        elif isinstance(value, list) and key.isascii() and key.isdigit() and int(key) < len(value):
            value = value[int(key)]
        else:
            raise ValueError(f"{case.test_case_id}: missing test_case.{path}")
    if value is None:
        raise ValueError(f"{case.test_case_id}: null test_case.{path}")
    return value


def case_paths(template: str) -> tuple[str, ...]:
    return tuple(
        m.group(1).strip()
        for m in _VARIABLE.finditer(template)
        if m.group(1) and m.group(1).strip().startswith("test_case.")
    )


def render_case_prompt(template: str, case: TestCase, output: Any) -> str:
    """One-pass substitution: values cannot inject additional placeholders."""

    def substitute(match: re.Match[str]) -> str:
        name = (match.group(1) or match.group(2)).strip()
        if name.startswith("test_case."):
            value = case_value(case, name)
        elif name == "output":
            value = output
        elif name in {"input", "expected"}:
            value = getattr(case, name)
        else:
            raise ValueError(f"unsupported local judge placeholder: {name}")
        return value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, allow_nan=False)

    return _VARIABLE.sub(substitute, template)
