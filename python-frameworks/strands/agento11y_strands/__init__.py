"""Public exports for agento11y Strands Agents hooks."""

from __future__ import annotations

import json
from typing import Any
from uuid import UUID, uuid4

from agento11y import Client, ToolDefinition

from .handler import Agento11yStrandsHandler

try:  # pragma: no cover - imported from strands-agents at runtime
    from strands.hooks import (
        AfterInvocationEvent,
        AfterModelCallEvent,
        AfterMultiAgentInvocationEvent,
        AfterNodeCallEvent,
        AfterToolCallEvent,
        BeforeInvocationEvent,
        BeforeModelCallEvent,
        BeforeMultiAgentInvocationEvent,
        BeforeNodeCallEvent,
        BeforeToolCallEvent,
        HookRegistry,
    )
except Exception:  # pragma: no cover - lightweight fallback for local unit tests
    AfterInvocationEvent = object  # type: ignore[assignment]
    AfterModelCallEvent = object  # type: ignore[assignment]
    AfterMultiAgentInvocationEvent = object  # type: ignore[assignment]
    AfterNodeCallEvent = object  # type: ignore[assignment]
    AfterToolCallEvent = object  # type: ignore[assignment]
    BeforeInvocationEvent = object  # type: ignore[assignment]
    BeforeModelCallEvent = object  # type: ignore[assignment]
    BeforeMultiAgentInvocationEvent = object  # type: ignore[assignment]
    BeforeNodeCallEvent = object  # type: ignore[assignment]
    BeforeToolCallEvent = object  # type: ignore[assignment]
    HookRegistry = Any  # type: ignore[assignment]


class Agento11yStrandsHookProvider:
    """HookProvider-compatible Strands bridge that records model and tool lifecycles."""

    def __init__(self, *, agento11y_handler: Agento11yStrandsHandler) -> None:
        self._agento11y_handler = agento11y_handler
        self._invocation_run_ids: dict[int, list[UUID]] = {}
        self._model_run_ids: dict[int, list[UUID]] = {}
        self._tool_run_ids: dict[str, UUID] = {}
        self._tool_fallback_run_ids: dict[int, list[UUID]] = {}
        self._multi_agent_run_ids: dict[int, list[UUID]] = {}
        self._node_run_ids: dict[tuple[int, str], list[UUID]] = {}

    def register_hooks(self, registry: HookRegistry, **_kwargs: Any) -> None:
        """Register agento11y callbacks with a Strands HookRegistry."""
        registry.add_callback(BeforeInvocationEvent, self.before_invocation)
        registry.add_callback(AfterInvocationEvent, self.after_invocation)
        registry.add_callback(BeforeModelCallEvent, self.before_model_call)
        registry.add_callback(AfterModelCallEvent, self.after_model_call)
        registry.add_callback(BeforeToolCallEvent, self.before_tool_call)
        registry.add_callback(AfterToolCallEvent, self.after_tool_call)
        registry.add_callback(BeforeMultiAgentInvocationEvent, self.before_multi_agent_invocation)
        registry.add_callback(AfterMultiAgentInvocationEvent, self.after_multi_agent_invocation)
        registry.add_callback(BeforeNodeCallEvent, self.before_node_call)
        registry.add_callback(AfterNodeCallEvent, self.after_node_call)

    def before_invocation(self, event: Any) -> None:
        run_id = uuid4()
        self._stack_for(self._invocation_run_ids, event).append(run_id)
        agent = _read(event, "agent")
        run_name = _agent_name(agent)
        self._agento11y_handler.set_default_agent_name(run_name)
        self._agento11y_handler.on_chain_start(
            {"name": run_name},
            {},
            run_id=run_id,
            parent_run_id=None,
            metadata=_metadata(event),
            run_type="invocation",
            run_name=run_name,
        )

    def after_invocation(self, event: Any) -> None:
        run_id = self._pop_stack(self._invocation_run_ids, event)
        if run_id is None:
            return
        self._agento11y_handler.on_chain_end({"status": "completed"}, run_id=run_id)

    def before_model_call(self, event: Any) -> None:
        run_id = uuid4()
        self._stack_for(self._model_run_ids, event).append(run_id)
        agent = _read(event, "agent")
        self._agento11y_handler.set_default_agent_name(_agent_name(agent))
        model_name = _model_name(agent)
        invocation_params = _model_invocation_params(agent, model_name=model_name)
        self._agento11y_handler.on_chat_model_start(
            _serialized_agent(agent, model_name=model_name),
            [_agent_messages(agent)],
            run_id=run_id,
            parent_run_id=self._peek_stack(self._invocation_run_ids, event),
            invocation_params=invocation_params,
            metadata=_metadata(event),
            run_name=_agent_name(agent),
        )

    def after_model_call(self, event: Any) -> None:
        run_id = self._pop_stack(self._model_run_ids, event)
        if run_id is None:
            return

        exception = _read(event, "exception")
        if exception is not None:
            self._agento11y_handler.on_llm_error(_as_exception(exception), run_id=run_id)
            return

        self._agento11y_handler.on_llm_end(_llm_end_payload(event), run_id=run_id)

    def before_tool_call(self, event: Any) -> None:
        agent = _read(event, "agent")
        self._agento11y_handler.set_default_agent_name(_agent_name(agent))
        tool_use = _read(event, "tool_use") or {}
        tool_use_id = _as_string(_read(tool_use, "toolUseId"))
        run_id = uuid4()
        if tool_use_id != "":
            self._tool_run_ids[self._tool_key(event, tool_use_id)] = run_id
        else:
            self._stack_for(self._tool_fallback_run_ids, event).append(run_id)

        selected_tool = _read(event, "selected_tool")
        tool_name = _first_non_empty(_as_string(_read(tool_use, "name")), _tool_name(selected_tool))
        tool_input = _read(tool_use, "input")
        metadata = _metadata(event)
        if tool_use_id != "":
            metadata["event_id"] = tool_use_id

        self._agento11y_handler.on_tool_start(
            {"name": tool_name, "description": _tool_description(selected_tool)},
            _json_string(tool_input),
            run_id=run_id,
            parent_run_id=self._peek_stack(self._model_run_ids, event),
            metadata=metadata,
            run_name=tool_name,
            inputs=tool_input,
        )

    def after_tool_call(self, event: Any) -> None:
        tool_use = _read(event, "tool_use") or {}
        tool_use_id = _as_string(_read(tool_use, "toolUseId"))
        if tool_use_id != "":
            run_id = self._tool_run_ids.pop(self._tool_key(event, tool_use_id), None)
        else:
            run_id = self._pop_stack(self._tool_fallback_run_ids, event)
        if run_id is None:
            return

        exception = _read(event, "exception")
        if exception is not None:
            self._agento11y_handler.on_tool_error(_as_exception(exception), run_id=run_id)
            return

        self._agento11y_handler.on_tool_end(_read(event, "result"), run_id=run_id)

    def before_multi_agent_invocation(self, event: Any) -> None:
        run_id = uuid4()
        self._stack_for(self._multi_agent_run_ids, event).append(run_id)
        source = _read(event, "source")
        run_name = _first_non_empty(_as_string(_read(source, "name")), _as_string(_read(source, "__class__.__name__")))
        self._agento11y_handler.on_chain_start(
            {"name": run_name or "multi_agent"},
            {},
            run_id=run_id,
            parent_run_id=None,
            metadata=_metadata(event),
            run_type="multi_agent",
            run_name=run_name or "multi_agent",
        )

    def after_multi_agent_invocation(self, event: Any) -> None:
        run_id = self._pop_stack(self._multi_agent_run_ids, event)
        if run_id is not None:
            self._agento11y_handler.on_chain_end({"status": "completed"}, run_id=run_id)

    def before_node_call(self, event: Any) -> None:
        run_id = uuid4()
        node_id = _as_string(_read(event, "node_id")) or "node"
        key = (self._source_key(event), node_id)
        self._node_run_ids.setdefault(key, []).append(run_id)
        self._agento11y_handler.on_chain_start(
            {"name": node_id},
            {},
            run_id=run_id,
            parent_run_id=self._peek_stack(self._multi_agent_run_ids, event),
            metadata=_metadata(event),
            run_type="node",
            run_name=node_id,
        )

    def after_node_call(self, event: Any) -> None:
        node_id = _as_string(_read(event, "node_id")) or "node"
        key = (self._source_key(event), node_id)
        stack = self._node_run_ids.get(key, [])
        run_id = stack.pop() if stack else None
        if not stack:
            self._node_run_ids.pop(key, None)
        if run_id is not None:
            self._agento11y_handler.on_chain_end({"status": "completed"}, run_id=run_id)

    def _stack_for(self, store: dict[int, list[UUID]], event: Any) -> list[UUID]:
        return store.setdefault(self._source_key(event), [])

    def _peek_stack(self, store: dict[int, list[UUID]], event: Any) -> UUID | None:
        stack = store.get(self._source_key(event), [])
        return stack[-1] if stack else None

    def _pop_stack(self, store: dict[int, list[UUID]], event: Any) -> UUID | None:
        key = self._source_key(event)
        stack = store.get(key, [])
        run_id = stack.pop() if stack else None
        if not stack:
            store.pop(key, None)
        return run_id

    def _source_key(self, event: Any) -> int:
        invocation_state = _read(event, "invocation_state")
        if isinstance(invocation_state, dict):
            return id(invocation_state)
        source = _read(event, "agent") or _read(event, "source") or event
        return id(source)

    def _tool_key(self, event: Any, tool_use_id: str) -> str:
        return f"{self._source_key(event)}:{tool_use_id}"


def create_agento11y_strands_handler(*, client: Client, **handler_kwargs: Any) -> Agento11yStrandsHandler:
    """Create a Strands agento11y lifecycle handler."""
    return Agento11yStrandsHandler(client=client, **handler_kwargs)


def create_agento11y_strands_hook_provider(*, client: Client, **handler_kwargs: Any) -> Agento11yStrandsHookProvider:
    """Create a HookProvider-compatible Strands integration."""
    return Agento11yStrandsHookProvider(
        agento11y_handler=create_agento11y_strands_handler(client=client, **handler_kwargs),
    )


def with_agento11y_strands_hooks(
    config_or_agent: dict[str, Any] | Any | None,
    *,
    client: Client,
    **handler_kwargs: Any,
) -> dict[str, Any] | Any:
    """Attach agento11y as a Strands hook provider on config dicts or agent instances."""
    provider = create_agento11y_strands_hook_provider(client=client, **handler_kwargs)

    if config_or_agent is None or isinstance(config_or_agent, dict):
        merged = dict(config_or_agent or {})
        hooks = _as_list(merged.get("hooks"))
        if not _contains_agento11y_provider(hooks):
            hooks.append(provider)
        merged["hooks"] = hooks
        return merged

    agent = config_or_agent
    hooks_registry = getattr(agent, "hooks", None)
    if hooks_registry is not None and hasattr(hooks_registry, "add_hook"):
        if not getattr(hooks_registry, "_agento11y_instrumented", False):
            hooks_registry.add_hook(provider)
            hooks_registry._agento11y_instrumented = True
        return agent

    hooks = _as_list(getattr(agent, "hooks", None))
    if not _contains_agento11y_provider(hooks):
        hooks.append(provider)
    agent.hooks = hooks
    return agent


def _serialized_agent(agent: Any, *, model_name: str) -> dict[str, Any]:
    payload: dict[str, Any] = {"name": _agent_name(agent)}
    if model_name != "":
        payload["kwargs"] = {"model": model_name}
    provider = _model_provider(agent)
    if provider != "":
        payload["provider"] = provider
    return payload


def _model_invocation_params(agent: Any, *, model_name: str) -> dict[str, Any]:
    params: dict[str, Any] = {"stream": True}
    if model_name != "":
        params["model"] = model_name
    provider = _model_provider(agent)
    if provider != "":
        params["provider"] = provider

    system_prompt = _as_string(_read(agent, "system_prompt"))
    if system_prompt != "":
        params["system_prompt"] = system_prompt

    config = _model_config(agent)
    for key in ("temperature", "max_tokens", "top_p", "tool_choice"):
        value = _read(config, key)
        if value is not None:
            params[key] = value

    tools = _tool_definitions(agent)
    if tools:
        params["tools"] = tools
    return params


def _llm_end_payload(event: Any) -> dict[str, Any]:
    stop_response = _read(event, "stop_response")
    message = _read(stop_response, "message")
    stop_reason = _as_string(_read(stop_response, "stop_reason"))
    usage = _usage_payload(_read(_read(message, "metadata"), "usage"))
    model_name = _model_name(_read(event, "agent"))

    llm_output: dict[str, Any] = {}
    if model_name != "":
        llm_output["model_name"] = model_name
    if stop_reason != "":
        llm_output["stop_reason"] = stop_reason
    if usage is not None:
        llm_output["token_usage"] = usage

    return {"llm_output": llm_output, "generations": [[{"message": message}]]}


def _usage_payload(usage: Any) -> dict[str, int] | None:
    if usage is None:
        return None
    payload: dict[str, int] = {}
    for source_key, dest_key in (
        ("inputTokens", "input_tokens"),
        ("input_tokens", "input_tokens"),
        ("prompt_tokens", "input_tokens"),
        ("outputTokens", "output_tokens"),
        ("output_tokens", "output_tokens"),
        ("completion_tokens", "output_tokens"),
        ("totalTokens", "total_tokens"),
        ("total_tokens", "total_tokens"),
        ("cacheReadInputTokens", "cache_read_input_tokens"),
        ("cacheWriteInputTokens", "cache_write_input_tokens"),
    ):
        value = _int_or_none(_read(usage, source_key))
        if value is not None:
            payload[dest_key] = value
    return payload or None


def _agent_messages(agent: Any) -> list[Any]:
    messages = _read(agent, "messages")
    return messages if isinstance(messages, list) else []


def _agent_name(agent: Any) -> str:
    return _first_non_empty(
        _as_string(_read(agent, "name")),
        _as_string(_read(agent, "agent_id")),
        _as_string(_read(agent, "__class__.__name__")),
        "strands_agent",
    )


def _model_name(agent: Any) -> str:
    config = _model_config(agent)
    model = _read(agent, "model")
    return _first_non_empty(
        _as_string(_read(config, "model")),
        _as_string(_read(config, "model_id")),
        _as_string(_read(config, "model_name")),
        _as_string(_read(model, "model")),
        _as_string(_read(model, "model_id")),
        _as_string(_read(model, "model_name")),
        _as_string(_read(model, "name")),
        "",
    )


def _model_provider(agent: Any) -> str:
    config = _model_config(agent)
    model = _read(agent, "model")
    return _first_non_empty(
        _as_string(_read(config, "provider")),
        _as_string(_read(config, "provider_id")),
        _as_string(_read(model, "provider")),
        _as_string(_read(model, "provider_id")),
        "",
    )


def _model_config(agent: Any) -> Any:
    model = _read(agent, "model")
    get_config = getattr(model, "get_config", None)
    if callable(get_config):
        try:
            return get_config()
        except Exception:
            return {}
    return _read(model, "config") or {}


def _tool_definitions(agent: Any) -> list[ToolDefinition]:
    registry = _read(agent, "tool_registry")
    get_config = getattr(registry, "get_all_tools_config", None)
    if not callable(get_config):
        return []
    try:
        raw_tools = get_config()
    except Exception:
        return []
    if isinstance(raw_tools, dict):
        values = raw_tools.values()
    elif isinstance(raw_tools, list):
        values = raw_tools
    else:
        return []

    definitions: list[ToolDefinition] = []
    for raw in values:
        spec = _read(raw, "toolSpec") or raw
        name = _as_string(_read(spec, "name"))
        if name == "":
            continue
        definitions.append(
            ToolDefinition(
                name=name,
                description=_as_string(_read(spec, "description")),
                type="strands",
                input_schema_json=_json_bytes(_read(spec, "inputSchema")),
            )
        )
    return definitions


def _tool_name(tool: Any) -> str:
    spec = _read(tool, "tool_spec")
    return _first_non_empty(
        _as_string(_read(spec, "name")),
        _as_string(_read(tool, "tool_name")),
        _as_string(_read(tool, "name")),
        _as_string(_read(tool, "__class__.__name__")),
        "framework_tool",
    )


def _tool_description(tool: Any) -> str:
    spec = _read(tool, "tool_spec")
    return _first_non_empty(_as_string(_read(spec, "description")), _as_string(_read(tool, "description")))


def _metadata(event: Any) -> dict[str, Any]:
    metadata: dict[str, Any] = {}
    invocation_state = _read(event, "invocation_state")
    if isinstance(invocation_state, dict):
        for key in ("conversation_id", "conversationId", "session_id", "sessionId", "group_id", "groupId"):
            value = _as_string(_read(invocation_state, key))
            if value != "":
                metadata[key] = value
        for key in ("thread_id", "threadId", "event_id", "eventId", "invocation_id", "invocationId"):
            value = _as_string(_read(invocation_state, key))
            if value != "":
                metadata[key] = value

    agent = _read(event, "agent")
    agent_id = _as_string(_read(agent, "agent_id"))
    if agent_id != "":
        metadata["component_name"] = agent_id
    return metadata


def _message_text(message: Any) -> str:
    content = _read(message, "content")
    if isinstance(content, str):
        return content.strip()
    if not isinstance(content, list):
        return ""
    parts: list[str] = []
    for item in content:
        text = _as_string(_read(item, "text"))
        if text != "":
            parts.append(text)
    return " ".join(parts).strip()


def _json_string(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    try:
        return json.dumps(value, default=str, sort_keys=True)
    except Exception:
        return str(value)


def _json_bytes(value: Any) -> bytes:
    if value is None:
        return b""
    try:
        return json.dumps(value, default=str, sort_keys=True).encode()
    except Exception:
        return b""


def _contains_agento11y_provider(hooks: list[Any]) -> bool:
    return any(isinstance(hook, Agento11yStrandsHookProvider) for hook in hooks)


def _as_list(value: Any) -> list[Any]:
    if isinstance(value, list):
        return list(value)
    if value is None:
        return []
    return [value]


def _read(value: Any, key: str) -> Any:
    if value is None:
        return None
    if key == "__class__.__name__":
        return value.__class__.__name__
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


def _as_exception(value: Any) -> BaseException:
    if isinstance(value, BaseException):
        return value
    return Exception(str(value))


__all__ = [
    "Agento11yStrandsHandler",
    "Agento11yStrandsHookProvider",
    "create_agento11y_strands_handler",
    "create_agento11y_strands_hook_provider",
    "with_agento11y_strands_hooks",
]
