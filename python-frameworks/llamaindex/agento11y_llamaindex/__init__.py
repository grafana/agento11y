"""Public exports for Sigil LlamaIndex callback handlers."""

from __future__ import annotations

import asyncio
import inspect
import json
from typing import Any
from uuid import UUID, uuid4

from agento11y import Client

from .handler import SigilAsyncLlamaIndexHandler, SigilLlamaIndexHandler

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


def create_sigil_llamaindex_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> SigilLlamaIndexHandler | SigilAsyncLlamaIndexHandler:
    """Create a LlamaIndex Sigil callback handler for sync or async flows."""
    if async_handler:
        return SigilAsyncLlamaIndexHandler(client=client, **handler_kwargs)
    return SigilLlamaIndexHandler(client=client, **handler_kwargs)


class SigilLlamaIndexCallbackHandler(BaseCallbackHandler):
    """LlamaIndex BaseCallbackHandler bridge that forwards lifecycle events to Sigil."""

    def __init__(self, sigil_handler: SigilLlamaIndexHandler | SigilAsyncLlamaIndexHandler) -> None:
        super().__init__(event_starts_to_ignore=[], event_ends_to_ignore=[])
        self._sigil_handler = sigil_handler
        self._run_ids: dict[str, UUID] = {}
        self._run_kinds: dict[str, str] = {}

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
            metadata = _event_metadata(event_payload, event_id=current_event_id)
            messages = _llm_messages(event_payload)
            if messages:
                _invoke_handler(
                    self._sigil_handler,
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
                    self._sigil_handler,
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
                self._sigil_handler,
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
                self._sigil_handler,
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
            _invoke_handler(
                self._sigil_handler,
                "on_llm_end",
                _llm_end_payload(event_payload),
                run_id=run_id,
            )
            return

        if run_kind == "tool" or event_name == "function_call":
            _invoke_handler(
                self._sigil_handler,
                "on_tool_end",
                _read(event_payload, "function_call_response"),
                run_id=run_id,
            )
            return

        if run_kind == "chain" or event_name in _chain_event_types:
            _invoke_handler(self._sigil_handler, "on_chain_end", event_payload, run_id=run_id)


def create_sigil_llamaindex_callback_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> SigilLlamaIndexCallbackHandler:
    """Create a callback-manager compatible LlamaIndex handler."""
    return SigilLlamaIndexCallbackHandler(
        create_sigil_llamaindex_handler(
            client=client,
            async_handler=async_handler,
            **handler_kwargs,
        )
    )


def with_sigil_llamaindex_callbacks(
    config: dict[str, Any] | None,
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> dict[str, Any]:
    """Register Sigil through LlamaIndex `callback_manager` wiring."""
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

    if not _has_sigil_handler(callback_manager):
        _add_handler(
            callback_manager,
            create_sigil_llamaindex_callback_handler(
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


def _llm_messages(payload: dict[str, Any]) -> list[dict[str, Any]]:
    messages = _read(payload, "messages")
    if not isinstance(messages, list):
        return []
    normalized: list[dict[str, Any]] = []
    for item in messages:
        role = _first_non_empty(_as_string(_read(item, "role")), "user")
        text = _message_text(item)
        if text == "":
            continue
        normalized.append({"role": role, "content": text})
    return normalized


def _llm_end_payload(payload: dict[str, Any]) -> dict[str, Any]:
    llm_output: dict[str, Any] = {}
    response = _read(payload, "response")
    model_name = _first_non_empty(
        _as_string(_read(payload, "model_name")),
        _as_string(_read(response, "model")),
    )
    if model_name != "":
        llm_output["model_name"] = model_name

    stop_reason = _llm_stop_reason(payload, response)
    if stop_reason != "":
        llm_output["finish_reason"] = stop_reason

    usage = _llm_usage(payload, response)
    if usage:
        llm_output["token_usage"] = usage

    text = _first_non_empty(
        _message_text(response),
        _as_string(_read(payload, "completion")),
    )
    if text == "":
        text = _message_text(payload)

    result: dict[str, Any] = {"llm_output": llm_output}
    if text != "":
        result["generations"] = [[{"text": text}]]
    return result


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


def _has_sigil_handler(callback_manager: Any) -> bool:
    handlers = _as_list(_read(callback_manager, "handlers"))
    return any(isinstance(handler, SigilLlamaIndexCallbackHandler) for handler in handlers)


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
    "SigilLlamaIndexHandler",
    "SigilAsyncLlamaIndexHandler",
    "SigilLlamaIndexCallbackHandler",
    "create_sigil_llamaindex_handler",
    "create_sigil_llamaindex_callback_handler",
    "with_sigil_llamaindex_callbacks",
]
