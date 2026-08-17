"""Unit tests for framework_handler utilities."""

from __future__ import annotations

import logging
from uuid import uuid4

import pytest
from agento11y.framework_handler import Agento11yFrameworkHandlerBase, _extract_tool_output


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
