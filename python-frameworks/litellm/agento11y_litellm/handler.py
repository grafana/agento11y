"""LiteLLM callback handler that exports generations to Sigil."""

from __future__ import annotations

import json
import logging
from datetime import datetime, timezone
from typing import Any

from agento11y import Client
from agento11y.models import (
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
    TokenUsage,
    ToolCall,
    ToolDefinition,
    ToolResult,
)
from agento11y.usage import from_openai_chat
from litellm.integrations.custom_logger import CustomLogger

logger = logging.getLogger(__name__)

_CHAT_CALL_TYPES = frozenset(
    {
        "completion",
        "acompletion",
        "text_completion",
        "atext_completion",
    }
)

_EMBEDDING_CALL_TYPES = frozenset({"embedding", "aembedding"})


def _make_tool_call_part(*, call_id: str, name: str, arguments: str) -> Part:
    """Build a Sigil TOOL_CALL Part from normalized arguments."""
    return Part(
        kind=PartKind.TOOL_CALL,
        tool_call=ToolCall(
            id=call_id,
            name=name,
            input_json=arguments.encode("utf-8"),
        ),
    )


def _map_messages(messages: list[dict[str, Any]] | None) -> tuple[list[Message], str]:
    """Map OpenAI-format messages to Sigil Messages, extracting system prompt."""
    if not messages:
        return [], ""

    out: list[Message] = []
    system_chunks: list[str] = []

    for msg in messages:
        role = (msg.get("role") or "").lower()
        content = _extract_text_content(msg.get("content"))

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

        if mapped_role == MessageRole.TOOL:
            out.append(
                _tool_result_message(
                    content=content,
                    tool_call_id=msg.get("tool_call_id", ""),
                    name=msg.get("name", ""),
                )
            )
            continue

        if mapped_role == MessageRole.ASSISTANT:
            parts.extend(_map_thinking_parts(msg))

        if content:
            parts.append(Part(kind=PartKind.TEXT, text=content))

        if mapped_role == MessageRole.ASSISTANT:
            parts.extend(_map_tool_call_parts(msg.get("tool_calls")))

        if not parts:
            continue

        out.append(Message(role=mapped_role, parts=parts))

    return out, "\n\n".join(system_chunks)


def _map_thinking_parts(message: dict[str, Any]) -> list[Part]:
    """Map reasoning/thinking from an OpenAI-format message to THINKING parts.

    Prefers structured ``thinking_blocks`` (Anthropic-style, may include
    redacted blocks) and falls back to the flat ``reasoning_content`` string.
    Reading both would double-emit the same text, since ``reasoning_content``
    is usually the concatenation of the blocks.
    """
    if not isinstance(message, dict):
        return []

    blocks = message.get("thinking_blocks")
    if isinstance(blocks, list) and blocks:
        out: list[Part] = []
        for block in blocks:
            if not isinstance(block, dict):
                continue
            if (block.get("type") or "").lower() == "redacted_thinking":
                text = block.get("data") or block.get("text") or ""
            else:
                text = block.get("thinking") or block.get("text") or ""
            if text:
                out.append(Part(kind=PartKind.THINKING, thinking=text))
        if out:
            return out

    reasoning = message.get("reasoning_content")
    if isinstance(reasoning, str) and reasoning:
        return [Part(kind=PartKind.THINKING, thinking=reasoning)]

    return []


def _map_tool_call_parts(tool_calls: list[dict[str, Any]] | None) -> list[Part]:
    """Map OpenAI-format tool_calls to Sigil ToolCall parts."""
    if not tool_calls:
        return []

    out: list[Part] = []
    for tc in tool_calls:
        function = tc.get("function") if isinstance(tc, dict) else getattr(tc, "function", None)
        if function is None:
            continue

        name = function.get("name", "") if isinstance(function, dict) else getattr(function, "name", "")
        if not name:
            continue

        arguments = function.get("arguments", "") if isinstance(function, dict) else getattr(function, "arguments", "")
        call_id = tc.get("id", "") if isinstance(tc, dict) else getattr(tc, "id", "")

        out.append(_make_tool_call_part(call_id=call_id or "", name=name, arguments=arguments or ""))
    return out


def _tool_result_message(*, content: str, tool_call_id: str, name: str) -> Message:
    """Create a Sigil tool result Message."""
    return Message(
        role=MessageRole.TOOL,
        parts=[
            Part(
                kind=PartKind.TOOL_RESULT,
                tool_result=ToolResult(
                    tool_call_id=tool_call_id,
                    name=name,
                    content=content,
                ),
            )
        ],
    )


def _map_response_output(response: Any) -> list[Message]:
    """Map SLO response to Sigil output Messages.

    Reads from the StandardLoggingPayload ``response`` field (dict or str)
    so that LiteLLM redaction settings are honoured.
    """
    if response is None:
        return []

    if isinstance(response, str):
        if not response:
            return []
        return [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=response)])]

    if not isinstance(response, dict):
        return []

    choices = response.get("choices")
    if not choices:
        return []

    out: list[Message] = []
    for choice in choices:
        if not isinstance(choice, dict):
            continue

        response_message = choice.get("message")
        if not isinstance(response_message, dict):
            continue

        content = response_message.get("content") or ""
        parts: list[Part] = []

        parts.extend(_map_thinking_parts(response_message))

        if content:
            parts.append(Part(kind=PartKind.TEXT, text=content))

        parts.extend(_map_tool_call_parts(response_message.get("tool_calls")))

        if not parts:
            continue

        out.append(Message(role=MessageRole.ASSISTANT, parts=parts))

    return out


def _extract_text_content(content: Any) -> str:
    """Extract text from OpenAI message content (string or content parts array)."""
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        texts = []
        for item in content:
            if isinstance(item, dict) and item.get("type") == "text":
                texts.append(item.get("text", ""))
            elif isinstance(item, str):
                texts.append(item)
        return " ".join(texts)
    return str(content)


def _epoch_to_utc(epoch: float | None) -> datetime | None:
    """Convert epoch seconds to UTC datetime."""
    if epoch is None or epoch == 0:
        return None
    return datetime.fromtimestamp(epoch, tz=timezone.utc)


def _datetime_to_utc(dt: datetime | None) -> datetime | None:
    """Ensure a datetime is UTC.

    Naive datetimes are assumed to be local time (matching datetime.now()
    which LiteLLM uses to create start_time/end_time).
    """
    if dt is None:
        return None
    return dt.astimezone(timezone.utc)


def _extract_stop_reason(response: Any) -> str:
    """Extract finish_reason from the SLO response dict."""
    if not isinstance(response, dict):
        return ""
    choices = response.get("choices")
    if not choices:
        return ""
    first_choice = choices[0]
    if not isinstance(first_choice, dict):
        return ""
    return first_choice.get("finish_reason") or ""


def _map_tool_definitions(kwargs: dict[str, Any]) -> list[ToolDefinition]:
    """Extract tool schemas from optional_params."""
    optional_params = kwargs.get("optional_params") or {}
    tools = optional_params.get("tools")
    if not tools or not isinstance(tools, list):
        return []

    out: list[ToolDefinition] = []
    for tool in tools:
        if not isinstance(tool, dict):
            continue
        tool_type = tool.get("type", "")
        function = tool.get("function") or {}
        name = function.get("name", "")
        if not name:
            continue
        description = function.get("description", "")
        parameters = function.get("parameters")
        schema_json = json.dumps(parameters).encode("utf-8") if parameters else b""
        out.append(
            ToolDefinition(
                name=name,
                description=description,
                type=tool_type,
                input_schema_json=schema_json,
            )
        )
    return out


def _safe_cast(params: dict[str, Any], key: str, cast: type) -> Any:
    """Safely cast a model parameter, returning None on missing or invalid values."""
    if key not in params:
        return None
    try:
        return cast(params[key])
    except (ValueError, TypeError):
        return None


def _normalize_embedding_inputs(inputs: Any) -> list[str]:
    """Extract embedding input text, dropping token-id inputs that aren't text.

    LiteLLM clears the input to an empty string when message logging is off, so
    an empty string is dropped to stay consistent with ``_embedding_input_count``.
    """
    if isinstance(inputs, str):
        return [inputs] if inputs else []
    if isinstance(inputs, list):
        return [item for item in inputs if isinstance(item, str)]
    return []


def _embedding_input_count(inputs: Any) -> int:
    """Count distinct embedding inputs.

    A single pre-tokenized input (``list[int]``) counts as one; a batch of
    token-id lists (``list[list[int]]``) counts each entry. LiteLLM clears the
    input to an empty string when message logging is off, so an empty string is
    treated as no input rather than a single one.
    """
    if inputs is None:
        return 0
    if isinstance(inputs, str):
        return 1 if inputs else 0
    if isinstance(inputs, list):
        if inputs and all(isinstance(item, int) for item in inputs):
            return 1
        return len(inputs)
    return 0


def _embedding_dimensions_from_response(response_obj: Any) -> int | None:
    """Read the embedding vector length from the first response item."""
    data = getattr(response_obj, "data", None)
    if not data:
        return None
    first = data[0]
    embedding = first.get("embedding") if isinstance(first, dict) else getattr(first, "embedding", None)
    if not isinstance(embedding, (list, tuple)):
        return None
    return len(embedding)


def _response_model(response_obj: Any) -> str:
    """Extract the response model name, tolerating a missing attribute."""
    return getattr(response_obj, "model", "") or ""


def _extract_detailed_usage(response_obj: Any, slo: dict[str, Any]) -> TokenUsage:
    """Build TokenUsage with detailed breakdowns from response_obj, basic counts from SLO."""
    usage = TokenUsage(
        input_tokens=slo.get("prompt_tokens") or 0,
        output_tokens=slo.get("completion_tokens") or 0,
        total_tokens=slo.get("total_tokens") or 0,
        input_is_disjoint=True,
    )

    if response_obj is None:
        return usage

    resp_usage = getattr(response_obj, "usage", None)
    if resp_usage is None:
        return usage

    detail = from_openai_chat(resp_usage)
    usage.cache_read_input_tokens = detail.cache_read_input_tokens
    usage.cache_write_input_tokens = detail.cache_write_input_tokens
    usage.reasoning_tokens = detail.reasoning_tokens
    # SLO prompt_tokens are OpenAI-style cache-inclusive counts (cached reads are part
    # of prompt_tokens). Subtract cache reads to recover fresh, non-cached input so the
    # disjoint cache_read bucket does not double-count. total_tokens is invariant.
    usage.input_tokens = max(usage.input_tokens - detail.cache_read_input_tokens, 0)
    return usage


class Agento11yLiteLLMLogger(CustomLogger):
    """LiteLLM callback logger that exports generations to Sigil.

    Uses the Sigil SDK recorder pattern directly. The SDK handles
    batching and export internally, so this extends CustomLogger
    (not CustomBatchLogger) to avoid double-batching.
    """

    def __init__(
        self,
        *,
        client: Client,
        capture_inputs: bool = True,
        capture_outputs: bool = True,
        agent_name: str = "",
        agent_version: str = "",
        conversation_id: str = "",
        extra_tags: dict[str, str] | None = None,
        extra_metadata: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> None:
        super().__init__(**kwargs)
        self._client = client
        self._capture_inputs = capture_inputs
        self._capture_outputs = capture_outputs
        self._agent_name = agent_name
        self._agent_version = agent_version
        self._conversation_id = conversation_id
        self._extra_tags = dict(extra_tags) if extra_tags else {}
        self._extra_metadata = dict(extra_metadata) if extra_metadata else {}

    def log_success_event(self, kwargs: dict, response_obj: Any, start_time: datetime, end_time: datetime) -> None:
        self._log_event(kwargs, response_obj, start_time, end_time, is_failure=False)

    def log_failure_event(self, kwargs: dict, response_obj: Any, start_time: datetime, end_time: datetime) -> None:
        self._log_event(kwargs, response_obj, start_time, end_time, is_failure=True)

    async def async_log_success_event(
        self, kwargs: dict, response_obj: Any, start_time: datetime, end_time: datetime
    ) -> None:
        self._log_event(kwargs, response_obj, start_time, end_time, is_failure=False)

    async def async_log_failure_event(
        self, kwargs: dict, response_obj: Any, start_time: datetime, end_time: datetime
    ) -> None:
        self._log_event(kwargs, response_obj, start_time, end_time, is_failure=True)

    def _log_event(
        self,
        kwargs: dict,
        response_obj: Any,
        start_time: datetime,
        end_time: datetime,
        *,
        is_failure: bool,
    ) -> None:
        slo = kwargs.get("standard_logging_object")
        if slo is None:
            return

        call_type = slo.get("call_type") or ""
        try:
            if call_type in _EMBEDDING_CALL_TYPES:
                self._record_embedding(kwargs, response_obj, slo, start_time, end_time, is_failure=is_failure)
            else:
                self._record_generation(kwargs, response_obj, slo, start_time, end_time, is_failure=is_failure)
        except Exception:
            logger.exception("agento11y: failed to record LiteLLM event")

    def _resolve_agent_name(self, kwargs: dict[str, Any]) -> str:
        """Resolve agent_name from per-request metadata, falling back to static."""
        litellm_params = kwargs.get("litellm_params") or {}
        metadata = litellm_params.get("metadata") or {}
        value = metadata.get("agent_name")
        if value:
            return str(value)
        return self._agent_name

    def _resolve_agent_version(self, kwargs: dict[str, Any]) -> str:
        """Resolve agent_version from per-request metadata, falling back to static."""
        litellm_params = kwargs.get("litellm_params") or {}
        metadata = litellm_params.get("metadata") or {}
        value = metadata.get("agent_version")
        if value:
            return str(value)
        return self._agent_version

    def _resolve_conversation_id(self, kwargs: dict[str, Any]) -> str:
        """Resolve conversation_id from per-request metadata, falling back to static.

        Checks metadata keys first (conversation_id, session_id, thread_id),
        then LiteLLM's built-in session tracking fields (litellm_session_id,
        litellm_trace_id) in both metadata and litellm_params.
        """
        litellm_params = kwargs.get("litellm_params") or {}
        metadata = litellm_params.get("metadata") or {}
        for key in ("conversation_id", "session_id", "thread_id"):
            value = metadata.get(key)
            if value:
                return str(value)
        for key in ("litellm_session_id", "litellm_trace_id"):
            value = metadata.get(key) or litellm_params.get(key)
            if value:
                return str(value)
        return self._conversation_id

    def _record_generation(
        self,
        kwargs: dict[str, Any],
        response_obj: Any,
        slo: dict[str, Any],
        start_time: datetime,
        end_time: datetime,
        *,
        is_failure: bool,
    ) -> None:
        call_type = slo.get("call_type") or ""
        if call_type and call_type not in _CHAT_CALL_TYPES:
            return

        is_stream = bool(slo.get("stream"))

        tags: dict[str, str] = {
            "agento11y.framework.name": "litellm",
            "agento11y.framework.source": "handler",
            "agento11y.framework.language": "python",
        }
        request_tags = slo.get("request_tags") or []
        for tag_value in request_tags:
            tag_str = str(tag_value)
            tags[f"litellm.tag.{tag_str}"] = tag_str
        # extra_tags take precedence
        tags.update(self._extra_tags)

        metadata: dict[str, Any] = dict(self._extra_metadata)

        model_params = slo.get("model_parameters") or {}
        temperature = _safe_cast(model_params, "temperature", float)
        max_tokens = _safe_cast(model_params, "max_tokens", int)
        top_p = _safe_cast(model_params, "top_p", float)

        system_prompt = ""
        input_messages: list[Message] = []
        if self._capture_inputs:
            raw_messages = slo.get("messages")
            if isinstance(raw_messages, list):
                input_messages, system_prompt = _map_messages(raw_messages)

        provider = (slo.get("custom_llm_provider") or "").lower()
        model_name = slo.get("model") or ""
        gen_id = slo.get("id") or ""
        user_id = slo.get("end_user") or ""
        conversation_id = self._resolve_conversation_id(kwargs)
        started_at = _datetime_to_utc(start_time)
        tools = _map_tool_definitions(kwargs)

        seed = GenerationStart(
            id=gen_id,
            model=ModelRef(provider=provider, name=model_name),
            mode=GenerationMode.STREAM if is_stream else GenerationMode.SYNC,
            system_prompt=system_prompt,
            temperature=temperature,
            max_tokens=max_tokens,
            top_p=top_p,
            user_id=user_id,
            agent_name=self._resolve_agent_name(kwargs),
            agent_version=self._resolve_agent_version(kwargs),
            conversation_id=conversation_id,
            tags=tags,
            metadata=metadata,
            started_at=started_at,
            tools=tools,
        )

        if is_stream:
            recorder = self._client.start_streaming_generation(seed)
        else:
            recorder = self._client.start_generation(seed)

        try:
            if is_stream:
                completion_start = slo.get("completionStartTime")
                if completion_start:
                    first_token_at = _epoch_to_utc(float(completion_start))
                    if first_token_at is not None:
                        recorder.set_first_token_at(first_token_at)

            if is_failure:
                error_str = slo.get("error_str") or ""
                if error_str:
                    recorder.set_call_error(RuntimeError(error_str))

            slo_response = slo.get("response")

            output_messages: list[Message] = []
            if self._capture_outputs:
                output_messages = _map_response_output(slo_response)

            usage = _extract_detailed_usage(response_obj, slo)

            stop_reason = _extract_stop_reason(slo_response)

            recorder.set_result(
                generation=Generation(
                    input=input_messages,
                    output=output_messages,
                    usage=usage,
                    stop_reason=stop_reason,
                    completed_at=_datetime_to_utc(end_time),
                ),
            )
        finally:
            recorder.end()
            err = recorder.err()
            if err is not None:
                logger.warning("agento11y: recorder error: %s", err)

    def _record_embedding(
        self,
        kwargs: dict[str, Any],
        response_obj: Any,
        slo: dict[str, Any],
        start_time: datetime,
        end_time: datetime,
        *,
        is_failure: bool,
    ) -> None:
        model_name = slo.get("model") or ""
        if not model_name:
            return

        provider = (slo.get("custom_llm_provider") or "").lower()
        optional_params = kwargs.get("optional_params") or {}
        dimensions = _safe_cast(optional_params, "dimensions", int)
        encoding_format = optional_params.get("encoding_format") or ""

        tags: dict[str, str] = {
            "agento11y.framework.name": "litellm",
            "agento11y.framework.source": "handler",
            "agento11y.framework.language": "python",
        }
        for tag_value in slo.get("request_tags") or []:
            tag_str = str(tag_value)
            tags[f"litellm.tag.{tag_str}"] = tag_str
        tags.update(self._extra_tags)

        recorder = self._client.start_embedding(
            EmbeddingStart(
                model=ModelRef(provider=provider, name=model_name),
                agent_name=self._resolve_agent_name(kwargs),
                agent_version=self._resolve_agent_version(kwargs),
                dimensions=dimensions,
                encoding_format=encoding_format,
                tags=tags,
                metadata=dict(self._extra_metadata),
                started_at=_datetime_to_utc(start_time),
            )
        )

        try:
            if is_failure:
                error_str = slo.get("error_str") or ""
                if error_str:
                    recorder.set_call_error(RuntimeError(error_str))

            # Embedding input lives in kwargs["input"], not the SLO. LiteLLM clears
            # it (sets it to "") before invoking callbacks when message logging is
            # turned off, so reading it here honours LiteLLM redaction settings.
            inputs = kwargs.get("input")
            input_texts = _normalize_embedding_inputs(inputs) if self._capture_inputs else []

            recorder.set_result(
                EmbeddingResult(
                    input_count=_embedding_input_count(inputs),
                    input_tokens=slo.get("prompt_tokens") or 0,
                    input_texts=input_texts,
                    response_model=_response_model(response_obj) or model_name,
                    dimensions=dimensions or _embedding_dimensions_from_response(response_obj),
                )
            )
        finally:
            recorder.end()
            err = recorder.err()
            if err is not None:
                logger.warning("agento11y: embedding recorder error: %s", err)
