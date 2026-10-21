"""Validation parity tests for generation payload semantics."""

from __future__ import annotations

from copy import copy, deepcopy
from dataclasses import replace
from datetime import datetime, timezone

import pytest
from agento11y import (
    ContentCaptureMode,
    EmbeddingResult,
    EmbeddingStart,
    Generation,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    ToolCall,
    ToolExecutionEnd,
    ToolExecutionKind,
    ToolExecutionStart,
    ToolResult,
    validate_embedding_result,
    validate_embedding_start,
    validate_generation,
)


def test_tool_execution_kind_values() -> None:
    assert {execution_kind.name: execution_kind.value for execution_kind in ToolExecutionKind} == {
        "LOAD_SKILL": "load_skill",
        "READ_SKILL_RESOURCE": "read_skill_resource",
        "COMMAND": "command",
    }
    for execution_kind in ToolExecutionKind:
        assert isinstance(execution_kind, str)
        assert ToolExecutionKind(execution_kind.value) is execution_kind


def test_tool_execution_models_preserve_positional_api_and_defaults() -> None:
    timestamp = datetime(2026, 1, 1, tzinfo=timezone.utc)
    start = ToolExecutionStart(
        "shell",
        "call-1",
        "function",
        "Run shell",
        "conv-1",
        "Title",
        "agent",
        "v1",
        "model",
        "provider",
        True,
        ContentCaptureMode.FULL,
        timestamp,
    )
    assert start == ToolExecutionStart(
        tool_name="shell",
        tool_call_id="call-1",
        tool_type="function",
        tool_description="Run shell",
        conversation_id="conv-1",
        conversation_title="Title",
        agent_name="agent",
        agent_version="v1",
        request_model="model",
        request_provider="provider",
        include_content=True,
        content_capture=ContentCaptureMode.FULL,
        started_at=timestamp,
    )
    assert start.execution_kind is None
    assert start.skill_name == ""
    assert start.skill_description == ""
    assert start.skill_source_uri == ""
    assert start.skill_resource_name == ""
    assert start.process_executable_name == ""
    assert start.process_executable_path == ""
    end = ToolExecutionEnd({"command": "pwd"}, "output", timestamp)
    assert end == ToolExecutionEnd(arguments={"command": "pwd"}, result="output", completed_at=timestamp)
    assert end.process_exit_code is None


@pytest.mark.parametrize("copy_model", [copy, deepcopy, replace])
@pytest.mark.parametrize("execution_kind", [None, *ToolExecutionKind])
def test_tool_execution_start_copy_preserves_skill_process_fields(copy_model, execution_kind) -> None:
    start = ToolExecutionStart(
        tool_name="tool",
        execution_kind=execution_kind,
        skill_name="review",
        skill_description="Review code",
        skill_source_uri="file:///skills/review/SKILL.md",
        skill_resource_name="references/checklist.md",
        process_executable_name="python",
        process_executable_path="/usr/bin/python",
    )

    copied = copy_model(start)

    assert copied is not start
    assert copied == start
    assert copied.execution_kind is execution_kind


@pytest.mark.parametrize("copy_model", [copy, deepcopy, replace])
@pytest.mark.parametrize("exit_code", [None, 0, 1, -9])
def test_tool_execution_end_copy_preserves_exit_code(copy_model, exit_code) -> None:
    end = ToolExecutionEnd(process_exit_code=exit_code)

    copied = copy_model(end)

    assert copied is not end
    assert copied == end
    assert copied.process_exit_code == exit_code
    assert type(copied.process_exit_code) is type(exit_code)


@pytest.mark.parametrize("exit_code", [True, False, "0", 0.0, 1.5, float("nan"), [], {}])
def test_tool_execution_end_rejects_invalid_exit_code(exit_code) -> None:
    with pytest.raises(ValueError, match="process_exit_code must be an int or None"):
        ToolExecutionEnd(process_exit_code=exit_code)


def _base_generation() -> Generation:
    return Generation(
        model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
        input=[
            Message(
                role=MessageRole.ASSISTANT,
                parts=[Part(kind=PartKind.TEXT, text="ok")],
            )
        ],
    )


def test_validate_generation_rejects_tool_call_for_user_role() -> None:
    generation = _base_generation()
    generation.input.append(
        Message(
            role=MessageRole.USER,
            parts=[Part(kind=PartKind.TOOL_CALL, tool_call=ToolCall(name="weather"))],
        )
    )

    with pytest.raises(
        ValueError, match=r"generation\.input\[1\].parts\[0\].tool_call only allowed for assistant role"
    ):
        validate_generation(generation)


def test_validate_generation_rejects_tool_result_for_assistant_role() -> None:
    generation = _base_generation()
    generation.input.append(
        Message(
            role=MessageRole.ASSISTANT,
            parts=[
                Part(
                    kind=PartKind.TOOL_RESULT,
                    tool_result=ToolResult(tool_call_id="toolu_1", content="sunny"),
                )
            ],
        )
    )

    with pytest.raises(ValueError, match=r"generation\.input\[1\].parts\[0\].tool_result only allowed for tool role"):
        validate_generation(generation)


def test_validate_generation_rejects_thinking_for_non_assistant_role_output_path() -> None:
    generation = _base_generation()
    generation.output = [
        Message(
            role=MessageRole.USER,
            parts=[Part(kind=PartKind.THINKING, thinking="private reasoning")],
        )
    ]

    with pytest.raises(
        ValueError, match=r"generation\.output\[0\].parts\[0\].thinking only allowed for assistant role"
    ):
        validate_generation(generation)


def test_validate_generation_accepts_conversation_and_response_fields() -> None:
    generation = Generation(
        conversation_id="conv-1",
        model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
        response_id="resp-1",
        response_model="claude-sonnet-4-5-20260201",
        input=[
            Message(
                role=MessageRole.USER,
                parts=[Part(kind=PartKind.TEXT, text="hello")],
            )
        ],
        output=[
            Message(
                role=MessageRole.ASSISTANT,
                parts=[Part(kind=PartKind.TEXT, text="hi")],
            )
        ],
    )

    validate_generation(generation)


def test_validate_embedding_start_requires_model_fields() -> None:
    with pytest.raises(ValueError, match=r"embedding\.model\.provider is required"):
        validate_embedding_start(
            EmbeddingStart(
                model=ModelRef(provider="", name="text-embedding-3-small"),
            )
        )

    with pytest.raises(ValueError, match=r"embedding\.model\.name is required"):
        validate_embedding_start(
            EmbeddingStart(
                model=ModelRef(provider="openai", name=""),
            )
        )


def test_validate_embedding_result_rejects_negative_counts() -> None:
    with pytest.raises(ValueError, match=r"embedding\.input_count must be >= 0"):
        validate_embedding_result(
            EmbeddingResult(
                input_count=-1,
            )
        )

    with pytest.raises(ValueError, match=r"embedding\.input_tokens must be >= 0"):
        validate_embedding_result(
            EmbeddingResult(
                input_count=1,
                input_tokens=-1,
            )
        )
