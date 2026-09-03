"""Validation parity tests for generation payload semantics."""

from __future__ import annotations

import pytest
from agento11y import (
    ContentCaptureMode,
    EmbeddingResult,
    EmbeddingStart,
    Generation,
    Media,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    ToolCall,
    ToolResult,
    media_part,
    validate_embedding_result,
    validate_embedding_start,
    validate_generation,
)
from agento11y.models import _metadata_key_content_capture_mode


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


def test_validate_generation_rejects_media_without_a_url() -> None:
    generation = _base_generation()
    generation.input.append(
        Message(
            role=MessageRole.USER,
            parts=[media_part(Media(kind="image", url="", mime_type="image/png", name="prompt.png"))],
        )
    )

    with pytest.raises(ValueError, match=r"generation\.input\[1\].parts\[0\].media\.url is required"):
        validate_generation(generation)


def test_validate_generation_rejects_media_without_a_payload() -> None:
    generation = _base_generation()
    generation.input.append(
        Message(
            role=MessageRole.USER,
            parts=[Part(kind=PartKind.MEDIA, media=None)],
        )
    )

    with pytest.raises(ValueError, match=r"generation\.input\[1\].parts\[0\] must set exactly one payload field"):
        validate_generation(generation)


@pytest.mark.parametrize("role", [MessageRole.USER, MessageRole.ASSISTANT, MessageRole.TOOL])
def test_validate_generation_accepts_media_on_every_role(role: MessageRole) -> None:
    # Media is allowed on every role, unlike thinking and tool_call (assistant only)
    # and tool_result (tool only).
    generation = _base_generation()
    generation.input.append(
        Message(
            role=role,
            parts=[
                media_part(
                    Media(
                        kind="image",
                        url="data:image/png;base64,abc123",
                        mime_type="image/png",
                        name="prompt.png",
                    )
                )
            ],
        )
    )

    validate_generation(generation)


def test_validate_generation_accepts_stripped_media_without_a_url() -> None:
    generation = _base_generation()
    generation.metadata[_metadata_key_content_capture_mode] = ContentCaptureMode.METADATA_ONLY.value
    generation.input.append(
        Message(
            role=MessageRole.USER,
            parts=[media_part(Media(kind="image", url="", mime_type="image/png", name="prompt.png"))],
        )
    )

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
