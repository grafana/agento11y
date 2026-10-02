"""Unit tests for framework_handler utilities."""

from __future__ import annotations

import logging
from uuid import uuid4

import pytest
from agento11y.framework_handler import (
    Agento11yFrameworkHandlerBase,
    _extract_tool_output,
    _map_chat_input_message,
    _map_framework_usage,
)
from agento11y.models import MessageRole, PartKind


class _FakeToolMessage:
    def __init__(self, content):
        self.content = content


class _StubToolRecorder:
    """Tool recorder that stores what it is given, and returns the error a test configures."""

    def __init__(self, *, final_error: Exception | None = None) -> None:
        self.result: dict[str, object] | None = None
        self.exec_error: Exception | None = None
        self.ended = False
        self._final_error = final_error

    def set_result(self, **payload) -> None:
        self.result = payload

    def set_exec_error(self, error) -> None:
        self.exec_error = error

    def end(self) -> None:
        self.ended = True

    def err(self):
        # The real recorder returns the exec error it was given.
        return self._final_error or self.exec_error


class _StubClient:
    def __init__(self, recorder: _StubToolRecorder) -> None:
        self._recorder = recorder

    def start_tool_execution(self, _start):
        return self._recorder


def test_extract_tool_output_unwraps_content_and_preserves_plain_values() -> None:
    payload = {"temp_c": 18}

    assert _extract_tool_output(_FakeToolMessage("tool result text")) == "tool result text"
    assert _extract_tool_output("plain string") == "plain string"
    assert _extract_tool_output(None) is None
    assert _extract_tool_output(payload) is payload


@pytest.mark.parametrize(
    ("outcome", "recorder_error", "expect_log"),
    [
        ("success", None, False),
        ("success", RuntimeError("serialize tool result: unsupported type"), True),
        ("failure", None, False),
        ("failure", RuntimeError("serialize tool result: unsupported type"), True),
    ],
)
def test_tool_callbacks_log_recorder_errors_instead_of_raising(outcome, recorder_error, expect_log, caplog) -> None:
    recorder = _StubToolRecorder(final_error=recorder_error)
    handler = Agento11yFrameworkHandlerBase(client=_StubClient(recorder), framework_name="test")  # type: ignore[arg-type]
    run_id = uuid4()
    handler._on_tool_start(serialized={"name": "weather"}, input_str="{}", run_id=run_id, parent_run_id=None)

    with caplog.at_level(logging.ERROR, logger="agento11y"):
        if outcome == "success":
            handler._on_tool_end(output="18C", run_id=run_id)
        else:
            handler._on_tool_error(error=RuntimeError("the tool itself failed"), run_id=run_id)

    assert recorder.ended is True
    assert handler._tool_runs == {}
    assert bool(caplog.records) is expect_log
    if outcome == "failure":
        assert str(recorder.exec_error) == "the tool itself failed"
    if expect_log:
        assert str(run_id) in caplog.text
        assert "serialize tool result" in caplog.text


class _FakeLangChainToolMessage:
    """The attribute shape of langchain_core.messages.ToolMessage."""

    type = "tool"

    def __init__(self, content, tool_call_id, name="", status="success"):
        self.content = content
        self.tool_call_id = tool_call_id
        self.name = name
        self.status = status


def test_map_chat_input_message_maps_tool_message_to_tool_result_part() -> None:
    message = _FakeLangChainToolMessage(
        '{"category": "Restaurants", "total": 2110.96}', tool_call_id="call_1", name="get_category_detail"
    )

    mapped = _map_chat_input_message(message)

    assert mapped is not None
    assert mapped.role is MessageRole.TOOL
    assert [part.kind for part in mapped.parts] == [PartKind.TOOL_RESULT]
    result = mapped.parts[0].tool_result
    assert result.tool_call_id == "call_1"
    assert result.name == "get_category_detail"
    assert result.content == '{"category": "Restaurants", "total": 2110.96}'
    assert result.is_error is False


def test_map_chat_input_message_marks_errored_tool_message() -> None:
    mapped = _map_chat_input_message(_FakeLangChainToolMessage("boom", tool_call_id="call_2", status="error"))

    assert mapped is not None
    assert mapped.parts[0].tool_result.is_error is True


def test_map_chat_input_message_keeps_empty_tool_output_as_a_result() -> None:
    mapped = _map_chat_input_message(_FakeLangChainToolMessage("", tool_call_id="call_3", name="noop"))

    assert mapped is not None
    assert mapped.parts[0].kind is PartKind.TOOL_RESULT
    assert mapped.parts[0].tool_result.content == ""


def test_map_chat_input_message_leaves_other_roles_as_text() -> None:
    mapped = _map_chat_input_message({"type": "human", "content": "What did we spend last month?"})

    assert mapped is not None
    assert mapped.role is MessageRole.USER
    assert [part.kind for part in mapped.parts] == [PartKind.TEXT]


def test_map_framework_usage_reads_langchain_cache_read_and_cache_creation() -> None:
    usage = _map_framework_usage(
        {
            "input_tokens": 9000,
            "output_tokens": 40,
            "total_tokens": 9040,
            "input_token_details": {"cache_read": 8796, "cache_creation": 51},
        }
    )
    assert usage.cache_read_input_tokens == 8796
    assert usage.cache_write_input_tokens == 51
