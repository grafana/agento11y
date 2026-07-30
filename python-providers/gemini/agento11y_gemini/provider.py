"""Gemini strict wrapper helpers and payload mappers."""

from __future__ import annotations

import json
from collections.abc import Awaitable, Callable, Iterable, Mapping
from dataclasses import asdict, dataclass, field, is_dataclass
from typing import TYPE_CHECKING, Any

from agento11y import (
    Artifact,
    ArtifactKind,
    EmbeddingResult,
    EmbeddingStart,
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
from agento11y.usage import from_gemini

if TYPE_CHECKING:
    from google.genai import types as genai_types

    GenerateContentConfig = genai_types.GenerateContentConfigOrDict
    GenerateContentResponse = genai_types.GenerateContentResponse
    GeminiContent = genai_types.ContentListUnion
else:
    GenerateContentConfig = Any
    GenerateContentResponse = Any
    GeminiContent = Any

_thinking_budget_metadata_key = "agento11y.gen_ai.request.thinking.budget_tokens"
_thinking_level_metadata_key = "agento11y.gen_ai.request.thinking.level"
_usage_tool_use_prompt_tokens_metadata_key = "agento11y.gen_ai.usage.tool_use_prompt_tokens"


@dataclass(slots=True)
class GeminiOptions:
    """Optional agento11y enrichments for Gemini wrappers."""

    provider_name: str = "gemini"
    conversation_id: str = ""
    agent_name: str = ""
    agent_version: str = ""
    tags: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)
    raw_artifacts: bool = False


@dataclass(slots=True)
class GeminiStreamSummary:
    """Streaming summary for Gemini models API flows."""

    responses: list[GenerateContentResponse] = field(default_factory=list)
    output_text: str = ""
    final_response: GenerateContentResponse | None = None


def _models_generate_content(
    client,
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    provider_call: Callable[[str, list[GeminiContent], GenerateContentConfig | None], GenerateContentResponse],
    options: GeminiOptions | None = None,
) -> GenerateContentResponse:
    opts = options or GeminiOptions()
    recorder = client.start_generation(_start_payload(model, contents, config, opts, GenerationMode.SYNC))

    try:
        response = provider_call(model, contents, config)
        recorder.set_result(_models_from_request_response(model, contents, config, response, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


async def _models_generate_content_async(
    client,
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    provider_call: Callable[
        [str, list[GeminiContent], GenerateContentConfig | None],
        Awaitable[GenerateContentResponse],
    ],
    options: GeminiOptions | None = None,
) -> GenerateContentResponse:
    opts = options or GeminiOptions()
    recorder = client.start_generation(_start_payload(model, contents, config, opts, GenerationMode.SYNC))

    try:
        response = await provider_call(model, contents, config)
        recorder.set_result(_models_from_request_response(model, contents, config, response, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


def _models_generate_content_stream(
    client,
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    provider_call: Callable[[str, list[GeminiContent], GenerateContentConfig | None], GeminiStreamSummary],
    options: GeminiOptions | None = None,
) -> GeminiStreamSummary:
    opts = options or GeminiOptions()
    recorder = client.start_streaming_generation(_start_payload(model, contents, config, opts, GenerationMode.STREAM))

    try:
        summary = provider_call(model, contents, config)
        recorder.set_result(_models_from_stream(model, contents, config, summary, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return summary


async def _models_generate_content_stream_async(
    client,
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    provider_call: Callable[
        [str, list[GeminiContent], GenerateContentConfig | None],
        Awaitable[GeminiStreamSummary],
    ],
    options: GeminiOptions | None = None,
) -> GeminiStreamSummary:
    opts = options or GeminiOptions()
    recorder = client.start_streaming_generation(_start_payload(model, contents, config, opts, GenerationMode.STREAM))

    try:
        summary = await provider_call(model, contents, config)
        recorder.set_result(_models_from_stream(model, contents, config, summary, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return summary


def _models_embed_content(
    client,
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    provider_call: Callable[[str, list[GeminiContent], GenerateContentConfig | None], Any],
    options: GeminiOptions | None = None,
) -> Any:
    opts = options or GeminiOptions()
    recorder = client.start_embedding(_embedding_start_payload(model, config, opts))

    try:
        response = provider_call(model, contents, config)
        recorder.set_result(_embedding_from_response(model, contents, config, response))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


async def _models_embed_content_async(
    client,
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    provider_call: Callable[
        [str, list[GeminiContent], GenerateContentConfig | None],
        Awaitable[Any],
    ],
    options: GeminiOptions | None = None,
) -> Any:
    opts = options or GeminiOptions()
    recorder = client.start_embedding(_embedding_start_payload(model, config, opts))

    try:
        response = await provider_call(model, contents, config)
        recorder.set_result(_embedding_from_response(model, contents, config, response))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


def _embedding_from_response(
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    response: Any,
) -> EmbeddingResult:
    requested_dimensions = _embedding_requested_dimensions(config)
    result = EmbeddingResult(
        input_count=_embedding_input_count(contents),
        input_texts=_embedding_input_texts(contents),
    )

    if response is None:
        if requested_dimensions is not None and requested_dimensions > 0:
            result.dimensions = requested_dimensions
        return result

    embeddings = _as_list(_read(response, "embeddings"))
    input_tokens = 0
    for embedding in embeddings:
        statistics = _read(embedding, "statistics")
        token_count = _as_int_or_none(_read(statistics, "token_count"))
        if token_count is None:
            token_count = _as_int_or_none(_read(statistics, "tokenCount"))
        if token_count is not None and token_count > 0:
            input_tokens += token_count

        values = _as_list(_read(embedding, "values"))
        if result.dimensions is None and values:
            result.dimensions = len(values)

    if input_tokens > 0:
        result.input_tokens = input_tokens

    result.response_model = _as_str(_read(response, "model")) or _as_str(_read(response, "model_version"))
    if result.dimensions is None and requested_dimensions is not None and requested_dimensions > 0:
        result.dimensions = requested_dimensions
    return result


def _models_from_request_response(
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    response: GenerateContentResponse,
    options: GeminiOptions | None = None,
) -> Generation:
    opts = options or GeminiOptions()
    request_controls = _request_controls(config)
    usage = from_gemini(_read(response, "usage_metadata"))
    usage_metadata = _gemini_usage_metadata(_read(response, "usage_metadata"))
    output = _map_output_messages(response)
    if not output:
        text = _extract_response_text(response)
        if text:
            output = [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=text)])]

    generation = Generation(
        conversation_id=opts.conversation_id,
        agent_name=opts.agent_name,
        agent_version=opts.agent_version,
        mode=GenerationMode.SYNC,
        model=ModelRef(provider=opts.provider_name, name=model.strip()),
        response_id=_as_str(_read(response, "response_id")),
        response_model=_as_str(_read(response, "model_version")) or model.strip(),
        system_prompt=_extract_system_prompt(config),
        max_tokens=request_controls.max_tokens,
        temperature=request_controls.temperature,
        top_p=request_controls.top_p,
        tool_choice=request_controls.tool_choice,
        thinking_enabled=request_controls.thinking_enabled,
        input=_map_input_messages(contents),
        output=output,
        tools=_map_tools(config),
        usage=usage,
        stop_reason=_response_stop_reason(response),
        tags=dict(opts.tags),
        metadata=_merge_metadata(
            _metadata_with_thinking_budget(
                opts.metadata,
                request_controls.thinking_budget,
                request_controls.thinking_level,
            ),
            usage_metadata,
        ),
    )

    if opts.raw_artifacts:
        generation.artifacts = [
            _json_artifact(
                ArtifactKind.REQUEST,
                "gemini.request",
                {
                    "model": model,
                    "contents": contents,
                    "config": config,
                },
            ),
            _json_artifact(ArtifactKind.RESPONSE, "gemini.response", response),
        ]
        if generation.tools:
            generation.artifacts.append(_json_artifact(ArtifactKind.TOOLS, "gemini.tools", generation.tools))

    return generation


def _models_from_stream(
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    summary: GeminiStreamSummary,
    options: GeminiOptions | None = None,
) -> Generation:
    opts = options or GeminiOptions()

    final_response = summary.final_response
    if final_response is None and summary.responses:
        final_response = summary.responses[-1]

    if final_response is not None:
        generation = _models_from_request_response(model, contents, config, final_response, opts)
        generation.mode = GenerationMode.STREAM
    else:
        controls = _request_controls(config)
        stream_usage_metadata = _gemini_stream_usage_metadata(summary.responses)
        generation = Generation(
            conversation_id=opts.conversation_id,
            agent_name=opts.agent_name,
            agent_version=opts.agent_version,
            mode=GenerationMode.STREAM,
            model=ModelRef(provider=opts.provider_name, name=model.strip()),
            response_model=model.strip(),
            system_prompt=_extract_system_prompt(config),
            max_tokens=controls.max_tokens,
            temperature=controls.temperature,
            top_p=controls.top_p,
            tool_choice=controls.tool_choice,
            thinking_enabled=controls.thinking_enabled,
            input=_map_input_messages(contents),
            output=[],
            tools=_map_tools(config),
            tags=dict(opts.tags),
            metadata=_merge_metadata(
                _metadata_with_thinking_budget(opts.metadata, controls.thinking_budget, controls.thinking_level),
                stream_usage_metadata,
            ),
        )

    output_text = summary.output_text.strip() or _extract_stream_output_text(summary.responses)
    if output_text:
        generation.output = [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=output_text)])]

    if opts.raw_artifacts:
        if not any(artifact.kind == ArtifactKind.REQUEST for artifact in generation.artifacts):
            generation.artifacts.append(
                _json_artifact(
                    ArtifactKind.REQUEST,
                    "gemini.request",
                    {
                        "model": model,
                        "contents": contents,
                        "config": config,
                    },
                )
            )
        if generation.tools and not any(artifact.kind == ArtifactKind.TOOLS for artifact in generation.artifacts):
            generation.artifacts.append(_json_artifact(ArtifactKind.TOOLS, "gemini.tools", generation.tools))
        generation.artifacts.append(
            _json_artifact(ArtifactKind.PROVIDER_EVENT, "gemini.stream.events", summary.responses)
        )

    return generation


def _start_payload(
    model: str,
    contents: list[GeminiContent],
    config: GenerateContentConfig | None,
    options: GeminiOptions,
    mode: GenerationMode,
) -> GenerationStart:
    del contents
    controls = _request_controls(config)
    return GenerationStart(
        conversation_id=options.conversation_id,
        agent_name=options.agent_name,
        agent_version=options.agent_version,
        mode=mode,
        model=ModelRef(provider=options.provider_name, name=model.strip()),
        system_prompt=_extract_system_prompt(config),
        max_tokens=controls.max_tokens,
        temperature=controls.temperature,
        top_p=controls.top_p,
        tool_choice=controls.tool_choice,
        thinking_enabled=controls.thinking_enabled,
        tools=_map_tools(config),
        tags=dict(options.tags),
        metadata=_metadata_with_thinking_budget(options.metadata, controls.thinking_budget, controls.thinking_level),
    )


def _embedding_start_payload(
    model: str,
    config: GenerateContentConfig | None,
    options: GeminiOptions,
) -> EmbeddingStart:
    return EmbeddingStart(
        agent_name=options.agent_name,
        agent_version=options.agent_version,
        model=ModelRef(provider=options.provider_name, name=model.strip()),
        dimensions=_embedding_requested_dimensions(config),
        tags=dict(options.tags),
        metadata=dict(options.metadata),
    )


@dataclass(slots=True)
class _RequestControls:
    max_tokens: int | None
    temperature: float | None
    top_p: float | None
    tool_choice: str | None
    thinking_enabled: bool | None
    thinking_budget: int | None
    thinking_level: str | None


def _request_controls(config: GenerateContentConfig | None) -> _RequestControls:
    tool_config = _read(config, "tool_config")
    function_calling = _read(tool_config, "function_calling_config")
    thinking_config = _read(config, "thinking_config")

    return _RequestControls(
        max_tokens=_as_int_or_none(_read(config, "max_output_tokens")),
        temperature=_as_float_or_none(_read(config, "temperature")),
        top_p=_as_float_or_none(_read(config, "top_p")),
        tool_choice=_canonical_tool_choice(_read(function_calling, "mode")),
        thinking_enabled=_as_bool_or_none(
            _first_not_none(
                _read(thinking_config, "include_thoughts"),
                _read(thinking_config, "includeThoughts"),
            )
        ),
        thinking_budget=_as_int_or_none(
            _first_not_none(
                _read(thinking_config, "thinking_budget"),
                _read(thinking_config, "thinkingBudget"),
            )
        ),
        thinking_level=_gemini_thinking_level(
            _first_not_none(
                _read(thinking_config, "thinking_level"),
                _read(thinking_config, "thinkingLevel"),
            )
        ),
    )


def _embedding_requested_dimensions(config: GenerateContentConfig | None) -> int | None:
    value = _read(config, "output_dimensionality")
    if value is None:
        value = _read(config, "outputDimensionality")
    return _as_int_or_none(value)


def _embedding_input_count(contents: list[GeminiContent]) -> int:
    count = 0
    for content in _as_list(contents):
        if content is not None:
            count += 1
    return count


def _embedding_input_texts(contents: list[GeminiContent]) -> list[str]:
    out: list[str] = []
    for content in _as_list(contents):
        text = _extract_text(_read(content, "parts") if _read(content, "parts") is not None else content)
        text = text.strip()
        if text:
            out.append(text)
    return out


def _map_input_messages(contents: list[GeminiContent]) -> list[Message]:
    mapped: list[Message] = []
    for raw_content in _as_list(contents):
        role = _normalize_role(_as_str(_read(raw_content, "role")))
        parts = _map_parts(_read(raw_content, "parts"), role)
        if parts:
            mapped_role = role
            if any(part.kind == PartKind.TOOL_RESULT for part in parts):
                mapped_role = MessageRole.TOOL
            mapped.append(Message(role=mapped_role, parts=parts))
    return mapped


def _map_output_messages(response: GenerateContentResponse) -> list[Message]:
    mapped: list[Message] = []
    for candidate in _as_list(_read(response, "candidates")):
        content = _read(candidate, "content")
        role = _normalize_role(_as_str(_read(content, "role")) or "assistant")
        parts = _map_parts(_read(content, "parts"), role)
        if parts:
            mapped.append(Message(role=role, parts=parts))
    return mapped


def _map_parts(raw_parts: Any, role: MessageRole) -> list[Part]:
    parts: list[Part] = []
    for raw_part in _as_list(raw_parts):
        text = _as_str(_read(raw_part, "text"))
        if text:
            if _as_bool(_read(raw_part, "thought")) and role == MessageRole.ASSISTANT:
                parts.append(Part(kind=PartKind.THINKING, thinking=text))
            else:
                parts.append(Part(kind=PartKind.TEXT, text=text))

        function_call = _read(raw_part, "function_call")
        if function_call is not None:
            parts.append(
                Part(
                    kind=PartKind.TOOL_CALL,
                    tool_call=ToolCall(
                        id=_as_str(_read(function_call, "id")),
                        name=_as_str(_read(function_call, "name")),
                        input_json=_json_bytes(_read(function_call, "args")),
                    ),
                )
            )

        function_response = _read(raw_part, "function_response")
        if function_response is not None:
            response_payload = _read(function_response, "response")
            parts.append(
                Part(
                    kind=PartKind.TOOL_RESULT,
                    tool_result=ToolResult(
                        tool_call_id=_as_str(_read(function_response, "id")),
                        name=_as_str(_read(function_response, "name")),
                        content=_extract_text(response_payload),
                        content_json=_json_bytes(response_payload),
                        is_error=_as_bool(_read(function_response, "is_error")),
                    ),
                )
            )

    return parts


def _extract_system_prompt(config: GenerateContentConfig | None) -> str:
    instruction = _read(config, "system_instruction")
    if instruction is None:
        return ""

    parts = _as_list(_read(instruction, "parts"))
    if not parts:
        return _extract_text(instruction)

    chunks: list[str] = []
    for part in parts:
        text = _as_str(_read(part, "text"))
        if text:
            chunks.append(text)
    return "\n".join(chunks)


def _map_tools(config: GenerateContentConfig | None) -> list[ToolDefinition]:
    mapped: list[ToolDefinition] = []
    for tool in _as_list(_read(config, "tools")):
        declarations = _as_list(_read(tool, "function_declarations"))
        for declaration in declarations:
            name = _as_str(_read(declaration, "name"))
            if not name:
                continue
            mapped.append(
                ToolDefinition(
                    name=name,
                    description=_as_str(_read(declaration, "description")),
                    type="function",
                    input_schema_json=_json_bytes(_read(declaration, "parameters_json_schema")),
                )
            )

    return mapped


def _response_stop_reason(response: GenerateContentResponse) -> str:
    stop_reason = ""
    for candidate in _as_list(_read(response, "candidates")):
        reason = _as_str(_read(candidate, "finish_reason"))
        if reason:
            stop_reason = reason.upper()
    return stop_reason


def _extract_response_text(response: GenerateContentResponse) -> str:
    chunks: list[str] = []
    for candidate in _as_list(_read(response, "candidates")):
        content = _read(candidate, "content")
        for part in _as_list(_read(content, "parts")):
            text = _as_str(_read(part, "text"))
            if text:
                chunks.append(text)
    return "\n".join(chunks)


def _extract_stream_output_text(responses: list[GenerateContentResponse]) -> str:
    chunks: list[str] = []
    for response in responses:
        text = _extract_response_text(response)
        if text:
            chunks.append(text)
    return "\n".join(chunks)


def _normalize_role(role: str) -> MessageRole:
    normalized = role.strip().lower()
    if normalized == "assistant" or normalized == "model":
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


def _metadata_with_thinking_budget(
    metadata: Mapping[str, Any],
    thinking_budget: int | None,
    thinking_level: str | None,
) -> dict[str, Any]:
    out = dict(metadata)
    if thinking_budget is not None:
        out[_thinking_budget_metadata_key] = thinking_budget
    if thinking_level is not None:
        out[_thinking_level_metadata_key] = thinking_level
    return out


def _gemini_thinking_level(value: Any) -> str | None:
    normalized = _as_str(value).strip().lower()
    if not normalized or normalized == "thinking_level_unspecified":
        return None
    if normalized in {"thinking_level_low", "low"}:
        return "low"
    if normalized in {"thinking_level_medium", "medium"}:
        return "medium"
    if normalized in {"thinking_level_high", "high"}:
        return "high"
    if normalized in {"thinking_level_minimal", "minimal"}:
        return "minimal"
    return normalized


def _gemini_usage_metadata(raw_usage: Any) -> dict[str, Any]:
    tool_use_prompt_tokens = _as_int_or_none(
        _read(raw_usage, "tool_use_prompt_token_count") or _read(raw_usage, "toolUsePromptTokenCount")
    )
    if tool_use_prompt_tokens is None:
        return {}
    if tool_use_prompt_tokens <= 0:
        return {}
    return {
        _usage_tool_use_prompt_tokens_metadata_key: tool_use_prompt_tokens,
    }


def _gemini_stream_usage_metadata(responses: list[GenerateContentResponse]) -> dict[str, Any]:
    for response in reversed(responses):
        metadata = _gemini_usage_metadata(_read(response, "usage_metadata"))
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

    to_json_dict = getattr(value, "to_json_dict", None)
    if callable(to_json_dict):
        return _to_plain(to_json_dict())

    to_dict = getattr(value, "to_dict", None)
    if callable(to_dict):
        return _to_plain(to_dict())

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


def _as_bool_or_none(value: Any) -> bool | None:
    if value is None:
        return None
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        lowered = value.strip().lower()
        if lowered in {"true", "1", "yes", "on"}:
            return True
        if lowered in {"false", "0", "no", "off"}:
            return False
    return None


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


def _first_not_none(*values: Any) -> Any:
    for value in values:
        if value is not None:
            return value
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


class _ModelsNamespace:
    """Namespace for Gemini models wrappers and mappers."""

    generate_content = staticmethod(_models_generate_content)
    generate_content_async = staticmethod(_models_generate_content_async)
    generate_content_stream = staticmethod(_models_generate_content_stream)
    generate_content_stream_async = staticmethod(_models_generate_content_stream_async)
    embed_content = staticmethod(_models_embed_content)
    embed_content_async = staticmethod(_models_embed_content_async)
    from_request_response = staticmethod(_models_from_request_response)
    from_stream = staticmethod(_models_from_stream)
    embedding_from_response = staticmethod(_embedding_from_response)


models = _ModelsNamespace()
