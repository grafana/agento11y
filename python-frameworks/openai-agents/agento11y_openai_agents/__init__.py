"""Public exports for Sigil OpenAI Agents callback handlers."""

from __future__ import annotations

import inspect
import json
from typing import Any
from uuid import UUID, uuid4

from agento11y import Client

from .handler import SigilAsyncOpenAIAgentsHandler, SigilOpenAIAgentsHandler

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


def create_sigil_openai_agents_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> SigilOpenAIAgentsHandler | SigilAsyncOpenAIAgentsHandler:
    """Create an OpenAI Agents Sigil handler for sync or async flows."""
    if async_handler:
        return SigilAsyncOpenAIAgentsHandler(client=client, **handler_kwargs)
    return SigilOpenAIAgentsHandler(client=client, **handler_kwargs)


class SigilOpenAIAgentsRunHooks(RunHooks):
    """RunHooks bridge that maps OpenAI Agents lifecycle callbacks into Sigil handlers."""

    def __init__(
        self,
        *,
        sigil_handler: SigilOpenAIAgentsHandler | SigilAsyncOpenAIAgentsHandler,
        delegate: Any | None = None,
    ) -> None:
        self._sigil_handler = sigil_handler
        self._delegate = delegate
        self._agent_run_stacks: dict[int, list[UUID]] = {}
        self._llm_run_stacks: dict[tuple[int, int], list[UUID]] = {}
        self._tool_run_stacks: dict[tuple[int, int], list[UUID]] = {}

    async def on_agent_start(self, context: Any, agent: Any) -> None:
        await _invoke_delegate(self._delegate, "on_agent_start", context, agent)

        context_key = id(context)
        stack = self._agent_run_stacks.setdefault(context_key, [])
        run_id = uuid4()
        parent_run_id = stack[-1] if stack else None
        stack.append(run_id)

        agent_name = _agent_name(agent)
        await _invoke_handler(
            self._sigil_handler,
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
        context_key = id(context)
        stack = self._agent_run_stacks.get(context_key, [])
        run_id = stack.pop() if stack else None
        if run_id is not None:
            await _invoke_handler(self._sigil_handler, "on_chain_end", {"output": output}, run_id=run_id)
        if not stack:
            self._agent_run_stacks.pop(context_key, None)

        await _invoke_delegate(self._delegate, "on_agent_end", context, agent, output)

    async def on_handoff(self, context: Any, from_agent: Any, to_agent: Any) -> None:
        run_id = uuid4()
        stack = self._agent_run_stacks.get(id(context), [])
        parent_run_id = stack[-1] if stack else None
        serialized = {
            "name": "agent_handoff",
            "source": _agent_name(from_agent),
            "destination": _agent_name(to_agent),
        }
        await _invoke_handler(
            self._sigil_handler,
            "on_chain_start",
            serialized,
            {},
            run_id=run_id,
            parent_run_id=parent_run_id,
            metadata=_context_metadata(context),
            run_type="handoff",
            run_name="agent_handoff",
        )
        await _invoke_handler(self._sigil_handler, "on_chain_end", {"handoff": True}, run_id=run_id)

        await _invoke_delegate(self._delegate, "on_handoff", context, from_agent, to_agent)

    async def on_llm_start(
        self,
        context: Any,
        agent: Any,
        system_prompt: str | None,
        input_items: list[Any],
    ) -> None:
        await _invoke_delegate(self._delegate, "on_llm_start", context, agent, system_prompt, input_items)

        context_key = id(context)
        llm_key = (context_key, id(agent))
        llm_stack = self._llm_run_stacks.setdefault(llm_key, [])
        run_id = uuid4()
        llm_stack.append(run_id)

        parent_stack = self._agent_run_stacks.get(context_key, [])
        parent_run_id = parent_stack[-1] if parent_stack else None

        model_name = _model_name(agent)
        invocation_params: dict[str, Any] = {}
        if model_name != "":
            invocation_params["model"] = model_name
        serialized = {"name": _agent_name(agent)}
        if model_name != "":
            serialized["kwargs"] = {"model": model_name}

        await _invoke_handler(
            self._sigil_handler,
            "on_llm_start",
            serialized,
            _input_prompts(system_prompt, input_items),
            run_id=run_id,
            parent_run_id=parent_run_id,
            invocation_params=invocation_params,
            metadata=_context_metadata(context),
            run_name=_agent_name(agent),
        )

    async def on_llm_end(self, context: Any, agent: Any, response: Any) -> None:
        llm_key = (id(context), id(agent))
        llm_stack = self._llm_run_stacks.get(llm_key, [])
        run_id = llm_stack.pop() if llm_stack else None
        if run_id is not None:
            await _invoke_handler(
                self._sigil_handler,
                "on_llm_end",
                _build_llm_end_response(response, model_name=_model_name(agent)),
                run_id=run_id,
            )
        if not llm_stack:
            self._llm_run_stacks.pop(llm_key, None)

        await _invoke_delegate(self._delegate, "on_llm_end", context, agent, response)

    async def on_tool_start(self, context: Any, agent: Any, tool: Any) -> None:
        await _invoke_delegate(self._delegate, "on_tool_start", context, agent, tool)

        context_key = id(context)
        tool_key = (context_key, id(tool))
        tool_stack = self._tool_run_stacks.setdefault(tool_key, [])
        run_id = uuid4()
        tool_stack.append(run_id)

        llm_stack = self._llm_run_stacks.get((context_key, id(agent)), [])
        if llm_stack:
            parent_run_id = llm_stack[-1]
        else:
            agent_stack = self._agent_run_stacks.get(context_key, [])
            parent_run_id = agent_stack[-1] if agent_stack else None

        tool_name = _tool_name(tool)
        tool_input = _read(context, "tool_input")
        await _invoke_handler(
            self._sigil_handler,
            "on_tool_start",
            {"name": tool_name},
            _tool_input_string(tool_input),
            run_id=run_id,
            parent_run_id=parent_run_id,
            metadata=_context_metadata(context),
            run_name=tool_name,
            inputs=tool_input,
        )

    async def on_tool_end(self, context: Any, agent: Any, tool: Any, result: Any) -> None:
        tool_key = (id(context), id(tool))
        tool_stack = self._tool_run_stacks.get(tool_key, [])
        run_id = tool_stack.pop() if tool_stack else None
        if run_id is not None:
            await _invoke_handler(self._sigil_handler, "on_tool_end", result, run_id=run_id)
        if not tool_stack:
            self._tool_run_stacks.pop(tool_key, None)

        await _invoke_delegate(self._delegate, "on_tool_end", context, agent, tool, result)


def create_sigil_openai_agents_hooks(
    *,
    client: Client,
    existing_hooks: Any | None = None,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> SigilOpenAIAgentsRunHooks:
    """Create a RunHooks instance wired to Sigil instrumentation."""
    sigil_handler = create_sigil_openai_agents_handler(
        client=client,
        async_handler=async_handler,
        **handler_kwargs,
    )
    return SigilOpenAIAgentsRunHooks(sigil_handler=sigil_handler, delegate=existing_hooks)


def with_sigil_openai_agents_hooks(
    run_options: dict[str, Any] | None,
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> dict[str, Any]:
    """Set OpenAI Agents `hooks` to a valid RunHooks bridge with Sigil instrumentation."""
    merged = dict(run_options or {})
    existing = merged.get("hooks")
    if isinstance(existing, list):
        raise TypeError("openai-agents expects `hooks` to be a RunHooks instance, not a list.")

    if isinstance(existing, SigilOpenAIAgentsRunHooks):
        return merged
    if isinstance(existing, (SigilOpenAIAgentsHandler, SigilAsyncOpenAIAgentsHandler)):
        existing = None

    merged["hooks"] = create_sigil_openai_agents_hooks(
        client=client,
        existing_hooks=existing,
        async_handler=async_handler,
        **handler_kwargs,
    )
    return merged


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


def _model_name(agent: Any) -> str:
    model = _read(agent, "model")
    candidates = (
        _as_string(model),
        _as_string(_read(model, "model")),
        _as_string(_read(model, "model_name")),
        _as_string(_read(model, "name")),
    )
    for candidate in candidates:
        if candidate != "":
            return candidate
    return ""


def _tool_name(tool: Any) -> str:
    candidates = (_as_string(_read(tool, "name")), _as_string(_read(tool, "__class__.__name__")))
    for candidate in candidates:
        if candidate != "":
            return candidate
    return "framework_tool"


def _input_prompts(system_prompt: str | None, input_items: list[Any]) -> list[str]:
    prompts: list[str] = []
    if isinstance(system_prompt, str) and system_prompt.strip() != "":
        prompts.append(system_prompt.strip())
    for item in input_items:
        text = _message_text(item)
        if text != "":
            prompts.append(text)
    return prompts or [""]


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
    resolved_model_name = _first_non_empty(model_name, _as_string(_read(response, "model")))
    if resolved_model_name != "":
        llm_output["model_name"] = resolved_model_name

    usage = _usage_payload(_read(response, "usage"))
    if usage is not None:
        llm_output["token_usage"] = usage

    payload: dict[str, Any] = {"llm_output": llm_output}
    if text != "":
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
    "SigilOpenAIAgentsHandler",
    "SigilAsyncOpenAIAgentsHandler",
    "SigilOpenAIAgentsRunHooks",
    "create_sigil_openai_agents_handler",
    "create_sigil_openai_agents_hooks",
    "with_sigil_openai_agents_hooks",
]
