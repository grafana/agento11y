"""Tests that metric recordings carry span context for exemplars."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from unittest.mock import patch

import pytest
from agento11y import (
    Client,
    ClientConfig,
    ContentCaptureMode,
    EmbeddingResult,
    EmbeddingStart,
    GenerationExportConfig,
    GenerationStart,
    ModelRef,
    TokenUsage,
    ToolExecutionKind,
    ToolExecutionStart,
)
from conftest import CapturingGenerationExporter
from opentelemetry import trace
from opentelemetry.sdk.metrics import AlwaysOnExemplarFilter, MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.sdk.trace.sampling import ALWAYS_OFF, ALWAYS_ON

_OPENAI = ModelRef(provider="openai", name="gpt-5")
_EMBEDDING_MODEL = ModelRef(provider="openai", name="text-embedding-3-small")
_START = datetime(2026, 1, 1, tzinfo=timezone.utc)
_DURATION = "gen_ai.client.operation.duration"
_TOOL_BASELINE = {
    "gen_ai.operation.name": "execute_tool",
    "gen_ai.tool.name": "weather",
    "gen_ai.provider.name": "",
    "gen_ai.request.model": "",
    "gen_ai.agent.name": "",
    "error.type": "",
    "error.category": "",
}


class _ExemplarHarness:
    """Shared OTel + agento11y plumbing for exemplar tests."""

    def __init__(self, *, tracing="sampled", **overrides) -> None:
        self.span_exporter = InMemorySpanExporter()
        self.tracer_provider = TracerProvider(sampler=ALWAYS_OFF if tracing == "unsampled" else ALWAYS_ON)
        self.tracer_provider.add_span_processor(SimpleSpanProcessor(self.span_exporter))

        self.metric_reader = InMemoryMetricReader()
        self.meter_provider = MeterProvider(
            metric_readers=[self.metric_reader], exemplar_filter=AlwaysOnExemplarFilter()
        )

        gen_export = GenerationExportConfig(
            batch_size=10,
            flush_interval=timedelta(seconds=60),
            queue_size=10,
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=1),
        )
        exporter = CapturingGenerationExporter()
        config = {
            "tracer": (
                trace.NoOpTracerProvider().get_tracer("agento11y-test")
                if tracing == "noop"
                else self.tracer_provider.get_tracer("agento11y-test")
            ),
            "meter": self.meter_provider.get_meter("agento11y-test"),
            "generation_export": gen_export,
            "generation_exporter": exporter,
        }
        self.client = Client(ClientConfig(**{**config, **overrides}))

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


@pytest.mark.parametrize("tracing", ["sampled", "unsampled", "noop"])
@pytest.mark.parametrize("mode", list(ContentCaptureMode))
@pytest.mark.parametrize(
    "kind, skill, additions",
    [
        (kind, skill, {"agento11y.tool.execution.kind": kind.value, **name})
        for kind in ToolExecutionKind
        for skill, name in [(" code-review ", {"gen_ai.skill.name": "code-review"}), (" \t ", {}), ("", {})]
    ]
    + [(None, "code-review", {}), ("unknown", "code-review", {})],
)
def test_tool_execution_metrics_carry_span_context(tracing, mode, kind, skill, additions):
    h = _ExemplarHarness(tracing=tracing, content_capture=mode)
    try:
        start = ToolExecutionStart(
            tool_name=" weather ",
            execution_kind=kind,
            skill_name=skill,
            skill_description="private description",
            skill_source_uri="file:///private/SKILL.md",
            skill_resource_name="private-resource",
            process_executable_name="private-executable",
            process_executable_path="/private/executable",
            tool_call_id="private-call",
            conversation_id="private-conversation",
            started_at=_START,
        )
        with patch.object(h.client._operation_duration_histogram, "record", side_effect=h.capturing_record):
            rec = h.client.start_tool_execution(start)
            start.execution_kind = ToolExecutionKind.COMMAND if kind != ToolExecutionKind.COMMAND else None
            start.skill_name = "mutated-skill"
            rec.set_result(result="private payload", completed_at=_START + timedelta(seconds=2), process_exit_code=7)
            with h.tracer_provider.get_tracer("application").start_as_current_span("unrelated"):
                rec.end()
                rec.end()
            assert rec.err() is None
        data = h.metric_reader.get_metrics_data()
        metrics = [
            metric for resource in data.resource_metrics for scope in resource.scope_metrics for metric in scope.metrics
        ]
        assert [metric.name for metric in metrics] == [_DURATION]
        assert metrics[0].unit == "s"
        [point] = metrics[0].data.data_points
        assert dict(point.attributes) == {**_TOOL_BASELINE, **additions}
        assert point.count == sum(point.bucket_counts) == 1
        assert point.sum == point.min == point.max == 2
        assert point.explicit_bounds == (
            0.01,
            0.02,
            0.04,
            0.08,
            0.16,
            0.32,
            0.64,
            1.28,
            2.56,
            5.12,
            10.24,
            20.48,
            40.96,
            81.92,
        )
        assert point.bucket_counts == (0,) * 8 + (1,) + (0,) * 6
        if tracing == "sampled":
            h.assert_metric_carries_trace_id("execute_tool", label="tool")
            [span] = [
                span
                for span in h.span_exporter.get_finished_spans()
                if span.attributes.get("gen_ai.operation.name") == "execute_tool"
            ]
            [exemplar] = point.exemplars
            assert (exemplar.trace_id, exemplar.span_id) == (span.context.trace_id, span.context.span_id)
            assert exemplar.value == point.sum
        else:
            assert len(h.captured_trace_ids) == (0 if tracing == "noop" else 1)
            assert not any(
                span.attributes.get("gen_ai.operation.name") == "execute_tool"
                for span in h.span_exporter.get_finished_spans()
            )
    finally:
        h.shutdown()


@pytest.mark.parametrize("kind", [None, ToolExecutionKind.LOAD_SKILL, ToolExecutionKind.COMMAND])
@pytest.mark.parametrize("mode", list(ContentCaptureMode))
@pytest.mark.parametrize("outcome", ["success", "explicit", "arguments", "result", "both", "nonzero-exit"])
def test_tool_metric_error_behavior(kind, mode, outcome):
    h = _ExemplarHarness(content_capture=mode)
    try:
        rec = h.client.start_tool_execution(
            ToolExecutionStart(
                tool_name="weather",
                execution_kind=kind,
                skill_name=" code-review ",
                include_content=True,
                started_at=_START,
            )
        )
        error = RuntimeError("execution failed") if outcome in ("explicit", "both") else None
        if error is not None:
            rec.set_exec_error(error)
        rec.set_result(
            arguments=object() if outcome == "arguments" else {},
            result=object() if outcome in ("result", "both") else "sunny",
            process_exit_code=7 if outcome == "nonzero-exit" else None,
            completed_at=_START + timedelta(seconds=2),
        )
        rec.end()
        rec.end()
        captures = mode not in (ContentCaptureMode.METADATA_ONLY, ContentCaptureMode.FULL_WITH_METADATA_SPANS)
        serialization_error = captures and outcome in ("arguments", "result", "both")
        failed = error is not None or serialization_error
        if serialization_error:
            assert isinstance(rec.err(), RuntimeError)
            assert f"serialize tool {'arguments' if outcome == 'arguments' else 'result'}" in str(rec.err())
            if error is not None:
                assert str(rec.err()).startswith("execution failed; serialize tool result:")
        else:
            assert rec.err() is error
        [span] = h.span_exporter.get_finished_spans()
        assert span.status.status_code.name == ("ERROR" if failed else "OK")
        if failed:
            assert span.attributes["error.type"] == "tool_execution_error"
            assert span.attributes["error.category"] == "sdk_error"
        else:
            assert "error.type" not in span.attributes
            assert "error.category" not in span.attributes
        data = h.metric_reader.get_metrics_data()
        [metric] = [
            metric for resource in data.resource_metrics for scope in resource.scope_metrics for metric in scope.metrics
        ]
        assert metric.name == _DURATION
        [point] = metric.data.data_points
        assert point.count == 1
        assert point.sum == 2
        additions = (
            {} if kind is None else {"agento11y.tool.execution.kind": kind.value, "gen_ai.skill.name": "code-review"}
        )
        assert dict(point.attributes) == {
            **_TOOL_BASELINE,
            **additions,
            "error.type": "tool_execution_error" if failed else "",
            "error.category": "sdk_error" if failed else "",
        }
    finally:
        h.shutdown()
