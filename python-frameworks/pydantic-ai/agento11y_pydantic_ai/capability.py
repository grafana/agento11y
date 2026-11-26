"""Pydantic AI capability that records agent runs, model requests and tool calls through an agento11y handler."""

from __future__ import annotations

import json
import logging
from collections.abc import AsyncIterable, AsyncIterator, Iterator
from contextlib import contextmanager
from typing import Any
from uuid import UUID, uuid4

from agento11y import Client, ToolDefinition
from pydantic_ai.capabilities import AbstractCapability
from pydantic_ai.capabilities.abstract import (
    ValidatedToolArgs,
    WrapModelRequestHandler,
    WrapRunHandler,
    WrapToolExecuteHandler,
)
from pydantic_ai.exceptions import (
    ApprovalRequired,
    CallDeferred,
    ModelRetry,
    SkipModelRequest,
    SkipToolExecution,
    SkipToolValidation,
    ToolRetryError,
)
from pydantic_ai.messages import AgentStreamEvent, ModelResponse, ToolCallPart
from pydantic_ai.models import ModelRequestContext, ModelRequestParameters
from pydantic_ai.run import AgentRunResult
from pydantic_ai.settings import ThinkingLevel
from pydantic_ai.tools import RunContext
from pydantic_ai.tools import ToolDefinition as PydanticToolDefinition
from pydantic_ai.usage import RequestUsage

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

_DEFAULT_RUN_NAME = "pydantic_ai_agent"
_V1_REASONING_DETAILS = ("reasoning_tokens", "thoughts_tokens")
_CONVERSATION_KEYS = ("conversation_id", "session_id", "thread_id")
_DEPS_CONVERSATION_ATTRIBUTES = (
    ("conversation_id", "conversationId"),
    ("session_id", "sessionId"),
    ("thread_id", "threadId"),
)
# Every key reaches a callable `provider_resolver`; the handler records the first four.
_FORWARDED_MODEL_SETTINGS = (
    "temperature",
    "max_tokens",
    "top_p",
    "tool_choice",
    "seed",
    "presence_penalty",
    "frequency_penalty",
)


def create_agento11y_pydantic_ai_handler(*, client: Client, **handler_kwargs: Any) -> Agento11yPydanticAIHandler:
    """Create a Pydantic AI agento11y handler."""
    return Agento11yPydanticAIHandler(client=client, **handler_kwargs)


class Agento11yPydanticAICapability(AbstractCapability[Any]):
    """Records each agent run, model request and tool call of a Pydantic AI agent through an agento11y handler.

    A failure to start a recording propagates and fails the run; a failure to finish one is logged.
    """

    def __init__(self, agento11y_handler: Agento11yPydanticAIHandler) -> None:
        self._agento11y_handler = agento11y_handler
        self._agent_run_id: UUID | None = None
        # Stream events name no request, so each goes to the request opened last.
        self._open_model_request_run_ids: list[UUID] = []

    async def for_run(self, ctx: RunContext[Any]) -> Agento11yPydanticAICapability:
        """Return a capability with its own run state, sharing this one's handler."""
        return Agento11yPydanticAICapability(self._agento11y_handler)

    @property
    def has_wrap_run_event_stream(self) -> bool:
        # Pydantic AI calls wrap_run_event_stream on every run that streams; reporting the
        # override would also make run() and run_sync() stream every model request.
        return False

    async def wrap_run(self, ctx: RunContext[Any], *, handler: WrapRunHandler) -> AgentRunResult[Any]:
        run_id = uuid4()
        run_name = _run_name(ctx)
        self._agento11y_handler.on_chain_start(
            {"name": run_name},
            {},
            run_id=run_id,
            parent_run_id=None,
            metadata=_conversation_metadata(ctx),
            run_type="agent",
            run_name=run_name,
        )

        self._agent_run_id = run_id
        try:
            result = await handler()
        except _PYDANTIC_CONTROL_FLOW:
            with _logging_failure_to_record("agent run discard"):
                self._agento11y_handler._discard_chain_run(run_id=run_id)
            raise
        except BaseException as exc:
            with _logging_failure_to_record("agent run error"):
                self._agento11y_handler.on_chain_error(exc, run_id=run_id)
            raise
        finally:
            self._agent_run_id = None

        with _logging_failure_to_record("agent run end"):
            self._agento11y_handler.on_chain_end({"status": "completed"}, run_id=run_id)
        return result

    async def wrap_model_request(
        self,
        ctx: RunContext[Any],
        *,
        request_context: ModelRequestContext,
        handler: WrapModelRequestHandler,
    ) -> ModelResponse:
        run_id = uuid4()
        run_name = _run_name(ctx)
        model_name = request_context.model.model_name
        messages, system_prompts = map_request_messages(request_context.messages)
        self._agento11y_handler.on_chat_model_start(
            {"name": run_name, "kwargs": {"model": model_name}},
            [messages],
            run_id=run_id,
            parent_run_id=self._agent_run_id,
            invocation_params=_invocation_params(
                request_context, system_prompts, streamed=_is_streamed(request_context, handler)
            ),
            metadata=_conversation_metadata(ctx),
            run_name=run_name,
        )

        self._open_model_request_run_ids.append(run_id)
        try:
            response = await handler(request_context)
        except _PYDANTIC_CONTROL_FLOW:
            with _logging_failure_to_record("model request discard"):
                self._agento11y_handler._discard_llm_run(run_id=run_id)
            raise
        except BaseException as exc:
            with _logging_failure_to_record("model request error"):
                self._agento11y_handler.on_llm_error(exc, run_id=run_id)
            raise
        finally:
            self._open_model_request_run_ids.remove(run_id)

        with _logging_failure_to_record("model response"):
            self._agento11y_handler.on_llm_end(_llm_result(response, model_name), run_id=run_id)
        return response

    async def on_model_request_error(
        self, ctx: RunContext[Any], *, request_context: ModelRequestContext, error: Exception
    ) -> ModelResponse:
        # Pydantic AI 2.x opens a streamed request inside wrap_model_request only when a capability
        # overrides this hook. Without the override a stream that fails to open reaches
        # wrap_model_request as a cancellation, and the generation loses the provider's error.
        raise error

    async def wrap_tool_execute(
        self,
        ctx: RunContext[Any],
        *,
        call: ToolCallPart,
        tool_def: PydanticToolDefinition,
        args: ValidatedToolArgs,
        handler: WrapToolExecuteHandler,
    ) -> Any:
        run_id = uuid4()
        self._agento11y_handler.on_tool_start(
            {"name": call.tool_name, "description": (tool_def.description or "").strip()},
            "",
            run_id=run_id,
            # Tools run after their model request has ended, so the agent run is the parent.
            parent_run_id=self._agent_run_id,
            metadata=_conversation_metadata(ctx),
            run_name=call.tool_name,
            inputs=args,
            tool_call_id=call.tool_call_id,
        )

        try:
            result = await handler(args)
        except _PYDANTIC_CONTROL_FLOW:
            with _logging_failure_to_record("tool discard"):
                self._agento11y_handler._discard_tool_run(run_id=run_id)
            raise
        except BaseException as exc:
            with _logging_failure_to_record("tool error"):
                self._agento11y_handler.on_tool_error(exc, run_id=run_id)
            raise

        with _logging_failure_to_record("tool result"):
            self._agento11y_handler.on_tool_end(result, run_id=run_id)
        return result

    async def wrap_run_event_stream(
        self, ctx: RunContext[Any], *, stream: AsyncIterable[AgentStreamEvent]
    ) -> AsyncIterator[AgentStreamEvent]:
        async for event in stream:
            token = stream_token(event)
            if token and self._open_model_request_run_ids:
                with _logging_failure_to_record("stream token"):
                    self._agento11y_handler.on_llm_new_token(token, run_id=self._open_model_request_run_ids[-1])
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


@contextmanager
def _logging_failure_to_record(what: str) -> Iterator[None]:
    # Recording runs beside the agent: once an operation has its outcome, losing its record must not change it.
    try:
        yield
    except Exception:
        logger.warning("agento11y: %s not recorded", what, exc_info=True)


def _run_name(ctx: RunContext[Any]) -> str:
    agent_name = (ctx.agent.name or "").strip() if ctx.agent is not None else ""
    return agent_name or ctx.model.model_name.strip() or _DEFAULT_RUN_NAME


def _conversation_metadata(ctx: RunContext[Any]) -> dict[str, str]:
    """The ids that place a run in a conversation: the caller's own from deps, then run metadata, else the run's."""
    metadata: dict[str, str] = {}

    if ctx.deps is not None:
        for snake, camel in _DEPS_CONVERSATION_ATTRIBUTES:
            value = _identifier(_read(ctx.deps, snake)) or _identifier(_read(ctx.deps, camel))
            if value:
                metadata[snake] = value

    if isinstance(ctx.metadata, dict):
        for key in (*_CONVERSATION_KEYS, "event_id"):
            value = _identifier(ctx.metadata.get(key))
            if value:
                metadata.setdefault(key, value)

    if not any(key in metadata for key in _CONVERSATION_KEYS):
        # A run has a conversation id from Pydantic AI 1.107 at the latest; 1.86 has only the run's own id.
        conversation_id = _identifier(getattr(ctx, "conversation_id", None))
        run_id = _identifier(ctx.run_id)
        if conversation_id:
            metadata["conversation_id"] = conversation_id
        elif run_id:
            metadata["conversation_id"] = f"agento11y:framework:pydantic-ai:{run_id}"

    return metadata


def _is_streamed(request_context: ModelRequestContext, handler: WrapModelRequestHandler) -> bool:
    streaming = getattr(request_context, "streaming", None)
    if streaming is not None:
        return streaming
    # Pydantic AI 1.x has no such field. Its only sign of a streamed request, before the first
    # stream event, is the `pydantic_ai._agent_graph` closure that serves it.
    return getattr(handler, "__name__", "") == "_streaming_handler"


def _invocation_params(
    request_context: ModelRequestContext, system_prompts: list[str], *, streamed: bool
) -> dict[str, Any]:
    settings = request_context.model_settings or {}
    parameters = request_context.model_request_parameters
    params: dict[str, Any] = {"model": request_context.model.model_name}
    for key in _FORWARDED_MODEL_SETTINGS:
        value = settings.get(key)
        if value is not None:
            params[key] = value
    params.update(_thinking_params(settings.get("thinking")))

    tools = _offered_tools(parameters)
    if tools:
        params["tools"] = tools

    system_prompt = _system_prompt(system_prompts, parameters)
    if system_prompt:
        params["system_prompt"] = system_prompt

    if streamed:
        params["stream"] = True

    return params


def _thinking_params(thinking: ThinkingLevel | None) -> dict[str, Any]:
    if thinking is None:
        return {}
    if isinstance(thinking, bool):
        return {"thinking_enabled": thinking}
    return {"thinking_enabled": True, "thinking_level": thinking}


def _offered_tools(parameters: ModelRequestParameters) -> list[ToolDefinition]:
    # The tools the model is offered, as pydantic-ai's own instrumentation records them: output tools ride
    # beside function tools, and a tool deferred loading withholds from the request (pydantic-ai 2.x) is not one.
    visibility_of = getattr(parameters, "visibility_of", None)
    return [
        ToolDefinition(
            name=tool_def.name,
            description=(tool_def.description or "").strip(),
            type="function",
            input_schema_json=_schema_to_bytes(tool_def.parameters_json_schema),
        )
        for tool_def in (*parameters.function_tools, *parameters.output_tools)
        if visibility_of is None or visibility_of(tool_def.name) != "withheld"
    ]


def _system_prompt(system_prompts: list[str], parameters: ModelRequestParameters) -> str:
    # Pydantic AI's models send system prompt parts ahead of instructions.
    instructions = [part.content.strip() for part in parameters.instruction_parts or ()]
    return "\n\n".join(text for text in (*system_prompts, *instructions) if text)


def _schema_to_bytes(schema: dict[str, Any]) -> bytes:
    try:
        return json.dumps(schema, default=str, sort_keys=True).encode("utf-8")
    except (TypeError, ValueError):
        return b""


def _llm_result(response: ModelResponse, request_model_name: str) -> dict[str, Any]:
    llm_output: dict[str, Any] = {
        "model_name": response.model_name or request_model_name,
        "token_usage": _token_usage(response.usage),
    }
    if response.finish_reason:
        llm_output["finish_reason"] = response.finish_reason
    if response.provider_response_id:
        llm_output["response_id"] = response.provider_response_id
    return {
        "llm_output": llm_output,
        "generations": [[{"message": message} for message in map_response_message(response)]],
    }


def _token_usage(usage: RequestUsage) -> dict[str, Any]:
    return {
        "input_tokens": usage.input_tokens,
        "output_tokens": usage.output_tokens,
        "cache_read_input_tokens": usage.cache_read_tokens,
        "cache_write_input_tokens": usage.cache_write_tokens,
        "output_token_details": {"reasoning": _reasoning_tokens(usage)},
    }


def _reasoning_tokens(usage: RequestUsage) -> int:
    # Pydantic AI 2.x has a field for reasoning tokens. 1.x leaves the count in `details`, under
    # the key its model class writes: OpenAI and xAI, then Google and Gemini.
    reported = getattr(usage, "output_reasoning_tokens", 0)
    return reported or next((usage.details[key] for key in _V1_REASONING_DETAILS if usage.details.get(key)), 0)


def _read(value: Any, key: str) -> Any:
    """Read a key of the caller's deps, which may be a mapping or any object."""
    if isinstance(value, dict):
        return value.get(key)
    return getattr(value, key, None)


def _identifier(value: Any) -> str:
    return "" if value is None else str(value).strip()
