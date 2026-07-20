"""Strands Agents hook handlers for Sigil generation recording."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any
from uuid import UUID

from agento11y import (
    Client,
    Generation,
    GenerationMode,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    TokenUsage,
    ToolCall,
    ToolResult,
)
from agento11y.framework_handler import ProviderResolver, SigilFrameworkHandlerBase, merge_framework_callback_kwargs
from agento11y.usage import map_usage

_framework_name = "strands"
_framework_source = "hooks"
_framework_language = "python"
_framework_instrumentation_name = "github.com/grafana/sigil/sdks/python-frameworks/strands"
_metadata_run_id = "sigil.framework.run_id"
_metadata_thread_id = "sigil.framework.thread_id"
_metadata_parent_run_id = "sigil.framework.parent_run_id"
_metadata_component_name = "sigil.framework.component_name"
_metadata_run_type = "sigil.framework.run_type"
_metadata_event_id = "sigil.framework.event_id"


@dataclass(slots=True)
class _StrandsRunState:
    recorder: Any
    input_messages: list[Message]
    capture_outputs: bool
    output_chunks: list[str] = field(default_factory=list)
    first_token_recorded: bool = False


class SigilStrandsHandler(SigilFrameworkHandlerBase):
    """Sigil framework handler for Strands Agents lifecycle hooks."""

    def __init__(
        self,
        *,
        client: Client,
        agent_name: str = "",
        agent_version: str = "",
        provider_resolver: ProviderResolver = "auto",
        provider: str = "",
        capture_inputs: bool = True,
        capture_outputs: bool = True,
        extra_tags: dict[str, str] | None = None,
        extra_metadata: dict[str, Any] | None = None,
    ) -> None:
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
            extra_metadata=extra_metadata,
        )
        self._strands_runs: dict[str, _StrandsRunState] = {}

    def set_default_agent_name(self, agent_name: str) -> None:
        """Set the generation agent name from Strands when the caller did not configure one."""
        if self._agent_name.strip() == "" and agent_name.strip() != "":
            self._agent_name = agent_name.strip()

    def on_chat_model_start(
        self,
        serialized: dict[str, Any] | None,
        messages: list[list[Any]],
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        invocation_params: dict[str, Any] | None = None,
        run_name: str | None = None,
        **kwargs: Any,
    ) -> None:
        run_key = str(run_id)
        if run_key in self._strands_runs:
            return

        callback_kwargs = merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name)
        invocation_params = invocation_params or {}
        model_name = _first_non_empty(
            _as_string(_read(invocation_params, "model")),
            _as_string(_read(_read(serialized, "kwargs"), "model")),
        )
        provider_name = _resolve_provider_name(self._provider, self._provider_resolver, model_name, invocation_params)
        conversation_id, thread_id = _resolve_conversation_id(
            framework_name=self._framework_name,
            run_key=run_key,
            callback_kwargs=callback_kwargs,
        )

        metadata_payload: dict[str, Any] = dict(self._extra_metadata)
        metadata_payload[_metadata_run_id] = run_key
        metadata_payload[_metadata_run_type] = "chat"
        parent_run_key = _as_string(parent_run_id)
        if thread_id != "":
            metadata_payload[_metadata_thread_id] = thread_id
        if parent_run_key != "":
            metadata_payload[_metadata_parent_run_id] = parent_run_key
        component_name = _first_non_empty(_as_string(run_name), _as_string(_read(serialized, "name")))
        if component_name != "":
            metadata_payload[_metadata_component_name] = component_name
        event_id = _event_id_from_payload(callback_kwargs)
        if event_id != "":
            metadata_payload[_metadata_event_id] = event_id

        tags_payload = dict(self._extra_tags)
        tags_payload["sigil.framework.name"] = self._framework_name
        tags_payload["sigil.framework.source"] = self._framework_source
        tags_payload["sigil.framework.language"] = self._framework_language

        start = GenerationStart(
            conversation_id=conversation_id,
            agent_name=self._agent_name,
            agent_version=self._agent_version,
            mode=GenerationMode.STREAM,
            model=ModelRef(provider=provider_name, name=model_name),
            tags=tags_payload,
            metadata=metadata_payload,
            system_prompt=_as_string(_read(invocation_params, "system_prompt")),
            tools=list(_read(invocation_params, "tools") or []),
            temperature=_optional_float(_read(invocation_params, "temperature")),
            max_tokens=_optional_int(_read(invocation_params, "max_tokens")),
            top_p=_optional_float(_read(invocation_params, "top_p")),
            tool_choice=_as_string(_read(invocation_params, "tool_choice")) or None,
        )
        recorder = self._client.start_streaming_generation(start)
        self._strands_runs[run_key] = _StrandsRunState(
            recorder=recorder,
            input_messages=_map_chat_inputs(messages) if self._capture_inputs else [],
            capture_outputs=self._capture_outputs,
        )

    def on_llm_new_token(self, token: str, *, run_id: UUID, **_kwargs: Any) -> None:
        run_state = self._strands_runs.get(str(run_id))
        if run_state is None or token.strip() == "":
            return
        if run_state.capture_outputs:
            run_state.output_chunks.append(token)
        if not run_state.first_token_recorded:
            run_state.first_token_recorded = True
            run_state.recorder.set_first_token_at(datetime.now(timezone.utc))

    def on_llm_end(self, response: Any, *, run_id: UUID, **_kwargs: Any) -> None:
        run_state = self._strands_runs.pop(str(run_id), None)
        if run_state is None:
            return

        try:
            llm_output = _read(response, "llm_output")
            output_messages: list[Message] = []
            if run_state.capture_outputs:
                output_messages = _map_output_messages(response)
                if not output_messages and run_state.output_chunks:
                    output_messages = [
                        Message(
                            role=MessageRole.ASSISTANT,
                            parts=[Part(kind=PartKind.TEXT, text="".join(run_state.output_chunks))],
                        )
                    ]

            run_state.recorder.set_result(
                Generation(
                    input=run_state.input_messages,
                    output=output_messages,
                    usage=_usage_from_llm_output(llm_output),
                    response_model=_as_string(_read(llm_output, "model_name")),
                    stop_reason=_as_string(_read(llm_output, "finish_reason") or _read(llm_output, "stop_reason")),
                )
            )
        finally:
            run_state.recorder.end()

        recorder_error = run_state.recorder.err()
        if recorder_error is not None:
            raise recorder_error

    def on_llm_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        run_state = self._strands_runs.pop(str(run_id), None)
        if run_state is None:
            return

        try:
            run_state.recorder.set_call_error(Exception(str(error)))
            if run_state.capture_outputs and run_state.output_chunks:
                run_state.recorder.set_result(
                    Generation(
                        input=run_state.input_messages,
                        output=[
                            Message(
                                role=MessageRole.ASSISTANT,
                                parts=[Part(kind=PartKind.TEXT, text="".join(run_state.output_chunks))],
                            )
                        ],
                    )
                )
        finally:
            run_state.recorder.end()

        recorder_error = run_state.recorder.err()
        if recorder_error is not None:
            raise recorder_error

    def on_tool_start(
        self,
        serialized: dict[str, Any] | None,
        input_str: str,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        run_name: str | None = None,
        **kwargs: Any,
    ) -> None:
        self._on_tool_start(
            serialized=serialized,
            input_str=input_str,
            run_id=run_id,
            parent_run_id=parent_run_id,
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    def on_tool_end(self, output: Any, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_end(output=output, run_id=run_id)

    def on_tool_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_tool_error(error=error, run_id=run_id)

    def on_chain_start(
        self,
        serialized: dict[str, Any] | None,
        _inputs: dict[str, Any] | None,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        run_type: str | None = None,
        run_name: str | None = None,
        **kwargs: Any,
    ) -> None:
        self._on_chain_start(
            serialized=serialized,
            run_id=run_id,
            parent_run_id=parent_run_id,
            run_type=run_type or "chain",
            callback_kwargs=merge_framework_callback_kwargs(kwargs, tags=tags, metadata=metadata, run_name=run_name),
        )

    def on_chain_end(self, _outputs: dict[str, Any] | None, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_chain_end(run_id=run_id)

    def on_chain_error(self, error: BaseException, *, run_id: UUID, **_kwargs: Any) -> None:
        self._on_chain_error(error=error, run_id=run_id)


def _map_chat_inputs(messages: list[list[Any]]) -> list[Message]:
    output: list[Message] = []
    for batch in messages:
        for message in batch:
            mapped = _map_message(message)
            if mapped is not None:
                output.append(mapped)
    return output


def _map_output_messages(response: Any) -> list[Message]:
    output: list[Message] = []
    text_chunks: list[str] = []
    for candidates in _as_list(_read(response, "generations")):
        for candidate in _as_list(candidates):
            text = _as_string(_read(candidate, "text"))
            if text != "":
                text_chunks.append(text)
                continue
            mapped = _map_message(_read(candidate, "message"))
            if mapped is not None:
                output.append(mapped)

    if text_chunks:
        output.insert(
            0,
            Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="\n".join(text_chunks))]),
        )
    if output:
        return output

    fallback = _as_string(_read(response, "text"))
    if fallback != "":
        return [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text=fallback)])]
    return []


def _map_message(message: Any) -> Message | None:
    role = _normalize_role(_as_string(_read(message, "role")) or _as_string(_read(message, "type")))
    content = _read(message, "content")
    parts: list[Part] = []
    contains_tool_result = False

    if isinstance(content, str):
        text = content.strip()
        if text != "":
            parts.append(Part(kind=PartKind.TEXT, text=text))
    elif isinstance(content, list):
        for item in content:
            if isinstance(item, str):
                text = item.strip()
                if text != "":
                    parts.append(Part(kind=PartKind.TEXT, text=text))
                continue

            text = _as_string(_read(item, "text"))
            if text != "":
                parts.append(Part(kind=PartKind.TEXT, text=text))
                continue

            tool_use = _map_tool_use(_read(item, "toolUse"))
            if tool_use is not None:
                parts.append(tool_use)
                continue

            tool_result = _map_tool_result(_read(item, "toolResult"))
            if tool_result is not None:
                contains_tool_result = True
                parts.append(tool_result)
    elif isinstance(content, dict):
        text = _as_string(_read(content, "text"))
        if text != "":
            parts.append(Part(kind=PartKind.TEXT, text=text))

    if not parts:
        return None

    if contains_tool_result:
        role = MessageRole.TOOL
        parts = [part for part in parts if part.kind == PartKind.TOOL_RESULT]

    return Message(role=role, parts=parts)


def _map_tool_use(tool_use: Any) -> Part | None:
    if tool_use is None:
        return None
    name = _as_string(_read(tool_use, "name"))
    if name == "":
        return None
    return Part(
        kind=PartKind.TOOL_CALL,
        tool_call=ToolCall(
            id=_as_string(_read(tool_use, "toolUseId")),
            name=name,
            input_json=_json_bytes(_read(tool_use, "input")),
        ),
    )


def _map_tool_result(tool_result: Any) -> Part | None:
    if tool_result is None:
        return None
    return Part(
        kind=PartKind.TOOL_RESULT,
        tool_result=ToolResult(
            tool_call_id=_as_string(_read(tool_result, "toolUseId")),
            content=_tool_result_text(_read(tool_result, "content")),
            content_json=_json_bytes(_read(tool_result, "content")),
            is_error=_as_string(_read(tool_result, "status")).lower() == "error",
        ),
    )


def _usage_from_llm_output(llm_output: Any) -> TokenUsage:
    return map_usage(_read(llm_output, "token_usage") or _read(llm_output, "usage"))


def _tool_result_text(content: Any) -> str:
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


def _resolve_provider_name(
    explicit_provider: str,
    provider_resolver: ProviderResolver,
    model_name: str,
    invocation_params: dict[str, Any],
) -> str:
    if explicit_provider.strip() != "":
        return explicit_provider.strip()

    provider = _as_string(_read(invocation_params, "provider"))
    if provider != "":
        return provider

    if callable(provider_resolver):
        resolved = provider_resolver(model_name, None, invocation_params)
        if resolved.strip() != "":
            return resolved.strip()
        return "custom"

    if provider_resolver == "none":
        return ""
    if provider_resolver not in {"", "auto"}:
        return str(provider_resolver)

    lower = model_name.lower()
    if lower.startswith("gpt-") or lower.startswith("o1") or lower.startswith("o3") or lower.startswith("o4"):
        return "openai"
    if "claude" in lower:
        return "anthropic"
    if "gemini" in lower:
        return "gemini"
    return "custom" if model_name != "" else ""


def _resolve_conversation_id(*, framework_name: str, run_key: str, callback_kwargs: dict[str, Any]) -> tuple[str, str]:
    metadata = _read(callback_kwargs, "metadata")
    conversation_id = _first_non_empty(
        _as_string(_read(metadata, "conversation_id")),
        _as_string(_read(metadata, "conversationId")),
        _as_string(_read(metadata, "session_id")),
        _as_string(_read(metadata, "sessionId")),
        _as_string(_read(metadata, "group_id")),
        _as_string(_read(metadata, "groupId")),
    )
    thread_id = _first_non_empty(_as_string(_read(metadata, "thread_id")), _as_string(_read(metadata, "threadId")))
    if conversation_id != "":
        return conversation_id, thread_id
    if thread_id != "":
        return thread_id, thread_id
    return f"sigil:framework:{framework_name}:{run_key}", ""


def _event_id_from_payload(payload: Any) -> str:
    metadata = _read(payload, "metadata")
    return _first_non_empty(
        _as_string(_read(payload, "event_id")),
        _as_string(_read(payload, "eventId")),
        _as_string(_read(metadata, "event_id")),
        _as_string(_read(metadata, "eventId")),
        _as_string(_read(metadata, "invocation_id")),
        _as_string(_read(metadata, "invocationId")),
    )


def _normalize_role(role: str) -> MessageRole:
    normalized = role.strip().lower()
    if normalized in {"assistant", "ai"}:
        return MessageRole.ASSISTANT
    if normalized == "tool":
        return MessageRole.TOOL
    return MessageRole.USER


def _as_list(value: Any) -> list[Any]:
    if isinstance(value, list):
        return value
    return []


def _optional_int(value: Any) -> int | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(value)
    if isinstance(value, str):
        try:
            return int(value.strip())
        except ValueError:
            return None
    return None


def _optional_float(value: Any) -> float | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int | float):
        return float(value)
    if isinstance(value, str):
        try:
            return float(value.strip())
        except ValueError:
            return None
    return None


def _json_bytes(value: Any) -> bytes:
    if value is None:
        return b""
    try:
        return json.dumps(value, default=str, sort_keys=True).encode("utf-8")
    except Exception:
        return b""


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


def _first_non_empty(*values: str) -> str:
    for value in values:
        if value != "":
            return value
    return ""
