"""Shared redaction conformance suites.

The fixtures in ``redaction/fixtures/`` are loaded by every redaction engine, so
a change here fails all of them at once instead of letting one SDK drift.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from agento11y.models import (
    Generation,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    ToolCall,
    ToolResult,
)
from agento11y.redaction import (
    SecretRedactionOptions,
    _SecretRedactor,
    create_secret_redaction_sanitizer,
)

_REPO_ROOT = Path(__file__).parents[2]
_FIXTURES = _REPO_ROOT / "redaction" / "fixtures"


def _load(name: str) -> dict:
    return json.loads((_FIXTURES / name).read_text(encoding="utf-8"))


_STRING_CASES = _load("strings.json")["cases"]


@pytest.mark.parametrize("case", _STRING_CASES, ids=[case["id"] for case in _STRING_CASES])
def test_conformance_redaction_strings(case: dict) -> None:
    redactor = _SecretRedactor(include_email_addresses=case["emails"])
    if case["mode"] == "full":
        actual = redactor.redact(case["input"])
    elif case["mode"] == "light":
        actual = redactor.redact_lightweight(case["input"])
    else:
        raise AssertionError(f"unknown mode {case['mode']!r}")

    assert actual == case["expected"]


_GENERATIONS = _load("generations.json")
_PROBE = _GENERATIONS["probe"]
_GENERATION_CASES = _GENERATIONS["cases"]


def _assistant_parts(probe: str) -> list[Part]:
    return [
        Part(kind=PartKind.TEXT, text=probe),
        Part(kind=PartKind.THINKING, thinking=probe),
        Part(kind=PartKind.TOOL_CALL, tool_call=ToolCall(name="bash", input_json=probe.encode("utf-8"))),
    ]


def _tool_parts(probe: str) -> list[Part]:
    return [
        Part(kind=PartKind.TEXT, text=probe),
        Part(
            kind=PartKind.TOOL_RESULT,
            tool_result=ToolResult(name="bash", content=probe, content_json=probe.encode("utf-8")),
        ),
    ]


def _build_probe_generation(probe: str) -> Generation:
    """Fills every slot in the matrix with the same probe."""

    return Generation(
        id="gen-conformance",
        model=ModelRef(provider="openai", name="gpt-5"),
        system_prompt=probe,
        conversation_title=probe,
        call_error=probe,
        input=[
            Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text=probe)]),
            Message(role=MessageRole.ASSISTANT, parts=_assistant_parts(probe)),
            Message(role=MessageRole.TOOL, parts=_tool_parts(probe)),
        ],
        output=[
            Message(role=MessageRole.ASSISTANT, parts=_assistant_parts(probe)),
            Message(role=MessageRole.TOOL, parts=_tool_parts(probe)),
        ],
    )


def _slot_values(generation: Generation) -> dict[str, str]:
    return {
        "systemPrompt": generation.system_prompt,
        "conversationTitle": generation.conversation_title,
        "callError": generation.call_error,
        "input.user.text": generation.input[0].parts[0].text,
        "input.assistant.text": generation.input[1].parts[0].text,
        "input.assistant.thinking": generation.input[1].parts[1].thinking,
        "input.assistant.toolCallInputJson": generation.input[1].parts[2].tool_call.input_json.decode("utf-8"),
        "input.tool.text": generation.input[2].parts[0].text,
        "input.tool.toolResultContent": generation.input[2].parts[1].tool_result.content,
        "input.tool.toolResultContentJson": generation.input[2].parts[1].tool_result.content_json.decode("utf-8"),
        "output.assistant.text": generation.output[0].parts[0].text,
        "output.assistant.thinking": generation.output[0].parts[1].thinking,
        "output.assistant.toolCallInputJson": generation.output[0].parts[2].tool_call.input_json.decode("utf-8"),
        "output.tool.text": generation.output[1].parts[0].text,
        "output.tool.toolResultContent": generation.output[1].parts[1].tool_result.content,
        "output.tool.toolResultContentJson": generation.output[1].parts[1].tool_result.content_json.decode("utf-8"),
    }


@pytest.mark.parametrize("case", _GENERATION_CASES, ids=[case["id"] for case in _GENERATION_CASES])
def test_conformance_redaction_generation_slots(case: dict) -> None:
    sanitize = create_secret_redaction_sanitizer(
        SecretRedactionOptions(
            redact_input_messages=case["redactInputMessages"],
            redact_email_addresses=case["redactEmailAddresses"],
        )
    )
    actual = _slot_values(sanitize(_build_probe_generation(_PROBE["input"])))

    assert set(actual) == set(case["slots"]), "Python harness slots and fixture slots disagree"
    assert actual == {slot: _PROBE[mode] for slot, mode in case["slots"].items()}
