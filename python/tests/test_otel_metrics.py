from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

pytest.importorskip("opentelemetry.util.genai.handler")

from agento11y import (
    Client,
    ClientConfig,
    EmbeddingResult,
    EmbeddingStart,
    Generation,
    GenerationExportConfig,
    GenerationMode,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    TokenInputSemantics,
    TokenUsage,
    ToolCall,
    ToolExecutionStart,
    tool_call_part,
)
from opentelemetry import trace
from opentelemetry.sdk.metrics import AlwaysOnExemplarFilter, Meter, MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

_DURATION = "gen_ai.client.operation.duration"
_TOKENS = "gen_ai.client.token.usage"
_TTFT = "gen_ai.client.time_to_first_token"
_TOOLS = "gen_ai.client.tool_calls_per_operation"
_CHUNKS = (
    "gen_ai.client.operation.time_to_first_chunk",
    "gen_ai.client.operation.time_per_output_chunk",
)
_SCOPE = "github.com/grafana/sigil/sdks/python"
_SCHEMA = "https://opentelemetry.io/schemas/1.37.0"
_START = datetime(2026, 1, 1, tzinfo=timezone.utc)
_COUNTS = {"input": 99, "output": 37, "cache_read": 7, "cache_write": 3, "reasoning": 11}
_FIELDS = {
    "input": "input_tokens",
    "output": "output_tokens",
    "cache_read": "cache_read_input_tokens",
    "cache_write": "cache_write_input_tokens",
    "reasoning": "reasoning_tokens",
}


def usage(counts=None, semantics=TokenInputSemantics.INCLUSIVE):
    return TokenUsage(
        **{_FIELDS[key]: value for key, value in (_COUNTS if counts is None else counts).items()},
        input_semantics=semantics,
    )


def tool_output():
    return [
        Message(
            role=MessageRole.ASSISTANT,
            parts=[
                tool_call_part(ToolCall(name="lookup", id="one", input_json=b"{}")),
                tool_call_part(ToolCall(name="lookup", id="two", input_json=b"{}")),
            ],
        )
    ]


def metrics(reader):
    data = reader.get_metrics_data()
    return (
        []
        if data is None
        else [
            (scope.scope, metric)
            for resource in data.resource_metrics
            for scope in resource.scope_metrics
            for metric in scope.metrics
        ]
    )


def points(snapshot, name):
    return [point for _, metric in snapshot if metric.name == name for point in metric.data.data_points]


def single_point(snapshot, name):
    found = points(snapshot, name)
    assert len(found) == 1, f"{name}: expected one series, got {len(found)}"
    assert found[0].count == 1, f"{name}: expected one observation"
    return found[0]


@pytest.fixture
def telemetry():
    reader = InMemoryMetricReader()
    meter_provider = MeterProvider(metric_readers=[reader], exemplar_filter=AlwaysOnExemplarFilter())
    exporter = InMemorySpanExporter()
    tracer_provider = TracerProvider()
    tracer_provider.add_span_processor(SimpleSpanProcessor(exporter))
    clients = []

    def new_client(**overrides):
        client = Client(
            ClientConfig(
                generation_export=GenerationExportConfig(protocol="otel"),
                tracer_provider=tracer_provider,
                meter_provider=meter_provider,
                now=lambda: _START + timedelta(seconds=2),
                **overrides,
            )
        )
        clients.append(client)
        return client

    yield new_client, reader, tracer_provider, exporter
    for client in clients:
        client.shutdown()
    tracer_provider.shutdown()
    meter_provider.shutdown()


def finish(client, **result):
    rec = client.start_generation(
        GenerationStart(
            model=ModelRef(provider="openai", name="start-model"),
            started_at=_START,
        )
    )
    rec.set_result(**result)
    rec.end()
    rec.end()
    assert rec.err() is None
    return rec


def test_duration_is_observed_once(telemetry):
    new_client, reader, _, exporter = telemetry
    client = new_client()
    finish(client)
    assert single_point(metrics(reader), _DURATION).sum >= 0
    assert len(exporter.get_finished_spans()) == 1
    assert client._pending_generations == []


@pytest.mark.parametrize(
    "semantics", [TokenInputSemantics.UNSPECIFIED, TokenInputSemantics.INCLUSIVE], ids=["unspecified", "inclusive"]
)
@pytest.mark.parametrize("zero_type", [None, "all", *_COUNTS], ids=["positive", "all-zero", *_COUNTS])
def test_token_observations_are_exact_and_zero_suppressed(telemetry, semantics, zero_type):
    new_client, reader, _, _ = telemetry
    counts = dict(_COUNTS)
    if zero_type == "all":
        counts = dict.fromkeys(counts, 0)
    elif zero_type is not None:
        counts[zero_type] = 0
    finish(new_client(), usage=usage(counts, semantics))
    snapshot = metrics(reader)
    single_point(snapshot, _DURATION)
    observed = points(snapshot, _TOKENS)
    expected = {key: value for key, value in counts.items() if value > 0}
    assert len(observed) == len(expected)
    assert {point.attributes["gen_ai.token.type"] for point in observed} == set(expected)
    for point in observed:
        token_type = point.attributes["gen_ai.token.type"]
        assert point.count == 1
        # Input already includes cache; output already includes reasoning.
        assert point.sum == expected[token_type]
        assert point.min == point.max == expected[token_type]
        if semantics == TokenInputSemantics.INCLUSIVE:
            assert point.attributes["gen_ai.token.semantics"] == "inclusive"
        else:
            assert "gen_ai.token.semantics" not in point.attributes


@pytest.mark.parametrize("operation, mapped", [("generateText", "chat"), ("streamText", "chat"), ("custom", "custom")])
@pytest.mark.parametrize("metric_name", [_DURATION, _TOKENS, _TTFT, _TOOLS])
def test_final_mapped_identity_and_client_only_tags(telemetry, operation, mapped, metric_name):
    new_client, reader, _, _ = telemetry
    client = new_client(tags={"team": "sigil"})
    rec = client.start_generation(
        GenerationStart(
            model=ModelRef(provider="openai", name="start-model"),
            operation_name="start-operation",
            agent_name="start-agent",
            agent_version="v1",
            started_at=_START,
            tags={"request_id": "start-request"},
        )
    )
    rec.set_first_token_at(_START + timedelta(seconds=0.25))
    rec.set_result(
        Generation(
            mode=GenerationMode.STREAM,
            operation_name=operation,
            model=ModelRef(provider="gemini", name="final-model"),
            response_model="served-model",
            agent_name="final-agent",
            agent_version="v2",
            usage=usage(),
            output=tool_output(),
            tags={"request_id": "final-request"},
        )
    )
    rec.end()
    rec.end()
    assert rec.err() is None
    observed = points(metrics(reader), metric_name)
    assert len(observed) == (5 if metric_name == _TOKENS else 1)
    expected = {
        "gen_ai.operation.name": mapped,
        "gen_ai.provider.name": "gcp.gemini",
        "gen_ai.request.model": "final-model",
        "gen_ai.agent.name": "final-agent",
        "gen_ai.agent.version": "v2",
        "agento11y.tag.team": "sigil",
    }
    for point in observed:
        assert point.count == 1
        assert {key: point.attributes.get(key) for key in expected} == expected
        assert not any("request_id" in key for key in point.attributes)
        assert "start-model" not in point.attributes.values()
        assert "start-operation" not in point.attributes.values()
        assert "final-request" not in point.attributes.values()


@pytest.mark.parametrize(
    "mode, operation, has_first_token, want_ttft",
    [
        (GenerationMode.STREAM, "custom", True, True),
        (GenerationMode.STREAM, "chat", True, True),
        (GenerationMode.STREAM, "custom", False, False),
        (GenerationMode.SYNC, "streamText", True, False),
    ],
)
def test_stream_mode_controls_first_token_without_synthetic_chunks(
    telemetry,
    mode,
    operation,
    has_first_token,
    want_ttft,
):
    new_client, reader, _, _ = telemetry
    client = new_client()
    rec = client.start_generation(
        GenerationStart(
            model=ModelRef(provider="openai", name="start-model"),
            mode=GenerationMode.SYNC,
            started_at=_START,
        )
    )
    if has_first_token:
        rec.set_first_token_at(_START + timedelta(seconds=0.25))
        rec.set_first_token_at(_START + timedelta(seconds=0.25))
    rec.set_result(mode=mode, operation_name=operation, output=tool_output())
    rec.end()
    rec.end()
    assert rec.err() is None
    snapshot = metrics(reader)
    single_point(snapshot, _DURATION)
    assert single_point(snapshot, _TOOLS).sum == 2
    if want_ttft:
        assert single_point(snapshot, _TTFT).sum == pytest.approx(0.25)
    else:
        assert points(snapshot, _TTFT) == []
    assert all(points(snapshot, name) == [] for name in _CHUNKS)


def test_supplementary_tokens_share_utility_scope_and_instrument_definition(telemetry, caplog, monkeypatch):
    new_client, reader, _, _ = telemetry
    registrations = []
    original = Meter.create_histogram

    def create_histogram(meter, name, unit="", description="", *, explicit_bucket_boundaries_advisory=None):
        if name == _TOKENS:
            registrations.append((meter, unit, description, tuple(explicit_bucket_boundaries_advisory or ())))
        return original(
            meter, name, unit, description, explicit_bucket_boundaries_advisory=explicit_bucket_boundaries_advisory
        )

    monkeypatch.setattr(Meter, "create_histogram", create_histogram)
    client = new_client()
    finish(client, usage=usage())
    utility_meter = client._config.meter_provider.get_meter(_SCOPE, "", schema_url=_SCHEMA)
    definitions = [definition[1:] for definition in registrations if definition[0] is utility_meter]
    expected_buckets = tuple(4**index for index in range(14))
    assert definitions
    assert all(
        definition == ("{token}", "Number of input and output tokens used by GenAI clients", expected_buckets)
        for definition in definitions
    )
    snapshot = metrics(reader)
    token_metrics = [(scope, metric) for scope, metric in snapshot if metric.name == _TOKENS]
    assert len(token_metrics) == 1, "utility and supplementary tokens must not split instruments/scopes"
    scope, metric = token_metrics[0]
    assert (scope.name, scope.version, scope.schema_url) == (_SCOPE, "", _SCHEMA)
    assert metric.description == "Number of input and output tokens used by GenAI clients"
    assert metric.unit == "{token}"
    assert len(metric.data.data_points) == 5
    for point in metric.data.data_points:
        assert point.explicit_bounds == expected_buckets
    duration_scope, duration = next((scope, metric) for scope, metric in snapshot if metric.name == _DURATION)
    assert duration_scope == scope
    assert duration.description == "Duration of GenAI client operation"
    assert duration.unit == "s"
    assert not any(
        "already registered" in record.message.lower() or "duplicate" in record.message.lower()
        for record in caplog.records
    )


def test_exemplars_use_invocation_context_not_ambient_span(telemetry):
    new_client, reader, tracer_provider, _ = telemetry
    client = new_client()
    rec = client.start_generation(
        GenerationStart(
            model=ModelRef(provider="openai", name="start-model"),
            started_at=_START,
        )
    )
    invocation_context = rec._otel_invocation.context
    expected = trace.get_current_span(invocation_context).get_span_context()
    rec.set_first_token_at(_START + timedelta(seconds=0.25))
    rec.set_result(mode=GenerationMode.STREAM, operation_name="custom", usage=usage(), output=tool_output())
    with tracer_provider.get_tracer("application").start_as_current_span("unrelated") as ambient:
        assert ambient.get_span_context().trace_id != expected.trace_id
        rec.end()
        rec.end()
        assert trace.get_current_span() is ambient
    assert rec.err() is None
    snapshot = metrics(reader)  # First collection preserves the one-observation exemplars.
    for name, count in [(_DURATION, 1), (_TOKENS, 5), (_TTFT, 1), (_TOOLS, 1)]:
        observed = points(snapshot, name)
        assert len(observed) == count
        for point in observed:
            assert point.count == 1
            assert len(point.exemplars) == 1
            exemplar = point.exemplars[0]
            assert (exemplar.trace_id, exemplar.span_id) == (expected.trace_id, expected.span_id)
            assert exemplar.value == point.sum


def test_direct_meter_is_ignored_for_generation_metrics(telemetry):
    new_client, reader, _, _ = telemetry
    direct_reader = InMemoryMetricReader()
    direct_provider = MeterProvider(metric_readers=[direct_reader])
    try:
        client = new_client(meter=direct_provider.get_meter("application-direct"))
        rec = client.start_generation(
            GenerationStart(
                model=ModelRef(provider="openai", name="start-model"),
                started_at=_START,
            )
        )
        rec.set_first_token_at(_START + timedelta(seconds=0.25))
        rec.set_result(mode=GenerationMode.STREAM, usage=usage(), output=tool_output())
        rec.end()
        rec.end()
        assert rec.err() is None
        generation_metrics = metrics(reader)
        assert metrics(direct_reader) == [], "even supplementary generation metrics must ignore the direct meter"
        single_point(generation_metrics, _DURATION)
        assert len(points(generation_metrics, _TOKENS)) == 5
        assert single_point(generation_metrics, _TTFT).sum == pytest.approx(0.25)
        assert single_point(generation_metrics, _TOOLS).sum == 2
    finally:
        direct_provider.shutdown()


@pytest.mark.parametrize("operation", ["execute_tool", "embeddings"])
def test_direct_meter_retains_precedence_for_tools_and_embeddings(telemetry, operation):
    new_client, reader, _, _ = telemetry
    direct_reader = InMemoryMetricReader()
    direct_provider = MeterProvider(metric_readers=[direct_reader])
    try:
        client = new_client(meter=direct_provider.get_meter("application-direct"))
        if operation == "execute_tool":
            rec = client.start_tool_execution(ToolExecutionStart(tool_name="lookup"))
            rec.set_result(result="found")
        else:
            rec = client.start_embedding(EmbeddingStart(model=ModelRef(provider="openai", name="embed")))
            rec.set_result(EmbeddingResult(input_tokens=7))
        rec.end()
        snapshot = metrics(direct_reader)
        assert all(scope.name == "application-direct" for scope, _ in snapshot)
        assert single_point(snapshot, _DURATION).attributes["gen_ai.operation.name"] == operation
        if operation == "embeddings":
            assert single_point(snapshot, _TOKENS).sum == 7
        else:
            assert points(snapshot, _TOKENS) == []
        assert points(snapshot, _TTFT) == points(snapshot, _TOOLS) == []
        assert metrics(reader) == [], "tools/embeddings must not leak to the utility's provider meter"
    finally:
        direct_provider.shutdown()
