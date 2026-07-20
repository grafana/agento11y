"""Tests for ContentCaptureMode: resolution, stripping, context propagation, tool spans."""

from __future__ import annotations

import concurrent.futures
import copy
import json
import socket
import threading
from dataclasses import dataclass
from datetime import timedelta
from http.server import BaseHTTPRequestHandler, HTTPServer

import grpc
import pytest
from agento11y import (
    ApiConfig,
    Client,
    ClientConfig,
    ContentCaptureMode,
    ConversationRatingInput,
    ConversationRatingValue,
    EmbeddingCaptureConfig,
    EmbeddingResult,
    EmbeddingStart,
    Generation,
    GenerationExportConfig,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    TokenUsage,
    ToolCall,
    ToolDefinition,
    ToolExecutionStart,
    ToolResult,
    validate_generation,
)
from agento11y.context import content_capture_mode_from_context, with_content_capture_mode
from agento11y.internal.gen.agento11y.v1 import generation_ingest_pb2 as agento11y_pb2
from agento11y.internal.gen.agento11y.v1 import generation_ingest_pb2_grpc as agento11y_pb2_grpc
from conftest import CapturingGenerationExporter
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

_METADATA_KEY = "agento11y.sdk.content_capture_mode"
# Sentinel substring guaranteed not to appear in any error category classifier
# output. If it leaks onto a span, the redaction is broken.
_LEAK_MARKER = "ignore previous instructions"


class _CapturingGenerationServicer(agento11y_pb2_grpc.GenerationIngestServiceServicer):
    """Real gRPC servicer that records exported generation protos."""

    def __init__(self) -> None:
        self.requests: list[agento11y_pb2.ExportGenerationsRequest] = []
        self._lock = threading.Lock()

    def ExportGenerations(self, request, _context):  # noqa: N802
        with self._lock:
            self.requests.append(copy.deepcopy(request))
        return agento11y_pb2.ExportGenerationsResponse(
            results=[
                agento11y_pb2.ExportGenerationResult(generation_id=generation.id, accepted=True)
                for generation in request.generations
            ]
        )


class _ContentCaptureEnv:
    """Real-gRPC content-capture test env.

    Spins up an in-process gRPC server that captures :class:`agento11y_pb2.Generation`
    payloads as they actually leave the SDK, plus an :class:`InMemorySpanExporter`
    for OTel span assertions. Use this when a test needs to assert on both the
    proto export and the span path (the proto/span split that
    :class:`ContentCaptureMode.FULL_WITH_METADATA_SPANS` introduces).
    """

    def __init__(self, **client_overrides) -> None:
        self.servicer = _CapturingGenerationServicer()
        self._grpc_server = grpc.server(concurrent.futures.ThreadPoolExecutor(max_workers=2))
        agento11y_pb2_grpc.add_GenerationIngestServiceServicer_to_server(self.servicer, self._grpc_server)

        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()
        self._grpc_server.add_insecure_port(f"127.0.0.1:{port}")
        self._grpc_server.start()

        self.span_exporter = InMemorySpanExporter()
        self._provider = TracerProvider()
        self._provider.add_span_processor(SimpleSpanProcessor(self.span_exporter))

        kwargs = dict(
            tracer=self._provider.get_tracer("agento11y-content-capture-test"),
            generation_export=GenerationExportConfig(
                protocol="grpc",
                endpoint=f"127.0.0.1:{port}",
                insecure=True,
                batch_size=1,
                flush_interval=timedelta(hours=1),
                queue_size=10,
                max_retries=1,
                initial_backoff=timedelta(milliseconds=1),
                max_backoff=timedelta(milliseconds=2),
            ),
        )
        kwargs.update(client_overrides)
        self.client = Client(ClientConfig(**kwargs))
        self._closed = False

    def __enter__(self) -> _ContentCaptureEnv:
        return self

    def __exit__(self, *_exc) -> None:
        self.shutdown()

    def shutdown(self) -> None:
        """Flush the client and tear the env down. Safe to call repeatedly."""
        if self._closed:
            return
        self._closed = True
        self.client.shutdown()
        self._provider.shutdown()
        self._grpc_server.stop(grace=0)

    def single_generation(self) -> agento11y_pb2.Generation:
        """The one and only proto generation the gRPC server received."""
        self.shutdown()  # ensure flush
        assert len(self.servicer.requests) == 1, f"expected 1 export request, got {len(self.servicer.requests)}"
        assert len(self.servicer.requests[0].generations) == 1
        return self.servicer.requests[0].generations[0]

    def generation_span(self):
        return self._single_span("generate")

    def embedding_span(self):
        return self._single_span("embeddings")

    def tool_span(self):
        return self._single_span("execute_tool")

    def _single_span(self, name_prefix: str):
        spans = [s for s in self.span_exporter.get_finished_spans() if s.name.startswith(name_prefix)]
        assert spans, f"no span starting with {name_prefix!r}"
        return spans[-1]


def _assert_span_error_redacted(span, expected_error_type: str) -> None:
    """Assert a span has the error type set and no raw provider text."""
    assert span.status.status_code.name == "ERROR"
    assert _LEAK_MARKER not in (span.status.description or "")
    for event in span.events:
        for value in event.attributes.values():
            assert _LEAK_MARKER not in str(value), f"span event {event.name!r} leaks raw error: {value!r}"
    assert span.attributes.get("error.type") == expected_error_type


def _new_client(exporter: CapturingGenerationExporter, tracer=None, **overrides) -> Client:
    generation_export = GenerationExportConfig(
        batch_size=overrides.get("batch_size", 10),
        flush_interval=overrides.get("flush_interval", timedelta(seconds=60)),
        queue_size=overrides.get("queue_size", 10),
        max_retries=overrides.get("max_retries", 1),
        initial_backoff=overrides.get("initial_backoff", timedelta(milliseconds=1)),
        max_backoff=overrides.get("max_backoff", timedelta(milliseconds=1)),
    )
    client_config_kwargs = dict(
        tracer=tracer,
        generation_export=generation_export,
        generation_exporter=exporter,
        content_capture=overrides.get("content_capture", ContentCaptureMode.DEFAULT),
        content_capture_resolver=overrides.get("content_capture_resolver", None),
    )
    if "embedding_capture" in overrides:
        client_config_kwargs["embedding_capture"] = overrides["embedding_capture"]
    return Client(ClientConfig(**client_config_kwargs))


def _seed(content_capture: ContentCaptureMode = ContentCaptureMode.DEFAULT) -> GenerationStart:
    return GenerationStart(
        model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
        content_capture=content_capture,
    )


def _full_generation() -> Generation:
    return Generation(
        system_prompt="You are helpful.",
        input=[
            Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="What is the weather?")]),
            Message(
                role=MessageRole.TOOL,
                parts=[
                    Part(
                        kind=PartKind.TOOL_RESULT,
                        tool_result=ToolResult(
                            tool_call_id="call_1",
                            name="weather",
                            content="sunny 18C",
                            content_json=b'{"temp":18}',
                        ),
                    )
                ],
            ),
        ],
        output=[
            Message(
                role=MessageRole.ASSISTANT,
                parts=[
                    Part(kind=PartKind.THINKING, thinking="let me think about weather"),
                    Part(
                        kind=PartKind.TOOL_CALL,
                        tool_call=ToolCall(name="weather", id="call_1", input_json=b'{"city":"Paris"}'),
                    ),
                    Part(kind=PartKind.TEXT, text="It's 18C and sunny in Paris."),
                ],
            )
        ],
        tools=[
            ToolDefinition(
                name="weather", description="Get weather info", type="function", input_schema_json=b'{"type":"object"}'
            ),
        ],
        usage=TokenUsage(input_tokens=120, output_tokens=42),
        stop_reason="end_turn",
        conversation_title="Weather chat",
        call_error="rate limit exceeded",
        artifacts=[],
        metadata={
            "agento11y.sdk.name": "sdk-python",
            "call_error": "rate limit exceeded",
            "agento11y.conversation.title": "Weather chat",
        },
    )


# ---------------------------------------------------------------------------
# Mode resolution
# ---------------------------------------------------------------------------


class TestContentCaptureModeResolution:
    @pytest.mark.parametrize(
        "client_mode, gen_mode, want_marker",
        [
            (ContentCaptureMode.DEFAULT, ContentCaptureMode.DEFAULT, "no_tool_content"),
            (ContentCaptureMode.METADATA_ONLY, ContentCaptureMode.DEFAULT, "metadata_only"),
            (ContentCaptureMode.FULL, ContentCaptureMode.METADATA_ONLY, "metadata_only"),
            (ContentCaptureMode.METADATA_ONLY, ContentCaptureMode.FULL, "full"),
            (ContentCaptureMode.FULL, ContentCaptureMode.DEFAULT, "full"),
        ],
    )
    def test_generation_content_capture(self, client_mode, gen_mode, want_marker):
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=client_mode)
        try:
            rec = client.start_generation(_seed(gen_mode))
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="Hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="Hi")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            assert rec.err() is None

            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == want_marker
        finally:
            client.shutdown()

    def test_default_resolution_is_no_tool_content(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter)
        try:
            rec = client.start_generation(_seed())
            rec.set_result(
                output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="ok")])],
                usage=TokenUsage(input_tokens=1, output_tokens=1),
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "no_tool_content"
            # Content should NOT be stripped under no_tool_content
            assert gen.output[0].parts[0].text == "ok"
        finally:
            client.shutdown()


# ---------------------------------------------------------------------------
# Content stripping (METADATA_ONLY)
# ---------------------------------------------------------------------------


class TestContentStripping:
    def test_metadata_only_strips_sensitive_content(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.METADATA_ONLY)
        try:
            rec = client.start_generation(_seed())
            rec.set_result(_full_generation())
            rec.end()
            assert rec.err() is None

            gen = rec.last_generation

            # Stripped
            assert gen.system_prompt == ""
            assert gen.input[0].parts[0].text == ""
            assert gen.output[0].parts[0].thinking == ""
            assert gen.output[0].parts[1].tool_call.input_json == b""
            assert gen.output[0].parts[2].text == ""
            assert gen.input[1].parts[0].tool_result.content == ""
            assert gen.input[1].parts[0].tool_result.content_json == b""
            assert gen.tools[0].description == ""
            assert gen.tools[0].input_schema_json == b""
            assert gen.conversation_title == ""
            assert "agento11y.conversation.title" not in gen.metadata

            # Preserved
            assert len(gen.input) == 2
            assert len(gen.output) == 1
            assert len(gen.output[0].parts) == 3
            assert gen.input[0].role == MessageRole.USER
            assert gen.output[0].parts[0].kind == PartKind.THINKING
            assert gen.output[0].parts[1].tool_call.name == "weather"
            assert gen.output[0].parts[1].tool_call.id == "call_1"
            assert gen.input[1].parts[0].tool_result.tool_call_id == "call_1"
            assert gen.input[1].parts[0].tool_result.name == "weather"
            assert gen.tools[0].name == "weather"
            assert gen.usage.input_tokens == 120
            assert gen.usage.output_tokens == 42
            assert gen.stop_reason == "end_turn"
            assert gen.metadata["agento11y.sdk.name"] == "sdk-python"
        finally:
            client.shutdown()

    def test_metadata_only_replaces_call_error_with_category(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.METADATA_ONLY)
        try:
            rec = client.start_generation(_seed())
            rec.set_call_error(RuntimeError("429 rate limit exceeded"))
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="world")])],
                    usage=TokenUsage(input_tokens=1, output_tokens=1),
                )
            )
            rec.end()

            gen = rec.last_generation
            assert gen.call_error == "rate_limit"
            assert "call_error" not in gen.metadata
        finally:
            client.shutdown()

    def test_metadata_only_falls_back_to_sdk_error(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.METADATA_ONLY)
        try:
            rec = client.start_generation(_seed())
            rec.set_call_error(RuntimeError("something went wrong"))
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="world")])],
                    usage=TokenUsage(input_tokens=1, output_tokens=1),
                )
            )
            rec.end()

            gen = rec.last_generation
            assert gen.call_error == "sdk_error"
        finally:
            client.shutdown()

    def test_metadata_only_strips_conversation_title_from_span(self):
        span_exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(span_exporter))
        tracer = provider.get_tracer("agento11y-test")
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, tracer=tracer, content_capture=ContentCaptureMode.METADATA_ONLY)
        try:
            rec = client.start_generation(
                GenerationStart(
                    model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
                    conversation_title="Sensitive topic",
                )
            )
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="Hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="Hi")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()

            gen_span = next(s for s in span_exporter.get_finished_spans() if s.name.startswith("generate"))
            assert "agento11y.conversation.title" not in gen_span.attributes
        finally:
            client.shutdown()
            provider.shutdown()

    def test_full_mode_preserves_all_content(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.FULL)
        try:
            rec = client.start_generation(_seed())
            rec.set_result(
                Generation(
                    system_prompt="You are helpful.",
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="Hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="Hi")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "full"
            assert gen.system_prompt == "You are helpful."
            assert gen.input[0].parts[0].text == "Hello"
            assert gen.output[0].parts[0].text == "Hi"
        finally:
            client.shutdown()


# ---------------------------------------------------------------------------
# Per-generation override
# ---------------------------------------------------------------------------


class TestPerGenerationOverride:
    def test_per_generation_full_overrides_client_metadata_only(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.METADATA_ONLY)
        try:
            rec = client.start_generation(_seed(ContentCaptureMode.FULL))
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="Hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="Hi")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "full"
            assert gen.input[0].parts[0].text == "Hello"
        finally:
            client.shutdown()

    def test_per_generation_metadata_only_overrides_client_full(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.FULL)
        try:
            rec = client.start_generation(_seed(ContentCaptureMode.METADATA_ONLY))
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="Hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="Hi")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "metadata_only"
            assert gen.input[0].parts[0].text == ""
        finally:
            client.shutdown()


# ---------------------------------------------------------------------------
# Resolver callback
# ---------------------------------------------------------------------------


class TestResolverCallback:
    def test_resolver_metadata_only_overrides_client_full(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(
            exporter,
            content_capture=ContentCaptureMode.FULL,
            content_capture_resolver=lambda _meta: ContentCaptureMode.METADATA_ONLY,
        )
        try:
            rec = client.start_generation(_seed())
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="world")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "metadata_only"
            assert gen.input[0].parts[0].text == ""
        finally:
            client.shutdown()

    def test_per_generation_overrides_resolver(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(
            exporter,
            content_capture=ContentCaptureMode.DEFAULT,
            content_capture_resolver=lambda _meta: ContentCaptureMode.METADATA_ONLY,
        )
        try:
            rec = client.start_generation(_seed(ContentCaptureMode.FULL))
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="world")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "full"
            assert gen.input[0].parts[0].text == "hello"
        finally:
            client.shutdown()

    def test_resolver_default_defers_to_client(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(
            exporter,
            content_capture=ContentCaptureMode.METADATA_ONLY,
            content_capture_resolver=lambda _meta: ContentCaptureMode.DEFAULT,
        )
        try:
            rec = client.start_generation(_seed())
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="world")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "metadata_only"
            assert gen.input[0].parts[0].text == ""
        finally:
            client.shutdown()

    def test_resolver_exception_fails_closed_to_metadata_only(self):
        def bad_resolver(_meta):
            raise RuntimeError("resolver bug")

        exporter = CapturingGenerationExporter()
        client = _new_client(
            exporter,
            content_capture=ContentCaptureMode.FULL,
            content_capture_resolver=bad_resolver,
        )
        try:
            rec = client.start_generation(_seed())
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="world")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "metadata_only"
            assert gen.input[0].parts[0].text == ""
        finally:
            client.shutdown()

    def test_resolver_returning_wrong_type_fails_closed(self):
        """A resolver returning a plain string instead of ContentCaptureMode should fail closed."""
        exporter = CapturingGenerationExporter()
        client = _new_client(
            exporter,
            content_capture=ContentCaptureMode.FULL,
            content_capture_resolver=lambda _meta: "metadata_only",
        )
        try:
            rec = client.start_generation(_seed())
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="world")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "metadata_only"
            assert gen.input[0].parts[0].text == ""
        finally:
            client.shutdown()

    def test_resolver_full_overrides_client_metadata_only(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(
            exporter,
            content_capture=ContentCaptureMode.METADATA_ONLY,
            content_capture_resolver=lambda _meta: ContentCaptureMode.FULL,
        )
        try:
            rec = client.start_generation(_seed())
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="world")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
            )
            rec.end()
            gen = rec.last_generation
            assert gen.metadata[_METADATA_KEY] == "full"
            assert gen.input[0].parts[0].text == "hello"
        finally:
            client.shutdown()


# ---------------------------------------------------------------------------
# Tool span content capture
# ---------------------------------------------------------------------------


class TestToolContentCapture:
    def _make_tool_client(self, content_capture=ContentCaptureMode.DEFAULT, **kw):
        span_exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(span_exporter))
        tracer = provider.get_tracer("agento11y-test")
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, tracer=tracer, content_capture=content_capture, **kw)
        return client, span_exporter, provider

    def _get_tool_span(self, span_exporter):
        for span in span_exporter.get_finished_spans():
            if span.name.startswith("execute_tool"):
                return span
        raise AssertionError("tool span not found")

    def test_client_full_includes_content(self):
        client, span_exporter, provider = self._make_tool_client(ContentCaptureMode.FULL)
        try:
            with client.start_tool_execution(ToolExecutionStart(tool_name="test_tool", include_content=False)) as rec:
                rec.set_result(arguments="args", result="result")

            span = self._get_tool_span(span_exporter)
            assert span.attributes.get("gen_ai.tool.call.arguments") is not None
        finally:
            client.shutdown()
            provider.shutdown()

    def test_client_metadata_only_suppresses_content(self):
        client, span_exporter, provider = self._make_tool_client(ContentCaptureMode.METADATA_ONLY)
        try:
            with client.start_tool_execution(
                ToolExecutionStart(tool_name="test_tool", include_content=True, conversation_title="Sensitive topic")
            ) as rec:
                rec.set_result(arguments="args", result="result")

            span = self._get_tool_span(span_exporter)
            assert span.attributes.get("gen_ai.tool.call.arguments") is None
            assert "agento11y.conversation.title" not in span.attributes
        finally:
            client.shutdown()
            provider.shutdown()

    def test_client_default_legacy_false_suppresses(self):
        client, span_exporter, provider = self._make_tool_client(ContentCaptureMode.DEFAULT)
        try:
            with client.start_tool_execution(ToolExecutionStart(tool_name="test_tool", include_content=False)) as rec:
                rec.set_result(arguments="args", result="result")

            span = self._get_tool_span(span_exporter)
            assert span.attributes.get("gen_ai.tool.call.arguments") is None
        finally:
            client.shutdown()
            provider.shutdown()

    def test_client_default_legacy_true_includes(self):
        client, span_exporter, provider = self._make_tool_client(ContentCaptureMode.DEFAULT)
        try:
            with client.start_tool_execution(ToolExecutionStart(tool_name="test_tool", include_content=True)) as rec:
                rec.set_result(arguments="args", result="result")

            span = self._get_tool_span(span_exporter)
            assert span.attributes.get("gen_ai.tool.call.arguments") is not None
        finally:
            client.shutdown()
            provider.shutdown()

    def test_per_tool_full_overrides_client_metadata_only(self):
        client, span_exporter, provider = self._make_tool_client(ContentCaptureMode.METADATA_ONLY)
        try:
            with client.start_tool_execution(
                ToolExecutionStart(
                    tool_name="test_tool",
                    content_capture=ContentCaptureMode.FULL,
                    include_content=True,
                )
            ) as rec:
                rec.set_result(arguments="args", result="result")

            span = self._get_tool_span(span_exporter)
            assert span.attributes.get("gen_ai.tool.call.arguments") is not None
        finally:
            client.shutdown()
            provider.shutdown()

    def test_per_tool_metadata_only_overrides_client_full(self):
        client, span_exporter, provider = self._make_tool_client(ContentCaptureMode.FULL)
        try:
            with client.start_tool_execution(
                ToolExecutionStart(
                    tool_name="test_tool",
                    content_capture=ContentCaptureMode.METADATA_ONLY,
                    include_content=True,
                )
            ) as rec:
                rec.set_result(arguments="args", result="result")

            span = self._get_tool_span(span_exporter)
            assert span.attributes.get("gen_ai.tool.call.arguments") is None
        finally:
            client.shutdown()
            provider.shutdown()

    def test_include_content_ignored_under_metadata_only(self):
        client, span_exporter, provider = self._make_tool_client(ContentCaptureMode.METADATA_ONLY)
        try:
            with client.start_tool_execution(ToolExecutionStart(tool_name="test_tool", include_content=True)) as rec:
                rec.set_result(arguments="args", result="result")

            span = self._get_tool_span(span_exporter)
            assert span.attributes.get("gen_ai.tool.call.arguments") is None
        finally:
            client.shutdown()
            provider.shutdown()

    def test_context_default_defers_to_client_full(self):
        """with_content_capture_mode(DEFAULT) should fall through to client FULL, not suppress content."""
        client, span_exporter, provider = self._make_tool_client(ContentCaptureMode.FULL)
        try:
            with with_content_capture_mode(ContentCaptureMode.DEFAULT):
                with client.start_tool_execution(
                    ToolExecutionStart(tool_name="test_tool", include_content=False)
                ) as rec:
                    rec.set_result(arguments="args", result="result")

            span = self._get_tool_span(span_exporter)
            assert span.attributes.get("gen_ai.tool.call.arguments") is not None
        finally:
            client.shutdown()
            provider.shutdown()

    def test_context_default_defers_to_client_metadata_only(self):
        """with_content_capture_mode(DEFAULT) should fall through to client METADATA_ONLY, not re-enable via legacy."""
        client, span_exporter, provider = self._make_tool_client(ContentCaptureMode.METADATA_ONLY)
        try:
            with with_content_capture_mode(ContentCaptureMode.DEFAULT):
                with client.start_tool_execution(
                    ToolExecutionStart(tool_name="test_tool", include_content=True)
                ) as rec:
                    rec.set_result(arguments="args", result="result")

            span = self._get_tool_span(span_exporter)
            assert span.attributes.get("gen_ai.tool.call.arguments") is None
        finally:
            client.shutdown()
            provider.shutdown()


# ---------------------------------------------------------------------------
# Context propagation (parent generation → child tool)
# ---------------------------------------------------------------------------


class TestContextPropagation:
    def test_generation_context_manager_sets_content_capture_mode(self):
        exporter = CapturingGenerationExporter()
        span_exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(span_exporter))
        tracer = provider.get_tracer("agento11y-test")
        client = _new_client(exporter, tracer=tracer, content_capture=ContentCaptureMode.METADATA_ONLY)

        try:
            with client.start_generation(_seed()) as gen_rec:
                # Within the generation context, content capture mode should be set
                mode = content_capture_mode_from_context()
                assert mode == ContentCaptureMode.METADATA_ONLY

                # Tool execution within this context should inherit the mode
                with client.start_tool_execution(
                    ToolExecutionStart(tool_name="test_tool", include_content=True)
                ) as tool_rec:
                    tool_rec.set_result(arguments="args", result="result")

                gen_rec.set_result(
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="ok")])],
                    usage=TokenUsage(input_tokens=1, output_tokens=1),
                )

            # Tool span should NOT have content (inherited MetadataOnly suppresses)
            for span in span_exporter.get_finished_spans():
                if span.name.startswith("execute_tool"):
                    assert span.attributes.get("gen_ai.tool.call.arguments") is None
                    break
            else:
                raise AssertionError("tool span not found")
        finally:
            client.shutdown()
            provider.shutdown()

    def test_generation_full_context_allows_tool_content(self):
        exporter = CapturingGenerationExporter()
        span_exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(span_exporter))
        tracer = provider.get_tracer("agento11y-test")
        client = _new_client(exporter, tracer=tracer, content_capture=ContentCaptureMode.METADATA_ONLY)

        try:
            # Per-generation override to FULL
            with client.start_generation(_seed(ContentCaptureMode.FULL)) as gen_rec:
                with client.start_tool_execution(
                    ToolExecutionStart(tool_name="test_tool", include_content=True)
                ) as tool_rec:
                    tool_rec.set_result(arguments="args", result="result")

                gen_rec.set_result(
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="ok")])],
                    usage=TokenUsage(input_tokens=1, output_tokens=1),
                )

            for span in span_exporter.get_finished_spans():
                if span.name.startswith("execute_tool"):
                    assert span.attributes.get("gen_ai.tool.call.arguments") is not None
                    break
            else:
                raise AssertionError("tool span not found")
        finally:
            client.shutdown()
            provider.shutdown()

    def test_with_content_capture_mode_context_manager(self):
        assert content_capture_mode_from_context() is None

        with with_content_capture_mode(ContentCaptureMode.FULL):
            assert content_capture_mode_from_context() == ContentCaptureMode.FULL

        assert content_capture_mode_from_context() is None

    def test_recorder_inside_with_content_capture_mode_preserves_override(self):
        """A recorder starting and ending inside with_content_capture_mode must not clobber the user override."""
        exporter = CapturingGenerationExporter()
        span_exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(span_exporter))
        tracer = provider.get_tracer("agento11y-test")
        client = _new_client(exporter, tracer=tracer, content_capture=ContentCaptureMode.METADATA_ONLY)

        try:
            with with_content_capture_mode(ContentCaptureMode.FULL):
                assert content_capture_mode_from_context() == ContentCaptureMode.FULL

                with client.start_generation(_seed()) as gen_rec:
                    gen_rec.set_result(
                        output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="ok")])],
                        usage=TokenUsage(input_tokens=1, output_tokens=1),
                    )

                # After the recorder ends, the context override must still be FULL
                assert content_capture_mode_from_context() == ContentCaptureMode.FULL

                # Tool execution here should still see FULL from the context
                with client.start_tool_execution(
                    ToolExecutionStart(tool_name="test_tool", include_content=False)
                ) as tool_rec:
                    tool_rec.set_result(arguments="args", result="result")

                for span in span_exporter.get_finished_spans():
                    if span.name.startswith("execute_tool"):
                        assert span.attributes.get("gen_ai.tool.call.arguments") is not None
                        break
                else:
                    raise AssertionError("tool span not found")

            assert content_capture_mode_from_context() is None
        finally:
            client.shutdown()
            provider.shutdown()

    def test_with_content_capture_mode_overrides_generation_to_metadata_only(self):
        """with_content_capture_mode(METADATA_ONLY) should strip generation content even when client is FULL."""
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.FULL)
        try:
            with with_content_capture_mode(ContentCaptureMode.METADATA_ONLY):
                rec = client.start_generation(_seed())
                rec.set_result(
                    Generation(
                        system_prompt="secret system prompt",
                        input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="Hello")])],
                        output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="Hi")])],
                        usage=TokenUsage(input_tokens=10, output_tokens=5),
                    )
                )
                rec.end()
                assert rec.err() is None

                gen = rec.last_generation
                assert gen.metadata[_METADATA_KEY] == "metadata_only"
                assert gen.system_prompt == ""
                assert gen.input[0].parts[0].text == ""
                assert gen.output[0].parts[0].text == ""
        finally:
            client.shutdown()

    def test_with_content_capture_mode_overrides_generation_to_full(self):
        """with_content_capture_mode(FULL) should preserve generation content even when client is METADATA_ONLY."""
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.METADATA_ONLY)
        try:
            with with_content_capture_mode(ContentCaptureMode.FULL):
                rec = client.start_generation(_seed())
                rec.set_result(
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="Hello")])],
                    usage=TokenUsage(input_tokens=10, output_tokens=5),
                )
                rec.end()
                assert rec.err() is None

                gen = rec.last_generation
                assert gen.metadata[_METADATA_KEY] == "full"
                assert gen.output[0].parts[0].text == "Hello"
        finally:
            client.shutdown()

    def test_per_recording_override_takes_priority_over_context(self):
        """GenerationStart.content_capture should override with_content_capture_mode."""
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.FULL)
        try:
            with with_content_capture_mode(ContentCaptureMode.FULL):
                rec = client.start_generation(_seed(ContentCaptureMode.METADATA_ONLY))
                rec.set_result(
                    Generation(
                        system_prompt="secret",
                        input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="Hello")])],
                        output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="Hi")])],
                        usage=TokenUsage(input_tokens=10, output_tokens=5),
                    )
                )
                rec.end()
                assert rec.err() is None

                gen = rec.last_generation
                assert gen.metadata[_METADATA_KEY] == "metadata_only"
                assert gen.system_prompt == ""
        finally:
            client.shutdown()


# ---------------------------------------------------------------------------
# Validation accepts stripped content
# ---------------------------------------------------------------------------


class TestValidationWithStrippedContent:
    def test_validation_accepts_stripped_generation(self):
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, content_capture=ContentCaptureMode.METADATA_ONLY)
        try:
            rec = client.start_generation(_seed())
            rec.set_result(_full_generation())
            rec.end()
            # No validation error
            assert rec.err() is None
        finally:
            client.shutdown()

    def test_validation_accepts_stripped_text_and_thinking(self):
        """Directly test that validate_generation accepts empty text/thinking when metadata marker is set."""
        gen = Generation(
            model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
            input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="")])],
            output=[
                Message(
                    role=MessageRole.ASSISTANT,
                    parts=[
                        Part(kind=PartKind.THINKING, thinking=""),
                        Part(kind=PartKind.TEXT, text=""),
                    ],
                )
            ],
            usage=TokenUsage(input_tokens=1, output_tokens=1),
            metadata={_METADATA_KEY: "metadata_only"},
        )
        # Should not raise
        validate_generation(gen)

    def test_validation_rejects_empty_text_without_stripped_marker(self):
        gen = Generation(
            model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
            input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="")])],
            output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="ok")])],
            usage=TokenUsage(input_tokens=1, output_tokens=1),
            metadata={},
        )
        with pytest.raises(ValueError):
            validate_generation(gen)


# ---------------------------------------------------------------------------
# Backward compatibility: include_content
# ---------------------------------------------------------------------------


class TestBackwardCompatibility:
    def test_include_content_still_works_without_content_capture(self):
        """When no ContentCaptureMode is set, include_content=True should still include content."""
        span_exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(span_exporter))
        tracer = provider.get_tracer("agento11y-test")
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, tracer=tracer)

        try:
            with client.start_tool_execution(ToolExecutionStart(tool_name="test_tool", include_content=True)) as rec:
                rec.set_result(arguments="some args", result="some result")

            span = None
            for s in span_exporter.get_finished_spans():
                if s.name.startswith("execute_tool"):
                    span = s
                    break
            assert span is not None
            assert span.attributes.get("gen_ai.tool.call.arguments") is not None
            assert span.attributes.get("gen_ai.tool.call.result") is not None
        finally:
            client.shutdown()
            provider.shutdown()

    def test_include_content_false_without_content_capture(self):
        """Default client + include_content=False → content suppressed."""
        span_exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(span_exporter))
        tracer = provider.get_tracer("agento11y-test")
        exporter = CapturingGenerationExporter()
        client = _new_client(exporter, tracer=tracer)

        try:
            with client.start_tool_execution(ToolExecutionStart(tool_name="test_tool", include_content=False)) as rec:
                rec.set_result(arguments="some args", result="some result")

            span = None
            for s in span_exporter.get_finished_spans():
                if s.name.startswith("execute_tool"):
                    span = s
                    break
            assert span is not None
            assert span.attributes.get("gen_ai.tool.call.arguments") is None
        finally:
            client.shutdown()
            provider.shutdown()


# ---------------------------------------------------------------------------
# Rating comment stripping
# ---------------------------------------------------------------------------


class TestRatingCommentStripping:
    def _make_rating_handler(self, captured):
        class _Handler(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                length = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(length)
                captured["payload"] = json.loads(body.decode("utf-8"))

                response = {
                    "rating": {
                        "rating_id": "rat-1",
                        "conversation_id": "conv-1",
                        "rating": "CONVERSATION_RATING_VALUE_BAD",
                        "created_at": "2026-04-10T12:00:00Z",
                    },
                    "summary": {
                        "total_count": 1,
                        "good_count": 0,
                        "bad_count": 1,
                        "latest_rating": "CONVERSATION_RATING_VALUE_BAD",
                        "latest_rated_at": "2026-04-10T12:00:00Z",
                        "has_bad_rating": True,
                    },
                }
                encoded = json.dumps(response).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(encoded)))
                self.end_headers()
                self.wfile.write(encoded)

            def log_message(self, _format, *_args):  # noqa: A003
                return

        return _Handler

    def test_metadata_only_strips_rating_comment(self):
        captured: dict = {}
        handler = self._make_rating_handler(captured)
        server = HTTPServer(("127.0.0.1", 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()

        client = Client(
            ClientConfig(
                content_capture=ContentCaptureMode.METADATA_ONLY,
                generation_export=GenerationExportConfig(
                    protocol="none",
                    batch_size=1,
                    flush_interval=timedelta(seconds=60),
                ),
                api=ApiConfig(endpoint=f"http://127.0.0.1:{server.server_address[1]}"),
            )
        )

        try:
            client.submit_conversation_rating(
                "conv-1",
                ConversationRatingInput(
                    rating_id="rat-1",
                    rating=ConversationRatingValue.BAD,
                    comment="this is sensitive feedback",
                ),
            )
            # Comment should have been stripped before sending
            assert "comment" not in captured["payload"] or captured["payload"].get("comment", "") == ""
        finally:
            client.shutdown()
            server.shutdown()
            server.server_close()

    def test_full_mode_preserves_rating_comment(self):
        captured: dict = {}
        handler = self._make_rating_handler(captured)
        server = HTTPServer(("127.0.0.1", 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()

        client = Client(
            ClientConfig(
                content_capture=ContentCaptureMode.FULL,
                generation_export=GenerationExportConfig(
                    protocol="none",
                    batch_size=1,
                    flush_interval=timedelta(seconds=60),
                ),
                api=ApiConfig(endpoint=f"http://127.0.0.1:{server.server_address[1]}"),
            )
        )

        try:
            client.submit_conversation_rating(
                "conv-1",
                ConversationRatingInput(
                    rating_id="rat-1",
                    rating=ConversationRatingValue.BAD,
                    comment="this should be preserved",
                ),
            )
            assert captured["payload"]["comment"] == "this should be preserved"
        finally:
            client.shutdown()
            server.shutdown()
            server.server_close()


# ---------------------------------------------------------------------------
# FULL_WITH_METADATA_SPANS — proto export full, span content omitted.
# ---------------------------------------------------------------------------


# Modes that strip content from spans. Generations carry a proto/span split;
# tools and embeddings have no separate proto export so both modes behave the
# same for them on the span path.
_STRIPPED_MODES = [ContentCaptureMode.METADATA_ONLY, ContentCaptureMode.FULL_WITH_METADATA_SPANS]


class TestFullWithMetadataSpans:
    """FULL_WITH_METADATA_SPANS keeps proto export full but drops content from OTel spans.

    Mirrors Go's ``TestConformance_FullWithMetadataSpansMode``. Uses a real gRPC
    ingest server so the proto export is asserted end-to-end, not on the
    in-memory ``Generation`` object before serialization.
    """

    def test_generation_proto_full_span_title_absent(self):
        with _ContentCaptureEnv(content_capture=ContentCaptureMode.FULL_WITH_METADATA_SPANS) as env:
            rec = env.client.start_generation(
                GenerationStart(
                    model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
                    conversation_title="Sensitive conversation",
                    system_prompt="Be helpful.",
                )
            )
            rec.set_result(
                Generation(
                    system_prompt="Be helpful.",
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello world")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="hi back")])],
                    tools=[
                        ToolDefinition(
                            name="weather",
                            description="Get weather info",
                            type="function",
                            input_schema_json=b'{"type":"object"}',
                        ),
                    ],
                    usage=TokenUsage(input_tokens=3, output_tokens=2),
                )
            )
            rec.end()
            assert rec.err() is None

            # Proto export (post-gRPC roundtrip) keeps all content untouched.
            gen = env.single_generation()
            assert gen.system_prompt == "Be helpful."
            assert gen.input[0].parts[0].text == "hello world"
            assert gen.output[0].parts[0].text == "hi back"
            assert gen.tools[0].description == "Get weather info"
            assert gen.tools[0].input_schema_json == b'{"type":"object"}'
            assert gen.metadata.fields[_METADATA_KEY].string_value == "full_with_metadata_spans"
            assert gen.metadata.fields["agento11y.conversation.title"].string_value == "Sensitive conversation"

            # Span path drops the title.
            assert "agento11y.conversation.title" not in env.generation_span().attributes

    def test_provider_call_error_redacted_on_span_raw_in_proto(self):
        """Provider call errors must not echo raw message on the span under
        FULL_WITH_METADATA_SPANS, but the proto export must keep the raw text.
        Mirrors Go's ``provider_call_error_redacted_on_span,_raw_in_proto``.
        """
        raw_err = "provider returned HTTP 400: blocked content '" + _LEAK_MARKER + "'"
        with _ContentCaptureEnv(content_capture=ContentCaptureMode.FULL_WITH_METADATA_SPANS) as env:
            rec = env.client.start_generation(
                GenerationStart(
                    model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
                    agent_name="agent-fwms-error",
                )
            )
            rec.set_call_error(RuntimeError(raw_err))
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="x")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="y")])],
                    usage=TokenUsage(input_tokens=1, output_tokens=1),
                )
            )
            rec.end()
            assert rec.err() is None

            # Proto export (post-gRPC roundtrip) preserves the raw provider error.
            gen = env.single_generation()
            assert gen.call_error == raw_err
            assert gen.metadata.fields["call_error"].string_value == raw_err

            _assert_span_error_redacted(env.generation_span(), "provider_call_error")

    # Tool span content omission and embedding span content omission both apply
    # to MetadataOnly and FullWithMetadataSpans. Embeddings have no proto
    # export, and the tool path doesn't have one either, so both modes are
    # equivalent on the span path.
    @pytest.mark.parametrize("mode", _STRIPPED_MODES)
    def test_stripped_modes_tool_span_omits_content_attrs(self, mode):
        # The full set of content-bearing attributes the tool span path can
        # carry. Under either stripped mode none of them should appear.
        with _ContentCaptureEnv(content_capture=mode) as env:
            with env.client.start_tool_execution(
                ToolExecutionStart(
                    tool_name="weather",
                    tool_call_id="call_1",
                    include_content=True,
                    conversation_title="Sensitive tool title",
                    tool_description="Get weather: free-form provider-supplied text",
                )
            ) as rec:
                rec.set_result(arguments={"city": "Paris"}, result={"temp_c": 18})

            tool_span = env.tool_span()
            assert "gen_ai.tool.call.arguments" not in tool_span.attributes
            assert "gen_ai.tool.call.result" not in tool_span.attributes
            assert "agento11y.conversation.title" not in tool_span.attributes
            assert "gen_ai.tool.description" not in tool_span.attributes
            # Identity attributes still emitted.
            assert tool_span.attributes.get("gen_ai.tool.name") == "weather"

    @pytest.mark.parametrize("mode", _STRIPPED_MODES)
    def test_stripped_modes_tool_span_redacts_call_error(self, mode):
        # Tool execution has no proto export — the raw provider error must
        # not echo on the span path under either stripped mode.
        raw_err = "provider returned HTTP 400: blocked content '" + _LEAK_MARKER + "'"
        with _ContentCaptureEnv(content_capture=mode) as env:
            with env.client.start_tool_execution(
                ToolExecutionStart(
                    tool_name="weather",
                    tool_call_id="call_1",
                    include_content=True,
                )
            ) as rec:
                rec.set_exec_error(RuntimeError(raw_err))
                rec.set_result(arguments={"city": "Paris"}, result={"temp_c": 18})

            _assert_span_error_redacted(env.tool_span(), "tool_execution_error")

    @pytest.mark.parametrize("mode", _STRIPPED_MODES)
    def test_stripped_modes_embedding_span_omits_input_texts(self, mode):
        with _ContentCaptureEnv(
            content_capture=mode,
            embedding_capture=EmbeddingCaptureConfig(capture_input=True),
        ) as env:
            rec = env.client.start_embedding(
                EmbeddingStart(model=ModelRef(provider="openai", name="text-embedding-3-small")),
            )
            rec.set_result(
                EmbeddingResult(
                    input_count=1,
                    input_tokens=10,
                    input_texts=["sensitive input text"],
                    response_model="text-embedding-3-small",
                )
            )
            rec.end()
            assert rec.err() is None

            emb_span = env.embedding_span()
            assert "gen_ai.embeddings.input_texts" not in emb_span.attributes
            # Non-content embedding span fields still populated.
            assert emb_span.attributes.get("gen_ai.embeddings.input_count") == 1
            assert emb_span.attributes.get("gen_ai.usage.input_tokens") == 10
            assert emb_span.attributes.get("gen_ai.response.model") == "text-embedding-3-small"

    @pytest.mark.parametrize("mode", _STRIPPED_MODES)
    def test_stripped_modes_embedding_provider_call_error_redacted_on_span(self, mode):
        """Embedding provider errors carry no raw text on the span. Embeddings have
        no proto export, so the raw error never escapes the span path.
        """
        raw_err = "provider returned HTTP 400: blocked content '" + _LEAK_MARKER + "'"
        with _ContentCaptureEnv(
            content_capture=mode,
            embedding_capture=EmbeddingCaptureConfig(capture_input=True),
        ) as env:
            rec = env.client.start_embedding(
                EmbeddingStart(model=ModelRef(provider="openai", name="text-embedding-3-small")),
            )
            rec.set_call_error(RuntimeError(raw_err))
            rec.set_result(
                EmbeddingResult(input_count=1, input_texts=["sensitive input text"]),
            )
            rec.end()
            assert rec.err() is None

            _assert_span_error_redacted(env.embedding_span(), "provider_call_error")

    def test_resolver_full_with_metadata_spans_hides_embedding_input_texts(self):
        """Resolver returning FULL_WITH_METADATA_SPANS hides input_texts when client default is FULL."""
        with _ContentCaptureEnv(
            content_capture=ContentCaptureMode.FULL,
            content_capture_resolver=lambda _meta: ContentCaptureMode.FULL_WITH_METADATA_SPANS,
            embedding_capture=EmbeddingCaptureConfig(capture_input=True),
        ) as env:
            rec = env.client.start_embedding(
                EmbeddingStart(model=ModelRef(provider="openai", name="text-embedding-3-small")),
            )
            rec.set_result(
                EmbeddingResult(input_count=1, input_texts=["resolver-gated sensitive text"]),
            )
            rec.end()
            assert rec.err() is None

            assert "gen_ai.embeddings.input_texts" not in env.embedding_span().attributes

    def test_context_full_does_not_weaken_embedding_full_with_metadata_spans(self):
        """with_content_capture_mode(FULL) must not unhide gen_ai.embeddings.input_texts when client
        is FULL_WITH_METADATA_SPANS. Embeddings gate on config + resolver only, matching Go/Java/JS/.NET."""
        with _ContentCaptureEnv(
            content_capture=ContentCaptureMode.FULL_WITH_METADATA_SPANS,
            embedding_capture=EmbeddingCaptureConfig(capture_input=True),
        ) as env:
            with with_content_capture_mode(ContentCaptureMode.FULL):
                rec = env.client.start_embedding(
                    EmbeddingStart(model=ModelRef(provider="openai", name="text-embedding-3-small")),
                )
                rec.set_result(
                    EmbeddingResult(input_count=1, input_texts=["context-should-not-leak"]),
                )
                rec.end()
                assert rec.err() is None

            assert "gen_ai.embeddings.input_texts" not in env.embedding_span().attributes

    def test_context_full_overrides_generation_full_with_metadata_spans(self):
        """with_content_capture_mode(FULL) is an explicit caller override and DOES weaken
        a client default of FULL_WITH_METADATA_SPANS for generations. Locks in the
        Python-specific override contract documented on with_content_capture_mode.

        Embeddings deliberately do NOT honor this override (see
        test_context_full_does_not_weaken_embedding_full_with_metadata_spans).
        """
        with _ContentCaptureEnv(content_capture=ContentCaptureMode.FULL_WITH_METADATA_SPANS) as env:
            with with_content_capture_mode(ContentCaptureMode.FULL):
                rec = env.client.start_generation(
                    GenerationStart(
                        model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
                        conversation_title="Sensitive conversation",
                    )
                )
                rec.set_result(
                    Generation(
                        input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hi")])],
                        output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="yo")])],
                        usage=TokenUsage(input_tokens=1, output_tokens=1),
                    )
                )
                rec.end()
                assert rec.err() is None

            gen = env.single_generation()
            # Proto export keeps content (also true under FULL_WITH_METADATA_SPANS).
            assert gen.metadata.fields["agento11y.conversation.title"].string_value == "Sensitive conversation"
            # Resolved mode is FULL, not FULL_WITH_METADATA_SPANS.
            assert gen.metadata.fields[_METADATA_KEY].string_value == "full"

            # Span path: the ctx FULL override re-enables the title attribute that
            # FULL_WITH_METADATA_SPANS would have suppressed.
            assert env.generation_span().attributes.get("agento11y.conversation.title") == "Sensitive conversation"

    def test_context_full_overrides_tool_full_with_metadata_spans(self):
        """with_content_capture_mode(FULL) overrides FULL_WITH_METADATA_SPANS for tool spans too.

        Tool executions have no separate proto export path, so a ctx FULL override
        re-introduces tool arguments, result, and title on the tool span. This is the
        Python-specific caller-override contract, intentionally consistent with
        generation behavior and explicitly different from embeddings.
        """
        with _ContentCaptureEnv(content_capture=ContentCaptureMode.FULL_WITH_METADATA_SPANS) as env:
            with with_content_capture_mode(ContentCaptureMode.FULL):
                with env.client.start_tool_execution(
                    ToolExecutionStart(
                        tool_name="weather",
                        tool_call_id="call_1",
                        include_content=True,
                        conversation_title="Sensitive tool title",
                        tool_description="Get weather",
                    )
                ) as rec:
                    rec.set_result(arguments={"city": "Paris"}, result={"temp_c": 18})

            tool_span = env.tool_span()
            assert tool_span.attributes.get("agento11y.conversation.title") == "Sensitive tool title"
            assert "gen_ai.tool.call.arguments" in tool_span.attributes
            assert "gen_ai.tool.call.result" in tool_span.attributes

    def test_rating_comment_preserved(self):
        captured: dict = {}
        handler = TestRatingCommentStripping._make_rating_handler(self, captured)
        server = HTTPServer(("127.0.0.1", 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()

        client = Client(
            ClientConfig(
                content_capture=ContentCaptureMode.FULL_WITH_METADATA_SPANS,
                generation_export=GenerationExportConfig(
                    protocol="none",
                    batch_size=1,
                    flush_interval=timedelta(seconds=60),
                ),
                api=ApiConfig(endpoint=f"http://127.0.0.1:{server.server_address[1]}"),
            )
        )

        try:
            client.submit_conversation_rating(
                "conv-1",
                ConversationRatingInput(
                    rating_id="rat-1",
                    rating=ConversationRatingValue.BAD,
                    comment="user-supplied free text",
                ),
            )
            assert captured["payload"]["comment"] == "user-supplied free text"
        finally:
            client.shutdown()
            server.shutdown()
            server.server_close()


# ---------------------------------------------------------------------------
# Mode × surface coverage matrix
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class _ModeExpect:
    """What each content capture mode should do to a full-content generation.

    Encodes the contract that every SDK is expected to honor: which fields stay
    in the proto, which get stripped, and what the OTel span sees.
    """

    mode: ContentCaptureMode
    marker: str  # metadata["agento11y.sdk.content_capture_mode"] value
    proto_content_stripped: bool  # system_prompt, message text/thinking, tool args/results, tools.description/schema
    span_title_present: bool  # whether agento11y.conversation.title appears on the generation span
    proto_call_error_raw: bool  # whether proto.call_error is the raw provider message vs the error category
    span_raw_error: bool  # whether the span echoes the raw provider message via exception events / status


# DEFAULT is intentionally absent here: it's the resolver fall-through and is
# covered by TestContentCaptureModeResolution. The four entries below are the
# actual on-the-wire modes.
_MODE_MATRIX = [
    _ModeExpect(
        mode=ContentCaptureMode.FULL,
        marker="full",
        proto_content_stripped=False,
        span_title_present=True,
        proto_call_error_raw=True,
        span_raw_error=True,
    ),
    _ModeExpect(
        mode=ContentCaptureMode.NO_TOOL_CONTENT,
        marker="no_tool_content",
        # NO_TOOL_CONTENT is generation-content-full; only tool spans gate
        # arguments/results via legacy include_content.
        proto_content_stripped=False,
        span_title_present=True,
        proto_call_error_raw=True,
        span_raw_error=True,
    ),
    _ModeExpect(
        mode=ContentCaptureMode.METADATA_ONLY,
        marker="metadata_only",
        proto_content_stripped=True,
        span_title_present=False,
        proto_call_error_raw=False,  # replaced with error category
        span_raw_error=False,
    ),
    _ModeExpect(
        mode=ContentCaptureMode.FULL_WITH_METADATA_SPANS,
        marker="full_with_metadata_spans",
        proto_content_stripped=False,  # proto path keeps full content
        span_title_present=False,  # but the span drops the title
        proto_call_error_raw=True,
        span_raw_error=False,
    ),
]

_MODE_IDS = [expect.mode.value for expect in _MODE_MATRIX]


class TestModeCoverageMatrix:
    """Single full-content fixture run through every mode, asserted via the matrix above.

    Catches gaps in any mode without writing four separate tests for each surface.
    """

    @pytest.mark.parametrize("expect", _MODE_MATRIX, ids=_MODE_IDS)
    def test_generation_proto_and_span(self, expect: _ModeExpect):
        # _full_generation() seeds conversation_title="Weather chat"; the
        # recorder uses the result's title as the source of truth, so we
        # assert against that.
        title = "Weather chat"
        with _ContentCaptureEnv(content_capture=expect.mode) as env:
            rec = env.client.start_generation(
                GenerationStart(
                    model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
                    conversation_title=title,
                    system_prompt="You are helpful.",
                )
            )
            rec.set_result(_full_generation())
            rec.end()
            assert rec.err() is None

            gen = env.single_generation()
            assert gen.metadata.fields[_METADATA_KEY].string_value == expect.marker

            # Content fields: stripped under METADATA_ONLY, preserved otherwise.
            assert gen.system_prompt == ("" if expect.proto_content_stripped else "You are helpful.")
            assert gen.input[0].parts[0].text == ("" if expect.proto_content_stripped else "What is the weather?")
            assert gen.output[0].parts[0].thinking == (
                "" if expect.proto_content_stripped else "let me think about weather"
            )
            assert gen.output[0].parts[1].tool_call.input_json == (
                b"" if expect.proto_content_stripped else b'{"city":"Paris"}'
            )
            assert gen.output[0].parts[2].text == (
                "" if expect.proto_content_stripped else "It's 18C and sunny in Paris."
            )
            assert gen.input[1].parts[0].tool_result.content == ("" if expect.proto_content_stripped else "sunny 18C")
            assert gen.input[1].parts[0].tool_result.content_json == (
                b"" if expect.proto_content_stripped else b'{"temp":18}'
            )
            assert gen.tools[0].description == ("" if expect.proto_content_stripped else "Get weather info")
            assert gen.tools[0].input_schema_json == (b"" if expect.proto_content_stripped else b'{"type":"object"}')
            # Conversation title lives only in metadata (no top-level proto
            # field); see the mirror assertion below.

            # Structural fields (counts, names, IDs, roles) always preserved.
            assert len(gen.input) == 2
            assert len(gen.output) == 1
            assert len(gen.output[0].parts) == 3
            assert gen.output[0].parts[1].tool_call.name == "weather"
            assert gen.output[0].parts[1].tool_call.id == "call_1"
            assert gen.tools[0].name == "weather"
            assert gen.usage.input_tokens == 120
            assert gen.usage.output_tokens == 42
            assert gen.stop_reason == "end_turn"

            # Conversation title metadata mirror: present iff the proto keeps the
            # title (METADATA_ONLY removes it; every other mode mirrors it).
            title_mirror = gen.metadata.fields.get("agento11y.conversation.title")
            if expect.proto_content_stripped:
                assert title_mirror is None or title_mirror.string_value == ""
            else:
                assert title_mirror.string_value == title

            # Span path: title attribute presence is what the mode advertises.
            gen_span = env.generation_span()
            if expect.span_title_present:
                assert gen_span.attributes.get("agento11y.conversation.title") == title
            else:
                assert "agento11y.conversation.title" not in gen_span.attributes

    @pytest.mark.parametrize("expect", _MODE_MATRIX, ids=_MODE_IDS)
    def test_generation_call_error(self, expect: _ModeExpect):
        raw_err = "provider returned HTTP 400: blocked content '" + _LEAK_MARKER + "'"
        with _ContentCaptureEnv(content_capture=expect.mode) as env:
            rec = env.client.start_generation(
                GenerationStart(
                    model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
                    agent_name="agent-matrix-error",
                )
            )
            rec.set_call_error(RuntimeError(raw_err))
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="x")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="y")])],
                    usage=TokenUsage(input_tokens=1, output_tokens=1),
                )
            )
            rec.end()
            assert rec.err() is None

            gen = env.single_generation()
            if expect.proto_call_error_raw:
                assert gen.call_error == raw_err
                assert gen.metadata.fields["call_error"].string_value == raw_err
            else:
                # METADATA_ONLY replaces with the error category and removes
                # the metadata mirror.
                assert gen.call_error != raw_err
                assert gen.call_error  # non-empty category
                assert "call_error" not in gen.metadata.fields

            gen_span = env.generation_span()
            if expect.span_raw_error:
                assert _LEAK_MARKER in (gen_span.status.description or "")
            else:
                _assert_span_error_redacted(gen_span, "provider_call_error")

    def test_streaming_full_with_metadata_spans(self):
        """Streaming generations honor the FWMS proto/span split.

        Streaming changes the span operation name from generateText to
        streamText but the redaction logic is shared with non-streaming. This
        test catches regressions where the two paths drift apart.
        """
        with _ContentCaptureEnv(content_capture=ContentCaptureMode.FULL_WITH_METADATA_SPANS) as env:
            rec = env.client.start_streaming_generation(
                GenerationStart(
                    model=ModelRef(provider="anthropic", name="claude-sonnet-4-5"),
                    conversation_title="Sensitive streaming conversation",
                    system_prompt="Be helpful.",
                )
            )
            rec.set_result(
                Generation(
                    system_prompt="Be helpful.",
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="hi")])],
                    usage=TokenUsage(input_tokens=1, output_tokens=1),
                )
            )
            rec.end()
            assert rec.err() is None

            # Proto export keeps streaming content full.
            gen = env.single_generation()
            assert gen.system_prompt == "Be helpful."
            assert gen.input[0].parts[0].text == "hello"
            assert gen.output[0].parts[0].text == "hi"
            title_field = gen.metadata.fields["agento11y.conversation.title"]
            assert title_field.string_value == "Sensitive streaming conversation"
            assert gen.metadata.fields[_METADATA_KEY].string_value == "full_with_metadata_spans"

            # Span uses the streamText operation name and still drops the title.
            stream_span = next(
                s
                for s in env.span_exporter.get_finished_spans()
                if s.attributes.get("gen_ai.operation.name") == "streamText"
            )
            assert "agento11y.conversation.title" not in stream_span.attributes
