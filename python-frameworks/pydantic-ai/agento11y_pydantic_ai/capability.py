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

from ._messages import map_request_messages, map_response_message, stream_token
from .handler import Agento11yPydanticAIHandler

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


def create_agento11y_pydantic_ai_handler(*, client: Client, **handler_kwargs: Any) -> Agento11yPydanticAIHandler:
    """Create a Pydantic AI agento11y handler."""
    return Agento11yPydanticAIHandler(client=client, **handler_kwargs)


class Agento11yPydanticAICapability(AbstractCapability):
    """Pydantic AI capability bridge that maps lifecycle hooks to agento11y handlers."""

    def __init__(self, agento11y_handler: Agento11yPydanticAIHandler) -> None:
        self._agento11y_handler = agento11y_handler
        self._run_stack: list[UUID] = []
        self._llm_run_ids: list[UUID] = []

    async def for_run(self, ctx: Any) -> Agento11yPydanticAICapability:
        """Per-run isolated state — each agent run gets its own stacks."""
        return Agento11yPydanticAICapability(self._agento11y_handler)

    @property
    def has_wrap_run_event_stream(self) -> bool:
        # Pydantic AI calls wrap_run_event_stream on every run that streams; reporting the
        # override would also make run() and run_sync() stream every model request.
        return False

    async def wrap_run(self, ctx: Any, *, handler: Any) -> Any:
        run_id = uuid4()
        self._run_stack.append(run_id)

        run_name = _agent_name(ctx)
        metadata = _run_context_metadata(ctx)
        try:
            self._agento11y_handler.on_chain_start(
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
                self._agento11y_handler._discard_chain_run(run_id=run_id)
            else:
                try:
                    self._agento11y_handler.on_chain_error(exc, run_id=run_id)
                except Exception:
                    logger.debug("agento11y: failed to record chain error", exc_info=True)
            raise

        _remove(self._run_stack, run_id)
        try:
            self._agento11y_handler.on_chain_end({"status": "completed"}, run_id=run_id)
        except Exception:
            logger.debug("agento11y: failed to record chain end", exc_info=True)
        return result

    async def wrap_model_request(self, ctx: Any, *, request_context: Any, handler: Any) -> Any:
        run_id = uuid4()
        self._llm_run_ids.append(run_id)

        parent_run_id = self._run_stack[-1] if self._run_stack else None

        model_name = _resolve_pydantic_model_name(request_context)
        messages, system_prompts = map_request_messages(request_context.messages)
        invocation_params = _build_invocation_params(model_name, request_context, handler, system_prompts)
        serialized: dict[str, Any] = {"name": _agent_name(ctx)}
        if model_name != "":
            serialized["kwargs"] = {"model": model_name}

        metadata = _run_context_metadata(ctx)
        try:
            self._agento11y_handler.on_chat_model_start(
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
                self._agento11y_handler._discard_llm_run(run_id=run_id)
            else:
                try:
                    self._agento11y_handler.on_llm_error(exc, run_id=run_id)
                except Exception:
                    logger.debug("agento11y: failed to record LLM error", exc_info=True)
            raise

        _remove(self._llm_run_ids, run_id)
        try:
            self._agento11y_handler.on_llm_end(_map_pydantic_response(response, model_name), run_id=run_id)
        except Exception:
            logger.debug("agento11y: failed to record LLM end", exc_info=True)
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
        self._agento11y_handler.on_tool_start(
            {"name": tool_name, "description": _as_string(_read(tool_def, "description"))},
            "",
            run_id=run_id,
            parent_run_id=parent_run_id,
            metadata=metadata,
            run_name=tool_name,
            inputs=args,
            tool_call_id=_as_string(_read(call, "tool_call_id")),
        )

        try:
            result = await handler(args)
        except BaseException as exc:
            if isinstance(exc, _PYDANTIC_CONTROL_FLOW):
                self._agento11y_handler._discard_tool_run(run_id=run_id)
            else:
                try:
                    self._agento11y_handler.on_tool_error(exc, run_id=run_id)
                except Exception:
                    logger.debug("agento11y: failed to record tool error", exc_info=True)
            raise

        try:
            self._agento11y_handler.on_tool_end(result, run_id=run_id)
        except Exception:
            logger.debug("agento11y: failed to record tool end", exc_info=True)
        return result

    async def on_tool_execute_error(self, ctx: Any, *, call: Any, tool_def: Any, args: Any, error: Any) -> Any:
        # wrap_tool_execute already handles cleanup and error reporting;
        # this hook fires after the wrapper re-raises, so just propagate.
        raise error

    async def wrap_run_event_stream(self, ctx: Any, *, stream: Any) -> Any:
        async for event in stream:
            token = stream_token(event)
            if token and self._llm_run_ids:
                try:
                    self._agento11y_handler.on_llm_new_token(token, run_id=self._llm_run_ids[-1])
                except Exception:
                    logger.debug("agento11y: failed to record stream token", exc_info=True)
            yield event


def create_agento11y_pydantic_ai_capability(*, client: Client, **handler_kwargs: Any) -> Agento11yPydanticAICapability:
    """Create a Pydantic AI capability wired to agento11y instrumentation."""
    return Agento11yPydanticAICapability(create_agento11y_pydantic_ai_handler(client=client, **handler_kwargs))


def with_agento11y_pydantic_ai_capability(
    capabilities: list[Any] | None,
    *,
    client: Client,
    **handler_kwargs: Any,
) -> list[Any]:
    """Add an agento11y capability to a list, with double-injection guard."""
    result = list(capabilities or [])
    if any(isinstance(cap, Agento11yPydanticAICapability) for cap in result):
        return result
    result.append(create_agento11y_pydantic_ai_capability(client=client, **handler_kwargs))
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


def _build_invocation_params(
    model_name: str, request_context: Any, handler: Any, system_prompts: list[str]
) -> dict[str, Any]:
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

    system_prompt = _system_prompt(system_prompts, request_params)
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


def _system_prompt(system_prompts: list[str], request_params: Any) -> str:
    # Pydantic AI's models send system prompt parts ahead of instructions.
    parts = _read(request_params, "instruction_parts")
    instructions = [_as_string(_read(part, "content")) for part in parts] if isinstance(parts, (list, tuple)) else []
    return "\n\n".join(text for text in [*system_prompts, *instructions] if text != "")


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
            metadata["conversation_id"] = f"agento11y:framework:pydantic-ai:{run_id}"

    return metadata


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

    return {
        "llm_output": llm_output,
        "generations": [[{"message": message} for message in map_response_message(response)]],
    }


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


def _tool_name(call: Any, tool_def: Any) -> str:
    for candidate in (
        _as_string(_read(call, "tool_name")),
        _as_string(_read(tool_def, "name")),
    ):
        if candidate != "":
            return candidate
    return "framework_tool"


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
