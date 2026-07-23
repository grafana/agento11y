"""LiteLLM handler tests."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from types import SimpleNamespace
from typing import Any

from agento11y import Client, ClientConfig, EmbeddingCaptureConfig, GenerationExportConfig
from agento11y.models import (
    ExportGenerationResult,
    ExportGenerationsResponse,
    GenerationMode,
    MessageRole,
    PartKind,
)
from agento11y_litellm import Agento11yLiteLLMLogger, create_agento11y_litellm_logger
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter


class _CapturingExporter:
    def __init__(self) -> None:
        self.requests: list[Any] = []

    def export_generations(self, request: Any) -> ExportGenerationsResponse:
        self.requests.append(request)
        return ExportGenerationsResponse(
            results=[ExportGenerationResult(generation_id=g.id, accepted=True) for g in request.generations]
        )

    def shutdown(self) -> None:
        pass


def _new_client(exporter: _CapturingExporter) -> Client:
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                batch_size=10,
                flush_interval=timedelta(seconds=60),
            ),
            generation_exporter=exporter,
        )
    )


def _new_span_client(
    exporter: _CapturingExporter,
    span_exporter: InMemorySpanExporter,
    *,
    embedding_capture: EmbeddingCaptureConfig | None = None,
) -> Client:
    """Client wired to an in-memory span exporter for span-only output."""
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    return Client(
        ClientConfig(
            tracer=provider.get_tracer("agento11y-litellm-test"),
            generation_export=GenerationExportConfig(
                batch_size=10,
                flush_interval=timedelta(seconds=60),
            ),
            generation_exporter=exporter,
            embedding_capture=embedding_capture or EmbeddingCaptureConfig(),
        )
    )


def _base_embedding_slo(**overrides: Any) -> dict[str, Any]:
    slo: dict[str, Any] = {
        "id": "embd-abc123",
        "call_type": "embedding",
        "stream": False,
        "custom_llm_provider": "openai",
        "model": "text-embedding-3-small",
        "prompt_tokens": 8,
        "total_tokens": 8,
        "error_str": None,
        "request_tags": [],
        "end_user": None,
    }
    slo.update(overrides)
    return slo


def _embedding_response_obj(dimensions: int = 3) -> SimpleNamespace:
    """Build an EmbeddingResponse-like object with one vector."""
    return SimpleNamespace(
        model="text-embedding-3-small",
        data=[{"embedding": [0.0] * dimensions}],
        usage=SimpleNamespace(prompt_tokens=8, total_tokens=8),
    )


def _make_slo_response(
    content: str = "Hello!",
    finish_reason: str = "stop",
    tool_calls: list[dict[str, Any]] | None = None,
    reasoning_content: str | None = None,
    thinking_blocks: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Build an SLO response dict in OpenAI chat completion format."""
    message: dict[str, Any] = {"content": content}
    if tool_calls is not None:
        message["tool_calls"] = tool_calls
    if reasoning_content is not None:
        message["reasoning_content"] = reasoning_content
    if thinking_blocks is not None:
        message["thinking_blocks"] = thinking_blocks
    return {
        "choices": [
            {
                "message": message,
                "finish_reason": finish_reason,
            }
        ]
    }


def _base_slo(**overrides: Any) -> dict[str, Any]:
    slo: dict[str, Any] = {
        "id": "chatcmpl-abc123",
        "call_type": "completion",
        "stream": False,
        "custom_llm_provider": "openai",
        "model": "gpt-4",
        "prompt_tokens": 10,
        "completion_tokens": 5,
        "total_tokens": 15,
        "startTime": 1700000000.0,
        "endTime": 1700000001.0,
        "completionStartTime": 0.0,
        "messages": [
            {"role": "user", "content": "Hello"},
        ],
        "response": _make_slo_response(),
        "error_str": None,
        "model_parameters": {},
        "request_tags": [],
        "end_user": None,
    }
    slo.update(overrides)
    return slo


_START = datetime(2024, 1, 1, 0, 0, 0, tzinfo=timezone.utc)
_END = datetime(2024, 1, 1, 0, 0, 1, tzinfo=timezone.utc)


def _make_kwargs(slo: dict[str, Any], **litellm_metadata: Any) -> dict[str, Any]:
    """Build kwargs dict as LiteLLM passes to callbacks."""
    kwargs: dict[str, Any] = {"standard_logging_object": slo}
    if litellm_metadata:
        kwargs["litellm_params"] = {"metadata": litellm_metadata}
    return kwargs


def test_missing_slo() -> None:
    """Handler returns gracefully when standard_logging_object is None."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        handler.log_success_event(
            kwargs={},
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()
        assert len(exporter.requests) == 0
    finally:
        client.shutdown()


def test_success_event_basic() -> None:
    """User text -> assistant text mapping plus model, provider, tokens, timestamps."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(response=_make_slo_response(content="Hi there!"))
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        assert len(exporter.requests) == 1
        gen = exporter.requests[0].generations[0]

        assert gen.model.provider == "openai"
        assert gen.model.name == "gpt-4"
        assert gen.mode == GenerationMode.SYNC

        assert len(gen.input) == 1
        assert gen.input[0].role == MessageRole.USER
        assert gen.input[0].parts[0].text == "Hello"

        assert len(gen.output) == 1
        assert gen.output[0].role == MessageRole.ASSISTANT
        assert gen.output[0].parts[0].text == "Hi there!"

        assert gen.usage.input_tokens == 10
        assert gen.usage.output_tokens == 5
        assert gen.usage.total_tokens == 15

        assert gen.started_at is not None
        assert gen.completed_at is not None
        assert gen.stop_reason == "stop"
    finally:
        client.shutdown()


def test_failure_event() -> None:
    """call_error is set and the generation is still recorded."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(error_str="Rate limit exceeded")
        handler.log_failure_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        assert len(exporter.requests) == 1
        gen = exporter.requests[0].generations[0]
        assert gen.call_error != ""
        assert "Rate limit exceeded" in gen.call_error
    finally:
        client.shutdown()


def test_system_prompt_extraction() -> None:
    """System messages are extracted into system_prompt."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            messages=[
                {"role": "system", "content": "You are helpful."},
                {"role": "developer", "content": "Be concise."},
                {"role": "user", "content": "Hello"},
            ]
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.system_prompt == "You are helpful.\n\nBe concise."
        assert len(gen.input) == 1
        assert gen.input[0].role == MessageRole.USER
    finally:
        client.shutdown()


def test_tool_calls() -> None:
    """Assistant tool_calls and tool results map correctly."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            messages=[
                {"role": "user", "content": "What's the weather?"},
                {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [
                        {
                            "id": "call_1",
                            "function": {
                                "name": "get_weather",
                                "arguments": '{"city": "Berlin"}',
                            },
                        }
                    ],
                },
                {
                    "role": "tool",
                    "tool_call_id": "call_1",
                    "name": "get_weather",
                    "content": "Sunny, 22°C",
                },
            ],
            response=_make_slo_response(content="It's sunny in Berlin!"),
        )

        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]

        assert len(gen.input) == 3
        assert gen.input[0].role == MessageRole.USER

        assistant_msg = gen.input[1]
        assert assistant_msg.role == MessageRole.ASSISTANT
        tool_call_part = [p for p in assistant_msg.parts if p.kind == PartKind.TOOL_CALL]
        assert len(tool_call_part) == 1
        assert tool_call_part[0].tool_call.name == "get_weather"
        assert tool_call_part[0].tool_call.id == "call_1"

        tool_msg = gen.input[2]
        assert tool_msg.role == MessageRole.TOOL
        assert tool_msg.parts[0].kind == PartKind.TOOL_RESULT
        assert tool_msg.parts[0].tool_result.content == "Sunny, 22°C"
        assert tool_msg.parts[0].tool_result.tool_call_id == "call_1"
    finally:
        client.shutdown()


def test_streaming_mode() -> None:
    """stream=True produces STREAM mode and completionStartTime sets first_token_at."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            stream=True,
            completionStartTime=1700000000.5,
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.mode == GenerationMode.STREAM
    finally:
        client.shutdown()


def test_tags_and_metadata() -> None:
    """request_tags and end_user flow through."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(
            client=client,
            extra_tags={"env": "test"},
            extra_metadata={"session": "s1"},
        )
        slo = _base_slo(
            request_tags=["prod", "blue"],
            end_user="user-42",
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]

        assert gen.tags["agento11y.framework.name"] == "litellm"
        assert gen.tags["agento11y.framework.source"] == "handler"
        assert gen.tags["agento11y.framework.language"] == "python"
        assert gen.tags["litellm.tag.prod"] == "prod"
        assert gen.tags["litellm.tag.blue"] == "blue"
        assert gen.tags["env"] == "test"
        assert gen.metadata["session"] == "s1"
        assert gen.user_id == "user-42"
    finally:
        client.shutdown()


def test_model_parameters() -> None:
    """temperature, max_tokens, and top_p are extracted."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            model_parameters={
                "temperature": "0.7",
                "max_tokens": "1024",
                "top_p": "0.9",
            }
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.temperature == 0.7
        assert gen.max_tokens == 1024
        assert gen.top_p == 0.9
    finally:
        client.shutdown()


def test_capture_inputs_disabled() -> None:
    """When capture_inputs=False, no input messages or system prompt."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client, capture_inputs=False)
        slo = _base_slo(
            messages=[
                {"role": "system", "content": "Secret system prompt"},
                {"role": "user", "content": "Hello"},
            ]
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert len(gen.input) == 0
        assert gen.system_prompt == ""
    finally:
        client.shutdown()


def test_capture_outputs_disabled() -> None:
    """When capture_outputs=False, no output messages."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client, capture_outputs=False)
        handler.log_success_event(
            kwargs=_make_kwargs(_base_slo()),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert len(gen.output) == 0
    finally:
        client.shutdown()


def test_response_tool_calls_in_output() -> None:
    """Tool calls in the SLO response map to output messages."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            response=_make_slo_response(
                content="Let me check.",
                tool_calls=[
                    {
                        "id": "call_99",
                        "function": {
                            "name": "get_weather",
                            "arguments": '{"city": "Berlin"}',
                        },
                    }
                ],
            )
        )

        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert len(gen.output) == 1
        output_msg = gen.output[0]
        assert output_msg.role == MessageRole.ASSISTANT

        text_parts = [p for p in output_msg.parts if p.kind == PartKind.TEXT]
        tool_parts = [p for p in output_msg.parts if p.kind == PartKind.TOOL_CALL]
        assert len(text_parts) == 1
        assert text_parts[0].text == "Let me check."
        assert len(tool_parts) == 1
        assert tool_parts[0].tool_call.name == "get_weather"
        assert tool_parts[0].tool_call.id == "call_99"
    finally:
        client.shutdown()


def test_async_log_success_event() -> None:
    """Async success callback records generation."""
    import asyncio

    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)

        asyncio.run(
            handler.async_log_success_event(
                kwargs=_make_kwargs(_base_slo()),
                response_obj=None,
                start_time=_START,
                end_time=_END,
            )
        )
        client.flush()

        assert len(exporter.requests) == 1
        gen = exporter.requests[0].generations[0]
        assert gen.model.name == "gpt-4"
    finally:
        client.shutdown()


def test_agent_name_and_conversation_id() -> None:
    """agent_name, agent_version, conversation_id flow through."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(
            client=client,
            agent_name="my-agent",
            agent_version="v2",
            conversation_id="conv-123",
        )
        handler.log_success_event(
            kwargs=_make_kwargs(_base_slo()),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.agent_name == "my-agent"
        assert gen.agent_version == "v2"
        assert gen.conversation_id == "conv-123"
    finally:
        client.shutdown()


def test_per_request_agent_name_from_metadata() -> None:
    """Per-request metadata agent_name overrides static value."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(
            client=client,
            agent_name="default-agent",
            agent_version="v1",
        )
        handler.log_success_event(
            kwargs=_make_kwargs(_base_slo(), agent_name="search-agent", agent_version="v3"),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.agent_name == "search-agent"
        assert gen.agent_version == "v3"
    finally:
        client.shutdown()


def test_per_request_agent_name_falls_back_to_static() -> None:
    """When metadata has no agent_name, static value is used."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(
            client=client,
            agent_name="default-agent",
            agent_version="v1",
        )
        handler.log_success_event(
            kwargs=_make_kwargs(_base_slo()),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.agent_name == "default-agent"
        assert gen.agent_version == "v1"
    finally:
        client.shutdown()


def test_create_agento11y_litellm_logger_factory() -> None:
    """Factory function creates a properly configured logger."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = create_agento11y_litellm_logger(
            client=client,
            capture_inputs=True,
            capture_outputs=True,
            extra_tags={"k": "v"},
        )
        assert isinstance(handler, Agento11yLiteLLMLogger)
    finally:
        client.shutdown()


def test_non_chat_call_type_skipped() -> None:
    """image_generation/transcription call types produce no generation export."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        for call_type in ("image_generation", "transcription"):
            slo = _base_slo(call_type=call_type)
            handler.log_success_event(
                kwargs=_make_kwargs(slo),
                response_obj=None,
                start_time=_START,
                end_time=_END,
            )
        client.flush()
        assert len(exporter.requests) == 0
    finally:
        client.shutdown()


def test_acompletion_call_type_recorded() -> None:
    """Async completion call_type is still recorded."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(call_type="acompletion")
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()
        assert len(exporter.requests) == 1
    finally:
        client.shutdown()


def test_text_completion_call_type_recorded() -> None:
    """text_completion and atext_completion call types produce generations."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        for call_type in ("text_completion", "atext_completion"):
            slo = _base_slo(call_type=call_type)
            handler.log_success_event(
                kwargs=_make_kwargs(slo),
                response_obj=None,
                start_time=_START,
                end_time=_END,
            )
        client.flush()
        assert len(exporter.requests) == 1
        assert len(exporter.requests[0].generations) == 2
    finally:
        client.shutdown()


def test_dynamic_conversation_id_from_metadata() -> None:
    """conversation_id is resolved from per-request litellm_params metadata."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client, conversation_id="static-fallback")
        slo = _base_slo()
        kwargs = _make_kwargs(slo, conversation_id="dynamic-conv-456")
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.conversation_id == "dynamic-conv-456"
    finally:
        client.shutdown()


def test_conversation_id_session_id_fallback() -> None:
    """session_id in metadata is used when conversation_id is absent."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo()
        kwargs = _make_kwargs(slo, session_id="sess-789")
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.conversation_id == "sess-789"
    finally:
        client.shutdown()


def test_litellm_session_id_used_as_conversation_id() -> None:
    """LiteLLM's built-in litellm_session_id resolves as conversation_id."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client, conversation_id="static-fallback")
        slo = _base_slo()
        kwargs: dict[str, Any] = {
            "standard_logging_object": slo,
            "litellm_params": {
                "metadata": {},
                "litellm_session_id": "litellm-sess-001",
            },
        }
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.conversation_id == "litellm-sess-001"
    finally:
        client.shutdown()


def test_litellm_trace_id_used_as_conversation_id() -> None:
    """LiteLLM's litellm_trace_id is used when no session_id is present."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo()
        kwargs: dict[str, Any] = {
            "standard_logging_object": slo,
            "litellm_params": {
                "metadata": {},
                "litellm_trace_id": "trace-abc",
            },
        }
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.conversation_id == "trace-abc"
    finally:
        client.shutdown()


def test_metadata_conversation_id_takes_precedence_over_litellm_session() -> None:
    """Explicit conversation_id in metadata wins over litellm_session_id."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo()
        kwargs: dict[str, Any] = {
            "standard_logging_object": slo,
            "litellm_params": {
                "metadata": {"conversation_id": "explicit-conv"},
                "litellm_session_id": "litellm-sess-002",
            },
        }
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.conversation_id == "explicit-conv"
    finally:
        client.shutdown()


def test_empty_tool_result_preserved() -> None:
    """Tool results with empty content are still recorded (not dropped)."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            messages=[
                {"role": "user", "content": "Send email"},
                {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [
                        {
                            "id": "call_1",
                            "function": {"name": "send_email", "arguments": "{}"},
                        }
                    ],
                },
                {
                    "role": "tool",
                    "tool_call_id": "call_1",
                    "name": "send_email",
                    "content": "",
                },
            ]
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert len(gen.input) == 3
        tool_msg = gen.input[2]
        assert tool_msg.role == MessageRole.TOOL
        assert tool_msg.parts[0].tool_result.tool_call_id == "call_1"
        assert tool_msg.parts[0].tool_result.content == ""
    finally:
        client.shutdown()


def test_string_response_in_slo() -> None:
    """SLO response can be a plain string (non-dict)."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(response="Plain text response")
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert len(gen.output) == 1
        assert gen.output[0].parts[0].text == "Plain text response"
    finally:
        client.shutdown()


def test_missing_call_type_still_recorded() -> None:
    """SLO without call_type is recorded (backwards compat with older LiteLLM)."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo()
        del slo["call_type"]
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()
        assert len(exporter.requests) == 1
    finally:
        client.shutdown()


def test_tool_definitions_captured() -> None:
    """Tool schemas from optional_params are recorded in generation."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo()
        kwargs = _make_kwargs(slo)
        kwargs["optional_params"] = {
            "tools": [
                {
                    "type": "function",
                    "function": {
                        "name": "get_weather",
                        "description": "Get the current weather",
                        "parameters": {
                            "type": "object",
                            "properties": {
                                "city": {"type": "string"},
                            },
                            "required": ["city"],
                        },
                    },
                },
                {
                    "type": "function",
                    "function": {
                        "name": "search",
                        "description": "Search the web",
                        "parameters": {
                            "type": "object",
                            "properties": {
                                "query": {"type": "string"},
                            },
                        },
                    },
                },
            ]
        }
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert len(gen.tools) == 2
        assert gen.tools[0].name == "get_weather"
        assert gen.tools[0].description == "Get the current weather"
        assert gen.tools[0].type == "function"
        assert b'"city"' in gen.tools[0].input_schema_json
        assert gen.tools[1].name == "search"
    finally:
        client.shutdown()


def test_detailed_token_usage() -> None:
    """Cache and reasoning token details are extracted from response_obj.usage."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(prompt_tokens=100, completion_tokens=50, total_tokens=150)

        response_obj = SimpleNamespace(
            choices=[SimpleNamespace(message=SimpleNamespace(content="Hi"), finish_reason="stop")],
            usage=SimpleNamespace(
                prompt_tokens_details=SimpleNamespace(
                    cached_tokens=30,
                    cache_creation_tokens=20,
                ),
                completion_tokens_details=SimpleNamespace(
                    reasoning_tokens=15,
                ),
            ),
        )

        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=response_obj,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        # prompt_tokens (100) is cache-inclusive: fresh input = 100 - 30 cached = 70.
        assert gen.usage.input_tokens == 70
        assert gen.usage.output_tokens == 50
        assert gen.usage.total_tokens == 150
        assert gen.usage.cache_read_input_tokens == 30
        assert gen.usage.cache_write_input_tokens == 20
        assert gen.usage.reasoning_tokens == 15
    finally:
        client.shutdown()


def test_zero_token_counts_preserved() -> None:
    """Explicit zero token counts are preserved, not dropped by truthiness check."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(prompt_tokens=100, completion_tokens=50, total_tokens=150)

        response_obj = SimpleNamespace(
            choices=[SimpleNamespace(message=SimpleNamespace(content="Hi"), finish_reason="stop")],
            usage=SimpleNamespace(
                prompt_tokens_details=SimpleNamespace(
                    cached_tokens=0,
                    cache_creation_tokens=0,
                ),
                completion_tokens_details=SimpleNamespace(
                    reasoning_tokens=0,
                ),
            ),
        )

        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=response_obj,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.usage.cache_read_input_tokens == 0
        assert gen.usage.cache_write_input_tokens == 0
        assert gen.usage.reasoning_tokens == 0
    finally:
        client.shutdown()


def test_non_utc_timezone_converted_to_utc() -> None:
    """Timezone-aware non-UTC datetimes are converted correctly in timestamps."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo()

        tz_plus5 = timezone(timedelta(hours=5))
        start = datetime(2024, 1, 1, 15, 0, 0, tzinfo=tz_plus5)  # = 10:00 UTC
        end = datetime(2024, 1, 1, 15, 0, 1, tzinfo=tz_plus5)  # = 10:00:01 UTC

        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=start,
            end_time=end,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.started_at == datetime(2024, 1, 1, 10, 0, 0, tzinfo=timezone.utc)
        assert gen.completed_at == datetime(2024, 1, 1, 10, 0, 1, tzinfo=timezone.utc)
    finally:
        client.shutdown()


def test_naive_datetime_produces_utc_aware_output() -> None:
    """Naive datetimes (as produced by datetime.now()) result in UTC-aware timestamps."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo()

        naive_start = datetime(2024, 6, 15, 14, 30, 0)
        naive_end = datetime(2024, 6, 15, 14, 30, 1)

        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=naive_start,
            end_time=naive_end,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert gen.started_at is not None
        assert gen.started_at.tzinfo is not None
        assert gen.completed_at is not None
        assert gen.completed_at.tzinfo is not None
    finally:
        client.shutdown()


def test_multi_choice_response_all_mapped() -> None:
    """All choices in a multi-completion response (n>1) are mapped to output."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            response={
                "choices": [
                    {"message": {"content": "Answer A"}, "finish_reason": "stop"},
                    {"message": {"content": "Answer B"}, "finish_reason": "stop"},
                    {"message": {"content": "Answer C"}, "finish_reason": "length"},
                ],
            }
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert len(gen.output) == 3
        assert gen.output[0].parts[0].text == "Answer A"
        assert gen.output[1].parts[0].text == "Answer B"
        assert gen.output[2].parts[0].text == "Answer C"
        # stop_reason comes from first choice
        assert gen.stop_reason == "stop"
    finally:
        client.shutdown()


def test_embedding_produces_span() -> None:
    """Embedding call emits a span with provider, model, counts, and dimensions."""
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(exporter, span_exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_embedding_slo()
        kwargs = _make_kwargs(slo)
        kwargs["input"] = "hello world"
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=_embedding_response_obj(dimensions=3),
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        assert len(exporter.requests) == 0
        spans = span_exporter.get_finished_spans()
        assert len(spans) == 1
        span = spans[0]
        assert span.name == "embeddings text-embedding-3-small"
        assert span.attributes.get("gen_ai.provider.name") == "openai"
        assert span.attributes.get("gen_ai.request.model") == "text-embedding-3-small"
        assert span.attributes.get("gen_ai.embeddings.input_count") == 1
        assert span.attributes.get("gen_ai.usage.input_tokens") == 8
        assert span.attributes.get("gen_ai.embeddings.dimension.count") == 3
    finally:
        client.shutdown()


def test_embedding_input_texts_suppressed_by_default() -> None:
    """input_texts is absent unless both capture flags are enabled."""
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(exporter, span_exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client, capture_inputs=True)
        slo = _base_embedding_slo()
        kwargs = _make_kwargs(slo)
        kwargs["input"] = "secret text"
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=_embedding_response_obj(),
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        span = span_exporter.get_finished_spans()[0]
        assert "gen_ai.embeddings.input_texts" not in span.attributes
        assert span.attributes.get("gen_ai.embeddings.input_count") == 1
        assert span.attributes.get("gen_ai.usage.input_tokens") == 8
    finally:
        client.shutdown()


def test_embedding_input_texts_captured_when_both_flags_enabled() -> None:
    """input_texts is attached only when capture_inputs and capture_input are both true."""
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(
        exporter,
        span_exporter,
        embedding_capture=EmbeddingCaptureConfig(capture_input=True),
    )
    try:
        handler = Agento11yLiteLLMLogger(client=client, capture_inputs=True)
        slo = _base_embedding_slo()
        kwargs = _make_kwargs(slo)
        kwargs["input"] = ["first", "second"]
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=_embedding_response_obj(),
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        span = span_exporter.get_finished_spans()[0]
        assert span.attributes.get("gen_ai.embeddings.input_texts") == ("first", "second")
        assert span.attributes.get("gen_ai.embeddings.input_count") == 2
    finally:
        client.shutdown()


def test_embedding_empty_input_sets_no_input_texts() -> None:
    """A redacted empty-string input counts as 0 and leaves input_texts unset."""
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(
        exporter,
        span_exporter,
        embedding_capture=EmbeddingCaptureConfig(capture_input=True),
    )
    try:
        handler = Agento11yLiteLLMLogger(client=client, capture_inputs=True)
        slo = _base_embedding_slo()
        kwargs = _make_kwargs(slo)
        kwargs["input"] = ""
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=_embedding_response_obj(),
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        span = span_exporter.get_finished_spans()[0]
        assert "gen_ai.embeddings.input_texts" not in span.attributes
        assert span.attributes.get("gen_ai.embeddings.input_count") == 0
    finally:
        client.shutdown()


def test_embedding_input_text_gated_by_handler_capture_inputs() -> None:
    """SDK capture_input alone is not enough; handler capture_inputs must also be true."""
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(
        exporter,
        span_exporter,
        embedding_capture=EmbeddingCaptureConfig(capture_input=True),
    )
    try:
        handler = Agento11yLiteLLMLogger(client=client, capture_inputs=False)
        slo = _base_embedding_slo()
        kwargs = _make_kwargs(slo)
        kwargs["input"] = "secret text"
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=_embedding_response_obj(),
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        span = span_exporter.get_finished_spans()[0]
        assert "gen_ai.embeddings.input_texts" not in span.attributes
    finally:
        client.shutdown()


def test_aembedding_recorded() -> None:
    """Async embedding call_type produces a span."""
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(exporter, span_exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_embedding_slo(call_type="aembedding")
        kwargs = _make_kwargs(slo)
        kwargs["input"] = "hello"
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=_embedding_response_obj(),
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        spans = span_exporter.get_finished_spans()
        assert len(spans) == 1
        assert spans[0].name == "embeddings text-embedding-3-small"
    finally:
        client.shutdown()


def test_embedding_failure_sets_error_status() -> None:
    """A failed embedding call produces an error-status span."""
    from opentelemetry.trace import StatusCode

    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(exporter, span_exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_embedding_slo(error_str="rate limit exceeded")
        kwargs = _make_kwargs(slo)
        kwargs["input"] = "hello"
        handler.log_failure_event(
            kwargs=kwargs,
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        span = span_exporter.get_finished_spans()[0]
        assert span.status.status_code == StatusCode.ERROR
        assert span.attributes.get("error.type") == "provider_call_error"
    finally:
        client.shutdown()


def test_embedding_input_count_string_vs_list() -> None:
    """String counts as 1, a list of N strings as N, and a redacted empty string as 0."""
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(exporter, span_exporter)
    cases = [
        ("just one", 1),
        (["a", "b", "c"], 3),
        ("", 0),
    ]
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        for inputs, _ in cases:
            slo = _base_embedding_slo()
            kwargs = _make_kwargs(slo)
            kwargs["input"] = inputs
            handler.log_success_event(
                kwargs=kwargs,
                response_obj=_embedding_response_obj(),
                start_time=_START,
                end_time=_END,
            )

        client.flush()
        spans = span_exporter.get_finished_spans()
        for span, (_, expected) in zip(spans, cases, strict=True):
            assert span.attributes.get("gen_ai.embeddings.input_count") == expected
    finally:
        client.shutdown()


def test_embedding_dimensions_fall_back_to_response() -> None:
    """When optional_params lacks dimensions, the response vector length is used."""
    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(exporter, span_exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_embedding_slo()
        kwargs = _make_kwargs(slo)
        kwargs["input"] = "hello"
        handler.log_success_event(
            kwargs=kwargs,
            response_obj=_embedding_response_obj(dimensions=5),
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        span = span_exporter.get_finished_spans()[0]
        assert span.attributes.get("gen_ai.embeddings.dimension.count") == 5
    finally:
        client.shutdown()


def test_embedding_input_text_honours_litellm_redaction() -> None:
    """With message logging off, LiteLLM clears kwargs['input'] before the callback,
    so no real input text reaches the span even with both capture flags enabled."""
    import litellm

    exporter = _CapturingExporter()
    span_exporter = InMemorySpanExporter()
    client = _new_span_client(
        exporter,
        span_exporter,
        embedding_capture=EmbeddingCaptureConfig(capture_input=True),
    )
    prev_redaction = litellm.turn_off_message_logging
    prev_callbacks = litellm.callbacks
    try:
        handler = Agento11yLiteLLMLogger(client=client, capture_inputs=True)
        litellm.turn_off_message_logging = True
        litellm.callbacks = [handler]
        litellm.embedding(
            model="openai/text-embedding-3-small",
            input=["secret one", "secret two"],
            mock_response=[0.1, 0.2, 0.3],
        )
        client.flush()

        span = span_exporter.get_finished_spans()[0]
        texts = span.attributes.get("gen_ai.embeddings.input_texts")
        assert texts is None or all(not text for text in texts)
    finally:
        litellm.turn_off_message_logging = prev_redaction
        litellm.callbacks = prev_callbacks
        client.shutdown()


def test_reasoning_content_mapped_to_thinking_output() -> None:
    """Flat reasoning_content becomes a THINKING part ordered before TEXT."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            response=_make_slo_response(
                content="The answer is 42.",
                reasoning_content="Let me work through this step by step.",
            )
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        parts = gen.output[0].parts
        assert [p.kind for p in parts] == [PartKind.THINKING, PartKind.TEXT]
        assert parts[0].thinking == "Let me work through this step by step."
        assert parts[1].text == "The answer is 42."
    finally:
        client.shutdown()


def test_thinking_blocks_including_redacted() -> None:
    """thinking_blocks produce a THINKING part per block, reading thinking/data."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            response=_make_slo_response(
                content="Done.",
                thinking_blocks=[
                    {"type": "thinking", "thinking": "First I consider X.", "signature": "sig"},
                    {"type": "redacted_thinking", "data": "encrypted-blob"},
                ],
            )
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        parts = gen.output[0].parts
        assert [p.kind for p in parts] == [PartKind.THINKING, PartKind.THINKING, PartKind.TEXT]
        assert parts[0].thinking == "First I consider X."
        assert parts[1].thinking == "encrypted-blob"
        assert parts[2].text == "Done."
    finally:
        client.shutdown()


def test_thinking_blocks_preferred_over_reasoning_content() -> None:
    """When both are present, thinking_blocks win and reasoning_content is not duplicated."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            response=_make_slo_response(
                content="Result.",
                reasoning_content="Block one. Block two.",
                thinking_blocks=[
                    {"type": "thinking", "thinking": "Block one."},
                    {"type": "thinking", "thinking": "Block two."},
                ],
            )
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        thinking_parts = [p for p in gen.output[0].parts if p.kind == PartKind.THINKING]
        assert [p.thinking for p in thinking_parts] == ["Block one.", "Block two."]
    finally:
        client.shutdown()


def test_thinking_dropped_when_outputs_disabled() -> None:
    """capture_outputs=False omits THINKING parts."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client, capture_outputs=False)
        slo = _base_slo(
            response=_make_slo_response(
                content="Hi",
                reasoning_content="Secret reasoning.",
            )
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert len(gen.output) == 0
    finally:
        client.shutdown()


def test_input_assistant_reasoning_mapped_to_thinking() -> None:
    """Input assistant message reasoning_content becomes a THINKING part before TEXT."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client)
        slo = _base_slo(
            messages=[
                {"role": "user", "content": "Hello"},
                {
                    "role": "assistant",
                    "content": "Sure.",
                    "reasoning_content": "They greeted me.",
                },
            ]
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assistant_msg = gen.input[1]
        assert assistant_msg.role == MessageRole.ASSISTANT
        assert [p.kind for p in assistant_msg.parts] == [PartKind.THINKING, PartKind.TEXT]
        assert assistant_msg.parts[0].thinking == "They greeted me."
        assert assistant_msg.parts[1].text == "Sure."
    finally:
        client.shutdown()


def test_input_assistant_thinking_dropped_when_inputs_disabled() -> None:
    """capture_inputs=False omits THINKING parts from input assistant messages."""
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        handler = Agento11yLiteLLMLogger(client=client, capture_inputs=False)
        slo = _base_slo(
            messages=[
                {"role": "user", "content": "Hello"},
                {
                    "role": "assistant",
                    "content": "Sure.",
                    "reasoning_content": "Secret reasoning.",
                },
            ]
        )
        handler.log_success_event(
            kwargs=_make_kwargs(slo),
            response_obj=None,
            start_time=_START,
            end_time=_END,
        )
        client.flush()

        gen = exporter.requests[0].generations[0]
        assert len(gen.input) == 0
    finally:
        client.shutdown()
