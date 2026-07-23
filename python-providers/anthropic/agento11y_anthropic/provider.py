"""Anthropic strict wrapper helpers and payload mappers."""

from __future__ import annotations

import json
from collections.abc import Awaitable, Callable, Iterable, Mapping
from dataclasses import asdict, dataclass, field, is_dataclass
from typing import TYPE_CHECKING, Any

from agento11y import (
    Artifact,
    ArtifactKind,
    Generation,
    GenerationMode,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    ToolCall,
    ToolDefinition,
    ToolResult,
)
from agento11y.usage import from_anthropic

if TYPE_CHECKING:
    from anthropic.types.message import Message as AnthropicMessage
    from anthropic.types.message_create_params import MessageCreateParams
    from anthropic.types.raw_message_stream_event import RawMessageStreamEvent
else:
    AnthropicMessage = Any
    MessageCreateParams = Any
    RawMessageStreamEvent = Any

_thinking_budget_metadata_key = "agento11y.gen_ai.request.thinking.budget_tokens"
_usage_server_tool_use_web_search_metadata_key = "agento11y.gen_ai.usage.server_tool_use.web_search_requests"
_usage_server_tool_use_web_fetch_metadata_key = "agento11y.gen_ai.usage.server_tool_use.web_fetch_requests"
_usage_server_tool_use_total_metadata_key = "agento11y.gen_ai.usage.server_tool_use.total_requests"


@dataclass(slots=True)
class AnthropicOptions:
    """Optional agento11y enrichments for Anthropic wrappers."""

    provider_name: str = "anthropic"
    conversation_id: str = ""
    agent_name: str = ""
    agent_version: str = ""
    tags: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)
    raw_artifacts: bool = False


@dataclass(slots=True)
class AnthropicStreamSummary:
    """Streaming summary for Anthropic messages API flows."""

    final_response: AnthropicMessage | None = None
    events: list[RawMessageStreamEvent] = field(default_factory=list)
    output_text: str = ""


def _messages_create(
    client,
    request: MessageCreateParams,
    provider_call: Callable[[MessageCreateParams], AnthropicMessage],
    options: AnthropicOptions | None = None,
) -> AnthropicMessage:
    opts = options or AnthropicOptions()
    recorder = client.start_generation(_start_payload(request, opts, GenerationMode.SYNC))

    try:
        response = provider_call(request)
        recorder.set_result(_messages_from_request_response(request, response, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


async def _messages_create_async(
    client,
    request: MessageCreateParams,
    provider_call: Callable[[MessageCreateParams], Awaitable[AnthropicMessage]],
    options: AnthropicOptions | None = None,
) -> AnthropicMessage:
    opts = options or AnthropicOptions()
    recorder = client.start_generation(_start_payload(request, opts, GenerationMode.SYNC))

    try:
        response = await provider_call(request)
        recorder.set_result(_messages_from_request_response(request, response, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


def _messages_stream(
    client,
    request: MessageCreateParams,
    provider_call: Callable[[MessageCreateParams], AnthropicStreamSummary],
    options: AnthropicOptions | None = None,
) -> AnthropicStreamSummary:
    opts = options or AnthropicOptions()
    recorder = client.start_streaming_generation(_start_payload(request, opts, GenerationMode.STREAM))

    try:
        summary = provider_call(request)
        recorder.set_result(_messages_from_stream(request, summary, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return summary


async def _messages_stream_async(
    client,
    request: MessageCreateParams,
    provider_call: Callable[[MessageCreateParams], Awaitable[AnthropicStreamSummary]],
    options: AnthropicOptions | None = None,
) -> AnthropicStreamSummary:
    opts = options or AnthropicOptions()
    recorder = client.start_streaming_generation(_start_payload(request, opts, GenerationMode.STREAM))

    try:
        summary = await provider_call(request)
        recorder.set_result(_messages_from_stream(request, summary, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return summary


def _messages_from_request_response(
    request: MessageCreateParams,
    response: AnthropicMessage,
    options: AnthropicOptions | None = None,
) -> Generation:
    opts = options or AnthropicOptions()
    thinking_budget = _anthropic_thinking_budget(_read(request, "thinking"))
    input_messages, system_prompt = _map_input_messages(request)
    output_message = _map_output_message(response)
    tools = _map_tools(_read(request, "tools"))
    usage = from_anthropic(_read(response, "usage"))
    usage_metadata = _anthropic_usage_metadata(_read(response, "usage"))

    generation = Generation(
        conversation_id=opts.conversation_id,
        agent_name=opts.agent_name,
        agent_version=opts.agent_version,
        mode=GenerationMode.SYNC,
        model=ModelRef(provider=opts.provider_name, name=_as_str(_read(request, "model"))),
        response_id=_as_str(_read(response, "id")),
        response_model=_as_str(_read(response, "model")) or _as_str(_read(request, "model")),
        system_prompt=system_prompt,
        max_tokens=_as_int_or_none(_read(request, "max_tokens")),
        temperature=_as_float_or_none(_read(request, "temperature")),
        top_p=_as_float_or_none(_read(request, "top_p")),
        tool_choice=_canonical_tool_choice(_read(request, "tool_choice")),
        thinking_enabled=_anthropic_thinking_enabled(_read(request, "thinking")),
        input=input_messages,
        output=[output_message] if output_message is not None else [],
        tools=tools,
        usage=usage,
        stop_reason=_as_str(_read(response, "stop_reason")),
        tags=dict(opts.tags),
        metadata=_merge_metadata(
            _metadata_with_thinking_budget(opts.metadata, thinking_budget),
            usage_metadata,
        ),
    )

    if opts.raw_artifacts:
        generation.artifacts = [
            _json_artifact(ArtifactKind.REQUEST, "anthropic.request", request),
            _json_artifact(ArtifactKind.RESPONSE, "anthropic.response", response),
        ]
        if generation.tools:
            generation.artifacts.append(_json_artifact(ArtifactKind.TOOLS, "anthropic.tools", generation.tools))

    return generation


def _messages_from_stream(
    request: MessageCreateParams,
    summary: AnthropicStreamSummary,
    options: AnthropicOptions | None = None,
) -> Generation:
    opts = options or AnthropicOptions()

    if summary.final_response is not None:
        generation = _messages_from_request_response(request, summary.final_response, opts)
        generation.mode = GenerationMode.STREAM
    else:
        input_messages, system_prompt = _map_input_messages(request)
        stream_usage_metadata = _anthropic_stream_usage_metadata(summary.events)
        generation = Generation(
            conversation_id=opts.conversation_id,
            agent_name=opts.agent_name,
            agent_version=opts.agent_version,
            mode=GenerationMode.STREAM,
            model=ModelRef(provider=opts.provider_name, name=_as_str(_read(request, "model"))),
            response_model=_as_str(_read(request, "model")),
            system_prompt=system_prompt,
            max_tokens=_as_int_or_none(_read(request, "max_tokens")),
            temperature=_as_float_or_none(_read(request, "temperature")),
            top_p=_as_float_or_none(_read(request, "top_p")),
            tool_choice=_canonical_tool_choice(_read(request, "tool_choice")),
            thinking_enabled=_anthropic_thinking_enabled(_read(request, "thinking")),
            input=input_messages,
            output=[],
            tools=_map_tools(_read(request, "tools")),
            tags=dict(opts.tags),
            metadata=_merge_metadata(
                _metadata_with_thinking_budget(
                    opts.metadata,
                    _anthropic_thinking_budget(_read(request, "thinking")),
                ),
                stream_usage_metadata,
            ),
        )

    output_text = summary.output_text.strip() or _extract_stream_output_text(summary.events)
    if output_text:
        generation.output = [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=output_text)])]

    if opts.raw_artifacts:
        if not any(artifact.kind == ArtifactKind.REQUEST for artifact in generation.artifacts):
            generation.artifacts.append(_json_artifact(ArtifactKind.REQUEST, "anthropic.request", request))
        if generation.tools and not any(artifact.kind == ArtifactKind.TOOLS for artifact in generation.artifacts):
            generation.artifacts.append(_json_artifact(ArtifactKind.TOOLS, "anthropic.tools", generation.tools))
        generation.artifacts.append(
            _json_artifact(ArtifactKind.PROVIDER_EVENT, "anthropic.stream.events", summary.events)
        )

    return generation


def _start_payload(request: MessageCreateParams, options: AnthropicOptions, mode: GenerationMode) -> GenerationStart:
    input_messages, system_prompt = _map_input_messages(request)
    del input_messages
    return GenerationStart(
        conversation_id=options.conversation_id,
        agent_name=options.agent_name,
        agent_version=options.agent_version,
        mode=mode,
        model=ModelRef(provider=options.provider_name, name=_as_str(_read(request, "model"))),
        system_prompt=system_prompt,
        max_tokens=_as_int_or_none(_read(request, "max_tokens")),
        temperature=_as_float_or_none(_read(request, "temperature")),
        top_p=_as_float_or_none(_read(request, "top_p")),
        tool_choice=_canonical_tool_choice(_read(request, "tool_choice")),
        thinking_enabled=_anthropic_thinking_enabled(_read(request, "thinking")),
        tools=_map_tools(_read(request, "tools")),
        tags=dict(options.tags),
        metadata=_metadata_with_thinking_budget(
            options.metadata,
            _anthropic_thinking_budget(_read(request, "thinking")),
        ),
    )


def _map_input_messages(request: MessageCreateParams) -> tuple[list[Message], str]:
    mapped: list[Message] = []
    system_prompt = _extract_system_prompt(_read(request, "system"))

    for raw_message in _as_list(_read(request, "messages")):
        role = _normalize_role(_as_str(_read(raw_message, "role")))
        parts = _map_parts(_read(raw_message, "content"), role_hint=role)
        if not parts:
            text = _extract_text(_read(raw_message, "content"))
            if text:
                parts = [Part(kind=PartKind.TEXT, text=text)]

        if parts:
            mapped_role = role
            if any(part.kind == PartKind.TOOL_RESULT for part in parts):
                mapped_role = MessageRole.TOOL
            mapped.append(
                Message(
                    role=mapped_role,
                    name=_as_str(_read(raw_message, "name")),
                    parts=parts,
                )
            )

    return mapped, system_prompt


def _map_output_message(response: AnthropicMessage) -> Message | None:
    parts = _map_parts(_read(response, "content"), role_hint=MessageRole.ASSISTANT)
    if not parts:
        text = _extract_text(_read(response, "content"))
        if text:
            parts = [Part(kind=PartKind.TEXT, text=text)]

    if not parts:
        return None

    return Message(
        role=MessageRole.ASSISTANT,
        parts=parts,
    )


def _map_parts(content: Any, role_hint: MessageRole) -> list[Part]:
    blocks = _as_list(content)
    if not blocks and isinstance(content, str):
        text = content.strip()
        return [Part(kind=PartKind.TEXT, text=text)] if text else []

    parts: list[Part] = []
    for block in blocks:
        block_type = _as_str(_read(block, "type")).lower()
        if block_type == "text":
            text = _as_str(_read(block, "text"))
            if text:
                parts.append(Part(kind=PartKind.TEXT, text=text))
            continue

        if block_type == "thinking":
            thinking = _as_str(_read(block, "thinking")) or _as_str(_read(block, "text"))
            if thinking:
                parts.append(Part(kind=PartKind.THINKING, thinking=thinking))
            continue

        if block_type == "redacted_thinking":
            thinking = _as_str(_read(block, "data")) or _as_str(_read(block, "text"))
            if thinking:
                parts.append(Part(kind=PartKind.THINKING, thinking=thinking))
            continue

        if block_type in {"tool_use", "server_tool_use", "mcp_tool_use"}:
            tool_name = _as_str(_read(block, "name"))
            tool_id = _as_str(_read(block, "id"))
            tool_input = _read(block, "input")
            parts.append(
                Part(
                    kind=PartKind.TOOL_CALL,
                    tool_call=ToolCall(
                        id=tool_id,
                        name=tool_name,
                        input_json=_json_bytes(tool_input),
                    ),
                )
            )
            continue

        if block_type == "tool_result" or role_hint == MessageRole.TOOL:
            tool_call_id = _as_str(_read(block, "tool_use_id")) or _as_str(_read(block, "tool_call_id"))
            content_text = _extract_text(_read(block, "content"))
            parts.append(
                Part(
                    kind=PartKind.TOOL_RESULT,
                    tool_result=ToolResult(
                        tool_call_id=tool_call_id,
                        name=_as_str(_read(block, "name")),
                        content=content_text,
                        content_json=_json_bytes(_read(block, "content")),
                        is_error=_as_bool(_read(block, "is_error")),
                    ),
                )
            )
            continue

    return parts


def _extract_system_prompt(raw_system: Any) -> str:
    if raw_system is None:
        return ""

    if isinstance(raw_system, str):
        return raw_system.strip()

    chunks: list[str] = []
    for block in _as_list(raw_system):
        text = _as_str(_read(block, "text"))
        if text:
            chunks.append(text)

    return "\n".join(chunks)


def _map_tools(raw_tools: Any) -> list[ToolDefinition]:
    tools: list[ToolDefinition] = []
    for raw_tool in _as_list(raw_tools):
        name = _as_str(_read(raw_tool, "name"))
        if not name:
            continue
        tools.append(
            ToolDefinition(
                name=name,
                description=_as_str(_read(raw_tool, "description")),
                type=_as_str(_read(raw_tool, "type")) or "function",
                input_schema_json=_json_bytes(_read(raw_tool, "input_schema")),
            )
        )

    return tools


def _extract_stream_output_text(events: list[RawMessageStreamEvent]) -> str:
    chunks: list[str] = []
    for event in events:
        event_type = _as_str(_read(event, "type"))
        if event_type == "content_block_delta":
            delta = _read(event, "delta")
            delta_type = _as_str(_read(delta, "type"))
            if delta_type == "text_delta":
                text = _as_str(_read(delta, "text"))
                if text:
                    chunks.append(text)
                    continue

            text = _as_str(_read(delta, "text"))
            if text:
                chunks.append(text)
                continue

        text = _extract_text(_read(event, "text"))
        if text:
            chunks.append(text)

    return "".join(chunks)


def _normalize_role(role: str) -> MessageRole:
    normalized = role.strip().lower()
    if normalized == "assistant":
        return MessageRole.ASSISTANT
    if normalized == "tool":
        return MessageRole.TOOL
    return MessageRole.USER


def _canonical_tool_choice(value: Any) -> str | None:
    if value is None:
        return None

    if isinstance(value, str):
        normalized = value.strip().lower()
        return normalized or None

    if hasattr(value, "value"):
        normalized = str(value.value).strip().lower()
        return normalized or None

    try:
        encoded = json.dumps(_to_plain(value), separators=(",", ":"), sort_keys=True)
    except Exception:  # noqa: BLE001
        fallback = str(value).strip().lower()
        return fallback or None

    return encoded or None


def _anthropic_thinking_enabled(thinking: Any) -> bool | None:
    if thinking is None:
        return None

    if isinstance(thinking, bool):
        return thinking

    if isinstance(thinking, str):
        normalized = thinking.strip().lower()
        if normalized in {"enabled", "adaptive"}:
            return True
        if normalized == "disabled":
            return False
        return None

    type_value = _as_str(_read(thinking, "type")).lower()
    if type_value in {"enabled", "adaptive"}:
        return True
    if type_value == "disabled":
        return False

    return None


def _anthropic_thinking_budget(thinking: Any) -> int | None:
    return _as_int_or_none(_read(thinking, "budget_tokens"))


def _metadata_with_thinking_budget(metadata: Mapping[str, Any], thinking_budget: int | None) -> dict[str, Any]:
    out = dict(metadata)
    if thinking_budget is not None:
        out[_thinking_budget_metadata_key] = thinking_budget
    return out


def _anthropic_usage_metadata(raw_usage: Any) -> dict[str, Any]:
    server_tool_use = _read(raw_usage, "server_tool_use")
    web_search_requests = _as_int_or_none(_read(server_tool_use, "web_search_requests")) or 0
    web_fetch_requests = _as_int_or_none(_read(server_tool_use, "web_fetch_requests")) or 0
    total_requests = web_search_requests + web_fetch_requests
    if total_requests <= 0:
        return {}

    out: dict[str, Any] = {
        _usage_server_tool_use_total_metadata_key: total_requests,
    }
    if web_search_requests > 0:
        out[_usage_server_tool_use_web_search_metadata_key] = web_search_requests
    if web_fetch_requests > 0:
        out[_usage_server_tool_use_web_fetch_metadata_key] = web_fetch_requests
    return out


def _anthropic_stream_usage_metadata(events: list[RawMessageStreamEvent]) -> dict[str, Any]:
    for event in reversed(events):
        if _as_str(_read(event, "type")).lower() != "message_delta":
            continue
        metadata = _anthropic_usage_metadata(_read(event, "usage"))
        if metadata:
            return metadata
    return {}


def _merge_metadata(base: Mapping[str, Any], extra: Mapping[str, Any]) -> dict[str, Any]:
    merged = dict(base)
    merged.update(extra)
    return merged


def _json_artifact(kind: ArtifactKind, name: str, payload: Any) -> Artifact:
    return Artifact(
        kind=kind,
        name=name,
        content_type="application/json",
        payload=_json_bytes(payload),
    )


def _json_bytes(value: Any) -> bytes:
    return json.dumps(_to_plain(value), separators=(",", ":"), sort_keys=True, default=str).encode("utf-8")


def _to_plain(value: Any) -> Any:
    if value is None:
        return None

    if isinstance(value, (str, int, float, bool)):
        return value

    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace")

    if isinstance(value, Mapping):
        return {str(key): _to_plain(inner) for key, inner in value.items()}

    if isinstance(value, list | tuple):
        return [_to_plain(inner) for inner in value]

    if is_dataclass(value):
        return {key: _to_plain(inner) for key, inner in asdict(value).items()}

    model_dump = getattr(value, "model_dump", None)
    if callable(model_dump):
        try:
            dumped = model_dump(mode="json")
        except TypeError:
            dumped = model_dump()
        return _to_plain(dumped)

    to_dict = getattr(value, "to_dict", None)
    if callable(to_dict):
        return _to_plain(to_dict())

    dict_method = getattr(value, "dict", None)
    if callable(dict_method):
        return _to_plain(dict_method())

    if hasattr(value, "__dict__"):
        return _to_plain(vars(value))

    return str(value)


def _extract_text(value: Any) -> str:
    if value is None:
        return ""

    if isinstance(value, str):
        return value.strip()

    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace").strip()

    if isinstance(value, Mapping):
        text = _as_str(value.get("text"))
        if text:
            return text
        content = value.get("content")
        if content is not None:
            return _extract_text(content)
        return ""

    if isinstance(value, list | tuple):
        chunks: list[str] = []
        for item in value:
            chunk = _extract_text(item)
            if chunk:
                chunks.append(chunk)
        return "\n".join(chunks)

    model_dump = getattr(value, "model_dump", None)
    if callable(model_dump):
        try:
            return _extract_text(model_dump(mode="json"))
        except TypeError:
            return _extract_text(model_dump())

    return _as_str(value)


def _read(value: Any, key: str, default: Any = None) -> Any:
    if value is None:
        return default

    if isinstance(value, Mapping):
        return value.get(key, default)

    if hasattr(value, key):
        return getattr(value, key)

    getter = getattr(value, "get", None)
    if callable(getter):
        try:
            return getter(key, default)
        except Exception:  # noqa: BLE001
            return default

    return default


def _as_list(value: Any) -> list[Any]:
    if value is None:
        return []
    if isinstance(value, list):
        return value
    if isinstance(value, tuple):
        return list(value)
    if isinstance(value, Iterable) and not isinstance(value, (str, bytes, Mapping)):
        return list(value)
    return []


def _as_str(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value.strip()
    return str(value).strip()


def _as_bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        lowered = value.strip().lower()
        if lowered in {"true", "1", "yes", "on"}:
            return True
        if lowered in {"false", "0", "no", "off"}:
            return False
    return False


def _as_int(value: Any) -> int:
    converted = _as_int_or_none(value)
    return converted if converted is not None else 0


def _as_int_or_none(value: Any) -> int | None:
    if value is None or isinstance(value, bool):
        return None

    if isinstance(value, int):
        return value

    if isinstance(value, float):
        integer = int(value)
        if float(integer) == value:
            return integer
        return None

    if isinstance(value, str):
        text = value.strip()
        if not text:
            return None
        try:
            return int(text)
        except ValueError:
            return None

    return None


def _as_float_or_none(value: Any) -> float | None:
    if value is None or isinstance(value, bool):
        return None

    if isinstance(value, (int, float)):
        return float(value)

    if isinstance(value, str):
        text = value.strip()
        if not text:
            return None
        try:
            return float(text)
        except ValueError:
            return None

    return None


class _MessagesNamespace:
    """Namespace for Anthropic messages wrappers and mappers."""

    create = staticmethod(_messages_create)
    create_async = staticmethod(_messages_create_async)
    stream = staticmethod(_messages_stream)
    stream_async = staticmethod(_messages_stream_async)
    from_request_response = staticmethod(_messages_from_request_response)
    from_stream = staticmethod(_messages_from_stream)


messages = _MessagesNamespace()
