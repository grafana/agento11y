from __future__ import annotations

import json
import logging
from typing import Any
from uuid import UUID, uuid4

from agento11y import Client, TokenUsage, ToolDefinition
from pydantic_ai.capabilities import AbstractCapability
from pydantic_ai.exceptions import (
    ApprovalRequired,
    CallDeferred,
    ModelRetry,
    SkipModelRequest,
    SkipToolExecution,
    SkipToolValidation,
    ToolRetryError,
)

from .handler import SigilPydanticAIHandler

logger = logging.getLogger(__name__)

# Pydantic AI control-flow exceptions that must propagate without error recording.
_PYDANTIC_CONTROL_FLOW: tuple[type[BaseException], ...] = (
    ModelRetry,
    SkipModelRequest,
    SkipToolValidation,
    SkipToolExecution,
    CallDeferred,
    ApprovalRequired,
    ToolRetryError,
)


def create_sigil_pydantic_ai_handler(*, client: Client, **handler_kwargs: Any) -> SigilPydanticAIHandler:
    """Create a Pydantic AI Sigil handler."""
    return SigilPydanticAIHandler(client=client, **handler_kwargs)


class SigilPydanticAICapability(AbstractCapability):
    """Pydantic AI capability bridge that maps lifecycle hooks to Sigil handlers."""

    def __init__(self, sigil_handler: SigilPydanticAIHandler) -> None:
        self._sigil_handler = sigil_handler
        self._run_stack: list[UUID] = []
        self._llm_run_ids: list[UUID] = []

    async def for_run(self, ctx: Any) -> SigilPydanticAICapability:
        """Per-run isolated state — each agent run gets its own stacks."""
        return SigilPydanticAICapability(self._sigil_handler)

    async def wrap_run(self, ctx: Any, *, handler: Any) -> Any:
        run_id = uuid4()
        self._run_stack.append(run_id)

        run_name = _agent_name(ctx)
        metadata = _run_context_metadata(ctx)
        try:
            self._sigil_handler.on_chain_start(
                {"name": run_name},
                {},
                run_id=run_id,
                parent_run_id=None,
                metadata=metadata,
                run_type="agent",
                run_name=run_name,
            )
        except BaseException:
            _remove(self._run_stack, run_id)
            raise

        try:
            result = await handler()
        except BaseException as exc:
            _remove(self._run_stack, run_id)
            if isinstance(exc, _PYDANTIC_CONTROL_FLOW):
                self._sigil_handler._discard_chain_run(run_id=run_id)
            else:
                try:
                    self._sigil_handler.on_chain_error(exc, run_id=run_id)
                except Exception:
                    logger.debug("sigil: failed to record chain error", exc_info=True)
            raise

        _remove(self._run_stack, run_id)
        try:
            self._sigil_handler.on_chain_end({"status": "completed"}, run_id=run_id)
        except Exception:
            logger.debug("sigil: failed to record chain end", exc_info=True)
        return result

    async def wrap_model_request(self, ctx: Any, *, request_context: Any, handler: Any) -> Any:
        run_id = uuid4()
        self._llm_run_ids.append(run_id)

        parent_run_id = self._run_stack[-1] if self._run_stack else None

        model_name = _resolve_pydantic_model_name(request_context)
        invocation_params = _build_invocation_params(model_name, request_context, handler)

        messages = _map_pydantic_messages(request_context)
        serialized: dict[str, Any] = {"name": _agent_name(ctx)}
        if model_name != "":
            serialized["kwargs"] = {"model": model_name}

        metadata = _run_context_metadata(ctx)
        try:
            self._sigil_handler.on_chat_model_start(
                serialized,
                [messages],
                run_id=run_id,
                parent_run_id=parent_run_id,
                invocation_params=invocation_params,
                metadata=metadata,
                run_name=_agent_name(ctx),
            )
        except BaseException:
            _remove(self._llm_run_ids, run_id)
            raise

        try:
            response = await handler(request_context)
        except BaseException as exc:
            _remove(self._llm_run_ids, run_id)
            if isinstance(exc, _PYDANTIC_CONTROL_FLOW):
                self._sigil_handler._discard_llm_run(run_id=run_id)
            else:
                try:
                    self._sigil_handler.on_llm_error(exc, run_id=run_id)
                except Exception:
                    logger.debug("sigil: failed to record LLM error", exc_info=True)
            raise

        _remove(self._llm_run_ids, run_id)
        try:
            self._sigil_handler.on_llm_end(_map_pydantic_response(response, model_name), run_id=run_id)
        except Exception:
            logger.debug("sigil: failed to record LLM end", exc_info=True)
        return response

    async def on_model_request_error(self, ctx: Any, *, request_context: Any, error: Any) -> Any:
        # wrap_model_request already handles cleanup and error reporting;
        # this hook fires after the wrapper re-raises, so just propagate.
        raise error

    async def wrap_tool_execute(self, ctx: Any, *, call: Any, tool_def: Any, args: Any, handler: Any) -> Any:
        run_id = uuid4()

        # Pydantic AI runs tools in CallToolsNode after ModelRequestNode
        # completes, so _llm_run_ids is always empty here. Use the agent run.
        parent_run_id = self._run_stack[-1] if self._run_stack else None

        tool_name = _tool_name(call, tool_def)
        metadata = _run_context_metadata(ctx)
        self._sigil_handler.on_tool_start(
            {"name": tool_name, "description": _as_string(_read(tool_def, "description"))},
            "",
            run_id=run_id,
            parent_run_id=parent_run_id,
            metadata=metadata,
            run_name=tool_name,
            inputs=args,
        )

        try:
            result = await handler(args)
        except BaseException as exc:
            if isinstance(exc, _PYDANTIC_CONTROL_FLOW):
                self._sigil_handler._discard_tool_run(run_id=run_id)
            else:
                try:
                    self._sigil_handler.on_tool_error(exc, run_id=run_id)
                except Exception:
                    logger.debug("sigil: failed to record tool error", exc_info=True)
            raise

        try:
            self._sigil_handler.on_tool_end(result, run_id=run_id)
        except Exception:
            logger.debug("sigil: failed to record tool end", exc_info=True)
        return result

    async def on_tool_execute_error(self, ctx: Any, *, call: Any, tool_def: Any, args: Any, error: Any) -> Any:
        # wrap_tool_execute already handles cleanup and error reporting;
        # this hook fires after the wrapper re-raises, so just propagate.
        raise error

    async def wrap_run_event_stream(self, ctx: Any, *, stream: Any) -> Any:
        async for event in stream:
            token = _extract_stream_token(event)
            if token != "" and self._llm_run_ids:
                try:
                    self._sigil_handler.on_llm_new_token(token, run_id=self._llm_run_ids[-1])
                except Exception:
                    logger.debug("sigil: failed to record stream token", exc_info=True)
            yield event


def create_sigil_pydantic_ai_capability(*, client: Client, **handler_kwargs: Any) -> SigilPydanticAICapability:
    """Create a Pydantic AI capability wired to Sigil instrumentation."""
    return SigilPydanticAICapability(create_sigil_pydantic_ai_handler(client=client, **handler_kwargs))


def with_sigil_pydantic_ai_capability(
    capabilities: list[Any] | None,
    *,
    client: Client,
    **handler_kwargs: Any,
) -> list[Any]:
    """Add a Sigil capability to a list, with double-injection guard."""
    result = list(capabilities or [])
    if any(isinstance(cap, SigilPydanticAICapability) for cap in result):
        return result
    result.append(create_sigil_pydantic_ai_capability(client=client, **handler_kwargs))
    return result


def _remove(stack: list[UUID], run_id: UUID) -> None:
    try:
        stack.remove(run_id)
    except ValueError:
        pass


def _agent_name(ctx: Any) -> str:
    for candidate in (
        _as_string(_read(_read(ctx, "agent"), "name")),
        _as_string(_read(_read(ctx, "model"), "name")),
        _as_string(_read(_read(ctx, "model"), "model_name")),
    ):
        if candidate != "":
            return candidate
    return "pydantic_ai_agent"


def _build_invocation_params(model_name: str, request_context: Any, handler: Any) -> dict[str, Any]:
    params: dict[str, Any] = {}
    if model_name != "":
        params["model"] = model_name

    settings = _read(request_context, "model_settings")
    for key in ("temperature", "max_tokens", "top_p", "tool_choice", "seed", "presence_penalty", "frequency_penalty"):
        value = _read(settings, key)
        if value is not None:
            params[key] = value

    request_params = _read(request_context, "model_request_parameters")
    tools = _map_pydantic_tools(request_params)
    if tools:
        params["tools"] = tools

    system_prompt = _join_instruction_parts(request_params)
    if system_prompt != "":
        params["system_prompt"] = system_prompt

    # Pydantic AI streams via a dedicated handler closure named `_streaming_handler`
    # in `pydantic_ai._agent_graph`. wrap_run_event_stream fires *after* on_chat_model_start,
    # so this is the only signal we have at this point.
    if getattr(handler, "__name__", "") == "_streaming_handler":
        params["stream"] = True

    return params


def _map_pydantic_tools(request_params: Any) -> list[ToolDefinition]:
    tool_defs = _read(request_params, "function_tools")
    if not isinstance(tool_defs, (list, tuple)):
        return []
    result: list[ToolDefinition] = []
    for tool_def in tool_defs:
        name = _as_string(_read(tool_def, "name"))
        if name == "":
            continue
        result.append(
            ToolDefinition(
                name=name,
                description=_as_string(_read(tool_def, "description")),
                type="function",
                input_schema_json=_schema_to_bytes(_read(tool_def, "parameters_json_schema")),
            )
        )
    return result


def _join_instruction_parts(request_params: Any) -> str:
    parts = _read(request_params, "instruction_parts")
    if not isinstance(parts, (list, tuple)):
        return ""
    contents = [_as_string(_read(part, "content")) for part in parts]
    return "\n\n".join(c for c in contents if c != "")


def _schema_to_bytes(schema: Any) -> bytes:
    if schema is None:
        return b""
    try:
        return json.dumps(schema, default=str, sort_keys=True).encode("utf-8")
    except Exception:
        return b""


def _resolve_pydantic_model_name(request_context: Any) -> str:
    model = _read(request_context, "model")
    for candidate in (
        model if isinstance(model, str) else "",
        _as_string(_read(model, "model_name")),
        _as_string(_read(model, "name")),
        _as_string(_read(model, "model")),
    ):
        if candidate != "":
            return candidate
    return ""


def _run_context_metadata(ctx: Any) -> dict[str, Any]:
    metadata: dict[str, Any] = {}

    deps = _read(ctx, "deps")
    if deps is not None:
        for snake, camel in (
            ("conversation_id", "conversationId"),
            ("session_id", "sessionId"),
            ("thread_id", "threadId"),
        ):
            value = _as_string(_read(deps, snake)) or _as_string(_read(deps, camel))
            if value != "":
                metadata[snake] = value

    ctx_metadata = _read(ctx, "metadata")
    if isinstance(ctx_metadata, dict):
        for key in ("conversation_id", "session_id", "thread_id", "event_id"):
            value = _as_string(ctx_metadata.get(key))
            if value != "" and key not in metadata:
                metadata[key] = value

    # Synthetic fallback when no user-provided IDs were found. Uses pydantic-ai's
    # ctx.run_id so spans for a single agent run share a stable conversation_id.
    if not any(k in metadata for k in ("conversation_id", "session_id", "thread_id")):
        run_id = _as_string(_read(ctx, "run_id"))
        if run_id != "":
            metadata["conversation_id"] = f"sigil:framework:pydantic-ai:{run_id}"

    return metadata


_REQUEST_PART_ROLES: dict[str, str] = {
    "system-prompt": "system",
    "user-prompt": "user",
    "tool-return": "tool",
    "retry-prompt": "user",
}


def _map_pydantic_messages(request_context: Any) -> list[dict[str, Any]]:
    messages_list = _read(request_context, "messages")
    if not isinstance(messages_list, list):
        return []
    result: list[dict[str, Any]] = []
    for message in messages_list:
        parts = _read(message, "parts")
        if not isinstance(parts, (list, tuple)):
            continue
        kind = _as_string(_read(message, "kind"))
        if kind == "request":
            for part in parts:
                role = _REQUEST_PART_ROLES.get(_as_string(_read(part, "part_kind")))
                if role:
                    text = _as_string(_read(part, "content"))
                    if text != "":
                        result.append({"role": role, "content": text})
        elif kind == "response":
            texts: list[str] = []
            tool_calls: list[dict[str, Any]] = []
            for part in parts:
                part_kind = _as_string(_read(part, "part_kind"))
                if part_kind == "text":
                    text = _as_string(_read(part, "content"))
                    if text != "":
                        texts.append(text)
                elif part_kind == "tool-call":
                    tool_calls.append(_tool_call_dict_from_part(part))
            if texts or tool_calls:
                msg: dict[str, Any] = {
                    "role": "assistant",
                    "content": " ".join(texts) if texts else "",
                }
                if tool_calls:
                    msg["tool_calls"] = tool_calls
                result.append(msg)
    return result


def _map_pydantic_response(response: Any, fallback_model_name: str) -> dict[str, Any]:
    llm_output: dict[str, Any] = {}
    model_name = _as_string(_read(response, "model_name")) or fallback_model_name
    if model_name != "":
        llm_output["model_name"] = model_name

    finish_reason = _as_string(_read(response, "finish_reason"))
    if finish_reason != "":
        llm_output["finish_reason"] = finish_reason

    token_usage = _map_pydantic_usage(_read(response, "usage"))
    if token_usage is not None:
        llm_output["token_usage"] = token_usage

    text = _extract_response_text(response)
    tool_calls = _extract_response_tool_calls(response)
    payload: dict[str, Any] = {"llm_output": llm_output}
    if text != "" or tool_calls:
        generation: dict[str, Any] = {"text": text}
        if tool_calls:
            generation["message"] = {"additional_kwargs": {"tool_calls": tool_calls}}
        payload["generations"] = [[generation]]
    return payload


def _map_pydantic_usage(usage: Any) -> TokenUsage | None:
    if usage is None:
        return None
    input_tokens = _int_or_none(_read(usage, "input_tokens"))
    output_tokens = _int_or_none(_read(usage, "output_tokens"))
    cache_read = _int_or_none(_read(usage, "cache_read_tokens"))
    cache_write = _int_or_none(_read(usage, "cache_write_tokens"))
    if input_tokens is None and output_tokens is None and cache_read is None and cache_write is None:
        return None
    return TokenUsage(
        input_tokens=input_tokens or 0,
        output_tokens=output_tokens or 0,
        cache_read_input_tokens=cache_read or 0,
        cache_write_input_tokens=cache_write or 0,
    ).normalize()


def _extract_response_text(response: Any) -> str:
    parts = _read(response, "parts")
    if not isinstance(parts, (list, tuple)):
        return ""
    texts: list[str] = []
    for part in parts:
        part_kind = _as_string(_read(part, "part_kind"))
        if part_kind == "text":
            text = _as_string(_read(part, "content"))
            if text != "":
                texts.append(text)
    return " ".join(texts).strip()


def _extract_response_tool_calls(response: Any) -> list[dict[str, Any]]:
    parts = _read(response, "parts")
    if not isinstance(parts, (list, tuple)):
        return []
    return [_tool_call_dict_from_part(part) for part in parts if _as_string(_read(part, "part_kind")) == "tool-call"]


def _tool_call_dict_from_part(part: Any) -> dict[str, Any]:
    call_info: dict[str, Any] = {
        "function": {
            "name": _as_string(_read(part, "tool_name")),
            "arguments": _json_string(_read(part, "args")),
        },
    }
    call_id = _as_string(_read(part, "tool_call_id"))
    if call_id != "":
        call_info["id"] = call_id
    return call_info


def _extract_stream_token(event: Any) -> str:
    event_kind = _as_string(_read(event, "event_kind"))
    if event_kind == "part_delta":
        delta = _read(event, "delta")
        return _as_string(_read(delta, "content_delta"))
    return ""


def _tool_name(call: Any, tool_def: Any) -> str:
    for candidate in (
        _as_string(_read(call, "tool_name")),
        _as_string(_read(tool_def, "name")),
    ):
        if candidate != "":
            return candidate
    return "framework_tool"


def _json_string(value: Any) -> str:
    if isinstance(value, str):
        return value
    try:
        return json.dumps(value, default=str, sort_keys=True)
    except Exception:
        return str(value)


def _read(value: Any, key: str) -> Any:
    if value is None:
        return None
    if isinstance(value, dict):
        return value.get(key)
    return getattr(value, key, None)


def _as_string(value: Any) -> str:
    if isinstance(value, str):
        return value.strip()
    return "" if value is None else str(value).strip()


def _int_or_none(value: Any) -> int | None:
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        integer = int(value)
        return integer if float(integer) == value else None
    if isinstance(value, str):
        stripped = value.strip()
        if stripped == "":
            return None
        try:
            return int(stripped)
        except ValueError:
            return None
    return None
