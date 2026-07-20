"""Tests that metric recordings carry span context for exemplars."""

from __future__ import annotations

from datetime import timedelta
from unittest.mock import patch

from agento11y import (
    Client,
    ClientConfig,
    EmbeddingResult,
    EmbeddingStart,
    GenerationExportConfig,
    GenerationStart,
    ModelRef,
    TokenUsage,
    ToolExecutionStart,
)
from conftest import CapturingGenerationExporter
from opentelemetry import trace
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

_OPENAI = ModelRef(provider="openai", name="gpt-5")
_EMBEDDING_MODEL = ModelRef(provider="openai", name="text-embedding-3-small")


class _ExemplarHarness:
    """Shared OTel + Sigil plumbing for exemplar tests."""

    def __init__(self) -> None:
        self.span_exporter = InMemorySpanExporter()
        self.tracer_provider = TracerProvider()
        self.tracer_provider.add_span_processor(SimpleSpanProcessor(self.span_exporter))

        self.metric_reader = InMemoryMetricReader()
        self.meter_provider = MeterProvider(metric_readers=[self.metric_reader])

        gen_export = GenerationExportConfig(
            batch_size=10,
            flush_interval=timedelta(seconds=60),
            queue_size=10,
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=1),
        )
        exporter = CapturingGenerationExporter()
        self.client = Client(
            ClientConfig(
                tracer=self.tracer_provider.get_tracer("sigil-test"),
                meter=self.meter_provider.get_meter("sigil-test"),
                generation_export=gen_export,
                generation_exporter=exporter,
            )
        )

        self.captured_trace_ids: list[str] = []
        self._original_record = self.client._operation_duration_histogram.record

    def capturing_record(self, value, attributes=None, context=None):
        span = trace.get_current_span()
        sc = span.get_span_context()
        if sc and sc.trace_id != 0:
            self.captured_trace_ids.append(format(sc.trace_id, "032x"))
        return self._original_record(value, attributes=attributes, context=context)

    def shutdown(self) -> None:
        self.client.shutdown()
        self.tracer_provider.shutdown()
        self.meter_provider.shutdown()

    def assert_metric_carries_trace_id(self, operation_filter: str | None, *, label: str) -> None:
        spans = self.span_exporter.get_finished_spans()
        if operation_filter is None:
            filtered = [
                s for s in spans if s.attributes.get("gen_ai.operation.name") not in ("execute_tool", "embeddings")
            ]
        else:
            filtered = [s for s in spans if s.attributes.get("gen_ai.operation.name") == operation_filter]

        assert len(filtered) == 1, f"expected 1 {label} span, got {len(filtered)}"
        want_trace_id = format(filtered[0].context.trace_id, "032x")

        assert len(self.captured_trace_ids) > 0, "histogram.record should have been called"
        assert self.captured_trace_ids[0] == want_trace_id, (
            f"metric should carry {label} span trace_id: got {self.captured_trace_ids[0]}, want {want_trace_id}"
        )


def test_generation_metrics_carry_span_context():
    h = _ExemplarHarness()
    try:
        with patch.object(h.client._operation_duration_histogram, "record", side_effect=h.capturing_record):
            rec = h.client.start_generation(GenerationStart(model=_OPENAI))
            rec.set_result(output=[], usage=TokenUsage(input_tokens=10, output_tokens=5))
            rec.end()
            assert rec.err() is None
        h.assert_metric_carries_trace_id(None, label="generation")
    finally:
        h.shutdown()


def test_embedding_metrics_carry_span_context():
    h = _ExemplarHarness()
    try:
        with patch.object(h.client._operation_duration_histogram, "record", side_effect=h.capturing_record):
            rec = h.client.start_embedding(EmbeddingStart(model=_EMBEDDING_MODEL))
            rec.set_result(EmbeddingResult(input_tokens=42))
            rec.end()
            assert rec.err() is None
        h.assert_metric_carries_trace_id("embeddings", label="embedding")
    finally:
        h.shutdown()


def test_tool_execution_metrics_carry_span_context():
    h = _ExemplarHarness()
    try:
        with patch.object(h.client._operation_duration_histogram, "record", side_effect=h.capturing_record):
            rec = h.client.start_tool_execution(ToolExecutionStart(tool_name="weather"))
            rec.set_result(result="sunny")
            rec.end()
            assert rec.err() is None
        h.assert_metric_carries_trace_id("execute_tool", label="tool")
    finally:
        h.shutdown()
