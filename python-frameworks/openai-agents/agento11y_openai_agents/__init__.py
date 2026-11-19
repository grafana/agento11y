"""Public exports for agento11y OpenAI Agents callback handlers."""

from __future__ import annotations

import contextvars
import inspect
import json
from typing import Any
from uuid import UUID, uuid4

from agento11y import Client, Message, MessageRole, Part, PartKind, ToolCall, ToolDefinition, ToolResult

from .handler import Agento11yAsyncOpenAIAgentsHandler, Agento11yOpenAIAgentsHandler

try:  # pragma: no cover - imported from openai-agents at runtime
    from agents import RunHooks
except Exception:  # pragma: no cover - lightweight fallback for local unit tests

    class RunHooks:  # type: ignore[no-redef]
        """Fallback RunHooks shape used when openai-agents isn't installed."""

        async def on_agent_start(self, context: Any, agent: Any) -> None:
            return

        async def on_agent_end(self, context: Any, agent: Any, output: Any) -> None:
            return

        async def on_handoff(self, context: Any, from_agent: Any, to_agent: Any) -> None:
            return

        async def on_llm_start(
            self,
            context: Any,
            agent: Any,
            system_prompt: str | None,
            input_items: list[Any],
        ) -> None:
            return

        async def on_llm_end(self, context: Any, agent: Any, response: Any) -> None:
            return

        async def on_tool_start(self, context: Any, agent: Any, tool: Any) -> None:
            return

        async def on_tool_end(self, context: Any, agent: Any, tool: Any, result: Any) -> None:
            return


def create_agento11y_openai_agents_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> Agento11yOpenAIAgentsHandler | Agento11yAsyncOpenAIAgentsHandler:
    """Create an OpenAI Agents agento11y handler for sync or async flows."""
    if async_handler:
        return Agento11yAsyncOpenAIAgentsHandler(client=client, **handler_kwargs)
    return Agento11yOpenAIAgentsHandler(client=client, **handler_kwargs)


class Agento11yOpenAIAgentsRunHooks(RunHooks):
    """RunHooks bridge that maps OpenAI Agents lifecycle callbacks into agento11y handlers."""

    def __init__(
        self,
        *,
        agento11y_handler: Agento11yOpenAIAgentsHandler | Agento11yAsyncOpenAIAgentsHandler,
        delegate: Any | None = None,
        run_config: Any | None = None,
    ) -> None:
        self._agento11y_handler = agento11y_handler
        self._delegate = delegate
        self._run_config = run_config
        self._agent_run_stacks: dict[int, list[tuple[int, UUID]]] = {}
        self._llm_run_stacks: dict[tuple[int, int], list[UUID]] = {}
        self._tool_run_stacks: dict[tuple[int, int, str | None], list[UUID]] = {}
        self._agent_runs_by_usage: dict[int, list[UUID]] = {}
        self._tool_runs_by_usage: dict[tuple[int, int], dict[UUID, bool]] = {}
        # Run context keys of Runner.run_streamed runs, mapped to the LLM call
        # currently streaming in that run (None between calls).
        self._streamed_llm_runs: dict[int, UUID | None] = {}
        _install_run_streamed_patch()

    def _effective_run_config(self) -> Any | None:
        """RunConfig of the run in progress, else the one given when the hooks were created."""
        current = _current_run_config.get()
        return current if current is not None else self._run_config

    def _register_streamed_result(self, result: Any) -> None:
        """Mark a RunResultStreaming's run as streamed and observe its raw model deltas."""
        context = getattr(result, "context_wrapper", None)
        queue = getattr(result, "_event_queue", None)
        put_nowait = getattr(queue, "put_nowait", None)
        if context is None or put_nowait is None:
            return
        context_key = _run_context_key(context)
        self._streamed_llm_runs[context_key] = None

        def observed_put_nowait(item: Any) -> None:
            put_nowait(item)
            if _read(item, "type") == "raw_response_event":
                self._on_raw_stream_event(context_key, _read(item, "data"))
            elif type(item).__name__ == "QueueCompleteSentinel":
                self._streamed_llm_runs.pop(context_key, None)

        queue.put_nowait = observed_put_nowait

    def _on_raw_stream_event(self, context_key: int, event: Any) -> None:
        run_id = self._streamed_llm_runs.get(context_key)
        event_type = _as_string(_read(event, "type"))
        delta = _read(event, "delta")
        if run_id is None or not event_type.endswith(".delta") or not isinstance(delta, str) or delta == "":
            return
        # Both handler flavors share the synchronous base implementation.
        self._agento11y_handler._on_llm_new_token(token=delta, run_id=run_id)

    async def on_agent_start(self, context: Any, agent: Any) -> None:
        await _invoke_delegate(self._delegate, "on_agent_start", context, agent)

        context_key = _run_context_key(context)
        stack = self._agent_run_stacks.setdefault(context_key, [])
        usage_key = _usage_context_key(context)
        usage_runs = self._agent_runs_by_usage.setdefault(usage_key, [])
        run_id = uuid4()
        parent_run_id = stack[-1][1] if stack else None
        if not stack:
            # Nested agent tools have their own run key but share outer Usage.
            # AgentHookContext loses the invoking tool's call ID, so claim the
            # oldest unclaimed active agent tool targeting this agent only.
            # Without one, use the oldest active agent in the shared usage group.
            tools = self._tool_runs_by_usage.get((usage_key, id(agent)), {})
            for tool_run_id, claimed in tools.items():
                if not claimed:
                    parent_run_id = tool_run_id
                    tools[tool_run_id] = True
                    break
            else:
                parent_run_id = usage_runs[0] if usage_runs else None
        stack.append((id(agent), run_id))
        usage_runs.append(run_id)

        agent_name = _agent_name(agent)
        await _invoke_handler(
            self._agento11y_handler,
            "on_chain_start",
            {"name": agent_name},
            {},
            run_id=run_id,
            parent_run_id=parent_run_id,
            metadata=_context_metadata(context),
            run_type="agent",
            run_name=agent_name,
        )

    async def on_agent_end(self, context: Any, agent: Any, output: Any) -> None:
        context_key = _run_context_key(context)
        stack = self._agent_run_stacks.get(context_key, [])
        # The SDK creates a fresh AgentHookContext for the end callback.
        run_id = None
        for index in range(len(stack) - 1, -1, -1):
            if stack[index][0] == id(agent):
                _, run_id = stack.pop(index)
                break
        if run_id is not None:
            usage_key = _usage_context_key(context)
            usage_runs = self._agent_runs_by_usage.get(usage_key, [])
            if run_id in usage_runs:
                usage_runs.remove(run_id)
            if not usage_runs:
                self._agent_runs_by_usage.pop(usage_key, None)
            await _invoke_handler(self._agento11y_handler, "on_chain_end", {"output": output}, run_id=run_id)
        if not stack:
            self._agent_run_stacks.pop(context_key, None)

        await _invoke_delegate(self._delegate, "on_agent_end", context, agent, output)

    async def on_handoff(self, context: Any, from_agent: Any, to_agent: Any) -> None:
        run_id = uuid4()
        parent_run_id = self._agent_run_id(context, from_agent)
        serialized = {
            "name": "agent_handoff",
            "source": _agent_name(from_agent),
            "destination": _agent_name(to_agent),
        }
        await _invoke_handler(
            self._agento11y_handler,
            "on_chain_start",
            serialized,
            {},
            run_id=run_id,
            parent_run_id=parent_run_id,
            metadata=_context_metadata(context),
            run_type="handoff",
            run_name="agent_handoff",
        )
        await _invoke_handler(self._agento11y_handler, "on_chain_end", {"handoff": True}, run_id=run_id)

        await _invoke_delegate(self._delegate, "on_handoff", context, from_agent, to_agent)

    async def on_llm_start(
        self,
        context: Any,
        agent: Any,
        system_prompt: str | None,
        input_items: list[Any],
    ) -> None:
        await _invoke_delegate(self._delegate, "on_llm_start", context, agent, system_prompt, input_items)

        context_key = _run_context_key(context)
        llm_key = (context_key, id(agent))
        llm_stack = self._llm_run_stacks.setdefault(llm_key, [])
        run_id = uuid4()
        llm_stack.append(run_id)

        parent_run_id = self._agent_run_id(context, agent)

        model_provider, model_name = _model_ref(agent, self._effective_run_config())
        invocation_params: dict[str, Any] = {}
        if model_name != "":
            invocation_params["model"] = model_name
        if model_provider != "":
            invocation_params["provider"] = model_provider
        tools = _tool_definitions(agent)
        if tools:
            invocation_params["tools"] = tools
        if context_key in self._streamed_llm_runs:
            invocation_params["stream"] = True
            self._streamed_llm_runs[context_key] = run_id
        serialized = {"name": _agent_name(agent)}
        if model_name != "":
            serialized["kwargs"] = {"model": model_name}

        await _invoke_handler(
            self._agento11y_handler,
            "on_chat_model_start",
            serialized,
            [_input_messages(system_prompt, input_items)],
            run_id=run_id,
            parent_run_id=parent_run_id,
            invocation_params=invocation_params,
            metadata=_context_metadata(context),
            run_name=_agent_name(agent),
        )

    async def on_llm_end(self, context: Any, agent: Any, response: Any) -> None:
        llm_key = (_run_context_key(context), id(agent))
        llm_stack = self._llm_run_stacks.get(llm_key, [])
        run_id = llm_stack.pop() if llm_stack else None
        if run_id is not None and self._streamed_llm_runs.get(llm_key[0]) == run_id:
            self._streamed_llm_runs[llm_key[0]] = None
        if run_id is not None:
            await _invoke_handler(
                self._agento11y_handler,
                "on_llm_end",
                _build_llm_end_response(response, model_name=_model_ref(agent, self._effective_run_config())[1]),
                run_id=run_id,
            )
        if not llm_stack:
            self._llm_run_stacks.pop(llm_key, None)

        await _invoke_delegate(self._delegate, "on_llm_end", context, agent, response)

    async def on_tool_start(self, context: Any, agent: Any, tool: Any) -> None:
        await _invoke_delegate(self._delegate, "on_tool_start", context, agent, tool)

        context_key = _run_context_key(context)
        tool_key = _tool_context_key(context, tool)
        tool_stack = self._tool_run_stacks.setdefault(tool_key, [])
        run_id = uuid4()
        tool_stack.append(run_id)
        agent_instance = getattr(tool, "_agent_instance", None)
        if getattr(tool, "_is_agent_tool", False) and agent_instance is not None:
            agent_tool_key = (_usage_context_key(context), id(agent_instance))
            self._tool_runs_by_usage.setdefault(agent_tool_key, {})[run_id] = False

        llm_stack = self._llm_run_stacks.get((context_key, id(agent)), [])
        if llm_stack:
            parent_run_id = llm_stack[-1]
        else:
            parent_run_id = self._agent_run_id(context, agent)

        tool_name = _tool_name(tool)
        tool_input = _read(context, "tool_input")
        await _invoke_handler(
            self._agento11y_handler,
            "on_tool_start",
            {"name": tool_name},
            _tool_input_string(tool_input),
            run_id=run_id,
            parent_run_id=parent_run_id,
            metadata=_context_metadata(context),
            run_name=tool_name,
            inputs=tool_input,
            tool_call_id=_as_string(_read(context, "tool_call_id")),
        )

    async def on_tool_end(self, context: Any, agent: Any, tool: Any, result: Any) -> None:
        tool_key = _tool_context_key(context, tool)
        tool_stack = self._tool_run_stacks.get(tool_key, [])
        run_id = tool_stack.pop() if tool_stack else None
        if run_id is not None:
            agent_tool_key = (_usage_context_key(context), id(getattr(tool, "_agent_instance", None)))
            tools = self._tool_runs_by_usage.get(agent_tool_key, {})
            tools.pop(run_id, None)
            if not tools:
                self._tool_runs_by_usage.pop(agent_tool_key, None)
            await _invoke_handler(self._agento11y_handler, "on_tool_end", result, run_id=run_id)
        if not tool_stack:
            self._tool_run_stacks.pop(tool_key, None)

        await _invoke_delegate(self._delegate, "on_tool_end", context, agent, tool, result)

    def _agent_run_id(self, context: Any, agent: Any) -> UUID | None:
        for agent_id, run_id in reversed(self._agent_run_stacks.get(_run_context_key(context), [])):
            if agent_id == id(agent):
                return run_id
        return None


def create_agento11y_openai_agents_hooks(
    *,
    client: Client,
    existing_hooks: Any | None = None,
    run_config: Any | None = None,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> Agento11yOpenAIAgentsRunHooks:
    """Create a RunHooks instance wired to agento11y instrumentation.

    Pass the run's `RunConfig` so agents without their own `model` record the model it selects.
    """
    agento11y_handler = create_agento11y_openai_agents_handler(
        client=client,
        async_handler=async_handler,
        **handler_kwargs,
    )
    return Agento11yOpenAIAgentsRunHooks(
        agento11y_handler=agento11y_handler,
        delegate=existing_hooks,
        run_config=run_config,
    )


def with_agento11y_openai_agents_hooks(
    run_options: dict[str, Any] | None,
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> dict[str, Any]:
    """Set OpenAI Agents `hooks` to a valid RunHooks bridge with agento11y instrumentation."""
    merged = dict(run_options or {})
    existing = merged.get("hooks")
    if isinstance(existing, list):
        raise TypeError("openai-agents expects `hooks` to be a RunHooks instance, not a list.")

    if isinstance(existing, Agento11yOpenAIAgentsRunHooks):
        return merged
    if isinstance(existing, (Agento11yOpenAIAgentsHandler, Agento11yAsyncOpenAIAgentsHandler)):
        existing = None

    merged["hooks"] = create_agento11y_openai_agents_hooks(
        client=client,
        existing_hooks=existing,
        run_config=merged.get("run_config"),
        async_handler=async_handler,
        **handler_kwargs,
    )
    return merged


# RunConfig passed to the Runner call in progress. RunHooks callbacks don't receive it,
# so the runner patch below records it for the duration of each run.
_current_run_config: contextvars.ContextVar[Any | None] = contextvars.ContextVar(
    "agento11y_openai_agents_run_config", default=None
)

_run_streamed_patched = False


def _install_run_streamed_patch() -> None:
    """Let our RunHooks see each run's RunConfig and tell Runner.run_streamed runs apart.

    RunHooks callbacks receive neither. Runner.run, run_sync and run_streamed are wrapped to
    expose `run_config` through a context variable for that run (tasks the runner starts copy
    it), and run_streamed results are registered with our hooks to record STREAM and TTFT.
    """
    global _run_streamed_patched
    if _run_streamed_patched:
        return
    _run_streamed_patched = True
    try:
        from agents.run import AgentRunner
    except Exception:  # pragma: no cover - openai-agents missing or reshaped
        return

    def bound_arguments(original: Any, self: Any, args: tuple[Any, ...], kwargs: dict[str, Any]) -> dict[str, Any]:
        try:
            return dict(inspect.signature(original).bind(self, *args, **kwargs).arguments)
        except (TypeError, ValueError):  # pragma: no cover - original validates the call itself
            return dict(kwargs)

    def option(arguments: dict[str, Any], name: str) -> Any:
        if name in arguments:
            return arguments[name]
        extra = arguments.get("kwargs")
        return extra.get(name) if isinstance(extra, dict) else None

    original_run = getattr(AgentRunner, "run", None)
    if original_run is not None:

        async def run(self: Any, *args: Any, **kwargs: Any) -> Any:
            token = _current_run_config.set(option(bound_arguments(original_run, self, args, kwargs), "run_config"))
            try:
                return await original_run(self, *args, **kwargs)
            finally:
                _current_run_config.reset(token)

        run.__wrapped__ = original_run  # type: ignore[attr-defined]
        AgentRunner.run = run  # type: ignore[method-assign]

    original_run_sync = getattr(AgentRunner, "run_sync", None)
    if original_run_sync is not None:

        def run_sync(self: Any, *args: Any, **kwargs: Any) -> Any:
            arguments = bound_arguments(original_run_sync, self, args, kwargs)
            token = _current_run_config.set(option(arguments, "run_config"))
            try:
                return original_run_sync(self, *args, **kwargs)
            finally:
                _current_run_config.reset(token)

        run_sync.__wrapped__ = original_run_sync  # type: ignore[attr-defined]
        AgentRunner.run_sync = run_sync  # type: ignore[method-assign]

    original_streamed = getattr(AgentRunner, "run_streamed", None)
    if original_streamed is not None:

        def run_streamed(self: Any, *args: Any, **kwargs: Any) -> Any:
            arguments = bound_arguments(original_streamed, self, args, kwargs)
            # The runner starts its background task inside this call, so the task copies the value.
            token = _current_run_config.set(option(arguments, "run_config"))
            try:
                result = original_streamed(self, *args, **kwargs)
            finally:
                _current_run_config.reset(token)
            hooks = option(arguments, "hooks")
            if isinstance(hooks, Agento11yOpenAIAgentsRunHooks):
                hooks._register_streamed_result(result)
            return result

        run_streamed.__wrapped__ = original_streamed  # type: ignore[attr-defined]
        AgentRunner.run_streamed = run_streamed  # type: ignore[method-assign]


async def _invoke_handler(handler: Any, method_name: str, *args: Any, **kwargs: Any) -> None:
    method = getattr(handler, method_name)
    result = method(*args, **kwargs)
    if inspect.isawaitable(result):
        await result


async def _invoke_delegate(delegate: Any | None, method_name: str, *args: Any) -> None:
    if delegate is None:
        return
    method = getattr(delegate, method_name, None)
    if method is None:
        return
    result = method(*args)
    if inspect.isawaitable(result):
        await result


def _agent_name(agent: Any) -> str:
    for value in (_read(agent, "name"), _read(agent, "__class__.__name__")):
        text = _as_string(value)
        if text != "":
            return text
    return "openai_agent"


def _model_ref(agent: Any, run_config: Any | None = None) -> tuple[str, str]:
    """Return (provider, model) for the model the runner uses, as a string or a `Model` object.

    Mirrors the SDK: `RunConfig.model` overrides the agent's model, and an agent with
    neither falls back to the SDK default model.
    """
    model = _read(run_config, "model") if run_config is not None else None
    if model is None or model == "":
        model = _read(agent, "model")
    if model is None or model == "":
        model = _sdk_default_model()
    if isinstance(model, str):
        raw = model.strip()
    else:
        # `Model` objects such as LitellmModel carry the id on an attribute; their
        # str() is only the object repr.
        raw = _first_non_empty(
            _as_string(_read(model, "model")),
            _as_string(_read(model, "model_name")),
        )
    return _split_provider_prefix(raw)


# LiteLLM provider prefixes that name a provider agento11y records directly.
# Serving platforms that host several vendors' models (``vertex_ai``, ``azure``,
# ``openrouter``, ...) are left alone so their generations are not attributed to
# whichever vendor the stripped model name resembles. Matches the google-adk adapter.
_LITELLM_PROVIDER_PREFIXES = {
    "openai": "openai",
    "anthropic": "anthropic",
    "gemini": "gemini",
    "bedrock": "bedrock",
}


def _sdk_default_model() -> str:
    try:
        from agents.models import get_default_model
    except Exception:  # pragma: no cover - older openai-agents without default_models
        return ""
    try:
        return _as_string(get_default_model())
    except Exception:  # pragma: no cover - defensive: env-driven lookup failed
        return ""


def _split_provider_prefix(model: str) -> tuple[str, str]:
    """Split LiteLLM-style ids like ``anthropic/claude-haiku-5-5`` into (provider, model)."""
    prefix, sep, rest = model.partition("/")
    provider = _LITELLM_PROVIDER_PREFIXES.get(prefix.strip().lower(), "")
    rest = rest.strip()
    if sep == "" or provider == "" or rest == "":
        return "", model
    return provider, rest


def _tool_name(tool: Any) -> str:
    candidates = (_as_string(_read(tool, "name")), _as_string(_read(tool, "__class__.__name__")))
    for candidate in candidates:
        if candidate != "":
            return candidate
    return "framework_tool"


def _tool_definitions(agent: Any) -> list[ToolDefinition]:
    """Definitions of the agent's own tools, as the model sees them.

    Reads `Agent.tools` rather than `get_all_tools()` so recording never triggers MCP
    listing calls; MCP server tools and handoffs are not included.
    """
    tools = _read(agent, "tools")
    if not isinstance(tools, (list, tuple)):
        return []
    definitions: list[ToolDefinition] = []
    for tool in tools:
        name = _as_string(_read(tool, "name"))
        if name == "":
            continue
        schema = _read(tool, "params_json_schema")
        definitions.append(
            ToolDefinition(
                name=name,
                description=_as_string(_read(tool, "description")),
                # Function tools carry a parameter schema. Hosted tools (WebSearchTool, ...)
                # don't, and their name is the API tool kind (web_search, file_search, ...).
                type="function" if isinstance(schema, dict) else _as_string(_read(tool, "type")) or name,
                input_schema_json=_json_bytes(schema) if isinstance(schema, dict) else b"",
            )
        )
    return definitions


def _json_bytes(value: Any) -> bytes:
    try:
        return json.dumps(value, default=str, sort_keys=True).encode("utf-8")
    except Exception:
        return b""


# Output items that end the turn so the runner can execute a client-side tool.
_CLIENT_TOOL_CALL_TYPES = {
    "function_call",
    "custom_tool_call",
    "computer_call",
    "local_shell_call",
    "shell_call",
    "apply_patch_call",
}


def _stop_reason(response: Any) -> str:
    """Derive the finish reason from a ModelResponse's output items.

    ModelResponse drops the provider's finish reason, but every model the SDK ships
    (Responses, Chat Completions, LiteLLM) converts it into output items: tool calls
    mean `tool_calls`, a completed assistant message means `stop`. An incomplete
    message has no recoverable cause, so it records nothing.
    """
    output = _read(response, "output")
    if not isinstance(output, list):
        return ""
    if any(_read(item, "type") in _CLIENT_TOOL_CALL_TYPES for item in output):
        return "tool_calls"
    statuses = [_as_string(_read(item, "status")) for item in output if _read(item, "type") == "message"]
    if statuses and all(status in ("", "completed") for status in statuses):
        return "stop"
    return ""


def _input_messages(system_prompt: str | None, input_items: list[Any]) -> list[Any]:
    messages: list[Any] = []
    if isinstance(system_prompt, str) and system_prompt.strip() != "":
        messages.append({"role": "system", "content": system_prompt.strip()})
    tool_names = {
        _as_string(_read(item, "call_id")): _as_string(_read(item, "name"))
        for item in input_items
        if _read(item, "type") == "function_call"
    }
    messages.extend(_map_response_items(input_items, tool_names=tool_names, default_role=MessageRole.USER))
    return messages


def _map_response_items(items: list[Any], *, tool_names: dict[str, str], default_role: MessageRole) -> list[Any]:
    """Map Responses API items (messages, function calls, call outputs) to messages.

    Consecutive function calls are grouped into one assistant message, matching
    how the model emitted them in a single turn.
    """
    messages: list[Any] = []
    for item in items:
        item_type = _read(item, "type")
        if item_type == "function_call":
            name = _as_string(_read(item, "name"))
            if name == "":
                # validate_generation requires a tool call name; drop the part rather
                # than fail the whole generation.
                continue
            part = Part(
                kind=PartKind.TOOL_CALL,
                tool_call=ToolCall(
                    id=_as_string(_read(item, "call_id")),
                    name=name,
                    input_json=_as_string(_read(item, "arguments")).encode("utf-8"),
                ),
            )
            last = messages[-1] if messages else None
            if (
                isinstance(last, Message)
                and last.role is MessageRole.ASSISTANT
                and all(existing.kind is PartKind.TOOL_CALL for existing in last.parts)
            ):
                last.parts.append(part)
            else:
                messages.append(Message(role=MessageRole.ASSISTANT, parts=[part]))
            continue
        if item_type == "function_call_output":
            call_id = _as_string(_read(item, "call_id"))
            output = _read(item, "output")
            messages.append(
                Message(
                    role=MessageRole.TOOL,
                    parts=[
                        Part(
                            kind=PartKind.TOOL_RESULT,
                            tool_result=ToolResult(
                                tool_call_id=call_id,
                                name=tool_names.get(call_id, ""),
                                content=output if isinstance(output, str) else _tool_input_string(output),
                            ),
                        )
                    ],
                )
            )
            continue
        text = _message_text(item)
        if text == "":
            continue
        role = _as_string(_read(item, "role")).lower()
        if role in ("system", "developer"):
            messages.append({"role": "system", "content": text})
            continue
        mapped_role = {"assistant": MessageRole.ASSISTANT, "user": MessageRole.USER}.get(role, default_role)
        messages.append(Message(role=mapped_role, parts=[Part(kind=PartKind.TEXT, text=text)]))
    return messages


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
        parts = [_message_text(part) for part in content]
        return " ".join(part for part in parts if part != "").strip()

    for key in ("text", "value"):
        text = _as_string(_read(value, key))
        if text != "":
            return text
    return ""


def _tool_input_string(tool_input: Any) -> str:
    if tool_input is None:
        return ""
    if isinstance(tool_input, str):
        return tool_input
    try:
        return json.dumps(tool_input, default=str, sort_keys=True)
    except Exception:
        return str(tool_input)


def _run_context_key(context: Any) -> int:
    # The SDK shares tool-state dicts between a run's wrapper and its fresh
    # AgentHookContext / ToolContext callbacks. Agent.as_tool creates fresh state
    # for each nested run, even when the same agent and Usage are reused in
    # parallel. Older SDKs lack this state, so retain usage-based correlation.
    tool_state = getattr(context, "_tool_invocations", None)
    return id(tool_state) if tool_state is not None else _usage_context_key(context)


def _usage_context_key(context: Any) -> int:
    usage = _read(context, "usage")
    return id(usage) if usage is not None else id(context)


def _tool_context_key(context: Any, tool: Any) -> tuple[int, int, str | None]:
    # Function tools have a per-call ID; other tool hooks may only receive a
    # RunContextWrapper, so retain the per-tool stack as a fallback.
    call_id = _read(context, "tool_call_id")
    return (_run_context_key(context), id(tool), call_id if isinstance(call_id, str) and call_id else None)


def _context_metadata(context: Any) -> dict[str, Any]:
    metadata: dict[str, Any] = {}

    context_payload = _read(context, "context")
    for key in ("conversation_id", "session_id", "group_id", "thread_id"):
        value = _first_non_empty(
            _as_string(_read(context, key)),
            _as_string(_read(context_payload, key)),
            _as_string(_read(context_payload, _camel_case(key))),
        )
        if value != "":
            metadata[key] = value

    context_object = context_payload if context_payload is not None else context
    trace_id = _first_non_empty(
        _as_string(_read(context_object, "trace_id")),
        _as_string(_read(context_object, "traceId")),
    )
    if trace_id != "":
        metadata["event_id"] = trace_id

    return metadata


def _build_llm_end_response(response: Any, *, model_name: str) -> dict[str, Any]:
    text = _extract_openai_agents_output_text(response)
    llm_output: dict[str, Any] = {}
    # The model the provider reports beats the configured alias.
    resolved_model_name = _first_non_empty(_as_string(_read(response, "model")), model_name)
    if resolved_model_name != "":
        llm_output["model_name"] = resolved_model_name

    usage = _usage_payload(_read(response, "usage"))
    if usage is not None:
        llm_output["token_usage"] = usage
    stop_reason = _stop_reason(response)
    if stop_reason != "":
        llm_output["finish_reason"] = stop_reason

    payload: dict[str, Any] = {"llm_output": llm_output}
    output = _read(response, "output")
    if isinstance(output, list) and any(_read(item, "type") == "function_call" for item in output):
        messages = _map_response_items(output, tool_names={}, default_role=MessageRole.ASSISTANT)
        payload["generations"] = [[{"message": message} for message in messages]]
    elif text != "":
        payload["generations"] = [[{"text": text}]]
    return payload


def _usage_payload(usage: Any) -> dict[str, int] | None:
    if usage is None:
        return None
    prompt_tokens = _int_or_none(_read(usage, "input_tokens"))
    completion_tokens = _int_or_none(_read(usage, "output_tokens"))
    total_tokens = _int_or_none(_read(usage, "total_tokens"))
    payload: dict[str, int] = {}
    if prompt_tokens is not None:
        payload["prompt_tokens"] = prompt_tokens
    if completion_tokens is not None:
        payload["completion_tokens"] = completion_tokens
    if total_tokens is not None:
        payload["total_tokens"] = total_tokens
    if not payload:
        return None
    return payload


def _extract_openai_agents_output_text(response: Any) -> str:
    output = _read(response, "output")
    if isinstance(output, list):
        parts = [_message_text(item) for item in output]
        return "\n".join(part for part in parts if part != "").strip()
    return _message_text(response)


def _read(value: Any, key: str) -> Any:
    if value is None:
        return None
    if "." in key:
        current = value
        for piece in key.split("."):
            current = _read(current, piece)
            if current is None:
                return None
        return current
    if isinstance(value, dict):
        return value.get(key)
    return getattr(value, key, None)


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
    "Agento11yOpenAIAgentsHandler",
    "Agento11yAsyncOpenAIAgentsHandler",
    "Agento11yOpenAIAgentsRunHooks",
    "create_agento11y_openai_agents_handler",
    "create_agento11y_openai_agents_hooks",
    "with_agento11y_openai_agents_hooks",
]
