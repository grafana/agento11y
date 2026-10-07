from __future__ import annotations

import json
from unittest.mock import Mock

import pytest

pytest.importorskip("opentelemetry.util.genai.handler")

from agento11y import (
    Client,
    ClientConfig,
    ClientShutdownError,
    ContentCaptureMode,
    EmbeddingCaptureConfig,
    EmbeddingResult,
    EmbeddingStart,
    EnqueueError,
    FlushNotVerifiableError,
    Generation,
    GenerationExportConfig,
    GenerationStart,
    ModelRef,
    TokenUsage,
    ToolExecutionStart,
    ValidationError,
    WorkflowStep,
    assistant_text_message,
    create_secret_redaction_sanitizer,
)
from agento11y.errors import ExportFlushError
from conftest import CapturingGenerationExporter
from opentelemetry import trace
from opentelemetry.sdk.metrics import ExemplarFilter, MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import SpanLimits, TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.sdk.trace.sampling import ALWAYS_OFF, ALWAYS_ON
from opentelemetry.trace import SpanKind, StatusCode


@pytest.fixture
def telemetry():
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    yield provider, exporter
    provider.shutdown()


def new_client(provider=None, **overrides):
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(protocol="otel", **overrides.pop("export", {})),
            tracer_provider=provider,
            **overrides,
        )
    )


def seed(**overrides):
    return GenerationStart(model=ModelRef(provider="openai", name="start-model"), **overrides)


def test_single_span_and_non_lifo_context(telemetry):
    provider, exporter = telemetry
    client = new_client(provider)
    with provider.get_tracer("app").start_as_current_span("application") as application:
        a = client.start_generation(seed(id="a"))
        b = client.start_generation(seed(id="b"))
        assert trace.get_current_span() is application
        for rec in (a, b):
            rec.set_result(
                model=ModelRef(provider="gemini", name="final-model"),
                operation_name="generateText",
                output=[assistant_text_message("answer")],
            )
            rec.end()
            rec.end()
            assert rec.err() is None
            assert rec.last_generation.model.provider == "gemini"
            assert rec.last_generation.operation_name == "generateText"
            assert trace.get_current_span() is application
        c = client.start_generation(seed(id="c"))
        c.end()
        spans = exporter.get_finished_spans()
        assert len(spans) == 3
        assert all(span.parent.span_id == application.get_span_context().span_id for span in spans)
        assert all(span.kind == SpanKind.CLIENT for span in spans)
        assert spans[0].name == "chat final-model"
        assert spans[0].attributes["gen_ai.provider.name"] == "gcp.gemini"
        assert all(span.attributes["agento11y.record"] == "true" for span in spans)
        assert client._pending_generations == []
        assert client._generation_exporter is None
        assert client._timer_thread is None
    client.shutdown()


@pytest.mark.parametrize("sampled, attribute_limit", [(False, 128), (True, 128), (True, 2)])
def test_sampling_and_span_limits_have_no_native_fallback(sampled, attribute_limit):
    exporter = InMemorySpanExporter()
    provider = TracerProvider(
        sampler=ALWAYS_ON if sampled else ALWAYS_OFF,
        span_limits=SpanLimits(max_attributes=attribute_limit),
    )
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    client = new_client(provider)
    try:
        rec = client.start_generation(seed(id="generation"))
        start_context = rec.span.get_span_context()
        assert start_context.trace_flags.sampled is sampled
        rec.set_result(output=[assistant_text_message("answer")], operation_name="custom")
        rec.end()
        assert rec.err() is None
        assert rec.last_generation.id == "generation"
        assert rec.span.get_span_context() == start_context
        assert client._pending_generations == []
        assert client._generation_exporter is None
        spans = exporter.get_finished_spans()
        assert len(spans) == int(sampled)
        if sampled:
            assert spans[0].name == "custom start-model"
            assert (spans[0].dropped_attributes > 0) is (attribute_limit == 2)
    finally:
        client.shutdown()
        provider.shutdown()


@pytest.mark.parametrize(
    "reason", ["validation", "normalization", "mapping", "encoding", "shutdown", "size", "metrics"]
)
def test_rejected_records_finish_without_document(telemetry, monkeypatch, reason):
    provider, exporter = telemetry
    client = new_client(provider, export={"payload_max_bytes": 1} if reason == "size" else {})
    rec = client.start_generation(seed(id="reject", conversation_title="secret-title"))
    result = Generation(output=[assistant_text_message("secret-content")])
    if reason == "validation":
        result.output = [assistant_text_message("")]
    elif reason == "normalization":
        monkeypatch.setattr(type(rec), "_normalize_generation", Mock(side_effect=ValueError("secret-error")))
    elif reason == "encoding":
        result.metadata["invalid"] = float("nan")
    elif reason == "metrics":
        monkeypatch.setattr(
            client._otel_tool_calls_histogram, "record", Mock(side_effect=RuntimeError("metric failure"))
        )
    rec.set_result(result, mapping_error=ValueError("secret-error") if reason == "mapping" else None)
    if reason == "shutdown":
        client.shutdown()
    rec.end()
    rec.end()
    error_class = ClientShutdownError if reason == "shutdown" else EnqueueError if reason == "size" else ValidationError
    assert isinstance(rec.err(), error_class)
    spans = exporter.get_finished_spans()
    assert len(spans) == 1
    attrs = spans[0].attributes
    assert attrs["agento11y.record"] == "false"
    assert attrs["gen_ai.operation.name"] == "agento11y.invalid_generation"
    assert "agento11y.generation.id" not in attrs
    assert not any(key.startswith(("gen_ai.input", "gen_ai.output", "agento11y.generation.")) for key in attrs)
    assert "secret" not in json.dumps(dict(attrs))
    assert spans[0].status.status_code == StatusCode.ERROR
    assert client._pending_generations == []
    client.shutdown()


@pytest.mark.parametrize(
    "outcome, fault",
    [
        (outcome, fault)
        for outcome in ("accepted", "provider-error", "shutdown", "validation", "normalization", "size")
        for fault in ("duration", "filter")
    ]
    + [(outcome, fault) for outcome in ("accepted", "provider-error") for fault in ("input", "output")],
)
def test_completion_telemetry_failure_preserves_admission(telemetry, monkeypatch, caplog, outcome, fault):
    from agento11y.client import _DURATION_BUCKETS_SECONDS
    from agento11y.otel_export import INSTRUMENTATION_NAME, SCHEMA_URL

    class RaisingFilter(ExemplarFilter):
        def should_sample(self, value, time_unix_nano, attributes, context):
            if value > 0 and "gen_ai.token.type" not in attributes:
                raise RuntimeError("secret telemetry error")
            return False

    provider, exporter = telemetry
    meter_provider = MeterProvider(exemplar_filter=RaisingFilter() if fault == "filter" else None)
    client = new_client(
        provider, meter_provider=meter_provider, export={"payload_max_bytes": 1} if outcome == "size" else {}
    )
    if fault != "filter":
        meter = meter_provider.get_meter(INSTRUMENTATION_NAME, "", schema_url=SCHEMA_URL)
        instrument = (
            meter.create_histogram(
                "gen_ai.client.operation.duration",
                unit="s",
                description="Duration of GenAI client operation",
                explicit_bucket_boundaries_advisory=list(_DURATION_BUCKETS_SECONDS),
            )
            if fault == "duration"
            else client._otel_token_usage_histogram
        )
        record = instrument.record

        def faulting_record(value, attributes=None, context=None):
            if fault == "duration" or attributes.get("gen_ai.token.type") == fault:
                raise RuntimeError("secret telemetry error")
            return record(value, attributes=attributes, context=context)

        monkeypatch.setattr(instrument, "record", faulting_record)
    rec = client.start_generation(seed(id="metric-failure"))
    invocation = rec._otel_invocation
    invocation.stop = Mock(wraps=invocation.stop)
    invocation.fail = Mock(wraps=invocation.fail)
    rec.set_result(
        output=[assistant_text_message("" if outcome == "validation" else "answer")],
        usage=TokenUsage(input_tokens=4, output_tokens=2),
    )
    if outcome == "normalization":
        monkeypatch.setattr(
            type(rec), "_normalize_generation", Mock(side_effect=ValueError("secret normalization error"))
        )
    if outcome == "provider-error":
        rec.set_call_error(ValueError("HTTP 429 provider error"))
    if outcome == "shutdown":
        client.shutdown()
    rec.end()
    rec.end()
    spans = exporter.get_finished_spans()
    assert len(spans) == 1
    accepted = outcome in ("accepted", "provider-error")
    assert spans[0].attributes["agento11y.record"] == str(accepted).lower()
    assert ("agento11y.generation.id" in spans[0].attributes) is accepted
    assert spans[0].attributes["gen_ai.operation.name"] == ("chat" if accepted else "agento11y.invalid_generation")
    assert spans[0].status.status_code == (StatusCode.UNSET if outcome == "accepted" else StatusCode.ERROR)
    if accepted:
        assert rec.err() is None
        assert rec.last_generation.id == "metric-failure"
    else:
        error_class = (
            ClientShutdownError if outcome == "shutdown" else EnqueueError if outcome == "size" else ValidationError
        )
        assert isinstance(rec.err(), error_class)
    assert invocation.stop.call_count == int(outcome == "accepted")
    assert invocation.fail.call_count == int(outcome != "accepted")
    assert "OTel invocation completion failed" in caplog.text
    assert "secret telemetry error" not in caplog.text
    assert "ended span" not in caplog.text
    client.shutdown()
    meter_provider.shutdown()


@pytest.mark.parametrize("mode", list(ContentCaptureMode))
def test_capture_resolution_is_client_local(telemetry, mode):
    provider, exporter = telemetry
    client = new_client(provider, content_capture=mode, embedding_capture=EmbeddingCaptureConfig(capture_input=True))
    native_exporter = CapturingGenerationExporter()
    native = Client(ClientConfig(tracer_provider=provider, generation_exporter=native_exporter))
    rec = client.start_generation(seed(conversation_title="title"))
    rec.set_result(output=[assistant_text_message("answer")])
    tool = client.start_tool_execution(ToolExecutionStart(tool_name="lookup"))
    tool.set_result(arguments={"q": "secret"}, result="secret-result")
    tool.end()
    embedding = client.start_embedding(EmbeddingStart(model=ModelRef(provider="openai", name="embed")))
    embedding.set_result(EmbeddingResult(input_count=1, input_texts=["secret-input"]))
    embedding.end()
    native_tool = native.start_tool_execution(ToolExecutionStart(tool_name="native-lookup"))
    native_tool.set_result(arguments={"q": "secret"}, result="secret-result")
    native_tool.end()
    native_rec = native.start_generation(seed(conversation_title="native-title"))
    native_rec.end()
    rec.end()
    spans = {span.name: span for span in exporter.get_finished_spans()}
    generation_attrs = spans["chat start-model"].attributes
    content = mode != ContentCaptureMode.METADATA_ONLY
    assert ("gen_ai.output.messages" in generation_attrs) == content
    assert ("agento11y.conversation.title" in generation_attrs) == content
    tool_content = mode in (ContentCaptureMode.FULL, ContentCaptureMode.FULL_WITH_METADATA_SPANS)
    assert ("gen_ai.tool.call.arguments" in spans["execute_tool lookup"].attributes) == tool_content
    assert ("gen_ai.embeddings.input_texts" in spans["embeddings embed"].attributes) == content
    if mode == ContentCaptureMode.FULL_WITH_METADATA_SPANS:
        assert "gen_ai.tool.call.arguments" not in spans["execute_tool native-lookup"].attributes
        assert "agento11y.conversation.title" not in native_rec.span.attributes
        assert native_rec.last_generation.conversation_title == "native-title"
    native.shutdown()
    client.shutdown()


@pytest.mark.parametrize("failure", ["raises", "none"])
def test_sanitizer_fallback_closes_once_without_content(telemetry, failure):
    provider, exporter = telemetry
    sanitizer = Mock(side_effect=ValueError("secret") if failure == "raises" else None, return_value=None)
    client = new_client(provider, content_capture=ContentCaptureMode.FULL, generation_sanitizer=sanitizer)
    rec = client.start_generation(seed(conversation_title="secret"))
    rec.set_result(output=[assistant_text_message("secret")])
    invocation = rec._otel_invocation
    original_stop = invocation.stop
    invocation.stop = Mock(wraps=original_stop)
    invocation.fail = Mock(wraps=invocation.fail)
    rec.end()
    rec.end()
    assert rec.err() is None
    invocation.stop.assert_called_once()
    invocation.fail.assert_not_called()
    attrs = exporter.get_finished_spans()[0].attributes
    assert attrs["agento11y.record"] == "true"
    assert "secret" not in json.dumps(dict(attrs))
    assert json.loads(attrs["agento11y.generation.metadata"])["agento11y.sdk.content_capture_mode"] == "metadata_only"
    client.shutdown()


@pytest.mark.parametrize("error_source", ["recorder", "generation"])
@pytest.mark.parametrize("mode", list(ContentCaptureMode))
@pytest.mark.parametrize("sanitizer_action", ["redact", "clear", "replace", "raises"])
def test_sanitized_content_only_and_no_operation_logs(telemetry, monkeypatch, error_source, mode, sanitizer_action):
    from opentelemetry.sdk._logs import LoggerProvider
    from opentelemetry.sdk._logs.export import InMemoryLogRecordExporter, SimpleLogRecordProcessor

    provider, exporter = telemetry
    logs = InMemoryLogRecordExporter()
    logger_provider = LoggerProvider()
    logger_provider.add_log_record_processor(SimpleLogRecordProcessor(logs))
    monkeypatch.setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "SPAN_AND_EVENT")
    monkeypatch.setenv("OTEL_INSTRUMENTATION_GENAI_EMIT_EVENT", "true")
    monkeypatch.setenv("OTEL_INSTRUMENTATION_GENAI_COMPLETION_HOOK", "test-hook")
    # Avoid the one-shot global setter so pytest can restore the provider.
    monkeypatch.setattr("opentelemetry._logs._internal._LOGGER_PROVIDER", logger_provider)
    monkeypatch.setattr(
        "opentelemetry.util.genai.completion_hook.entry_points", Mock(side_effect=AssertionError("hook discovery"))
    )
    secret = "AKIAIOSFODNN7EXAMPLE"
    redactor = create_secret_redaction_sanitizer()

    def sanitizer(generation):
        if sanitizer_action == "raises":
            raise ValueError("secret sanitizer error")
        generation = redactor(generation)
        if sanitizer_action == "clear":
            generation.call_error = ""
        elif sanitizer_action == "replace":
            generation.call_error = "sanitized error"
        return generation

    reader = InMemoryMetricReader()
    meter_provider = MeterProvider(metric_readers=[reader])
    client = new_client(provider, content_capture=mode, generation_sanitizer=sanitizer, meter_provider=meter_provider)
    rec = client.start_generation(seed(conversation_title=secret))
    assert secret not in json.dumps(dict(rec.span.attributes))
    error = f"HTTP 429 {secret}"
    rec.set_result(output=[assistant_text_message(secret)], call_error=error if error_source == "generation" else "")
    if error_source == "recorder":
        rec.set_call_error(ValueError(error))
    rec.end()
    span = exporter.get_finished_spans()[0]
    assert secret not in span.to_json()
    metadata = json.loads(span.attributes["agento11y.generation.metadata"])
    assert "call_error" not in metadata
    assert "agento11y.conversation.title" not in metadata
    assert logs.get_finished_logs() == ()
    assert rec.err() is None
    assert span.attributes["error.type"] == "provider_call_error"
    assert span.status.status_code == StatusCode.ERROR
    assert span.attributes["error.category"] == "rate_limit"
    assert span.status.description == (rec.last_generation.call_error or "rate_limit")
    for resource in reader.get_metrics_data().resource_metrics:
        for scope in resource.scope_metrics:
            for metric in scope.metrics:
                for point in metric.data.data_points:
                    assert point.attributes["error.category"] == "rate_limit"
                    assert point.attributes["error.type"] == "provider_call_error"
    logger_provider.shutdown()
    client.shutdown()
    meter_provider.shutdown()


@pytest.mark.parametrize("outcome", ["none", "missing", "false", "true", "raises"])
def test_explicit_flush_outcomes(telemetry, outcome):
    provider, _ = telemetry
    client = new_client(provider if outcome != "none" else None)
    if outcome == "missing":
        provider.force_flush = None
    elif outcome in ("false", "true", "raises"):
        provider.force_flush = Mock(
            return_value=outcome == "true",
            side_effect=RuntimeError("processor failed") if outcome == "raises" else None,
        )
    if outcome in ("none", "missing"):
        with pytest.raises(FlushNotVerifiableError):
            client.flush()
    elif outcome == "false":
        with pytest.raises(ExportFlushError):
            client.flush()
    elif outcome == "raises":
        with pytest.raises(RuntimeError, match="processor failed"):
            client.flush()
    else:
        client.flush()
    client.shutdown()


def test_shutdown_best_effort_and_application_ownership(caplog):
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    processor = BatchSpanProcessor(exporter, schedule_delay_millis=60000)
    provider.add_span_processor(processor)
    client = new_client(provider)
    rec = client.start_generation(seed())
    rec.end()
    assert exporter.get_finished_spans() == ()
    provider.force_flush = Mock(return_value=False)
    provider.shutdown = Mock()
    client.shutdown()
    client.shutdown()
    provider.force_flush.assert_called_once()
    provider.shutdown.assert_not_called()
    assert client._closed
    assert "flush on shutdown failed" in caplog.text
    processor.shutdown()


def test_workflow_export_is_rejected(telemetry):
    provider, exporter = telemetry
    client = new_client(provider)
    with pytest.raises(EnqueueError, match="workflow"):
        client.enqueue_workflow_step(WorkflowStep(id="workflow"))
    assert exporter.get_finished_spans() == ()
    assert client._pending_workflow_steps == []
    client.shutdown()
