from __future__ import annotations

import copy
import importlib.util
import json
from dataclasses import replace
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch

import pytest
from agento11y import (
    Client,
    ClientConfig,
    ContentCaptureMode,
    GenerationExportConfig,
    ToolExecutionEnd,
    ToolExecutionKind,
    ToolExecutionStart,
)
from agento11y.client import _DURATION_BUCKETS_SECONDS, _tool_duration_attributes
from agento11y.experimental import ENV_ENABLE_EXPERIMENTAL_FEATURES
from opentelemetry.exporter.otlp.proto.common.metrics_encoder import encode_metrics
from opentelemetry.proto.collector.metrics.v1.metrics_service_pb2 import ExportMetricsServiceRequest
from opentelemetry.sdk.metrics import AlwaysOnExemplarFilter, MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.sdk.trace.sampling import ALWAYS_OFF
from opentelemetry.trace import NoOpTracerProvider

_CONTRACT = json.loads((Path(__file__).resolve().parents[2] / "conformance/tool-metrics/cases.json").read_text())
_TOOL = _CONTRACT["instrument"]
_LEGACY = "gen_ai.client.operation.duration"
_KIND = "agento11y.tool.execution.kind"
_START = datetime(2026, 1, 1, tzinfo=timezone.utc)
_END = _START + timedelta(seconds=1.25)
_HAS_OTEL = importlib.util.find_spec("opentelemetry.util.genai") is not None
_PROTOCOLS = ["none", pytest.param("otel", marks=pytest.mark.skipif(not _HAS_OTEL, reason="requires otel extra"))]


class _ExitCode(int):
    pass


def _metrics(reader):
    data = reader.get_metrics_data()
    return (
        {}
        if data is None
        else {
            metric.name: metric
            for resource in data.resource_metrics
            for scope in resource.scope_metrics
            for metric in scope.metrics
        }
    )


def _points(snapshot, name):
    return list(snapshot[name].data.data_points) if name in snapshot else []


def _single(snapshot, name):
    points = _points(snapshot, name)
    assert len(points) == 1
    return points[0]


@pytest.fixture
def telemetry():
    resources = []

    def new_client(*, sampling="sampled", protocol="none", meter_source="provider", **overrides):
        reader = InMemoryMetricReader()
        meters = MeterProvider(metric_readers=[reader], exemplar_filter=AlwaysOnExemplarFilter())
        spans = InMemorySpanExporter()
        if sampling == "none":
            traces = NoOpTracerProvider()
        elif sampling == "dropped":
            traces = TracerProvider(sampler=ALWAYS_OFF)
        else:
            traces = TracerProvider()
        if isinstance(traces, TracerProvider):
            traces.add_span_processor(SimpleSpanProcessor(spans))
        config = ClientConfig(
            generation_export=GenerationExportConfig(protocol=protocol),
            tracer_provider=traces,
            meter_provider=meters if meter_source == "provider" else None,
            meter=meters.get_meter("tool-metrics-test") if meter_source == "direct" else None,
            now=lambda: _END,
            **overrides,
        )
        client = Client(config)
        resources.append((client, meters, traces))
        return client, reader, spans, config

    yield new_client
    for client, meters, traces in reversed(resources):
        client.shutdown()
        meters.shutdown()
        if isinstance(traces, TracerProvider):
            traces.shutdown()


@pytest.mark.parametrize("case", _CONTRACT["cases"], ids=lambda case: case["id"])
def test_shared_tool_metric_matrix(telemetry, case):
    client, reader, spans, _ = telemetry()
    for _ in range(case.get("executions", 1)):
        rec = client.start_tool_execution(ToolExecutionStart(**case["start"], started_at=_START))
        rec.set_result(ToolExecutionEnd(**case.get("end", {}), completed_at=_END))
        if "error" in case:
            rec.set_exec_error(RuntimeError(case["error"]))
        for _ in range(case.get("end_calls", 1)):
            rec.end()
    snapshot = _metrics(reader)
    if case["count"]:
        point = _single(snapshot, _TOOL)
        assert point.count == case["count"]
        assert point.sum == 1.25 * case["count"]
        assert dict(point.attributes) == case["attributes"]
        assert snapshot[_TOOL].unit == _CONTRACT["unit"]
        assert point.explicit_bounds == _DURATION_BUCKETS_SECONDS
    else:
        assert not _points(snapshot, _TOOL)
    legacy = _single(snapshot, _LEGACY)
    assert legacy.count == case.get("executions", 1)
    assert legacy.sum == 1.25 * legacy.count
    assert legacy.attributes["error.type"] == ("tool_execution_error" if "error" in case else "")
    assert not any(key.startswith(("gen_ai.skill.", "process.")) or key == _KIND for key in legacy.attributes)
    assert len(spans.get_finished_spans()) == legacy.count


def test_metrics_wait_for_finalization(telemetry):
    client, reader, _, _ = telemetry()
    rec = client.start_tool_execution(
        ToolExecutionStart(tool_name="Skill", execution_kind=ToolExecutionKind.LOAD_SKILL, started_at=_START)
    )
    assert not _metrics(reader)
    rec.set_result(verified_skill_name="verified")
    assert not _metrics(reader)
    rec.end()
    snapshot = _metrics(reader)
    assert _single(snapshot, _TOOL).count == _single(snapshot, _LEGACY).count == 1


@pytest.mark.parametrize("kind", [None, "future", *ToolExecutionKind])
@pytest.mark.parametrize("verified", [False, True])
def test_skill_metrics_require_verified_result_evidence(telemetry, kind, verified):
    client, reader, spans, _ = telemetry()
    rec = client.start_tool_execution(
        ToolExecutionStart(
            tool_name="read",
            execution_kind=kind,
            skill_name="attempted",
            skill_resource_name="attempted.md",
            started_at=_START,
        )
    )
    rec.set_result(
        arguments={"skill_name": "argument", "skill_resource_name": "argument.md"},
        result={"skill_name": "output", "skill_resource_name": "output.md"},
        verified_skill_name=" verified " if verified else "",
        verified_skill_resource_name=" verified.md " if verified else "",
    )
    rec.end()
    snapshot = _metrics(reader)
    point = _single(snapshot, _TOOL)
    known_kind = kind in tuple(ToolExecutionKind)
    attrs = {
        "gen_ai.tool.name": "read",
        _KIND: kind.value if known_kind else "generic",
    }
    if verified and known_kind:
        attrs["gen_ai.skill.name"] = "verified"
    if verified and kind in (ToolExecutionKind.READ_SKILL_RESOURCE, ToolExecutionKind.COMMAND):
        attrs["gen_ai.skill.resource.name"] = "verified.md"
    assert dict(point.attributes) == attrs
    assert point.count == _single(snapshot, _LEGACY).count == 1
    assert point.sum == 1.25
    assert len(spans.get_finished_spans()) == 1


@pytest.mark.parametrize("protocol", _PROTOCOLS)
@pytest.mark.parametrize("sampling", ["sampled", "dropped", "none"])
@pytest.mark.parametrize("mode", list(ContentCaptureMode))
@pytest.mark.parametrize("execution_error", [False, True])
def test_serialization_error_is_not_execution_error(telemetry, protocol, sampling, mode, execution_error):
    client, reader, spans, _ = telemetry(
        protocol=protocol,
        sampling=sampling,
    )
    rec = client.start_tool_execution(
        ToolExecutionStart(
            tool_name="Skill",
            execution_kind=ToolExecutionKind.LOAD_SKILL,
            skill_name="attempted",
            started_at=_START,
            include_content=True,
            content_capture=mode,
        )
    )
    rec.set_result(result=object(), verified_skill_name="verified")
    if execution_error:
        rec.set_exec_error(RuntimeError("tool failed"))
    rec.end()
    rec.end()
    snapshot = _metrics(reader)
    point = _single(snapshot, _TOOL)
    legacy = _single(snapshot, _LEGACY)
    assert point.count == legacy.count == 1
    assert point.attributes.get("error.type") == ("tool_execution_error" if execution_error else None)
    assert point.attributes["gen_ai.skill.name"] == "verified"
    # OTel generation export resolves FULL_WITH_METADATA_SPANS to FULL.
    serializes = mode not in (ContentCaptureMode.METADATA_ONLY, ContentCaptureMode.FULL_WITH_METADATA_SPANS)
    serializes |= protocol == "otel" and mode == ContentCaptureMode.FULL_WITH_METADATA_SPANS
    assert bool(rec.err()) is (serializes or execution_error)
    assert legacy.attributes["error.type"] == ("tool_execution_error" if serializes or execution_error else "")
    finished = spans.get_finished_spans()
    if sampling == "sampled":
        assert len(finished) == 1
        span = finished[0]
        assert span.name == "execute_tool Skill attempted"
        assert span.attributes["gen_ai.skill.name"] == "attempted"
        assert span.status.status_code.name == ("ERROR" if serializes or execution_error else "OK")
    else:
        assert not finished


@pytest.mark.parametrize("convenience", [False, True])
@pytest.mark.parametrize("copier", [copy.copy, copy.deepcopy, replace], ids=["copy", "deepcopy", "replace"])
def test_result_snapshot_and_replacement(telemetry, convenience, copier):
    client, reader, spans, _ = telemetry()
    start = ToolExecutionStart(
        tool_name="exec",
        execution_kind=ToolExecutionKind.COMMAND,
        skill_name="attempted",
        started_at=_START,
        include_content=True,
    )
    rec = client.start_tool_execution(start)
    start.execution_kind = None
    start.skill_name = "mutated"
    original = ToolExecutionEnd(
        verified_skill_name="verified",
        verified_skill_resource_name="check.py",
        process_exit_code=2,
        process_exit_code_is_error=False,
        arguments={"value": ["original"]},
        completed_at=_END,
    )
    result = copier(original)
    if convenience:
        rec.set_result(**{field: getattr(result, field) for field in result.__dataclass_fields__})
    else:
        rec.set_result(result)
    assert result == original
    for payload in (original, result):
        payload.verified_skill_name = "mutated"
        payload.verified_skill_resource_name = "mutated"
        payload.process_exit_code = 99
        payload.process_exit_code_is_error = True
        payload.completed_at = _START
        payload.arguments["value"].append("mutated")
    rec.end()
    point = _single(_metrics(reader), _TOOL)
    assert point.sum == 1.25
    assert point.attributes["gen_ai.skill.name"] == "verified"
    assert point.attributes["gen_ai.skill.resource.name"] == "check.py"
    assert point.attributes["process.exit.code"] == 2
    assert "error.type" not in point.attributes
    span = spans.get_finished_spans()[0]
    assert span.name == "execute_tool exec"
    assert "mutated" not in span.attributes["gen_ai.tool.call.arguments"]

    replacement = client.start_tool_execution(
        ToolExecutionStart(
            tool_name="exec",
            execution_kind=ToolExecutionKind.COMMAND,
            started_at=_START,
        )
    )
    replacement.set_result(verified_skill_name="stale", verified_skill_resource_name="stale", process_exit_code=9)
    replacement.set_result(ToolExecutionEnd(completed_at=_END))
    replacement.end()
    points = _points(_metrics(reader), _TOOL)
    blank = next(point for point in points if "gen_ai.skill.name" not in point.attributes)
    assert blank.count == 1
    assert "gen_ai.skill.resource.name" not in blank.attributes
    assert "process.exit.code" not in blank.attributes
    assert "error.type" not in blank.attributes


@pytest.mark.parametrize("invalid", [True, False, "2", 2.0, float("nan"), float("inf"), -float("inf")])
@pytest.mark.parametrize("mutated", [False, True])
def test_invalid_exit_is_rejected_without_retaining_result(telemetry, invalid, mutated):
    client, reader, _, _ = telemetry()
    rec = client.start_tool_execution(ToolExecutionStart(tool_name="exec", execution_kind=ToolExecutionKind.COMMAND))
    with pytest.raises(ValueError, match="process_exit_code"):
        if mutated:
            result = ToolExecutionEnd(verified_skill_name="rejected")
            result.process_exit_code = invalid
            rec.set_result(result)
        else:
            rec.set_result(process_exit_code=invalid, verified_skill_name="rejected")
    rec.end()
    point = _single(_metrics(reader), _TOOL)
    assert "process.exit.code" not in point.attributes
    assert "error.type" not in point.attributes
    assert "gen_ai.skill.name" not in point.attributes


@pytest.mark.parametrize("invalid", [None, True, False, "2", 2.0, float("nan"), float("inf"), -float("inf")])
@pytest.mark.parametrize("execution_error", ["", "tool_execution_error"])
def test_invalid_exit_metric_attributes(invalid, execution_error):
    result = ToolExecutionEnd()
    result.process_exit_code = invalid
    attrs = _tool_duration_attributes(
        ToolExecutionStart(tool_name="exec", execution_kind=ToolExecutionKind.COMMAND), result, execution_error
    )
    assert "process.exit.code" not in attrs
    assert attrs.get("error.type") == (execution_error or None)


@pytest.mark.parametrize(
    "exit_code, expected_exit",
    [
        (-1, -1),
        (0, 0),
        (2, 2),
        (-(2**63), -(2**63)),
        (2**63 - 1, 2**63 - 1),
        (-(2**63) - 1, None),
        (2**63, None),
        (2**100, None),
        pytest.param(_ExitCode(2), None, id="int-subclass"),
    ],
)
@pytest.mark.parametrize("override", [None, False, True])
@pytest.mark.parametrize("execution_error", [False, True])
def test_command_exit_does_not_change_legacy_error_semantics(
    telemetry, exit_code, expected_exit, override, execution_error
):
    client, reader, spans, _ = telemetry()
    rec = client.start_tool_execution(ToolExecutionStart(tool_name="exec", execution_kind=ToolExecutionKind.COMMAND))
    rec.set_result(process_exit_code=exit_code, process_exit_code_is_error=override)
    error = RuntimeError("command failed") if execution_error else None
    if error is not None:
        rec.set_exec_error(error)
    rec.end()
    snapshot = _metrics(reader)
    point = _single(snapshot, _TOOL)
    assert point.attributes.get("process.exit.code") == expected_exit
    expected_error = str(expected_exit) if expected_exit and override is not False else None
    if execution_error:
        expected_error = "tool_execution_error"
    assert point.attributes.get("error.type") == expected_error
    assert _single(snapshot, _LEGACY).attributes["error.type"] == ("tool_execution_error" if execution_error else "")
    span = spans.get_finished_spans()[0]
    assert span.attributes["process.exit.code"] == exit_code
    assert span.status.status_code.name == ("ERROR" if execution_error else "OK")
    assert rec.err() is error


@pytest.mark.parametrize("exit_code", [-(2**63), 2**63 - 1, -(2**63) - 1, 2**63])
@pytest.mark.parametrize("execution_error", [False, True])
def test_command_exit_otlp_encoding(telemetry, exit_code, execution_error):
    client, reader, _, _ = telemetry()
    rec = client.start_tool_execution(ToolExecutionStart(tool_name="exec", execution_kind=ToolExecutionKind.COMMAND))
    rec.set_result(process_exit_code=exit_code)
    if execution_error:
        rec.set_exec_error(RuntimeError("command failed"))
    rec.end()
    encoded = encode_metrics(reader.get_metrics_data()).SerializeToString()
    request = ExportMetricsServiceRequest.FromString(encoded)
    metrics = {
        metric.name: metric
        for resource in request.resource_metrics
        for scope in resource.scope_metrics
        for metric in scope.metrics
    }
    (point,) = metrics[_TOOL].histogram.data_points
    attrs = {attr.key: attr.value for attr in point.attributes}
    assert point.count == 1
    if -(2**63) <= exit_code < 2**63:
        assert attrs["process.exit.code"].WhichOneof("value") == "int_value"
        assert attrs["process.exit.code"].int_value == exit_code
        assert attrs["error.type"].string_value == ("tool_execution_error" if execution_error else str(exit_code))
    else:
        assert "process.exit.code" not in attrs
        if execution_error:
            assert attrs["error.type"].string_value == "tool_execution_error"
        else:
            assert "error.type" not in attrs
    (legacy,) = metrics[_LEGACY].histogram.data_points
    assert legacy.count == 1
    legacy_attrs = {attr.key: attr.value for attr in legacy.attributes}
    assert legacy_attrs["error.type"].string_value == ("tool_execution_error" if execution_error else "")


def test_metric_attribute_allowlist_and_reserved_keys(telemetry):
    # Python exposes no raw tool metric-attribute map.
    reserved = _CONTRACT["reserved_attributes"] + [
        "gen_ai.tool.name",
        "gen_ai.tool.type",
        "gen_ai.agent.name",
        "gen_ai.conversation.id",
        "gen_ai.tool.call.id",
        "gen_ai.tool.description",
        "gen_ai.tool.call.arguments",
        "gen_ai.tool.call.result",
    ]
    raw = dict.fromkeys(reserved, "injected")
    client, reader, _, _ = telemetry(tags=raw)
    rec = client.start_tool_execution(
        ToolExecutionStart(
            tool_name="read",
            tool_type="function",
            tool_description="private",
            tool_call_id="private",
            conversation_id="private",
            request_provider="openai",
            request_model="model",
            agent_name="agent",
            agent_version="v1",
            execution_kind=ToolExecutionKind.READ_SKILL_RESOURCE,
            skill_name="attempted",
            skill_resource_name="attempted.md",
            skill_description="private",
            skill_source_uri="file:///private/skill",
            process_executable_name="private",
            process_executable_path="/private",
        )
    )
    rec.set_result(arguments=raw, result=raw, verified_skill_resource_name="known.md", process_exit_code=2)
    rec.end()
    attrs = dict(_single(_metrics(reader), _TOOL).attributes)
    for key in reserved:
        assert attrs.pop("agento11y.tag." + key) == "injected"
    assert attrs == {
        "gen_ai.tool.name": "read",
        "gen_ai.tool.type": "function",
        _KIND: "read_skill_resource",
        "gen_ai.provider.name": "openai",
        "gen_ai.request.model": "model",
        "gen_ai.agent.name": "agent",
        "gen_ai.agent.version": "v1",
        "gen_ai.skill.resource.name": "known.md",
    }


@pytest.mark.parametrize("seconds", [-1, 0, *_DURATION_BUCKETS_SECONDS, 100])
def test_duration_buckets_and_negative_clamping(telemetry, seconds):
    client, reader, _, _ = telemetry()
    rec = client.start_tool_execution(ToolExecutionStart(tool_name="read", started_at=_START))
    rec.set_result(completed_at=_START + timedelta(seconds=seconds))
    rec.end()
    snapshot = _metrics(reader)
    for name in (_TOOL, _LEGACY):
        point = _single(snapshot, name)
        duration = max(seconds, 0)
        assert point.count == 1
        assert point.sum == duration
        assert point.explicit_bounds == _DURATION_BUCKETS_SECONDS
        expected_bucket = next(
            (i for i, bound in enumerate(_DURATION_BUCKETS_SECONDS) if duration <= bound),
            len(_DURATION_BUCKETS_SECONDS),
        )
        assert point.bucket_counts == tuple(
            int(i == expected_bucket) for i in range(len(_DURATION_BUCKETS_SECONDS) + 1)
        )


def test_both_histograms_carry_tool_span_exemplars(telemetry):
    client, reader, spans, _ = telemetry()
    rec = client.start_tool_execution(ToolExecutionStart(tool_name="read", started_at=_START))
    rec.end()
    context = spans.get_finished_spans()[0].context
    snapshot = _metrics(reader)
    for name in (_TOOL, _LEGACY):
        exemplars = _single(snapshot, name).exemplars
        assert len(exemplars) == 1
        assert exemplars[0].trace_id == context.trace_id
        assert exemplars[0].span_id == context.span_id


@pytest.mark.parametrize("failure", ["creation", "record"])
def test_tool_instrument_failure_preserves_legacy_and_span(telemetry, failure):
    if failure == "creation":
        from opentelemetry.sdk.metrics import Meter

        original = Meter.create_histogram

        def create(meter, name, *args, **kwargs):
            if name == _TOOL:
                raise RuntimeError("tool instrument unavailable")
            return original(meter, name, *args, **kwargs)

        with patch.object(Meter, "create_histogram", create):
            client, reader, spans, _ = telemetry()
        assert client._tool_duration_histogram is None
        rec = client.start_tool_execution(ToolExecutionStart(tool_name="read", started_at=_START))
        rec.end()
    else:
        client, reader, spans, _ = telemetry()
        with patch.object(client._tool_duration_histogram, "record", side_effect=RuntimeError("record failed")):
            rec = client.start_tool_execution(ToolExecutionStart(tool_name="read", started_at=_START))
            rec.end()
    rec.end()
    assert rec.err() is None
    assert _single(_metrics(reader), _LEGACY).count == 1
    assert len(spans.get_finished_spans()) == 1


@pytest.mark.parametrize("meter_source", ["provider", "direct"])
def test_default_tool_metrics_use_normal_meter(telemetry, monkeypatch, meter_source):
    from opentelemetry.sdk.metrics import Meter

    monkeypatch.delenv(ENV_ENABLE_EXPERIMENTAL_FEATURES, raising=False)
    original = Meter.create_histogram
    names = []

    def create(meter, name, *args, **kwargs):
        names.append(name)
        return original(meter, name, *args, **kwargs)

    with patch.object(Meter, "create_histogram", create):
        client, reader, _, _ = telemetry(meter_source=meter_source)
    assert names.count(_TOOL) == names.count(_LEGACY) == 1
    client.start_tool_execution(ToolExecutionStart(tool_name="read", started_at=_START)).end()
    snapshot = _metrics(reader)
    point = _single(snapshot, _TOOL)
    assert point.count == _single(snapshot, _LEGACY).count == 1
    assert point.sum == 1.25
    assert dict(point.attributes) == {"gen_ai.tool.name": "read", _KIND: "generic"}
    assert snapshot[_TOOL].unit == "s"
    assert point.explicit_bounds == _DURATION_BUCKETS_SECONDS


def test_empty_tool_name_does_not_record(telemetry):
    client, reader, spans, _ = telemetry()
    rec = client.start_tool_execution(ToolExecutionStart(tool_name=" "))
    rec.set_result(verified_skill_name="ignored")
    rec.end()
    assert not _metrics(reader)
    assert not spans.get_finished_spans()
