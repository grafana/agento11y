"""Microsoft Agent Framework Foundry middleware handlers."""

from __future__ import annotations

import json
from collections.abc import Awaitable, Callable, Mapping, Sequence
from typing import Any
from uuid import UUID, uuid4

from sigil_sdk import Client
from sigil_sdk.framework_handler import ProviderResolver, SigilFrameworkHandlerBase

try:  # pragma: no cover - imported from agent-framework at runtime
    from agent_framework import (
        AgentContext,
        AgentMiddleware,
        ChatContext,
        ChatMiddleware,
        FunctionInvocationContext,
        FunctionMiddleware,
    )
except Exception:  # pragma: no cover - lightweight fallback for local unit tests without dependency

    class AgentMiddleware:  # type: ignore[no-redef]
        pass

    class ChatMiddleware:  # type: ignore[no-redef]
        pass

    class FunctionMiddleware:  # type: ignore[no-redef]
        pass

    AgentContext = ChatContext = FunctionInvocationContext = Any  # type: ignore[assignment]


_framework_name = "agent-framework-foundry"
_framework_source = "middleware"
_framework_language = "python"
_framework_instrumentation_name = "github.com/grafana/sigil/sdks/python-frameworks/agent-framework-foundry"
_sigil_agent_run_id_key = "sigil_agent_framework_foundry.agent_run_id"


class SigilAgentFrameworkFoundryHandler(SigilFrameworkHandlerBase):
    """Shared lifecycle mapper for Microsoft Agent Framework Foundry middleware."""

    def __init__(
        self,
        *,
        client: Client,
        agent_name: str = "",
        agent_version: str = "",
        conversation_id: str = "",
        provider_resolver: ProviderResolver = "auto",
        provider: str = "azure_foundry",
        capture_inputs: bool = True,
        capture_outputs: bool = True,
        extra_tags: dict[str, str] | None = None,
        extra_metadata: dict[str, Any] | None = None,
    ) -> None:
        metadata = dict(extra_metadata or {})
        if conversation_id:
            metadata.setdefault("conversation_id", conversation_id)
        super().__init__(
            client=client,
            framework_name=_framework_name,
            framework_source=_framework_source,
            framework_language=_framework_language,
            framework_instrumentation_name=_framework_instrumentation_name,
            agent_name=agent_name,
            agent_version=agent_version,
            provider_resolver=provider_resolver,
            provider=provider,
            capture_inputs=capture_inputs,
            capture_outputs=capture_outputs,
            extra_tags=extra_tags,
            extra_metadata=metadata,
        )
        self._default_conversation_id = conversation_id

    def set_default_agent_name(self, agent_name: str) -> None:
        if self._agent_name.strip() == "" and agent_name.strip() != "":
            self._agent_name = agent_name.strip()

    def on_agent_start(self, context: AgentContext, run_id: UUID) -> None:
        agent_name = _agent_name(_read(context, "agent"))
        self.set_default_agent_name(agent_name)
        self._on_chain_start(
            serialized={"name": agent_name, "provider": "microsoft-foundry"},
            inputs={"messages": [_message_to_dict(message) for message in _as_list(_read(context, "messages"))]},
            run_id=run_id,
            parent_run_id=None,
            run_type=_agent_run_type(_read(context, "agent")),
            callback_kwargs={
                "metadata": _context_metadata(context, default_conversation_id=self._default_conversation_id),
                "run_name": agent_name,
            },
        )

    def on_agent_end(self, run_id: UUID) -> None:
        self._on_chain_end(run_id=run_id)

    def on_agent_error(self, error: BaseException, run_id: UUID) -> None:
        self._on_chain_error(error=error, run_id=run_id)

    def on_chat_start(self, context: ChatContext, run_id: UUID, parent_run_id: UUID | None = None) -> None:
        model_name = _chat_model_name(context)
        client = _read(context, "client")
        self._on_chat_model_start(
            serialized={
                "name": client.__class__.__name__ if client is not None else "FoundryChatClient",
                "kwargs": {"model": model_name, "provider": "microsoft-foundry"},
            },
            messages=[[_message_to_dict(message) for message in _as_list(_read(context, "messages"))]],
            run_id=run_id,
            parent_run_id=parent_run_id,
            invocation_params=_chat_invocation_params(context, model_name=model_name),
            callback_kwargs={
                "metadata": _context_metadata(context, default_conversation_id=self._default_conversation_id),
                "run_name": "foundry_chat",
            },
        )

    def on_chat_token(self, token: str, run_id: UUID) -> None:
        self._on_llm_new_token(token=token, run_id=run_id)

    def on_chat_end(self, response: Any, run_id: UUID) -> None:
        self._on_llm_end(response=_chat_end_payload(response), run_id=run_id)

    def on_chat_error(self, error: BaseException, run_id: UUID) -> None:
        self._on_llm_error(error=error, run_id=run_id)

    def on_function_start(self, context: FunctionInvocationContext, run_id: UUID) -> None:
        function = _read(context, "function")
        tool_name = _first_non_empty(_as_string(_read(function, "name")), function.__class__.__name__)
        self._on_tool_start(
            serialized={"name": tool_name, "description": _as_string(_read(function, "description"))},
            input_str=_json_string(_read(context, "arguments")),
            run_id=run_id,
            parent_run_id=None,
            callback_kwargs={
                "metadata": _context_metadata(context, default_conversation_id=self._default_conversation_id),
                "run_name": tool_name,
                "inputs": _read(context, "arguments"),
            },
        )

    def on_function_end(self, result: Any, run_id: UUID) -> None:
        self._on_tool_end(output=result, run_id=run_id)

    def on_function_error(self, error: BaseException, run_id: UUID) -> None:
        self._on_tool_error(error=error, run_id=run_id)


class SigilAgentFrameworkFoundryAgentMiddleware(AgentMiddleware):
    """Agent middleware for Agent.run and FoundryAgent.run lifecycle spans."""

    def __init__(self, handler: SigilAgentFrameworkFoundryHandler) -> None:
        self._handler = handler

    async def process(self, context: AgentContext, call_next: Callable[[], Awaitable[None]]) -> None:
        run_id = uuid4()
        context.client_kwargs[_sigil_agent_run_id_key] = str(run_id)
        self._handler.on_agent_start(context, run_id)

        try:
            if _read(context, "stream"):
                context.stream_result_hooks.append(lambda _response: self._agent_stream_result(_response, run_id))
                context.stream_cleanup_hooks.append(lambda: self._agent_stream_cleanup(run_id))
                await call_next()
            else:
                await call_next()
                self._handler.on_agent_end(run_id)
        except Exception as exc:
            self._handler.on_agent_error(exc, run_id)
            raise

    def _agent_stream_result(self, response: Any, run_id: UUID) -> Any:
        self._handler.on_agent_end(run_id)
        return response

    def _agent_stream_cleanup(self, run_id: UUID) -> None:
        del run_id
        return None


class SigilAgentFrameworkFoundryChatMiddleware(ChatMiddleware):
    """Chat middleware that records FoundryChatClient calls as Sigil generations."""

    def __init__(self, handler: SigilAgentFrameworkFoundryHandler) -> None:
        self._handler = handler

    async def process(self, context: ChatContext, call_next: Callable[[], Awaitable[None]]) -> None:
        run_id = uuid4()
        parent_run_id = _uuid_or_none(_read(_read(context, "kwargs"), _sigil_agent_run_id_key))
        self._handler.on_chat_start(context, run_id, parent_run_id=parent_run_id)

        try:
            if _read(context, "stream"):
                context.stream_transform_hooks.append(lambda update: self._stream_update(update, run_id))
                context.stream_result_hooks.append(lambda response: self._stream_result(response, run_id))
                await call_next()
            else:
                await call_next()
                self._handler.on_chat_end(_read(context, "result"), run_id)
        except Exception as exc:
            self._handler.on_chat_error(exc, run_id)
            raise

    def _stream_update(self, update: Any, run_id: UUID) -> Any:
        text = _as_string(_read(update, "text"))
        if text:
            self._handler.on_chat_token(text, run_id)
        return update

    def _stream_result(self, response: Any, run_id: UUID) -> Any:
        self._handler.on_chat_end(response, run_id)
        return response


class SigilAgentFrameworkFoundryFunctionMiddleware(FunctionMiddleware):
    """Function middleware that records Agent Framework local tools."""

    def __init__(self, handler: SigilAgentFrameworkFoundryHandler) -> None:
        self._handler = handler

    async def process(self, context: FunctionInvocationContext, call_next: Callable[[], Awaitable[None]]) -> None:
        run_id = uuid4()
        self._handler.on_function_start(context, run_id)
        try:
            await call_next()
            self._handler.on_function_end(_read(context, "result"), run_id)
        except Exception as exc:
            self._handler.on_function_error(exc, run_id)
            raise


def _message_to_dict(message: Any) -> dict[str, Any]:
    return {
        "role": _as_string(_read(message, "role")) or "user",
        "content": _message_text(message),
    }


def _message_text(message: Any) -> str:
    text = _as_string(_read(message, "text"))
    if text:
        return text
    content = _read(message, "content")
    if isinstance(content, str):
        return content
    contents = _read(message, "contents")
    if isinstance(contents, Sequence) and not isinstance(contents, (str, bytes)):
        return " ".join(_content_text(item) for item in contents if _content_text(item))
    return ""


def _content_text(content: Any) -> str:
    if isinstance(content, str):
        return content
    return _as_string(_read(content, "text"))


def _chat_end_payload(response: Any) -> dict[str, Any]:
    text = _message_text(response)
    if not text:
        text = _as_string(_read(response, "text"))
    return {
        "generations": [[{"text": text, "message": {"role": "assistant", "content": text}}]],
        "llm_output": {
            "model_name": _as_string(_read(response, "model")),
            "finish_reason": _as_string(_read(response, "finish_reason")),
            "usage": _usage_dict(_read(response, "usage_details")),
        },
    }


def _usage_dict(usage: Any) -> dict[str, int]:
    if usage is None:
        return {}
    return {
        "input_tokens": _as_int(_read(usage, "input_token_count")),
        "output_tokens": _as_int(_read(usage, "output_token_count")),
        "total_tokens": _as_int(_read(usage, "total_token_count")),
        "cache_read_input_tokens": _as_int(_read(usage, "cache_read_input_token_count")),
        "cache_write_input_tokens": _as_int(_read(usage, "cache_creation_input_token_count")),
        "reasoning_tokens": _as_int(_read(usage, "reasoning_output_token_count")),
    }


def _chat_invocation_params(context: ChatContext, *, model_name: str) -> dict[str, Any]:
    options = dict(_read(context, "options") or {})
    kwargs = dict(_read(context, "kwargs") or {})
    return {
        **options,
        "model": model_name,
        "stream": bool(_read(context, "stream")),
        "project_endpoint": _project_endpoint(_read(context, "client")),
        **{key: value for key, value in kwargs.items() if key.startswith("sigil_")},
    }


def _chat_model_name(context: ChatContext) -> str:
    options = _read(context, "options") or {}
    client = _read(context, "client")
    return _first_non_empty(
        _as_string(_read(options, "model")),
        _as_string(_read(client, "model")),
        _as_string(_read(client, "_model")),
        _as_string(_read(client, "deployment_name")),
        _as_string(_read(client, "_deployment_name")),
    )


def _project_endpoint(client: Any) -> str:
    return _first_non_empty(
        _as_string(_read(client, "project_endpoint")),
        _as_string(_read(client, "service_url")),
        _as_string(_read(client, "_project_endpoint")),
        _as_string(_read(client, "_service_url")),
    )


def _context_metadata(context: Any, *, default_conversation_id: str) -> dict[str, Any]:
    metadata: dict[str, Any] = {}
    for source_name in ("metadata", "options", "kwargs", "function_invocation_kwargs", "client_kwargs"):
        source = _read(context, source_name)
        if isinstance(source, Mapping):
            metadata.update(_safe_metadata(source))

    session = _read(context, "session")
    session_id = _as_string(_read(session, "session_id"))
    service_session_id = _as_string(_read(session, "service_session_id"))
    if session_id:
        metadata.setdefault("session_id", session_id)
        metadata.setdefault("thread_id", session_id)
    if service_session_id:
        metadata.setdefault("service_session_id", service_session_id)
        metadata.setdefault("conversation_id", service_session_id)
    if default_conversation_id:
        metadata.setdefault("conversation_id", default_conversation_id)
    return metadata


def _safe_metadata(value: Mapping[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, item in value.items():
        if isinstance(key, str) and _metadata_allowed(key):
            out[key] = item
    return out


def _metadata_allowed(key: str) -> bool:
    return key in {
        "conversation_id",
        "thread_id",
        "event_id",
        "session_id",
        "service_session_id",
        "project_endpoint",
        "agent_name",
        "agent_version",
    } or key.startswith("sigil.")


def _agent_name(agent: Any) -> str:
    return _first_non_empty(
        _as_string(_read(agent, "name")),
        _as_string(_read(agent, "agent_name")),
        agent.__class__.__name__ if agent is not None else "",
    )


def _agent_run_type(agent: Any) -> str:
    class_name = agent.__class__.__name__.lower() if agent is not None else ""
    if "foundryagent" in class_name:
        return "foundry_agent"
    return "agent"


def _json_string(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    try:
        return json.dumps(value, default=str, sort_keys=True)
    except TypeError:
        return str(value)


def _uuid_or_none(value: Any) -> UUID | None:
    text = _as_string(value)
    if not text:
        return None
    try:
        return UUID(text)
    except ValueError:
        return None


def _first_non_empty(*values: str) -> str:
    for value in values:
        if value:
            return value
    return ""


def _as_list(value: Any) -> list[Any]:
    if isinstance(value, list):
        return value
    if isinstance(value, tuple):
        return list(value)
    return []


def _read(value: Any, key: str) -> Any:
    if value is None:
        return None
    if isinstance(value, Mapping):
        return value.get(key)
    return getattr(value, key, None)


def _as_string(value: Any) -> str:
    if isinstance(value, str):
        return value.strip()
    return ""


def _as_int(value: Any) -> int:
    if isinstance(value, bool) or value is None:
        return 0
    if isinstance(value, int):
        return value
    if isinstance(value, float) and value.is_integer():
        return int(value)
    if isinstance(value, str):
        try:
            return int(value.strip())
        except ValueError:
            return 0
    return 0
