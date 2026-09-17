"""Hermes native plugin for Grafana Agent Observability."""
from __future__ import annotations

import json
import logging
import os
import threading
from typing import Any

from agento11y import (
    Client,
    ClientConfig,
    ContentCaptureMode,
    GenerationStart,
    HookContext,
    HookEvaluateRequest,
    HookInput,
    HookModel,
    HookPhase,
    HooksConfig,
    Message,
    MessageRole,
    ModelRef,
    TokenUsage,
    ToolCall,
    ToolExecutionStart,
    assistant_text_message,
    text_part,
    tool_call_part,
)

logger = logging.getLogger(__name__)
_lock = threading.Lock()
_client: Client | None = None
_generations: dict[str, tuple[Any, list[Message]]] = {}


def _enabled(name: str, default: bool = False) -> bool:
    value = os.getenv(name)
    return default if value is None else value.strip().lower() in {"1", "true", "yes", "on"}


def _capture_mode() -> ContentCaptureMode:
    raw = os.getenv("AGENTO11Y_CONTENT_CAPTURE_MODE", ContentCaptureMode.METADATA_ONLY.value)
    try:
        return ContentCaptureMode(raw)
    except ValueError:
        logger.warning("invalid AGENTO11Y_CONTENT_CAPTURE_MODE=%r; using metadata_only", raw)
        return ContentCaptureMode.METADATA_ONLY


def _get_client() -> Client:
    global _client
    with _lock:
        if _client is None:
            _client = Client(ClientConfig(content_capture=_capture_mode(), agent_name="hermes"))
        return _client


def _text(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    if isinstance(value, (dict, list)):
        return json.dumps(value, ensure_ascii=False, default=str)
    return str(value)


def _messages(value: Any) -> list[Message]:
    if not isinstance(value, list):
        return []
    result: list[Message] = []
    for item in value:
        if not isinstance(item, dict):
            continue
        role = str(item.get("role", "user")).lower()
        mapped = {"assistant": MessageRole.ASSISTANT, "tool": MessageRole.TOOL}.get(
            role, MessageRole.USER
        )
        result.append(
            Message(role=mapped, parts=[text_part(_text(item.get("content", item.get("text", ""))))])
        )
    return result


def _key(kwargs: dict[str, Any]) -> str:
    explicit = _text(kwargs.get("api_request_id"))
    if explicit:
        return explicit
    return ":".join(
        (_text(kwargs.get("session_id")), _text(kwargs.get("task_id")), _text(kwargs.get("api_call_count")))
    )


def _conversation_id(kwargs: dict[str, Any]) -> str:
    return _text(kwargs.get("session_id") or kwargs.get("task_id") or kwargs.get("turn_id"))


def on_pre_api_request(**kwargs: Any) -> None:
    """Open a generation around one Hermes provider API request."""
    try:
        request_key = _key(kwargs)
        input_messages = _messages(kwargs.get("conversation_history") or kwargs.get("messages"))
        recorder = _get_client().start_streaming_generation(
            GenerationStart(
                model=ModelRef(provider=_text(kwargs.get("provider")), name=_text(kwargs.get("model"))),
                conversation_id=_conversation_id(kwargs),
                agent_name="hermes",
                operation_name="hermes.api_request",
                max_tokens=kwargs.get("max_tokens") if isinstance(kwargs.get("max_tokens"), int) else None,
                metadata={"platform": _text(kwargs.get("platform")), "api_mode": _text(kwargs.get("api_mode"))},
            )
        )
        recorder.__enter__()
        with _lock:
            previous = _generations.pop(request_key, None)
            _generations[request_key] = (recorder, input_messages)
        if previous:
            previous[0].end()
    except Exception as exc:
        logger.debug("start AgentO11y generation failed: %s", exc)


def _finish_generation(kwargs: dict[str, Any], error: Exception | None = None) -> None:
    try:
        with _lock:
            state = _generations.pop(_key(kwargs), None)
        if state is None:
            return
        recorder, input_messages = state
        if error is not None:
            recorder.set_call_error(error)
        else:
            content = kwargs.get("assistant_content", kwargs.get("response", ""))
            output = [assistant_text_message(_text(content))] if content else []
            usage = kwargs.get("usage") if isinstance(kwargs.get("usage"), dict) else {}
            recorder.set_result(
                input=input_messages,
                output=output,
                response_model=_text(kwargs.get("response_model") or kwargs.get("model")),
                stop_reason=_text(kwargs.get("finish_reason")),
                usage=TokenUsage(
                    input_tokens=int(usage.get("input_tokens", 0) or 0),
                    output_tokens=int(usage.get("output_tokens", 0) or 0),
                ),
            )
        recorder.end()
    except Exception as exc:
        logger.debug("finish AgentO11y generation failed: %s", exc)


def on_post_api_request(**kwargs: Any) -> None:
    _finish_generation(kwargs)


def on_api_request_error(**kwargs: Any) -> None:
    _finish_generation(
        kwargs,
        RuntimeError(_text(kwargs.get("error") or kwargs.get("message") or "Hermes API request failed")),
    )


def on_pre_tool_call(**kwargs: Any) -> dict[str, Any] | None:
    """Run optional policy before Hermes dispatches a tool."""
    if not _enabled("AGENTO11Y_GUARDS_ENABLED"):
        return None
    args = kwargs.get("args") if isinstance(kwargs.get("args"), dict) else {}
    try:
        call = ToolCall(
            name=_text(kwargs.get("tool_name")),
            id=_text(kwargs.get("tool_call_id")),
            input_json=json.dumps(args).encode(),
        )
        response = _get_client().evaluate_hook(
            HookEvaluateRequest(
                phase=HookPhase.POSTFLIGHT.value,
                context=HookContext(
                    model=HookModel(provider=_text(kwargs.get("provider")), name=_text(kwargs.get("model"))),
                    agent_name="hermes",
                    conversation_id=_conversation_id(kwargs),
                ),
                input=HookInput(messages=[Message(role=MessageRole.ASSISTANT, parts=[tool_call_part(call)])]),
            ),
            hooks=HooksConfig(
                enabled=True,
                phases=[HookPhase.POSTFLIGHT.value],
                fail_open=_enabled("AGENTO11Y_GUARDS_FAIL_OPEN", True),
            ),
        )
        if response.is_deny:
            return {"action": "block", "message": response.reason or "Denied by Grafana Agent Observability policy"}
        transformed = response.transformed_input
        if transformed and transformed.messages:
            for part in transformed.messages[0].parts:
                if part.tool_call and part.tool_call.input_json:
                    replacement = json.loads(part.tool_call.input_json)
                    if isinstance(replacement, dict):
                        return {"action": "modify", "args": replacement}
    except Exception as exc:
        if not _enabled("AGENTO11Y_GUARDS_FAIL_OPEN", True):
            return {"action": "block", "message": "AgentO11y guard evaluation failed closed: " + _text(exc)}
        logger.warning("AgentO11y guard evaluation failed open: %s", exc)
    return None


def on_post_tool_call(**kwargs: Any) -> None:
    try:
        args = kwargs.get("args") if isinstance(kwargs.get("args"), dict) else {}
        recorder = _get_client().start_tool_execution(
            ToolExecutionStart(
                tool_name=_text(kwargs.get("tool_name")),
                tool_call_id=_text(kwargs.get("tool_call_id")),
                conversation_id=_conversation_id(kwargs),
                agent_name="hermes",
                request_model=_text(kwargs.get("model")),
                request_provider=_text(kwargs.get("provider")),
                include_content=_capture_mode() in {ContentCaptureMode.FULL, ContentCaptureMode.NO_TOOL_CONTENT},
            )
        )
        recorder.set_result(arguments=args, result=kwargs.get("result"))
        recorder.end()
    except Exception as exc:
        logger.debug("export AgentO11y tool execution failed: %s", exc)


def register(ctx: Any) -> None:
    for event, handler in (
        ("pre_api_request", on_pre_api_request),
        ("post_api_request", on_post_api_request),
        ("api_request_error", on_api_request_error),
        ("pre_tool_call", on_pre_tool_call),
        ("post_tool_call", on_post_tool_call),
    ):
        ctx.register_hook(event, handler)
