"""OpenAI provider wrappers and strict mappers for agento11y Python SDK."""

from __future__ import annotations

import json
from collections.abc import Awaitable, Callable, Mapping
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
from agento11y.usage import from_openai_chat, from_openai_responses

if TYPE_CHECKING:
    from openai.types.chat import ChatCompletion
    from openai.types.chat.chat_completion_chunk import ChatCompletionChunk
    from openai.types.chat.completion_create_params import (
        CompletionCreateParamsNonStreaming,
        CompletionCreateParamsStreaming,
    )
    from openai.types.responses.response import Response
    from openai.types.responses.response_create_params import (
        ResponseCreateParamsNonStreaming,
        ResponseCreateParamsStreaming,
    )
    from openai.types.responses.response_stream_event import ResponseStreamEvent

    ChatCreateRequest = CompletionCreateParamsNonStreaming
    ChatStreamRequest = CompletionCreateParamsStreaming
    ChatCreateResponse = ChatCompletion
    ChatStreamEvent = ChatCompletionChunk

    ResponsesCreateRequest = ResponseCreateParamsNonStreaming
    ResponsesStreamRequest = ResponseCreateParamsStreaming
    ResponsesCreateResponse = Response
    ResponsesStreamEvent = ResponseStreamEvent
else:
    ChatCreateRequest = Any
    ChatStreamRequest = Any
    ChatCreateResponse = Any
    ChatStreamEvent = Any

    ResponsesCreateRequest = Any
    ResponsesStreamRequest = Any
    ResponsesCreateResponse = Any
    ResponsesStreamEvent = Any

_thinking_budget_metadata_key = "agento11y.gen_ai.request.thinking.budget_tokens"


@dataclass(slots=True)
class OpenAIOptions:
    """Optional agento11y enrichments for OpenAI wrappers."""

    provider_name: str = "openai"
    conversation_id: str = ""
    agent_name: str = ""
    agent_version: str = ""
    tags: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)
    raw_artifacts: bool = False


@dataclass(slots=True)
class ChatCompletionsStreamSummary:
    """Streaming summary for chat-completions flow."""

    final_response: ChatCreateResponse | None = None
    events: list[ChatStreamEvent] = field(default_factory=list)
    output_text: str = ""


@dataclass(slots=True)
class ResponsesStreamSummary:
    """Streaming summary for responses flow."""

    final_response: ResponsesCreateResponse | None = None
    events: list[ResponsesStreamEvent] = field(default_factory=list)
    output_text: str = ""


def _chat_completions_create(
    client,
    request: ChatCreateRequest,
    provider_call: Callable[[ChatCreateRequest], ChatCreateResponse],
    options: OpenAIOptions | None = None,
) -> ChatCreateResponse:
    opts = options or OpenAIOptions()
    start = _chat_start_payload(request, opts, GenerationMode.SYNC)
    recorder = client.start_generation(start)

    try:
        response = provider_call(request)
        recorder.set_result(_chat_from_request_response(request, response, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


async def _chat_completions_create_async(
    client,
    request: ChatCreateRequest,
    provider_call: Callable[[ChatCreateRequest], Awaitable[ChatCreateResponse]],
    options: OpenAIOptions | None = None,
) -> ChatCreateResponse:
    opts = options or OpenAIOptions()
    start = _chat_start_payload(request, opts, GenerationMode.SYNC)
    recorder = client.start_generation(start)

    try:
        response = await provider_call(request)
        recorder.set_result(_chat_from_request_response(request, response, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


def _chat_completions_stream(
    client,
    request: ChatStreamRequest,
    provider_call: Callable[[ChatStreamRequest], ChatCompletionsStreamSummary],
    options: OpenAIOptions | None = None,
) -> ChatCompletionsStreamSummary:
    opts = options or OpenAIOptions()
    start = _chat_start_payload(request, opts, GenerationMode.STREAM)
    recorder = client.start_streaming_generation(start)

    try:
        summary = provider_call(request)
        recorder.set_result(_chat_from_stream(request, summary, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return summary


async def _chat_completions_stream_async(
    client,
    request: ChatStreamRequest,
    provider_call: Callable[[ChatStreamRequest], Awaitable[ChatCompletionsStreamSummary]],
    options: OpenAIOptions | None = None,
) -> ChatCompletionsStreamSummary:
    opts = options or OpenAIOptions()
    start = _chat_start_payload(request, opts, GenerationMode.STREAM)
    recorder = client.start_streaming_generation(start)

    try:
        summary = await provider_call(request)
        recorder.set_result(_chat_from_stream(request, summary, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return summary


def _chat_from_request_response(
    request: ChatCreateRequest,
    response: ChatCreateResponse,
    options: OpenAIOptions | None = None,
) -> Generation:
    opts = options or OpenAIOptions()

    model_name = _as_str(_read(request, "model"))
    response_model = _as_str(_read(response, "model")) or model_name

    input_messages, system_prompt = _map_chat_request_messages(request)
    output_messages = _map_chat_response_output(response)
    tools = _map_chat_tools(request)
    usage = from_openai_chat(_read(response, "usage"))

    max_tokens = _chat_request_max_tokens(request)
    temperature = _as_float_or_none(_read(request, "temperature"))
    top_p = _as_float_or_none(_read(request, "top_p"))
    tool_choice = _canonical_tool_choice(_read(request, "tool_choice"))
    reasoning = _read(request, "reasoning")
    thinking_enabled = True if reasoning is not None else None
    thinking_budget = _openai_thinking_budget(reasoning)

    generation = Generation(
        conversation_id=opts.conversation_id,
        agent_name=opts.agent_name,
        agent_version=opts.agent_version,
        mode=GenerationMode.SYNC,
        model=ModelRef(provider=opts.provider_name, name=model_name),
        response_id=_as_str(_read(response, "id")),
        response_model=response_model,
        system_prompt=system_prompt,
        max_tokens=max_tokens,
        temperature=temperature,
        top_p=top_p,
        tool_choice=tool_choice,
        thinking_enabled=thinking_enabled,
        input=input_messages,
        output=output_messages,
        tools=tools,
        usage=usage,
        stop_reason=_normalize_chat_stop_reason(_first_chat_finish_reason(response)),
        tags=dict(opts.tags),
        metadata=_metadata_with_thinking_budget(opts.metadata, thinking_budget),
    )

    if opts.raw_artifacts:
        generation.artifacts = [
            _json_artifact(ArtifactKind.REQUEST, "openai.chat.request", request),
            _json_artifact(ArtifactKind.RESPONSE, "openai.chat.response", response),
        ]
        if tools:
            generation.artifacts.append(_json_artifact(ArtifactKind.TOOLS, "openai.chat.tools", tools))

    return generation


def _chat_from_stream(
    request: ChatStreamRequest,
    summary: ChatCompletionsStreamSummary,
    options: OpenAIOptions | None = None,
) -> Generation:
    opts = options or OpenAIOptions()

    if summary.final_response is not None:
        generation = _chat_from_request_response(request, summary.final_response, opts)
        generation.mode = GenerationMode.STREAM
    else:
        model_name = _as_str(_read(request, "model"))
        input_messages, system_prompt = _map_chat_request_messages(request)
        tools = _map_chat_tools(request)

        generation = Generation(
            conversation_id=opts.conversation_id,
            agent_name=opts.agent_name,
            agent_version=opts.agent_version,
            mode=GenerationMode.STREAM,
            model=ModelRef(provider=opts.provider_name, name=model_name),
            response_model=model_name,
            system_prompt=system_prompt,
            max_tokens=_chat_request_max_tokens(request),
            temperature=_as_float_or_none(_read(request, "temperature")),
            top_p=_as_float_or_none(_read(request, "top_p")),
            tool_choice=_canonical_tool_choice(_read(request, "tool_choice")),
            thinking_enabled=True if _read(request, "reasoning") is not None else None,
            input=input_messages,
            output=[],
            tools=tools,
            tags=dict(opts.tags),
            metadata=_metadata_with_thinking_budget(
                opts.metadata,
                _openai_thinking_budget(_read(request, "reasoning")),
            ),
        )

    output_text = summary.output_text.strip() or _extract_chat_stream_text(summary.events)
    if output_text:
        generation.output = [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=output_text)])]

    if opts.raw_artifacts:
        if not any(artifact.kind == ArtifactKind.REQUEST for artifact in generation.artifacts):
            generation.artifacts.append(_json_artifact(ArtifactKind.REQUEST, "openai.chat.request", request))
        if generation.tools and not any(artifact.kind == ArtifactKind.TOOLS for artifact in generation.artifacts):
            generation.artifacts.append(_json_artifact(ArtifactKind.TOOLS, "openai.chat.tools", generation.tools))
        generation.artifacts.append(
            _json_artifact(ArtifactKind.PROVIDER_EVENT, "openai.chat.stream_events", summary.events)
        )

    return generation


def _responses_create(
    client,
    request: ResponsesCreateRequest,
    provider_call: Callable[[ResponsesCreateRequest], ResponsesCreateResponse],
    options: OpenAIOptions | None = None,
) -> ResponsesCreateResponse:
    opts = options or OpenAIOptions()
    start = _responses_start_payload(request, opts, GenerationMode.SYNC)
    recorder = client.start_generation(start)

    try:
        response = provider_call(request)
        recorder.set_result(_responses_from_request_response(request, response, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


async def _responses_create_async(
    client,
    request: ResponsesCreateRequest,
    provider_call: Callable[[ResponsesCreateRequest], Awaitable[ResponsesCreateResponse]],
    options: OpenAIOptions | None = None,
) -> ResponsesCreateResponse:
    opts = options or OpenAIOptions()
    start = _responses_start_payload(request, opts, GenerationMode.SYNC)
    recorder = client.start_generation(start)

    try:
        response = await provider_call(request)
        recorder.set_result(_responses_from_request_response(request, response, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


def _responses_stream(
    client,
    request: ResponsesStreamRequest,
    provider_call: Callable[[ResponsesStreamRequest], ResponsesStreamSummary],
    options: OpenAIOptions | None = None,
) -> ResponsesStreamSummary:
    opts = options or OpenAIOptions()
    start = _responses_start_payload(request, opts, GenerationMode.STREAM)
    recorder = client.start_streaming_generation(start)

    try:
        summary = provider_call(request)
        recorder.set_result(_responses_from_stream(request, summary, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return summary


async def _responses_stream_async(
    client,
    request: ResponsesStreamRequest,
    provider_call: Callable[[ResponsesStreamRequest], Awaitable[ResponsesStreamSummary]],
    options: OpenAIOptions | None = None,
) -> ResponsesStreamSummary:
    opts = options or OpenAIOptions()
    start = _responses_start_payload(request, opts, GenerationMode.STREAM)
    recorder = client.start_streaming_generation(start)

    try:
        summary = await provider_call(request)
        recorder.set_result(_responses_from_stream(request, summary, opts))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return summary


def _embeddings_create(
    client,
    request: Any,
    provider_call: Callable[[Any], Any],
    options: OpenAIOptions | None = None,
) -> Any:
    opts = options or OpenAIOptions()
    recorder = client.start_embedding(_embeddings_start_payload(request, opts))

    try:
        response = provider_call(request)
        recorder.set_result(_embeddings_from_request_response(request, response))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


async def _embeddings_create_async(
    client,
    request: Any,
    provider_call: Callable[[Any], Awaitable[Any]],
    options: OpenAIOptions | None = None,
) -> Any:
    opts = options or OpenAIOptions()
    recorder = client.start_embedding(_embeddings_start_payload(request, opts))

    try:
        response = await provider_call(request)
        recorder.set_result(_embeddings_from_request_response(request, response))
    except Exception as exc:  # noqa: BLE001
        recorder.set_call_error(exc)
        raise
    finally:
        recorder.end()

    if recorder.err() is not None:
        raise recorder.err()

    return response


def _embeddings_from_request_response(request: Any, response: Any) -> EmbeddingResult:
    result = EmbeddingResult(
        input_count=_embedding_input_count(_read(request, "input")),
        input_texts=_embedding_input_texts(_read(request, "input")),
    )

    usage = _read(response, "usage")
    input_tokens = _as_int_or_none(_read(usage, "prompt_tokens"))
    if input_tokens is None:
        input_tokens = _as_int_or_none(_read(usage, "total_tokens"))
    if input_tokens is not None:
        result.input_tokens = input_tokens

    result.response_model = _as_str(_read(response, "model"))

    data = _as_list(_read(response, "data"))
    if data:
        embedding = _read(data[0], "embedding")
        if isinstance(embedding, list):
            result.dimensions = len(embedding)

    return result


def _responses_from_request_response(
    request: ResponsesCreateRequest,
    response: ResponsesCreateResponse,
    options: OpenAIOptions | None = None,
) -> Generation:
    opts = options or OpenAIOptions()

    request_payload = _map_responses_request(request)
    controls = _map_responses_controls(request)

    model_name = _as_str(_read(request, "model"))
    response_model = _as_str(_read(response, "model")) or model_name

    generation = Generation(
        conversation_id=opts.conversation_id,
        agent_name=opts.agent_name,
        agent_version=opts.agent_version,
        mode=GenerationMode.SYNC,
        model=ModelRef(provider=opts.provider_name, name=model_name),
        response_id=_as_str(_read(response, "id")),
        response_model=response_model,
        system_prompt=request_payload["system_prompt"],
        max_tokens=controls["max_tokens"],
        temperature=controls["temperature"],
        top_p=controls["top_p"],
        tool_choice=controls["tool_choice"],
        thinking_enabled=controls["thinking_enabled"],
        input=request_payload["input"],
        output=_map_responses_output_items(_read(response, "output")),
        tools=request_payload["tools"],
        usage=from_openai_responses(_read(response, "usage")),
        stop_reason=_normalize_responses_stop_reason(response),
        tags=dict(opts.tags),
        metadata=_metadata_with_thinking_budget(opts.metadata, controls["thinking_budget"]),
    )

    if opts.raw_artifacts:
        generation.artifacts = [
            _json_artifact(ArtifactKind.REQUEST, "openai.responses.request", request),
            _json_artifact(ArtifactKind.RESPONSE, "openai.responses.response", response),
        ]
        if request_payload["tools"]:
            generation.artifacts.append(
                _json_artifact(ArtifactKind.TOOLS, "openai.responses.tools", request_payload["tools"])
            )

    return generation


def _responses_from_stream(
    request: ResponsesStreamRequest,
    summary: ResponsesStreamSummary,
    options: OpenAIOptions | None = None,
) -> Generation:
    opts = options or OpenAIOptions()

    events = list(summary.events)
    final_from_events = _find_responses_final_from_events(events)
    final_response = summary.final_response or final_from_events

    if final_response is not None:
        generation = _responses_from_request_response(request, final_response, opts)
        generation.mode = GenerationMode.STREAM
    else:
        request_payload = _map_responses_request(request)
        controls = _map_responses_controls(request)
        model_name = _as_str(_read(request, "model"))

        generation = Generation(
            conversation_id=opts.conversation_id,
            agent_name=opts.agent_name,
            agent_version=opts.agent_version,
            mode=GenerationMode.STREAM,
            model=ModelRef(provider=opts.provider_name, name=model_name),
            response_model=model_name,
            system_prompt=request_payload["system_prompt"],
            max_tokens=controls["max_tokens"],
            temperature=controls["temperature"],
            top_p=controls["top_p"],
            tool_choice=controls["tool_choice"],
            thinking_enabled=controls["thinking_enabled"],
            input=request_payload["input"],
            tools=request_payload["tools"],
            stop_reason=_normalize_responses_stop_reason_from_events(events),
            tags=dict(opts.tags),
            metadata=_metadata_with_thinking_budget(opts.metadata, controls["thinking_budget"]),
        )

    output_text = summary.output_text.strip() or _extract_responses_stream_text(events)
    if output_text:
        generation.output = [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=output_text)])]

    if opts.raw_artifacts:
        if not any(artifact.kind == ArtifactKind.REQUEST for artifact in generation.artifacts):
            generation.artifacts.append(_json_artifact(ArtifactKind.REQUEST, "openai.responses.request", request))
        if generation.tools and not any(artifact.kind == ArtifactKind.TOOLS for artifact in generation.artifacts):
            generation.artifacts.append(_json_artifact(ArtifactKind.TOOLS, "openai.responses.tools", generation.tools))
        generation.artifacts.append(
            _json_artifact(ArtifactKind.PROVIDER_EVENT, "openai.responses.stream_events", events)
        )

    return generation


def _chat_start_payload(
    request: ChatCreateRequest | ChatStreamRequest, options: OpenAIOptions, mode: GenerationMode
) -> GenerationStart:
    input_messages, system_prompt = _map_chat_request_messages(request)
    tools = _map_chat_tools(request)
    reasoning = _read(request, "reasoning")

    return GenerationStart(
        conversation_id=options.conversation_id,
        agent_name=options.agent_name,
        agent_version=options.agent_version,
        mode=mode,
        model=ModelRef(provider=options.provider_name, name=_as_str(_read(request, "model"))),
        system_prompt=system_prompt,
        max_tokens=_chat_request_max_tokens(request),
        temperature=_as_float_or_none(_read(request, "temperature")),
        top_p=_as_float_or_none(_read(request, "top_p")),
        tool_choice=_canonical_tool_choice(_read(request, "tool_choice")),
        thinking_enabled=True if reasoning is not None else None,
        tools=tools,
        tags=dict(options.tags),
        metadata=_metadata_with_thinking_budget(options.metadata, _openai_thinking_budget(reasoning)),
    )


def _responses_start_payload(
    request: ResponsesCreateRequest | ResponsesStreamRequest,
    options: OpenAIOptions,
    mode: GenerationMode,
) -> GenerationStart:
    payload = _map_responses_request(request)
    controls = _map_responses_controls(request)

    return GenerationStart(
        conversation_id=options.conversation_id,
        agent_name=options.agent_name,
        agent_version=options.agent_version,
        mode=mode,
        model=ModelRef(provider=options.provider_name, name=_as_str(_read(request, "model"))),
        system_prompt=payload["system_prompt"],
        max_tokens=controls["max_tokens"],
        temperature=controls["temperature"],
        top_p=controls["top_p"],
        tool_choice=controls["tool_choice"],
        thinking_enabled=controls["thinking_enabled"],
        tools=payload["tools"],
        tags=dict(options.tags),
        metadata=_metadata_with_thinking_budget(options.metadata, controls["thinking_budget"]),
    )


def _embeddings_start_payload(request: Any, options: OpenAIOptions) -> EmbeddingStart:
    return EmbeddingStart(
        agent_name=options.agent_name,
        agent_version=options.agent_version,
        model=ModelRef(provider=options.provider_name, name=_as_str(_read(request, "model"))),
        dimensions=_as_int_or_none(_read(request, "dimensions")),
        encoding_format=_as_str(_read(request, "encoding_format")),
        tags=dict(options.tags),
        metadata=dict(options.metadata),
    )


def _map_chat_request_messages(request: ChatCreateRequest | ChatStreamRequest) -> tuple[list[Message], str]:
    messages = _as_list(_read(request, "messages"))

    out: list[Message] = []
    system_chunks: list[str] = []

    for message in messages:
        role = _as_str(_read(message, "role")).lower()
        content = _extract_text(_read(message, "content"))

        if role in {"system", "developer"}:
            if content:
                system_chunks.append(content)
            continue

        mapped_role = MessageRole.USER
        if role == "assistant":
            mapped_role = MessageRole.ASSISTANT
        elif role == "tool":
            mapped_role = MessageRole.TOOL

        parts: list[Part] = []
        if mapped_role != MessageRole.TOOL and content:
            parts.append(Part(kind=PartKind.TEXT, text=content))

        if mapped_role == MessageRole.TOOL:
            tool_message = _tool_result_message(
                _read(message, "content"),
                tool_call_id=_as_str(_read(message, "tool_call_id"))
                or _as_str(_read(message, "toolCallId"))
                or _as_str(_read(message, "id")),
                name=_as_str(_read(message, "name")),
                is_error=_read(message, "is_error"),
            )
            if tool_message is not None:
                out.append(tool_message)
            continue

        if mapped_role == MessageRole.ASSISTANT:
            for part in _map_chat_tool_call_parts(_read(message, "tool_calls")):
                parts.append(part)

        if not parts:
            continue

        out.append(
            Message(
                role=mapped_role,
                name=_as_str(_read(message, "name")),
                parts=parts,
            )
        )

    return out, "\n\n".join(system_chunks)


def _map_chat_response_output(response: ChatCreateResponse) -> list[Message]:
    choices = _as_list(_read(response, "choices"))
    if not choices:
        return []

    first_choice = choices[0]
    response_message = _read(first_choice, "message")

    content = _extract_text(_read(response_message, "content"))
    refusal = _as_str(_read(response_message, "refusal"))

    chunks: list[str] = []
    if content:
        chunks.append(content)
    if refusal:
        chunks.append(refusal)

    parts: list[Part] = []
    if chunks:
        parts.append(Part(kind=PartKind.TEXT, text="\n".join(chunks)))

    for part in _map_chat_tool_call_parts(_read(response_message, "tool_calls")):
        parts.append(part)

    if not parts:
        return []

    return [Message(role=MessageRole.ASSISTANT, parts=parts)]


def _map_chat_tool_call_parts(tool_calls_value: Any) -> list[Part]:
    out: list[Part] = []
    for tool_call in _as_list(tool_calls_value):
        function_payload = _read(tool_call, "function")
        name = _as_str(_read(function_payload, "name"))
        if not name:
            continue

        arguments = _read(function_payload, "arguments")
        input_json = _json_bytes(arguments)

        part = Part(
            kind=PartKind.TOOL_CALL,
            tool_call=ToolCall(
                id=_as_str(_read(tool_call, "id")),
                name=name,
                input_json=input_json,
            ),
        )
        part.metadata.provider_type = "tool_call"
        out.append(part)

    return out


def _map_chat_tools(request: ChatCreateRequest | ChatStreamRequest) -> list[ToolDefinition]:
    out: list[ToolDefinition] = []
    for tool in _as_list(_read(request, "tools")):
        tool_type = _as_str(_read(tool, "type"))

        if tool_type == "function":
            function_payload = _read(tool, "function")
            name = _as_str(_read(function_payload, "name"))
            if not name:
                continue
            out.append(
                ToolDefinition(
                    name=name,
                    description=_as_str(_read(function_payload, "description")),
                    type="function",
                    input_schema_json=_json_bytes(_read(function_payload, "parameters")),
                )
            )
            continue

        name = _as_str(_read(tool, "name"))
        if tool_type and name:
            out.append(ToolDefinition(name=name, type=tool_type))

    return out


def _chat_request_max_tokens(request: ChatCreateRequest | ChatStreamRequest) -> int | None:
    first = _as_int_or_none(_read(request, "max_completion_tokens"))
    if first is not None:
        return first
    return _as_int_or_none(_read(request, "max_tokens"))


def _first_chat_finish_reason(response: ChatCreateResponse) -> str:
    for choice in _as_list(_read(response, "choices")):
        finish_reason = _as_str(_read(choice, "finish_reason"))
        if finish_reason:
            return finish_reason
    return ""


def _normalize_chat_stop_reason(value: str) -> str:
    return value


def _extract_chat_stream_text(events: list[ChatStreamEvent]) -> str:
    chunks: list[str] = []
    for event in events:
        for choice in _as_list(_read(event, "choices")):
            delta = _read(choice, "delta")
            piece = _read(delta, "content")
            if isinstance(piece, str):
                chunks.append(piece)
            elif piece is not None:
                chunks.append(str(piece))
    return "".join(chunks)


def _map_responses_request(request: ResponsesCreateRequest | ResponsesStreamRequest) -> dict[str, Any]:
    input_messages: list[Message] = []
    system_chunks: list[str] = []

    instructions = _extract_text(_read(request, "instructions"))
    if instructions:
        system_chunks.append(instructions)

    input_payload = _read(request, "input")
    if isinstance(input_payload, str):
        input_messages.append(Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text=input_payload)]))
    else:
        for item in _as_list(input_payload):
            role = _as_str(_read(item, "role")).lower()
            item_type = _as_str(_read(item, "type")).lower()

            if role in {"system", "developer"} and item_type == "message":
                text = _extract_text(_read(item, "content"))
                if text:
                    system_chunks.append(text)
                continue

            if item_type == "function_call_output":
                tool_message = _tool_result_message(
                    _read(item, "output"),
                    tool_call_id=_as_str(_read(item, "call_id")) or _as_str(_read(item, "callId")),
                    name=_as_str(_read(item, "name")),
                    is_error=_read(item, "is_error"),
                )
                if tool_message is not None:
                    input_messages.append(tool_message)
                continue

            if item_type == "message" or role:
                text = _extract_text(_read(item, "content"))
                if not text:
                    continue
                mapped_role = MessageRole.USER
                if role == "assistant":
                    mapped_role = MessageRole.ASSISTANT
                elif role == "tool":
                    mapped_role = MessageRole.TOOL

                input_messages.append(Message(role=mapped_role, parts=[Part(kind=PartKind.TEXT, text=text)]))

    return {
        "input": input_messages,
        "system_prompt": "\n\n".join(system_chunks),
        "tools": _map_responses_tools(_read(request, "tools")),
    }


def _map_responses_controls(request: ResponsesCreateRequest | ResponsesStreamRequest) -> dict[str, Any]:
    reasoning = _read(request, "reasoning")
    return {
        "max_tokens": _as_int_or_none(_read(request, "max_output_tokens")),
        "temperature": _as_float_or_none(_read(request, "temperature")),
        "top_p": _as_float_or_none(_read(request, "top_p")),
        "tool_choice": _canonical_tool_choice(_read(request, "tool_choice")),
        "thinking_enabled": True if reasoning is not None else None,
        "thinking_budget": _openai_thinking_budget(reasoning),
    }


def _map_responses_tools(value: Any) -> list[ToolDefinition]:
    out: list[ToolDefinition] = []

    for tool in _as_list(value):
        tool_type = _as_str(_read(tool, "type"))
        if tool_type == "function":
            name = _as_str(_read(tool, "name"))
            if not name:
                continue
            out.append(
                ToolDefinition(
                    name=name,
                    description=_as_str(_read(tool, "description")),
                    type="function",
                    input_schema_json=_json_bytes(_read(tool, "parameters")),
                )
            )
            continue

        name = _as_str(_read(tool, "name"))
        if tool_type and name:
            out.append(ToolDefinition(name=name, type=tool_type))

    return out


def _map_responses_output_items(value: Any) -> list[Message]:
    out: list[Message] = []

    for item in _as_list(value):
        item_type = _as_str(_read(item, "type"))

        if item_type == "message":
            text = _extract_text(_read(item, "content"))
            if text:
                out.append(Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=text)]))
            continue

        if item_type == "function_call":
            name = _as_str(_read(item, "name"))
            if not name:
                continue
            arguments = _read(item, "arguments")
            part = Part(
                kind=PartKind.TOOL_CALL,
                tool_call=ToolCall(
                    id=_as_str(_read(item, "call_id")),
                    name=name,
                    input_json=_json_bytes(arguments),
                ),
            )
            part.metadata.provider_type = "tool_call"
            out.append(Message(role=MessageRole.ASSISTANT, parts=[part]))
            continue

        if item_type == "function_call_output":
            tool_message = _tool_result_message(
                _read(item, "output"),
                tool_call_id=_as_str(_read(item, "call_id")) or _as_str(_read(item, "callId")),
                name=_as_str(_read(item, "name")),
                is_error=_read(item, "is_error"),
            )
            if tool_message is not None:
                out.append(tool_message)
            continue

        fallback = _extract_text(item)
        if fallback:
            out.append(Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=fallback)]))

    return out


def _tool_result_message(value: Any, *, tool_call_id: str, name: str, is_error: Any) -> Message | None:
    content = _extract_text(value)
    content_json = _json_bytes(value)
    rendered_content = content or content_json.decode("utf-8")
    if not rendered_content:
        return None

    part = Part(
        kind=PartKind.TOOL_RESULT,
        tool_result=ToolResult(
            tool_call_id=tool_call_id,
            name=name,
            content=rendered_content,
            content_json=content_json,
            is_error=is_error if isinstance(is_error, bool) else None,
        ),
    )
    part.metadata.provider_type = "tool_result"
    return Message(role=MessageRole.TOOL, parts=[part])


def _normalize_responses_stop_reason(response: ResponsesCreateResponse) -> str:
    status = _as_str(_read(response, "status")).lower()
    reason = _as_str(_read(_read(response, "incomplete_details"), "reason")).lower()

    if status == "incomplete" and reason:
        return reason
    if status == "completed":
        return "stop"
    return status


def _normalize_responses_stop_reason_from_events(events: list[ResponsesStreamEvent]) -> str:
    for event in reversed(events):
        event_type = _as_str(_read(event, "type"))
        if event_type == "response.incomplete":
            reason = _as_str(_read(_read(_read(event, "response"), "incomplete_details"), "reason")).lower()
            if reason:
                return reason
        if event_type == "response.completed":
            return "stop"
        if event_type == "response.failed":
            return "failed"
        if event_type == "response.cancelled":
            return "cancelled"
    return ""


def _extract_responses_stream_text(events: list[ResponsesStreamEvent]) -> str:
    chunks: list[str] = []

    for event in events:
        event_type = _as_str(_read(event, "type"))
        if event_type == "response.output_text.delta":
            delta = _read(event, "delta")
            if isinstance(delta, str):
                chunks.append(delta)
            elif delta is not None:
                chunks.append(str(delta))
            continue

        if event_type == "response.output_text.done" and not chunks:
            text = _read(event, "text")
            if isinstance(text, str):
                chunks.append(text)
            elif text is not None:
                chunks.append(str(text))
            continue

        if event_type == "response.refusal.delta":
            delta = _read(event, "delta")
            if isinstance(delta, str):
                chunks.append(delta)
            elif delta is not None:
                chunks.append(str(delta))

    return "".join(chunks)


def _find_responses_final_from_events(events: list[ResponsesStreamEvent]) -> ResponsesCreateResponse | None:
    for event in reversed(events):
        event_type = _as_str(_read(event, "type"))
        if event_type in {"response.completed", "response.incomplete"}:
            response = _read(event, "response")
            if response is not None:
                return response
    return None


def _embedding_input_count(value: Any) -> int:
    if value is None:
        return 0
    if isinstance(value, str):
        return 1
    if isinstance(value, Mapping):
        return 1
    if isinstance(value, list | tuple):
        if not value:
            return 0
        if all(isinstance(item, int) and not isinstance(item, bool) for item in value):
            return 1
        return len(value)
    return 0


def _embedding_input_texts(value: Any) -> list[str]:
    if value is None:
        return []
    if isinstance(value, str):
        text = value.strip()
        return [text] if text else []
    if isinstance(value, Mapping):
        text = _as_str(value.get("text"))
        return [text] if text else []
    if isinstance(value, list | tuple):
        out: list[str] = []
        for item in value:
            if isinstance(item, str):
                text = item.strip()
                if text:
                    out.append(text)
                continue
            if isinstance(item, Mapping):
                text = _as_str(item.get("text"))
                if text:
                    out.append(text)
        return out
    return []


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


def _openai_thinking_budget(reasoning: Any) -> int | None:
    if reasoning is None:
        return None

    for key in ("budget_tokens", "thinking_budget", "thinkingBudget", "max_output_tokens"):
        candidate = _as_int_or_none(_read(reasoning, key))
        if candidate is not None:
            return candidate

    return None


def _metadata_with_thinking_budget(metadata: Mapping[str, Any], thinking_budget: int | None) -> dict[str, Any]:
    out = dict(metadata)
    if thinking_budget is not None:
        out[_thinking_budget_metadata_key] = thinking_budget
    return out


def _json_artifact(kind: ArtifactKind, name: str, payload: Any) -> Artifact:
    return Artifact(
        kind=kind,
        name=name,
        content_type="application/json",
        payload=_json_bytes(payload),
    )


def _json_text(value: Any) -> str:
    return _json_bytes(value).decode("utf-8")


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
        content = _as_str(value.get("content"))
        if content:
            return content
        refusal = _as_str(value.get("refusal"))
        if refusal:
            return refusal
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
    return []


def _as_str(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value.strip()
    return str(value).strip()


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


class _ChatCompletionsNamespace:
    """Namespace for OpenAI chat-completions wrappers and mappers."""

    create = staticmethod(_chat_completions_create)
    create_async = staticmethod(_chat_completions_create_async)
    stream = staticmethod(_chat_completions_stream)
    stream_async = staticmethod(_chat_completions_stream_async)
    from_request_response = staticmethod(_chat_from_request_response)
    from_stream = staticmethod(_chat_from_stream)


class _ChatNamespace:
    """Namespace for chat APIs."""

    completions = _ChatCompletionsNamespace()


class _ResponsesNamespace:
    """Namespace for OpenAI responses wrappers and mappers."""

    create = staticmethod(_responses_create)
    create_async = staticmethod(_responses_create_async)
    stream = staticmethod(_responses_stream)
    stream_async = staticmethod(_responses_stream_async)
    from_request_response = staticmethod(_responses_from_request_response)
    from_stream = staticmethod(_responses_from_stream)


class _EmbeddingsNamespace:
    """Namespace for OpenAI embeddings wrappers and mappers."""

    create = staticmethod(_embeddings_create)
    create_async = staticmethod(_embeddings_create_async)
    from_request_response = staticmethod(_embeddings_from_request_response)


chat = _ChatNamespace()
responses = _ResponsesNamespace()
embeddings = _EmbeddingsNamespace()
