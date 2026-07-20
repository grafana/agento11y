"""Tests that client-level tags are emitted as ``agento11y.tag.<key>`` span and metric attributes."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

from agento11y import (
    Client,
    ClientConfig,
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
    ToolExecutionStart,
)
from conftest import CapturingGenerationExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

_OPENAI = ModelRef(provider="openai", name="gpt-5")
_EMBEDDING_MODEL = ModelRef(provider="openai", name="text-embedding-3-small")
_CLIENT_TAG_PROJECT_KEY = "agento11y.tag.project"


class _TagsHarness:
    """In-memory OTel span + metric plumbing for client tag tests."""

    def __init__(self, tags: dict[str, str] | None = None) -> None:
        self.span_exporter = InMemorySpanExporter()
        self.tracer_provider = TracerProvider()
        self.tracer_provider.add_span_processor(SimpleSpanProcessor(self.span_exporter))

        self.metric_reader = InMemoryMetricReader()
        self.meter_provider = MeterProvider(metric_readers=[self.metric_reader])

        self.generation_exporter = CapturingGenerationExporter()
        self.client = Client(
            ClientConfig(
                tracer=self.tracer_provider.get_tracer("agento11y-test"),
                meter=self.meter_provider.get_meter("agento11y-test"),
                generation_export=GenerationExportConfig(
                    batch_size=10,
                    flush_interval=timedelta(seconds=60),
                    queue_size=10,
                    max_retries=1,
                    initial_backoff=timedelta(milliseconds=1),
                    max_backoff=timedelta(milliseconds=1),
                ),
                generation_exporter=self.generation_exporter,
                tags=tags,
            )
        )

    def shutdown(self) -> None:
        self.client.shutdown()
        self.tracer_provider.shutdown()
        self.meter_provider.shutdown()

    def spans_for_operation(self, operation_name: str | None):
        spans = self.span_exporter.get_finished_spans()
        if operation_name is None:
            return [s for s in spans if s.attributes.get("gen_ai.operation.name") not in ("execute_tool", "embeddings")]
        return [s for s in spans if s.attributes.get("gen_ai.operation.name") == operation_name]

    def single_span(self, operation_name: str | None):
        spans = self.spans_for_operation(operation_name)
        assert len(spans) == 1, f"expected 1 span for {operation_name!r}, got {len(spans)}"
        return spans[0]

    def metric_data_points(self, metric_name: str):
        data = self.metric_reader.get_metrics_data()
        points = []
        for resource_metric in data.resource_metrics:
            for scope_metric in resource_metric.scope_metrics:
                for metric in scope_metric.metrics:
                    if metric.name == metric_name:
                        points.extend(metric.data.data_points)
        assert points, f"expected data points for {metric_name}"
        return points

    def assert_metric_points_carry_tag(self, metric_name: str, key: str, value: str) -> None:
        points = self.metric_data_points(metric_name)
        for point in points:
            assert dict(point.attributes).get(key) == value, (
                f"expected {key}={value!r} on every {metric_name} data point, got {dict(point.attributes)}"
            )


def test_client_tags_on_generation_span_and_metrics():
    h = _TagsHarness(tags={"project": "checkout-svc"})
    try:
        rec = h.client.start_streaming_generation(GenerationStart(model=_OPENAI))
        rec.set_first_token_at(datetime.now(timezone.utc))
        rec.set_result(
            Generation(
                output=[
                    Message(
                        role=MessageRole.ASSISTANT,
                        parts=[
                            Part(
                                kind=PartKind.TOOL_CALL,
                                tool_call=ToolCall(id="call-1", name="weather"),
                            )
                        ],
                    )
                ],
                usage=TokenUsage(input_tokens=10, output_tokens=5),
            )
        )
        rec.end()
        assert rec.err() is None

        span = h.single_span(None)
        assert span.attributes.get(_CLIENT_TAG_PROJECT_KEY) == "checkout-svc"

        for metric_name in (
            "gen_ai.client.operation.duration",
            "gen_ai.client.token.usage",
            "gen_ai.client.tool_calls_per_operation",
            "gen_ai.client.time_to_first_token",
        ):
            h.assert_metric_points_carry_tag(metric_name, _CLIENT_TAG_PROJECT_KEY, "checkout-svc")
    finally:
        h.shutdown()


def test_client_tags_on_embedding_and_tool_spans_and_metrics():
    h = _TagsHarness(tags={"project": "embed-tools"})
    try:
        embed = h.client.start_embedding(EmbeddingStart(model=_EMBEDDING_MODEL))
        embed.set_result(EmbeddingResult(input_tokens=1))
        embed.end()
        assert embed.err() is None

        embed_span = h.single_span("embeddings")
        assert embed_span.attributes.get(_CLIENT_TAG_PROJECT_KEY) == "embed-tools"

        tool = h.client.start_tool_execution(ToolExecutionStart(tool_name="weather"))
        tool.set_result(result={"temp": 72})
        tool.end()
        assert tool.err() is None

        tool_span = h.single_span("execute_tool")
        assert tool_span.attributes.get(_CLIENT_TAG_PROJECT_KEY) == "embed-tools"

        # Embedding duration + tool duration share the operation.duration
        # instrument; embedding token usage is the token.usage instrument.
        h.assert_metric_points_carry_tag("gen_ai.client.operation.duration", _CLIENT_TAG_PROJECT_KEY, "embed-tools")
        h.assert_metric_points_carry_tag("gen_ai.client.token.usage", _CLIENT_TAG_PROJECT_KEY, "embed-tools")
    finally:
        h.shutdown()


def test_client_tags_are_normalized_and_sorted():
    h = _TagsHarness(tags={" z ": " last ", "   ": "discard", " a ": ""})
    try:
        rec = h.client.start_generation(GenerationStart(model=_OPENAI))
        rec.set_result(Generation())
        rec.end()
        assert rec.err() is None

        span = h.single_span(None)
        tag_attrs = [(k, v) for k, v in span.attributes.items() if k.startswith("agento11y.tag.")]
        assert tag_attrs == [("agento11y.tag.a", ""), ("agento11y.tag.z", "last")]
    finally:
        h.shutdown()


def test_empty_client_tags_are_noop():
    h = _TagsHarness()
    try:
        rec = h.client.start_generation(GenerationStart(model=_OPENAI))
        rec.set_result(Generation(usage=TokenUsage(input_tokens=1)))
        rec.end()
        assert rec.err() is None

        embed = h.client.start_embedding(EmbeddingStart(model=_EMBEDDING_MODEL))
        embed.set_result(EmbeddingResult(input_tokens=1))
        embed.end()

        tool = h.client.start_tool_execution(ToolExecutionStart(tool_name="weather"))
        tool.set_result(result="sunny")
        tool.end()

        for span in h.span_exporter.get_finished_spans():
            for key in span.attributes:
                assert not key.startswith("agento11y.tag."), f"unexpected {key} on span {span.name}"

        for metric_name in ("gen_ai.client.operation.duration", "gen_ai.client.token.usage"):
            for point in h.metric_data_points(metric_name):
                for key in dict(point.attributes):
                    assert not key.startswith("agento11y.tag."), f"unexpected {key} on {metric_name}"
    finally:
        h.shutdown()


def test_per_call_generation_tags_stay_export_only():
    h = _TagsHarness()
    try:
        rec = h.client.start_generation(GenerationStart(model=_OPENAI, tags={"call_only": "yes"}))
        rec.set_result(Generation(usage=TokenUsage(input_tokens=1)))
        rec.end()
        assert rec.err() is None

        span = h.single_span(None)
        assert "agento11y.tag.call_only" not in span.attributes

        for point in h.metric_data_points("gen_ai.client.operation.duration"):
            assert "agento11y.tag.call_only" not in dict(point.attributes)

        h.client.flush()
        assert len(h.generation_exporter.requests) == 1
        generation = h.generation_exporter.requests[0].generations[0]
        assert generation.tags["call_only"] == "yes"
    finally:
        h.shutdown()
