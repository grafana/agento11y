"""LangChain callback handler that records OpenTelemetry generative AI spans.

This replaces the Agent Observability SDK. It emits spans that follow the OpenTelemetry
generative AI semantic conventions, and Agent Observability reads them from the OTLP
trace export:

- one ``chat <model>`` span per model call, with provider, model, token usage, and messages
- one ``execute_tool <name>`` span per tool call

Chat spans carry ``agento11y.record=true`` so Agent Observability turns them into generations.
Tool spans do not, because tool calls don't create standalone generations.
"""

from __future__ import annotations

import json
import os
from typing import Any
from uuid import UUID

from langchain_core.callbacks import BaseCallbackHandler
from langchain_core.messages import BaseMessage
from opentelemetry.trace import Span, SpanKind, Status, StatusCode, Tracer

# Content capture modes, named like the SDK example's capture modes.
CAPTURE_FULL = "full"
CAPTURE_NO_TOOL_CONTENT = "no_tool_content"
CAPTURE_NONE = "metadata_only"


def capture_mode() -> str:
    mode = os.getenv("AGENTO11Y_CONTENT_CAPTURE_MODE", CAPTURE_NO_TOOL_CONTENT).strip().lower()
    return mode if mode in {CAPTURE_FULL, CAPTURE_NO_TOOL_CONTENT, CAPTURE_NONE} else CAPTURE_NO_TOOL_CONTENT


def _text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(p.get("text", "") for p in content if isinstance(p, dict) and p.get("type") == "text")
    return ""


def _role(message: BaseMessage) -> str:
    return {"human": "user", "ai": "assistant", "tool": "tool", "system": "system"}.get(message.type, message.type)


def _parts(message: BaseMessage, mode: str) -> list[dict[str, Any]]:
    """Convert one LangChain message to GenAI message parts."""
    parts: list[dict[str, Any]] = []
    text = _text(message.content)
    keep_tool_content = mode == CAPTURE_FULL
    if message.type == "tool":
        part: dict[str, Any] = {"type": "tool_call_response", "id": getattr(message, "tool_call_id", "")}
        if keep_tool_content:
            part["result"] = text
        return [part]
    if text:
        parts.append({"type": "text", "content": text})
    for call in getattr(message, "tool_calls", None) or []:
        part = {"type": "tool_call", "id": call.get("id", ""), "name": call.get("name", "")}
        if keep_tool_content:
            part["arguments"] = call.get("args", {})
        parts.append(part)
    return parts


def _messages_json(messages: list[BaseMessage], mode: str) -> str:
    return json.dumps([{"role": _role(m), "parts": _parts(m, mode)} for m in messages if m.type != "system"])


def _system_json(messages: list[BaseMessage]) -> str:
    return json.dumps([{"type": "text", "content": _text(m.content)} for m in messages if m.type == "system"])


class GenAISpanHandler(BaseCallbackHandler):
    """Record chat and tool spans for one agent run."""

    def __init__(self, tracer: Tracer, *, provider: str, model: str, agent_name: str, conversation_id: str) -> None:
        self._tracer = tracer
        self._provider = provider
        self._model = model
        self._agent_name = agent_name
        self._conversation_id = conversation_id
        self._mode = capture_mode()
        self._spans: dict[UUID, Span] = {}

    def on_chat_model_start(
        self,
        serialized: dict[str, Any],
        messages: list[list[BaseMessage]],
        *,
        run_id: UUID,
        invocation_params: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> None:
        params = invocation_params or {}
        model = str(params.get("model_id") or params.get("model") or params.get("model_name") or self._model)
        batch = messages[0] if messages else []
        span = self._tracer.start_span(f"chat {model}", kind=SpanKind.CLIENT)
        span.set_attributes(
            {
                "agento11y.record": True,
                "gen_ai.operation.name": "chat",
                "gen_ai.provider.name": self._provider,
                "gen_ai.request.model": model,
                "gen_ai.agent.name": self._agent_name,
                "gen_ai.conversation.id": self._conversation_id,
            }
        )
        if self._mode != CAPTURE_NONE:
            span.set_attribute("gen_ai.input.messages", _messages_json(batch, self._mode))
            system = _system_json(batch)
            if system != "[]":
                span.set_attribute("gen_ai.system_instructions", system)
        self._spans[run_id] = span

    def on_llm_end(self, response: Any, *, run_id: UUID, **kwargs: Any) -> None:
        span = self._spans.pop(run_id, None)
        if span is None:
            return
        message = response.generations[0][0].message if response.generations and response.generations[0] else None
        if message is not None:
            self._set_response(span, message)
        span.end()

    def _set_response(self, span: Span, message: BaseMessage) -> None:
        usage = getattr(message, "usage_metadata", None) or {}
        details = usage.get("input_token_details") or {}
        span.set_attributes(
            {
                "gen_ai.usage.input_tokens": int(usage.get("input_tokens", 0)),
                "gen_ai.usage.output_tokens": int(usage.get("output_tokens", 0)),
                "gen_ai.usage.cache_read.input_tokens": int(details.get("cache_read", 0)),
                "gen_ai.usage.cache_creation.input_tokens": int(details.get("cache_creation", 0)),
            }
        )
        response_model = (getattr(message, "response_metadata", None) or {}).get("model_id") or (
            getattr(message, "response_metadata", None) or {}
        ).get("model_name")
        if response_model:
            span.set_attribute("gen_ai.response.model", str(response_model))
        if self._mode != CAPTURE_NONE:
            span.set_attribute("gen_ai.output.messages", _messages_json([message], self._mode))

    def on_tool_start(self, serialized: dict[str, Any], input_str: str, *, run_id: UUID, **kwargs: Any) -> None:
        name = str(serialized.get("name") or kwargs.get("name") or "tool")
        span = self._tracer.start_span(f"execute_tool {name}", kind=SpanKind.INTERNAL)
        span.set_attributes(
            {
                "gen_ai.operation.name": "execute_tool",
                "gen_ai.tool.name": name,
                "gen_ai.conversation.id": self._conversation_id,
            }
        )
        if self._mode == CAPTURE_FULL:
            span.set_attribute("gen_ai.tool.call.arguments", input_str)
        self._spans[run_id] = span

    def on_tool_end(self, output: Any, *, run_id: UUID, **kwargs: Any) -> None:
        span = self._spans.pop(run_id, None)
        if span is None:
            return
        if self._mode == CAPTURE_FULL:
            span.set_attribute("gen_ai.tool.call.result", _text(getattr(output, "content", output)) or str(output))
        span.end()

    def on_llm_error(self, error: BaseException, *, run_id: UUID, **kwargs: Any) -> None:
        self._fail(run_id, error)

    def on_tool_error(self, error: BaseException, *, run_id: UUID, **kwargs: Any) -> None:
        self._fail(run_id, error)

    def _fail(self, run_id: UUID, error: BaseException) -> None:
        span = self._spans.pop(run_id, None)
        if span is None:
            return
        span.set_attribute("error.type", type(error).__name__)
        span.record_exception(error)
        span.set_status(Status(StatusCode.ERROR, str(error)))
        span.end()
