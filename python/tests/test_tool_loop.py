"""Tests for Client.execute_tool_calls."""

from __future__ import annotations

import json
from datetime import timedelta

import pytest
from agento11y import (
    Client,
    ClientConfig,
    ContentCaptureMode,
    EmbeddingCaptureConfig,
    ExecuteToolCallsOptions,
    GenerationExportConfig,
    Message,
    MessageRole,
    Part,
    PartKind,
    ToolCall,
    create_secret_redaction_sanitizer,
    tool_call_part,
)
from conftest import CapturingGenerationExporter
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

_API_KEY = "sk-proj-" + "a" * 48


def _new_client(
    exporter: CapturingGenerationExporter,
    span_exporter: InMemorySpanExporter,
    *,
    content_capture: ContentCaptureMode = ContentCaptureMode.DEFAULT,
    generation_sanitizer=None,
) -> Client:
    tp = TracerProvider()
    tp.add_span_processor(SimpleSpanProcessor(span_exporter))
    generation_export = GenerationExportConfig(
        batch_size=10,
        flush_interval=timedelta(seconds=60),
        queue_size=10,
        max_retries=1,
        initial_backoff=timedelta(milliseconds=1),
        max_backoff=timedelta(milliseconds=1),
        payload_max_bytes=4 << 20,
    )
    return Client(
        ClientConfig(
            tracer=tp.get_tracer("test"),
            generation_export=generation_export,
            embedding_capture=EmbeddingCaptureConfig(),
            generation_exporter=exporter,
            content_capture=content_capture,
            generation_sanitizer=generation_sanitizer,
        )
    )


def _tool_spans(exporter: InMemorySpanExporter) -> list[str]:
    return [s.name for s in exporter.get_finished_spans() if s.name.startswith("execute_tool ")]


def test_execute_tool_calls_happy_path_two_tools() -> None:
    exporter = CapturingGenerationExporter()
    spans = InMemorySpanExporter()
    client = _new_client(exporter, spans)
    try:
        messages = [
            Message(
                role=MessageRole.ASSISTANT,
                parts=[
                    tool_call_part(
                        ToolCall(id="c1", name="weather", input_json=json.dumps({"city": "Paris"}).encode())
                    ),
                    tool_call_part(ToolCall(id="c2", name="math", input_json=json.dumps({"a": 1, "b": 2}).encode())),
                ],
            )
        ]

        def run(name: str, args: object) -> object:
            if name == "weather":
                return {"temp_c": 18}
            return args

        opts = ExecuteToolCallsOptions(
            conversation_id="conv-loop",
            agent_name="agent-x",
            agent_version="1.0.0",
            request_model="gpt-test",
            request_provider="openai",
        )
        out = client.execute_tool_calls(messages, run, options=opts)
        assert len(out) == 2
        assert out[0].role == MessageRole.TOOL
        assert out[0].name == "weather"
        assert out[0].parts[0].tool_result.tool_call_id == "c1"
        assert out[0].parts[0].tool_result.name == "weather"
        assert out[0].parts[0].tool_result.content_json == b'{"temp_c": 18}'
        assert out[1].parts[0].tool_result.tool_call_id == "c2"
        assert json.loads(out[1].parts[0].tool_result.content_json.decode()) == {"a": 1, "b": 2}

        names = _tool_spans(spans)
        assert names.count("execute_tool weather") == 1
        assert names.count("execute_tool math") == 1
    finally:
        client.shutdown()


def test_execute_tool_calls_executor_error() -> None:
    exporter = CapturingGenerationExporter()
    spans = InMemorySpanExporter()
    client = _new_client(exporter, spans)
    try:
        messages = [
            Message(
                role=MessageRole.ASSISTANT,
                parts=[
                    tool_call_part(ToolCall(id="c1", name="boom", input_json=b"{}")),
                ],
            )
        ]

        def run(_name: str, _args: object) -> object:
            raise RuntimeError("tool failed")

        out = client.execute_tool_calls(messages, run, options=ExecuteToolCallsOptions())
        assert len(out) == 1
        tr = out[0].parts[0].tool_result
        assert tr.is_error is True
        assert "tool failed" in tr.content
        err_spans = [s for s in spans.get_finished_spans() if s.name == "execute_tool boom"]
        assert len(err_spans) == 1
        assert err_spans[0].status.status_code.name == "ERROR"
    finally:
        client.shutdown()


@pytest.mark.parametrize(
    ("content_capture", "generation_sanitizer", "exception_message"),
    [
        pytest.param(ContentCaptureMode.DEFAULT, None, f"upstream rejected {_API_KEY}", id="default"),
        pytest.param(
            ContentCaptureMode.DEFAULT,
            create_secret_redaction_sanitizer(),
            "upstream rejected [REDACTED:openai-project-key]",
            id="sanitizer",
        ),
        pytest.param(ContentCaptureMode.METADATA_ONLY, None, None, id="metadata_only"),
    ],
)
def test_execute_tool_calls_records_the_executor_error_only_as_the_recorder_does(
    content_capture: ContentCaptureMode, generation_sanitizer, exception_message: str | None
) -> None:
    exporter = CapturingGenerationExporter()
    spans = InMemorySpanExporter()
    client = _new_client(exporter, spans, content_capture=content_capture, generation_sanitizer=generation_sanitizer)
    try:
        messages = [
            Message(
                role=MessageRole.ASSISTANT,
                parts=[tool_call_part(ToolCall(id="c1", name="boom", input_json=b"{}"))],
            )
        ]

        def run(_name: str, _args: object) -> object:
            raise RuntimeError(f"upstream rejected {_API_KEY}")

        client.execute_tool_calls(messages, run, options=ExecuteToolCallsOptions())

        span = next(s for s in spans.get_finished_spans() if s.name == "execute_tool boom")
        assert span.status.status_code.name == "ERROR"
        if exception_message is None:
            assert len(span.events) == 0
            assert span.status.description == span.attributes["error.category"]
        else:
            assert len(span.events) == 1
            assert span.events[0].attributes["exception.message"] == exception_message
        if exception_message is None or generation_sanitizer is not None:
            span_text = json.dumps(
                [dict(span.attributes), span.status.description, [dict(e.attributes) for e in span.events]]
            )
            assert _API_KEY not in span_text
    finally:
        client.shutdown()


def test_execute_tool_calls_nests_executor_spans_under_tool_span() -> None:
    """Spans created inside the executor must nest under the execute_tool span, not become
    flat siblings of it (i.e. the executor runs in the tool span's context)."""
    exporter = CapturingGenerationExporter()
    spans = InMemorySpanExporter()
    tp = TracerProvider()
    tp.add_span_processor(SimpleSpanProcessor(spans))
    tracer = tp.get_tracer("test")
    client = _new_client(exporter, spans)
    try:
        messages = [
            Message(
                role=MessageRole.ASSISTANT,
                parts=[tool_call_part(ToolCall(id="c1", name="weather", input_json=b"{}"))],
            )
        ]

        def run(_name: str, _args: object) -> object:
            # An RPC-like span the executor opens while doing its work.
            with tracer.start_as_current_span("downstream.rpc"):
                pass
            return {"ok": True}

        client.execute_tool_calls(messages, run, options=ExecuteToolCallsOptions())

        finished = {s.name: s for s in spans.get_finished_spans()}
        tool_span = finished["execute_tool weather"]
        child_span = finished["downstream.rpc"]
        assert child_span.parent is not None
        assert child_span.parent.span_id == tool_span.context.span_id
        assert child_span.context.trace_id == tool_span.context.trace_id
    finally:
        client.shutdown()


def test_execute_tool_calls_no_tool_parts() -> None:
    exporter = CapturingGenerationExporter()
    spans = InMemorySpanExporter()
    client = _new_client(exporter, spans)
    try:
        messages = [Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="hi")])]
        out = client.execute_tool_calls(messages, lambda *_: None)
        assert out == []
        assert _tool_spans(spans) == []
    finally:
        client.shutdown()


def test_execute_tool_calls_single_tool() -> None:
    exporter = CapturingGenerationExporter()
    spans = InMemorySpanExporter()
    client = _new_client(exporter, spans)
    try:
        messages = [
            Message(
                role=MessageRole.ASSISTANT,
                parts=[tool_call_part(ToolCall(id="id1", name="echo", input_json=b'{"x":1}'))],
            )
        ]
        out = client.execute_tool_calls(messages, lambda _n, a: a, options=ExecuteToolCallsOptions())
        assert len(out) == 1
        assert out[0].parts[0].tool_result.tool_call_id == "id1"
        assert _tool_spans(spans) == ["execute_tool echo"]
    finally:
        client.shutdown()


def test_execute_tool_calls_empty_messages() -> None:
    exporter = CapturingGenerationExporter()
    spans = InMemorySpanExporter()
    client = _new_client(exporter, spans)
    try:
        assert client.execute_tool_calls([], lambda *_: None) == []
        assert client.execute_tool_calls((), lambda *_: None) == []
        assert _tool_spans(spans) == []
    finally:
        client.shutdown()


def test_execute_tool_calls_skips_empty_tool_name() -> None:
    exporter = CapturingGenerationExporter()
    spans = InMemorySpanExporter()
    client = _new_client(exporter, spans)
    try:
        messages = [
            Message(
                role=MessageRole.ASSISTANT,
                parts=[tool_call_part(ToolCall(id="x", name="   ", input_json=b"{}"))],
            )
        ]
        assert client.execute_tool_calls(messages, lambda *_: 1) == []
        assert _tool_spans(spans) == []
    finally:
        client.shutdown()
