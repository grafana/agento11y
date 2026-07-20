"""Unit tests for framework_handler utilities."""

from __future__ import annotations

from agento11y.framework_handler import _extract_tool_output


class _FakeToolMessage:
    def __init__(self, content):
        self.content = content


def test_extract_tool_output_unwraps_content_and_preserves_plain_values() -> None:
    payload = {"temp_c": 18}

    assert _extract_tool_output(_FakeToolMessage("tool result text")) == "tool result text"
    assert _extract_tool_output("plain string") == "plain string"
    assert _extract_tool_output(None) is None
    assert _extract_tool_output(payload) is payload
