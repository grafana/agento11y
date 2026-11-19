"""Map Pydantic AI messages and stream events to agento11y messages.

Every part is rendered by Pydantic AI's own serializers, so a recorded message holds what the
model was sent or returned. agento11y allows tool calls only in assistant messages and tool
results only in tool messages, so one Pydantic AI message becomes one agento11y message per run
of consecutive parts that share a role, in their original order.
"""

from __future__ import annotations

from collections.abc import Iterable, Iterator, Sequence
from itertools import groupby
from typing import Any, NamedTuple

from agento11y import Message, MessageRole, Part, PartKind, ToolCall, ToolResult
from pydantic_ai.messages import (
    BaseToolCallPart,
    BaseToolReturnPart,
    ModelMessage,
    ModelRequest,
    ModelRequestPart,
    ModelResponse,
    ModelResponsePart,
    PartDeltaEvent,
    PartStartEvent,
    RetryPromptPart,
    SystemPromptPart,
    TextContent,
    TextPart,
    TextPartDelta,
    ThinkingPart,
    ThinkingPartDelta,
    UserContent,
    UserPromptPart,
)

try:
    from pydantic_ai.messages import ERROR_OUTCOMES
except ImportError:  # 1.x and early 2.x releases lack the constant; one without `retried` never records it
    ERROR_OUTCOMES = ("failed", "retried")

try:
    from pydantic_ai.messages import RetryFeedbackPart

    _RETRY_FEEDBACK_PARTS: tuple[type, ...] = (RetryFeedbackPart,)
except ImportError:  # earlier releases send this feedback as a RetryPromptPart without a tool name
    _RETRY_FEEDBACK_PARTS = ()


class _RolePart(NamedTuple):
    role: MessageRole
    part: Part


def map_request_messages(messages: Sequence[ModelMessage]) -> tuple[list[Message], list[str]]:
    """Map the history sent to the model; returns its messages and its system prompt parts.

    agento11y has no system role: system prompt parts go to the generation's system prompt.
    """
    mapped: list[Message] = []
    system_prompts: list[str] = []
    for message in messages:
        if isinstance(message, ModelRequest):
            system_prompts.extend(
                part.content for part in message.parts if isinstance(part, SystemPromptPart) and part.content.strip()
            )
            mapped.extend(_group(_request_parts(message.parts)))
        elif isinstance(message, ModelResponse):
            mapped.extend(map_response_message(message))
    return mapped, system_prompts


def map_response_message(response: ModelResponse) -> list[Message]:
    """Map one model response; a provider-executed tool's result becomes a tool message between its turns."""
    return list(_group(_response_parts(response.parts)))


def stream_token(event: object) -> str | list[dict[str, Any]]:
    """What a stream event adds to the response: its text, or a content block for output that is not text.

    A block marks the first token without adding to the response text; an event that adds no
    output returns an empty string.
    """
    part = event.part if isinstance(event, PartStartEvent) else None
    delta = event.delta if isinstance(event, PartDeltaEvent) else None
    if isinstance(part, TextPart):
        return part.content
    if isinstance(delta, TextPartDelta):
        return delta.content_delta
    if isinstance(part, ThinkingPart):
        return [{"thinking": part.content}]
    if isinstance(delta, ThinkingPartDelta) and delta.content_delta:
        return [{"thinking": delta.content_delta}]
    if isinstance(part, BaseToolCallPart):
        return [{"name": part.tool_name}]
    return ""


def _group(parts: Iterable[_RolePart]) -> Iterator[Message]:
    for role, run in groupby(parts, key=lambda role_part: role_part.role):
        yield Message(role=role, parts=[role_part.part for role_part in run])


def _request_parts(parts: Sequence[ModelRequestPart]) -> Iterator[_RolePart]:
    # Parts not matched here are system prompts, read by the caller, or framework state with no
    # agento11y counterpart: instruction and tool-availability deltas, compaction, speech.
    for part in parts:
        if isinstance(part, UserPromptPart):
            yield from (_RolePart(MessageRole.USER, _text(text)) for text in _user_texts(part.content))
        elif isinstance(part, BaseToolReturnPart):
            yield _RolePart(MessageRole.TOOL, _tool_result(part))
        elif isinstance(part, RetryPromptPart) and part.tool_name:
            yield _RolePart(MessageRole.TOOL, _retried_tool_result(part))
        elif isinstance(part, (RetryPromptPart, *_RETRY_FEEDBACK_PARTS)):
            yield _RolePart(MessageRole.USER, _text(part.model_response()))


def _response_parts(parts: Sequence[ModelResponsePart]) -> Iterator[_RolePart]:
    # A file or compaction part has no agento11y counterpart; an empty thinking part is a
    # signature-only block that carries no text.
    for part in parts:
        if isinstance(part, TextPart) and part.content.strip():
            yield _RolePart(MessageRole.ASSISTANT, _text(part.content))
        elif isinstance(part, ThinkingPart) and part.content.strip():
            yield _RolePart(MessageRole.ASSISTANT, Part(kind=PartKind.THINKING, thinking=part.content))
        elif isinstance(part, BaseToolCallPart):
            yield _RolePart(MessageRole.ASSISTANT, _tool_call(part))
        elif isinstance(part, BaseToolReturnPart):
            yield _RolePart(MessageRole.TOOL, _tool_result(part))


def _user_texts(content: str | Sequence[UserContent]) -> Iterator[str]:
    # agento11y's Python SDK has no media part, so images, audio, documents and cache points are not recorded.
    for item in [content] if isinstance(content, str) else content:
        text = item.content if isinstance(item, TextContent) else item
        if isinstance(text, str) and text.strip():
            yield text


def _text(text: str) -> Part:
    return Part(kind=PartKind.TEXT, text=text)


def _tool_call(part: BaseToolCallPart) -> Part:
    return Part(
        kind=PartKind.TOOL_CALL,
        tool_call=ToolCall(id=part.tool_call_id, name=part.tool_name, input_json=part.args_as_json_str().encode()),
    )


def _tool_result(part: BaseToolReturnPart) -> Part:
    return Part(
        kind=PartKind.TOOL_RESULT,
        tool_result=ToolResult(
            tool_call_id=part.tool_call_id,
            name=part.tool_name,
            content=part.model_response_str(),
            is_error=part.outcome in ERROR_OUTCOMES,
        ),
    )


def _retried_tool_result(part: RetryPromptPart) -> Part:
    return Part(
        kind=PartKind.TOOL_RESULT,
        tool_result=ToolResult(
            tool_call_id=part.tool_call_id,
            name=part.tool_name or "",
            content=part.model_response(),
            is_error=True,
        ),
    )
