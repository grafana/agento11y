"""Public exports for Sigil Google ADK callback handlers."""

from __future__ import annotations

import inspect
import json
from typing import Any
from uuid import UUID, uuid4

from agento11y import Client, ToolDefinition

from .handler import Agento11yAsyncGoogleAdkHandler, Agento11yGoogleAdkHandler

try:  # pragma: no cover - imported from google-adk at runtime
    from google.adk.plugins import BasePlugin
except Exception:  # pragma: no cover - lightweight fallback for local unit tests

    class BasePlugin:  # type: ignore[no-redef]
        """Fallback BasePlugin shape used when google-adk isn't installed."""

        async def before_run_callback(self, *, invocation_context: Any) -> Any:
            del invocation_context
            return None

        async def on_event_callback(self, *, invocation_context: Any, event: Any) -> Any:
            del invocation_context, event
            return None

        async def after_run_callback(self, *, invocation_context: Any) -> None:
            del invocation_context
            return None

        async def before_agent_callback(self, *, agent: Any, callback_context: Any) -> Any:
            del agent, callback_context
            return None

        async def after_agent_callback(self, *, agent: Any, callback_context: Any) -> Any:
            del agent, callback_context
            return None

        async def before_model_callback(self, *, callback_context: Any, llm_request: Any) -> Any:
            del callback_context, llm_request
            return None

        async def after_model_callback(self, *, callback_context: Any, llm_response: Any) -> Any:
            del callback_context, llm_response
            return None

        async def before_tool_callback(self, *, tool: Any, tool_args: dict[str, Any], tool_context: Any) -> Any:
            del tool, tool_args, tool_context
            return None

        async def after_tool_callback(
            self,
            *,
            tool: Any,
            tool_args: dict[str, Any],
            tool_context: Any,
            result: dict[str, Any],
        ) -> Any:
            del tool, tool_args, tool_context, result
            return None


_adk_callback_fields = (
    "before_model_callback",
    "after_model_callback",
    "on_model_error_callback",
    "before_tool_callback",
    "after_tool_callback",
    "on_tool_error_callback",
)


def create_agento11y_google_adk_handler(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> Agento11yGoogleAdkHandler | Agento11yAsyncGoogleAdkHandler:
    """Create a Google ADK Sigil callback handler for sync or async flows."""
    if async_handler:
        return Agento11yAsyncGoogleAdkHandler(client=client, **handler_kwargs)
    return Agento11yGoogleAdkHandler(client=client, **handler_kwargs)


class Agento11yGoogleAdkCallbacks:
    """Google ADK callback bridge that forwards agent callback fields to Sigil."""

    def __init__(self, agento11y_handler: Agento11yGoogleAdkHandler | Agento11yAsyncGoogleAdkHandler) -> None:
        self._agento11y_handler = agento11y_handler
        self._llm_run_stacks: dict[str, list[UUID]] = {}
        self._tool_run_ids: dict[str, UUID] = {}
        self._tool_fallback_run_stacks: dict[str, list[UUID]] = {}

    async def before_model_callback(
        self,
        callback_context: Any,
        llm_request: Any,
        *,
        parent_run_id: UUID | None = None,
    ) -> None:
        invocation_key = self._invocation_key(callback_context)
        run_id = uuid4()
        self._llm_run_stacks.setdefault(invocation_key, []).append(run_id)

        invocation_params = _adk_invocation_params(llm_request)
        model_name = _as_string(invocation_params.get("model"))

        messages = _adk_messages(_read(llm_request, "contents"))
        await _invoke_handler(
            self._agento11y_handler,
            "on_chat_model_start",
            _serialized_llm_payload(callback_context, model_name),
            [messages],
            run_id=run_id,
            parent_run_id=parent_run_id,
            invocation_params=invocation_params,
            metadata=_adk_context_metadata(callback_context),
            run_name=_agent_name(callback_context),
        )
        return None

    async def after_model_callback(self, callback_context: Any, llm_response: Any) -> None:
        invocation_key = self._invocation_key(callback_context)
        run_id = self._pop_llm_run_id(invocation_key)
        if run_id is None:
            return None

        await _invoke_handler(
            self._agento11y_handler,
            "on_llm_end",
            _adk_llm_end_payload(llm_response),
            run_id=run_id,
        )
        return None

    async def on_model_error_callback(self, callback_context: Any, llm_request: Any, error: Exception) -> None:
        del llm_request
        invocation_key = self._invocation_key(callback_context)
        run_id = self._pop_llm_run_id(invocation_key)
        if run_id is None:
            return None

        await _invoke_handler(self._agento11y_handler, "on_llm_error", error, run_id=run_id)
        return None

    async def before_tool_callback(self, tool: Any, args: dict[str, Any], tool_context: Any) -> None:
        run_id = uuid4()
        invocation_key = self._invocation_key(tool_context)
        function_call_id = _as_string(_read(tool_context, "function_call_id"))
        if function_call_id != "":
            self._tool_run_ids[f"{invocation_key}:{function_call_id}"] = run_id
        else:
            self._tool_fallback_run_stacks.setdefault(invocation_key, []).append(run_id)

        llm_stack = self._llm_run_stacks.get(invocation_key, [])
        parent_run_id = llm_stack[-1] if llm_stack else None

        tool_name = _first_non_empty(_as_string(_read(tool, "name")), "framework_tool")
        metadata = _adk_context_metadata(tool_context)
        if function_call_id != "":
            metadata["event_id"] = function_call_id

        await _invoke_handler(
            self._agento11y_handler,
            "on_tool_start",
            {"name": tool_name, "description": _as_string(_read(tool, "description"))},
            _json_string(args),
            run_id=run_id,
            parent_run_id=parent_run_id,
            metadata=metadata,
            run_name=tool_name,
            inputs=args,
        )
        return None

    async def after_tool_callback(
        self, tool: Any, args: dict[str, Any], tool_context: Any, result: dict[str, Any]
    ) -> None:
        del tool, args
        invocation_key = self._invocation_key(tool_context)
        function_call_id = _as_string(_read(tool_context, "function_call_id"))
        if function_call_id != "":
            run_id = self._tool_run_ids.pop(f"{invocation_key}:{function_call_id}", None)
        else:
            run_id = self._pop_fallback_tool_run_id(invocation_key)
        if run_id is None:
            return None

        await _invoke_handler(self._agento11y_handler, "on_tool_end", result, run_id=run_id)
        return None

    async def on_tool_error_callback(
        self, tool: Any, args: dict[str, Any], tool_context: Any, error: Exception
    ) -> None:
        del tool, args
        invocation_key = self._invocation_key(tool_context)
        function_call_id = _as_string(_read(tool_context, "function_call_id"))
        if function_call_id != "":
            run_id = self._tool_run_ids.pop(f"{invocation_key}:{function_call_id}", None)
        else:
            run_id = self._pop_fallback_tool_run_id(invocation_key)
        if run_id is None:
            return None

        await _invoke_handler(self._agento11y_handler, "on_tool_error", error, run_id=run_id)
        return None

    def _invocation_key(self, callback_context: Any) -> str:
        return _invocation_key(callback_context)

    def _pop_llm_run_id(self, invocation_key: str) -> UUID | None:
        stack = self._llm_run_stacks.get(invocation_key, [])
        run_id = stack.pop() if stack else None
        if not stack:
            self._llm_run_stacks.pop(invocation_key, None)
        return run_id

    def _pop_fallback_tool_run_id(self, invocation_key: str) -> UUID | None:
        stack = self._tool_fallback_run_stacks.get(invocation_key, [])
        run_id = stack.pop() if stack else None
        if not stack:
            self._tool_fallback_run_stacks.pop(invocation_key, None)
        return run_id

    def _peek_llm_run_id(self, invocation_key: str) -> UUID | None:
        stack = self._llm_run_stacks.get(invocation_key, [])
        if not stack:
            return None
        return stack[-1]


class Agento11yGoogleAdkPlugin(BasePlugin):
    """Google ADK BasePlugin-compatible bridge that forwards plugin callbacks to Sigil."""

    name = "agento11y_google_adk_plugin"

    def __init__(self, agento11y_handler: Agento11yGoogleAdkHandler | Agento11yAsyncGoogleAdkHandler) -> None:
        self._agento11y_handler = agento11y_handler
        self._callbacks = Agento11yGoogleAdkCallbacks(agento11y_handler)
        self._run_ids: dict[str, UUID] = {}
        self._agent_run_stacks: dict[str, list[UUID]] = {}

    async def before_run_callback(self, *, invocation_context: Any) -> None:
        invocation_key = _invocation_key(invocation_context)
        run_id = uuid4()
        self._run_ids[invocation_key] = run_id
        run_name = _agent_name(invocation_context)
        await _invoke_handler(
            self._agento11y_handler,
            "on_chain_start",
            {"name": run_name},
            {},
            run_id=run_id,
            parent_run_id=None,
            metadata=_adk_context_metadata(invocation_context),
            run_type="invocation",
            run_name=run_name,
        )
        return None

    async def on_event_callback(self, *, invocation_context: Any, event: Any) -> None:
        invocation_key = _first_non_empty(
            _as_string(_read(event, "invocation_id")),
            _as_string(_read(event, "invocationId")),
            _invocation_key(invocation_context),
        )
        llm_run_id = self._callbacks._peek_llm_run_id(invocation_key)
        if llm_run_id is None:
            return None

        is_partial = _is_true(_read(event, "partial"))
        is_final = _is_true(_read(event, "turn_complete")) or _is_true(_read(event, "turnComplete"))
        if is_final or not is_partial:
            return None

        token = _adk_event_text(event)
        if token == "":
            return None

        await _invoke_handler(self._agento11y_handler, "on_llm_new_token", token, run_id=llm_run_id)
        return None

    async def after_run_callback(self, *, invocation_context: Any) -> None:
        invocation_key = _invocation_key(invocation_context)
        run_id = self._run_ids.pop(invocation_key, None)
        if run_id is None:
            return None
        self._callbacks._llm_run_stacks.pop(invocation_key, None)
        self._agent_run_stacks.pop(invocation_key, None)
        await _invoke_handler(self._agento11y_handler, "on_chain_end", {"status": "completed"}, run_id=run_id)
        return None

    async def before_agent_callback(self, *, callback_context: Any, agent: Any | None = None) -> None:
        invocation_context = _adk_invocation_context(callback_context)
        invocation_key = _invocation_key(invocation_context)
        run_id = uuid4()
        stack = self._agent_run_stacks.setdefault(invocation_key, [])
        parent_run_id = stack[-1] if stack else self._run_ids.get(invocation_key)
        stack.append(run_id)

        run_name = _agent_name(callback_context, agent)
        await _invoke_handler(
            self._agento11y_handler,
            "on_chain_start",
            {"name": run_name},
            {},
            run_id=run_id,
            parent_run_id=parent_run_id,
            metadata=_adk_context_metadata(callback_context),
            run_type="agent",
            run_name=run_name,
        )
        return None

    async def after_agent_callback(self, *, callback_context: Any, agent: Any | None = None) -> None:
        del agent
        invocation_key = _invocation_key(_adk_invocation_context(callback_context))
        stack = self._agent_run_stacks.get(invocation_key, [])
        run_id = stack.pop() if stack else None
        if run_id is None:
            return None
        if not stack:
            self._agent_run_stacks.pop(invocation_key, None)

        await _invoke_handler(self._agento11y_handler, "on_chain_end", {"status": "completed"}, run_id=run_id)
        return None

    async def before_model_callback(self, *, callback_context: Any, llm_request: Any) -> None:
        invocation_key = _invocation_key(_adk_invocation_context(callback_context))
        agent_stack = self._agent_run_stacks.get(invocation_key, [])
        parent_run_id = agent_stack[-1] if agent_stack else self._run_ids.get(invocation_key)
        await self._callbacks.before_model_callback(callback_context, llm_request, parent_run_id=parent_run_id)
        return None

    async def after_model_callback(self, *, callback_context: Any, llm_response: Any) -> None:
        await self._callbacks.after_model_callback(callback_context, llm_response)
        return None

    async def on_model_error_callback(self, *, callback_context: Any, llm_request: Any, error: Exception) -> None:
        await self._callbacks.on_model_error_callback(callback_context, llm_request, error)
        return None

    async def before_tool_callback(self, *, tool: Any, tool_args: dict[str, Any], tool_context: Any) -> None:
        await self._callbacks.before_tool_callback(tool, tool_args, tool_context)
        return None

    async def after_tool_callback(
        self,
        *,
        tool: Any,
        tool_args: dict[str, Any],
        tool_context: Any,
        result: dict[str, Any],
    ) -> None:
        await self._callbacks.after_tool_callback(tool, tool_args, tool_context, result)
        return None

    async def on_tool_error_callback(
        self,
        *,
        tool: Any,
        tool_args: dict[str, Any],
        tool_context: Any,
        error: Exception,
    ) -> None:
        await self._callbacks.on_tool_error_callback(tool, tool_args, tool_context, error)
        return None


def create_agento11y_google_adk_callbacks(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> Agento11yGoogleAdkCallbacks:
    """Create callback functions compatible with Google ADK agent callback fields."""
    agento11y_handler = create_agento11y_google_adk_handler(
        client=client,
        async_handler=async_handler,
        **handler_kwargs,
    )
    return Agento11yGoogleAdkCallbacks(agento11y_handler)


def create_agento11y_google_adk_plugin(
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> Agento11yGoogleAdkPlugin:
    """Create a Google ADK plugin instance wired to Sigil instrumentation."""
    agento11y_handler = create_agento11y_google_adk_handler(
        client=client,
        async_handler=async_handler,
        **handler_kwargs,
    )
    return Agento11yGoogleAdkPlugin(agento11y_handler)


def with_agento11y_google_adk_callbacks(
    config_or_agent: dict[str, Any] | Any | None,
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> dict[str, Any] | Any:
    """Attach Sigil to Google ADK callback fields on either config dicts or agent objects."""
    callbacks = create_agento11y_google_adk_callbacks(
        client=client,
        async_handler=async_handler,
        **handler_kwargs,
    )

    if config_or_agent is None or isinstance(config_or_agent, dict):
        merged = dict(config_or_agent or {})
        for field_name in _adk_callback_fields:
            merged[field_name] = _merge_callback_value(
                existing=merged.get(field_name),
                callback=getattr(callbacks, field_name),
                field_name=field_name,
            )
        return merged

    target = config_or_agent
    for field_name in _adk_callback_fields:
        existing = getattr(target, field_name, None)
        setattr(
            target,
            field_name,
            _merge_callback_value(
                existing=existing,
                callback=getattr(callbacks, field_name),
                field_name=field_name,
            ),
        )
    return target


def with_agento11y_google_adk_plugins(
    config_or_agent: dict[str, Any] | Any | None,
    *,
    client: Client,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> dict[str, Any] | Any:
    """Attach Sigil as a Google ADK plugin on either config dicts or agent-like objects."""
    plugin = create_agento11y_google_adk_plugin(
        client=client,
        async_handler=async_handler,
        **handler_kwargs,
    )

    if config_or_agent is None or isinstance(config_or_agent, dict):
        merged = dict(config_or_agent or {})
        plugins = _as_list(merged.get("plugins"))
        if not _contains_agento11y_plugin(plugins):
            plugins.append(plugin)
        merged["plugins"] = plugins
        return merged

    target = config_or_agent
    plugins = _as_list(getattr(target, "plugins", None))
    if not _contains_agento11y_plugin(plugins):
        plugins.append(plugin)
    target.plugins = plugins
    return target


async def _invoke_handler(handler: Any, method_name: str, *args: Any, **kwargs: Any) -> None:
    method = getattr(handler, method_name)
    result = method(*args, **kwargs)
    if inspect.isawaitable(result):
        await result


def _merge_callback_value(existing: Any, callback: Any, *, field_name: str) -> Any:
    if _contains_agento11y_callback(existing, field_name):
        return existing
    if existing is None:
        return callback
    if isinstance(existing, list):
        return [*existing, callback]
    if callable(existing):
        return [existing, callback]
    raise TypeError(f"google-adk `{field_name}` must be a callback or list of callbacks.")


def _contains_agento11y_callback(existing: Any, field_name: str) -> bool:
    callbacks = existing if isinstance(existing, list) else [existing]
    for callback in callbacks:
        if callback is None:
            continue
        instance = getattr(callback, "__self__", None)
        name = getattr(callback, "__name__", "")
        if isinstance(instance, Agento11yGoogleAdkCallbacks) and name == field_name:
            return True
    return False


def _contains_agento11y_plugin(plugins: list[Any]) -> bool:
    return any(isinstance(plugin, Agento11yGoogleAdkPlugin) for plugin in plugins)


def _as_list(value: Any) -> list[Any]:
    if isinstance(value, list):
        return list(value)
    if value is None:
        return []
    return [value]


def _invocation_key(callback_context: Any) -> str:
    invocation_context = _adk_invocation_context(callback_context)
    return _first_non_empty(
        _as_string(_read(callback_context, "invocation_id")),
        _as_string(_read(callback_context, "invocationId")),
        _as_string(_read(_read(callback_context, "session"), "id")),
        _as_string(_read(invocation_context, "invocation_id")),
        _as_string(_read(invocation_context, "invocationId")),
        _as_string(_read(_read(invocation_context, "session"), "id")),
        f"agento11y-google-adk-invocation:{id(callback_context)}",
    )


def _serialized_llm_payload(callback_context: Any, model_name: str) -> dict[str, Any]:
    serialized: dict[str, Any] = {"name": _agent_name(callback_context)}
    if model_name != "":
        serialized["kwargs"] = {"model": model_name}
    return serialized


def _adk_invocation_params(llm_request: Any) -> dict[str, Any]:
    config = _read(llm_request, "config")
    invocation_params: dict[str, Any] = {}

    model_name = _first_non_empty(
        _as_string(_read(llm_request, "model")),
        _as_string(_read(config, "model")),
    )
    if model_name != "":
        invocation_params["model"] = model_name

    system_prompt = _adk_content_text(_read(config, "system_instruction"))
    if system_prompt != "":
        invocation_params["system_prompt"] = system_prompt

    tools = _adk_tool_definitions(_read(config, "tools"), _read(llm_request, "tools_dict"))
    if tools:
        invocation_params["tools"] = tools

    max_tokens = _read(config, "max_output_tokens")
    if max_tokens is not None:
        invocation_params["max_tokens"] = max_tokens

    temperature = _read(config, "temperature")
    if temperature is not None:
        invocation_params["temperature"] = temperature

    top_p = _read(config, "top_p")
    if top_p is not None:
        invocation_params["top_p"] = top_p

    tool_choice = _adk_tool_choice(_read(config, "tool_config"))
    if tool_choice != "":
        invocation_params["tool_choice"] = tool_choice

    thinking_config = _read(config, "thinking_config")
    thinking_enabled = _bool_or_none(
        _first_non_none(
            _read(thinking_config, "include_thoughts"),
            _read(thinking_config, "includeThoughts"),
        )
    )
    if thinking_enabled is not None:
        invocation_params["thinking_enabled"] = thinking_enabled

    thinking_budget = _int_or_none(
        _first_non_none(
            _read(thinking_config, "thinking_budget"),
            _read(thinking_config, "thinkingBudget"),
        )
    )
    if thinking_budget is not None:
        invocation_params["thinking_budget"] = thinking_budget

    thinking_level = _adk_thinking_level(
        _first_non_none(
            _read(thinking_config, "thinking_level"),
            _read(thinking_config, "thinkingLevel"),
        )
    )
    if thinking_level != "":
        invocation_params["thinking_level"] = thinking_level

    return invocation_params


def _adk_tool_definitions(config_tools: Any, tools_dict: Any) -> list[ToolDefinition]:
    definitions: list[ToolDefinition] = []
    seen: set[tuple[str, str]] = set()

    def add(definition: ToolDefinition) -> None:
        key = (definition.type, definition.name)
        if definition.name == "" or key in seen:
            return
        seen.add(key)
        definitions.append(definition)

    for tool in _as_sequence(config_tools):
        for declaration in _as_sequence(_read(tool, "function_declarations")):
            name = _as_string(_read(declaration, "name"))
            if name == "":
                continue
            schema = _first_non_none(
                _read(declaration, "parameters_json_schema"),
                _read(declaration, "parameters"),
            )
            add(
                ToolDefinition(
                    name=name,
                    description=_as_string(_read(declaration, "description")),
                    type="function",
                    input_schema_json=_json_bytes(schema),
                )
            )

        for builtin_name in (
            "google_search",
            "google_search_retrieval",
            "code_execution",
            "url_context",
            "retrieval",
            "enterprise_web_search",
            "google_maps",
            "computer_use",
            "file_search",
        ):
            if _read(tool, builtin_name) is not None:
                add(ToolDefinition(name=builtin_name, type=builtin_name))

    if isinstance(tools_dict, dict):
        for key, tool in tools_dict.items():
            name = _first_non_empty(_as_string(_read(tool, "name")), _as_string(key))
            if name == "":
                continue
            add(
                ToolDefinition(
                    name=name,
                    description=_as_string(_read(tool, "description")),
                    type="function",
                )
            )

    return definitions


def _adk_tool_choice(tool_config: Any) -> str:
    function_calling_config = _read(tool_config, "function_calling_config")
    mode = _as_string(_jsonable(_read(function_calling_config, "mode"))).lower()
    allowed_names = _as_sequence(_read(function_calling_config, "allowed_function_names"))
    if len(allowed_names) == 1:
        return _as_string(allowed_names[0])
    return mode


def _adk_thinking_level(value: Any) -> str:
    normalized = _as_string(_jsonable(value)).lower()
    if normalized in {"", "thinking_level_unspecified"}:
        return ""
    if normalized in {"thinking_level_low", "low"}:
        return "low"
    if normalized in {"thinking_level_medium", "medium"}:
        return "medium"
    if normalized in {"thinking_level_high", "high"}:
        return "high"
    if normalized in {"thinking_level_minimal", "minimal"}:
        return "minimal"
    return normalized


def _adk_messages(contents: Any) -> list[dict[str, Any]]:
    if not isinstance(contents, list):
        return []
    messages: list[dict[str, Any]] = []
    for content in contents:
        message = _adk_content_message(content, default_role="user")
        if message is None:
            continue
        messages.append(message)
    return messages


def _adk_content_message(content: Any, *, default_role: str) -> dict[str, Any] | None:
    parts = _read(content, "parts")
    text = _content_parts_text(parts)
    tool_calls = _adk_function_calls(parts)
    tool_results = _adk_function_responses(parts)
    if text == "" and not tool_calls and not tool_results:
        return None

    role = _adk_role(_first_non_empty(_as_string(_read(content, "role")), default_role))
    if tool_results:
        role = "tool"

    message: dict[str, Any] = {
        "role": role,
    }
    if text != "":
        message["content"] = text
    if tool_calls:
        message["tool_calls"] = tool_calls
    if tool_results:
        message["tool_results"] = tool_results
    return message


def _content_parts_text(parts: Any) -> str:
    if not isinstance(parts, list):
        return ""
    text_parts: list[str] = []
    for part in parts:
        text = _first_non_empty(
            _as_string(_read(part, "text")),
            _as_string(_read(_read(part, "inline_data"), "display_name")),
            _as_string(_read(_read(part, "file_data"), "file_uri")),
        )
        if text != "":
            text_parts.append(text)
    return " ".join(text_parts).strip()


def _adk_function_calls(parts: Any) -> list[dict[str, Any]]:
    if not isinstance(parts, list):
        return []

    calls: list[dict[str, Any]] = []
    for part in parts:
        function_call = _read(part, "function_call")
        if function_call is None:
            continue

        name = _as_string(_read(function_call, "name"))
        if name == "":
            continue

        call: dict[str, Any] = {
            "id": _as_string(_read(function_call, "id")),
            "name": name,
        }
        args = _read(function_call, "args")
        if args is not None:
            call["args"] = _jsonable(args)
        calls.append(call)
    return calls


def _adk_function_responses(parts: Any) -> list[dict[str, Any]]:
    if not isinstance(parts, list):
        return []

    results: list[dict[str, Any]] = []
    for part in parts:
        function_response = _read(part, "function_response")
        if function_response is None:
            continue

        response = _jsonable(_read(function_response, "response"))
        result: dict[str, Any] = {
            "tool_call_id": _as_string(_read(function_response, "id")),
            "name": _as_string(_read(function_response, "name")),
            "response": response,
        }
        if isinstance(response, dict) and "error" in response:
            result["is_error"] = True
        results.append(result)
    return results


def _adk_llm_end_payload(llm_response: Any) -> dict[str, Any]:
    llm_output: dict[str, Any] = {}
    model_name = _first_non_empty(
        _as_string(_read(llm_response, "model_version")),
        _as_string(_read(_read(llm_response, "custom_metadata"), "model")),
    )
    if model_name != "":
        llm_output["model_name"] = model_name

    finish_reason = _as_string(_read(llm_response, "finish_reason"))
    if finish_reason != "":
        llm_output["finish_reason"] = finish_reason

    usage_metadata = _read(llm_response, "usage_metadata")
    usage = _adk_usage_metadata(usage_metadata)
    if usage:
        llm_output["usage"] = usage

    payload: dict[str, Any] = {"llm_output": llm_output}
    message = _adk_content_message(_read(llm_response, "content"), default_role="assistant")
    if message is not None:
        payload["generations"] = [[{"message": message}]]
    return payload


def _adk_usage_metadata(usage_metadata: Any) -> dict[str, int]:
    fields = (
        "prompt_token_count",
        "candidates_token_count",
        "total_token_count",
        "cached_content_token_count",
        "cache_write_input_token_count",
        "cache_creation_input_token_count",
        "thoughts_token_count",
        "tool_use_prompt_token_count",
    )
    token_usage: dict[str, int] = {}
    for field_name in fields:
        value = _int_or_none(_read(usage_metadata, field_name))
        if value is not None:
            token_usage[field_name] = value
    return token_usage


def _adk_context_metadata(callback_context: Any) -> dict[str, Any]:
    invocation_context = _adk_invocation_context(callback_context)
    metadata: dict[str, Any] = {}
    session_id = _first_non_empty(
        _as_string(_read(callback_context, "session_id")),
        _as_string(_read(_read(callback_context, "session"), "id")),
        _as_string(_read(invocation_context, "session_id")),
        _as_string(_read(_read(invocation_context, "session"), "id")),
    )
    if session_id != "":
        metadata["conversation_id"] = session_id
        metadata["session_id"] = session_id

    thread_id = _first_non_empty(
        _as_string(_read(callback_context, "thread_id")),
        _as_string(_read(_read(_read(callback_context, "session"), "state"), "thread_id")),
        _as_string(_read(invocation_context, "thread_id")),
        _as_string(_read(_read(_read(invocation_context, "session"), "state"), "thread_id")),
    )
    if thread_id != "":
        metadata["thread_id"] = thread_id

    invocation_id = _first_non_empty(
        _as_string(_read(callback_context, "invocation_id")),
        _as_string(_read(callback_context, "invocationId")),
        _as_string(_read(invocation_context, "invocation_id")),
        _as_string(_read(invocation_context, "invocationId")),
    )
    if invocation_id != "":
        metadata["event_id"] = invocation_id
    return metadata


def _agent_name(callback_context: Any, agent: Any | None = None) -> str:
    invocation_context = _adk_invocation_context(callback_context)
    return _first_non_empty(
        _as_string(_read(callback_context, "agent_name")),
        _as_string(_read(callback_context, "agentName")),
        _as_string(_read(_read(callback_context, "agent"), "name")),
        _as_string(_read(agent, "name")),
        _as_string(_read(invocation_context, "agent_name")),
        _as_string(_read(invocation_context, "agentName")),
        _as_string(_read(_read(invocation_context, "agent"), "name")),
        "google_adk_agent",
    )


def _adk_invocation_context(callback_context: Any) -> Any:
    return _first_non_none(
        _read(callback_context, "invocation_context"),
        _read(callback_context, "invocationContext"),
        callback_context,
    )


def _adk_event_text(event: Any) -> str:
    return _first_non_empty(
        _adk_content_text(_read(event, "content")),
        _adk_content_text(_read(event, "partial")),
        _as_string(_read(event, "text")),
        _as_string(_read(event, "delta")),
    )


def _adk_content_text(content: Any) -> str:
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        return " ".join(text for text in (_adk_content_text(item) for item in content) if text != "").strip()
    parts_text = _content_parts_text(_read(content, "parts"))
    if parts_text != "":
        return parts_text
    return _as_string(_read(content, "text"))


def _adk_role(role: str) -> str:
    normalized = role.strip().lower()
    if normalized == "model":
        return "assistant"
    if normalized in {"function", "tool"}:
        return "tool"
    if normalized != "":
        return normalized
    return "user"


def _json_string(value: Any) -> str:
    try:
        return json.dumps(value, default=str, sort_keys=True)
    except Exception:
        return str(value)


def _json_bytes(value: Any) -> bytes:
    if value is None:
        return b""
    try:
        return json.dumps(_jsonable(value), default=str, sort_keys=True).encode("utf-8")
    except Exception:
        return b""


def _jsonable(value: Any) -> Any:
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if isinstance(value, dict):
        return {str(key): _jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(item) for item in value]
    enum_value = getattr(value, "value", None)
    if enum_value is not None and not callable(enum_value):
        return enum_value
    model_dump = getattr(value, "model_dump", None)
    if callable(model_dump):
        try:
            return model_dump(by_alias=True, exclude_none=True, mode="json")
        except TypeError:
            return model_dump()
    return value


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
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    return None


def _bool_or_none(value: Any) -> bool | None:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        normalized = value.strip().lower()
        if normalized in {"1", "true", "yes", "on"}:
            return True
        if normalized in {"0", "false", "no", "off"}:
            return False
    return None


def _as_sequence(value: Any) -> list[Any]:
    if isinstance(value, list):
        return value
    if isinstance(value, tuple):
        return list(value)
    return []


def _is_true(value: Any) -> bool:
    return value is True


def _first_non_none(*values: Any) -> Any:
    for value in values:
        if value is not None:
            return value
    return None


def _first_non_empty(*values: str) -> str:
    for value in values:
        if value != "":
            return value
    return ""


__all__ = [
    "Agento11yGoogleAdkHandler",
    "Agento11yAsyncGoogleAdkHandler",
    "Agento11yGoogleAdkCallbacks",
    "Agento11yGoogleAdkPlugin",
    "create_agento11y_google_adk_handler",
    "create_agento11y_google_adk_callbacks",
    "create_agento11y_google_adk_plugin",
    "with_agento11y_google_adk_callbacks",
    "with_agento11y_google_adk_plugins",
]
