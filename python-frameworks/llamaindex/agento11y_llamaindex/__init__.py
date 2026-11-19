"""Public exports for agento11y LlamaIndex callback handlers."""

from __future__ import annotations

import asyncio
import inspect
import json
from typing import Any
from uuid import UUID, uuid4

from agento11y import Client
from agento11y.models import Message, MessageRole, Part, PartKind, ToolCall, ToolResult
from agento11y.usage import from_anthropic

from ._instrumentation import current_span_id, register_bridge
from .handler import Agento11yAsyncLlamaIndexHandler, Agento11yLlamaIndexHandler

try:  # pragma: no cover - imported from llama-index at runtime
    from llama_index.core.callbacks import CallbackManager
    from llama_index.core.callbacks.base_handler import BaseCallbackHandler
except Exception:  # pragma: no cover - lightweight fallback for local unit tests

    class BaseCallbackHandler:  # type: ignore[no-redef]
        """Fallback BaseCallbackHandler shape used when llama-index isn't installed."""

        def __init__(self, event_starts_to_ignore: list[str], event_ends_to_ignore: list[str]) -> None:
            self.event_starts_to_ignore = tuple(event_starts_to_ignore)
            self.event_ends_to_ignore = tuple(event_ends_to_ignore)

        def on_event_start(
            self,
            event_type: Any,
            payload: dict[str, Any] | None = None,
            event_id: str = "",
            parent_id: str = "",
            **kwargs: Any,
        ) -> str:
            raise NotImplementedError

        def on_event_end(
            self,
            event_type: Any,
            payload: dict[str, Any] | None = None,
            event_id: str = "",
            **kwargs: Any,
        ) -> None:
            raise NotImplementedError

        def start_trace(self, trace_id: str | None = None) -> None:
            return

        def end_trace(self, trace_id: str | None = None, trace_map: dict[str, list[str]] | None = None) -> None:
            return

    class CallbackManager:  # type: ignore[no-redef]
        """Fallback CallbackManager shape used when llama-index isn't installed."""

        def __init__(self, handlers: list[Any] | None = None) -> None:
            self.handlers = list(handlers or [])

        def add_handler(self, handler: Any) -> None:
            self.handlers.append(handler)


_chain_event_types = {"query", "retrieve", "synthesize", "tree", "sub_question", "agent_step"}


def create_agento11y_llamaindex_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> Agento11yLlamaIndexHandler | Agento11yAsyncLlamaIndexHandler:
    """Create a LlamaIndex agento11y callback handler for sync or async flows."""
    if async_handler:
        return Agento11yAsyncLlamaIndexHandler(client=client, **handler_kwargs)
    return Agento11yLlamaIndexHandler(client=client, **handler_kwargs)


class Agento11yLlamaIndexCallbackHandler(BaseCallbackHandler):
    """LlamaIndex BaseCallbackHandler bridge that forwards lifecycle events to Agent Observability."""

    def __init__(self, agento11y_handler: Agento11yLlamaIndexHandler | Agento11yAsyncLlamaIndexHandler) -> None:
        super().__init__(event_starts_to_ignore=[], event_ends_to_ignore=[])
        self._agento11y_handler = agento11y_handler
        self._run_ids: dict[str, UUID] = {}
        self._run_kinds: dict[str, str] = {}
        # Instrumentation span id of each open LLM call -> callback event id.
        self._llm_span_events: dict[str, str] = {}
        self._llm_event_spans: dict[str, str] = {}
        # Per LLM event: stop reason and usage seen on streamed chunks.
        self._llm_stream_state: dict[str, dict[str, Any]] = {}
        # Instrumentation span id of each open workflow tool step -> tool run id.
        self._tool_step_runs: dict[str, UUID] = {}
        # Tool call ids returned by this handler's LLM calls and not yet executed.
        self._pending_tool_call_ids: dict[str, bool] = {}
        register_bridge(self)

    def start_trace(self, trace_id: str | None = None) -> None:
        return

    def end_trace(self, trace_id: str | None = None, trace_map: dict[str, list[str]] | None = None) -> None:
        return

    def on_event_start(
        self,
        event_type: Any,
        payload: dict[str, Any] | None = None,
        event_id: str = "",
        parent_id: str = "",
        **_kwargs: Any,
    ) -> str:
        current_event_id = event_id or str(uuid4())
        event_name = _event_name(event_type)
        event_payload = payload or {}
        parent_run_id = self._run_ids.get(parent_id)

        if event_name == "llm":
            run_id = uuid4()
            self._run_ids[current_event_id] = run_id
            self._run_kinds[current_event_id] = "llm"
            serialized, invocation_params = _llm_serialized_payload(event_payload)
            span_id = current_span_id()
            if span_id != "":
                self._llm_span_events[span_id] = current_event_id
                self._llm_event_spans[current_event_id] = span_id
                self._llm_stream_state[current_event_id] = {}
                if _is_streaming_span(span_id):
                    invocation_params["stream"] = True
            metadata = _event_metadata(event_payload, event_id=current_event_id)
            messages = _llm_messages(event_payload)
            if messages:
                _invoke_handler(
                    self._agento11y_handler,
                    "on_chat_model_start",
                    serialized,
                    [messages],
                    run_id=run_id,
                    parent_run_id=parent_run_id,
                    invocation_params=invocation_params,
                    metadata=metadata,
                    run_name="llamaindex.llm",
                )
            else:
                _invoke_handler(
                    self._agento11y_handler,
                    "on_llm_start",
                    serialized,
                    _llm_prompts(event_payload),
                    run_id=run_id,
                    parent_run_id=parent_run_id,
                    invocation_params=invocation_params,
                    metadata=metadata,
                    run_name="llamaindex.llm",
                )
            return current_event_id

        if event_name == "function_call":
            run_id = uuid4()
            self._run_ids[current_event_id] = run_id
            self._run_kinds[current_event_id] = "tool"
            tool_name = _tool_name(event_payload)
            _invoke_handler(
                self._agento11y_handler,
                "on_tool_start",
                {"name": tool_name},
                _tool_input_string(_read(event_payload, "function_call")),
                run_id=run_id,
                parent_run_id=parent_run_id,
                metadata=_event_metadata(event_payload, event_id=current_event_id),
                run_name=tool_name,
                inputs=_read(event_payload, "function_call"),
            )
            return current_event_id

        if event_name in _chain_event_types:
            run_id = uuid4()
            self._run_ids[current_event_id] = run_id
            self._run_kinds[current_event_id] = "chain"
            chain_name = f"llamaindex.{event_name}"
            _invoke_handler(
                self._agento11y_handler,
                "on_chain_start",
                {"name": chain_name},
                event_payload,
                run_id=run_id,
                parent_run_id=parent_run_id,
                metadata=_event_metadata(event_payload, event_id=current_event_id),
                run_type=event_name,
                run_name=chain_name,
            )

        return current_event_id

    def on_event_end(
        self,
        event_type: Any,
        payload: dict[str, Any] | None = None,
        event_id: str = "",
        **_kwargs: Any,
    ) -> None:
        if event_id == "":
            return
        run_id = self._run_ids.pop(event_id, None)
        run_kind = self._run_kinds.pop(event_id, "")
        if run_id is None:
            return

        event_name = _event_name(event_type)
        event_payload = payload or {}

        if run_kind == "llm" or event_name == "llm":
            span_id = self._llm_event_spans.pop(event_id, "")
            self._llm_span_events.pop(span_id, None)
            stream_state = self._llm_stream_state.pop(event_id, {})
            exception = _read(event_payload, "exception")
            if isinstance(exception, BaseException):
                _invoke_handler(self._agento11y_handler, "on_llm_error", exception, run_id=run_id)
                return
            end_payload = _llm_end_payload(event_payload, stream_state)
            for call_id in _tool_call_ids(end_payload):
                self._pending_tool_call_ids[call_id] = True
            _invoke_handler(self._agento11y_handler, "on_llm_end", end_payload, run_id=run_id)
            return

        if run_kind == "tool" or event_name == "function_call":
            _invoke_handler(
                self._agento11y_handler,
                "on_tool_end",
                _read(event_payload, "function_call_response"),
                run_id=run_id,
            )
            return

        if run_kind == "chain" or event_name in _chain_event_types:
            _invoke_handler(self._agento11y_handler, "on_chain_end", event_payload, run_id=run_id)

    def _on_llm_stream_chunk(self, span_id: str, response: Any) -> None:
        """Record time to first token, stop reason, and usage from one streamed chunk."""
        event_id = self._llm_span_events.get(span_id)
        if event_id is None:
            return
        run_id = self._run_ids.get(event_id)
        state = self._llm_stream_state.get(event_id)
        if run_id is None or state is None:
            return

        raw = _read(response, "raw")
        stop_reason = _raw_stop_reason(raw, response)
        if stop_reason != "":
            state["stop_reason"] = stop_reason
        usage = _raw_usage(raw)
        if usage is not None:
            state["usage"] = usage

        message = _read(response, "message")
        tool_call_chunks = [
            {"name": _as_string(_read(block, "tool_name"))}
            for block in _as_list(_read(message, "blocks"))
            if _read(block, "block_type") == "tool_call"
        ]
        delta = _read(response, "delta")
        self._agento11y_handler._on_llm_new_token(
            token=delta if isinstance(delta, str) else "",
            run_id=run_id,
            chunk={"message": {"tool_call_chunks": tool_call_chunks}},
        )

    def _on_tool_step_start(self, span_id: str, tool_call: Any) -> None:
        """Start a tool execution for a workflow agent's ``call_tool`` step.

        The step carries no reference to its agent, so a handler records only the
        tool calls that one of its own LLM calls returned.
        """
        tool_call_id = _as_string(_read(tool_call, "tool_id"))
        if span_id in self._tool_step_runs or self._pending_tool_call_ids.pop(tool_call_id, False) is False:
            return
        tool_name = _first_non_empty(_as_string(_read(tool_call, "tool_name")), "framework_tool")
        tool_kwargs = _read(tool_call, "tool_kwargs")
        run_id = uuid4()
        self._tool_step_runs[span_id] = run_id
        _invoke_handler(
            self._agento11y_handler,
            "on_tool_start",
            {"name": tool_name},
            _tool_input_string(tool_kwargs),
            run_id=run_id,
            run_name=tool_name,
            tool_call_id=tool_call_id,
            inputs=tool_kwargs,
        )

    def _on_tool_step_end(self, span_id: str, *, result: Any = None, error: BaseException | None = None) -> None:
        run_id = self._tool_step_runs.pop(span_id, None)
        if run_id is None:
            return
        tool_output = _read(result, "tool_output")
        if error is None and _read(tool_output, "is_error") is True:
            error = RuntimeError(_as_string(_read(tool_output, "content")) or "tool call failed")
        if error is not None:
            _invoke_handler(self._agento11y_handler, "on_tool_error", error, run_id=run_id)
            return
        _invoke_handler(
            self._agento11y_handler,
            "on_tool_end",
            _first_present(_read(tool_output, "content"), _read(tool_output, "raw_output")),
            run_id=run_id,
        )


def create_agento11y_llamaindex_callback_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> Agento11yLlamaIndexCallbackHandler:
    """Create a callback-manager compatible LlamaIndex handler."""
    return Agento11yLlamaIndexCallbackHandler(
        create_agento11y_llamaindex_handler(
            client=client,
            async_handler=async_handler,
            **handler_kwargs,
        )
    )


def with_agento11y_llamaindex_callbacks(
    config: dict[str, Any] | None,
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> dict[str, Any]:
    """Register agento11y through LlamaIndex `callback_manager` wiring."""
    merged = dict(config or {})

    callback_manager = merged.get("callback_manager")
    if callback_manager is None:
        callback_manager = CallbackManager([])
        merged["callback_manager"] = callback_manager

    if not hasattr(callback_manager, "add_handler"):
        raise TypeError("llamaindex callback_manager must expose add_handler(handler).")

    existing_handlers = _as_list(_read(merged, "callbacks"))
    for existing_handler in existing_handlers:
        if _is_callback_handler(existing_handler):
            _add_handler(callback_manager, existing_handler)
    merged.pop("callbacks", None)

    if not _has_agento11y_handler(callback_manager):
        _add_handler(
            callback_manager,
            create_agento11y_llamaindex_callback_handler(
                client=client,
                async_handler=async_handler,
                **handler_kwargs,
            ),
        )

    return merged


def _invoke_handler(handler: Any, method_name: str, *args: Any, **kwargs: Any) -> None:
    method = getattr(handler, method_name)
    result = method(*args, **kwargs)
    if inspect.isawaitable(result):
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            asyncio.run(result)
        else:
            loop.create_task(result)


def _event_name(event_type: Any) -> str:
    value = _read(event_type, "value")
    if isinstance(value, str):
        return value.strip().lower()
    return _as_string(event_type).lower()


def _llm_serialized_payload(payload: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]:
    serialized = _as_dict(_read(payload, "serialized"))
    if not serialized:
        serialized = {"name": "llamaindex.llm"}
    model_name = _first_non_empty(
        _as_string(_read(payload, "model_name")),
        _as_string(_read(_read(payload, "response"), "model")),
    )
    invocation_params = _as_dict(_read(payload, "additional_kwargs"))
    if model_name != "":
        invocation_params["model"] = model_name
    if model_name != "" and _read(serialized, "kwargs") is None:
        serialized["kwargs"] = {"model": model_name}
    return serialized, invocation_params


def _llm_prompts(payload: dict[str, Any]) -> list[str]:
    prompts: list[str] = []
    prompt = _first_non_empty(
        _as_string(_read(payload, "formatted_prompt")),
        _as_string(_read(payload, "prompt")),
        _as_string(_read(payload, "query_str")),
    )
    if prompt != "":
        prompts.append(prompt)
    messages = _llm_messages(payload)
    if messages and not prompts:
        prompts.extend(_as_string(_read(message, "content")) for message in messages)
    return [prompt for prompt in prompts if prompt != ""] or [""]


def _llm_messages(payload: dict[str, Any]) -> list[Any]:
    """Map the LLM call's input messages.

    System messages stay as ``{"role", "content"}`` dicts so the base handler
    moves them into the system prompt. Every other message becomes an agento11y
    ``Message`` that keeps tool calls and tool results, not only its text.
    """
    messages = _read(payload, "messages")
    if not isinstance(messages, list):
        return []
    normalized: list[Any] = []
    for item in messages:
        role = _first_non_empty(_role_name(_read(item, "role")), "user")
        if role in {"system", "developer"}:
            text = _message_text(item)
            if text != "":
                normalized.append({"role": role, "content": text})
            continue
        mapped = _map_llamaindex_message(item, role=role)
        if mapped is not None:
            normalized.append(mapped)
    return normalized


def _map_llamaindex_message(message: Any, *, role: str = "") -> Message | None:
    """Map a LlamaIndex ``ChatMessage`` (or a dict of the same shape) to an agento11y ``Message``."""
    role = role or _first_non_empty(_role_name(_read(message, "role")), "assistant")
    additional_kwargs = _read(message, "additional_kwargs")

    if role == "tool":
        tool_call_id = _first_non_empty(
            _as_string(_read(additional_kwargs, "tool_call_id")),
            _as_string(_read(additional_kwargs, "tool_use_id")),
        )
        if tool_call_id != "":
            return Message(
                role=MessageRole.TOOL,
                parts=[
                    Part(
                        kind=PartKind.TOOL_RESULT,
                        tool_result=ToolResult(
                            tool_call_id=tool_call_id,
                            name=_as_string(_read(additional_kwargs, "name")),
                            content=_message_text(message),
                        ),
                    )
                ],
            )

    parts: list[Part] = []
    blocks = _read(message, "blocks")
    if isinstance(blocks, list):
        for block in blocks:
            block_type = _as_string(_read(block, "block_type"))
            if block_type == "text":
                text = _as_string(_read(block, "text"))
                if text != "":
                    parts.append(Part(kind=PartKind.TEXT, text=text))
            elif block_type == "thinking":
                thinking = _as_string(_read(block, "content"))
                if thinking != "":
                    parts.append(Part(kind=PartKind.THINKING, thinking=thinking))
            elif block_type == "tool_call" and _as_string(_read(block, "tool_name")) != "":
                parts.append(
                    _tool_call_part(
                        _read(block, "tool_call_id"), _read(block, "tool_name"), _read(block, "tool_kwargs")
                    )
                )
    else:
        text = _message_text(message)
        if text != "":
            parts.append(Part(kind=PartKind.TEXT, text=text))

    # Older integrations keep provider tool calls only in additional_kwargs.
    if not any(part.kind == PartKind.TOOL_CALL for part in parts):
        for tool_call in _as_list(_read(additional_kwargs, "tool_calls")):
            function = _read(tool_call, "function")
            name = _first_non_empty(_as_string(_read(function, "name")), _as_string(_read(tool_call, "name")))
            if name == "":
                continue
            arguments = _first_present(_read(function, "arguments"), _read(tool_call, "input"))
            parts.append(_tool_call_part(_read(tool_call, "id"), name, arguments))

    if not parts:
        return None
    if role == "tool":
        return Message(role=MessageRole.TOOL, parts=parts)
    return Message(role=MessageRole.ASSISTANT if role == "assistant" else MessageRole.USER, parts=parts)


def _tool_call_ids(end_payload: dict[str, Any]) -> list[str]:
    ids: list[str] = []
    for candidates in _as_list(end_payload.get("generations")):
        for candidate in _as_list(candidates):
            for part in _as_list(_read(_read(candidate, "message"), "parts")):
                if part.kind == PartKind.TOOL_CALL and part.tool_call is not None and part.tool_call.id:
                    ids.append(part.tool_call.id)
    return ids


def _tool_call_part(call_id: Any, name: Any, arguments: Any) -> Part:
    if isinstance(arguments, str):
        input_json = arguments.encode("utf-8")
    elif arguments is None:
        input_json = b""
    else:
        input_json = json.dumps(arguments, default=str, sort_keys=True).encode("utf-8")
    return Part(
        kind=PartKind.TOOL_CALL,
        tool_call=ToolCall(id=_as_string(call_id), name=_as_string(name), input_json=input_json),
    )


def _role_name(role: Any) -> str:
    value = _read(role, "value")
    return _as_string(value if isinstance(value, str) else role).lower()


def _llm_end_payload(payload: dict[str, Any], stream_state: dict[str, Any] | None = None) -> dict[str, Any]:
    stream_state = stream_state or {}
    llm_output: dict[str, Any] = {}
    response = _first_present(_read(payload, "response"), _read(payload, "completion"))
    raw = _read(response, "raw")
    model_name = _first_non_empty(
        _as_string(_read(payload, "model_name")),
        _as_string(_read(response, "model")),
        _as_string(_read(raw, "model")),
    )
    if model_name != "":
        llm_output["model_name"] = model_name

    stop_reason = _first_non_empty(
        _raw_stop_reason(raw, response),
        _as_string(stream_state.get("stop_reason")),
        _llm_stop_reason(payload, response),
    )
    if stop_reason != "":
        llm_output["finish_reason"] = stop_reason

    usage = _first_present(_raw_usage(raw), stream_state.get("usage"))
    if usage is None:
        usage = _llm_usage(payload, response) or None
    if usage:
        llm_output["token_usage"] = usage

    result: dict[str, Any] = {"llm_output": llm_output}
    message = _read(response, "message")
    if message is not None:
        mapped = _map_llamaindex_message(message)
        if mapped is not None:
            result["generations"] = [[{"message": mapped}]]
            return result

    text = _first_non_empty(
        _message_text(response),
        _as_string(_read(payload, "completion")),
    )
    if text == "":
        text = _message_text(payload)
    if text != "":
        result["generations"] = [[{"text": text}]]
    return result


def _raw_stop_reason(raw: Any, response: Any) -> str:
    """Read the stop reason from a raw OpenAI or Anthropic response or chunk."""
    choices = _read(raw, "choices")
    if isinstance(choices, list) and choices:
        reason = _as_string(_read(choices[0], "finish_reason"))
        if reason != "":
            return reason
    message_additional = _read(_read(response, "message"), "additional_kwargs")
    return _first_non_empty(
        _as_string(_read(raw, "stop_reason")),
        _as_string(_read(_read(raw, "delta"), "stop_reason")),
        _as_string(_read(message_additional, "stop_reason")),
    )


def _raw_usage(raw: Any) -> Any:
    """Return usage from a raw provider response, in the OpenAI chat shape.

    OpenAI usage passes through. Anthropic reports ``input_tokens`` without the
    cache buckets, so it is normalized with ``from_anthropic`` and handed on in
    the inclusive OpenAI shape, which keeps the cache read and write counts.
    """
    usage = _read(raw, "usage")
    if usage is None:
        return None
    if _read(usage, "prompt_tokens") is not None:
        return usage
    if _read(usage, "input_tokens") is not None:
        normalized = from_anthropic(usage)
        return {
            "prompt_tokens": normalized.input_tokens,
            "completion_tokens": normalized.output_tokens,
            "total_tokens": normalized.total_tokens,
            "prompt_tokens_details": {
                "cached_tokens": normalized.cache_read_input_tokens,
                "cache_creation_tokens": normalized.cache_write_input_tokens,
            },
        }
    return None


def _is_streaming_span(span_id: str) -> bool:
    method = span_id.split("-", 1)[0].rsplit(".", 1)[-1]
    return method in {"stream_chat", "astream_chat", "stream_complete", "astream_complete"}


def _llm_stop_reason(payload: dict[str, Any], response: Any) -> str:
    payload_additional = _read(payload, "additional_kwargs")
    response_additional = _read(response, "additional_kwargs")
    return _first_non_empty(
        _as_string(_read(payload, "finish_reason")),
        _as_string(_read(payload, "finishReason")),
        _as_string(_read(payload_additional, "finish_reason")),
        _as_string(_read(payload_additional, "finishReason")),
        _as_string(_read(response, "finish_reason")),
        _as_string(_read(response, "finishReason")),
        _as_string(_read(response_additional, "finish_reason")),
        _as_string(_read(response_additional, "finishReason")),
    )


def _llm_usage(payload: dict[str, Any], response: Any) -> dict[str, int]:
    usage_sources = (
        _read(response, "usage"),
        _read(payload, "usage"),
        # LlamaIndex's OpenAI integration copies the token counts here.
        _read(response, "additional_kwargs"),
    )
    for usage in usage_sources:
        parsed = _parse_usage(usage)
        if parsed:
            return parsed
    return {}


def _parse_usage(raw_usage: Any) -> dict[str, int]:
    prompt_tokens = _int_or_none(_read(raw_usage, "prompt_tokens"))
    if prompt_tokens is None:
        prompt_tokens = _int_or_none(_read(raw_usage, "promptTokens"))
    completion_tokens = _int_or_none(_read(raw_usage, "completion_tokens"))
    if completion_tokens is None:
        completion_tokens = _int_or_none(_read(raw_usage, "completionTokens"))
    total_tokens = _int_or_none(_read(raw_usage, "total_tokens"))
    if total_tokens is None:
        total_tokens = _int_or_none(_read(raw_usage, "totalTokens"))

    token_usage: dict[str, int] = {}
    if prompt_tokens is not None:
        token_usage["prompt_tokens"] = prompt_tokens
    if completion_tokens is not None:
        token_usage["completion_tokens"] = completion_tokens
    if total_tokens is not None:
        token_usage["total_tokens"] = total_tokens
    return token_usage


def _event_metadata(payload: dict[str, Any], *, event_id: str) -> dict[str, Any]:
    metadata: dict[str, Any] = {"event_id": event_id}
    additional_kwargs = _as_dict(_read(payload, "additional_kwargs"))
    for source in (payload, additional_kwargs):
        for key in ("conversation_id", "session_id", "group_id", "thread_id"):
            value = _first_non_empty(
                _as_string(_read(source, key)),
                _as_string(_read(source, _camel_case(key))),
            )
            if value != "":
                metadata[key] = value
    return metadata


def _tool_name(payload: dict[str, Any]) -> str:
    tool = _read(payload, "tool")
    candidates = (
        _as_string(_read(tool, "name")),
        _as_string(tool),
    )
    for candidate in candidates:
        if candidate != "":
            return candidate
    return "framework_tool"


def _tool_input_string(value: Any) -> str:
    if isinstance(value, str):
        return value
    if value is None:
        return ""
    try:
        return json.dumps(value, default=str, sort_keys=True)
    except Exception:
        return str(value)


def _add_handler(callback_manager: Any, handler: Any) -> None:
    callback_manager.add_handler(handler)


def _is_callback_handler(value: Any) -> bool:
    required_methods = ("on_event_start", "on_event_end", "start_trace", "end_trace")
    return all(callable(getattr(value, method_name, None)) for method_name in required_methods)


def _has_agento11y_handler(callback_manager: Any) -> bool:
    handlers = _as_list(_read(callback_manager, "handlers"))
    return any(isinstance(handler, Agento11yLlamaIndexCallbackHandler) for handler in handlers)


def _message_text(value: Any) -> str:
    if isinstance(value, str):
        return value.strip()
    if isinstance(value, list):
        parts = [_message_text(item) for item in value]
        return " ".join(part for part in parts if part != "").strip()

    content = _read(value, "content")
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        parts = [_message_text(item) for item in content]
        return " ".join(part for part in parts if part != "").strip()
    if isinstance(content, dict):
        text = _as_string(_read(content, "text"))
        if text != "":
            return text

    text = _as_string(_read(value, "text"))
    if text != "":
        return text
    return _as_string(_read(value, "message"))


def _read(value: Any, key: str) -> Any:
    if value is None:
        return None
    if isinstance(value, dict):
        return value.get(key)
    return getattr(value, key, None)


def _as_dict(value: Any) -> dict[str, Any]:
    return dict(value) if isinstance(value, dict) else {}


def _as_list(value: Any) -> list[Any]:
    if isinstance(value, list):
        return value
    if value is None:
        return []
    return [value]


def _as_string(value: Any) -> str:
    if isinstance(value, str):
        return value.strip()
    return "" if value is None else str(value).strip()


def _int_or_none(value: Any) -> int | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    return None


def _first_present(*values: Any) -> Any:
    for value in values:
        if value is not None:
            return value
    return None


def _first_non_empty(*values: str) -> str:
    for value in values:
        if value != "":
            return value
    return ""


def _camel_case(snake_case: str) -> str:
    pieces = snake_case.split("_")
    if not pieces:
        return snake_case
    return pieces[0] + "".join(piece.capitalize() for piece in pieces[1:])


__all__ = [
    "Agento11yLlamaIndexHandler",
    "Agento11yAsyncLlamaIndexHandler",
    "Agento11yLlamaIndexCallbackHandler",
    "create_agento11y_llamaindex_handler",
    "create_agento11y_llamaindex_callback_handler",
    "with_agento11y_llamaindex_callbacks",
]
